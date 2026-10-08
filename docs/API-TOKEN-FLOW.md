# API keys and join tokens: flow, security, compliance

Internal. This describes mechanisms, so keep it off the public sites (see
`PATENT-ASSESSMENT.md`). Public copy describes outcomes only.

## The flow

```mermaid
sequenceDiagram
    autonumber
    participant D as Device (MoqomKit)
    participant B as Customer backend
    participant C as moqom-control
    participant R as moqom-relay
    D->>D: Secure Enclave P-256 key (non-exportable)
    D->>B: signs in to the customer's app, sends device public key
    B->>C: MintToken(user, room, grants, device key) with API key mqk_…
    C->>C: resolve key (constant time), tenant in credit, room policy
    C-->>B: token: EdDSA JWS, ≤15 min, cnf.jkt = device thumbprint
    B-->>D: token
    D->>R: QUIC + TLS 1.3, SETUP with token + ES256 proof bound to the token
    R->>R: verify token signature, expiry, scope; proof key = cnf.jkt; proof unused
    R-->>D: session admitted; identity and grants pinned for its life
    D->>B: before expiry, asks for a fresh token
    D->>R: renewal: new token + new proof
```

Three credentials with three holders, none of which can stand in for another:

| Credential | Held by | Lifetime | Can do |
|---|---|---|---|
| API key `mqk_<id>_<secret>` | Customer backend, or an agent with a scoped key | Until revoked, rotated or its optional expiry | Admin API for one tenant, within its kind; mint tokens |
| Join token (JWS) | One device | ≤15 min, default 10 | Join one room with the stated grants |
| Device key (P-256) | Secure Enclave | Device life | Prove the token was issued to this device |

## 1. API keys

`moqom/go/internal/billing/keys.go` (format, hashing, kinds),
`keyauth.go` (every use), `keyservice.go` (issue, rotate), `github.go`
(leak reports).

```go
const keyPrefix = "mqk_"

// HMAC-SHA-256 under the current pepper; the version is stored with the hash.
func (p Peppers) hash(secret string) (hash, version string)

// Every use, gRPC or HTTP, goes through one place, in this order.
func (a *KeyAuth) Authorize(ctx context.Context, presented string, use Use) (Key, error) {
	id, secret, ok := ParseKey(presented)       // ErrInvalidKey
	key, err := a.Store.GetKey(ctx, id)         // ErrInvalidKey
	matched, stale := a.Peppers.verify(key, secret) // constant time
	switch {
	case !matched:                        // ErrInvalidKey, audited bad_secret
	case key.RevokedAsOf(now):            // ErrInvalidKey
	case key.ExpiredAsOf(now):            // ErrKeyExpired
	case !permits(key.Kind, use.Operation): // ErrOutOfScope
	case !allowedFrom(key, use.Address):  // ErrAddressRefused
	case !a.limits.allow(key.ID, key.Limit(), now): // ErrKeyRateLimited
	}
	if stale {
		a.rehash(ctx, key, secret) // legacy or older pepper -> current
	}
	// audited ok
}
```

Reasoning:

- **The secret is never stored.** The stored value is HMAC-SHA-256 of the
  secret under a **pepper** held in Secret Manager (`MOQOM_KEY_PEPPER`, a
  mounted file), not in the database. A dump of `billing_keys` alone cannot be
  used to test a guessed secret. The secret is 256 random bits, so a slow
  password hash would still add nothing and cost latency on every call.
- **Pepper versions.** Each hash records `pepper_version`, an ID derived from
  the pepper (`HMAC(pepper, label)[:12]`), never configured, so two peppers
  cannot share a name. The pepper file lists versions newest first: the first
  hashes, all verify. A hash under a version no longer configured fails closed.
  Rotation is a new line on top; keys move to it at their next use; an old
  version can be removed once no row names it.
- **Migration from bare SHA-256.** Rows written before peppering have
  `pepper_version = ''` and are verified as bare SHA-256, then rehashed under
  the current pepper on the first successful use (`RehashKey`, conditional on
  the old hash, so concurrent replicas agree). With no pepper configured the
  system keeps writing bare SHA-256, so the secret can be created empty and
  filled later without breaking anything. Keys never used again stay bare
  until they are; `SELECT count(*) FROM billing_keys WHERE pepper_version = ''
  AND revoked_at IS NULL` tracks that.
- **The ID is inside the key**, so lookup is one indexed read and the compare
  is against one row.
