#!/bin/sh
# Everything a phone needs to join, as a QR code it can be pointed at.
#
#   deploy/tools/invite.sh [room]
#
# A device has to be told a relay's address, the hash of the certificate to pin,
# where to sign in for tokens, and where the room's keys are delivered. None of
# it can be typed: the hash is forty-four characters of base64 where one
# transposition is a connection that fails with nothing on screen to say why.
#
# The code carries no secret. The phone signs in with Apple at the sample token
# server (deploy/tools/sample-tokens.sh), which mints a token bound to that
# phone's own device key each time it dials. A photograph of the code lets
# somebody find the token server, which is public anyway, and nothing more.
set -eu

if [ "$#" -gt 1 ]; then
    cat >&2 <<'USAGE'
usage: invite.sh [room]

Prints a QR code holding a moqom://join link, and opens the same thing as a
PNG. Scan it with the Camera app; MOQOM opens and asks whether to use it.

  MOQOM_RELAY_HOST     which relay to point at (default: the LA load balancer)
  MOQOM_RELAY_PORT     default 443
  MOQOM_TOKEN_SERVER   where the phone signs in (default: the sample token server)
  MOQOM_MLS_HOST       the key delivery service's address (default: from terraform)
  MOQOM_MLS_PORT       default 8444
  MOQOM_MLS_CERT_HASH  its certificate hash, which is the control plane's
  MOQOM_QR_ONLY        skip the PNG, print only to the terminal
USAGE
    exit 2
fi

ROOM="${1:-}"

PROJECT="${MOQOM_PROJECT:-moqom-503700}"
PREFIX="${MOQOM_PREFIX:-moqom-dev}"
REGION="${MOQOM_REGION:-us-west1}"
GCLOUD="gcloud --project=$PROJECT --quiet"

# The LA forwarding rule, which is where the sample's built-in default points.
# Oregon is 34.53.113.58 and runs the same build.
HOST="${MOQOM_RELAY_HOST:-35.235.101.37}"
PORT="${MOQOM_RELAY_PORT:-443}"

HERE="$(cd "$(dirname "$0")" && pwd)"

say() { printf '==> %s\n' "$*" >&2; }

# The hash the whole fleet shares, read from Secret Manager rather than pasted,
# so a rotation cannot leave this printing codes for a certificate no relay
# serves any more. Matched by shape rather than by line number, so a change to
# that script's wording does not silently produce an empty hash.
say "reading the certificate the relays serve"
HASH="$(
    "$HERE/relay-certificate.sh" \
        | tr -d ' \r' \
        | grep -E '^[A-Za-z0-9+/]{43}=$' \
        | head -1
)"
if [ -z "$HASH" ]; then
    echo "could not read the certificate hash — run relay-certificate.sh and see why" >&2
    exit 1
fi

say "reading the token server"
TOKEN_SERVER="${MOQOM_TOKEN_SERVER:-$(
    $GCLOUD run services describe "$PREFIX-sample-tokens" --region="$REGION" \
        --format='value(status.url)' 2>/dev/null || true
)}"
if [ -z "$TOKEN_SERVER" ]; then
    echo "no token server: run deploy/tools/sample-tokens.sh or set MOQOM_TOKEN_SERVER" >&2
    exit 1
fi

# The key delivery service is on the control plane's public listener, so its
# certificate is the control plane's and not the relay's. Pinning either to the
# other's hash is a connection that fails with nothing on screen to say why.
say "reading the key delivery service"
MLS_HOST="${MOQOM_MLS_HOST:-$(
    cd "$HERE/../envs/dev" 2>/dev/null && terraform output -raw join_endpoint 2>/dev/null || true
)}"
MLS_PORT="${MOQOM_MLS_PORT:-8444}"
MLS_HASH="${MOQOM_MLS_CERT_HASH:-$(
    $GCLOUD secrets versions access latest --secret="$PREFIX-link-server-cert" 2>/dev/null \
        | openssl x509 -outform der 2>/dev/null \
        | openssl dgst -sha256 -binary 2>/dev/null \
        | openssl base64 2>/dev/null \
        | tr -d '\n' || true
)}"

# Only the characters a query string would read as punctuation, so the hash in
# the link is the hash the deployment prints and the two can be compared by eye.
encode() {
    printf '%s' "$1" | sed -e 's|%|%25|g' -e 's|+|%2B|g' -e 's|/|%2F|g' -e 's|=|%3D|g' -e 's|:|%3A|g' -e 's|&|%26|g'
}

LINK="moqom://join?relay=$HOST:$PORT&cert=$(encode "$HASH")&tokens=$(encode "$TOKEN_SERVER")"
if [ -n "$MLS_HOST" ]; then
    LINK="$LINK&mls=$MLS_HOST:$MLS_PORT"
    if [ -n "$MLS_HASH" ]; then
        LINK="$LINK&mlscert=$(encode "$MLS_HASH")"
    fi
fi
if [ -n "$ROOM" ]; then
    LINK="$LINK&room=$(encode "$ROOM")"
fi

PNG=""
if [ -z "${MOQOM_QR_ONLY:-}" ]; then
    PNG="$(mktemp -t moqom-invite).png"
fi

say "the link, and the code for it"
echo
swift "$HERE/qr.swift" "$LINK" ${PNG:+"$PNG"}
echo
echo "$LINK"
echo

if [ -n "$PNG" ]; then
    say "also opened as $PNG, which is easier to scan than a terminal"
    open "$PNG"
fi
say "the phone signs in with Apple at $TOKEN_SERVER and fetches its own tokens"
