# Security model: MOQOM compared with other MoQ and real-time video stacks

> **Before publishing: check against `PATENT-ASSESSMENT.md`.** This document is
> written to be adapted for public use, and it describes several protections at
> outcome level only, on purpose. Do not add mechanism detail for anything that
> document marks *File*, *Decide first* or *Trade secret* until counsel clears
> it. Also remove this box, the "Editor notes" section at the end, and any
> environment-variable names before it leaves the company. Re-verify every
> third-party claim on the day of publication; the access dates below are
> 6 October 2026.

Owner: Elliott Wave LLC. Status: draft for review, October 2026.

## How to read this

- **Shipped** means code that exists, is tested, and runs in MOQOM's deployed
  development environment today. **Planned** means designed or scheduled, with
  no code yet, or code that is not on by default.
- For other products, every row cites the public documentation it was read
  from. **Documented: yes** means the vendor's own docs describe the feature;
  **not documented** means we looked and found nothing in their public docs.
  "Not documented" does not mean a product lacks the feature, only that we could
  not confirm it.
- This is a comparison of documented defaults and features, not a security
  rating of anyone. Every project here is moving fast, and three of them track a
  different revision of the same IETF draft.

## Summary

MOQOM targets [`draft-ietf-moq-transport-19`][moqt19] and ships these by
default on its deployed relays and control plane:

- **TLS 1.3 on every media connection** (inherent in QUIC and WebTransport).
- **Short-lived signed tokens**, at most 15 minutes, scoped to one tenant, one
  room and a set of grants, checked against every namespace each request names.
  The relay closes a session when its token lapses, and a client can renew
  without reconnecting.
- **Device-bound tokens.** A token names the device it was issued to, and
  MOQOM's deployed relays refuse a session that cannot prove it holds that
  device's key. On Apple devices the key lives in the Secure Enclave and cannot
  be exported. A copied token is useless on its own.
- **Revocation that reaches live sessions.** A ban closes the participant's open
  connection, not just their next join attempt.
- **Issuer pinning.** A relay accepts tokens only from the issuer it is
  configured for.
- **Mutually authenticated relay-to-control-plane link**, using TLS in both
  directions with a private certificate authority. No plaintext fallback on the
  deployed port.
- **No stored secrets in plaintext.** API keys are stored only as SHA-256
  hashes and compared in constant time. The database uses cloud IAM
  authentication, so there is no database password at all.
- **Verified billing webhooks.** Stripe webhooks are checked by HMAC-SHA256
  with a five-minute replay window, and every ledger entry carries a unique
  idempotency key, so a replayed delivery cannot credit an account twice.

MOQOM also provides, **required by default**: frame-level end-to-end encryption
with SFrame ([RFC 9605][rfc9605]) in the Apple SDK (`MoqomKit`), using
AES-256-GCM by default. Room keys are **managed with MLS** (RFC 9420): devices
agree them through a delivery service on the control plane that orders handshake
messages and never holds a media key, and removing a device starts a new epoch.
An application may still supply a shared room key instead. Sender keys for
large audiences are **planned**, not built. See
[E2EE-KEY-MANAGEMENT.md](E2EE-KEY-MANAGEMENT.md).

## Comparison table

All third-party cells: accessed 6 October 2026. Sources are listed below the
table, keyed by the bracketed letters.

