#!/bin/sh
# Fill the environment's secrets and put them where the control plane reads them.
#
# Two stores, on purpose. Secret Manager is the durable copy: Terraform creates
# the empty containers and never reads them, so no payload is ever written to
# Terraform state. Kubernetes holds a working copy that pods mount. This script
# is the only thing that sees both, and it generates what is missing rather than
# asking somebody to invent a key by hand.
#
# Idempotent. Run it again and it reuses every value that already exists; the
# only way to get a new signing key is to add a version yourself, which is what
# rotation looks like.
#
#   deploy/tools/sync-secrets.sh
#
# Needs kubectl and gcloud. It deliberately does not need the GKE auth plugin:
# it talks to the API server with an access token and the cluster's own CA,
# which is the same thing the plugin would arrange with more moving parts.
set -eu

PROJECT="${MOQOM_PROJECT:-moqom-503700}"
PREFIX="${MOQOM_PREFIX:-moqom-dev}"
REGION="${MOQOM_REGION:-us-west1}"
CLUSTER="${MOQOM_CLUSTER:-$PREFIX-cluster}"
NAMESPACE="${MOQOM_NAMESPACE:-moqom}"
TENANT="${MOQOM_TENANT:-moqom}"

export CLOUDSDK_METRICS_ENVIRONMENT=datacloud.cursor
GCLOUD="gcloud --project=$PROJECT --quiet"

say() { printf '==> %s\n' "$*"; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# --- Secret Manager ----------------------------------------------------------

# Print a secret's current value, generating one from stdin's command if the
# container is empty. The generator runs only when there is nothing there, so a
# second run never invalidates the tokens the first one handed out.
ensure() {
    id="$1"
    generator="$2"

    if $GCLOUD secrets versions access latest --secret="$id" 2>/dev/null; then
        return 0
    fi

    value="$(eval "$generator")"
    printf '%s' "$value" | $GCLOUD secrets versions add "$id" --data-file=- >/dev/null
    printf '%s' "$value"
}

# A 32-byte Ed25519 seed in base64url without padding, which is the form the
# control plane parses. Translated from base64 rather than produced directly:
# `basenc --base64url` would be the obvious tool and it is GNU coreutils, which
# a Mac does not have.
seed_generator="openssl rand -base64 32 | tr '+/' '-_' | tr -d '=\n'"

# Bearer credentials. Hex rather than base64 so that a value can be pasted into
# a shell, a header, or a log filter without anything quoting it differently.
credential_generator="openssl rand -hex 32"

say "signing key"
SIGNING="$(ensure "$PREFIX-cat-signing-key" "$seed_generator")"

say "relay credential"
RELAY_CREDENTIAL="$(ensure "$PREFIX-relay-credential" "$credential_generator")"

say "admin credential"
ADMIN_CREDENTIAL="$(ensure "$PREFIX-admin-credential" "$credential_generator")"

# The account file is derived from the two credentials above rather than
# generated independently, so the token a relay presents is by construction the
# one the control plane will accept.
#
# Two accounts and no more. `admin` is an operator's own key: owner of every
# room in the tenant, which is what creating rooms and issuing bans from a
# laptop requires. `relay` is the tier's key, and `is_relay` is what permits
# attaching to RelayLink -- it is not an owner and cannot mint tokens.
say "service accounts"
cat > "$WORK/accounts.json" <<JSON
{
  "accounts": [
    {
      "name": "admin",
      "credential": "$ADMIN_CREDENTIAL",
      "tenant": "$TENANT",
      "is_owner": true
    },
    {
      "name": "relay",
      "credential": "$RELAY_CREDENTIAL",
      "tenant": "$TENANT",
      "is_relay": true
    }
  ]
}
JSON

if ! $GCLOUD secrets versions access latest --secret="$PREFIX-control-accounts" >/dev/null 2>&1; then
    $GCLOUD secrets versions add "$PREFIX-control-accounts" --data-file="$WORK/accounts.json" >/dev/null
else
    # Regenerated from the same credentials every run, so a difference here
    # means somebody edited the file directly. Keep theirs.
    $GCLOUD secrets versions access latest --secret="$PREFIX-control-accounts" > "$WORK/accounts.json"
fi

# --- Kubernetes --------------------------------------------------------------

say "reading the cluster"
ENDPOINT="$($GCLOUD container clusters describe "$CLUSTER" --region="$REGION" --format='value(endpoint)')"
$GCLOUD container clusters describe "$CLUSTER" --region="$REGION" \
    --format='value(masterAuth.clusterCaCertificate)' | base64 --decode > "$WORK/ca.crt"

KUBE="kubectl --server=https://$ENDPOINT --certificate-authority=$WORK/ca.crt --token=$($GCLOUD auth print-access-token)"

# The namespace may not exist yet: this script runs before the first apply of
# the workload, because the deployment will not start without the secret it
# mounts.
$KUBE create namespace "$NAMESPACE" --dry-run=client -o yaml | $KUBE apply -f - >/dev/null

say "writing the kubernetes secret"
$KUBE create secret generic "$PREFIX-control" \
    --namespace="$NAMESPACE" \
    --from-literal=signing-keys="$SIGNING" \
    --from-file=accounts.json="$WORK/accounts.json" \
    --dry-run=client -o yaml | $KUBE apply -f - >/dev/null

cat <<EOF

Done. The control plane reads $PREFIX-control in namespace $NAMESPACE.

The admin credential, for calls to the admin API:

  gcloud secrets versions access latest --secret=$PREFIX-admin-credential --project=$PROJECT

Changing a value is adding a version in Secret Manager and running this again;
the pods pick it up on their next roll, which for a mounted file is a restart.
EOF
