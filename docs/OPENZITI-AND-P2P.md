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
Carrying [QUIC](https://www.rfc-editor.org/rfc/rfc9000) media over a TCP underlay brings back head-of-line blocking:
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

The media path already has what OpenZiti would add: [TLS 1.3](https://www.rfc-editor.org/rfc/rfc8446) on every QUIC
connection, token and device-bound authorisation on every request, revocation
that closes live connections, and [SFrame](https://www.rfc-editor.org/rfc/rfc9605) for payload encryption.

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

Media stays on [QUIC](https://www.rfc-editor.org/rfc/rfc9000) directly. Viewers never need an OpenZiti identity.

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
- Only when **every participant opts in**, with the IP exposure stated in the UI.
  MoqomKit opts in by default (`DirectPolicy.automatic`); `.relayOnly` opts out
- Only where the room's policy allows: `RoomPolicy.direct_paths` (allowed unless
  `DIRECT_PATHS_RELAY_ONLY`) and `max_direct_participants` (2 to 4, default 4).
  Every token states it (`MintClientTokenResponse.direct`, `direct` from
  `/join`), the SDK skips the opt-in where it is not allowed, and the relay
  issues no direct tickets in a relay-only room (`RoomSnapshot.direct`)

Never for public broadcast, never for viewers, never when a watchdog (recorder,
safety detector) is required in the room.

### Design

1. **Join as usual through the relay.** Tokens, possession proofs, [MLS](https://www.rfc-editor.org/rfc/rfc9420) keys
   and room state all work exactly as now, and media starts flowing through the
   relay immediately. Direct is an upgrade, never a precondition.
2. **Exchange candidates over the room.** Each peer publishes its candidate
   addresses on a private track only the other members can read, encrypted
   under the room's MLS key, so the relay carries them but cannot read them.
   The candidates are the device's own addresses plus its public one. The
   device learns the public address by asking a [STUN](https://www.rfc-editor.org/rfc/rfc8489) server from the socket
   the direct path will use. Every relay answers STUN on UDP 3478, and the
   room's direct grant names one (`DirectGrant.stun`, set from
   `MOQOM_CONTROL_STUN`). Both SDKs ask that server by default, and an app can
   name its own instead. The relay answers only well-formed Binding requests,
   rate-limits each source, and replies from the address it was asked on.
3. **Hole-punch with [QUIC](https://www.rfc-editor.org/rfc/rfc9000) NAT traversal.** Simultaneous outbound connections
   from both sides, the technique standardised in the IETF QUIC NAT traversal
   work. [iroh](https://docs.iroh.computer/concepts/nat-traversal) runs it in
   production on a quinn fork and reports roughly 90% direct-connection success,
   falling back to its relays otherwise.
4. **Speak MOQT directly.** One peer accepts a MOQT session from the other; the
   accepting side runs the same subscribe/publish code the relay talks to, with
   no caching or fan-out. [SFrame](https://www.rfc-editor.org/rfc/rfc9605) payloads are unchanged, so nothing about media
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

- **Network.framework hole-punching is burst-only.** MoqomKit's direct path is
  built on Network.framework: a UDP probe flow on the [QUIC](https://www.rfc-editor.org/rfc/rfc9000) port steals the
  listener's packets, so Swift sends three probes and stops at the first
  inbound packet, and has no QUIC datagrams on direct paths. To cover this,
  MoqomKit can now run the direct path on the engine's Rust socket layer
  through a narrow C ABI (`moqom-direct-ffi`), chosen at init with
  `directBackend: .rust`. The Rust backend probes continuously from the same
  socket quinn uses, and carries datagrams. Only the socket and QUIC sit
  behind the ABI; MOQT, offers, peer checks and tickets stay in Swift. Neither
  backend has been measured across real NATs yet; see
  [Open work: the Rust direct backend](#open-work-the-rust-direct-backend).
- **iroh is a fork of quinn, and the engine vendors quinn.** Adopting iroh means
  either two QUIC stacks or re-basing on iroh's fork. Use iroh to prototype and
  to measure, and decide after.
- **Mobile networks.** Carrier-grade NAT is where hole-punching fails most, so
  phone-to-phone on cellular will often stay on the relay. Measure before
  advertising.
- **Symmetric NATs, and no TURN.** [STUN](https://www.rfc-editor.org/rfc/rfc8489) tells a device the public port its
  NAT used toward the relay. A NAT that picks a new port for each destination
  (address-dependent mapping) gives the peer a different one, so the punch
  misses. MOQOM runs no TURN server, so those pairs fall back to the relay.
  That fallback already carries the media, so the call keeps working, but it
  is not point to point. The relays' load balancer is IPv4-only, so the relay
  reports no IPv6 public address. IPv6 host candidates still work where
  neither side filters inbound traffic.

### Status

| Piece | Rust (`moqom_client::direct`) | Swift (`MoqomKit/Direct`) |
|---|---|---|
| Eligibility rules and the IP-exposure acknowledgement | Built, with the room's limit and `DirectPolicy` | Built, same rules and order; `DirectPolicy` on `MoqomSession`, automatic by default |
| Sealed candidate offers | Built | Built; byte-identical, pinned by a shared test vector |
| Offer exchange on the participant's `.direct` track | `DirectRoom`, on the stage slot | `DirectOffers` |
| Direct endpoint: pinned mutual TLS, hole-punching, MOQT both ways | Built, loopback-tested | `DirectEndpoint`, loopback-tested on macOS; builds for iOS. The dialler is a legacy `NWConnectionGroup`, because `NetworkConnection<QUIC>` cannot open streams from a fixed local port. No [QUIC](https://www.rfc-editor.org/rfc/rfc9000) datagrams on direct paths |
| [STUN](https://www.rfc-editor.org/rfc/rfc8489) server | `DirectPolicy::AUTOMATIC` asks the grant's (`RoomDirectPolicy::stun`), an explicit one wins | `.automatic` asks the grant's (`DirectRoomPolicy.stun`), `.automatic(stun:)` wins, `.relayOnly` asks none |
| Direct socket backend | quinn on one UDP socket, STUN and continuous probing (`direct::socket`) | `DirectBackend`: `.networkFramework` (default) or `.rust`, which runs the Rust socket layer through `moqom-direct-ffi` (opt-in `MoqomDirectRust` module, iOS and macOS). All pairings interoperate on macOS |
| Peer verification | Through the `PeerVerifier` trait; the relay's `PeerCheck` implements it | `DirectPeerCheck`, a port of the relay's token and proof checks, with a required proof. A refused peer is closed with `0x03`; the dialler's empty unidirectional stream is the final admission |
| Merging relay and direct paths, with fallback | `DualSubscription` | `DualSubscription`, same rules |
| Pausing the relay copy while direct | Same rules in `DualSubscription` (`PathSettings::quiet`, 500 ms); `pauses_relay` turns it off | `FORWARD=0` in `REQUEST_UPDATE` once direct, `FORWARD=1` on fallback or after 500 ms of direct silence |
| Wired into `Room` | `DirectRoom` beside a `Session`: on by default under `DirectPolicy::AUTOMATIC` with the token's `RoomDirectPolicy`, per-peer `PathState`, relay fallback with backoff, tickets per path, a renewed grant applied mid-room, backstage never direct. No catalog or [MLS](https://www.rfc-editor.org/rfc/rfc9420) in the Rust client, so the caller states the roster, opt-ins and offer key (`RoomView`). Loopback-tested against an authenticating, ticketing relay | On by default under `DirectPolicy.automatic` (peers verified against `authorityKeys`), or `.custom(DirectRoomOptions)`; per-participant path state (`directPathChanged`), relay fallback with backoff, tickets per path. Backstage never goes direct. Offers sealed under `MLS-Exporter("MOQOM direct offer")`. Loopback-tested on both backends |
| Direct tickets | `DirectRoom` asks the relay, presents them and holds the peer to its own (`verify_ticket`, on `moqom-token`'s keys) | `DirectTicketFeed` asks the relay; `DirectTicketGuard` presents them and holds the peer to its own |
| Metering | Relay session, plus direct seconds from tickets | Same; `DirectUsage` counts direct bytes for the app |

Offers travel on a track under the sender's own stage namespace, which the
existing scope rules already allow, and the relay cannot read them. The one
relay change is honouring draft-19 `FORWARD` (§10.2.17) on `REQUEST_UPDATE`,
so a subscriber can pause its copy without giving up the subscription. A
resumed copy starts at the next subgroup, so a fallback loses the rest of the
current group; `NEW_GROUP_REQUEST` could shorten that for tracks with dynamic
groups.

### Open work: the Rust direct backend

Built on the `oct-2026-direct-ffi` branches of moqom-engine and moqom-swift,
not pushed. The C header is `moqom-engine/moqom-direct-ffi/include/moqom_direct.h`
(ABI version 1); `MoqomDirectRust` checks the version before use.

Verified: 19 FFI tests and the Rust client's direct tests; in Swift, Rust ↔ Rust,
Network.framework dialling Rust, and Rust dialling Network.framework all set
up, refuse a viewer, an impostor and the wrong certificate, and carry media on
macOS.

Not done, or not known yet:

- **Real networks.** Nothing has run across real NATs. The field test should
  compare Swift on Network.framework, Swift on Rust, and the Rust client, on a
  LAN, across a city and on cellular.
- **visionOS and tvOS.** No slices: their Rust targets are tier 3. Those
  platforms keep the Network.framework backend.
- **IPv4 only.** The Rust socket binds IPv4 and skips IPv6 candidates, so an
  IPv6-only network gets no direct path on that backend.
- **A lost stream reset.** When a Network.framework side that accepted the
  connection resets a stream, a Rust reader doesn't see the reset while the
  stream is open. Logged as a known issue in the Swift tests.
- **Datagrams need both sides.** quinn won't send datagrams from a side that
  didn't offer them, so a Rust ↔ Network.framework path has none.
- **A dial can succeed and then close.** In [TLS 1.3](https://www.rfc-editor.org/rfc/rfc8446) the client finishes before
  the server checks its certificate, so a dialler whose certificate the peer
  rejects sees a connection that closes at once; SETUP fails on it.
- **Distribution.** Locally the package finds the framework through
  `Artifacts/` (`Scripts/direct-ffi.sh`) or `MOQOM_DIRECT_RUST=1`. A release
  needs a remote `binaryTarget` with a URL and checksum, and a decision on where
  it is hosted.
- **Size.** About 4.4 MB stripped on iOS. `ring` is still linked alongside
  aws-lc through rcgen's default features; removing it means changing the
  workspace's rcgen features, which the relay and recorder share.
- **`Room` gaps.** Backstage publishing counts as audience; nothing watches for
  network-interface changes; a viewer holding the room's [MLS](https://www.rfc-editor.org/rfc/rfc9420) keys keeps the room on
  the relay; the epoch-change re-offer has no dedicated test.
- **iOS Simulator crash in the Network.framework carrier.** Running the direct
  suite on the iOS 26.2 Simulator, the process traps inside Network.framework
  when a `MoqtSession` left from an earlier Network.framework test is freed and
  releases its `NetworkQUICCarrier` stream after the connection has failed. It
  doesn't happen on macOS, and the Rust-only tests pass on the simulator. The
  frames are unsymbolicated and the framework logs no message, so the cause
  isn't known. It could affect any iOS app that tears down a direct or relay
  session after a failure, so it needs a real iPhone run and, if it
  reproduces, a minimal case for Apple. Nothing has run on a real iPhone.

Questions for the owner:

1. Should `.rust` become the default on iOS and macOS once the field test is in,
   or stay opt-in?
2. Where should the release xcframework be hosted (GitHub release on the SDK
   repo, or GCS)?

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
{"version":1,"ticket":"<compact JWS>","expires":1791288000,"keys":[{"kty":"AKP","alg":"ML-DSA-87","kid":"…","pub":"…"}]}
```

The ticket is an ML-DSA-87 compact [JWS](https://www.rfc-editor.org/rfc/rfc7515), header
`{"alg":"ML-DSA-87","typ":"moqom-direct-ticket","kid"}`, claims `ten`, `room`,
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

**The keys.** `MOQOM_RELAY_TICKET_KEYS` names a [JWK](https://www.rfc-editor.org/rfc/rfc7517) set: the first key with a
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
honest participant is enough to make the pair pay. The Rust client enforces
them in `DirectRoom`.

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
