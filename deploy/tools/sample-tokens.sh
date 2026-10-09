#!/bin/sh
# Deploy the sample app's token server (go/cmd/moqom-sample-tokens) to Cloud Run.
#
# What a phone running the sample app signs in to instead of holding a
# credential: it verifies Sign in with Apple and mints through the admin API with
# an account only this service holds. The account can publish in the tenant and
# do nothing else, and the rooms it will mint for are a fixed list.
#
# Idempotent: every value that exists is reused. The control plane is restarted
# only on the run that adds the account.
#
#   deploy/tools/sample-tokens.sh
#
# Run from anywhere; it builds from the directory holding moqom, moqom-go and
# moqom-security (override with MOQOM_SOURCE_ROOT, MOQOM_SOURCE_GO and
# MOQOM_SOURCE_SECURITY when those are worktrees under other names).
set -eu

PROJECT="${MOQOM_PROJECT:-moqom-503700}"
PREFIX="${MOQOM_PREFIX:-moqom-dev}"
REGION="${MOQOM_REGION:-us-west1}"
CLUSTER="${MOQOM_CLUSTER:-$PREFIX-cluster}"
NAMESPACE="${MOQOM_NAMESPACE:-moqom}"
TENANT="${MOQOM_TENANT:-moqom}"
SERVICE="$PREFIX-sample-tokens"
AUDIENCES="${MOQOM_TOKENS_AUDIENCES:-cloud.moqom.room}"
ROOMS="${MOQOM_TOKENS_ROOMS:-demo,bench}"
IMAGE="us-docker.pkg.dev/$PROJECT/$PREFIX/moqom-sample-tokens:dev"

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="${MOQOM_SOURCE_ROOT:-$(cd "$HERE/../.." && pwd)}"
SOURCE_GO="${MOQOM_SOURCE_GO:-$(dirname "$ROOT")/moqom-go}"
SOURCE_SECURITY="${MOQOM_SOURCE_SECURITY:-$(dirname "$ROOT")/moqom-security}"

export CLOUDSDK_METRICS_ENVIRONMENT=datacloud.cursor
GCLOUD="gcloud --project=$PROJECT --quiet"

