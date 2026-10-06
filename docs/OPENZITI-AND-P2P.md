# OpenZiti, and a point-to-point mode

Two questions: should MOQOM use [OpenZiti](https://github.com/openziti), and
should media be able to go directly between participants instead of through a
relay?

**Short answers.** OpenZiti is a strong fit for the *control and management
plane* and a poor fit for *media*. Point-to-point media is worth building for
small private rooms, as an opt-in mode that falls back to the relay, and it
should not use OpenZiti for its transport, because OpenZiti does not do direct
connections.

## What OpenZiti is

An Apache-2.0 zero-trust overlay network from NetFoundry. Every endpoint has a
cryptographic identity, every connection is authorised by policy, and services
are *dark*: they open no listening ports, so they cannot be scanned or attacked
from the internet. Components:

- **Controller** — identities, policies, sessions.
- **Edge routers** — a mesh that carries traffic between endpoints.
- **SDKs** (Go, C, Swift, Kotlin, Node, Python, C#) that embed the identity in
  the application, or **tunnelers** that intercept traffic for unmodified apps.

Security model, per [NetFoundry's connection-security
docs](https://netfoundry.io/docs/openziti/learn/core-concepts/security/connection-security/):
mTLS 1.2+ on control, router-to-router and SDK-to-router connections, and
application data end-to-end encrypted with libsodium between the two SDKs.

## Why not for media

Three properties of OpenZiti conflict with what the media path needs.

**1. Traffic always goes through routers.** OpenZiti has no direct
peer-to-peer connections. Its maintainers describe P2P as on the roadmap but not
implemented ([forum,
2026](https://openziti.discourse.group/t/plans-to-use-a-similiar-nat-traversal-functionality-like-tailscale/5529)).
So it cannot provide the point-to-point mode either.

**2. The underlay is TCP.** Router links and SDK-to-router connections are
TCP/mTLS in production. UDP options exist — *Transwarp* for long-haul router
links ([design doc](https://github.com/openziti/ziti/blob/main/doc/transwarp_b1/transwarp_b1.md)),
experimental DTLS — but the maintainers report they have not matched TCP's
performance ([forum](https://openziti.discourse.group/t/does-openziti-support-dtls/2189)).
Carrying QUIC media over a TCP underlay brings back head-of-line blocking:
one lost packet stalls every stream behind it, which is exactly what MoQ over
QUIC exists to avoid. It also puts a second congestion controller under QUIC's,
and the two fight.

**3. Circuits are one-to-one; MOQOM's value is fan-out.** An OpenZiti service
connection joins two identities. The relay pulls a track once and copies it to
6,000 viewers on four cores (see
[`BENCHMARKS-AND-SPECS.md`](BENCHMARKS-AND-SPECS.md)). Over OpenZiti, each
viewer would be its own circuit, and the copying would still have to happen in
a MoQ relay at one end — so OpenZiti would add a hop and a layer of encryption
while removing nothing.

The media path already has what OpenZiti would add: TLS 1.3 on every QUIC
connection, token and device-bound authorisation on every request, revocation
that closes live connections, and SFrame for payload encryption.

## Where OpenZiti fits well

These are the parts of MOQOM that are request/response or long-lived management
connections, where TCP is fine and "no open ports" is a real security gain.

| Use | Today | With OpenZiti |
|---|---|---|
| **Relay admin port** | Bearer token or mutual TLS, or probes only; an open listener must be loopback. Binds separately and defaults off | Dark service; only operator identities can reach `/metrics` and `/sessions` |
| **Admin gRPC API** used by `moqom-go`/`moqom-rust` | Public HTTPS endpoint; `moqom-control` can also serve it on an overlay service (`MOQOM_CONTROL_ZITI_*`) | Optionally dark: a customer backend embeds the OpenZiti Go SDK and holds an identity. Offer as an enterprise tier, not the default — it adds onboarding friction |
| **Relay ↔ control-plane link** (bans, directives) | Relay dials the control plane | Both sides dark; a stolen relay credential is useless without the enrolled identity |
| **BYOC relays** | Customer opens ports for the control link | The customer's relay dials *out* over OpenZiti to Elliott Wave LLC's control plane. No inbound firewall rules, and the enrolment identity doubles as the licence — revoke it and the relay stops getting directives and certificates |
| **Operator access** (SSH, Postgres, dashboards) | IAP / bastion | Dark services, per-person identities, policy-based |

Media stays on QUIC directly. Viewers never need an OpenZiti identity.

### Status of the management plane (dev, 6 October 2026)

Built and running in dev; nothing in prod. How it works and how to operate it are in
[deploy/README.md](../deploy/README.md#the-management-overlay).

- **Overlay.** A self-hosted OpenZiti 2.0.6 controller and one public edge router on a single
  `e2-small` in us-west1 (`modules/ziti`), with state on a persistent disk and the PKI and admin
  password in Secret Manager. Services, policies and standing identities come from
  `deploy/tools/ziti-provision.sh`, so no token or key reaches Terraform state.
- **Control plane.** Binds two dark services: `moqom-dev-control`, the same mutual-TLS gRPC as its
  internal port, and `moqom-dev-api`, where the dialler's overlay identity is the credential for
  BYOC relays. The TCP listener stays up for everything except RelayLink.
- **Relay ↔ control link.** Each relay VM gets its own identity at boot. The control plane checks
  a Google-signed instance token plus the fleet's relay credential, names the identity after the
  VM, and deletes it within one five-minute reconcile after the VM is gone. Both relays attach
  through their tunneler, and the control plane refuses RelayLink from anything not on the
  overlay (`relay_link_overlay_only`).
- **Relay admin port.** The full surface (`/metrics`, `/sessions`) binds on loopback and is
  reachable only through the overlay, by operator identities. The network port serves probes
  only.
- **BYOC relays.** `moqom-byoc create|list|revoke` over the dark API service, or the owner's own
  credential. Enrolments are in Postgres (conformance suite run against Cloud SQL). A BYOC relay
  is pinned to its tenant for sessions, snapshots and live-map data, and revoking it closes its
  stream at once on the replica holding it, and within 30 seconds on any other.
- **Operator access.** Per-person identities dial Postgres (IAM login through a Cloud SQL proxy on
  the overlay VM), each relay's admin port and sshd, and both control services.
- **Enterprise dark admin API.** `moqom-go` has `WithDialer`, with no new dependency. A backend
  that embeds the OpenZiti Go SDK dials `moqom-dev-api` with its own identity. This has been
  exercised against dev, but no customer is on it.

Verified end to end in dev:

- Relays attached over the overlay, with the overlay-only flag on.
- An operator read relay `/metrics` and ran a Postgres query.
- A BYOC enrolment was created, used by a test relay with no credential on disk, and revoked;
  its link dropped in the same second.
- `moqom-go` listed rooms over the dark API.

Known gaps are listed in the owner's `open.md`. In short: the controller is one VM in one zone,
so losing it stops new circuits, though the media path is unaffected. The control plane's
identity is an overlay admin. Identity certificates have no renewal yet. Desktop Edge's
per-relay hostname intercept hasn't been tried.

### Licence and dependency check

OpenZiti is Apache-2.0, compatible with both halves of MOQOM. It goes in the
private control plane and relay, so it adds nothing to the public SDKs. It is
self-hostable, so there is no dependency on NetFoundry's commercial service.

## Point-to-point mode

### What it buys, and what it costs

| | Relay (today) | Direct |
|---|---|---|
| Latency | Detour via the relay | Shortest path; largest gain when both are in the same city or LAN. Not yet measured |
| Who carries media | Relay | The two devices only |
| What the relay sees | Ciphertext if E2EE; plaintext payload otherwise | Nothing — not even sizes and timing |
| **Peers learn each other's IP address** | No | **Yes** |
| Server-side moderation and recording | Possible (non-E2EE rooms) | Impossible |
| Fan-out | Thousands | Each extra peer costs the publisher a full upload |
| Works behind every NAT | Yes | ~90% with hole-punching, so a relay fallback is required |
| Cost to Elliott Wave LLC | Relay egress | Signalling only |

The IP-address row decides where this is allowed. For a creator platform,
handing a viewer the creator's home IP is a stalking and doxxing risk, so
**direct mode must never be offered to an audience**. It is for participants who
already trust each other.

### Where it is allowed

- 1:1 calls and private rooms up to about four participants
- Backstage between cohosts
- Only in **E2EE rooms**, where server-side moderation and recording are already
  off, so direct mode takes nothing away that was not already gone
- Only when **every participant opts in**, with the IP exposure stated in the UI

Never for public broadcast, never for viewers, never when a watchdog (recorder,
safety detector) is required in the room.

### Design

1. **Join as usual through the relay.** Tokens, possession proofs, MLS keys
   and room state all work exactly as now, and media starts flowing through the
   relay immediately. Direct is an upgrade, never a precondition.
2. **Exchange candidates over the room.** Each peer publishes its candidate
   addresses on a private track only the other members can read, encrypted
   under the room's MLS key, so the relay carries them but cannot read them.
3. **Hole-punch with QUIC NAT traversal.** Simultaneous outbound connections
   from both sides, the technique standardised in the IETF QUIC NAT traversal
   work. [iroh](https://docs.iroh.computer/concepts/nat-traversal) runs it in
   production on a quinn fork and reports roughly 90% direct-connection success,
   falling back to its relays otherwise.
4. **Speak MOQT directly.** One peer accepts a MOQT session from the other; the
   accepting side runs the same subscribe/publish code the relay talks to, with
   no caching or fan-out. SFrame payloads are unchanged, so nothing about media
   encryption differs between paths.
5. **Verify peers like a relay would.** Each side checks the other's MOQOM
   token and possession proof before accepting. That reuses the relay's code,
   and it keeps the player locked to the network (see
   [`OPEN-CORE.md`](OPEN-CORE.md)): a direct session still requires
   MOQOM-issued credentials on both ends.
6. **Fall back without a gap.** Keep the relay subscription open until the
   direct path has delivered for a few seconds, and switch back on loss or a
   network change. Pausing it would save the egress, but the relay does not
   yet honour a paused subscription. QUIC multipath is the cleaner way to do
   this eventually.
7. **Meter it.** Direct sessions cost no egress but still consume a room,
   tokens and keys. They are billed per direct participant-minute, counted by
   the relay from the tickets it issues; see
   [Direct tickets and billing](#direct-tickets-and-billing).

### Implementation risks

- **Swift has no QUIC NAT traversal.** MoqomKit uses Network.framework, which
  does not expose hole-punching. The options are building it on
  Network.framework's UDP primitives, or a Rust core through FFI — and MoqomKit
  deliberately has no C ABI today. This is the biggest cost in the plan, and it
  should be prototyped before it is promised.
- **iroh is a fork of quinn, and the engine vendors quinn.** Adopting iroh means
  either two QUIC stacks or re-basing on iroh's fork. Use iroh to prototype and
  to measure, and decide after.
- **Mobile networks.** Carrier-grade NAT is where hole-punching fails most, so
  phone-to-phone on cellular will often stay on the relay. Measure before
  advertising.

### Status

| Piece | Rust (`moqom_client::direct`) | Swift (`MoqomKit/Direct`) |
|---|---|---|
| Eligibility rules and the IP-exposure acknowledgement | Built | Built, same rules and order |
| Sealed candidate offers | Built | Built; byte-identical, pinned by a shared test vector |
| Offer exchange on the participant's `.direct` track | Caller publishes on its stage slot | `DirectOffers` |
| Direct endpoint: pinned mutual TLS, hole-punching, MOQT both ways | Built, loopback-tested | `DirectEndpoint`, loopback-tested on macOS; builds for iOS. The dialler is a legacy `NWConnectionGroup`, because `NetworkConnection<QUIC>` cannot open streams from a fixed local port. No QUIC datagrams on direct paths |
| Peer verification | Through the `PeerVerifier` trait; the relay's `PeerCheck` implements it | `DirectPeerCheck`, a port of the relay's token and proof checks, with a required proof. A refused peer is closed with `0x03`; the dialler's empty unidirectional stream is the final admission |
| Merging relay and direct paths, with fallback | `DualSubscription` | `DualSubscription`, same rules |
| Pausing the relay copy while direct | Not built: its stall detection compares against relay objects | `FORWARD=0` in `REQUEST_UPDATE` once direct, `FORWARD=1` on fallback or after 500 ms of direct silence |
| Wired into `Room` | No | No |
| Direct tickets | Not enforced | `DirectTicketFeed` asks the relay; `DirectTicketGuard` presents them and holds the peer to its own |
| Metering | Relay session, plus direct seconds from tickets | Same; `DirectUsage` counts direct bytes for the app |

Offers travel on a track under the sender's own stage namespace, which the
existing scope rules already allow, and the relay cannot read them. The one
relay change is honouring draft-19 `FORWARD` (§10.2.17) on `REQUEST_UPDATE`,
so a subscriber can pause its copy without giving up the subscription. A
resumed copy starts at the next subgroup, so a fallback loses the rest of the
current group; `NEW_GROUP_REQUEST` could shorten that for tracks with dynamic
groups.

### Direct tickets and billing

**Pricing.** Each tenant gets 1,000 free direct participant-minutes per
calendar month (UTC), then pays $0.50 per 1,000, the same on Cloud and
self-hosted. Bytes that go device to device are never billed: only the devices
see them, and a count the client reports is one the tenant's app could change.
The standby relay session a direct participant keeps open is metered and billed
exactly as before (connected seconds, role, tier, and whatever it still
carries).

**What is counted.** Not the media, which the relay never sees, but tickets.
While a participant is on a direct path it holds a subscription on its relay to
its own slot's track `.direct/{peerUser}/{peerDevice}` (the stage slot
`[tenant, r, room, user, device]`, or the backstage one). The relay answers
`SUBSCRIBE_OK` and then sends one object per group, a fresh ticket every 60 s:

```json
{"version":1,"ticket":"<compact JWS>","expires":1791288000,"keys":[{"kty":"OKP","crv":"Ed25519","kid":"…","x":"…"}]}
```

The ticket is an Ed25519 compact JWS, header
`{"alg":"EdDSA","typ":"moqom-direct-ticket","kid"}`, claims `ten`, `room`,
`sub`, `dev`, `peer` (`user/device`), `iat`, `exp` (90 s after issue), with
unknown claims refused. It is bound to the holder's verified identity, the room
and the one peer. Seconds with at least one ticket subscription open are the
participant's **direct seconds** (`moqom_relay::usage`), reported as
`direct_seconds` on the session's usage record; see
[`USAGE-METERING.md`](USAGE-METERING.md#direct-seconds).

**Who can get one.** A verified participant in a room with some publish grant,
on its own slot, for a well-formed peer that is not itself. Viewers, view-only
grants, relay links (`rly`), unauthenticated sessions and other people's slots
get `UNAUTHORIZED`; a relay without ticket keys answers `NOT_SUPPORTED`. The
grants are rechecked on every renewal, and a change that makes the participant
ineligible ends the subscription with `PUBLISH_DONE` (`UNAUTHORIZED`), which
stops the metering with it.

**The keys.** `MOQOM_RELAY_TICKET_KEYS` names a JWK set: the first key with a
private half signs, and every public half is published in each ticket object,
so keys can be rotated by adding the new one, waiting a ticket lifetime, then
moving it first. It is a fleet key, separate from the token keys: a ticket has
its own `typ` and a token checker requires `MOQT`, so neither passes as the
other, and a relay holding ticket keys still cannot mint tokens. A peer learns
the ticket keys **from its own relay session**, never from the other peer, so a
peer cannot vouch for itself.

**Enforcement.** Each side presents its current ticket to the other over the
direct session, on the same slot-and-name track, re-presented every few
seconds so a late subscriber is not left waiting for a renewal. Each side
checks the other's with the same rules as `moqom_relay::ticket::verify`
(Swift: `DirectTicketCheck`) and keeps a deadline at the latest expiry plus
30 s of skew, or 10 s of grace before the first. A missing, malformed, forged,
expired or misbound ticket closes the direct session with `0x03` and the
reason, and `DualSubscription` falls back to the relay and sends `FORWARD=1`.
So an app that drops its relay ticket subscription to avoid the fee loses the
direct path within about two minutes.

**Limitation.** Enforcement lives in the SDKs. Two modified apps that both skip
the check, or a modified app talking to a modified peer, can keep a direct path
with no tickets and pay nothing for it. The relay cannot see a direct path, so
nothing server-side can close this; what the design guarantees is that one
honest participant is enough to make the pair pay. The Rust client does not
enforce tickets yet.

**Billing.** `RecordUsage` claims a record's direct seconds against the
tenant's month (`Store.ClaimDirectSeconds`, keyed by the ledger idempotency
key, so a re-sent record neither counts twice nor moves the allowance), and
charges `Pricing.DirectFee(before, seconds)`: the difference between the
cumulative fee past the allowance after and before the record, so per-record
rounding never drifts from the monthly total. The fee is added to the record's
ledger entry after the Agora cap, which does not apply to it. Defaults are
`DirectFreeMinutesPerMonth = 1000` and `DirectFeePer1000Minutes = 50¢`,
overridden by `MOQOM_BILLING_DIRECT_FREE_MINUTES` and
`MOQOM_BILLING_DIRECT_FEE_PER_1000_MIN`, logged at startup and served by
`/v1/rates` as `direct_free_minutes_per_month` and
`direct_fee_per_1000_minutes`. Postgres keeps the months in
`billing_direct_months` and the claims in `billing_direct_claims`.

**Deploy order.**

1. The control plane (billing): the migration, the field and the rates. A
   record without `direct_seconds` charges exactly as before, and an older
   handler would refuse the new field, so this goes first.
2. Relays, with `MOQOM_RELAY_TICKET_KEYS` from a secret shared by the fleet.
   A self-hosted operator's relays need one key set between them, and their
   participants can only take a direct path with peers on the same fleet.
3. SDKs that request tickets and enforce them. Until the SDK ships, nobody is
   on a direct path, so there is nothing to bill.

### Order of work

1. Measure: the same two-phone bench, relay versus direct, on one LAN and
   across a city, using an iroh prototype. If the gain is a few milliseconds,
   stop here.
2. Lock down the management plane with OpenZiti: the control plane's gRPC
   port (an overlay listener exists), then the relay ↔ control link, then BYOC
   enrolment. The relay admin port no longer has an open gap to close.
   *Done in dev; see [Status of the management plane](#status-of-the-management-plane-dev-6-october-2026).
   Prod is not started.*
3. Build direct mode for 1:1 E2EE calls only, behind a flag.
4. Widen to small private rooms once fallback is proven on cellular.
