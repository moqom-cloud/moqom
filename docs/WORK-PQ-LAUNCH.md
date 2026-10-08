# Post-quantum launch: work tracker

Living list for the October 2026 launch: post-quantum end-to-end encryption, device-bound
tokens, open-source security libraries, point-to-point calls and the site claims that depend
on them. Branch `oct-2026-pq-signatures` in moqom, moqom-engine, moqom-swift, moqom-go and
moqom-rust; library repo `moqom-security` (private, `main`).

Status: **done** · **in progress** · **to do** · **blocked** (needs a person).

## Libraries (`moqom-security`)

| Item | Status | Notes |
| --- | --- | --- |
| moqom-sframe (Swift, Go, Rust, RFC 9605 vectors) | done | caec138 |
| moqom-token (ML-DSA-65 sign, Ed25519 verify-only, 64 vectors) | done | e2a32b9 |
| moqom-mls (ML-KEM-768 + ML-DSA-65 suite, mlswg vectors) | done | 4875d09 |
| moqom-proof (ML-DSA-65 device proofs, ES256 rollover, 70 vectors) | done | fb6298a |
| SFrame replay window really 1024 | done | 6c5f905; capped at 65536; also fixed a Swift race that could let one counter replay |
| Proof: refuse JWKs carrying private-key members | done | 6a23287 |
| Proof: same certificate-less behaviour in all three languages | done | a71c89d; opt-in `requireCertificate` |
| Proof: Go error class for signing failures | done | 11f8fc0, `ErrSigning` |
| Token: duplicate JSON member names refused in all three languages | done | 9321820; Rust and Go product suites pass against it; Swift checked by the direct-mode run |
| Rust X-Wing HPKE (suite 0xF0A1, legacy groups only) | done | moqom-security a46fa03 (pushed); matches the draft's and the HPKE working group's vectors; Swift, Go and Rust open each other's ciphertexts |
| Review the unsafe ML-KEM seed load through aws-lc-sys | done | moqom-security fb9cec8 (pushed); no memory-safety bug; seed and expanded key now wiped, strict PKCS#8 parse, guard frees the key once |
| Wipe other private keys and shared secrets in Rust moqom-mls | done | moqom-security 04c97f0 (pushed); keys returned by `generate_key_pair` are the caller's to wipe |
| Confirm Swift refuses an X-Wing ciphertext whose X25519 half is all zero | done | CryptoKit refuses it (CoreCrypto error -7), so all three languages agree |
| Licence check on copied vectors (RFC 9605, mlswg) | blocked | counsel; fallback is fetch-at-test-time |

## Products

| Item | Status | Notes |
| --- | --- | --- |
| Control plane, relay, client, loadgen, recorder, Swift SDK on the libraries | done | one commit per worktree |
| /join refuses a bare thumbprint (every relay now requires a proof) | done | ea277b2; join-endpoint doc updated |
| Loadgen refuses runs its 15-minute tokens can't outlast | done | engine 616026f |
| Loadgen renews tokens mid-run, for soak tests past 15 minutes | done | engine 7c751fb; adds `Subscription::renew` and `Announcement::renew` to moqom-client, with a test |
| Relay, control plane, director, ingest, recorder, congestion controller: proprietary, binary only | done | moqom 4ad2e6a, engine cdb82fc; `LICENSES/LicenseRef-MOQOM-Proprietary.txt`; sites updated (notices, self-hosting, open-source). Counsel to review the text; `legal@moqom.cloud` must exist |
| Direct paths by default in Swift rooms: `DirectPolicy`, room policy, relay ticket refusal, backstage | done | swift 55f9ae6, pq 1a69af7, engine 3149af4, go 031894e, rust 7e08b34; Swift 1021 and Rust 521 tests pass |
| Relays answer STUN and SDKs use it by default, so direct paths cross NATs | done, not deployed | relay responder on UDP 3478, `DirectGrant.stun` from `MOQOM_CONTROL_STUN`, Swift and Rust ask it by default; deploy config opens 3478 but has not been applied. Symmetric NATs still fall back to the relay (no TURN) |
| Relays on IPv6: dual-stack subnet, IPv6 forwarding rules for QUIC and STUN, IPv6 firewall twins and egress deny, AAAA steering | done, not applied | pq 0972952, ef96ac2; engine 29088bc makes `[::]` dual stack; dev plan: 26 add, 5 change, 2 destroy, relay templates replaced (rolling) |
| `terraform validate` and dev `plan` | done | both envs valid; dev plan clean with `-lock=false`; prod has never been initialised (no backend or tfvars), so it can't be planned |
| Field test of direct paths on real networks | in progress | `moqom-nat-probe` (engine 8c63efb) and `docs/FIELD-TEST-DIRECT.md`; first network: symmetric on IPv4, punchable on IPv6. Needs a person on other networks, and the relays deployed for real calls |
| Rust client: direct paths wired into a room, relay copy paused once direct, Swift `.custom` uses the grant's STUN | done | engine 08ff794, 328ec89 (`DirectRoom`, tickets enforced in Rust); swift 4befce2, 6132a8f; Rust client and relay 545 tests, Swift 1029 pass. The Rust caller supplies the roster and offer key (no MLS or catalog in Rust) |
| Backstage moves tested against a relay that enforces the backstage grant | done | swift d30ec7a (six MoqomKit fixes: backstage direct tickets, retried offer requests, revocation after REQUEST_OK, own and ended-backstage roster entries, catalog re-send), 18dd1f5 (`BackstageRelayTests` on moqom-relay, going live by renewal and by rejoin); full suite 1,027 tests pass. Not covered: automatic reconnection, audio-only go-live |
| Postgres test for `direct_paths` and `max_direct_participants` | done | pq 5acbad5; conformance case and a migration test, run on a local Postgres 17 with every other Go test |
| `directPolicy` with `.relayOnly`; 1:1 calls point to point by default | in progress | direct-mode agent |
| Small private rooms point to point (mesh up to a limit) | in progress | direct-mode agent |
| Swift dependency on moqom-security by URL and tag, not path | to do | at publication: needs the repo public and tagged |
| `docs/post-quantum.md` | done | |

