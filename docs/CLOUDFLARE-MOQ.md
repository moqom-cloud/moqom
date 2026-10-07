# Using Cloudflare's MoQ relays

MOQOM SDKs can publish and subscribe through Cloudflare's MoQ relay network as well as through
MOQOM relays. Cloudflare's relays are free during its beta and run in over 330 cities. They
implement an older draft of MoQ Transport and a subset of its messages.

MOQOM's own relays, and self-hosted ones, track the latest draft.

## Which draft goes where

| Relay | Drafts | ALPN over raw QUIC |
| --- | --- | --- |
| MOQOM Cloud and self-hosted relays | 22 (preferred) and 19 | `moqt-22`, `moqt-19` |
| Cloudflare production (`draft-16.cloudflare.mediaoverquic.com`) | 16 | `moqt-16` |
| Cloudflare production (`draft-14.cloudflare.mediaoverquic.com`) | 14, no authentication | `moq-00`; not supported by MOQOM |
| Cloudflare interop (`draft-18-interop.cloudflare.mediaoverquic.com`) | 18, testing only | `moqt-18`; to add when Cloudflare deploys it globally |

The ALPNs were checked with a QUIC handshake on 2026-10-06. Raw QUIC works, so no WebTransport
is needed for Cloudflare.

## Authentication

Cloudflare relays are provisioned per application, through the Cloudflare dashboard or API, and
each comes with publish and subscribe tokens. A client presents its token as the URL path. Over
raw QUIC, that's the draft-16 `PATH` setup parameter, set to `/<token>`. Cloudflare warns that
these tokens appear in its access logs, so issue short-lived tokens per client.

MOQOM doesn't hold or issue Cloudflare tokens. The customer's backend creates them with its own
Cloudflare account.

## What works on a Cloudflare relay

- Publishing and subscribing to tracks, by namespace (`PUBLISH_NAMESPACE`, `PUBLISH`,
  `SUBSCRIBE`, `SUBSCRIBE_NAMESPACE`).
- End-to-end encryption. SFrame encrypts payloads only, so Cloudflare relays ciphertext it
  can't read. Room keys still need a key service; see below.

## What doesn't

Cloudflare's relays don't implement these messages (see its
[feature matrix](https://developers.cloudflare.com/moq/feature-matrix/)), and MOQOM relays
provide the rest:

| Feature | Why it's unavailable |
| --- | --- |
| Catch-up and late join from cache | No FETCH |
| Pause, resume and priority changes on a subscription | No REQUEST_UPDATE |
| Graceful relay migration | No GOAWAY |
| MOQOM admission tickets, grants, kick and ban, spend limits | These are enforced by MOQOM relays |
| Usage metering, events and recording | These run on MOQOM relays |
| Relay-side layer and bitrate adaptation | MOQOM relay feature |

The SDKs fail fast with a descriptive error rather than send a message Cloudflare doesn't
support.

## Billing

Billing works the same way as for self-hosted relays, after a free trial:

| Period | Price |
| --- | --- |
| Usage reported before 1 November 2026 (UTC) | Free. Recorded in the ledger at $0, marked `trial`. |
| From November 2026 | The first 1,000 participant-minutes each calendar month are free. After that, the self-hosted fee of $0.05/GiB of egress. Same Agora-based cap. |

The tenant pays Cloudflare for Cloudflare's relays, and MOQOM pays nothing, so there's no cost or
markup. The allowance is separate from the 1,000 free direct-call minutes.

MOQOM can't see traffic on Cloudflare's relays, so the tenant reports it, as a self-hosted relay
does. The tenant's reporter posts to `POST /v1/usage` with a relay key, adding
`"hosting": "cloudflare"` to the body. The records are the same as a self-hosted relay's:
participant, role, tier, seconds, and egress bytes. The rules:

- MOQOM's own relays can't use the Cloudflare hosting value.
- A Cloudflare record can't report direct-call time, which only MOQOM relays issue.
- The trial is decided by when the report arrives, so October usage reported after
  1 November is charged.

Code: `go/internal/billing` (`HostingCloudflare`, `Pricing.CloudflareFreeUntil`,
`CloudflareFreeMinutesPerMonth`, `CloudflarePrice`). The Postgres tables are
`billing_cloudflare_months` and `billing_cloudflare_claims`.

Not built yet: SDK usage counters on the Cloudflare profile. The tenant's backend needs those
counters to fill the records. A relay key is a secret, so apps never report usage themselves.
