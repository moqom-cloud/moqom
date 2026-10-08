#!/usr/bin/env bash
# Make the management overlay's services, policies and standing identities.
#
#   deploy/tools/ziti-provision.sh                    # apply; safe to rerun
#   deploy/tools/ziti-provision.sh delete-identity NAME
#   deploy/tools/ziti-provision.sh get PATH           # read the management API
#   deploy/tools/ziti-provision.sh enroll-identity NAME '["#role"]' FILE
#
# Talks to the controller's management API (modules/ziti puts it on its own
# port, closed to the internet) through an IAP tunnel, logged in as the default
# admin with the password the VM generated into Secret Manager. The password is
# piped from `gcloud secrets` into the login request and is on no command line;
# the session token is in a 0600 file under a private temporary directory.
#
# A script and not a Terraform provider: there is no maintained OpenZiti
# provider for a self-hosted controller, and a provider would put every
# enrolment token and the control plane's private key into state, which this
# tree keeps free of secret material.
#
# What it makes, by name, so a rerun converges rather than duplicates:
#
#   services   PREFIX-control      RelayLink and the admin API, bound by the control
#                                  plane, the same mTLS gRPC as its internal port
#              PREFIX-api          a second gRPC server with no TLS of its own (the
#                                  circuit is the encryption), where the overlay
#                                  identity is the credential for BYOC relays and
#                                  enterprise backends present a bearer key
#              PREFIX-relay-admin  each relay's full admin surface, one terminator
#                                  per relay, addressed by name
#              PREFIX-relay-ssh    each relay's sshd, the same way (OS Login still
#                                  decides who gets a shell)
#              PREFIX-postgres     Cloud SQL, through the proxy on the overlay VM
#   identities PREFIX-control      the control plane (overlay admin, see below)
#              <operator>          one per person, enrolment token to Secret Manager
#
# Role attributes are the contract with the rest of the tree: #moqom-control
# binds, #moqom-relay is a managed relay (the control plane mints these),
# #moqom-byoc-relay a customer's relay, #moqom-api-client an enterprise backend,
# #moqom-operator a person, #moqom-ops-host the overlay VM's own router.
set -euo pipefail

PROJECT="${MOQOM_PROJECT:-moqom-503700}"
PREFIX="${MOQOM_PREFIX:-moqom-dev}"
REGION="${MOQOM_REGION:-us-west1}"
ZONE="${MOQOM_ZITI_ZONE:-$REGION-b}"
VM="$PREFIX-ziti"
ROUTER="${MOQOM_ZITI_ROUTER:-$PREFIX-router-1}"
OPERATORS="${MOQOM_ZITI_OPERATORS:-elliott}"
DOMAIN="${MOQOM_ZITI_DOMAIN:-$PREFIX.ziti}"
CONTROL_PORT="${MOQOM_CONTROL_GRPC_PORT:-8443}"
RELAY_ADMIN_PORT="${MOQOM_RELAY_ADMIN_LOCAL_PORT:-8082}"
# Loopback ports the relays' tunnelers listen on for the two gRPC services.
PROXY_CONTROL_PORT="${MOQOM_ZITI_PROXY_CONTROL_PORT:-18443}"
PROXY_API_PORT="${MOQOM_ZITI_PROXY_API_PORT:-18444}"
OPERATOR_TOKEN_DAYS="${MOQOM_ZITI_OPERATOR_TOKEN_DAYS:-7}"
TUNNEL_CLIENT_PORT="${MOQOM_ZITI_TUNNEL_CLIENT_PORT:-21280}"
TUNNEL_MGMT_PORT="${MOQOM_ZITI_TUNNEL_MGMT_PORT:-21281}"

export CLOUDSDK_METRICS_ENVIRONMENT=datacloud.cursor
GCLOUD="gcloud --project=$PROJECT --quiet"

say() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

for tool in curl jq openssl ziti; do
    command -v "$tool" >/dev/null || die "$tool is required"
done