| | **MOQOM** | **moq-dev / moq-relay** (kixelated) | **Cloudflare MoQ relays** | **moxygen** (Meta) | **LiveKit** (WebRTC SFU) | **Agora** |
|---|---|---|---|---|---|---|
| Protocol | MOQT draft-19 | moq-lite, a subset of moq-transport; can negotiate moq-transport [A] | MOQT draft-14 and draft-16; draft-18 in testing [D][E] | MOQT draft-16 and draft-18 [G] | WebRTC | Proprietary (SD-RTN) |
| Transport encryption | TLS 1.3 (QUIC / WebTransport) | TLS 1.3 over QUIC; also plaintext TCP and Unix-socket listeners for local gateways [B] | TLS 1.3 (QUIC) | TLS (QUIC); `--insecure` flag documented for self-signed testing only [G] | DTLS-SRTP [J][O] | Not documented in sources reviewed |
| Client credential | Ed25519-signed token, 15-minute cap | JWT (HMAC, RSA, ECDSA or EdDSA), or an auth-server grant [B] | Cloudflare-signed JWT [E] | Not documented [G][H] | JWT signed with API secret [K] | AccessToken2, HMAC-SHA256, valid 24 hours at most [Q] |
| Where the credential travels | In-band, in the MOQT SETUP message | `?jwt=` query string or SETUP AUTHORIZATION TOKEN [B] | URL path; docs note it appears in server access logs [D] | Not documented | Signalling connection | SDK join |
| Scope of a credential | Tenant + room + grants, checked per namespace on every request | Publish and subscribe path patterns under a root [B] | Whole relay, publish and/or subscribe [E] | Not documented | Room + grants [K] | Channel + user ID + role [Q] |
| Session closed when credential expires | Yes | Yes, `exp` enforced for the whole session [B] | Not documented | Not documented | No: expiry affects the initial connection only, not reconnects [K] | Not documented in sources reviewed |
| Renew without reconnecting | Yes | Relay re-checks on a cadence via the auth server [B] | Not documented | Not documented | Server issues refreshed tokens to connected clients [K] | Not documented in sources reviewed |
| Proof of possession (token bound to a device key) | **Yes**, enforced on MOQOM-operated relays; hardware key on Apple | Not documented | Not documented | Not documented | Not documented | Not documented |
| Kick closes the live session | Yes | Yes, via a revalidate push to the auth server [B] | Not documented (tokens are revocable individually [E]) | Not documented | Yes, `RemoveParticipant`; token also revoked on LiveKit Cloud only [L] | Yes, REST "kicking rule"; docs note a kick can miss a client that is momentarily disconnected from the edge [R] |
| End-to-end media encryption | SFrame (RFC 9605), opt-in, Apple SDK | `moq-e2ee`: AES-128-GCM, opaque track names, not SFrame [C] | Not documented | Not documented | Optional, AES-GCM frame encryption [M] | App-keyed media encryption, including salted AES-GCM modes [P] |
| E2EE key distribution provided | Yes: MLS-managed per room (Apple SDK); shared key as an override | No: app distributes the secret [C] | n/a | n/a | No: app distributes keys [M] | No: app supplies key and salt [P] |
| Relay ↔ relay / control-plane auth | Mutual TLS, private CA | Mutual TLS or JWT between cluster peers [B][F] | Not documented | Not documented | n/a | n/a |
| Webhook / callback signing | Inbound Stripe webhooks verified (HMAC-SHA256, 5-minute window). MOQOM sends no outbound webhooks; events stream over the authenticated admin API | n/a | n/a | n/a | JWT in `Authorization` header with a SHA-256 hash of the body [N] | HMAC-SHA1 and HMAC-SHA256 signature headers [S] |

### What the IETF drafts say

- **MOQT draft-19** leaves both authorization and media encryption to other
  documents. Section 13.3 says MOQT supports mutual TLS and token schemes, and
  names Privacy Pass ([draft-ietf-moq-privacy-pass-auth][ppa]) and CAT-based
  tokens ([draft-ietf-moq-c4m][c4m]). Section 13.3.1 makes replay protection
  "the responsibility of the specific token scheme". Section 13.4 says
  end-to-end media security is "handled by mechanisms external to this
  specification" and points to [draft-ietf-moq-secure-objects][secobj]. [moqt19]
- **C4M** (draft-ietf-moq-c4m-01, June 2026) defines MOQT authorization on the
  Common Access Token, CTA-5007-B, and includes optional DPoP-style
  proof-of-possession using a `cnf.jkt` confirmation claim
  ([RFC 9449][rfc9449]). [c4m]
- **Secure Objects** (draft-ietf-moq-secure-objects-01, July 2026) is the
  working group's SFrame-based end-to-end scheme for MOQT objects. [secobj]
