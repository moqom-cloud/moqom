# MOQOM

A Media over QUIC platform: relay, control plane, and client SDKs for iOS,
Android, web, and desktop.

Targets [`draft-ietf-moq-transport-19`](https://datatracker.ietf.org/doc/draft-ietf-moq-transport/).
The protocol implementation is our own rather than a fork, because owning the
wire format is what makes the congestion controller, the object-to-transport
mapping, and end-to-end encryption our decisions instead of someone else's.

## Status

Phase 0. `moqom-proto` covers the object model, the data plane — object
datagrams and subgroup streams — and every control message in draft-19 except
the fetch object framing used for VOD playback. Nothing connects to anything
yet. `moqom-transport` accepts sessions over both QUIC and WebTransport on one
UDP port, but nothing above it speaks MOQT over them, so the bytes are correct
and now movable rather than moving. See the plan for the full build order.

## Layout

```
rs/          Rust: protocol, transport, relay, client core, CLI
go/          Go: control plane and director (identity, tokens, rooms, policy)
sdk/         Platform SDKs: ios, android, web
schemas/     FlatBuffers schemas for catalog, data, and agent tracks
agents/      Reference AI agents
samples/     Quickstarts, reference app, platform showcases
deploy/      Terraform
vendor/      Vendored dependencies (quinn, verified unmodified in CI)
```

The Rust/Go split is by role, not preference: Go owns request/response state
backed by Postgres, Rust owns everything on the media path. Nothing written in
Go sits between a publisher and a subscriber.

## Development

Requires the toolchain pinned in `rust-toolchain.toml` (rustup installs it
automatically), Go 1.23+, and Docker.

```sh
cd rs
cargo test --all-features
cargo clippy --all-targets --all-features -- -D warnings
cargo fmt --all --check
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
passes. Only differential testing against an independent implementation, or
third-party vectors, closes that gap — which is why interop against `moq-rs` and
`moxygen` is a milestone rather than a nice-to-have, and why it is scheduled for
as soon as `moqom-transport` can hold a session open.

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