WORK="$(mktemp -d)"
chmod 0700 "$WORK"
TUNNELS=()
cleanup() {
    for pid in "${TUNNELS[@]:-}"; do [ -n "$pid" ] && kill "$pid" 2>/dev/null || true; done
    rm -rf "$WORK"
}
trap cleanup EXIT
umask 077

tunnel() {
    local remote="$1" local_port="$2"
    $GCLOUD compute start-iap-tunnel "$VM" "$remote" --zone="$ZONE" \
        --local-host-port="localhost:$local_port" > "$WORK/tunnel-$remote.log" 2>&1 &
    TUNNELS+=("$!")
    # Any TLS answer will do; gcloud's own "listening" line is not reliably printed.
    for _ in $(seq 1 60); do
        curl -sk -o /dev/null --max-time 3 "https://localhost:$local_port/" && return 0
        sleep 1
    done
    cat "$WORK/tunnel-$remote.log" >&2
    die "IAP tunnel to $VM:$remote did not come up"
}

say "opening IAP tunnels to $VM"
tunnel 1280 "$TUNNEL_CLIENT_PORT"
tunnel 1281 "$TUNNEL_MGMT_PORT"

# The controller's CA bundle, over the IAP tunnel: IAP authenticated the path
# to the VM, so trusting what answers there is not trust on first use.
curl -sfk "https://localhost:$TUNNEL_CLIENT_PORT/.well-known/est/cacerts" | openssl base64 -d > "$WORK/ca.p7"
openssl pkcs7 -inform DER -in "$WORK/ca.p7" -print_certs -out "$WORK/ca.pem"
grep -q 'BEGIN CERTIFICATE' "$WORK/ca.pem" || die "no CA bundle from the controller"

API="https://localhost:$TUNNEL_MGMT_PORT/edge/management/v1"

say "logging in to the management API"
$GCLOUD secrets versions access latest --secret="$PREFIX-ziti-admin-password" |
    jq -Rs '{username: "admin", password: .}' |
    curl -sf --cacert "$WORK/ca.pem" -H 'Content-Type: application/json' --data-binary @- \
        "$API/authenticate?method=password" |
    jq -r '"zt-session: " + .data.token' > "$WORK/session.hdr"
grep -q '^zt-session: .' "$WORK/session.hdr" || die "login failed"

api() {
    local method="$1" path="$2" out
    shift 2
    if ! out="$(curl -sS --fail-with-body --cacert "$WORK/ca.pem" -H @"$WORK/session.hdr" \
        -H 'Content-Type: application/json' -X "$method" "$API$path" "$@")"; then
        printf '%s %s: %s\n' "$method" "$path" "$(jq -c '.error // .' <<<"$out" 2>/dev/null || printf '%s' "$out")" >&2
        return 1
    fi
    printf '%s' "$out"
}

id_of() {
    api GET "/$1" -G --data-urlencode "filter=name=\"$2\"" --data-urlencode 'limit=1' | jq -r '.data[0].id // empty'
}

# Create or update an entity by name. The body is complete; PATCH with it
# converges a changed definition without replacing the entity's id.
upsert() {
    local kind="$1" name="$2" body="$3" id
    id="$(id_of "$kind" "$name")"
    if [ -z "$id" ]; then
        id="$(api POST "/$kind" --data-binary "$body" | jq -r .data.id)"
        say "created $kind $name"
    else
        api PATCH "/$kind/$id" --data-binary "$body" > /dev/null
    fi
    printf '%s' "$id"
}

config_type() {
    local id
    id="$(id_of config-types "$1")"
    [ -n "$id" ] && { printf '%s' "$id"; return; }
    # proxy.v1 is read by the Go tunneler but not created by the controller.
    [ "$1" = proxy.v1 ] || die "config type $1 is missing"
    api POST /config-types --data-binary '{"name":"proxy.v1","schema":{}}' | jq -r .data.id
}

config() {
    local name="$1" type="$2" data="$3"
    upsert configs "$name" "$(jq -n --arg n "$name" --arg t "$(config_type "$type")" --argjson d "$data" \
        '{name: $n, configTypeId: $t, data: $d}')" > /dev/null
}

