#!/bin/sh
# Mint a client token for one person joining one room, and print it.
#
# What an application does here is ask its own server, which knows who the user
# signing in is and what they are allowed to do; the server asks the control
# plane, and hands back the token. The sample application has no server, so this
# is one: a person at a terminal, deciding by hand.
#
#   deploy/tools/join.sh ada bench
#
# The room is created if it does not exist, because minting checks that it does
# and there is nothing else standing between a bench run and that error.
#
# The admin API has no public address on purpose -- it is an internal load
# balancer inside the VPC -- so this reaches it the way every other tool here
# does, through a port-forward into the cluster. Needs gcloud, kubectl, and Go.
set -eu

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
    cat >&2 <<'USAGE'
usage: join.sh <username> [room]

Prints a token on stdout and everything else on stderr, so it can be
substituted straight into a launch argument:

  --relay-token "$(deploy/tools/join.sh ada bench)"

The token is good for fifteen minutes, which is the control plane's ceiling.
A session that is already established outlives it -- the credential is checked
when the session starts and not again -- so a long run only needs the token to
be valid at the moment the phone connects.
USAGE
    exit 2
fi

USER_NAME="$1"
ROOM="${2:-}"

PROJECT="${MOQOM_PROJECT:-moqom-503700}"
PREFIX="${MOQOM_PREFIX:-moqom-dev}"
REGION="${MOQOM_REGION:-us-west1}"
CLUSTER="${MOQOM_CLUSTER:-$PREFIX-cluster}"
NAMESPACE="${MOQOM_NAMESPACE:-moqom}"
# The tenant the admin account is scoped to, and the one the client has to name.
TENANT="${MOQOM_TENANT:-moqom}"
# Unlikely to be in use, and only bound to the loopback.
PORT="${MOQOM_MINT_PORT:-18443}"

HERE="$(cd "$(dirname "$0")/../.." && pwd)"

export CLOUDSDK_METRICS_ENVIRONMENT=datacloud.cursor
GCLOUD="gcloud --project=$PROJECT --quiet"

say() { printf '==> %s\n' "$*" >&2; }

WORK="$(mktemp -d)"
FORWARD=""
cleanup() {
    [ -n "$FORWARD" ] && kill "$FORWARD" 2>/dev/null
    rm -rf "$WORK"
}
trap cleanup EXIT

say "reading the cluster"
ENDPOINT="$($GCLOUD container clusters describe "$CLUSTER" --region="$REGION" --format='value(endpoint)')"
$GCLOUD container clusters describe "$CLUSTER" --region="$REGION" \
    --format='value(masterAuth.clusterCaCertificate)' | base64 --decode > "$WORK/ca.crt"

# As sync-secrets.sh: an access token and the cluster's own CA, rather than the
# GKE auth plugin and the kubeconfig it wants to rewrite.
#
# Started inside a subshell so that it is nobody's job here. A background job of
# this shell that is later killed makes the shell announce it -- "Terminated:
# 15", on stderr, after the token has been printed -- and the announcement reads
# like a failure of the thing it is reporting the successful cleanup of.
(
    kubectl --server="https://$ENDPOINT" --certificate-authority="$WORK/ca.crt" \
        --token="$($GCLOUD auth print-access-token)" \
        --namespace="$NAMESPACE" \
        port-forward "svc/$PREFIX-control" "$PORT:8443" >"$WORK/forward.log" 2>&1 &
    echo $! > "$WORK/forward.pid"
)
FORWARD="$(cat "$WORK/forward.pid")"

# Waited for rather than slept past: the forward takes a second or two to bind,
# and a mint that races it fails with a connection refused that reads like the
# control plane being down.
waited=0
while [ "$waited" -lt 30 ]; do
    if grep -q 'Forwarding from' "$WORK/forward.log" 2>/dev/null; then break; fi
    if ! kill -0 "$FORWARD" 2>/dev/null; then
        say "the port-forward died:"
        cat "$WORK/forward.log" >&2
        exit 1
    fi
    sleep 1
    waited=$((waited + 1))
done

# The port asks every caller for a certificate, RelayLink and admin alike: the
# credential below says who this claims to be, and the certificate says it is
# something this deployment issued to. Written to the temporary directory and
# deleted with it, so the private key does not accumulate in a home directory.
$GCLOUD secrets versions access latest --secret="$PREFIX-link-ca" > "$WORK/link-ca.pem"
$GCLOUD secrets versions access latest --secret="$PREFIX-link-admin-cert" > "$WORK/admin-cert.pem"
$GCLOUD secrets versions access latest --secret="$PREFIX-link-admin-key" > "$WORK/admin-key.pem"
chmod 0600 "$WORK/admin-key.pem"

say "minting for $USER_NAME in tenant $TENANT, room ${ROOM:-none}"
MOQOM_MINT_TARGET="127.0.0.1:$PORT" \
MOQOM_MINT_TENANT="$TENANT" \
MOQOM_MINT_CREDENTIAL="$($GCLOUD secrets versions access latest --secret="$PREFIX-admin-credential")" \
MOQOM_ADMIN_CA="$WORK/link-ca.pem" \
MOQOM_ADMIN_CERT="$WORK/admin-cert.pem" \
MOQOM_ADMIN_KEY="$WORK/admin-key.pem" \
MOQOM_ADMIN_SERVER_NAME="${MOQOM_LINK_SERVER_NAME:-moqom-control}" \
    go -C "$HERE/go" run ./cmd/moqom-mint -user "$USER_NAME" -room "$ROOM"
