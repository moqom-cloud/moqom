# Usage metering in the relay

How `moqom-relay` turns sessions into the records `POST /v1/usage` charges, and what it does
when the endpoint misbehaves. The endpoint's contract is
`sites/moqom-dev/src/content/docs/reference/usage-api.md`; the handler is
`moqom/go/internal/billing/http.go` (`usage`, `reporter`) and `service.go` (`RecordUsage`). The
relay code is `moqom-engine/moqom-relay/src/usage.rs` (meters, records, queue) and
`src/usage/report.rs` (the reporter).

## Two modes

| | Hosted (MOQOM Cloud) | Self-hosted (BYOC) |
| --- | --- | --- |
| Selected by | `MOQOM_RELAY_USAGE_URL` set, no key file | `MOQOM_RELAY_USAGE_KEY_FILE` set |
| Credential | `MOQOM_CONTROL_CREDENTIAL` — must be a service account with `is_relay: true` and **no** `tenant` in `MOQOM_CONTROL_ACCOUNTS`; the handler refuses anything else with 401 | The `mqk_…` relay key in the file, reread per report |
| URL | Required. The billing listener: the join endpoint's port, or `MOQOM_BILLING_ADDR`. A bare origin gets `/v1/usage` appended | Defaults to `https://api.moqom.cloud/v1/usage` |
| `tenant` in records | Always, from the verified token. Sessions without a verified tenant are **not metered** — the handler would 400 the whole batch for one record without a tenant | Omitted. The key's tenant is charged; omitting it means a token from another issuer cannot make a record that 400s the batch |
| Sessions on a relay with no verification keys (`Permission::Unrestricted`) | Not metered (no tenant to charge) | Metered, as the key's tenant, participant `anonymous` |
| Sessions whose token did not verify (`Permission::Nothing`) | Not metered | Not metered |
| `admitted` | Not returned; never gates | Gates new sessions |

Neither variable set: `Usage::disabled()`, no meters, nothing on the media path.

Optional for both: `MOQOM_RELAY_USAGE_INTERVAL` (seconds, default 60), `MOQOM_RELAY_USAGE_QUEUE`
(records, default 100 000), `MOQOM_RELAY_USAGE_CA` (extra PEM roots). `MOQOM_RELAY_ID` (else
`HOSTNAME`, else `relay`) is the id prefix.

## The meter

One `Meter` per session, created in `Session::run` after enrolment from the verified permission,
dropped in `Session::serve` when the session ends (which cuts the final partial record). The
session holds it in a `OnceLock`; a `Route` carries a clone so fan-out counts egress without a
lookup.

- **Seconds** — monotonic clock. Interim cuts floor the cumulative elapsed time and report the
  delta from what was already reported; the final cut rounds. A session is never reported for
  more seconds than it lasted, and intervals sum to its length.
- **Egress** — every chunk a subscriber's writer completes, beside `metrics.egress`, plus fetch
  answers. The subgroup header the relay writes per stream is not counted (nor is it in
  `metrics.egress`).
- **Ingress** — every subgroup chunk read from the session, beside `metrics.ingress`. A subgroup
  nobody subscribes to and nothing caches is not read, so not counted.
- **Room, participant** — `scope.room`, `"{user}/{device}"`.

### Role

Sampled every 5 s and at each subscription change; an interval is billed at the **highest** role
seen in it (host > audience > broadcast_audience), so a mid-interval promotion is a host interval.

| Grant | Role |
| --- | --- |
| `owner`, or publish includes audio, video or screen and not backstage | `host` |
| Anything else with a room | `audience` (interactive viewer) |
| No room (a view-only grant) | `broadcast_audience` |
| Unrestricted (self-hosted relay with no keys) | `host` if it published anything this interval, else `audience` |

The token has no broadcast flag, so a room viewer in a broadcast room is billed `audience`.
Getting `broadcast_audience` for those needs the control plane to say so in the token.

The recorder joins as a subscribe-only room viewer and is billed exactly like one; nothing
exempts it.

### Tier

Peak over the interval of the **sum of pixels of every video track the session is subscribed to
at once**, sampled at every subscribe/unsubscribe and every 5 s:

| Pixels | Tier |
| --- | --- |
| 0 (audio or data only) | `audio` |
| ≤ 1280×720 | `hd` |
| ≤ 1920×1080 | `full_hd` |
| ≤ 2560×1440 | `2k` |
| above | `2k_plus` |