- **WebRTC** mandates DTLS-SRTP on every media channel
  ([RFC 8827, §6.5][rfc8827]). An SFU terminates that encryption per
  participant, so without frame-level E2EE the server can read media; this is
  the general WebRTC model, not a property of any single vendor.

MOQOM's position relative to these drafts: its tokens are not yet CAT/CWT and
its E2EE is not yet the secure-objects wire format. Both are **planned**
alignments; see "Honest gaps".

## Threat model summary

Each row says what MOQOM ships today and what remains. "Shipped" claims are
limited to MOQOM's deployed configuration unless noted.

| Threat | What MOQOM ships | What remains |
|---|---|---|
| **Stolen client token** (copied from a log, a crash report, a backend, a proxy) | Tokens last at most 15 minutes and are scoped to one tenant, room and grant set. Each names a device, and MOQOM-operated relays refuse a session that cannot prove possession of that device's key. On Apple devices that key is generated in the Secure Enclave and is not exportable. | Possession is required by default on every relay binary (since 2026-10-06). A self-hosted operator can still set `MOQOM_RELAY_POSSESSION=observed` or `bearer`, which logs a warning at startup and on every session admitted without a proof; on such a relay a token is a bearer credential for its remaining lifetime. Android, web and desktop SDKs are not released, so their key storage is not yet shipped. |
| **Replay** (of a token, a proof or a request) | Possession proofs are single-use and tied to the token they accompany. Expired tokens are refused; clock skew tolerance is one-directional, 30 seconds. Admin mutations take idempotency keys. | A relay that restarts forgets which proofs it has seen, so an undelivered captured proof could be replayed within its token's remaining minutes. Capturing one requires breaking TLS first. |
| **Relay compromise** (attacker controls a relay process) | Relays hold no token-signing keys; they receive only verification keys, from a control plane they authenticate with mutual TLS. A relay cannot mint tokens. With SFrame enabled, a relay sees only ciphertext and cannot alter an object's position, priority or properties without failing authentication. | Without SFrame, a relay sees media in the clear, like every SFU and every MoQ relay. Managed E2EE keys are planned. A compromised relay can still drop, delay or withhold media; no transport prevents that. |
| **Malicious relay in the path** (a third-party or misconfigured relay between publisher and viewer) | Same as above with SFrame: confidentiality and integrity of payload and position metadata. Issuer pinning means a relay accepts only MOQOM-issued tokens for its deployment. | Track and namespace names, object sizes and timing are visible to any relay, as MOQT §13.2 and §13.4 note for all implementations. |
| **Leaked API key** | Keys carry a fixed `mqk_` prefix so secret scanners can catch them, are shown once, stored only as SHA-256 hashes, compared in constant time, and revocable instantly. Self-hosted relays get a separate key type that can only report usage. Every authentication failure returns the same error. | A backend key administers its whole tenant, so a leak is serious until revoked. There is no per-key IP allowlist yet. |
| **Tenant breakout** | Every token names a tenant, and the relay checks it against every namespace a request names. Every admin call names a tenant that must match the caller's credential; resources in other tenants return "not found" rather than "forbidden". The CI deployer cannot create identities or edit IAM. | Tenancy is logical, in shared processes and a shared database. Per-tenant infrastructure isolation is not offered. |
| **Webhook forgery** | Inbound Stripe webhooks are verified with HMAC-SHA256 over the timestamp and body, with a five-minute window, using a constant-time compare. Ledger entries carry unique idempotency keys, so a replayed or duplicated delivery cannot credit twice. | Billing is built but not yet switched on in production. MOQOM sends no outbound webhooks today, so there is nothing for a customer to verify; if they are added, they must be signed. |

## Honest gaps

These are known and stated so nobody has to discover them. Each is either
planned or deliberately deferred.

- **No WAF or Cloud Armor.** Exactly one control-plane port faces the internet,
  the device join endpoint. It is rate-limited per credential and caps request
  bodies, but nothing in front of it rate-limits per source address or blocks
  by geography. That needs a global load balancer in front, which is planned.
- **One control-plane replica.** The registry of which relay is attached to
  which control-plane process is in memory, so the deployment runs one replica
  on purpose. A second would let a moderation call miss relays attached to the
  other one. High availability waits on a shared registry.
