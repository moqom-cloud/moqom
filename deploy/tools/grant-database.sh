#!/bin/sh
# Give the control plane's role the privileges PostgreSQL will not give it.
#
# The control plane authenticates to Cloud SQL as its own Google identity, and
# an IAM role arrives with nothing but the right to connect. PostgreSQL 15
# removed CREATE on the public schema from PUBLIC, so the first migration fails
# with "permission denied for schema public" -- a privilege problem wearing the
# costume of an authentication one.
#
# Only a member of cloudsqlsuperuser can fix it, and the only such member is the
# built-in `postgres` user. Rather than create a standing password for it, this
# generates one, uses it for the length of one Job, and replaces it with a value
# it throws away. Nothing is left to rotate because nothing knows it.
#
# It runs in the cluster rather than here, because the instance has no public
# address: a laptop cannot reach 10.x through a peering it is not part of, and a
# pod can.
#
#   deploy/tools/grant-database.sh
#
# Idempotent, and safe to run after every deploy.
set -eu

PROJECT="${MOQOM_PROJECT:-moqom-503700}"
PREFIX="${MOQOM_PREFIX:-moqom-dev}"
REGION="${MOQOM_REGION:-us-west1}"
INSTANCE="${MOQOM_SQL_INSTANCE:-$PREFIX-pg}"
DATABASE="${MOQOM_DATABASE:-moqom}"
CLUSTER="${MOQOM_CLUSTER:-$PREFIX-cluster}"
NAMESPACE="${MOQOM_NAMESPACE:-moqom}"
IMAGE="${MOQOM_CONTROL_IMAGE:-us-docker.pkg.dev/$PROJECT/moqom-dev/moqom-control:dev}"

export CLOUDSDK_METRICS_ENVIRONMENT=datacloud.cursor
GCLOUD="gcloud --project=$PROJECT --quiet"

say() { printf '==> %s\n' "$*"; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

ROLE="${MOQOM_DB_ROLE:-$PREFIX-control@$PROJECT.iam}"
HOST="$($GCLOUD sql instances describe "$INSTANCE" --format='value(ipAddresses[0].ipAddress)')"
say "instance $INSTANCE at $HOST, role $ROLE"

say "reading the cluster"
ENDPOINT="$($GCLOUD container clusters describe "$CLUSTER" --region="$REGION" --format='value(endpoint)')"
$GCLOUD container clusters describe "$CLUSTER" --region="$REGION" \
    --format='value(masterAuth.clusterCaCertificate)' | base64 --decode > "$WORK/ca.crt"

kube() {
    kubectl --server="https://$ENDPOINT" --certificate-authority="$WORK/ca.crt" \
        --token="$($GCLOUD auth print-access-token)" --namespace="$NAMESPACE" "$@"
}

JOB="$PREFIX-dbadmin"
SECRET="$PREFIX-dbadmin"

scramble() {
    $GCLOUD sql users set-password postgres --instance="$INSTANCE" \
        --password="$(openssl rand -hex 24)" >/dev/null 2>&1 || true
}

cleanup() {
    status=$?
    # Password first: an interrupt between these two would otherwise leave a
    # credential this script generated and nobody rotated.
    scramble
    kube delete secret "$SECRET" --ignore-not-found >/dev/null 2>&1 || true
    kube delete job "$JOB" --ignore-not-found >/dev/null 2>&1 || true
    rm -rf "$WORK"
    exit $status
}
trap cleanup EXIT INT TERM

TEMPORARY="$(openssl rand -hex 24)"
say "setting a temporary password on the postgres user"
$GCLOUD sql users set-password postgres --instance="$INSTANCE" --password="$TEMPORARY" >/dev/null

# sslmode=require rather than verify-full: the instance refuses anything
# unencrypted, and the certificate it presents is Cloud SQL's own, which no
# public root signs. The alternative is distributing the server CA to a Job that
# lives for four seconds.
URL="postgres://postgres:$TEMPORARY@$HOST:5432/$DATABASE?sslmode=require"

kube delete job "$JOB" --ignore-not-found >/dev/null 2>&1 || true
kube create secret generic "$SECRET" --from-literal=url="$URL" \
    --dry-run=client -o yaml | kube apply -f - >/dev/null

say "running the grant in the cluster"
cat > "$WORK/job.yaml" <<YAML
apiVersion: batch/v1
kind: Job
metadata:
  name: $JOB
  namespace: $NAMESPACE
spec:
  backoffLimit: 2
  ttlSecondsAfterFinished: 300
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: dbadmin
          image: $IMAGE
          command: ["/moqom-dbadmin"]
          env:
            - name: MOQOM_DBADMIN_ROLE
              value: "$ROLE"
            - name: MOQOM_DBADMIN_URL
              valueFrom:
                secretKeyRef:
                  name: $SECRET
                  key: url
          securityContext:
            runAsNonRoot: true
            runAsUser: 65532
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          resources:
            requests:
              cpu: 250m
              memory: 256Mi
YAML
kube apply -f "$WORK/job.yaml" >/dev/null

if kube wait --for=condition=complete "job/$JOB" --timeout=180s >/dev/null 2>&1; then
    kube logs "job/$JOB" 2>&1 | sed 's/^/    /'
    say "granted"
else
    say "the job did not complete; its logs follow"
    kube logs "job/$JOB" 2>&1 | sed 's/^/    /'
    kube describe "job/$JOB" 2>&1 | tail -20
    exit 1
fi