## Security libraries 0.1.0

| Item | Status | Notes |
| --- | --- | --- |
| moqom.dev page per library, with examples the test suites run | done | token, proof, sframe, mls pages; examples run in all three suites |
| SFrame binds the full track name (a relay could move an object between one sender's tracks) | done | moqom-security 7e70980, d5170f2; swift b13e6b8 (also fixed two nonce-reuse bugs: shared-key KIDs and counters restarting on rekey) |
| Pre-release parity: strict JSON in token and proof, typed errors, `configuration` checks, raw key lengths, MLS `invalid-length`, Swift byte-for-byte string compares | done | moqom-security 9422a91 through c0caafa; 120 token, 49 key-set, 104 proof vectors |
| CHANGELOG, README install lines, Versions section on moqom.dev | done | moqom-security 1f0b495 |
| Push main, tag `v0.1.0` and `go/v0.1.0` (repo stays private) | done | at c0caafa. Library suites: Swift 109, Rust 70, Go all with -race. Products against it: engine 963, Swift SDK 1029, Go control plane and SDK all pass. Known gap: public JWK types decode with each language's own JSON decoder |
| crates.io publish, repo public | blocked | release day, with counsel sign-off |

## Release and merge

| Item | Status | Notes |
| --- | --- | --- |
| Other session's `oct-2026-pq-open-work` committed and pushed | blocked | owner: the other session; 12 uncommitted files in `moqom` |
| PRs: open-work into main, then pq-signatures | blocked | after the above; CI is on a billing hold, so local test results go in each PR |
| moqom-security public | blocked | counsel sign-off (patents filed, vector licences) |

## Sites (edited as launched, not published)

| Item | Status | Notes |
| --- | --- | --- |
| Security, DPoP, 1:1 point to point, Cloudflare, libraries, footers, studio cards, licensing, changelog | done | all four sites build |
| Publish | blocked | after: Hover CAA and DNSSEC, repo public, direct mode shipped, proofs merged |
| six10nine.elliottwave.studio | blocked | doesn't resolve; the studio work page links to it |

## Needs a person

- Hover, all four domains: CAA `0 issue "pki.goog"`, `0 issue "letsencrypt.org"`,
  `0 issuewild ";"`, `0 iodef "mailto:security@moqom.cloud"`; enable DNSSEC; hardware key on
  the account; consider registry lock.
- Counsel: patent filings before anything goes public (absolute novelty outside the US), the
  proprietary licence text, IETF IPR disclosure for PR #1970 and the hop-limit draft, the
  vector licences, and the "first and only platform" claim.

## Open questions (after the STUN, backstage and Rust room work)

- Apply dev (rolls the relay VMs and also applies the other session's recorder resources), or
  wait for the other session?

- Direct paths that hole-punching can't open (symmetric NATs, some cellular carriers) fall back to
  the relay. Is that enough, or should MOQOM offer TURN?
- Should relays answer STUN on the QUIC port instead of 3478, so one firewall rule covers both?
- The IP-address notice before a direct call is the app's job today. Should MoqomKit ship a
  default notice or require the app to confirm it showed one?
- Backends that mint through the admin API must pass `MintClientTokenResponse.direct` on. Should
  the token itself carry the grant, so nothing can drop it?
- `deploy/tools/conformance.sh` resets the dev Cloud SQL `postgres` password on every run. Keep
  that, or document running it against a local Postgres as done here?
- The Rust `DirectRoom` still lacks parts of MoqomKit's: MLS and the catalog (the caller supplies
  the roster, opt-ins and offer key), streaming direct groups (only whole subgroups), reacting to
  network changes, per-path byte counters, and a test of the 500 ms quiet resume. The room must
  also be created before the participant's slot is announced. Which of these matter before the
  Rust SDK is documented on moqom.dev?
- Backstage go-live is untested with automatic reconnection and with audio only.