service() {
    local name="$1"
    shift
    local configs config
    configs="$(for config in "$@"; do id_of configs "$config"; echo; done | jq -R . | jq -s 'map(select(. != ""))')"
    [ "$(jq length <<<"$configs")" = "$#" ] || die "service $name: a config is missing"
    upsert services "$name" "$(jq -n --arg n "$name" --argjson c "$configs" \
        '{name: $n, configs: $c, encryptionRequired: true, roleAttributes: []}')" > /dev/null
}

service_policy() {
    local name="$1" type="$2" identities="$3" services="" svc id
    shift 3
    # @ names a service by id in a policy; the names are resolved here.
    for svc in "$@"; do
        id="$(id_of services "$svc")"
        [ -n "$id" ] || die "policy $name: no service $svc"
        services="$services\"@$id\","
    done
    services="[${services%,}]"
    upsert service-policies "$name" "$(jq -n --arg n "$name" --arg t "$type" \
        --argjson i "$identities" --argjson s "$services" \
        '{name: $n, type: $t, semantic: "AnyOf", identityRoles: $i, serviceRoles: $s, postureCheckRoles: []}')" > /dev/null
}

secret_has_version() {
    [ -n "$($GCLOUD secrets versions list "$1" --filter='state=ENABLED' --limit=1 --format='value(name)' 2>/dev/null)" ]
}

identity() {
    local name="$1" admin="$2" roles="$3" id
    id="$(id_of identities "$name")"
    if [ -z "$id" ]; then
        id="$(api POST /identities --data-binary "$(jq -n --arg n "$name" --argjson a "$admin" --argjson r "$roles" \
            '{name: $n, type: "Default", isAdmin: $a, roleAttributes: $r, enrollment: {ott: true}}')" | jq -r .data.id)"
        say "created identity $name"
    else
        api PATCH "/identities/$id" --data-binary "$(jq -n --argjson r "$roles" '{roleAttributes: $r}')" > /dev/null
    fi
    printf '%s' "$id"
}

enrolled() {
    [ "$(api GET "/identities/$1" | jq -r '.data.authenticators.cert != null')" = true ]
}

delete_identity() {
    local id
    id="$(id_of identities "$1")"
    [ -n "$id" ] || { say "no identity $1"; return 0; }
    api DELETE "/identities/$id" > /dev/null
    say "deleted identity $1"
}

case "${1:-apply}" in
    delete-identity)
        [ -n "${2:-}" ] || die "usage: $0 delete-identity NAME"
        delete_identity "$2"
        exit 0
        ;;
    get)
        [ -n "${2:-}" ] || die "usage: $0 get PATH"
        api GET "$2" | jq .
        exit 0
        ;;
    enroll-identity)
        # A short-lived identity enrolled on this machine, for a test or a CI
        # job: nothing goes to Secret Manager, and delete-identity removes it.
        [ -n "${4:-}" ] || die "usage: $0 enroll-identity NAME ROLES_JSON OUT_FILE"
        [ ! -e "$4" ] || die "$4 exists"
        identity "$2" false "$3" > "$WORK/id"
        api GET "/identities/$(cat "$WORK/id")" | jq -r '.data.enrollment.ott.jwt // empty' > "$WORK/enroll.jwt"
        [ -s "$WORK/enroll.jwt" ] || die "$2 has no enrolment token; delete it and try again"
        ziti edge enroll --jwt "$WORK/enroll.jwt" --out "$4" >&2
        chmod 0600 "$4"
        say "enrolled $2 into $4"
        exit 0
        ;;
    apply) ;;
    *) die "unknown command $1" ;;
esac

# --- Routing -----------------------------------------------------------------

say "edge router and service edge router policies"
upsert edge-router-policies "$PREFIX-all" \
    '{"name":"'"$PREFIX"'-all","semantic":"AnyOf","identityRoles":["#all"],"edgeRouterRoles":["#all"]}' > /dev/null
