# MOQOM

**Media Over [QUIC](https://www.rfc-editor.org/rfc/rfc9000) Orchestration Mesh**: a relay, a control plane, and client
SDKs for iOS, Android, web, and desktop.

Targets [`draft-ietf-moq-transport-19`](https://datatracker.ietf.org/doc/draft-ietf-moq-transport/).
The protocol implementation is our own rather than a fork, because owning the
wire format is what makes the congestion controller, the object-to-transport
mapping, and end-to-end encryption our decisions instead of someone else's.

This repository is the **platform**: the control plane, the gRPC contract every
other repository is written against, the deployment, and the plan. The media
path lives in [`moqom-engine`](https://github.com/moqom-cloud/moqom-engine), and
the SDKs each live in a repository of their own.

| Repository | License | What |
| --- | --- | --- |
| **`moqom`** (here) | Mixed | Control plane, `proto/`, Terraform, and [the plan](docs/PLAN.md) |
| [`moqom-engine`](https://github.com/moqom-cloud/moqom-engine) | Mixed | The media path: codec, transport, relay |
| [`moqom-swift`](https://github.com/moqom-cloud/moqom-swift) | Apache-2.0 | `MoqomKit`, the Apple-platform client SDK, and the `MoqomRoom` sample |
| [`moqom-go`](https://github.com/moqom-cloud/moqom-go) | Apache-2.0 | Server-side SDK for backends |
| [`moqom-rust`](https://github.com/moqom-cloud/moqom-rust) | Apache-2.0 | Server-side SDK for backends |
| `moqom-android` | Apache-2.0 | Android client SDK — scoped, work ongoing, release planned October 2026 |
| `moqom-desktop` | Apache-2.0 | Windows, macOS and Linux client SDK — scoped, work ongoing, release planned October 2026 |
| `moqom-obs` | GPL-2.0-or-later | The OBS output plugin — scoped, work ongoing, release planned October 2026 |

## Where it runs, and who we are talking to

MOQOM runs on Google Cloud today: relays on Premium Tier VMs behind UDP load
balancers, the control plane on GKE, Postgres on Cloud SQL, tenants isolated
per account. We are pursuing partnerships with **Cloudflare, Google Cloud, AWS**
and other CDNs and content providers, for edge capacity, negotiated delivery
cost and distribution.

We don't wait on them to ship. Nothing in the relay assumes one provider's
edge: the forwarding rule and DNS steering are the only pieces that move, so a
partner's network can be added beside GCP rather than in place of it, and
customers can self-host relays anywhere today. No partnership is signed yet;
this section changes when one is.

Most tooling here assumes those are **sibling checkouts** — cloned into the same
directory as this one, which is where they land if you clone them side by side.
Where that is required rather than convenient, the script says so and names the
environment variable that overrides it.

## Status

**Media moves.** `moqom-proto` covers the object model, every control message in
draft-19, and the whole data plane. `moqom-transport` accepts sessions over both
[QUIC](https://www.rfc-editor.org/rfc/rfc9000) and [WebTransport](https://datatracker.ietf.org/doc/draft-ietf-webtrans-http3/) on one UDP port, and `moqom-relay` speaks MOQT over them:
setup, namespace announcement and withdrawal, prefix watching, subscription, and
subgroup fan-out with alias rewriting. History is retained and fetchable, which
is what makes a reconnection survivable, and caching is a tee off the forward
path rather than a stage in it — so end-to-end encryption and retention stay
compatible. The relay can also be watched, through a separate admin port serving
Prometheus metrics and JSON session snapshots. What it does not yet do is
authorise anything, deduplicate upstream subscriptions, or schedule under
congestion; [`moqom-engine`](https://github.com/moqom-cloud/moqom-engine) has the
detail and the list of gaps.

**A room can be administered.** The Go control plane serves the admin API —
rooms and their policy, moderation, client token minting, and a per-room event
stream — over gRPC, with `moqom-go` and `moqom-rust` as the server-side SDKs a
customer's backend uses to reach it. Neither SDK speaks MOQT: administering a
room and carrying media are different jobs, and making every backend link a QUIC
stack to change somebody's grants would be a poor trade.

**The two halves now meet, and the seam is tested.** A kick is an enforced
decision rather than only a durable one:
`moqom-engine/moqom-relay/tests/interop_link.rs` starts this control plane's real
binary, attaches a real relay to its link, joins a real client over QUIC with a
token this server minted, and bans them through the admin API — and asserts the
open connection closes, not merely that the next token is refused. Unbanning and
closing a room travel the same way. Those tests are `#[ignore]`d because they
need a Go toolchain, and `tools/interop.sh` is what runs them, along with the
check that the two SDKs and the server agree about what a capability bit means —
and about what a device thumbprint is, which is a committed fixture rather than a
running process: the Swift client writes proofs and [RFC 7638](https://www.rfc-editor.org/rfc/rfc7638) vectors, the relay
verifies the proofs, and this server recomputes the thumbprints, so the three
cannot drift without one of them failing.

**What is still honest about the gap.** The store defaults to memory, so a
restart forgets every ban and the process says so in a warning when it starts. A
Postgres store is written and reachable with `MOQOM_CONTROL_STORE=postgres`,
against Cloud SQL as the runtime's own Google identity, so there is no password
to configure, store, or rotate — but it is the choice rather than the default,
and a default that loses the moderation log is the one that will be running
somewhere. Tokens are [ML-DSA-65](https://csrc.nist.gov/pubs/fips/204/final) over JSON rather than CAT/CWT's CBOR and COSE,
which is an encoding away from the target and no longer a binding away from it:
`cnf.jkt` is derived from a key the device sends at join, held in its Secure
Enclave, the control plane mints no token without one, and every relay makes every
session sign for it — so a stolen token works from nowhere. And
the relay registry is per-replica and in memory, which means a directive reaches
the relays one replica happens to know about — enough for a single control plane,
not for two.

**The client is further along than the server it will face.**
[`MoqomKit`](https://github.com/moqom-cloud/moqom-swift) covers the client logic
that does not touch hardware — rooms, roles and capabilities, the session state
machine, render-aware layer selection, fetch, reconnection — under Swift 6
strict concurrency. Room state travels on its own control track as a
sequence-numbered log with periodic snapshots, so a client returning from an
outage recovers grants, mutes and stage exactly rather than inferring them from
who happens to be publishing. It reports on itself through a single
`RoomStatistics` snapshot read at one instant, and it hands the media out: raw
frames and samples are tappable at four points, after decryption, so an
application can run its own detection or transcription instead of ours. The
capture and encode path has run end to end on two iPhones through a relay, but
the test suite doesn't cover it, because that needs a device.

[`docs/PLAN.md`](docs/PLAN.md) is the full design and build order — what is
built, what is next, and the reasoning behind the decisions that are hard to
reverse. It is checked against every repository on every push, so an item marked
done has files on disk to show for it and an item marked pending does not.
[`docs/PLAN-CHECK.md`](docs/PLAN-CHECK.md) explains how that works and what to
do when it fails.

## Design documents

| Document | What it covers |
| --- | --- |
| [`API-TOKEN-FLOW.md`](docs/API-TOKEN-FLOW.md) | API keys and join tokens: how they're issued, bound to a device and checked |
| [`E2EE-KEY-MANAGEMENT.md`](docs/E2EE-KEY-MANAGEMENT.md) | MLS-managed end-to-end encryption keys, and the room `e2ee` policy |
| [`OPENZITI-AND-P2P.md`](docs/OPENZITI-AND-P2P.md) | The OpenZiti management overlay (live in dev), BYOC relay enrolment, and direct peer-to-peer calls with their tickets and billing |
| [`USAGE-METERING.md`](docs/USAGE-METERING.md) | What relays meter, how records reach billing, and direct seconds |
| [`RECORDING.md`](docs/RECORDING.md) | Hosted recording |
| [`OPERATOR-CONSOLE.md`](docs/OPERATOR-CONSOLE.md) | The operator console at admin.moqom.cloud |
| [`OPEN-CORE.md`](docs/OPEN-CORE.md) | What is public, what stays private, and how the player is tied to the network |
| [`BENCHMARKS-AND-SPECS.md`](docs/BENCHMARKS-AND-SPECS.md) | Measured performance and specifications |
| [`compliance/`](docs/compliance/README.md) | The compliance programme: policies, GDPR, CCPA, SOC 2 and ISO 27001 mappings, incident response |
| [`deploy/README.md`](deploy/README.md) | Deploying: environments, secrets, the management overlay, viewer licensing |

## Layout

```
go/          Go: control plane and director (identity, tokens, rooms, policy)
proto/       The gRPC contract: admin API, and the internal relay link
schemas/     Track layouts on the MOQOM record format, and warehouse schemas
agents/      Reference AI agents
deploy/      Terraform
docs/        The plan, and the rules that keep it true
tools/       Repository-level checks, including the cross-language ones
```

The Rust/Go split is by role, not preference: Go owns request/response state
backed by Postgres, Rust owns everything on the media path. Nothing written in
Go sits between a publisher and a subscriber. That line is now a repository
boundary rather than a directory one.

## Why so many repositories

The default was the opposite — a protocol still moving through drafts wants the
codec, the relay, and the SDKs changing in one commit — and each split was
forced by something specific.

**Swift, by SwiftPM.** It resolves a git dependency by looking for
`Package.swift` at the repository *root* and cannot be pointed at a
subdirectory, so a package nested in a monorepo is not consumable by URL at all.
The `MoqomRoom` sample moved with it, because a sample that can only depend on a
released version stops being an integration test.

**Go, because an import path is a URL.** `go get` resolves a module by fetching
its path, so the Go SDK's path decides where it lives: it declares itself
`github.com/moqom-cloud/moqom-go` and has to be published from a repository of
that name. It is still developed against this one — the control plane's tests
drive it against a real server over bufconn, which is worth considerably more
than a mirror's CI — through `go.work` and a sibling checkout.

**Rust, for symmetry and to keep [QUIC](https://www.rfc-editor.org/rfc/rfc9000) out of a backend.** Cargo imposes no such
constraint, but the engine's workspace patches `crates.io` to a vendored quinn,
and a patch applies to a whole workspace. Being a separate workspace is what
keeps a QUIC stack out of the dependency graph of a backend that only wants to
kick somebody out of a room; being a separate repository makes that structural
rather than a rule someone has to remember.

**The engine, because the halves stopped needing each other daily.** The media
path shares no code with the control plane and no build with it — the split is
along the same line the architecture already drew. What crosses it is the gRPC
contract in `proto/` and one token fixture, and both are checked in CI.

**OBS, by the GPL.** A plugin links `libobs` and is a combined work, so its
source must be public and GPL-licensed — which this repository, private and half
proprietary, cannot be. Only Apache-2.0 code may cross that boundary; see
[LICENSING.md](LICENSING.md#the-gpl-boundary-plugins-for-hosts-we-do-not-own)
for what that costs. Treat the next host integration as a new repository by
default.

Effects are not in this category. `moqom-fx` is the runtime — frame contract,
shader interpreter, watchdog — and is client core, so it belongs with the
engine. An individual effect is WGSL and a parameter schema we execute, or a
third-party binary under contract, and is versioned by whoever ships it.

## Development

Requires Go 1.23+ and Docker. For anything cross-language, clone the siblings
first:

```sh
git clone https://github.com/moqom-cloud/moqom-engine
git clone https://github.com/moqom-cloud/moqom-go
git clone https://github.com/moqom-cloud/moqom-rust
```

The control plane, the SDK it is developed against, and the check that they
agree:

```sh
cd go && go test ./...          # includes the Go SDK, via go.work
./tools/interop.sh              # both SDKs against a real moqom-control
```

`tools/interop.sh` is the one that earns its keep, and it is deliberately still
here rather than in either SDK. A capability is a bit in the SDKs and an enum on
the wire, and a mapping table where two rows point at the same wire value would
pass every test in every one of those packages while handing a moderator a power
somebody meant to withhold. So it sends all twenty-one capabilities one at a
time through a real server and reads each one back. Neither SDK can run that
alone, and a check that belongs to nobody stops running — so it lives with the
contract it is checking.

Regenerating the gRPC stubs needs `protoc` and is not part of a normal build,
because a published SDK cannot demand it of the people who depend on it:

```sh
./tools/generate-proto.sh                                    # Go, into ../moqom-go
cd ../moqom-rust && MOQOM_REGENERATE_PROTO=1 cargo build     # Rust
```

## Cross-repository checks

Splitting the platform up moved most of the interesting failures to the seams,
so three checks here run against other repositories:

- **The plan** resolves its evidence against sibling checkouts, and CI clones
  them. Without that it would pass while judging almost nothing.
- **The token fixture** is minted by `go/internal/token` and verified by the
  relay. It is committed in the engine repository so that `cargo test` needs no
  Go toolchain; CI re-mints it here and fails if the bytes disagree.
- **Interop** builds the control plane and runs both SDKs against it.

Those jobs clone sibling repositories, which for private repositories needs a
token: `GITHUB_TOKEN` is scoped to the repository it was issued for and cannot
read another. They use an organisation secret named `MOQOM_REPO_TOKEN` — a
fine-grained PAT with **Contents: read** on `moqom-cloud`, set once at the
organisation level rather than per repository, since all four need the same
thing. Without it those jobs fail at checkout; the rest of CI is unaffected.

**What none of them catch: misreading the draft.** A rule misread the same way
in an encoder and its decoder is self-consistent, and every check passes. Only
differential testing against an independent implementation closes that gap.
`MoqomKit`'s codec was written separately from the engine's and its interop
suite runs a Swift publisher and subscriber through a real relay, which is the
first of it — but two implementations that agree is a weaker claim than two
written from the draft by different people. Interop against `moq-rs` and
`moxygen` remains a milestone rather than a nice-to-have.

## Licence

Split by role. Protocol, client core, SDKs, schemas, and samples are
Apache-2.0; the relay, control plane, recorder and `moqom-cc` are proprietary and
ship only as binaries. Self-hosting (BYOC) customers get them under a written
licence that permits self-hosting for their own applications and users and
prohibits offering MOQOM to third parties as a hosted service.

The split is by role rather than by repository, so both this repository and
`moqom-engine` contain each half. See [LICENSING.md](LICENSING.md) for the full
map and the reasoning.

## Contributing

Elliott Wave LLC, founded and solely owned by Elliott, owns all of MOQOM's
intellectual property — the open-source parts included. An open-source licence
lets you use the code; it does not transfer ownership. Contributions therefore
require signing the [Copyright Assignment Agreement](.github/CLA.md), which
assigns your contribution's copyright to Elliott Wave LLC and licenses it back
to you for any use. We have contacted legal counsel to review it.
