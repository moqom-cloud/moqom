#!/bin/sh
# Everything a phone needs to join, as a QR code it can be pointed at.
#
#   deploy/tools/invite.sh ada bench
#
# The three things a device has to be told are an address, the hash of the
# certificate to pin, and a credential. None of them can be typed: the hash is
# forty-four characters of base64 where one transposition is a connection that
# fails with nothing on screen to say why, and the token is four hundred that
# expire in fifteen minutes. Until this existed, the only way to get them onto a
# phone was a Mac, a port-forward into the cluster, and `devicectl` — so a phone
# picked up off the desk could not reach a deployed relay at all.
#
# What the code carries has changed, and the trade with it. It used to be a token,
# which made a code worth exactly the fifteen minutes left on it -- reassuring
# from a security point of view and useless in practice, because codes were
# routinely stale by the time somebody held a phone up to one. Now it carries the
# address of the join endpoint and the device credential for it, so the phone
# fetches its own token every time it dials and the code keeps working.
#
# That is a durable secret in something designed to be photographed, and it should
# be read that way: anyone who captures one can join rooms in this tenant until the
# credential is rotated. What bounds it is what the credential can buy -- joining,
# in one tenant, never moderation and never ownership, enforced at the endpoint
# regardless of what is asked for. Print these in front of the person scanning, and
# rotate the secret if a code gets away.
set -eu

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
    cat >&2 <<'USAGE'
usage: invite.sh <username> [room]

Prints a QR code holding a moqom://join link, and opens the same thing as a
PNG. Scan it with the Camera app; MOQOM opens and asks whether to use it.

  MOQOM_RELAY_HOST   which relay to point at (default: the LA load balancer)
  MOQOM_RELAY_PORT   default 443
  MOQOM_QR_ONLY      skip the PNG, print only to the terminal

  MOQOM_JOIN_HOST        the token service's address (default: from terraform)
  MOQOM_JOIN_PORT        default 8444
  MOQOM_DEVICE_CREDENTIAL  what the phone presents there (default: Secret Manager)
  MOQOM_JOIN_CERT_HASH   the control plane's certificate hash, not the relay's
USAGE
    exit 2
fi

USER_NAME="$1"
ROOM="${2:-}"

PROJECT="${MOQOM_PROJECT:-moqom-503700}"
GCLOUD="gcloud --project=$PROJECT --quiet"

# The LA forwarding rule, which is where the sample's built-in default points.
# Oregon is 34.53.113.58 and runs the same build.
HOST="${MOQOM_RELAY_HOST:-35.235.101.37}"
PORT="${MOQOM_RELAY_PORT:-443}"

HERE="$(cd "$(dirname "$0")" && pwd)"

say() { printf '==> %s\n' "$*" >&2; }

# The hash the whole fleet shares, read from Secret Manager rather than pasted,
# so a rotation cannot leave this printing codes for a certificate no relay
# serves any more.
say "reading the certificate the relays serve"
# Matched by shape rather than by line number, so a change to that script's
# wording does not silently produce an empty hash and a link nothing can dial.
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

# The device credential and where to spend it, which is what makes a code worth
# scanning tomorrow. Without these the link carries a token instead, and a token
# is good for fifteen minutes -- which meant a code was often stale between being
# printed and being held up to a phone.
say "reading the join endpoint"
JOIN_HOST="${MOQOM_JOIN_HOST:-$(
    cd "$HERE/../envs/dev" 2>/dev/null && terraform output -raw join_endpoint 2>/dev/null || true
)}"
JOIN_PORT="${MOQOM_JOIN_PORT:-8444}"
DEVICE_CREDENTIAL="${MOQOM_DEVICE_CREDENTIAL:-$(
    $GCLOUD secrets versions access latest \
        --secret="${MOQOM_PREFIX:-moqom-dev}-device-credential" 2>/dev/null || true
)}"

# Its certificate, which is the control plane's and not the relay's. Two machines,
# two certificates: pinning either to the other's hash is a connection that fails
# with nothing on screen to say why.
JOIN_HASH="${MOQOM_JOIN_CERT_HASH:-$(
    $GCLOUD secrets versions access latest \
        --secret="${MOQOM_PREFIX:-moqom-dev}-link-server-cert" 2>/dev/null \
        | openssl x509 -outform der 2>/dev/null \
        | openssl dgst -sha256 -binary 2>/dev/null \
        | openssl base64 2>/dev/null \
        | tr -d '\n' || true
)}"

# A token as well, and only as a fallback, and only when the scanning phone has
# been named. A token is bound to a key in one phone's Secure Enclave, so minting
# one here needs to know which phone will scan the link -- which a link meant to
# be handed to anybody does not. Left out, the link carries the device credential
# instead and the phone fetches a token bound to its own key, which is the path
# that works without knowing anything about the phone in advance.
TOKEN=""
if [ -n "${MOQOM_DEVICE:-}" ]; then
    TOKEN="$("$HERE/join.sh" "$USER_NAME" "$ROOM" "$MOQOM_DEVICE" 2>/dev/null || true)"
fi
if [ -z "$TOKEN" ] && [ -z "$DEVICE_CREDENTIAL" ]; then
    echo "no credential of either kind: set MOQOM_DEVICE to the scanning phone's" >&2
    echo "thumbprint for a pasted token, or check the device-credential secret" >&2
    exit 1
fi

# Percent-encoded, because a base64 hash contains `+`, `/` and `=`, and all three
# mean something else in a query string. Encoded here rather than by choosing a
# friendlier alphabet, so the string in the link is the same string the tools
# print and the two can be compared by eye.
# Three characters and no others, because a base64 hash contains exactly three
# that a query string reads as punctuation. Encoding every byte also works and
# is what this did first: it turned forty-four readable characters into a
# hundred and thirty-two of hex, made the code larger for no reason, and lost
# the property the escaping was for — that the hash in the link is the hash the
# deployment prints, so the two can be compared by looking at them.
encode() {
    printf '%s' "$1" | sed -e 's|+|%2B|g' -e 's|/|%2F|g' -e 's|=|%3D|g'
}

LINK="moqom://join?relay=$HOST:$PORT&cert=$(encode "$HASH")"
if [ -n "$TOKEN" ]; then
    LINK="$LINK&token=$TOKEN"
fi
if [ -n "$JOIN_HOST" ] && [ -n "$DEVICE_CREDENTIAL" ]; then
    LINK="$LINK&join=$JOIN_HOST:$JOIN_PORT&key=$DEVICE_CREDENTIAL"
    if [ -n "$JOIN_HASH" ]; then
        LINK="$LINK&joincert=$(encode "$JOIN_HASH")"
    fi
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

if [ -n "$JOIN_HOST" ] && [ -n "$DEVICE_CREDENTIAL" ]; then
    # Worth saying, because it is the opposite of what the previous version of this
    # script printed and the difference matters to whoever is holding the code.
    say "this one enrols the phone: it will fetch its own token on every join,"
    say "so the code keeps working — and so does anyone else who photographs it,"
    say "until the device credential is rotated"
else
    say "good for fifteen minutes; the session outlives it once connected"
fi
