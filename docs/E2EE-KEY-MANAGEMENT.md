# E2EE key management (MLS)

Internal. What is built, where it lives, the rules each side follows, and what
is deliberately not done. The public page is
`sites/moqom-dev/src/content/docs/guides/e2ee.md`; this one may describe
mechanism, that one may not.

## Outcome

A room whose token says `e2ee: required` keys itself when the app has given
`DeviceCredentials` a `keyDelivery` and has not called `useEncryption`. Devices
agree an MLS group per room; each epoch's exporter becomes the SFrame base key.
The control plane orders handshake messages and stores public key packages. It
never sees an epoch secret, an exporter, or an SFrame key.

## Client: `moqom-swift/Sources/MoqomKit/Security`

| File | What |
| --- | --- |
| `MLSCipherSuite.swift` | Suite `0x0001` (X25519, AES-128-GCM, SHA-256, Ed25519) on CryptoKit; labelled KDF, SignWithLabel, EncryptWithLabel. |
| `MLSKeySchedule.swift` | RFC 9420 §8 epoch schedule, exporter, and the RFC 9605 §5.2 bridge. |
| `MLSGroup.swift` | KeyPackages, Welcome, Add/Remove commits, roster, epoch authenticator. |
| `MLSDelivery.swift` | `MLSDeliveryService` protocol and `HTTPMLSDelivery` (pinned like `TokenService`). |
| `ManagedRoomKeys.swift` | The actor that runs a room's membership: sync loop, committer rule, key installation. |

### Conformance

Primitives and key schedule pass the official vectors in
`Tests/MoqomKitTests/MLSVectors.swift` (from `mlswg/mls-implementations`):
crypto-basics (RefHash, ExpandWithLabel, DeriveSecret, DeriveTreeSecret,
SignWithLabel, EncryptWithLabel), key-schedule (every epoch, every secret, the
exporter), and deserialization (variable-length vector headers). Tree math,
secret tree, tree validation, transcript hashes, welcome and message vectors do
not apply: there is no ratchet tree, no secret tree (SFrame derives per-sender
keys itself), and the framing is our own.

### Where the group protocol departs from RFC 9420

- **Flat member list, no ratchet tree.** The committer seals the commit secret
  to every member's leaf key with HPKE. Linear per commit; fine for calls of a
  few hundred, which is the scope of this construction.
- **Own TLS-style encoding** for KeyPackage, Commit and Welcome. Not
  interoperable with other MLS stacks, and not claimed to be.
- **No standalone proposals.** Add and Remove ride inside the commit that
  applies them. Update is an empty commit, which refreshes the committer's leaf.
- **No PSKs, no external commits, no secret tree, no application messages.**
- **Credentials are Ed25519, not the Secure Enclave.** The enclave only does
  P-256, which is suite `0x0002`. The binding to the enclave comes instead from
  the delivery service: it accepts a KeyPackage only when the identity inside
  equals the caller's token identity, and the token is bound to the enclave key.

### SFrame bridge (RFC 9605 §5.2)

`base_key = MLS-Exporter("SFrame 1.0 Base Key", "", AEAD.Nk)` per epoch.
`KID = (leaf_index << 4) | (epoch % 16)`; sender separation is by KID through
SFrame's own key derivation, so every member can derive every sender key in its
epoch (§5.2's stated limitation: members can forge each other's media).
Receivers drop an old epoch's keys before its low bits wrap.

### Committer rule

The **online member with the lowest leaf index** commits outstanding work
(adds of pending devices, removals of members the service no longer reports
online). If it does not within 3 s, the member of rank r (by leaf index among
online members) commits after r × 3 s. Two committers racing is harmless: the
service accepts the first commit at the current epoch and returns 409 for the
other, whose author processes the winner and re-evaluates. External commits
were rejected because they let any device with a token change the group
without a member's agreement.

### Epoch changes without dropped frames

- The sender switches key the moment its group advances (own commit accepted,
  or another's processed), so a removed member's key stops being used as soon
  as the removal is ordered.
- Receivers install the new epoch's keys at once and keep the previous epoch's
  for 5 s.
- An object with a KID the receiver does not have yet is held up to 750 ms
  inline (order preserved) while the receiver catches up, then dropped and
  counted.
- Before a device is in any group, the protected writer drops rather than
  publishing clear or failing the broadcast.

## Delivery service: `moqom/go/internal/mls`

HTTP JSON under `/mls/v1/` on the control plane's join listener (the only
internet-facing port), authenticated by the participant token
(`server.KeyDeliveryHandler`): tenant = `aud`, room = `moqt.room` (required),
device = the token identity `username/device`. Account and device bans are
checked on every request; a banned device stops being heard from and the next
commit removes it. Not admin gRPC (mTLS, backend-only) and not MOQT tracks (the
relay is stateless and ordering needs one writer per room).

| Route | Body → reply |
| --- | --- |
| `PUT /mls/v1/keypackages` | `{keyPackages, lastResort}` → 204. Replaces the device's set; identity must equal the caller; Ed25519 signatures checked. |
| `POST /mls/v1/keypackages/claim` | `{device}` → `{keyPackage}`. Caller must be a member; target must be pending. One-time first, then the never-consumed last resort. |
| `GET /mls/v1/sync?after=&wait=` | Long poll ≤ 25 s → `{group, messages, members, pending, cursor}`. Welcomes only to their recipients. |
| `POST /mls/v1/group` | `{groupId}` → 201, or 409 if a group exists. |
| `POST /mls/v1/commits` | `{epoch, commit, welcome, joiners, removed}` → `{seq}`, or 409 stale. Caller must be a member; group id and epoch read from the commit header. The sender's signature is checked by the members, which hold the leaf keys. |
| `POST /mls/v1/rejoin` | Leaf is stale; next commit replaces it. |
| `DELETE /mls/v1/presence` | Leaving; next commit removes it. |

Rules: one group per room; a commit is accepted only at the current epoch,
first wins; `seq` is monotonic per room and survives a group being dropped;
a group with no online member (heard from within 30 s) is dropped so the next
device founds a fresh one; messages older than 10 minutes are pruned.

Stores: `mls.Memory` and `mls/postgres` (tables `mls_rooms`, `mls_messages`,
`mls_devices`, `mls_key_packages`; schema applied at startup under its own
advisory lock; commits serialised by `SELECT … FOR UPDATE` on the room row;
claims by `FOR UPDATE SKIP LOCKED`). Both pass `mlstest.Run`. The Postgres store
is used whenever the control plane's store is Postgres; otherwise memory, since
losing this state costs each live room one re-founding and nothing else.

## Not done

- Sender keys for large broadcast audiences.
- A ratchet tree (logarithmic commits), PSKs, external commits, cross-stack
  interop.
- Per-sender signatures on media (SFrame does not provide them).
- Clients other than Apple.

## Deploy

1. Control plane: redeploy `moqom-control` with `MOQOM_CONTROL_JOIN_ADDR` set
   (already the case where the join endpoint is live). The `mls_*` tables are
   created at startup. No new environment variables.
2. Load balancer / ingress for the join listener: long polls hold a request for
   up to 25 s; any idle timeout must exceed ~35 s.
3. SDK: release MoqomKit; apps opt in by passing `keyDelivery` on
   `DeviceCredentials` (`TokenService.keyDelivery` or `HTTPMLSDelivery`).
4. Publish the site after the SDK release, not before.