upsert service-edge-router-policies "$PREFIX-all" \
    '{"name":"'"$PREFIX"'-all","semantic":"AnyOf","serviceRoles":["#all"],"edgeRouterRoles":["#all"]}' > /dev/null

# The overlay VM's router hosts the Postgres service, from the proxy beside it.
ROUTER_IDENTITY="$(id_of identities "$ROUTER")"
[ -n "$ROUTER_IDENTITY" ] || die "edge router $ROUTER has no identity yet; is the VM up?"
api PATCH "/identities/$ROUTER_IDENTITY" --data-binary '{"roleAttributes":["moqom-ops-host"]}' > /dev/null

# --- Services ----------------------------------------------------------------

say "configs and services"
TCP='["tcp"]'

# A relay's tunneler listens on loopback for these and nothing else; an
# operator's Desktop Edge intercepts the hostnames.
config "$PREFIX-control.proxy" proxy.v1 "{\"port\":$PROXY_CONTROL_PORT,\"protocols\":$TCP,\"binding\":\"127.0.0.1\"}"
config "$PREFIX-control.intercept" intercept.v1 \
    "{\"protocols\":$TCP,\"addresses\":[\"control.$DOMAIN\"],\"portRanges\":[{\"low\":$CONTROL_PORT,\"high\":$CONTROL_PORT}]}"
service "$PREFIX-control" "$PREFIX-control.proxy" "$PREFIX-control.intercept"

config "$PREFIX-api.proxy" proxy.v1 "{\"port\":$PROXY_API_PORT,\"protocols\":$TCP,\"binding\":\"127.0.0.1\"}"
config "$PREFIX-api.intercept" intercept.v1 \
    "{\"protocols\":$TCP,\"addresses\":[\"api.$DOMAIN\"],\"portRanges\":[{\"low\":$CONTROL_PORT,\"high\":$CONTROL_PORT}]}"
service "$PREFIX-api" "$PREFIX-api.proxy" "$PREFIX-api.intercept"

# One terminator per relay, named after the relay, so a dial picks the machine:
# <instance>.relays.<domain> from Desktop Edge, or a dial with that identity
# from the SDK (cmd/moqom-overlay -identity).
RELAY_TERMINATOR='$tunneler_id.name.relays.'"$DOMAIN"
config "$PREFIX-relay-admin.host" host.v1 \
    "{\"protocol\":\"tcp\",\"address\":\"127.0.0.1\",\"port\":$RELAY_ADMIN_PORT,\"listenOptions\":{\"identity\":\"$RELAY_TERMINATOR\"}}"
config "$PREFIX-relay-admin.intercept" intercept.v1 \
    "{\"protocols\":$TCP,\"addresses\":[\"*.relays.$DOMAIN\"],\"portRanges\":[{\"low\":$RELAY_ADMIN_PORT,\"high\":$RELAY_ADMIN_PORT}],\"dialOptions\":{\"identity\":\"\$dst_hostname\"}}"
service "$PREFIX-relay-admin" "$PREFIX-relay-admin.host" "$PREFIX-relay-admin.intercept"

config "$PREFIX-relay-ssh.host" host.v1 \
    "{\"protocol\":\"tcp\",\"address\":\"127.0.0.1\",\"port\":22,\"listenOptions\":{\"identity\":\"$RELAY_TERMINATOR\"}}"
config "$PREFIX-relay-ssh.intercept" intercept.v1 \
    "{\"protocols\":$TCP,\"addresses\":[\"*.relays.$DOMAIN\"],\"portRanges\":[{\"low\":22,\"high\":22}],\"dialOptions\":{\"identity\":\"\$dst_hostname\"}}"
service "$PREFIX-relay-ssh" "$PREFIX-relay-ssh.host" "$PREFIX-relay-ssh.intercept"

config "$PREFIX-postgres.host" host.v1 '{"protocol":"tcp","address":"127.0.0.1","port":5432}'
config "$PREFIX-postgres.intercept" intercept.v1 \
    "{\"protocols\":$TCP,\"addresses\":[\"postgres.$DOMAIN\"],\"portRanges\":[{\"low\":5432,\"high\":5432}]}"