- **Constant-time compare**, including against an unknown pepper version, so
  timing does not leak how much of a hash matched or which version a key uses.
- **One error before the secret is proven.** Bad format, unknown ID, wrong
  secret and revoked all return `invalid api key`. After the secret is proven
  the refusal is specific (`expired`, out of scope, address, rate): the caller
  holds the key, so the detail helps only them.
- **`mqk_` prefix** lets secret scanners recognise a leaked key, and GitHub's
  partner program reports it (below).
- **Key kinds.** `backend` is the tenant's owner: every admin RPC and route
  except usage, unchanged by any of this. `relay` only reports usage and is
  refused elsewhere as an invalid key. `mint` (agents, MCP) may call
  `MintClientToken`, `ListVerificationKeys`, `POST /join` and the room and
  participant reads, with a ceiling of every publish right and no moderation,
  so the token ceiling in `MintClientToken` refuses moderators and owners.
  `readonly` reads rooms, participants, watchdogs, the moderation log and
  events, and `GET /v1/billing[/ledger]`. Scopes are allowlists of gRPC full
  methods and HTTP route patterns: a method added later is closed to scoped
  keys until listed. Enforced in `billing.Accounts.Lookup`, which the gRPC
  interceptor and the control plane's own HTTP routes call, and in the billing
  HTTP wrapper. The interceptor maps the refusal to `PERMISSION_DENIED` or
  `RESOURCE_EXHAUSTED` (`server.ErrNotPermitted`, `server.ErrRateLimited`).
- **Expiry.** Optional `expires_at`, 1 hour to 3 years ahead, fixed at issue.
  Rotation renews it with the same lifetime from the rotation. Expired keys
  do not count as working for the last-key confirmation.
