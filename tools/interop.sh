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

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

binary="$(mktemp -d)/moqom-control"
trap 'rm -rf "$(dirname "$binary")"' EXIT

echo "==> building the control plane"
# Pinned toolchain: the modules declare go 1.23 and letting the toolchain upgrade itself
# would test a different compiler than the one CI and production use.
(cd go && GOTOOLCHAIN=local go build -o "$binary" ./cmd/moqom-control)

echo "==> go: the control plane's own tests, including the Go SDK against it"
(cd go && GOTOOLCHAIN=local go test ./...)

echo "==> rust: the SDK against the control plane"
(cd sdk/rust && MOQOM_CONTROL_BIN="$binary" cargo test -p moqom-sdk --test interop -- --ignored)

echo "==> ok"