service "$PREFIX-postgres" "$PREFIX-postgres.host" "$PREFIX-postgres.intercept"

# --- Policies ----------------------------------------------------------------

say "service policies"
service_policy "$PREFIX-control-bind" Bind '["#moqom-control"]' "$PREFIX-control" "$PREFIX-api"
service_policy "$PREFIX-control-dial" Dial '["#moqom-relay","#moqom-operator"]' "$PREFIX-control"
service_policy "$PREFIX-api-dial" Dial '["#moqom-byoc-relay","#moqom-api-client","#moqom-operator"]' "$PREFIX-api"
service_policy "$PREFIX-relay-bind" Bind '["#moqom-relay"]' "$PREFIX-relay-admin" "$PREFIX-relay-ssh"
service_policy "$PREFIX-ops-bind" Bind '["#moqom-ops-host"]' "$PREFIX-postgres"
service_policy "$PREFIX-operator-dial" Dial '["#moqom-operator"]' \
    "$PREFIX-relay-admin" "$PREFIX-relay-ssh" "$PREFIX-postgres"

# --- Identities --------------------------------------------------------------

# The control plane. An overlay admin, because it creates and deletes the relay
# and BYOC identities; see docs/OPENZITI-AND-P2P.md for why that is acceptable
# here and what would narrow it. Enrolled on this machine into a private
# temporary directory and piped straight to Secret Manager; enrolled again
# only when Secret Manager has no copy.
CONTROL="$PREFIX-control"
if secret_has_version "$PREFIX-ziti-control-identity"; then
    identity "$CONTROL" true '["moqom-control"]' > /dev/null
else
    say "enrolling $CONTROL into $PREFIX-ziti-control-identity"
    delete_identity "$CONTROL"
    id="$(identity "$CONTROL" true '["moqom-control"]')"
    api GET "/identities/$id" | jq -r .data.enrollment.ott.jwt > "$WORK/control.jwt"
    ziti edge enroll --jwt "$WORK/control.jwt" --out "$WORK/control.json" >/dev/null
    $GCLOUD secrets versions add "$PREFIX-ziti-control-identity" --data-file="$WORK/control.json" >/dev/null
    rm -f "$WORK/control.json" "$WORK/control.jwt"
fi

# People. The token goes to Secret Manager and nowhere else; it is one use, and
# reissued here when it has expired or is about to.
for person in $OPERATORS; do
    id="$(identity "$person" false '["moqom-operator"]')"
    if enrolled "$id"; then
        continue
    fi
    secret="$PREFIX-ziti-operator-$person"
    expires="$(api GET "/identities/$id" | jq -r '.data.enrollment.ott.expiresAt // empty')"
    soon="$(date -u -v+1d +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '+1 day' +%Y-%m-%dT%H:%M:%SZ)"
    if [ -n "$expires" ] && [[ "$expires" > "$soon" ]] && secret_has_version "$secret"; then
        continue
    fi
    for enrollment in $(api GET /enrollments -G --data-urlencode "filter=identity=\"$id\"" | jq -r '.data[].id'); do
        api DELETE "/enrollments/$enrollment" > /dev/null
    done
    until="$(date -u -v+"${OPERATOR_TOKEN_DAYS}"d +%Y-%m-%dT%H:%M:%SZ 2>/dev/null ||
        date -u -d "+$OPERATOR_TOKEN_DAYS days" +%Y-%m-%dT%H:%M:%SZ)"
    api POST /enrollments --data-binary "$(jq -n --arg i "$id" --arg u "$until" \
        '{method: "ott", identityId: $i, expiresAt: $u}')" > /dev/null
    api GET "/identities/$id" | jq -j .data.enrollment.ott.jwt |
        $GCLOUD secrets versions add "$secret" --data-file=- >/dev/null
    say "enrolment token for $person is in $secret, valid until $until"
done

say "done"
