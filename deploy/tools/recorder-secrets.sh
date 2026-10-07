#!/bin/sh
# Provision the hosted recorder: its key, its is_recorder account, its link
# certificate, and the Kubernetes secret the workers mount.
#
# Run after the first apply that creates module.recorder (which makes the
# Secret Manager containers) and after tools/sync-secrets.sh (which makes the
# link authority and the accounts file). Then run sync-secrets.sh again, so the
# control plane's pod gets the accounts file with the recorder in it.
#
# Idempotent, like sync-secrets.sh: every value that exists is reused, so a
# second run changes nothing and prints the same thumbprint.
#
#   deploy/tools/recorder-secrets.sh
#
# Needs gcloud, kubectl, openssl, python3, and the recorder binary to make the
# key and its thumbprint: MOQOM_RECORDER_BIN, or cargo and the moqom-engine
# checkout beside this repository.
set -eu

PROJECT="${MOQOM_PROJECT:-moqom-503700}"
PREFIX="${MOQOM_PREFIX:-moqom-dev}"
REGION="${MOQOM_REGION:-us-west1}"
CLUSTER="${MOQOM_CLUSTER:-$PREFIX-cluster}"
NAMESPACE="${MOQOM_NAMESPACE:-moqom}"

export CLOUDSDK_METRICS_ENVIRONMENT=datacloud.cursor
GCLOUD="gcloud --project=$PROJECT --quiet"

HERE="$(cd "$(dirname "$0")" && pwd)"
ENGINE="${MOQOM_ENGINE:-$HERE/../../../moqom-engine}"

say() { printf '==> %s\n' "$*" >&2; }

recorder() {
    if [ -n "${MOQOM_RECORDER_BIN:-}" ]; then
        "$MOQOM_RECORDER_BIN" "$@"
    else
        cargo run -q --manifest-path "$ENGINE/Cargo.toml" -p moqom-recorder --bin moqom-recorder -- "$@"
    fi
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
umask 077

fetch() {
    $GCLOUD secrets versions access latest --secret="$1" > "$2" 2>/dev/null
}

# --- The key -----------------------------------------------------------------

# The key the workers sign their proofs with. The control plane binds every
# recorder token to its thumbprint, so this is what makes a stolen token
# useless without the pod's key file.
say "recorder key"
if ! fetch "$PREFIX-recorder-key" "$WORK/recorder-key.pem"; then
    rm -f "$WORK/recorder-key.pem"
    say "  generating one"
    recorder thumbprint --generate "$WORK/recorder-key.pem" >/dev/null
    $GCLOUD secrets versions add "$PREFIX-recorder-key" --data-file="$WORK/recorder-key.pem" >/dev/null
fi
THUMBPRINT="$(recorder thumbprint "$WORK/recorder-key.pem")"

# --- The account -------------------------------------------------------------

say "recorder credential"
if ! fetch "$PREFIX-recorder-credential" "$WORK/credential"; then
    openssl rand -hex 32 | tr -d '\n' > "$WORK/credential"
    $GCLOUD secrets versions add "$PREFIX-recorder-credential" --data-file="$WORK/credential" >/dev/null
fi

# One is_recorder account for the pool, no tenant: it serves every tenant's
# recordings and reaches RecorderLink and nothing else. Added to the accounts
# file if no recorder account is there; an existing one is left exactly as it
# is, and its credential must then be the one above.
say "recorder account"
fetch "$PREFIX-control-accounts" "$WORK/accounts.json" || {
    echo "no $PREFIX-control-accounts yet: run tools/sync-secrets.sh first" >&2
    exit 1
}
status=0
CREDENTIAL_FILE="$WORK/credential" python3 - "$WORK/accounts.json" <<'PY' || status=$?
import json, os, sys

path = sys.argv[1]
with open(path) as f:
    document = json.load(f)
accounts = document.setdefault("accounts", [])
if any(a.get("is_recorder") for a in accounts):
    sys.exit(3)
with open(os.environ["CREDENTIAL_FILE"]) as f:
    credential = f.read().strip()
accounts.append({"name": "recorders", "credential": credential, "is_recorder": True})
with open(path, "w") as f:
    json.dump(document, f, indent=2)
PY
ACCOUNTS_CHANGED=0
case "$status" in
    0)
        say "  adding it to the accounts file"
        $GCLOUD secrets versions add "$PREFIX-control-accounts" --data-file="$WORK/accounts.json" >/dev/null
        ACCOUNTS_CHANGED=1
        ;;
    3) say "  already there" ;;
    *) exit "$status" ;;