- **Production is not deployed.** The development environment is live and is
  where every "shipped" claim here was observed. The production configuration
  validates but has never been applied.
- **Secure defaults are not yet universal defaults.** The relay binary's
  defaults suit benchmarks and gradual fleet upgrades, not the internet. With no
  keys configured it authorizes nothing. Possession enforcement is opt-in for
  self-hosters. The control plane's store defaults to memory, so a restart
  forgets bans; the deployed environment uses Postgres. Self-hosters must
  configure all three.
- **The relay admin port is unauthenticated.** It is off by default and must be
  bound to a private interface if enabled.
- **Token format is MOQOM-specific.** Tokens are Ed25519 over JSON, not CAT
  over CBOR/COSE, and are not interoperable with C4M relays. Moving to CAT is
  planned.
- **E2EE group keys are a simplified MLS.** RFC 9420's primitives and key
  schedule (checked against the official test vectors) with a flat member list
  instead of a ratchet tree, so a commit costs one HPKE seal per member, and
  MOQOM's own message encoding, so it does not interoperate with other MLS
  stacks. Senders within a room are separated by KID, not by signature, so any
  member can still forge media as another. Sender keys for large rooms are not
  built. The SFrame framing is not yet the secure-objects wire format.
- **No organisation policies, VPC Service Controls or customer-managed
  encryption keys.**
- **No synthetic media monitoring.** Uptime checks cover the control plane
  only.
- **Regional failover is manual**, bounded below by DNS TTL.
- **The CI deployer's broadest role** can roll instance groups. Narrowing it
  needs a custom role, which is on the list.
- **No independent audit or penetration test yet.** The vulnerability
  disclosure program is the first external check.
- **Interop against other MoQ implementations is untested.** Our two
  implementations (Rust and Swift) agree with each other; testing against
  moq-rs and moxygen is a milestone.

## Reporting a vulnerability

