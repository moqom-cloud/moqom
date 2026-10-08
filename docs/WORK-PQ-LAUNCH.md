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
| Rust X-Wing HPKE (suite 0xF0A1, legacy groups only) | to do | not used for new groups |
| Review the unsafe ML-KEM seed load through aws-lc-sys | to do | before going public |
| Licence check on copied vectors (RFC 9605, mlswg) | blocked | counsel; fallback is fetch-at-test-time |

## Products

| Item | Status | Notes |
| --- | --- | --- |
| Control plane, relay, client, loadgen, recorder, Swift SDK on the libraries | done | one commit per worktree |
| /join refuses a bare thumbprint (every relay now requires a proof) | done | ea277b2; join-endpoint doc updated |
| Loadgen refuses runs its 15-minute tokens can't outlast | done | engine 616026f |
| Loadgen renews tokens mid-run, for soak tests past 15 minutes | to do | `Watching::renew` exists in moqom-client |
| Relay, control plane, director, ingest, recorder, congestion controller: proprietary, binary only | done | moqom 4ad2e6a, engine cdb82fc; `LICENSES/LicenseRef-MOQOM-Proprietary.txt`; sites updated (notices, self-hosting, open-source). Counsel to review the text; `legal@moqom.cloud` must exist |
| Direct paths wired into `Room` (Swift) and the Rust client | in progress | direct-mode agent (restarted after a dropped session) |
| `directPolicy` with `.relayOnly`; 1:1 calls point to point by default | in progress | direct-mode agent |
| Small private rooms point to point (mesh up to a limit) | in progress | direct-mode agent |
| Swift dependency on moqom-security by URL and tag, not path | to do | at publication: needs the repo public and tagged |
| `docs/post-quantum.md` | done | |

## Release and merge

| Item | Status | Notes |
| --- | --- | --- |
| Other session's `oct-2026-pq-open-work` committed and pushed | blocked | owner: the other session; 12 uncommitted files in `moqom` |
| PRs: open-work into main, then pq-signatures | blocked | after the above; CI is on a billing hold, so local test results go in each PR |
| moqom-security public | blocked | counsel sign-off (patents filed, vector licences) |

## Sites (edited as launched, not published)

| Item | Status | Notes |
| --- | --- | --- |
| Security, DPoP, 1:1 point to point, Cloudflare, libraries, footers, studio cards | done | all four sites build |
| Publish | blocked | after: Hover CAA and DNSSEC, repo public, direct mode shipped, proofs merged |
| six10nine.elliottwave.studio | blocked | doesn't resolve; the studio work page links to it |

## Needs a person

- Hover, all four domains: CAA `0 issue "pki.goog"`, `0 issue "letsencrypt.org"`,
  `0 issuewild ";"`, `0 iodef "mailto:security@moqom.cloud"`; enable DNSSEC; hardware key on
  the account; consider registry lock.
- Counsel: patent filings before anything goes public (absolute novelty outside the US), the
  proprietary licence text, IETF IPR disclosure for PR #1970 and the hop-limit draft, the
  vector licences, and the "first and only platform" claim.
