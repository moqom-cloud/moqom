# Post-quantum cryptography in MOQOM

Where MOQOM uses post-quantum algorithms, what is still classical, and why. The code that
implements all of it is open source in
[`moqom-security`](https://github.com/moqom-cloud/moqom-security): `moqom-token`,
`moqom-proof`, `moqom-sframe` and `moqom-mls`, in Swift, Go and Rust, with shared test vectors.

## The threat

Two different attacks matter, and they need different defences:

- **Record now, decrypt later.** Someone records encrypted traffic today and decrypts it once a
  large quantum computer exists. This breaks key agreement (ECDH), so it has to be fixed *now*,
  for media and keys that must stay private for years.
- **Forgery once a quantum computer exists.** Breaking a signature (ECDSA, Ed25519) lets an
  attacker forge tokens or group membership, but only from the day the computer exists. Moving
  signatures early means nothing has to be rushed then.

## Where each algorithm is used

| What | Algorithm | Notes |
| --- | --- | --- |
| Room keys ([MLS](https://www.rfc-editor.org/rfc/rfc9420) key agreement) | ML-KEM-1024 ([FIPS 203](https://csrc.nist.gov/pubs/fips/203/final)) | Suite `MLS_256_MLKEM1024_AES256GCM_SHA512_MLDSA87` (private-use 0xF0AC), the only suite; NIST category 5 throughout |
| Group membership (MLS signatures) | ML-DSA-87 ([FIPS 204](https://csrc.nist.gov/pubs/fips/204/final)) | Key packages, commits and Welcomes |
| Audience keys for broadcasts | [HPKE](https://www.rfc-editor.org/rfc/rfc9180) over ML-KEM-1024 | Each epoch's [SFrame](https://www.rfc-editor.org/rfc/rfc9605) base key sealed to each viewer |
| Media | SFrame with AES-256-GCM (RFC 9605) | Symmetric; Grover's algorithm halves the strength to 128 bits, which stays out of reach |
| Join tokens | ML-DSA-87 (JOSE `alg` `ML-DSA-87`) | The only algorithm accepted; Ed25519 and ML-DSA-65 tokens are refused |
| Device proofs | ML-DSA-87 key in the Secure Enclave | Software key where there is no enclave; ES256 and ML-DSA-65 proofs are refused |
| Room control log | ML-DSA-87 | Signed by the control plane |
| Direct tickets | ML-DSA-87 | Signed by the relay fleet's ticket keys |
| Transport ([QUIC](https://www.rfc-editor.org/rfc/rfc9000), [TLS 1.3](https://www.rfc-editor.org/rfc/rfc8446)) | [X25519MLKEM768](https://datatracker.ietf.org/doc/draft-ietf-tls-ecdhe-mlkem/) hybrid key exchange | Preferred by the relay; the certificate signature is still classical, see below |

## What is still classical, and why that's acceptable

**The relay's TLS certificate signature.** Certificate authorities don't issue ML-DSA
certificates yet, so the signature in the TLS handshake is ECDSA. That signature only
authenticates the relay for the length of the handshake. It can't be attacked by recording
traffic, and the apps pin the relays' certificates, so a forged certificate isn't accepted
anyway. Media is end-to-end encrypted under post-quantum keys before it reaches a relay, so
even a relay impersonated with a future quantum computer would forward ciphertext it can't
read. We move to ML-DSA certificates when they're issued and supported.

**No rollover verification.** Every signature MOQOM makes is
[ML-DSA-87](https://csrc.nist.gov/pubs/fips/204/final), NIST category 5, the same level as MLS. Nothing
verifies Ed25519, ES256 or ML-DSA-65: a device with a key of another kind makes a new one and is
enrolled again.

## Sizes

ML-DSA is much larger than ECDSA, and every limit was checked for it:

| Item | Size | Limit |
| --- | --- | --- |
| [ML-DSA-87](https://csrc.nist.gov/pubs/fips/204/final) signature | 4,627 bytes | |
| ML-DSA-87 public key, as a [JWK](https://www.rfc-editor.org/rfc/rfc7517) | about 3.5 KB | |
| Device proof | about 11 KB | 16 KiB in all three libraries |
| Join token | about 6.7 KB | 8 KiB |
| Direct ticket | about 6.5 KB | 8 KiB |
| `POST /join` body | | 16 KiB, raised from 4 KiB |
| MOQT SETUP with token and proof | about 18 KB | 65,535-byte parameter limit |
| MLS key package (two ML-KEM-1024 keys, an ML-DSA-87 key and signature) | about 10.4 KB | 16 KiB |
| MLS commit or Welcome, per member | about 1.7 KB or 4.2 KB | 3.5 MiB, a group of 800 (raised from 3 MiB for ML-DSA-87) |

## Where to read more

- [`E2EE-KEY-MANAGEMENT.md`](E2EE-KEY-MANAGEMENT.md): how room keys are made, rotated and
  delivered.
- `moqom-security/docs/moqom-*.md`: each library's wire format, API and error classes.
- [moqom.cloud/security](https://moqom.cloud/security/): the public summary.