- **IP allowlists.** Up to 20 CIDR prefixes, stored masked. The address is the
  trusted one: `clientAddress` for HTTP (`X-Forwarded-For` believed only from
  Google's front-end ranges, and only its second-to-last hop), the TCP peer for
  gRPC. An unknown address fails a key that has an allowlist. The control
  plane's own HTTP routes (`/join`, `/v1/rooms`, the live map) are wrapped in
  `billing.Annotate` in `cmd/moqom-control` so they pass the same address.
- **Rate limits.** Token bucket per key, ten seconds of burst, **in each
  replica's memory**: N replicas admit up to N times the rate. It bounds one
  runaway client, not a quota. Scoped kinds default to 600 a minute; backend
  and relay keys have none unless set.
- **Audit of every use.** Key ID, tenant, operation (gRPC method or HTTP route
  pattern, never a path with IDs or a query), trusted source address, outcome,
  counted per minute in `billing_key_use`. Aggregated in memory and written
  every 10 s and at shutdown, so a hot backend writes a few rows a minute and
  the call never waits on the write; a crash loses at most 10 s of counts.
  Uses with the wrong secret are recorded against the key ID (the tenant's to
  see); unknown IDs are not (there is no tenant). The buffer drops new groups
  above 20,000 and logs `api_key_audit_dropped`. Retention 400 days (sweep
  rule `api_key_use`). Tenants read it at `GET /v1/keys/{id}/audit` and
  `GET /v1/audit`. No secret or hash is ever written to it or to a log.
- **Leaked keys.** `POST /v1/github/secret-scanning` verifies GitHub's
  ECDSA P-256 signature over the raw body against the key named by
  `Github-Public-Key-Identifier`, fetched from
  `api.github.com/meta/public_keys/secret_scanning` and cached (5 min to 1 day;
  unknown identifiers refetch at most once a minute). A reported `mqk_` key
  whose secret verifies is revoked at once, bypassing the last-key
  confirmation, audited `leaked_revoked` with where it was found, logged as
  `api_key_leaked`, and the tenant is emailed after the response. The response
  labels each match `true_positive` or `false_positive` by SHA-256 of the
  token, so no secret is echoed.
- **Rotation with overlap.** `RevokedAt` may be in the future, so the old key
  works until the replacement is rolled out. The replacement keeps kind,
  allowlist and rate limit.
- **Shown once**, at creation.

## 2. Join tokens

`moqom/go/internal/token/token.go`

```go
const algorithm = "EdDSA"            // the only one accepted
const tokenType = "MOQT"
const MaxLifetime = 15 * time.Minute

func Mint(key Key, claims Claims, lifetime time.Duration, now time.Time) (string, time.Time, error) {
	if lifetime == 0 {
		lifetime = DefaultLifetime
	}
	if lifetime < 0 || lifetime > MaxLifetime {
		return "", time.Time{}, fmt.Errorf("%w: lifetime %s, maximum %s", control.ErrInvalid, lifetime, MaxLifetime)
	}
	if claims.Confirmation.Thumbprint == "" {
		return "", time.Time{}, fmt.Errorf("%w: token needs a device thumbprint", control.ErrInvalid)
	}
	// ...
}

func (v *Verifier) Verify(token string, now time.Time) (Claims, error) {
	// ...
	if parsed.Algorithm != algorithm { /* ErrUnsupported */ }
	if parsed.Type != tokenType { /* ErrUnsupported */ }
	public, ok := v.keys[parsed.KeyID]
	if !ok { /* ErrUnknownKey */ }
	if !ed25519.Verify(public, []byte(parts[0]+"."+parts[1]), signature) {
		return Claims{}, ErrSignature
	}
	// claims are decoded only after the signature verifies,
	// with unknown fields refused
	// ...
}
```

Claims: `iss`, `sub` (username), `aud`, `jti`, `iat`, `exp`, `cnf.jkt` (device
thumbprint), `moqt` (room, publish and capability bitmasks, owner, backstage,
age-verified), optional `via` (provenance) and `e2ee` (`required` unless the
room relaxes it; absent means required).

Reasoning:

- **Asymmetric signing.** Relays hold public keys only. A compromised relay can
  verify tokens and cannot mint one.
- **Algorithm pinned.** The classic JWT forgeries are `alg: none` and HMAC
  verified with the public key as the secret. Only EdDSA is accepted, so
  neither applies (RFC 8725 §3.1).
- **Explicit type.** `typ: MOQT` stops a JWS minted for something else from
  being replayed at a relay (RFC 8725 §3.11).
- **Signature before claims.** Nothing in an unverified token is read.
- **Strict decoding.** Unknown claims and unknown capability bits are refused,
  so a newer minter and an older verifier cannot disagree silently.
- **Lifetime refused, not clamped.** Asking for a longer token fails loudly.
- **Skew one way.** 30 s for a token from slightly in the future; an expired
  token is never extended.
- **Key ID derived from the key** (`sha256(public)[:8]`), stable across restarts
  and impossible to republish as a fake rotation. The verifier holds a set of
  keys, so rotation has an overlap.
- **Identity derived, not carried.** The device ID comes from `cnf.jkt`, so a
  token cannot name a device its key does not match.
- **Encryption policy is signed.** A client cannot talk itself out of E2EE.

## 3. Proof of possession at the relay

`moqom-engine/moqom-relay/src/possession.rs`

On its own a token would be a bearer credential: any copy works until it
expires. The relay requires, at SETUP and at every renewal, an ES256 signature
from the key named in `cnf.jkt`.

```rust
const ALGORITHM: &str = "ES256";
const PROOF_TYPE: &str = "MOQT-POP";
const MAX_PROOF_BYTES: usize = 2048;
const MAX_REMEMBERED: usize = 100_000;

pub enum Enforcement {
    Bearer,
    Observed,
    #[default]
    Required,
}
```

Checks: algorithm and type; signature; the proof key's thumbprint equals
`cnf.jkt`; the proof commits to this token by hash; it names this relay; its ID
has not been seen.

Reasoning:

- **ES256, not Ed25519.** The Secure Enclave does P-256 only. An Ed25519 key
  would sit in ordinary memory, where whoever dumps the token dumps the key.
- **One proof per session.** A session pins identity and grants once; later
  announces and subscribes are checked against that. Per-request proofs would
  cost an enclave signature per subscribe and prove nothing new.
- **No server nonce.** The client speaks first in MOQT, so a challenge would add
  a round trip to every connection. Binding to the token, minute lifetimes and
  single-use proof IDs give the same freshness.
- **Bounded.** Proofs over 2 KB are refused before parsing; the replay set is
  capped and fails closed.
- **Required by default.** `Bearer` and `Observed` exist only for crossing a
  fleet upgrade.

Residual: a restarted relay forgets which proofs it has seen, so a proof
captured and never delivered could be replayed at that relay within its
token's remaining minutes. Capturing one requires breaking TLS 1.3, which
exposes the session anyway.

## 4. What an attacker gets

| Stolen | Result |
|---|---|
| Key database | SHA-256 hashes of 256-bit secrets. Unusable. |
| A join token | Useless without the device's enclave key; expires in ≤15 min. |
| A relay | Public keys only, cannot mint; media is E2EE by default. |
| A token for room A | Cannot join room B, exceed its grants, or drop encryption. |
| A backend API key | Full control of one tenant until revoked, from anywhere unless it has an allowlist. The credential to protect; see below. |
| A `mint` or `readonly` key | Viewer and publisher tokens, or reads, for one tenant; no moderation, keys or billing. |
| A database dump | Key hashes that cannot be checked without the pepper; no secrets. |

## 5. Hardening from the Oct 9 review

Built (section 1):

1. HMAC-peppered key hashes, pepper in Secret Manager, versioned, with
   upgrade-on-use from bare SHA-256.
2. Scoped keys: `mint` and `readonly`.
3. Optional expiry at creation, renewed by rotation. Not built: expiry **by
   default**, and rotation reminders (the notifier exists; nothing schedules
   them yet).
4. Per-key IP allowlists and per-replica rate limits.
5. Audit log of key use, per minute, 400 days.
6. GitHub secret scanning endpoint. The partner **registration** with GitHub
   is an owner step (regex, URL, contact) and is not done by code.

Not built:

7. Confirm-before-charge for credit-spending actions.
8. Persisting the relay's spent-proof set, if the restart residual is worth it.
9. A rate limit shared across replicas, if the per-replica ceiling proves too
   loose.

## 6. Compliance mapping

Mappings to control language, not certifications. MOQOM is not SOC 2 or
ISO 27001 certified; status is in `docs/compliance/`.

| Framework | Control | How this design addresses it | Status |
|---|---|---|---|
| SOC 2 TSC | CC6.1 logical access | Per-tenant keys, key kinds, scoped tokens | Implemented |
| SOC 2 TSC | CC6.2, CC6.3 provisioning and removal | Revocable keys, rotation overlap | Implemented |
| SOC 2 TSC | CC6.6 boundary protection | Relays verify signatures; admin API refuses relay keys | Implemented |
| SOC 2 TSC | CC6.7 transmission | TLS 1.3 every hop; E2EE by default | Implemented |
| SOC 2 TSC | CC6.1 least privilege | `mint` and `readonly` keys; expiry; IP allowlists | Implemented |
| SOC 2 TSC | CC7.2 monitoring | Metrics for bad signatures, replays, wrong-key proofs; per-key audit log of every use and outcome; leaked-key revocation | Implemented |
| ISO 27001:2022 | A.5.17 authentication information | Peppered hashes, shown once, leak-detectable prefix, automatic revocation on public leak | Implemented |
| ISO 27001:2022 | A.8.5 secure authentication | Short-lived signed tokens bound to a hardware key | Implemented |
| ISO 27001:2022 | A.8.24 cryptography | Ed25519, ECDSA P-256, SHA-256, AES-256-GCM (SFrame) | Implemented |
| ISO 27001:2022 | A.8.15 logging | Auth failures counted by cause; key-use audit (key, tenant, operation, source, outcome, time), 400-day retention, tenant-readable | Implemented |
| OWASP API Top 10 2023 | API1 object-level authorisation | Tenant and room in the signed scope | Implemented |
| OWASP API Top 10 2023 | API2 broken authentication | Pinned algorithm, short life, possession proof | Implemented |
| OWASP API Top 10 2023 | API4 resource consumption | Proof size cap, bounded replay set, credit gate, per-key rate limits (per replica) | Implemented |
| OWASP API Top 10 2023 | API5 function-level authorisation | Key kinds; capability bitmask enforced at relay | Implemented |
| RFC 8725 | §3.1 algorithm verification, §3.11 explicit typing | `alg` pinned, `typ: MOQT` | Implemented |
| RFC 9449, RFC 7638 | Sender-constrained tokens, JWK thumbprint | `cnf.jkt` plus ES256 proof, one per session | Implemented |
| NIST SP 800-57 | Key management and rotation | Key sets with overlap, derived IDs; versioned pepper in Secret Manager with rehash on use | Implemented |
| GDPR Art. 25, 32 | Minimisation, security of processing | Token carries a username only; E2EE by default | Implemented |
| PCI DSS v4.0 | Cardholder data | Stripe Checkout takes cards; MOQOM never sees a card number (SAQ A) | By design |
