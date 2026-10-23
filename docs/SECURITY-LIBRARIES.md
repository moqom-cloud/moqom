# Security libraries

> **Confidential until counsel has reviewed it.** These libraries stay private
> until counsel has decided, per mechanism, what to file and what to publish
> (see [PATENT-ASSESSMENT.md](PATENT-ASSESSMENT.md)). Publishing a library under
> Apache-2.0 grants every user a patent license to whatever it does, so the
> license decision below is also a patent decision.

MOQOM's security stack split into four libraries that other people can build
on, each in Swift, Go and Rust, with one set of test vectors shared by all three
languages. The products keep using them; the libraries are the same code, not a
second copy.

## Why

- **Standing.** Being the reference implementation of post-quantum security for
  MoQ is the "authority" goal. That comes from a correct, tested, open
  implementation of the standards and published numbers, not from new
  algorithms.
- **One implementation per language.** Today SFrame and MLS exist only in
  Swift, tokens are minted only in Go, and the relay verifies them in Rust. Every
  new client language would otherwise reimplement all of it.
- **Closing the gaps the site discloses.** Join tokens are still Ed25519. The
  token library is where they become ML-DSA.

## The four libraries

| Library | Does | Standards | Exists today |
| --- | --- | --- | --- |
| `moqom-token` | Mint and verify join tokens: tenant, room, grants, expiry, `cnf.jkt` device binding, key rotation by key ID | JWS compact form now; CWT/COSE (`draft-ietf-moq-c4m`) as the target encoding; ML-DSA in JOSE/COSE drafts | Go mints and verifies Ed25519 (`moqom/go/internal/token`); Rust verifies in the relay and mints in `moqom-loadgen`; Swift reads claims |
| `moqom-proof` | Device-bound proof of possession: a single-use signature from the device key, bound to the token and the peer's certificate, renewable in-session | DPoP (RFC 9449) shape; JWK thumbprints (RFC 7638) | Swift signs with an ES256 Secure Enclave key; Rust relay and `moqom-client` verify; Go derives the thumbprint |
| `moqom-sframe` | Seal and open MoQ objects: AES-GCM suites, counter nonces, object metadata bound into the tag, replay window | RFC 9605; `draft-ietf-moq-secure-objects` | Swift only (`SFrame.swift`, `MediaProtection.swift`) |
| `moqom-mls` | Room key agreement that produces SFrame keys: key packages, commits, welcomes, epoch authenticator | RFC 9420; `draft-ietf-mls-pq-ciphersuites`; RFC 9605 §5.2 | Swift only (`MLSGroup.swift` and siblings); Go parses key packages and commit headers for the delivery service |

### Algorithms per language

| | Swift | Go | Rust |
| --- | --- | --- | --- |
| ML-DSA-65 | CryptoKit `MLDSA65`, also in the Secure Enclave | `crypto/mldsa` | `aws-lc-rs` `signature::ML_DSA_65` (already a dependency through rustls) |
| ML-KEM-768 | CryptoKit `MLKEM768` | `crypto/mlkem`, `crypto/hpke` | `aws-lc-rs` `kem` |
| X-Wing (hybrid HPKE) | CryptoKit HPKE | `crypto/hpke` `MLKEM768X25519` | to confirm |
| AES-GCM, HKDF, SHA-2 | CryptoKit | standard library | `aws-lc-rs` |

No library implements a primitive. Each composes the platform's audited
implementation, and where a composition is ours (HPKE over pure ML-KEM in Swift,
which CryptoKit's HPKE does not offer) it is checked against another language's
standard library and the working group's vectors.

## Post-quantum by default

- **Tokens:** ML-DSA-65 signing for new keys. Ed25519 stays verifiable during
  the rollover, selected by key ID, so a fleet can rotate without a flag day.
  An ML-DSA-65 signature is 3,309 bytes against Ed25519's 64, so a token grows
  from about 400 bytes to about 4.7 KB in compact JWS. It travels once per
  session and once per renewal; measure the setup cost before deciding whether
  CWT's binary encoding is worth doing at the same time.
- **Proofs:** the Secure Enclave can hold an ML-DSA-65 key from iOS 26 and
  macOS 26. Moving proofs from ES256 to ML-DSA-65 depends on the oldest OS
  supported; until then ES256 remains, and the site says so.
- **SFrame:** unchanged. AES-256-GCM is already quantum-resistant.
- **MLS:** `MLS_192_MLKEM768_AES256GCM_SHA384_MLDSA65` is the only suite new
  groups use, as shipped for launch.

## API shape

Every language gets the same nouns, in its own idiom:

- `moqom-token`: `Signer` (holds a key and key ID), `Verifier` (holds a key
  set), `Claims`, `Grant`. `sign(claims) -> token`, `verify(token, now) ->
  claims`. Verification never returns claims from an unverified token.
- `moqom-proof`: `Prover` (device key), `Checker`. `prove(token, peer
  certificate hash, nonce) -> proof`; `check(proof, token, certificate hash) ->
  ok`. Single use is enforced by the checker's nonce store, which the caller
  supplies.
- `moqom-sframe`: `Context(suite, keyID, baseKey)`, `seal(payload, metadata) ->
  object`, `open(object, metadata) -> payload`. `metadata` is the canonical
  object description, so binding is not optional.
- `moqom-mls`: `Group.create`, `KeyPackage.generate`, `group.commit(adding:,
  removing:)`, `Group.join(welcome)`, `group.exporter(label)`,
  `group.epochAuthenticator`.

## Shared test vectors

One `vectors/` directory, JSON, consumed by all three test suites: token
sign/verify (including tampered and expired cases), proofs bound to the wrong
certificate, SFrame seal/open per suite and per metadata change, and MLS
key schedule and welcome round trips. A library is done in a language when it
passes every vector, not when it passes its own tests. The RFC 9605 and RFC 9420
published vectors are included as they are.

## Licensing and packaging (for counsel)

| Option | Effect |
| --- | --- |
| Apache-2.0 | Maximum adoption and the standing goal; grants a patent license to everything the library does |
| BUSL-1.1, converting to Apache later | Readable, not freely usable in production until conversion; little standing |
| Private | No standing; keeps every option open |

Recommendation from engineering: Apache-2.0 for all four, because each
implements published standards that cannot be claimed anyway, and keep patent
claims on relay and control-plane behaviour that these libraries do not
contain (PATENT-ASSESSMENT candidates 1 and 3). Counsel decides.

Packaging when published: Swift package products in one repository; Go module
`github.com/moqom-cloud/moqom-security/go/...`; Rust crates `moqom-token`,
`moqom-proof`, `moqom-sframe`, `moqom-mls`.

## Order of work

1. Launch first: the ML-DSA MLS suite in Swift and the Go delivery service.
2. `moqom-token` in Go, Rust and Swift, with ML-DSA-65 and the shared vectors.
   Smallest, and closes the token gap.
3. `moqom-sframe` in Rust and Go, extracted from the Swift implementation's
   vectors.
4. `moqom-proof`, after the oldest-OS decision on Secure Enclave ML-DSA.
5. `moqom-mls` in Rust and Go: the largest piece, a port of the Swift group code.

## Open questions

- License per library (counsel).
- Whether any mechanism in `moqom-proof` (single-use, certificate-bound,
  renewable in-session) is to be filed before the library is public; it overlaps
  PATENT-ASSESSMENT candidate 4.
- Oldest Apple OS supported, which decides when proofs can move to ML-DSA.
- CWT/COSE now or later for tokens.
