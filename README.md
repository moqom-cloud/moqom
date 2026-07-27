# MOQOM

A Media over QUIC platform: relay, control plane, and client SDKs for iOS,
Android, web, and desktop.

Targets [`draft-ietf-moq-transport-19`](https://datatracker.ietf.org/doc/draft-ietf-moq-transport/).
The protocol implementation is our own rather than a fork, because owning the
wire format is what makes the congestion controller, the object-to-transport
mapping, and end-to-end encryption our decisions instead of someone else's.

## Status

Phase 0. `moqom-proto` has variable-length integers and the streaming-decode
contract; nothing connects to anything yet. See the plan for the full build
order.

## Layout

```
rs/          Rust: protocol, transport, relay, client core, CLI
go/          Go: control plane and director (identity, tokens, rooms, policy)
sdk/         Platform SDKs: ios, android, web
schemas/     FlatBuffers schemas for catalog, data, and agent tracks
agents/      Reference AI agents
samples/     Quickstarts, reference app, platform showcases
deploy/      Terraform
vendor/      Vendored dependencies (quinn, unmodified)
```

The Rust/Go split is by role, not preference: Go owns request/response state
backed by Postgres, Rust owns everything on the media path. Nothing written in
Go sits between a publisher and a subscriber.

## Development

Requires the toolchain pinned in `rust-toolchain.toml` (rustup installs it
automatically), Go 1.23+, and Docker.

```sh
cd rs
cargo test
cargo clippy --all-targets -- -D warnings
cargo fmt --all --check
```

## Design notes

Two documents are worth reading before changing protocol code:

- `rs/moqom-proto/src/coding.rs` — the streaming-decode contract. Only
  `VarInt::decode` leaves the buffer intact on a short read; assuming that of
  other decoders will corrupt a session.
- `rs/moqom-proto/src/error.rs` — why "incomplete" and "malformed" are
  different types. One means wait, the other means close the connection.

## Licence

Not yet set, deliberately. See [LICENSING.md](LICENSING.md).
