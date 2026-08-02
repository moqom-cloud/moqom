#!/usr/bin/env bash
#
# Runs the SDKs and the relay against the real control plane.
#
# The unit tests in each repository check that it agrees with itself. This checks the
# seam the unit tests cannot reach: that the Rust SDK, the Go SDK, the relay and the Go
# control plane agree about what a capability bit means, what a closed room does, what
# comes back on an event stream, and whose connection a ban closes. A disagreement there
# passes every test in every one of those crates.
#
# Needs a Go toolchain and a Rust toolchain, which is why it is a script rather than part
# of `cargo test`.
#
# It also needs the other repositories checked out, since each is consumable only from a
# repository of its own. MOQOM_GO_SDK, MOQOM_RUST_SDK and MOQOM_ENGINE override; the
# defaults assume siblings, which is where they land if you cloned them into the same
# directory.

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

go_sdk="${MOQOM_GO_SDK:-$root/../moqom-go}"
rust_sdk="${MOQOM_RUST_SDK:-$root/../moqom-rust}"
engine="${MOQOM_ENGINE:-$root/../moqom-engine}"

for repository in "$go_sdk" "$rust_sdk" "$engine"; do
  if [[ ! -d "$repository" ]]; then
    echo "nothing at $repository" >&2
    echo "check out moqom-go, moqom-rust and moqom-engine beside this repository, or" >&2
    echo "set MOQOM_GO_SDK, MOQOM_RUST_SDK and MOQOM_ENGINE." >&2
    exit 1
  fi
done

binary="$(mktemp -d)/moqom-control"
trap 'rm -rf "$(dirname "$binary")"' EXIT

echo "==> building the control plane"
# Pinned toolchain: the modules declare go 1.23 and letting the toolchain upgrade itself
# would test a different compiler than the one CI and production use.
(cd go && GOTOOLCHAIN=local go build -o "$binary" ./cmd/moqom-control)

echo "==> go: the control plane's own tests, including the Go SDK against it"
(cd go && GOTOOLCHAIN=local go test ./...)

echo "==> rust: the SDK against the control plane"
(cd "$rust_sdk" && MOQOM_CONTROL_BIN="$binary" cargo test -p moqom-sdk --test interop -- --ignored)

# One at a time: each test starts its own control plane and its own relay, and the
# assertions are about who is in a room. Running them concurrently would still pass,
# but a failure would be about scheduling rather than about the protocol.
echo "==> rust: the relay against the control plane"
(cd "$engine" && MOQOM_CONTROL_BIN="$binary" \
  cargo test -p moqom-relay --test interop_link -- --ignored --test-threads=1)

echo "==> ok"
