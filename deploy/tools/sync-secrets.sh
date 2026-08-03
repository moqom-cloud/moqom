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

# --- The link's certificates ------------------------------------------------
#
# RelayLink is mutually authenticated, and these are what it is authenticated
# with. The interesting direction is the control plane proving itself to a
# relay: over that stream it sends the verification keys the relay will
# authenticate every session against, so a relay that cannot tell the control
# plane from anything else answering to its address is a relay whose idea of who
# may publish is set by whatever that turns out to be.
#
# A private authority rather than a public one, because the control plane's
# address is an internal load balancer and no public authority will issue for an
# RFC1918 address. Nothing is lost by that: both ends are ours, so pinning one
# authority is stronger than trusting the hundred a machine ships with.
#
# The name in the certificate is a name and not the address, so that the load
# balancer can be given a different address without reissuing anything.
SERVER_NAME="${MOQOM_LINK_SERVER_NAME:-moqom-control}"

# An extension file rather than -addext, because this has to run on whatever
# openssl is in front: LibreSSL, which is what a Mac ships as `openssl`, has not
# always had it, and a script that quietly issues a certificate without the
# extensions it asked for produces a handshake failure nobody can read.
extensions() {
    cat > "$WORK/ext.cnf" <<EXT
basicConstraints = critical, CA:FALSE
keyUsage = critical, digitalSignature, keyEncipherment
extendedKeyUsage = $1
subjectAltName = $2
EXT
}

# Issue a leaf from the CA and store both halves, if it is not already there.
# Existing certificates are left alone: reissuing one on every run would mean
# every run is a rolling restart of both tiers.
leaf() {
    name="$1"
    usage="$2"
    sans="$3"

    if $GCLOUD secrets versions access latest --secret="$PREFIX-link-$name-cert" >/dev/null 2>&1; then
        return 0
    fi

    say "  issuing the $name certificate"
    openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
        -keyout "$WORK/$name.key" -out "$WORK/$name.csr" \
        -subj "/CN=$name" 2>/dev/null
    extensions "$usage" "$sans"
    openssl x509 -req -in "$WORK/$name.csr" \
        -CA "$WORK/link-ca.pem" -CAkey "$WORK/link-ca.key" -CAcreateserial \
        -days 825 -sha256 -extfile "$WORK/ext.cnf" \
        -out "$WORK/$name.pem" 2>/dev/null

    $GCLOUD secrets versions add "$PREFIX-link-$name-cert" --data-file="$WORK/$name.pem" >/dev/null
    $GCLOUD secrets versions add "$PREFIX-link-$name-key" --data-file="$WORK/$name.key" >/dev/null
}

say "link certificate authority"
if $GCLOUD secrets versions access latest --secret="$PREFIX-link-ca" >/dev/null 2>&1; then
    $GCLOUD secrets versions access latest --secret="$PREFIX-link-ca" > "$WORK/link-ca.pem"
    $GCLOUD secrets versions access latest --secret="$PREFIX-link-ca-key" > "$WORK/link-ca.key"
else
    say "  generating one, good for ten years"
    # Long-lived because replacing it means replacing every certificate under it
    # at once, in two tiers, in an order that has no correct version. Rotating
    # this is a planned exercise; rotating a leaf is a rerun of this script.
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
        -keyout "$WORK/link-ca.key" -out "$WORK/link-ca.pem" \
        -days 3650 -sha256 -subj "/CN=$PREFIX link authority" \
        -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
        -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null
    $GCLOUD secrets versions add "$PREFIX-link-ca" --data-file="$WORK/link-ca.pem" >/dev/null
    $GCLOUD secrets versions add "$PREFIX-link-ca-key" --data-file="$WORK/link-ca.key" >/dev/null
fi

say "link certificates"
# The service's in-cluster name as well as the short one, so a caller inside the
# cluster and a relay dialling the load balancer verify the same certificate.
leaf server serverAuth "DNS:$SERVER_NAME,DNS:$PREFIX-control.$NAMESPACE.svc.cluster.local,DNS:$PREFIX-control,DNS:localhost"
# One certificate for the tier rather than one per machine. A per-instance
# identity is worth having and needs an issuing service to go with it, since a
# managed instance group creates machines nobody is present for; this is the
# boundary that can be drawn with a secret and a boot script.
leaf relay clientAuth "DNS:$PREFIX-relay"
# For the operator's own tools -- moqom-smoke, join.sh -- which reach the same
# port through a port-forward and are refused without one.
leaf admin clientAuth "DNS:$PREFIX-admin"

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

# The server's half of the link, and the authority it verifies relays with.
# Fetched rather than assumed present in $WORK, because the leaves above are
# only written on the run that issues them.
$GCLOUD secrets versions access latest --secret="$PREFIX-link-server-cert" > "$WORK/link-cert.pem"
$GCLOUD secrets versions access latest --secret="$PREFIX-link-server-key" > "$WORK/link-key.pem"

say "writing the kubernetes secret"
$KUBE create secret generic "$PREFIX-control" \
    --namespace="$NAMESPACE" \
    --from-literal=signing-keys="$SIGNING" \
    --from-file=accounts.json="$WORK/accounts.json" \
    --from-file=link-ca.pem="$WORK/link-ca.pem" \
    --from-file=link-cert.pem="$WORK/link-cert.pem" \
    --from-file=link-key.pem="$WORK/link-key.pem" \
    --dry-run=client -o yaml | $KUBE apply -f - >/dev/null

cat <<EOF

Done. The control plane reads $PREFIX-control in namespace $NAMESPACE.

The admin credential, for calls to the admin API:

  gcloud secrets versions access latest --secret=$PREFIX-admin-credential --project=$PROJECT

The API also asks for a client certificate, which deploy/tools/join.sh fetches
for itself. By hand it is $PREFIX-link-admin-cert, $PREFIX-link-admin-key and
$PREFIX-link-ca, verified against the name $SERVER_NAME.

Changing a value is adding a version in Secret Manager and running this again;
the pods pick it up on their next roll, which for a mounted file is a restart.
EOF