A track's size comes from, in order:

1. The publisher's `.catalog` entry for it, when it gives both `width` and `height` (`kind`
   `video` or `screen`; any other kind is 0).
2. The rung in its name — `video/1080p` is 1080 lines at 16:9. Applies to names `media::classify`
   calls video or screen.
3. Otherwise a video or screen track counts as HD: the cheapest video tier, so an unsized track
   never overcharges.

Catalogs are registered per namespace by the session that publishes them and forgotten when that
session ends. Subscribing is the measure, not delivery: a subscription the relay is shedding
still counts, because the participant asked for it.

### Direct seconds

Time in the interval during which the session held **at least one direct-ticket subscription**
(`.direct/{peer}` on its own slot; see
[`OPENZITI-AND-P2P.md`](OPENZITI-AND-P2P.md#direct-tickets-and-billing)). Two peers at once
count once: the price is per participant-minute, not per path. Cut like `seconds` — interim cuts
floor the cumulative total, the final cut rounds — so it never exceeds `seconds` for the same
record. Reported as `direct_seconds`, omitted when zero, so a record from a session that never
went direct is byte-identical to before. A record with only direct seconds is still sent.

Only sessions that can hold a ticket subscription accrue any: verified room participants with a
publish grant. Viewers, view-only grants, relay links and unauthenticated sessions are refused
the subscription, so they never do.

The control plane charges direct seconds separately from the session record: the first 1,000
minutes per tenant per UTC month are free, then $0.50 per 1,000 (`DirectFreeMinutesPerMonth`,
`DirectFeePer1000Minutes`, env `MOQOM_BILLING_DIRECT_FREE_MINUTES`,
`MOQOM_BILLING_DIRECT_FEE_PER_1000_MIN`). The claim against the month is keyed by the record's
ledger idempotency key, so a resent record changes neither the fee nor the allowance, and the
fee is added after the Agora cap, which does not apply to it. The ledger entry's detail carries
`direct_seconds` and `direct_fee`.

## Records and ids

```text
{relay_id}:{interval_start_rfc3339}:{user}/{device}#{boot_ms}.{session_number}
```

`interval_start` is the wall clock rounded down to the interval, so every relay's intervals align.
`boot_ms` and the session number make the id unique across a relay restart and across two
sessions of one participant in one interval (a reconnect), which would otherwise collide and be
charged once. `anonymous` stands in for an unauthenticated participant. An id over 200 characters
becomes `h:` + SHA-256 hex of it. Ids are deterministic per relay process: the same cut produces
the same id, and retries resend it verbatim.

**`MOQOM_RELAY_ID` must be unique per account.** Two relays sharing one would still not collide
(different boot times), but their records would be indistinguishable in the ledger.

One record per participant-session per interval; an interval with zero seconds and zero bytes is
not sent. `seconds` is clamped to 86 400.

## Reporter

Two tasks. The **cutter** wakes every 5 s (or at the boundary) to sample, and at each boundary
cuts every meter into the queue. The **sender** drains the queue. Neither is on the media path;
the media path only does relaxed atomic adds and, on subscription changes, a short mutex.

- **Queue** — bounded (`MOQOM_RELAY_USAGE_QUEUE`). Full: drop the oldest, count
  `dropped_total{reason="overflow"}`, warn once per episode. At 60 s intervals and 1 000
  sessions, 100 000 records is about 100 minutes of outage.
- **Batches** — at most 5 000 records and 1 000 000 serialised bytes (the handler's limit is
  1 MiB). A single record too large to send alone is dropped as rejected.
- **Network error, timeout, 429, other 5xx** — the same batch, same ids, after a jittered
  exponential wait: 1 s doubling to 60 s, drawn from the upper half. Never given up.
- **400 / 413 / 422** — the handler refuses a whole batch for one bad record, so the batch is
  halved and each half resent until the bad records are alone; those are dropped and logged
  (`dropped_total{reason="rejected"}`). A refused batch charges nothing, so halves cannot
  double-charge.
- **401 / 403 / 404, or an unreadable/empty key** — logged at error on every attempt, batch kept,
  retried at the same backoff. A rotated key file or fixed config resumes without loss.
- **Shutdown** — after the listeners drain, every open meter is cut as final, and the sender gets
  10 s. What remains is counted as `dropped_total{reason="shutdown"}` and logged at error. There
  is no on-disk spool: a crash loses up to one interval plus whatever is queued.

### Admission (self-hosted)

`admitted` from each answer sets `Usage::admitting`. `Session::admissible` — the existing
admission path, run once at SETUP and not on renewal — refuses with
`"this account is not admitting new sessions"` (policy refusal `billing`) while it is false.
Sessions already open are untouched.

Because admission is only learned by reporting, a self-hosted relay sends an empty batch at every
boundary when it has nothing else to send. Without it a relay that was told no would have no
sessions, so no records, so no way to hear yes.

**Fail open.** Until the first answer, and through any outage, the last answer stands — which at
startup is yes. The control plane already refuses to mint tokens for a tenant that may not admit,
and refusing everyone whenever billing is unreachable would make billing an outage.

## Metrics

On the admin `/metrics`, after the relay's own (only when metrics are on):

| Metric | |
| --- | --- |
| `moqom_relay_usage_records_queued_total` | Records cut |
| `moqom_relay_usage_records_sent_total` | Accepted by the endpoint |
| `moqom_relay_usage_records_duplicate_total` | Accepted as already charged |
| `moqom_relay_usage_records_unbilled_total` | Accepted, not charged |
| `moqom_relay_usage_records_dropped_total{reason}` | `overflow`, `rejected`, `shutdown` |
| `moqom_relay_usage_records_waiting` | Queued or in flight |
| `moqom_relay_usage_meters` | Sessions being metered |
| `moqom_relay_usage_reports_total{outcome}` | `ok`, `rejected`, `unauthorized`, `failed` |
| `moqom_relay_usage_report_seconds` | Histogram of report latency |
| `moqom_relay_usage_last_success_timestamp_seconds` | Unix time of the last accepted report |
| `moqom_relay_usage_admitting` | 1 while admitting |

Alert on `time() - last_success_timestamp_seconds > 600`, any `unauthorized`, and any increase in
`dropped_total`.

## Deploying to hosted relays

1. Make sure the account whose credential relays present (`MOQOM_CONTROL_CREDENTIAL`, from the
   relay's credential secret) has `"is_relay": true` and no `tenant` in `MOQOM_CONTROL_ACCOUNTS`.
2. Add `MOQOM_RELAY_USAGE_URL` to the relay module's `container_env` — the public
   `https://api.moqom.cloud/v1/usage`, or the billing listener's internal address (add
   `MOQOM_RELAY_USAGE_CA` if that is privately certified).
3. Optionally set `MOQOM_RELAY_ID`; with `--network host` the default is the instance name, which
   is unique and stable.
4. The systemd stop timeout must allow the drain plus up to 10 s for the last report.
5. For tiers: deploy relays before the control plane that mints `rly`, and set
   `MOQOM_RELAY_LINK_URL` on each edge (beside `MOQOM_RELAY_UPSTREAM`) to the control plane's
   `/relay/link`, so the edge mints its own link credentials (see Relay-to-relay links). An edge
   still configured with a static `MOQOM_RELAY_UPSTREAM_TOKEN` viewer token (which needs
   `MOQOM_RELAY_UPSTREAM_KEY` beside it) is metered as audience at its parent.

## Relay-to-relay links

A child relay reads from its parent over an ordinary session, and its viewers are metered at
the child. So the parent must not meter the link, and a person must not be able to pass as a
link to watch unmetered.

- **The grant** is `moqt.rly: true` in the token, a signed claim verified like every other grant.
  `Usage::begin` skips a scope with `relay` set, in both modes.
- **Who can get one**: only `POST /relay/link` on the control plane's join listener mints it
  (`internal/server/relaylinktoken.go`). That route accepts only a service account with
  `is_relay: true` (backend keys, owner accounts and anonymous callers get 401), and a
  tenant-scoped relay account only for its own tenant. The body is `{tenant, room, key|device}`
  and the answer is the join response's shape. `sub` is `moqom-relay`, reserved on the backend
  mint path. `ScopeFrom`, which the admin API and `/join` mint through, never sets it.
- **Read-only**: the relay refuses (`Unsupported`) a token with `rly` alongside any publish
  bit, capability, `own` or `bs`. So a link can't be an unmetered host, even if a minter bug
  set both.
- **Fixed for the session**: `Scope::same_seat` includes `relay`, so a renewal can't turn a
  metered session into a link or back.
- **Forged or unsigned** link tokens fail verification and the session is refused, so it is
  neither served nor unmetered.
- **One room per link.** The minter requires `room` and refuses a room that doesn't exist (404)
  or has ended (410). The relay refuses an `rly` token with no room, and on a link it refuses
  `PUBLISH_NAMESPACE` and any read outside that room, backstage included, with `UNAUTHORIZED`.
- **Bound to a key.** The minter requires `key` (a public JWK) and binds the token to it with
  `cnf.jkt`. The edge generates a P-256 key at startup, held only in memory, and on every link
  presents an ES256 `MOQT-POP` proof with the token, made the same way as a client's
  (`moqom-client`'s device key). It binds `ath` (the token's hash), `rel` (the SHA-256 of the
  parent's leaf certificate, which the edge reads off its own TLS connection) and a `jti` that
  the parent accepts only once. A parent with `MOQOM_RELAY_POSSESSION=required` refuses a link
  with no proof, a reused proof, or a proof made for another relay's certificate, and counts each
  refusal in `moqom_relay_tokens_refused_total{cause}`.
- **Short-lived and renewed.** A link token lasts 5 minutes. The parent ends the session at
  `exp` plus 30 s of clock skew. The edge (`moqom-relay/src/edge.rs`) mints the replacement once
  a third of the lifetime is left, with ±10% jitter, retrying on failure with exponential backoff
  (a random wait between half and all of a delay that starts at 1 s and doubles to at most 60 s). It sends the new token and a fresh proof as a
  `REQUEST_UPDATE` on the room's namespace watch, and keeps serving on the current token until
  it lapses. A renewal must name the same scope and the same principal.
- **Revocation.** The edge treats 401, 403, 404 or 410 from the minter as revoked. A room that
  is refused when the link first opens gets no link, and its viewers are refused, for 30 s; then
  the next request tries again. When renewal is refused, the link ends with its current
  credential, at most 5 minutes and 30 seconds later. Nothing pushes a revocation to the parent
  sooner.
- **Per room, on demand.** With `MOQOM_RELAY_LINK_URL` set, the edge opens a link the first time
  a viewer asks for a room it doesn't publish, holds the subscription for up to 5 s while the
  link comes up, and closes the link after it has carried nothing for a minute.

Rollout order: relays first. The relay parses `moqt` strictly, so a relay older than this
refuses link tokens, which fails closed. The Go verifier is strict too, and it ships in the same
binary as the minter.

## Known gaps

- **Hosted tiers aren't deployed.** The edge's link minting is built and tested against a relay
  parent, but no Terraform environment sets `MOQOM_RELAY_LINK_URL` yet.
- **Links serve subscriptions only.** A link forwards `SUBSCRIBE`. It does not forward
  `FETCH`, `TRACK_STATUS` or `SUBSCRIBE_TRACKS`, and a backstage (`bs`) namespace can't be read
  over a link.
- **Single-link mode is metered as audience.** Without `MOQOM_RELAY_LINK_URL`, `upstream::attach`
  presents `MOQOM_RELAY_UPSTREAM_TOKEN` and proves it with the device key in
  `MOQOM_RELAY_UPSTREAM_KEY`, against the certificate the parent presented. There is no
  unproven mode: every relay requires a proof, the relay refuses to start with a token and no
  key, and the control plane mints no token without a device thumbprint. The token is an
  ordinary viewer token, so the parent meters the link as audience.
- **`relay_link_overlay_only` isn't enforced on `/relay/link`** yet, only on the gRPC RelayLink.
- **Links appear on the parent's roster** as a participant named `moqom-relay`.
- **`broadcast_audience`** is only reached by view-only grants with no room; see Role.
- **Sampling** — roles and pixels are sampled at subscription changes and every 5 s, so a change
  of grants that lasts under 5 s with no subscription change can be missed.
- **No spool** — a crash or a shutdown past the deadline loses what is queued.
- **Ingress of unread subgroups** is not counted (it is not charged under either mode today).
- **Direct seconds trust the SDKs to enforce tickets.** Two modified apps can keep a direct path
  without tickets and accrue none; see the limitation in `OPENZITI-AND-P2P.md`.
- **The usage breakdown does not split out direct minutes.** The fee is in the ledger entry and
  its detail, but `/v1/usage` summaries don't show direct minutes or the remaining allowance.