esac

# --- The link certificate ----------------------------------------------------

# The control plane's gRPC port asks every caller for a client certificate
# from the link authority, as sync-secrets.sh issues the relay's.
say "recorder link certificate"
fetch "$PREFIX-link-ca" "$WORK/link-ca.pem"
if ! fetch "$PREFIX-link-recorder-cert" "$WORK/link-cert.pem" ||
   ! fetch "$PREFIX-link-recorder-key" "$WORK/link-key.pem"; then
    say "  issuing it"
    fetch "$PREFIX-link-ca-key" "$WORK/link-ca.key"
    openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
        -keyout "$WORK/link-key.pem" -out "$WORK/recorder.csr" \
        -subj "/CN=recorder" 2>/dev/null
    cat > "$WORK/ext.cnf" <<EXT
basicConstraints = critical, CA:FALSE
keyUsage = critical, digitalSignature, keyEncipherment
extendedKeyUsage = clientAuth
subjectAltName = DNS:$PREFIX-recorder
EXT
    openssl x509 -req -in "$WORK/recorder.csr" \
        -CA "$WORK/link-ca.pem" -CAkey "$WORK/link-ca.key" -CAcreateserial \
        -days 825 -sha256 -extfile "$WORK/ext.cnf" \
        -out "$WORK/link-cert.pem" 2>/dev/null
    rm -f "$WORK/link-ca.key"
    $GCLOUD secrets versions add "$PREFIX-link-recorder-cert" --data-file="$WORK/link-cert.pem" >/dev/null
    $GCLOUD secrets versions add "$PREFIX-link-recorder-key" --data-file="$WORK/link-key.pem" >/dev/null
fi

# The certificate the phones pin, which the workers pin too.
say "relay certificate"
fetch "$PREFIX-relay-tls-cert" "$WORK/relay-cert.pem"

# --- Kubernetes --------------------------------------------------------------

say "writing the kubernetes secret"
ENDPOINT="$($GCLOUD container clusters describe "$CLUSTER" --region="$REGION" --format='value(endpoint)')"
$GCLOUD container clusters describe "$CLUSTER" --region="$REGION" \
    --format='value(masterAuth.clusterCaCertificate)' | base64 --decode > "$WORK/ca.crt"
KUBE="kubectl --server=https://$ENDPOINT --certificate-authority=$WORK/ca.crt --token=$($GCLOUD auth print-access-token)"
$KUBE create secret generic "$PREFIX-recorder" \
    --namespace="$NAMESPACE" \
    --from-file=recorder-key.pem="$WORK/recorder-key.pem" \
    --from-file=credential="$WORK/credential" \
    --from-file=link-ca.pem="$WORK/link-ca.pem" \
    --from-file=link-cert.pem="$WORK/link-cert.pem" \
    --from-file=link-key.pem="$WORK/link-key.pem" \
    --from-file=relay-cert.pem="$WORK/relay-cert.pem" \
    --dry-run=client -o yaml | $KUBE apply -f - >/dev/null

cat <<EOF

Done. The workers read $PREFIX-recorder in namespace $NAMESPACE.

Turn recording on in the control plane with, in envs/dev/terraform.tfvars:

  recorder_thumbprint = "$THUMBPRINT"

EOF
if [ "$ACCOUNTS_CHANGED" = 1 ]; then
    echo "The accounts file changed: run tools/sync-secrets.sh so the control plane's pod gets it."
fi