Email **security@moqom.cloud**. See the
[responsible disclosure policy](https://moqom.dev/security/disclosure/).

## Sources

All accessed 6 October 2026.

**IETF and standards**

- [moqt19] draft-ietf-moq-transport-19, 6 July 2026, §§2.5, 10.2, 10.3, 13, 15:
  <https://datatracker.ietf.org/doc/draft-ietf-moq-transport/19/>. Note: the
  current revision is -22 (1 October 2026):
  <https://datatracker.ietf.org/doc/html/draft-ietf-moq-transport>
- [c4m] draft-ietf-moq-c4m-01, 18 June 2026, §3 (DPoP integration):
  <https://datatracker.ietf.org/doc/draft-ietf-moq-c4m/>
- [ppa] draft-ietf-moq-privacy-pass-auth-03:
  <https://datatracker.ietf.org/doc/draft-ietf-moq-privacy-pass-auth/>
- [secobj] draft-ietf-moq-secure-objects-01, 6 July 2026:
  <https://datatracker.ietf.org/doc/draft-ietf-moq-secure-objects/>
- [rfc9605] RFC 9605, Secure Frame (SFrame): <https://www.rfc-editor.org/rfc/rfc9605>
- [rfc9449] RFC 9449, OAuth 2.0 DPoP: <https://www.rfc-editor.org/rfc/rfc9449>
- [rfc8827] RFC 8827, WebRTC Security Architecture: <https://www.rfc-editor.org/rfc/rfc8827.html>
- CTA-5007-B, Common Access Token (as cited by C4M): <https://shop.cta.tech/products/cta-5007>

**Implementations and vendors**

- [A] moq-dev/moq README: <https://github.com/moq-dev/moq>
- [B] moq-relay authentication docs: <https://doc.moq.dev/bin/relay/auth>
- [C] `moq-e2ee` crate and draft-lcurley-moq-e2ee-00:
  <https://crates.io/crates/moq-e2ee>,
  <https://datatracker.ietf.org/doc/html/draft-lcurley-moq-e2ee-00>
- [D] Cloudflare MoQ docs, last updated 31 July 2026: <https://developers.cloudflare.com/moq/>
- [E] Cloudflare blog, "An API for MoQ: provision your own isolated relays",
  31 July 2026: <https://blog.cloudflare.com/moq-relays/>; token API:
  <https://developers.cloudflare.com/api/resources/moq/subresources/relays/subresources/tokens/methods/create/>
- [F] moq-relay clustering docs: <https://github.com/moq-dev/moq/blob/main/doc/bin/relay/cluster.md>
- [G] moxygen README: <https://github.com/facebookexperimental/moxygen>
- [H] moxygen ARCHITECTURE.md: <https://github.com/facebookexperimental/moxygen/blob/main/ARCHITECTURE.md>
- Cloudflare launch blog (August 2025, unauthenticated preview):
  <https://blog.cloudflare.com/moq/>; <https://moq.dev/blog/first-cdn/>
- [J] LiveKit security overview: <https://livekit.com/security/overview>
- [K] LiveKit tokens and grants: <https://docs.livekit.io/frontends/reference/tokens-grants/>
- [L] LiveKit participants, `RemoveParticipant`:
  <https://docs.livekit.io/intro/basics/rooms-participants-tracks/participants/>
- [M] LiveKit encryption: <https://docs.livekit.io/transport/encryption/>
- [N] LiveKit webhooks: <https://docs.livekit.io/intro/basics/rooms-participants-tracks/webhooks-events/>
- [O] General SFU model, RFC 8827 §6.5 (above)
- [P] Agora media stream encryption (Web):
  <https://docs.agora.io/en/realtime-media/video/build/secure-and-protect-channels/media-stream-encryption/web>
- [Q] Agora token server, AccessToken2:
  <https://docs.agora.io/en/realtime-media/rtc/build/authenticate-users/deploy-token-server>
- [R] Agora banning rules and best practices:
  <https://docs.agora.io/en/api-reference/api-ref/rtc/create-ban-rule>,
  <https://docs.agora.io/en/api-reference/api-ref/rtc/ban-user-privileges-best-practices>
- [S] Agora notification signatures:
  <https://docs.agora.io/en/realtime-media/media-pull/build/receive-notifications>

## Editor notes (remove before publishing)

Internal evidence for each MOQOM claim, so a reviewer can check it without
asking:

- Token cap and default: `moqom/go/internal/token/token.go` (`MaxLifetime`,
  `DefaultLifetime`). Algorithm and type pinning, 30-second skew:
  `moqom-engine/moqom-relay/src/identity.rs`.
- Possession proofs, single use, residual on restart:
  `moqom-engine/moqom-relay/src/possession.rs`; Secure Enclave key:
  `moqom-swift/Sources/MoqomKit/Security/DeviceKey.swift`; dev relays run
  `MOQOM_RELAY_POSSESSION=required` and pin `MOQOM_RELAY_ISSUER`
  (`moqom/deploy/envs/dev/main.tf`).
- Mutual TLS link, IAM database auth, CI least privilege, no WAF:
  `moqom/deploy/README.md`. Single replica: `moqom/deploy/envs/dev/main.tf`
  (`replicas = 1`, with the reason in a comment).
- API keys: `moqom/go/internal/billing/keys.go`. Stripe webhook and ledger
  idempotency: `stripe.go`, `ledger.go`, `postgres/schema.sql` (unique
  `idempotency_key`). Billing not live in prod: `OPEN-CORE.md` checklist.
- SFrame: `moqom-swift/Sources/MoqomKit/Security/SFrame.swift` and
  `MediaProtection.swift`. It is opt-in (`RoomSession.useEncryption`) and the
  only key mode is `RoomEncryption.sharedKey`.
- Rows of `PATENT-ASSESSMENT.md` this document touches at outcome level only:
  4 (device-bound possession proofs) and 5 (in-session revocation). Do not add
  how the proof is bound, when it is renewed, or how a ban reaches the relay.
- `moqom.dev/security/overview` lists watchdogs as a shipped control;
  `PATENT-ASSESSMENT.md` row 15 marks watchdogs as pending. Reconcile before
  either page is quoted.
