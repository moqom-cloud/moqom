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
| Room keys (MLS key agreement) | ML-KEM-768 (FIPS 203) | Suite `MLS_192_MLKEM768_AES256GCM_SHA384_MLDSA65` (0xF0AA), the only suite for new groups |
| Group membership (MLS signatures) | ML-DSA-65 (FIPS 204) | Key packages, commits and Welcomes |
| Audience keys for broadcasts | HPKE over ML-KEM-768 | Each epoch's SFrame base key sealed to each viewer |
| Media | SFrame with AES-256-GCM (RFC 9605) | Symmetric; Grover's algorithm halves the strength to 128 bits, which stays out of reach |
| Join tokens | ML-DSA-65 (JOSE `alg` `ML-DSA-65`) | Ed25519 accepted for verification only, during the rollover |
| Device proofs | ML-DSA-65 key in the Secure Enclave | Software key where there is no enclave; ES256 accepted from SDKs released before October 2026 |
| Room control log | ML-DSA-65 | Signed by the control plane |
| Transport (QUIC, TLS 1.3) | X25519MLKEM768 hybrid key exchange | Preferred by the relay; the certificate signature is still classical, see below |

## What is still classical, and why that's acceptable

**The relay's TLS certificate signature.** Certificate authorities don't issue ML-DSA
certificates yet, so the signature in the TLS handshake is ECDSA. That signature only
authenticates the relay for the length of the handshake. It can't be attacked by recording
traffic, and the apps pin the relays' certificates, so a forged certificate isn't accepted
anyway. Media is end-to-end encrypted under post-quantum keys before it reaches a relay, so
even a relay impersonated with a future quantum computer would forward ciphertext it can't
read. We move to ML-DSA certificates when they're issued and supported.

**Rollover verification.** Relays still verify Ed25519 tokens and ES256 device proofs from older
SDKs. New SDKs only create ML-DSA-65 keys and tokens. Verification of the classical forms ends
when the last supported SDK that creates them is retired.

**Legacy MLS suites.** Groups founded before the switch under X-Wing (0xF0A1) or RFC 9420's
suite 1 (0x0001) can still be read; no new group uses them. The Rust library can't seal or open
under X-Wing.

## Sizes

ML-DSA is much larger than ECDSA, and every limit was checked for it:

| Item | Size | Limit |
| --- | --- | --- |
| ML-DSA-65 signature | 3,309 bytes | |
| ML-DSA-65 public key, as a JWK | about 2.7 KB | |
| Device proof | 8,194 bytes (ES256: 506) | 16 KiB in all three libraries |
| Join token | at most 8 KiB | |
| `POST /join` body | | 16 KiB, raised from 4 KiB |
| MOQT SETUP with token and proof | about 16 KB | 65,535-byte parameter limit |

## Where to read more

- [`E2EE-KEY-MANAGEMENT.md`](E2EE-KEY-MANAGEMENT.md): how room keys are made, rotated and
  delivered.
- `moqom-security/docs/moqom-*.md`: each library's wire format, API and error classes.
- [moqom.cloud/security](https://moqom.cloud/security/): the public summary.
