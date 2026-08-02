#!/bin/sh
# Issue the certificate the relay tier serves, and print the hash clients pin.
#
# One certificate for every relay in every region, which is the point. The
# phones pin a SHA-256 of the DER, so a per-machine or per-region certificate
# would mean a client had to know which relay it was about to reach before it
# could decide whether to trust it. A fleet needs one certificate distributed to
# N machines: a distribution problem, not an issuance problem.
#
# Self-signed, and that is a dev decision rather than a permanent one. The
# phones already pin, so a public authority buys nothing here; a browser would
# need one, and so would anything that cannot be told a hash in advance. That is
# the day this grows an ACME client and stops being a script.
#
#   deploy/tools/relay-certificate.sh
#
# Idempotent: it reuses the certificate already in Secret Manager and prints its
# hash. Pass --rotate to issue a new one, which invalidates every hash already
# handed out.
set -eu

PROJECT="${MOQOM_PROJECT:-moqom-503700}"
PREFIX="${MOQOM_PREFIX:-moqom-dev}"
DAYS="${MOQOM_CERT_DAYS:-365}"

export CLOUDSDK_METRICS_ENVIRONMENT=datacloud.cursor
GCLOUD="gcloud --project=$PROJECT --quiet"

CERT_SECRET="$PREFIX-relay-tls-cert"
KEY_SECRET="$PREFIX-relay-tls-key"

ROTATE=0
[ "${1:-}" = "--rotate" ] && ROTATE=1

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

say() { printf '==> %s\n' "$*"; }

if [ "$ROTATE" -eq 0 ] && $GCLOUD secrets versions access latest --secret="$CERT_SECRET" > "$WORK/cert.pem" 2>/dev/null; then
    say "reusing the certificate already in $CERT_SECRET"
else
    say "issuing a certificate valid for $DAYS days"

    # No SANs for addresses. A pinning client does not check names, and the
    # forwarding rule's address is allocated by the same apply that needs this
    # secret to exist -- so naming the addresses here would be a cycle. The
    # subject says what it is for, which reads correctly in a packet capture and
    # costs nothing.
    # ECDSA P-256, because a browser that trusts a certificate by hash requires
    # exactly that key type -- and a validity no longer than 14 days, which this
    # is not. The phones pin through the native stack, which does not care; the
    # key type is chosen so that the browser case needs a shorter certificate
    # rather than a different one.
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -sha256 -days "$DAYS" -nodes \
        -keyout "$WORK/key.pem" -out "$WORK/cert.pem" \
        -subj "/CN=$PREFIX-relay" \
        -addext "subjectAltName=DNS:$PREFIX-relay" 2>/dev/null

    $GCLOUD secrets versions add "$CERT_SECRET" --data-file="$WORK/cert.pem" >/dev/null
    $GCLOUD secrets versions add "$KEY_SECRET" --data-file="$WORK/key.pem" >/dev/null
    say "stored in $CERT_SECRET and $KEY_SECRET"
fi

# Through a file rather than a pipe: openssl writes DER as binary and some
# builds quietly produce nothing when it is piped straight into the digest,
# which yields a plausible-looking hash of an empty input.
openssl x509 -in "$WORK/cert.pem" -outform DER -out "$WORK/cert.der"
HASH="$(openssl dgst -sha256 -binary "$WORK/cert.der" | base64 | tr -d '\r\n')"

cat <<EOF

Certificate hash, which is what a phone pins:

  $HASH

Set it as MOQOM_RELAY_CERT_HASH. Relays already running do not pick up a new
certificate until they restart, because they read it at boot.
EOF
