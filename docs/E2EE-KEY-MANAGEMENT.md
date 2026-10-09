# E2EE key management (MLS)

Internal. What is built, where it lives, the rules each side follows, and what
is deliberately not done. The public page is
`sites/moqom-dev/src/content/docs/guides/e2ee.md`; this one may describe
mechanism, that one may not.

## Outcome

Every room requires end-to-end encryption, and a room keys itself when the app
has given `DeviceCredentials` a `keyDelivery` and has not called
`useEncryption`. Devices that may publish, and agents, agree an [MLS](https://www.rfc-editor.org/rfc/rfc9420) group per
room; each epoch's exporter becomes the [SFrame](https://www.rfc-editor.org/rfc/rfc9605) base key. Viewers are not
members: the committer seals each epoch's base key to them (sender keys,
below). The control plane orders handshake messages, stores public key
packages and viewers' public keys, and holds sealed envelopes. It never sees an
epoch secret, an exporter, or an SFrame key.

## Client: `moqom-swift/Sources/MoqomKit/Security`

| File | What |
| --- | --- |
| `MLSCipherSuite.swift` | The one suite, `0xF0AC` ([ML-KEM-1024](https://csrc.nist.gov/pubs/fips/203/final), AES-256-GCM, SHA-512, [ML-DSA-87](https://csrc.nist.gov/pubs/fips/204/final)), on CryptoKit; labelled KDF, SignWithLabel, EncryptWithLabel. See "Cipher suites" below. |
| `MLSKeySchedule.swift` | [RFC 9420](https://www.rfc-editor.org/rfc/rfc9420) §8 epoch schedule, exporter, and the [RFC 9605](https://www.rfc-editor.org/rfc/rfc9605) §5.2 bridge. |
| `MLSGroup.swift` | KeyPackages, Welcome, Add/Remove commits, roster, epoch authenticator. |
| `MLSDelivery.swift` | `MLSDeliveryService` protocol and `HTTPMLSDelivery` (pinned like `TokenService`). |
| `ManagedRoomKeys.swift` | The actor that runs a room's membership: sync loop, committer rule, key installation, agent disclosure, sealing to the audience and rotating for it. |
| `AudienceKeys.swift` | A viewer's side of sender keys: its ML-KEM-1024 key, the envelope format, and the receiver resolver that derives any publisher's key from an epoch base key. |
| `../Room/RoomAgent.swift` | A recorder or classifier running as a member: subscribes to everybody's top layer. |

### Conformance

Primitives and key schedule pass the official vectors in
`Tests/MoqomKitTests/MLSVectors.swift` (from `mlswg/mls-implementations`):
crypto-basics (RefHash, ExpandWithLabel, DeriveSecret, DeriveTreeSecret,
SignWithLabel, EncryptWithLabel), key-schedule (every epoch, every secret, the
exporter), and deserialization (variable-length vector headers). Tree math,
secret tree, tree validation, transcript hashes, welcome and message vectors do
not apply: there is no ratchet tree, no secret tree ([SFrame](https://www.rfc-editor.org/rfc/rfc9605) derives per-sender
keys itself), and the framing is our own. The working group publishes no
vectors for any post-quantum suite. For `0xF0AC`, `MLSPostQuantumSuiteTests`
checks the [ML-KEM-1024](https://csrc.nist.gov/pubs/fips/203/final) [HPKE](https://www.rfc-editor.org/rfc/rfc9180) (KEM `0x0042`) and
[ML-DSA-87](https://csrc.nist.gov/pubs/fips/204/final) signatures against Go's `crypto/hpke` and `crypto/mldsa`, and the
SHA-512 key schedule passes the working group's suite 5 vectors.

### Cipher suites

| Value | Name | Source |
| --- | --- | --- |
| `0xF0AC` (the only suite) | `MLS_256_MLKEM1024_AES256GCM_SHA512_MLDSA87` | [draft-ietf-mls-pq-ciphersuites](https://datatracker.ietf.org/doc/draft-ietf-mls-pq-ciphersuites/)' category-5 suite of the same name: [HPKE](https://www.rfc-editor.org/rfc/rfc9180) KEM `0x0042` over ML-KEM-1024 ([FIPS 203](https://csrc.nist.gov/pubs/fips/203/final), [draft-ietf-hpke-pq](https://datatracker.ietf.org/doc/draft-ietf-hpke-pq/)), HKDF-SHA512, AES-256-GCM, ML-DSA-87 ([FIPS 204](https://csrc.nist.gov/pubs/fips/204/final)) |

Every part of it is NIST category 5. The draft has not been assigned a value
for it, so `0xF0AC` is a private-use value (RFC 9420 §17.1,
`0xF000`–`0xFFFF`). It is defined in moqom-security (`MLSCipherSuite.swift`,
`go/mls/suite.go`, `rust/moqom-mls/src/suite.rs`), which the control plane
imports, so changing it means changing all three together. No other MLS
implementation recognises it. RFC 9420's `0x0001`, the draft's X-Wing
(`0xF0A1`), and MOQOM's earlier `0xF0AA` (ML-KEM-768) and `0xF0AB` (ML-KEM-1024
with ML-DSA-65) that `0xF0AC` replaced, are not implemented. Interop is not a goal here anyway (see "Own TLS-style encoding" below).

A group's suite is fixed when it is founded and is part of its GroupContext, so
every signature and every derived secret is bound to it. Key packages name their
suite. A commit refuses a package in another suite, and a joiner refuses a
Welcome into a group in another suite. There is no migration between suites; a
room changes suite by founding a new group. The control plane refuses to found a
group in any suite but `0xF0AC`, refuses key packages in any other, and drops a
stored group in any other the next time its room is used.

**What is post-quantum.** Everything that agrees or authenticates a group key.
Every secret MLS sends is HPKE-sealed to ML-KEM-1024: commit secrets to leaf
keys, and joiner secrets to init keys. Key packages, commits and GroupInfo are
signed with ML-DSA-87. Everything derived from an epoch inherits that, which
covers the exporter, the [SFrame](https://www.rfc-editor.org/rfc/rfc9605) base key and per-sender keys, the key sealed to
viewers (also over ML-KEM-1024), and the key for
sealed direct-mode candidate offers. Media is protected by AES-256-GCM under
those keys. Participant tokens, device possession proofs, direct tickets and the room
control log are ML-DSA-87 too.

**What is not.** TLS and [QUIC](https://www.rfc-editor.org/rfc/rfc9000) to the relay and control plane are outside this document.

**Sizes.** An ML-KEM-1024 public key is 1,568 bytes and an encapsulation 1,568;
an ML-DSA-87 public key is 2,592 bytes and a signature 4,627. A key package is
about 10.1 KiB (10,373 bytes for identity `bob/b`). Each member adds about 1,660
bytes to a commit and about 4,180 to a Welcome, so a Welcome into a group of 800
is about 3.35 MB. `maxKeyPackageBytes` is 16 KiB and `maxCommitBytes` 3.5 MiB, which
keeps a group of 800 within bounds; audiences beyond the stage are viewers, not
members. HPKE seals per commit are still linear in members.

**Negotiation.** There is none. A device founds in `0xF0AC`
(`ManagedRoomKeys(cipherSuite:)`) and publishes key packages in it before it
has seen the room. A group in a suite the build does not implement, `0xF0AA`
and `0xF0AB` included, is not joined.

### Where the group protocol departs from RFC 9420

- **Flat member list, no ratchet tree.** The committer seals the commit secret
  to every member's leaf key with [HPKE](https://www.rfc-editor.org/rfc/rfc9180). Linear per commit; fine for calls of a
  few hundred, which is the scope of this construction.
- **Own TLS-style encoding** for KeyPackage, Commit and Welcome. Not
  interoperable with other [MLS](https://www.rfc-editor.org/rfc/rfc9420) stacks, and not claimed to be.
- **No standalone proposals.** Add and Remove ride inside the commit that
  applies them. Update is an empty commit, which refreshes the committer's leaf.
- **No PSKs, no external commits, no secret tree, no application messages.**
- **Credentials are the suite's signature key, not the Secure Enclave.** The
  enclave only does P-256. The binding to the enclave comes instead from the
  delivery service: it accepts a KeyPackage only when the identity inside
  equals the caller's token identity, and the token is bound to the enclave key.

### SFrame bridge (RFC 9605 §5.2)

`base_key = MLS-Exporter("SFrame 1.0 Base Key", "", AEAD.Nk)` per epoch.
`KID = (leaf_index << 4) | (epoch % 16)`; sender separation is by KID through
[SFrame](https://www.rfc-editor.org/rfc/rfc9605)'s own key derivation, so every member can derive every sender key in its
epoch (§5.2's stated limitation: members can forge each other's media).
Receivers drop an old epoch's keys before its low bits wrap.

### Agents

A recorder or classifier that has to see media is a member under the username
`watchdog.<id>`. The control plane mints that username only for a watchdog
attached to the room and not failed, and with no grants, so it can watch and
not publish. Every member sees its leaf, and `ManagedRoomKeys` withholds the
device's own sending key (`clearKey`) while the group holds an agent whose
watchdog the room's signed control log has not disclosed. `RoomSession` passes
the log's watchdogs to `disclose(watchdogs:)` whenever they change.

Hosted rooms have no `.control` publisher yet; only `assumeAuthority` writes the
log, for tests and self-hosted deployments. Until the control plane publishes
it, an agent in a hosted room stays undisclosed and the room's media is held.

### Sender keys for viewers

A device whose token carries `moqt.pub == 0` and is not an agent is a viewer.
The key service refuses it key packages and founding (`Caller.Viewer`), and
MoqomKit runs `AudienceKeys` for it instead of `ManagedRoomKeys`:

- The viewer makes an [ML-KEM-1024](https://csrc.nist.gov/pubs/fips/203/final) key pair per session, keeps the private half
  in memory, and posts the public half with every long poll
  (`POST /mls/v1/audience`), which is also its heartbeat.
- A member's sync reports `audience` (viewers heard from within 30 s) and
  `audienceWaiting` (those without the current epoch's envelope); waiting
  viewers make the sync actionable.
- The rank-0 committer asks for the work, seals
  `MLS-Exporter("SFrame 1.0 Base Key")` to each with
  `EncryptWithLabel(pk, "MOQOM audience key", groupID ‖ epoch, base_key)` over
  ML-KEM-1024 [HPKE](https://www.rfc-editor.org/rfc/rfc9180), and posts up to 512 envelopes per request. The service
  refuses envelopes for any epoch but the current one.
- The viewer installs a `SFrameReceiver` resolver: for a KID whose low four bits
  match a held epoch, `SFrameKey(keyID: kid, baseKey: base)`. It keeps the
  current and previous epoch.
- While `audience > 0`, the committer commits an empty update every five
  minutes, so a viewer that stopped polling is not sealed the next epoch.
- A renewed token that grants or withdraws publishing moves the device across.
- Direct-path eligibility counts the audience as participants who have not
  opted in, so a room with viewers stays on the relay.

The trade: members see each other and compare epoch authenticators; viewers
are a list the service gives the committer, so a compromised control plane
could add an unseen viewer. That is the same power it has over issuing a
viewer token.

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
| `PUT /mls/v1/keypackages` | `{keyPackages, lastResort}` → 204. Replaces the device's set; identity must equal the caller; signatures checked ([ML-DSA-87](https://csrc.nist.gov/pubs/fips/204/final) with `crypto/mldsa`, or Ed25519); every package in one known suite, keys the suite's length, ML-KEM keys checked with `crypto/mlkem`. 403 for a viewer. |
| `POST /mls/v1/keypackages/claim` | `{device}` → `{keyPackage}`. Caller must be a member; target must be pending. One-time first, then the never-consumed last resort. 409 if the package is not in the group's suite (it is still consumed). |
| `GET /mls/v1/sync?after=&wait=` | Long poll ≤ 25 s → `{group: {id, epoch, cipherSuite}, messages, members, pending, cursor, audience, audienceWaiting}`. Welcomes only to their recipients; the audience counts only to members. |
| `POST /mls/v1/group` | `{groupId, cipherSuite}` → 201, or 409 if a group exists, or 400 for any suite but `0xF0AC`. 403 for a viewer. |
| `POST /mls/v1/commits` | `{epoch, commit, welcome, joiners, removed}` → `{seq}`, or 409 stale. Caller must be a member; group id and epoch read from the commit header. The sender's signature is checked by the members, which hold the leaf keys. |
| `POST /mls/v1/rejoin` | Leaf is stale; next commit replaces it. |
| `DELETE /mls/v1/presence` | Leaving; next commit removes it. |
| `POST /mls/v1/audience` | `{publicKey, haveGroupId?, haveEpoch?, wait}` → `{groupId, epoch, data}` once an envelope newer than `have` exists, else 204 after the wait. The key must be 1,568 bytes. Does not wake members' held syncs. |
| `DELETE /mls/v1/audience` | Stop being a viewer. |
| `GET /mls/v1/audience/work` | Members only → `{groupId, epoch, seats: [{device, publicKey}]}`: online viewers without the current epoch. |
| `POST /mls/v1/audience/envelopes` | Members only. `{groupId, epoch, envelopes: {device: data}}` → 204, 409 if the epoch is not current. At most 1,024 envelopes of at most 2 KiB. |

Rules: one group per room; a commit is accepted only at the current epoch,
first wins; `seq` is monotonic per room and survives a group being dropped;
a group with no online member (heard from within 30 s) is dropped so the next
device founds a fresh one; messages older than 10 minutes are pruned.

Stores: `mls.Memory` and `mls/postgres` (tables `mls_rooms` with
`cipher_suite`, default 1, added by `ALTER TABLE … ADD COLUMN IF NOT EXISTS`, `mls_messages`,
`mls_devices`, `mls_key_packages`, `mls_audience`; schema applied at startup under its own
advisory lock; commits serialised by `SELECT … FOR UPDATE` on the room row;
claims by `FOR UPDATE SKIP LOCKED`). Both pass `mlstest.Run`. The Postgres store
is used whenever the control plane's store is Postgres; otherwise memory, since
losing this state costs each live room one re-founding and nothing else.

## Not done

- Keys for personal channels. `RoomSession` keys rooms only, so under required
  encryption a personal channel can't publish yet.
- A hosted `.control` publisher, without which agents can't be disclosed in
  hosted rooms.
- A ratchet tree (logarithmic commits), PSKs, external commits, cross-stack
  interop.
- Per-sender signatures on media ([SFrame](https://www.rfc-editor.org/rfc/rfc9605) does not provide them).
- Clients other than Apple.

## Deploy

1. Control plane: redeploy `moqom-control` with `MOQOM_CONTROL_JOIN_ADDR` set
   (already the case where the join endpoint is live). The `mls_*` tables are
   created at startup. No new environment variables. Deploy it before an SDK
   that founds in `0xF0AC` or uses the audience routes. The `mls_audience`
   store has not yet been run against Postgres; run `mlstest.Run` with
   `MOQOM_TEST_POSTGRES` before deploying.
2. Load balancer / ingress for the join listener: long polls hold a request for
   up to 25 s; any idle timeout must exceed ~35 s.
3. SDK: release MoqomKit; apps opt in by passing `keyDelivery` on
   `DeviceCredentials` (`TokenService.keyDelivery` or `HTTPMLSDelivery`).
4. Publish the site after the SDK release, not before.
