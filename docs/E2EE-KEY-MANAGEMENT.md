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
| `MLSCipherSuite.swift` | Two suites on CryptoKit: the hybrid default `0xF0A1` (X-Wing, AES-128-GCM, SHA-256, Ed25519) and `0x0001` (X25519, AES-128-GCM, SHA-256, Ed25519); labelled KDF, SignWithLabel, EncryptWithLabel. See "Cipher suites" below. |
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
keys itself), and the framing is our own. The working group publishes these
vectors for suite `0x0001` only. For the hybrid suite,
`MLSHybridSuiteTests` checks CryptoKit's X-Wing HPKE against the HPKE working
group's vector for KEM `0x647a` with HKDF-SHA256 (`hpkewg/hpke-pq`
`test-vectors.json`). That vector uses ChaCha20-Poly1305; none is published
with AES-128-GCM.

### Cipher suites

| Value | Name | Source |
| --- | --- | --- |
| `0xF0A1` (default for new groups) | `MLS_128_MLKEM768X25519_AES128GCM_SHA256_Ed25519` | draft-ietf-mls-pq-ciphersuites-06, suite "TBD1": HPKE KEM `0x647a` (MLKEM768-X25519 in draft-ietf-hpke-pq, the X-Wing KEM of draft-connolly-cfrg-xwing-kem), HKDF-SHA256, AES-128-GCM, Ed25519 |
| `0x0001` | `MLS_128_DHKEMX25519_AES128GCM_SHA256_Ed25519` | RFC 9420 |

`0xF0A1` is a private-use value (RFC 9420 §17.1, `0xF000`–`0xFFFF`) because the
draft's value is still TBD. When IANA assigns one, the constant has to change
in `MLSCipherSuite.swift` and `go/internal/mls/wire.go` together. No other MLS
implementation recognises `0xF0A1`. Interop is not a goal here anyway (see
"Own TLS-style encoding" below).

The two suites differ only in the KEM. The hash, KDF, AEAD and signature are
the same, so the key schedule and the SFrame bridge are unchanged. A group's
suite is fixed when it is founded and is part of its GroupContext, so every
signature and every derived secret is bound to it. Key packages name their
suite. A commit refuses a package in another suite, and a joiner refuses a
Welcome into a group in another suite. There is no migration between suites;
a room changes suite by founding a new group.

**What is post-quantum.** The KEM is a hybrid of ML-KEM-768 and X25519. Every
secret MLS sends is HPKE-sealed to X-Wing: commit secrets to leaf keys, and
joiner secrets to init keys. An attacker who records the handshake therefore
has to break both ML-KEM-768 and X25519 to learn an epoch. Everything derived
from the epoch inherits that, which covers the exporter, the SFrame base key
and per-sender keys, and any other exporter-keyed secret such as the key for
sealed direct-mode candidate offers when it is taken from the group's
exporter. Media is protected by AES-GCM under those keys.

**What is not.** Authentication. MLS signatures (key packages, commits,
GroupInfo) are Ed25519, as the draft's suite specifies. Device keys are P-256
in the Secure Enclave and sign ES256 possession proofs. Participant tokens and
direct tickets are EdDSA (Ed25519) JWTs.
A quantum attacker could forge these, but only while they are being checked;
forging one later does not decrypt recorded traffic. The draft's suites with
ML-DSA signatures were not used. The one with a hybrid KEM needs ML-DSA-44,
which CryptoKit (iOS/macOS/tvOS/visionOS 26.2 SDK) does not offer; it has only
ML-DSA-65 and -87. The others use pure ML-KEM, which CryptoKit's HPKE does not
offer as a KEM. TLS and QUIC to the relay and control plane are outside this
document.

**Sizes.** X-Wing public keys are 1,216 bytes and encapsulations 1,120. A
hybrid key package is about 2.5 KB (it was about 200 bytes). Each member adds
about 1,175 bytes to a commit and about 1,260 bytes to a Welcome, where it was
about 100. The service's limits were raised to match: `maxKeyPackageBytes`
went from 1 KiB to 4 KiB and `maxCommitBytes` from 512 KiB to 1 MiB, which
keeps a room of 800 within bounds. HPKE seals per commit are still linear in
members.

**Negotiation.** A device founds in its own suite, which is the hybrid one by
default (`ManagedRoomKeys(cipherSuite:)`). It publishes key packages in that
suite before it has seen the room. If a sync shows a group in a different
implemented suite, it replaces them with packages in the group's suite and is
added on the committer's next pass. A group in a suite the build does not
implement is not joined.

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
| `PUT /mls/v1/keypackages` | `{keyPackages, lastResort}` → 204. Replaces the device's set; identity must equal the caller; Ed25519 signatures checked; every package in one known suite, keys the suite's length, X-Wing's ML-KEM half checked with `crypto/mlkem`. |
| `POST /mls/v1/keypackages/claim` | `{device}` → `{keyPackage}`. Caller must be a member; target must be pending. One-time first, then the never-consumed last resort. 409 if the package is not in the group's suite (it is still consumed). |
| `GET /mls/v1/sync?after=&wait=` | Long poll ≤ 25 s → `{group: {id, epoch, cipherSuite}, messages, members, pending, cursor}`. Welcomes only to their recipients. |
| `POST /mls/v1/group` | `{groupId, cipherSuite}` → 201, or 409 if a group exists, or 400 for an unknown suite. An absent `cipherSuite` means `0x0001`, which is what clients from before suites existed founded. |
| `POST /mls/v1/commits` | `{epoch, commit, welcome, joiners, removed}` → `{seq}`, or 409 stale. Caller must be a member; group id and epoch read from the commit header. The sender's signature is checked by the members, which hold the leaf keys. |
| `POST /mls/v1/rejoin` | Leaf is stale; next commit replaces it. |
| `DELETE /mls/v1/presence` | Leaving; next commit removes it. |

Rules: one group per room; a commit is accepted only at the current epoch,
first wins; `seq` is monotonic per room and survives a group being dropped;
a group with no online member (heard from within 30 s) is dropped so the next
device founds a fresh one; messages older than 10 minutes are pruned.

Stores: `mls.Memory` and `mls/postgres` (tables `mls_rooms` with
`cipher_suite`, default 1, added by `ALTER TABLE … ADD COLUMN IF NOT EXISTS`, `mls_messages`,
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
   created at startup. No new environment variables. Deploy it before an SDK
   that founds in the hybrid suite: an older service refuses hybrid key
   packages and does not record a group's suite.
2. Load balancer / ingress for the join listener: long polls hold a request for
   up to 25 s; any idle timeout must exceed ~35 s.
3. SDK: release MoqomKit; apps opt in by passing `keyDelivery` on
   `DeviceCredentials` (`TokenService.keyDelivery` or `HTTPMLSDelivery`).
4. Publish the site after the SDK release, not before.
