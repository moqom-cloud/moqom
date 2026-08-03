#!/bin/sh
# Run the store's contract against the deployed database.
#
# The suite that makes the in-memory store and the Postgres one interchangeable
# is worth nothing unless it is run against the Postgres a deployment actually
# uses -- same version, same collation, same defaults as the thing serving. That
# instance has no public address, so this runs the suite as a Job in the cluster
# rather than from a laptop through a proxy.
#
#   deploy/tools/conformance.sh
#
# In its own database, created here and dropped after. The suite makes a schema
# per case and drops it, so pointing it at `moqom` would probably be harmless;
# "probably harmless" is not a good enough reason to run DDL next to live rows.
#
# As the `postgres` user with a password generated for this run and scrambled
# after, the same trick grant-database.sh uses. The control plane's own IAM role
# is deliberately not used: it holds what a control plane needs, and the suite
# creates and drops schemas.
set -eu

PROJECT="${MOQOM_PROJECT:-moqom-503700}"
PREFIX="${MOQOM_PREFIX:-moqom-dev}"
REGION="${MOQOM_REGION:-us-west1}"
INSTANCE="${MOQOM_SQL_INSTANCE:-$PREFIX-pg}"
DATABASE="${MOQOM_DATABASE:-conformance}"
CLUSTER="${MOQOM_CLUSTER:-$PREFIX-cluster}"
NAMESPACE="${MOQOM_NAMESPACE:-moqom}"
IMAGE="${MOQOM_CONTROL_IMAGE:-us-docker.pkg.dev/$PROJECT/moqom-dev/moqom-control:dev}"

export CLOUDSDK_METRICS_ENVIRONMENT=datacloud.cursor
GCLOUD="gcloud --project=$PROJECT --quiet"

say() { printf '==> %s\n' "$*"; }

WORK="$(mktemp -d)"

HOST="$($GCLOUD sql instances describe "$INSTANCE" --format='value(ipAddresses[0].ipAddress)')"
say "instance $INSTANCE at $HOST, database $DATABASE"

say "reading the cluster"
ENDPOINT="$($GCLOUD container clusters describe "$CLUSTER" --region="$REGION" --format='value(endpoint)')"
$GCLOUD container clusters describe "$CLUSTER" --region="$REGION" \
    --format='value(masterAuth.clusterCaCertificate)' | base64 --decode > "$WORK/ca.crt"

kube() {
    kubectl --server="https://$ENDPOINT" --certificate-authority="$WORK/ca.crt" \
        --token="$($GCLOUD auth print-access-token)" --namespace="$NAMESPACE" "$@"
}

JOB="$PREFIX-conformance"
SECRET="$PREFIX-conformance"

cleanup() {
    status=$?
    # The password first. An interrupt between these would otherwise leave a
    # credential this script generated and nobody rotated.
    $GCLOUD sql users set-password postgres --instance="$INSTANCE" \
        --password="$(openssl rand -hex 24)" >/dev/null 2>&1 || true
    kube delete secret "$SECRET" --ignore-not-found >/dev/null 2>&1 || true
    kube delete job "$JOB" --ignore-not-found >/dev/null 2>&1 || true
    # The database is left behind on purpose when the suite fails: whatever it
    # was in the middle of is the only evidence of why.
    if [ "$status" -eq 0 ] && [ -z "${MOQOM_KEEP_DATABASE:-}" ]; then
        $GCLOUD sql databases delete "$DATABASE" --instance="$INSTANCE" >/dev/null 2>&1 || true
    fi
    rm -rf "$WORK"
    exit $status
}
trap cleanup EXIT INT TERM

say "creating the database"
$GCLOUD sql databases create "$DATABASE" --instance="$INSTANCE" >/dev/null 2>&1 ||
    say "  it was already there"

TEMPORARY="$(openssl rand -hex 24)"
say "setting a temporary password on the postgres user"
$GCLOUD sql users set-password postgres --instance="$INSTANCE" --password="$TEMPORARY" >/dev/null

# sslmode=require rather than verify-full: the instance refuses anything
# unencrypted, and the certificate it presents is Cloud SQL's own, which no
# public root signs. Distributing that CA to a Job that lives for a minute buys
# protection against an attacker who is already inside this VPC and can forge an
# address on it.
URL="postgres://postgres:$TEMPORARY@$HOST:5432/$DATABASE?sslmode=require"

kube delete job "$JOB" --ignore-not-found >/dev/null 2>&1 || true
kube create secret generic "$SECRET" --from-literal=url="$URL" \
    --dry-run=client -o yaml | kube apply -f - >/dev/null

say "running the contract in the cluster"
cat > "$WORK/job.yaml" <<YAML
apiVersion: batch/v1
kind: Job
metadata:
  name: $JOB
  namespace: $NAMESPACE
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      volumes:
        # A test binary writes to TMPDIR, and the root filesystem is read-only
        # for the same reason it is everywhere else here.
        - name: tmp
          emptyDir: {}
      containers:
        - name: conformance
          image: $IMAGE
          command: ["/moqom-conformance", "-test.v", "-test.timeout=10m"]
          volumeMounts:
            - name: tmp
              mountPath: /tmp
          env:
            - name: MOQOM_TEST_POSTGRES
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
              cpu: 500m
              memory: 512Mi
YAML
kube apply -f "$WORK/job.yaml" >/dev/null

# Polled rather than `kubectl wait`, which takes one condition and so has to be
# told to wait for success -- and then sits out the whole timeout after a
# failure, by which point the pod it would have explained has been collected. No
# ttlSecondsAfterFinished either, for the same reason: the Job is deleted on the
# way out of this script, once its logs have been read.
outcome=""
for _ in $(seq 1 300); do
    if [ "$(kube get "job/$JOB" -o jsonpath='{.status.succeeded}' 2>/dev/null)" = "1" ]; then
        outcome="passed"
        break
    fi
    if [ -n "$(kube get "job/$JOB" -o jsonpath='{.status.failed}' 2>/dev/null)" ]; then
        outcome="failed"
        break
    fi
    sleep 2
done

case "$outcome" in
    passed)
        kube logs "job/$JOB" 2>&1 | tail -15 | sed 's/^/    /'
        say "the store keeps its contract against $INSTANCE"
        ;;
    failed)
        say "the suite did not pass; its output follows"
        kube logs "job/$JOB" 2>&1 | sed 's/^/    /'
        kube describe "job/$JOB" 2>&1 | tail -15 | sed 's/^/    /'
        exit 1
        ;;
    *)
        say "the suite is still running after ten minutes; leaving the Job for inspection"
        kube logs "job/$JOB" --tail=40 2>&1 | sed 's/^/    /'
        # Trapped cleanup would delete it, and the point of saying this is that
        # somebody is going to go and look.
        trap - EXIT INT TERM
        exit 1
        ;;
esac
