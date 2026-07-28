#!/usr/bin/env bash
#
# Runs the SDKs against the real control plane.
#
# The unit tests in each SDK check that it agrees with itself. This checks the seam the
# unit tests cannot reach: that the Rust SDK, the Go SDK and the Go control plane agree
# about what a capability bit means, what a closed room does, and what comes back on an
# event stream. A disagreement there passes every test in every one of those crates.
#
# Needs a Go toolchain and a Rust toolchain, which is why it is a script rather than part
# of `cargo test`.
#
# It also needs both SDK repositories checked out, since each is consumable only from a
# repository of its own. MOQOM_GO_SDK and MOQOM_RUST_SDK override; the defaults assume
# siblings, which is where they land if you cloned them into the same directory.

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

go_sdk="${MOQOM_GO_SDK:-$root/../moqom-go}"
rust_sdk="${MOQOM_RUST_SDK:-$root/../moqom-rust}"

for sdk in "$go_sdk" "$rust_sdk"; do
  if [[ ! -d "$sdk" ]]; then
    echo "no SDK at $sdk" >&2
    echo "check out moqom-go and moqom-rust beside this repository, or set" >&2
    echo "MOQOM_GO_SDK and MOQOM_RUST_SDK." >&2
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

echo "==> ok"
