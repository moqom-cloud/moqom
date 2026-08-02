#!/usr/bin/env bash
#
# Runs the store contract against a real Postgres.
#
# The conformance suite is the only thing that makes the in-memory store and this
# one interchangeable, and it is worth nothing unless it is actually run against
# a database. `go test ./...` skips it, because a laptop has no Postgres and a
# test that fails when it is absent trains people to ignore failures. This script
# is what runs it, and it defaults to the Cloud SQL instance the deployment uses
# -- the same collation, the same version, the same defaults -- because a suite
# green against a container and red against production has told nobody anything.
#
# Connects through the Cloud SQL Auth Proxy rather than an authorised network:
# the instance then needs no public allowlist, and access is IAM rather than
# whichever address a laptop happens to have today.
#
# Needs: gcloud, cloud-sql-proxy, a Go toolchain, and permission on the project.

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

project="${MOQOM_PROJECT:-moqom-503700}"
instance="${MOQOM_SQL_INSTANCE:-$project:us-west1:moqom-control-db}"
database="${MOQOM_SQL_DATABASE:-moqom}"
user="${MOQOM_SQL_USER:-moqom}"
secret="${MOQOM_SQL_SECRET:-moqom-control-db-password}"

# A high port, so this does not collide with a Postgres somebody is running.
port="${MOQOM_SQL_PORT:-55433}"

for tool in gcloud cloud-sql-proxy go; do
  if ! command -v "$tool" >/dev/null; then
    echo "$tool is not on PATH" >&2
    exit 1
  fi
done

echo "==> fetching the database password"
password="$(gcloud secrets versions access latest --secret="$secret" --project="$project")"

echo "==> starting the Cloud SQL proxy on port $port"
# --gcloud-auth rather than application default credentials: the ADC on a
# developer's machine is a second login that expires separately, and the failure
# when it has ("invalid_rapt") names neither gcloud nor the fix. Borrowing the
# CLI's own token means anybody who can run the gcloud commands above can run
# this.
cloud-sql-proxy --gcloud-auth --port "$port" "$instance" &
proxy=$!
trap 'kill "$proxy" 2>/dev/null || true' EXIT

# Wait for the socket rather than sleeping: the proxy takes a variable moment to
# authenticate, and a fixed sleep is either too short on a bad network or wasted
# on a good one.
for _ in $(seq 1 50); do
  if nc -z 127.0.0.1 "$port" 2>/dev/null; then
    break
  fi
  sleep 0.2
done
if ! nc -z 127.0.0.1 "$port" 2>/dev/null; then
  echo "the proxy never listened on $port" >&2
  exit 1
fi

echo "==> running the store contract against $instance"
export MOQOM_TEST_POSTGRES="postgres://$user:$password@127.0.0.1:$port/$database?sslmode=disable"
# sslmode=disable is not a downgrade: the proxy is the TLS endpoint, and the
# connection it is given is a loopback socket on this machine.

cd go
GOTOOLCHAIN=local go test ./internal/store/... -count=1 "$@"

echo "==> ok"
