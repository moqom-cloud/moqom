# MOQOM

**Media Over QUIC Orchestration Mesh**: relay, control plane, and client SDKs for
iOS, Android, web, and desktop.

Targets [`draft-ietf-moq-transport-19`](https://datatracker.ietf.org/doc/draft-ietf-moq-transport/).
The protocol implementation is our own rather than a fork, because owning the
wire format is what makes the congestion controller, the object-to-transport
mapping, and end-to-end encryption our decisions instead of someone else's.

## Status

Media moves. `moqom-proto` covers the object model, every control message in
draft-19, and the whole data plane: object datagrams, subgroup streams, and fetch
streams with the gap markers a partial cache needs. `moqom-transport` accepts
sessions over both QUIC and WebTransport on one UDP port, and `moqom-relay` now
speaks MOQT over them: setup, namespace announcement and withdrawal, prefix
watching, subscription, and subgroup fan-out with alias rewriting.

History is retained and fetchable, which is what makes a reconnection survivable.
A client that dropped its connection asks for the range it missed; a client
joining a live track asks for the group already in progress rather than waiting
out a keyframe interval of black. Retention is bounded by group count and by byte
budget, evicting whole groups because half a group is undecodable, and can be
turned off entirely for operators who must not store media. Caching is a tee off
the forward path rather than a stage in it — forwarding still never parses an
object, so end-to-end encryption and retention stay compatible.

The relay can be watched. An admin surface on its own port — off unless
`MOQOM_RELAY_ADMIN_ADDR` says otherwise — exposes Prometheus metrics for
sessions, subscriptions, fetches, fan-out and bytes, plus JSON snapshots of live
sessions and occupied rooms, and separate liveness and readiness so draining
stops new connections without killing the ones in flight. Round-trip time,
congestion window and loss are sampled from the QUIC connection itself rather
than estimated a second time. Metric labels are fixed and low-cardinality;
anything per-room or per-participant lives in the JSON, because a label whose
values grow with traffic is how a metrics backend becomes an outage.

What the relay is not yet: authorising anything (tokens are parsed and ignored),
deduplicating upstream subscriptions, or scheduling under congestion. The admin
surface is also unauthenticated, which is why it binds separately and defaults
off. `rs/moqom-relay/src/lib.rs` lists the gaps and says which matter; none of it
is ready to face the internet.

A room can now be administered. The Go control plane serves the admin API —
rooms and their policy, moderation, client token minting, and a per-room event
stream — over gRPC, with `sdk/go` and `sdk/rust` as the server-side SDKs a
customer's backend uses to reach it. Neither SDK speaks MOQT: administering a
room and carrying media are different jobs, and making every backend link a QUIC
stack to change somebody's grants would be a poor trade.

The gap between those two halves is the honest part. The control plane can tell a
relay to eject somebody, and no relay has ever listened — `RelayLink` is tested
against a fake one. Until that is wired up, a kick is a durable decision and not
yet an enforced one. State lives in memory, tokens are bearer rather than
device-bound, and `tools/interop.sh` is what proves the two SDKs and the server
agree about what a capability bit means.

[`MoqomKit`](https://github.com/moqom-cloud/moqom-swift) covers the client logic
that does not touch hardware — rooms, roles and capabilities, the session state
machine, render-aware layer selection, fetch, reconnection — under Swift 6 strict
concurrency. Room state travels on its own control track as a sequence-numbered
log with periodic snapshots, so a client returning from an outage recovers grants,
mutes and stage exactly rather than inferring them from who happens to be
publishing. Three of its tests publish and subscribe through `moqom-relay` over a
real socket, which is the first evidence that the two codecs read the draft the
same way. The capture and encode path is written and compiles but has never
executed, because that needs a device.

It also reports on itself. One `RoomStatistics` snapshot carries the link, the
uplink including frames the encoder dropped before anything reached the network,
per-participant receive and decode state, control-track health, and a five-bucket
quality level per participant and for the local device — all read at one instant,
because comparing an encoder's output rate against a link estimate is only
meaningful if both were read together. Its logger is injectable and silent by
default, and cannot write down a payload or a key: the log value type has no case
that holds bytes, so the leak is prevented by the compiler rather than by review.

And it hands the media out. Raw frames and samples are tappable at four points —
captured video and post-cancellation PCM on their way to the encoders, decoded
video and PCM per participant on the way back — so an application can run its own
detection, transcription or translation instead of ours. Three rules make that
safe to ship: a tap is a bounded buffer that drops rather than stalls, so a slow
consumer degrades itself and never the picture or the uplink; a cadence can
decline the decode rather than filter after it, which is why classifying keyframes
only is closer to a still-image decode every couple of seconds than to running a
decoder; and taps sit after decryption, which is what makes end-to-end encryption
and content classification compatible rather than a choice. What the SDK cannot
do is tell an on-device model from an upload, and the documentation says so
instead of implying otherwise.

[`docs/PLAN.md`](docs/PLAN.md) is the full design and build order — what is
built, what is next, and the reasoning behind the decisions that are hard to
reverse. It is checked against this repository on every push, so an item marked
done has files on disk to show for it and an item marked pending does not.
[`docs/PLAN-CHECK.md`](docs/PLAN-CHECK.md) explains how that works and what to
do when it fails.

## Layout

```
rs/          Rust: protocol, transport, relay, client core, CLI
go/          Go: control plane and director (identity, tokens, rooms, policy)
proto/       The gRPC contract: admin API, and the internal relay link
sdk/         SDKs: go and rust for backends, android and web for clients
schemas/     Track layouts on the MOQOM record format, and warehouse schemas
agents/      Reference AI agents
samples/     Quickstarts, reference app, platform showcases
deploy/      Terraform
docs/        The plan, and the rules that keep it true
tools/       Repository-level checks
vendor/      Vendored dependencies (quinn, verified unmodified in CI)
```

The Rust/Go split is by role, not preference: Go owns request/response state
backed by Postgres, Rust owns everything on the media path. Nothing written in
Go sits between a publisher and a subscriber.

`sdk/go` and `sdk/rust` are separate modules rather than part of `go/` and `rs/`,
and for the same reason twice. They are Apache-2.0 against a BUSL-1.1 server, so
the boundary has to be a contract rather than a package. And a backend that takes
a dependency on the Rust SDK should not thereby take one on our transport tree —
`rs/` patches crates.io to a vendored quinn, and a patch applies to a whole
workspace, so `sdk/rust` is its own workspace with no QUIC in its graph at all.

## Other repositories

| Repository | License | What |
| --- | --- | --- |
| [`moqom-swift`](https://github.com/moqom-cloud/moqom-swift) | Apache-2.0 | `MoqomKit`, the Apple-platform SDK, and the `MoqomRoom` sample |
| `moqom-go` *(not yet created)* | Apache-2.0 | Mirror of `sdk/go`, which a Go import path forces |
| `moqom-obs` *(not yet created)* | GPL-2.0-or-later | The OBS output plugin |

The default is to keep things here — a protocol still moving through drafts
wants the codec, the relay, and the SDKs changing in one commit. Something
leaves only when staying breaks it, and so far two things do.

**Swift, because of SwiftPM.** It resolves a git dependency by looking for
`Package.swift` at the repository *root* and cannot be pointed at a
subdirectory, so a package nested in this tree is not consumable by URL at all.
Android and web have no such constraint — Gradle and npm resolve published
artifacts and do not care where the source lives — so those SDKs stay in `sdk/`.
The `MoqomRoom` sample moved with it, because a sample that can only depend on a
released version stops being an integration test.

**Go, because an import path is a URL.** `go get` resolves a module by fetching
its path, so the Go SDK's path decides where it lives: `sdk/go` declares itself
`github.com/moqom-cloud/moqom-go` and will be published from a repository of that
name. It is developed here — the control plane's own tests exercise it against the
real server, which is worth more than a mirror's CI — and consumed locally through
a `replace` directive until it is published. The Rust SDK has no such constraint,
since Cargo resolves a published crate and does not care where its source lived.

**OBS, because of the GPL.** A plugin links `libobs` and is a combined work, so
its source must be public and GPL-licensed — which this repository, private and
half BUSL-1.1, cannot be. Only Apache-2.0 code may cross that boundary; see
[LICENSING.md](LICENSING.md#the-gpl-boundary-plugins-for-hosts-we-do-not-own)
for what that costs. The same reasoning applies to the next host integration,
so treat one as a new repository by default.

Effects are not in this category. `moqom-fx` is the runtime — frame contract,
shader interpreter, watchdog — and is client core, so it stays in `rs/`. An
individual effect is WGSL and a parameter schema we execute, or a third-party
binary under contract, and is versioned by whoever ships it.

## Development

Requires the toolchain pinned in `rust-toolchain.toml` (rustup installs it
automatically), Go 1.23+, and Docker.

```sh
cd rs
cargo test --all-features
cargo clippy --all-targets --all-features -- -D warnings
cargo fmt --all --check
```

The control plane, the SDKs, and the check that they agree:

```sh
cd go && go test ./...          # includes the Go SDK against the real server
cd sdk/rust && cargo test       # the Rust SDK against a stub
./tools/interop.sh              # both SDKs against a real moqom-control
```

`tools/interop.sh` is the one that earns its keep. A capability is a bit in the
SDKs and an enum on the wire, and a mapping table where two rows point at the same
wire value would pass every test in every one of those packages while handing a
moderator a power somebody meant to withhold. So it sends all twenty-one
capabilities one at a time through a real server and reads each one back.

Regenerating the gRPC stubs needs `protoc` and is not part of a normal build,
because a published SDK cannot demand it of the people who depend on it:

```sh
./tools/generate-proto.sh                                 # Go
cd sdk/rust && MOQOM_REGENERATE_PROTO=1 cargo build       # Rust
```

## Testing the codec

A wire codec fails in a small number of ways, and they recur for every message
type. `encoded_len` disagreeing with `encode` corrupts the enclosing frame's
length field rather than failing locally. A peer-supplied count drives an
allocation before anything bounds it. A decoder reads past its own region and
desynchronises everything after it. A field is silently dropped on decode, which
a round-trip test can never see because it only ever replays values the encoder
produced.

Eighteen control messages and two data-plane mappings share the same handful of
failure modes, so the contract is stated once, in `rs/moqom-proto/src/testing.rs`, and enforced
uniformly. Adding a wire type means implementing `Codec` and adding one line to
`assert_all_decoder_contracts`; it then inherits every check, and an omission is
visible in one place instead of being a hazard you had to remember.

One measurement shaped the design. Against 20,000 uniformly random inputs, the
`SUBSCRIBE` decoder succeeded **zero** times and the frame decoder once — so the
idempotence check, the only one that can catch a dropped field, was never
running. Random bytes are close to worthless against a structured format. The
suite instead mutates valid encodings from `seed_corpus()`, which lifts those
counts into the thousands, and
`mutation_actually_reaches_the_interesting_decoders` asserts a floor so it
cannot quietly regress to testing nothing.

Fuzzing runs the same assertions with coverage guidance:

```sh
cargo run --example seed-corpus --features testing -- fuzz/corpus/decoders
cargo +nightly fuzz run decoders
```

Seeding is not optional, for the same reason. CI runs a 60-second smoke pass per
target on every change and deeper scheduled runs in `fuzz.yml`. Crashes get
transcribed into `REGRESSION_CORPUS`, so they run on stable forever rather than
living in an artifact nobody opens again.

**What none of this catches: misreading the draft.** A rule misread the same way
in both the encoder and the decoder is self-consistent, and every check here
passes. Only differential testing against an independent implementation closes
that gap. The first of it exists: `MoqomKit`'s codec was written separately from
this one, and its interop suite runs a Swift publisher and a Swift subscriber
through `moqom-relay` over a socket. Two implementations that agree is a weaker
claim than two written from the draft by different people, so interop against
`moq-rs` and `moxygen` remains a milestone rather than a nice-to-have.

## Design notes

Two documents are worth reading before changing protocol code:

- `rs/moqom-proto/src/coding.rs` — the streaming-decode contract. Only
  `VarInt::decode` leaves the buffer intact on a short read; assuming that of
  other decoders will corrupt a session.
- `rs/moqom-proto/src/error.rs` — why "incomplete" and "malformed" are
  different types. One means wait, the other means close the connection.

## Licence

Dual-licensed. Protocol, client core, SDKs, schemas, and samples are
Apache-2.0; the relay, control plane, and `moqom-cc` are BUSL-1.1, converting to
Apache-2.0 four years after each release. The Business Source License permits
self-hosting for your own applications and users, including commercially, and
prohibits only offering MOQOM to third parties as a competing hosted service.

See [LICENSING.md](LICENSING.md) for the full map and the reasoning.

## Contributing

Contributions require signing the [CLA](.github/CLA.md). MOQOM is dual-licensed
and the CLA permits relicensing — Section 4 spells out exactly what that means,
because it is the term most worth reading before you agree to it.