say() { printf '==> %s\n' "$*"; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

secret_exists() { $GCLOUD secrets describe "$1" >/dev/null 2>&1; }

# Create the secret if needed and give it a first version from the generator.
ensure() {
    id="$1"
    generator="$2"
    secret_exists "$id" || $GCLOUD secrets create "$id" --replication-policy=automatic >/dev/null
    if ! $GCLOUD secrets versions access latest --secret="$id" >/dev/null 2>&1; then
        eval "$generator" | $GCLOUD secrets versions add "$id" --data-file=- >/dev/null
    fi
}

say "credential and enrollment key"
ensure "$SERVICE-credential" "openssl rand -hex 32 | tr -d '\n'"
ensure "$SERVICE-enrollment-key" "openssl rand -base64 32 | tr '+/' '-_' | tr -d '=\n'"

say "client certificate"
if ! $GCLOUD secrets versions access latest --secret="$PREFIX-link-sample-tokens-cert" >/dev/null 2>&1; then
    $GCLOUD secrets versions access latest --secret="$PREFIX-link-ca" > "$WORK/link-ca.pem"
    $GCLOUD secrets versions access latest --secret="$PREFIX-link-ca-key" > "$WORK/link-ca.key"
    openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
        -keyout "$WORK/leaf.key" -out "$WORK/leaf.csr" -subj "/CN=sample-tokens" 2>/dev/null
    cat > "$WORK/ext.cnf" <<EXT
basicConstraints = critical, CA:FALSE
keyUsage = critical, digitalSignature, keyEncipherment
extendedKeyUsage = clientAuth
subjectAltName = DNS:$SERVICE
EXT
    openssl x509 -req -in "$WORK/leaf.csr" -CA "$WORK/link-ca.pem" -CAkey "$WORK/link-ca.key" \
        -CAcreateserial -days 825 -sha256 -extfile "$WORK/ext.cnf" -out "$WORK/leaf.pem" 2>/dev/null
    for half in cert key; do
        id="$PREFIX-link-sample-tokens-$half"
        secret_exists "$id" || $GCLOUD secrets create "$id" --replication-policy=automatic >/dev/null
    done
    $GCLOUD secrets versions add "$PREFIX-link-sample-tokens-cert" --data-file="$WORK/leaf.pem" >/dev/null
    $GCLOUD secrets versions add "$PREFIX-link-sample-tokens-key" --data-file="$WORK/leaf.key" >/dev/null
    rm -f "$WORK/link-ca.key" "$WORK/leaf.key"
fi

say "service account on the control plane"
$GCLOUD secrets versions access latest --secret="$PREFIX-control-accounts" > "$WORK/accounts.json"
if ! grep -q '"sample-tokens"' "$WORK/accounts.json"; then
    say "  adding the sample-tokens account"
    $GCLOUD secrets versions access latest --secret="$SERVICE-credential" > "$WORK/credential"
    TENANT="$TENANT" python3 - "$WORK/accounts.json" "$WORK/credential" <<'PY'
import json, os, sys

path, credential = sys.argv[1], open(sys.argv[2]).read().strip()
with open(path) as f:
    document = json.load(f)
document.setdefault("accounts", []).append({
    "name": "sample-tokens",
    "credential": credential,
    "tenant": os.environ["TENANT"],
    "publish": ["audio", "video", "screen_share", "data"],
})
with open(path, "w") as f:
    json.dump(document, f, indent=2)
PY
    rm -f "$WORK/credential"
    $GCLOUD secrets versions add "$PREFIX-control-accounts" --data-file="$WORK/accounts.json" >/dev/null

    ENDPOINT="$($GCLOUD container clusters describe "$CLUSTER" --region="$REGION" --format='value(endpoint)')"
    $GCLOUD container clusters describe "$CLUSTER" --region="$REGION" \
        --format='value(masterAuth.clusterCaCertificate)' | base64 --decode > "$WORK/ca.crt"
    KUBE="kubectl --server=https://$ENDPOINT --certificate-authority=$WORK/ca.crt --token=$($GCLOUD auth print-access-token)"

    # Only accounts.json is replaced: the secret carries keys this script does
    # not own, and recreating it would drop them.
    ENCODED="$(base64 < "$WORK/accounts.json" | tr -d '\n')"
    printf '{"data":{"accounts.json":"%s"}}' "$ENCODED" > "$WORK/patch.json"
    $KUBE -n "$NAMESPACE" patch secret "$PREFIX-control" --type=merge --patch-file="$WORK/patch.json" >/dev/null
    rm -f "$WORK/patch.json"
    $KUBE -n "$NAMESPACE" rollout restart "deployment/$PREFIX-control" >/dev/null
    $KUBE -n "$NAMESPACE" rollout status "deployment/$PREFIX-control" --timeout=300s
fi
rm -f "$WORK/accounts.json"

say "runtime identity"
ACCOUNT="$SERVICE@$PROJECT.iam.gserviceaccount.com"
$GCLOUD iam service-accounts describe "$ACCOUNT" >/dev/null 2>&1 ||
    $GCLOUD iam service-accounts create "$SERVICE" --display-name="Sample app token server" >/dev/null
for id in "$SERVICE-credential" "$SERVICE-enrollment-key" "$PREFIX-link-ca" \
          "$PREFIX-link-sample-tokens-cert" "$PREFIX-link-sample-tokens-key"; do
    $GCLOUD secrets add-iam-policy-binding "$id" --member="serviceAccount:$ACCOUNT" \
        --role=roles/secretmanager.secretAccessor >/dev/null
done

say "building $IMAGE"
mkdir -p "$WORK/src/moqom" "$WORK/src/moqom-go" "$WORK/src/moqom-security"
rsync -a --exclude .git --exclude '*_test.go' "$ROOT/go" "$WORK/src/moqom/"
rsync -a --exclude .git --exclude '*_test.go' "$SOURCE_GO/" "$WORK/src/moqom-go/"
rsync -a --exclude .git "$SOURCE_SECURITY/go" "$WORK/src/moqom-security/"
$GCLOUD builds submit "$WORK/src" --config="$HERE/cloudbuild-sample-tokens.yaml" >/dev/null

say "deploying $SERVICE"
CONTROL="$($GCLOUD compute forwarding-rules list \
    --filter="region:$REGION AND loadBalancingScheme=INTERNAL" --format='value(IPAddress,ports.list())' \
    | awk '$2 ~ /(^|,)8443(,|$)/ { print $1; exit }')"
[ -n "$CONTROL" ] || { echo "no internal load balancer on 8443 in $REGION" >&2; exit 1; }
$GCLOUD run deploy "$SERVICE" \
    --region="$REGION" \
    --image="$IMAGE" \
    --service-account="$ACCOUNT" \
    --network="$PREFIX" \
    --subnet="$PREFIX-serverless-$REGION" \
    --vpc-egress=private-ranges-only \
    --allow-unauthenticated \
    --max-instances=2 \
    --cpu=1 --memory=256Mi \
    --set-env-vars="^;^MOQOM_TOKENS_TARGET=$CONTROL:8443;MOQOM_TOKENS_TENANT=$TENANT;MOQOM_TOKENS_AUDIENCES=$AUDIENCES;MOQOM_TOKENS_ROOMS=$ROOMS;MOQOM_ADMIN_CA=/secrets/ca/ca.pem;MOQOM_ADMIN_CERT=/secrets/cert/cert.pem;MOQOM_ADMIN_KEY=/secrets/key/key.pem" \
    --set-secrets="MOQOM_TOKENS_CREDENTIAL=$SERVICE-credential:latest,MOQOM_TOKENS_ENROLLMENT_KEY=$SERVICE-enrollment-key:latest,/secrets/ca/ca.pem=$PREFIX-link-ca:latest,/secrets/cert/cert.pem=$PREFIX-link-sample-tokens-cert:latest,/secrets/key/key.pem=$PREFIX-link-sample-tokens-key:latest" \
    >/dev/null

URL="$($GCLOUD run services describe "$SERVICE" --region="$REGION" --format='value(status.url)')"
cat <<EOF

Done. The sample app signs in at:

  $URL

Rooms: $ROOMS (each must exist; a phone cannot create one).
EOF
