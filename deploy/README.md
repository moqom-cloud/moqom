# deploy

Terraform for MOQOM on GCP. Two tiers with genuinely different shapes: a relay
tier of VMs behind a UDP load balancer, and a control plane that is a container
somewhere else.

Where exactly differs by environment, and this used to say Cloud Run without
qualification. Dev runs the control plane on GKE Autopilot, because it holds its
own certificate so a relay can authenticate the process rather than whatever
terminates TLS for it, and because a relay reaches it over an internal address
that never leaves the VPC. Prod's configuration still names Cloud Run and has not
been applied, so treat that half as a design and this half as a description.

```
bootstrap/                 State bucket. Run once, by hand, before anything else.
modules/
  project-services/        API enablement, and the wait that makes it usable.
  network/                 VPC, subnets, firewall, Cloud NAT, private services access.
  artifact-registry/       Container images and their retention.
  relay/                   Instance template, regional MIG, autoscaler, UDP NLB.
  control-plane/           One Cloud Run service, instantiated per component. Prod only.
  gke/                     Autopilot cluster and the workload identity bindings on it.
  workload/                One Deployment and its services. What dev runs the control plane as.
  data/                    Cloud SQL Postgres, recording bucket, secret containers.
  recorder/                Hosted recorder workers and the accounts that write and read recordings.
  federation/              Workload identity for CI and for non-GCP relays.
  observability/           Log routing, log-based metrics, alerts, uptime check.
  dns-steering/            Geolocation routing across relay regions.
envs/
  dev/                     Two relay regions, control plane on GKE. The one that is applied.
  prod/                    Three regions, HA database, control plane on Cloud Run. Not applied.
```

Resources are defined in `modules/` and nowhere else. The environment roots hold
sizing, region lists, and wiring; if you find yourself adding a `resource` block
under `envs/`, it belongs in a module.

`federation/` and `dns-steering/` are not in the original module list. They exist
because workload identity and DNS steering each span the others — federation is
consumed by the registry and by storage, steering is consumed by every relay
region at once — and folding either into a neighbour would have created a
dependency cycle between modules.

## Why the relay is not on Cloud Run

MoQ runs over QUIC, and QUIC is UDP. Nothing in GCP's HTTP-serving stack will
carry it: Cloud Run, the global external Application Load Balancer, and both
proxy Network Load Balancers speak TCP or terminate HTTP. The Application LB
does offer HTTP/3 to clients, but it terminates that QUIC connection at the
Google front end and re-originates the request to the backend over HTTP/2, which
throws away the three things that matter — the connection is no longer the
client's, WebTransport's extended CONNECT does not survive the hop, and
per-connection congestion state ends up on a machine we do not control.

So the QUIC connection has to terminate on our own process, and the only GCP
product that delivers an unmodified UDP datagram to our own process is the
**external passthrough Network Load Balancer**. That is the decision the relay
module is built around. WebTransport over HTTP/3 needs nothing extra: it is the
same UDP socket, distinguished by ALPN inside the connection, which is above the
layer a passthrough balancer inspects.

The alternative was GKE with a UDP `Service`, which provisions the same
passthrough NLB underneath — so the real question was what runs the container,
not what carries the packets. The MIG won on kernel tuning (socket buffers,
`SO_REUSEPORT`, gVNIC are natural on a VM and awkward through a pod spec), on
drain semantics (a relay drains over the lifetime of a live stream, not over a
termination grace period), and on the fact that a self-hoster can take the relay
module alone without adopting a cluster.

What that costs: scale-out is a VM boot rather than a pod schedule, roughly
60–90 seconds against 5, so `min_replicas` carries more headroom than it
otherwise would; there is no bin-packing; and rollouts are MIG rolling updates,
which are coarser than a Deployment. If MOQOM later grows sidecars or
co-tenanted workloads, GKE becomes the better answer and the backend service and
forwarding rule stay exactly as they are.

Two consequences worth knowing before you operate this:

- **Health checks cannot speak UDP.** GCP offers no UDP health check, so the
  relay serves an HTTP probe on a separate TCP port and the load balancer steers
  UDP on the strength of it. The probe handler has to assert that the QUIC
  listener is actually accepting, or the balancer's view of the fleet is
  fiction. There are two probes: `/healthz` decides load balancer membership and
  fails immediately on drain, `/livez` decides autohealing and keeps passing
  throughout a drain.
- **QUIC connection migration does not survive the hash.** The balancer cannot
  read a connection ID, so a client that changes address lands on a relay with
  no state for it. `CLIENT_IP` affinity with `PER_SESSION` tracking covers the
  common case — a NAT rebinding that only changes the port — and does nothing
  for a genuine wifi-to-cellular move. Closing that properly needs QUIC-LB style
  routable connection IDs so any relay can forward a stray packet to the one
  holding the session. The cost of `CLIENT_IP` is load skew behind
  carrier-grade NAT, so watch per-instance session counts and not just CPU.

## Reaching the nearest relay

There is no anycast UDP on GCP. A passthrough NLB forwarding rule is regional
and there is no global equivalent for UDP, so "nearest relay" is assembled from
three parts:

1. **Premium Tier addresses.** An international client enters Google's network
   at the PoP nearest to it and crosses the backbone to the region, rather than
   traversing the public internet the whole way. This is the single largest win
   and it is a one-word setting.
2. **A Cloud DNS geolocation policy** across the per-region addresses, in
   `dns-steering`. Geography approximates latency, and resolver location makes
   that approximation unreliable.
3. **`moqom-director`**, which supersedes the DNS policy once it exists, because
   it can measure rather than infer and can hand a client a specific relay.

The gap this leaves is failover. Cloud DNS health-checked routing only supports
internal load balancers, so a region that goes dark keeps being handed out until
something rewrites the record; the TTL is the floor on that recovery, not the
whole of it. Until the director closes it, treat a regional outage as a manual
action with a runbook.

True anycast means Cloudflare Spectrum or BYOIP. Nothing outside the relay
module's forwarding rule assumes the address is regional, so moving the edge to
Cloudflare later replaces one module rather than the design.

## Prerequisites

- Terraform >= 1.9.
- A GCP organisation, a billing account, and one project per environment.
- `roles/owner` or an equivalent bundle on those projects for the first apply.
  Steady-state applies need far less; see the CI section.
- A registrable domain, delegated to the Cloud DNS zone this creates. Nothing
  serves browser clients until that delegation is live, because WebTransport
  requires a publicly trusted certificate.

## Bootstrap order

State lives in GCS, and the bucket that holds it cannot be created by the
configuration that stores its state there. So it is a separate root with no
backend block, applied once:

```sh
cd deploy/bootstrap
cp terraform.tfvars.example terraform.tfvars   # edit
terraform init
terraform apply
```

That leaves a `terraform.tfstate` on disk. It describes one bucket and its IAM,
holds no secrets, and is cheap to reconstruct with `terraform import` if lost —
which is the argument for leaving it local rather than building a
chicken-and-egg migration. It is gitignored. If you would rather not have it at
all, `terraform state push` it into the bucket it just created and add a backend
block; both are defensible.

Then each environment, in order:

```sh
cd deploy/envs/dev
cp backend.hcl.example backend.hcl             # bucket name from the bootstrap output
cp terraform.tfvars.example terraform.tfvars   # edit
terraform init -backend-config=backend.hcl
terraform plan
```

The backend is a partial configuration on purpose: the bucket name is the one
piece of the layout a fork or a self-hoster must change, and keeping it out of
the tracked files means they change nothing tracked.

`envs/dev` and `envs/prod` are separate states with separate lifecycles. There
are no workspaces, because workspaces share a configuration and these two
deliberately do not have the same shape.

Dev runs relays in two regions — `us-west1` and `us-west2` — while everything
else in it is single-region. That is not prod in miniature; it exists because
the latency work compares a relay beside the phones against one a region away,
and a comparison that cannot be reproduced in the environment it was measured in
stops being trusted. It also means the control plane is reached across regions
from the first day, which is where an internal load balancer's regional scope
would otherwise be discovered late: the `Service` carries
`internal-load-balancer-allow-global-access`, and without it the relays in the
second region cannot reach a control plane that is working perfectly.

### What is not created, and why nothing works until it is

`terraform apply` finishes with a dozen empty secrets. This is deliberate: a
`google_secret_manager_secret_version` stores its payload in plaintext in
Terraform state, and state has a wider audience than any of these keys should.
Terraform makes the containers; what goes in them it never sees.

`tools/sync-secrets.sh` fills all but two of them, generating what can be
generated — the token signing key, the relay and admin credentials, the service
account file, and the link's certificate authority with its three leaves. Run it
after the first apply and after any change to that list; it only adds what is
missing, so it is safe to run repeatedly.

The two it cannot generate are the certificate a phone will pin:

```sh
gcloud secrets versions add moqom-prod-relay-tls-cert --data-file=cert.pem
gcloud secrets versions add moqom-prod-relay-tls-key  --data-file=key.pem
```

The relay will not serve TLS until those have versions, and the control plane
will not mint tokens until `sync-secrets.sh` has run. Certificate renewal is out of scope here on purpose: a
fleet of N relays needs one certificate distributed to N machines, which is a
distribution problem rather than an issuance problem, and Secret Manager is the
distribution mechanism. Whatever runs ACME writes here.

### Nothing in the project is hand-made

Everything in `moqom-503700` is now in this tree's state: the network and its
peering, the registry, the GKE Autopilot cluster, the Cloud SQL instance
`moqom-dev-pg` with its IAM users, the recording bucket, both relay managed
instance groups with their load balancers and firewall rules, the control
plane's Kubernetes deployment, and every Secret Manager container.

It was not always. Bring-up left a hand-built `moqom-control-db` instance and a
`moqom-control-db-password` secret beside the real ones, which is the failure
mode worth naming: a project where some resources answer to `terraform destroy`
and some do not has no reliable way to be torn down, costed, or reproduced, and
the hand-made ones are invisible in review. Both were deleted once the Terraform
instance was serving. `gcloud sql instances list` and `gcloud secrets list`
should agree with `terraform state list`; where they do not, one of them is
wrong and it is usually not Terraform.

### Delegating the zone will take email with it

`dns_domain` is the apex, `moqom.cloud.`, so pointing the registrar at the
nameservers Cloud DNS hands out makes Cloud DNS authoritative for the *whole*
domain — not just the `api.` and `edge.` records Terraform creates. Everything
else currently answered at the registrar stops existing the moment delegation
moves, and mail is the part that matters:

```
moqom.cloud.  MX   10 mx.hover.com.cust.hostedemail.com.
moqom.cloud.  TXT  "v=spf1 include:_spf.hostedemail.com ~all"
```

Copy those into the zone *before* changing nameservers, along with any DKIM
selector the mail host publishes. Losing them costs more than ordinary
downtime, because `notification_emails` points at a mailbox on this domain: the
alerting that would tell you something is wrong is delivered to the address the
mistake just disabled. Inbound mail also fails silently from the sender's side
for a while, since resolvers keep serving the old MX until the TTL expires.

The alternative is to leave the apex delegated to the registrar and give
Terraform a subdomain instead — `dns_domain = "net.moqom.cloud."` — which keeps
mail entirely out of the blast radius at the cost of a longer hostname on every
client. Worth taking if nobody is going to own the zone carefully.

## How a relay comes to authenticate anybody

A relay verifies a session's token against the control plane's public keys, and
it is not given them. They arrive over the same stream the moderation directives
do, once when it attaches and again on every rotation, and a relay applies them
wholesale:

```
INFO moqom_relay::link: took the control plane's verification keys pushed=1 verifying=1 first=true
```

`MOQOM_RELAY_JWKS` exists and is deliberately unset here. Distributing a key set
to every machine is a second copy of something the control plane already knows
and already sends, and a copy that can be stale is worse than no copy — it goes
wrong at a rotation, which is the one moment the two halves have to agree. The
file is for a relay with no control plane to be sent anything by; where both are
present the two are unioned, so a push cannot withdraw what an operator wrote
down.

`MOQOM_RELAY_ISSUER` is set, and pins whose tokens count. Without it a relay
accepts a token minted by any deployment that happens to share a key identifier,
which is a real hazard when identifiers are derived from the key and two
environments restore the same secret. So a relay with a control plane or a key
file refuses to start without it, unless `MOQOM_RELAY_ALLOW_ANY_ISSUER=true`
says that was meant.

The control plane is strict in the same way: it will not start without
`MOQOM_CONTROL_TLS_CERT`, `MOQOM_CONTROL_TLS_KEY` and `MOQOM_CONTROL_CLIENT_CA`.
`MOQOM_CONTROL_ALLOW_PLAINTEXT=true` (TLS terminated in front of it, as on Cloud
Run, or a laptop) and `MOQOM_CONTROL_ALLOW_NO_CLIENT_CERT=true` are the explicit
ways out, and each logs a warning at startup.

`MOQOM_CONTROL_STUN` names the STUN server put in every direct grant
(`direct.stun` in a minted token and in `/join`), as `host` or `host:port`;
the port defaults to 3478. The SDKs ask it for the public address they offer a
peer, so direct paths can cross NATs. Every relay answers STUN on UDP 3478
(`MOQOM_RELAY_STUN_ADDR`, `off` to disable). The relay and network modules
open it (`stun_port` and `relay_stun_port`, null to close), and the
environments point `MOQOM_CONTROL_STUN` at the steered edge name. Unset means
grants name no server, and the SDKs offer only local addresses.

`MOQOM_CONTROL_ZITI_IDENTITY` (an enrolled OpenZiti identity file) and
`MOQOM_CONTROL_ZITI_SERVICE` (the service to bind) serve the same gRPC port —
admin API and RelayLink — on an OpenZiti overlay as well, where only identities
the overlay's policy allows can reach it. Both or neither; one alone refuses to
start. The TLS settings above still apply on top of the overlay. The TCP
listener stays up beside it, so a deployment moves callers onto the overlay
before closing the TCP port to them. The rest of the overlay — a second service
for BYOC relays, relay identities, operator access — is in
[The management overlay](#the-management-overlay).

Until the keys land — the first few milliseconds of a relay's life, or forever
for one that cannot reach a control plane — every session is anonymous and
charged to its address. After they land, a session that presents no token is
still admitted and may do nothing at all. Both are logged at startup.

`MOQOM_RELAY_POSSESSION` decides whether holding a token is enough. A token names
the device it was minted for in `cnf.jkt`; `required` makes a session prove it,
by signing the token and this relay's certificate with the key that thumbprint
identifies, at SETUP and again at every renewal, each signature usable once. A
stolen token is then worth nothing without the Secure Enclave it was bound to.
The default is `observed` — check a proof when one is offered, admit a session
that offers none — because a fleet upgrades one build at a time and a relay that
refused the older ones would be an outage. Move to `required` once the fleet is
past it; the log at startup says which of the three is in force, and `observed`
says out loud that it is not enforcing.

Dev is on `required`, and the order it was reached in is the point. The control
plane went first: until it derives `cnf.jkt` from the key a device sends, it puts
the truncated thumbprint there instead, and a relay recomputing the full one from
a proof reads that as the wrong key. Since `observed` refuses a *wrong* proof and
only tolerates a *missing* one, rolling the relay first would have refused exactly
the clients that had been upgraded. Then the phones, while the relays still ignored
proofs, which is what separates "the device key and the minting work" from "the
proofs verify" — two failures with one symptom otherwise. Then `required`.

What that establishes, on the deployed pair rather than in a test:

```
INFO moqom_relay: sessions must prove they hold the key their token names
INFO moqom_relay::session: token refused address=… error=no proof of possession was presented
```

The second line is a client holding a token this deployment minted a minute
earlier, refused because it could not sign for it.

### Why the link is mutually authenticated

Read the paragraph above again with an adversary in it. Whoever the relay is
talking to decides which keys it trusts, and therefore who may publish into
every room on that machine. A bearer credential travelling from relay to control
plane does not help with that: it proves the relay, in the direction that was
already the easy one.

So RelayLink is TLS in both directions, terminated by the control plane process
itself rather than by anything in front of it — a terminator can only prove it
is the terminator, and it is the process that holds the signing keys. The
authority is private, generated by `tools/sync-secrets.sh` and kept in Secret
Manager: the endpoint is an internal load balancer, no public authority issues
for an RFC1918 address, and pinning one authority is stronger than trusting the
hundred a machine ships with.

Three certificates come out of it. The control plane serves the `server` one and
requires a client certificate from everybody, which is `RequireAndVerifyClientCert`
and not the `VerifyClientCertIfGiven` that reads the same in a diagram and
authenticates nobody. Relays present `relay`, one per tier rather than one per
machine — per-instance identity wants an issuing service, since a managed
instance group creates machines nobody is present for. Operators' tools present
`admin`, because being reachable only through a port-forward is not the same as
knowing who is on the other end of it.

What the relay verifies is a name, `moqom-control`, and not the address it
dialled. The address is the load balancer's current value and the certificate
should outlive it, which is what `MOQOM_CONTROL_SERVER_NAME` is for. Attached
looks like this, with the scheme being the visible part:

```
INFO moqom_relay::link: attached to the control plane relay=moqom-dev-relay-us-west1-j318 endpoint=https://10.20.32.4:8443 sessions=0
```

and the control plane says which of the three modes it came up in:

```
{"msg":"control plane listening","address":"[::]:8443","transport":"mutual tls"}
```

`plaintext` is the third, and it is what a laptop and the test suites run in.
There is no plaintext fallback on the deployed port on purpose: one would be
reached by exactly the callers that could not complete the handshake.

Rotation is a rerun of `sync-secrets.sh`, which issues only what is missing —
delete a leaf's two secrets and rerun to replace it, then roll the tier that
holds it. Replacing the authority itself means replacing every leaf under it in
both tiers at once, which is why it is issued for ten years and the leaves for
two.

### Giving a phone a token

Minting is an admin call. An application's own server authenticates its user and
asks the control plane for a token saying what that user may do; a phone never
asks, because a phone that could mint its own token could mint any token.

The sample application has no server, so `deploy/tools/join.sh` is one:

```sh
deploy/tools/join.sh ada bench cn-I_WNMClehiVp5   # good for fifteen minutes
```

It creates the room if it is missing, since minting checks that the room exists,
and reaches the admin API through a port-forward because that API has no public
address. `MOQOM_MINT=1 Scripts/bench-two-phones.sh`, in moqom-swift, mints one
per phone and passes them as launch arguments.

Four things have to agree or the session connects and is refused every track:
the tenant, the username, the room, and the device. The last is the third
argument above, and it used to be a fixed string every phone shared —
`sample-device-key`, which `moqom-mint` defaulted to and which meant the field
carried nothing. It is now the phone's own key, printed under the username on its
join screen and typed in here. Getting it wrong is visible before the relay ever
sees it: the join screen compares the token's `cnf.jkt` against the key it holds
and says so.

That matters more than it did when the field was decorative. On a relay running
`MOQOM_RELAY_POSSESSION=required`, a token minted against one phone's thumbprint
cannot be used from another — the session has to sign for it — so a token pasted
into the wrong phone is refused rather than quietly publishing under a namespace
nobody is watching.

### Taking one away again

`go run ./cmd/moqom-ban -user ada -room bench` is the other half, and the one
worth running after any change to the link: a ban is the only admin call that
has to travel back down it. The control plane writes the ban, finds the relays
holding a session for that account, and pushes a directive; everything else in
the admin API is a write the client discovers on its next request. So the reply
answers a question about RelayLink rather than about moderation:

```
moqom-ban: banned: publisher is out of ban2 and cannot rejoin (entry …-3, enforced=true)
INFO moqom_relay::link: directive carried out directive=…-1 action="ban" tenant=moqom room=ban2 sessions=1
INFO moqom_relay::link: directive carried out directive=…-2 action="eject" tenant=moqom room=ban2 sessions=0
```

`enforced=false` with a session plainly live means the link is down and the ban
is waiting for the relay's next snapshot, which is the degradation the grace
period exists to bound.

Leave `-on-behalf-of` unset for an action the service account takes on its own.
Naming a moderator asks the control plane to check that they hold moderation in
that room, and an operator at a terminal usually holds nothing in it.

### Relay tiers: an edge's link to its parent

RelayLink is a relay's link to the control plane. A relay's link to a parent relay is a
separate thing: an ordinary MoQ session, opened per room, on a credential the edge mints for
itself. `relay_parent` (empty by default, and empty in dev) turns every relay in an environment
into an edge of the relay at that `HOST:PORT`. The relay then sets `MOQOM_RELAY_UPSTREAM`, plus
`MOQOM_RELAY_LINK_URL` pointing at the control plane's `/relay/link` on the join port.

For each room it is asked for and doesn't publish, the edge does the following:

- It POSTs `{tenant, room, key}` with its relay account's credential. `key` is the public half
  of a P-256 key that the edge generates at boot and keeps in memory.
- It gets back a five-minute `rly` token for that room alone, bound to the key.
- It opens a session to the parent, proving the key with a signature over the token and the
  parent's certificate.
- It renews the token over that session once a third of its life is left.

The parent refuses the session if the proof is missing, reused, or made for another relay's
certificate. A link can read only its room and announce nothing. When minting is refused, the
room is refused at the edge for 30 seconds, and a link that is already up ends when its
credential lapses.

Before `relay_parent` is worth setting:

- The parent's certificate must verify from the edge. `MOQOM_RELAY_UPSTREAM_CA` is not wired
  here.
- The parent must not be one of the relays it applies to. The hop limit (3 by default,
  `MOQOM_RELAY_MAX_HOP_LIMIT`) ends a cycle, but only after it has been walked.
- The relay account in `MOQOM_CONTROL_ACCOUNTS` must be `is_relay`.

The design is in `docs/USAGE-METERING.md` under "Relay-to-relay links".

## The management overlay

Everything that is management rather than media can also travel over a
self-hosted [OpenZiti](https://openziti.io) network: the control plane's gRPC,
each relay's link to it, each relay's admin port, Postgres for operators, and
the link a customer's BYOC relay dials. A service on the overlay has no
listening port anyone can scan; to reach it a caller needs an enrolled identity
that a policy allows to dial it. Media is not on it and is not going to be —
[docs/OPENZITI-AND-P2P.md](../docs/OPENZITI-AND-P2P.md) says why — and viewers
never hold an identity.

`modules/ziti` is one `e2-small` VM in the control plane's region running the
controller and a public edge router, with the controller's database and PKI on
a persistent disk that outlives the VM. Its client API (1280) and router
(3022) are public, because a BYOC relay has to reach them from anywhere; its
management API is on a port of its own (1281) open only to the VPC, the
cluster's pods and IAP. The VM generates the admin password itself and keeps
it, and the PKI, in Secret Manager. The same VM runs a Cloud SQL proxy (IAM
authentication, no password) so Postgres can be an overlay service.

`tools/ziti-provision.sh` makes the services, policies and standing identities
by name, so rerunning it converges. It is a script rather than a Terraform
provider so that no enrolment token or private key reaches state. Role
attributes are the contract with the rest of the tree:

| Role | Who | May dial | Binds |
|---|---|---|---|
| `#moqom-control` | the control plane | — | `PREFIX-control`, `PREFIX-api` |
| `#moqom-relay` | a managed relay, identity minted at boot | `PREFIX-control` | `PREFIX-relay-admin`, `PREFIX-relay-ssh` |
| `#moqom-byoc-relay` | a customer's relay | `PREFIX-api` | — |
| `#moqom-api-client` | an enterprise backend | `PREFIX-api` | — |
| `#moqom-operator` | a person | everything above, and `PREFIX-postgres` | — |

There are two control services because they authenticate differently.
`PREFIX-control` is the same mutual-TLS gRPC server as the internal port, so a
managed relay presents its link certificate and bearer credential over the
circuit as it would over the VPC. `PREFIX-api` is a second gRPC server with no
TLS of its own — the circuit is already encrypted end to end — where the
dialler's overlay identity is the credential: an identity the control plane
enrolled for a BYOC relay becomes that relay's service account, pinned to one
tenant, and anything else falls through to the usual bearer key.

### How a managed relay gets an identity

Nobody hands a relay an enrolment token. With `ziti_relay_enabled`, cloud-init
fetches the instance's identity token from the metadata server (audience
`moqom-dev-relay-enroll`, format `full`) and posts it, with the relay's bearer
credential, to `/relay/enroll` on the control plane's join port. The control
plane checks Google's signature, the project, the instance-name prefix and the
service account, deletes any identity of that instance's name and creates a
fresh one tagged with the instance ID, and returns a one-use token. The VM
enrols it and runs `ziti tunnel proxy` as `moqom-ziti.service`, which listens on
loopback for the two control services and hosts the relay's admin port and
sshd. Every five minutes the control plane lists the project's instances and
deletes any minted relay identity whose instance is gone; a listing that fails
or comes back partial deletes nothing.

The relay's full admin surface then binds to `127.0.0.1:8082` and is reached
only over the overlay, one terminator per relay addressed by name; the network
admin port keeps serving the health check's probes and nothing else.

### Flags, in the order they are meant to be turned on

| Variable | Effect |
|---|---|
| `ziti_control_enabled` | Control plane binds both services, mints relay identities, serves BYOC enrolments |
| `ziti_relay_enabled` | Relays enrol at boot and run the tunneler; admin surface moves to loopback |
| `relay_link_via_overlay` | Relays dial the control plane through their tunneler instead of the internal load balancer |
| `relay_link_overlay_only` | The control plane refuses a RelayLink that did not arrive over the overlay |

Each is additive and each is undone by setting it back. The last is the one
that makes the internal path dark for relays; turn it on only after every relay
in every region has logged `attached to the control plane` through
`127.0.0.1:18443`, because a relay that cannot reach the overlay then has no
link at all and falls back to the grace period.

### Operator access

`ziti-provision.sh` makes one identity per person in `MOQOM_ZITI_OPERATORS` and
puts its one-use enrolment token in Secret Manager as
`PREFIX-ziti-operator-NAME`, reissued when it nears expiry. Enrol it in Ziti
Desktop Edge (Add Identity, then the JWT) and the intercepts resolve:
`postgres.moqom-dev.ziti:5432` (log in with your IAM user and
`gcloud auth print-access-token` as the password), `control.moqom-dev.ziti:8443`
and `api.moqom-dev.ziti:8443`. Relays are `INSTANCE.relays.moqom-dev.ziti` on
8082 (admin) and 22 (sshd; OS Login still decides who gets a shell).

Without Desktop Edge, `go run ./cmd/moqom-overlay` forwards one loopback port to
one service with an identity file, and takes `-terminator` for a relay:

```
MOQOM_ZITI_IDENTITY=me.json go run ./cmd/moqom-overlay -service moqom-dev-relay-admin \
    -terminator moqom-dev-relay-us-west1-abcd.relays.moqom-dev.ziti -listen 127.0.0.1:18082
curl -s http://127.0.0.1:18082/metrics
```

### BYOC enrolments

`go run ./cmd/moqom-byoc -tenant T create -name "office relay"` prints a one-use
enrolment token (valid for 24 hours) for a customer's relay;
`list` and `revoke -id ID` are the rest. It dials `PREFIX-api` with
`MOQOM_ZITI_IDENTITY` and authenticates with `MOQOM_BYOC_CREDENTIAL`, a platform
service account or the tenant's owner. Revoking marks the enrolment revoked in
Postgres, deletes the identity, and cuts that relay's live link on the replica
holding it; other replicas recheck every 30 seconds. The customer side is in the
[self-hosting guide](https://moqom.dev/guides/self-hosting/).

## Proving the store against the database it will use

`deploy/tools/conformance.sh` runs the store's contract — the suite that makes
the in-memory and Postgres implementations interchangeable — as a Job in the
cluster, against the deployed Cloud SQL instance:

```
==> running the contract in the cluster
    PASS
==> the store keeps its contract against moqom-dev-pg
```

In the cluster because the instance has no public address, and against that
instance rather than a container because a suite green on Postgres 17 in Docker
and red on the managed 16 with a different collation has told nobody anything.
`go test ./...` skips it for want of a database, which is the right default for
a laptop and the reason this exists.

It runs in a `conformance` database of its own, created and dropped around the
run. The suite makes a schema per case and drops it, so pointing it at `moqom`
would probably be harmless — and "probably harmless" is not a reason to run DDL
beside live rows. The Job is left behind when the suite fails, because whatever
it was in the middle of is the only evidence of why.

Against any other Postgres, the suite is an environment variable and a test run:

```sh
MOQOM_TEST_POSTGRES='postgres://…' go -C go test ./internal/store/... -count=1
```

There used to be a `tools/postgres-test.sh` that did this through the Cloud SQL
proxy against a hand-made instance. Both are gone: the instance was a scratch
one from bring-up, and a proxy on a laptop cannot reach the private address the
deployed instance now has.

## The database has no passwords

The control plane authenticates to Postgres with IAM: `cloudsql.iam_authentication`
is on, each Cloud Run service registers itself as a `CLOUD_IAM_SERVICE_ACCOUNT`
user, and the access token is the password. There is no `random_password`
anywhere in this tree, so there is no credential in state to leak or rotate.

One trap. Cloud SQL derives the Postgres role name by stripping
`.gserviceaccount.com` from the service account email, and connecting as the
full email fails with "password authentication failed" for an account that has
no password. The `control-plane` module injects the correct value as `PGUSER`;
the `data` module exports the same mapping as `iam_usernames` for anything else
that needs it.

## BYO-bucket recording

The product requirement is that a customer can have MOQOM write recordings into
their own bucket. Which mechanism that needs depends on whose cloud the bucket
is in, and only one of the two is Workload Identity Federation.

**Customer bucket on GCS.** Not federation at all. MOQOM already runs as a
Google identity, so the customer grants the `recording_writer` service account
`roles/storage.objectCreator` on their bucket:

```sh
gsutil iam ch \
  serviceAccount:$(terraform output -raw recording_writer_service_account):objectCreator \
  gs://their-bucket
```

**Customer bucket on S3.** The federation is configured on the AWS side, not
ours: an IAM OIDC provider trusting `https://accounts.google.com`, and a role
whose trust policy is conditioned on the `sub` claim — which is the service
account's *unique numeric id*, not its email, because the email is mutable in a
way the id is not. No Terraform in our project can create that role. We publish
the id as `recording_writer_unique_id` and the customer builds the trust policy
from it.

Workload Identity Federation in `modules/federation` covers the other direction:
external identities acting as MOQOM service accounts. That is GitHub Actions
pushing images and rolling revisions, and BYOC relays on EC2 or a customer's
Kubernetes cluster reaching MOQOM-managed resources.

The one thing to get right there is the provider's attribute condition. A GitHub
provider without `assertion.repository_owner == "..."` trusts every token GitHub
issues, from every repository on the platform, and is one careless service
account binding away from being a public credential. The module makes `owner`
mandatory for that reason. The per-tenant impersonation bindings for the BYOC
account are deliberately *not* created here: which external principal may act as
it is a per-customer decision with a per-customer blast radius, and encoding it
in the shared environment root turns every onboarding into a change to
production infrastructure.

## Hosted recording in dev

`envs/dev` has a `module "recorder"` block (`modules/recorder`) and three
variables, all off by default:

- `recorder_image` is the recorder image, pinned by digest. While it is null,
  the module creates the identities, grants and secret containers, and runs no
  workers.
- `recorder_thumbprint` is the JWK thumbprint of the recorder's key. While it is
  null, the control plane has no recording environment. The control plane
  refuses to start with `MOQOM_RECORDING_BUCKET` and no thumbprint, so the two
  go on together.
- `recorder_replicas` is the number of rooms that can be recorded at once.

Once the thumbprint is set, the control plane gets these:

- `MOQOM_RECORDING_BUCKET`
- `MOQOM_RECORDER_THUMBPRINT`
- `MOQOM_RECORDING_SIGNER_ACCOUNT`, from the module's `signer_account`
- `MOQOM_RECORDING_BUCKET_LOCATION` and `MOQOM_RECORDING_LIFECYCLE`, which come
  from the same values the bucket's lifecycle rules use. Storage is billed at
  the price of the storage class each object is actually in.

`tools/recorder-secrets.sh` provisions the `is_recorder` account. It also does
the following:

- generates the recorder's key;
- issues a link client certificate;
- adds `{"name": "recorders", "is_recorder": true, …}` to the accounts file;
- writes the `moqom-dev-recorder` Kubernetes secret that the workers mount;
- prints the thumbprint.

It is idempotent, like `sync-secrets.sh`, and none of these values pass through
Terraform state.

The commands, from the workspace root (`moqom-cloud`), in order. Review each
plan before applying it.

```sh
# 1. The Secret Manager containers, the two service accounts and their grants.
#    No workers yet, and recording still off in the control plane.
cd moqom/deploy/envs/dev
terraform init -backend-config=backend.hcl
terraform plan -out=recorder.tfplan
terraform apply recorder.tfplan
cd ../../../..

# 2. The key, the account, the certificate and the workers' secret. Then
#    sync-secrets.sh again, so the control plane mounts the new accounts file.
moqom/deploy/tools/recorder-secrets.sh        # prints recorder_thumbprint = "…"
moqom/deploy/tools/sync-secrets.sh

# 3. The image, built for the nodes' architecture.
cd moqom-engine
docker buildx build --platform linux/amd64 -f Dockerfile.recorder \
  --build-context security=../moqom-security \
  -t us-docker.pkg.dev/moqom-503700/moqom-dev/moqom-recorder:dev --push .
gcloud artifacts docker images describe \
  us-docker.pkg.dev/moqom-503700/moqom-dev/moqom-recorder:dev \
  --project=moqom-503700 --format='value(image_summary.digest)'
cd ..

# 4. In envs/dev/terraform.tfvars:
#      recorder_image      = "us-docker.pkg.dev/moqom-503700/moqom-dev/moqom-recorder@sha256:…"
#      recorder_thumbprint = "…"   # from step 2
cd moqom/deploy/envs/dev
terraform plan -out=recorder.tfplan
terraform apply recorder.tfplan

# 5. Check: one worker running, and the control plane saying recording is on.
kubectl -n moqom get pods -l app.kubernetes.io/component=recorder
kubectl -n moqom logs deploy/moqom-dev-control | grep -E 'hosted recording on|recording storage billed'
```

The workers reach the control plane by its in-cluster service name, which the
link server certificate carries. They reach the relay in `var.region` on its
public address, pinned to the certificate the phones pin. If that relay's
certificate is reissued, run `recorder-secrets.sh` again: it rewrites the secret
from the current `relay-tls-cert`, and the pods pick the new certificate up when
they restart.

## Running the relay tier alone

The relay module takes plain strings — a network, a subnet, an image, a registry
host — and creates its own service account, load balancer, and IAM. It does not
reference the control plane, the database, or the environment root. A self-hoster
who wants only the media path needs:

```hcl
module "relay" {
  source = "github.com/moqom/moqom//deploy/modules/relay"

  project_id   = "acme-media"
  region       = "europe-west4"
  name_prefix  = "acme"
  network      = google_compute_network.theirs.id
  subnetwork   = google_compute_subnetwork.theirs.id
  network_tags = ["moqom-relay"]

  image         = "europe-docker.pkg.dev/acme-media/relay/moqom-relay@sha256:..."
  registry_host = "europe-docker.pkg.dev"
  machine_type  = "n2d-standard-4"
  min_replicas  = 3
  max_replicas  = 12
}
```

They also need the firewall rules — UDP/443 and UDP/3478 (STUN) from anywhere
to the relay tag, TCP on the health port from the prober ranges, and egress for NTP and for the
control plane's port. Those live in `modules/network` and are readable in
isolation; taking the whole network module is easier than reproducing them, and
it will not conflict with an existing VPC as long as `name_prefix` differs.

### Four ways a relay looks healthy and serves nothing

Every one of these produced a relay that was running, answering its own probes
on the machine, and useless. They are written down because none of them appears
in a log the relay itself writes.

**The prober range is not the one you think.** Most load balancers probe from
`35.191.0.0/16` and `130.211.0.0/22`. The external passthrough Network Load
Balancer — the only kind that forwards UDP, so the only kind that can front this
tier — probes from `35.191.0.0/16` and `209.85.152.0/22` and `209.85.204.0/22`
instead. Allowing the usual pair gives every backend `UNHEALTHY` with "Timeout
waiting for connect" against a process that answers the same request locally.

**Container-Optimized OS has its own firewall.** It boots `-P INPUT DROP` and
permits loopback, ICMP, established flows and SSH. A VPC rule is necessary and
not sufficient: the packet is allowed onto the wire and dropped by the machine it
was addressed to. The relay unit opens UDP/443, UDP/3478 and the health port with
`iptables` on every start, rather than once at first boot, so a reboot cannot
lose them.

**443 is a privileged port and the container is not privileged.** The relay runs
as a non-root user in a container sharing the host's network namespace, so
binding 443 fails with `EACCES` and the container exits immediately with status
1. `net.ipv4.ip_unprivileged_port_start = 0` is the fix; running as root to bind
one socket is not.

**Egress is deny by default, and that includes NTP.** The chrony configuration
points at Cloudflare so relays and clients smear leap seconds the same way.
Blocking UDP/123 does not fall back to something reasonable — the clock
free-runs from boot and every timecode the relay stamps is wrong by the drift.

Two things make this diagnosable at all. Container logs go straight to Cloud
Logging with the `gcplogs` driver, because a machine with no external address
keeps its local logs to itself and a crash on startup takes its only explanation
with it. And `tools/quic-probe.py <address>` forces a QUIC version negotiation,
which is the only check here that actually observes the media socket — Google
Cloud has no UDP health check, so nothing else does.

`artifact_registry_repository`, `recording_bucket`, and the TLS secret variables
are all nullable, so a relay that pulls from elsewhere and records nowhere is a
supported configuration.

The relay is proprietary and ships as a binary. Self-hosting for your own
applications and users is permitted under a BYOC licence; see
[LICENSING.md](../LICENSING.md).

## Rollouts and draining

A MIG rolling update surges before it removes, and `max_unavailable_fixed = 0`
means capacity never dips below the current size — the instances being replaced
are holding sessions that cannot be re-established elsewhere without the viewer
noticing.

The part Terraform does not express is the drain itself. GCE allows roughly 90
seconds between a delete call and the machine going away, and a live stream is
longer than that. The sequence that actually works:

1. The relay fails `/healthz`. `connection_persistence_on_unhealthy_backends`
   is `ALWAYS_PERSIST`, so no new flows arrive and existing ones keep working —
   without that setting a drain becomes a mass disconnect.
2. Existing sessions finish, or are told to reconnect with `GOAWAY`.
3. The instance is deleted.

Steps 1 and 2 need an external controller for anything longer than the shutdown
window. Until one exists, roll during a trough and watch session counts.

Scaling in is throttled hard for the same reason: `scale_in_control` allows one
instance every thirty minutes in prod, because removing an instance ends every
session on it and a lull between two live events is not a trough.

## CI

GitHub Actions authenticates through the federation pool with no downloaded key:

```yaml
- uses: google-github-actions/auth@v2
  with:
    workload_identity_provider: ${{ vars.WIF_PROVIDER }}   # tf output github_workload_identity_provider
    service_account: ${{ vars.WIF_SERVICE_ACCOUNT }}       # tf output ci_service_account
```

The deployer holds `run.developer` and `compute.instanceAdmin.v1` at the project
level, and nothing else. It cannot create service accounts, edit IAM, or reach
the database, so the worst a compromised workflow can do is deploy a bad image —
recoverable — rather than mint itself an identity, which is not. In prod it is
additionally restricted to `refs/heads/main`.

`roles/iam.serviceAccountUser` is granted on individual service accounts, never
on the project, and `deployer_roles` has a validation rule that refuses it.
Deploying the relay does require acting as the relay identity, because an
instance template names the account its VMs run as. But that role at project
scope means acting as *any* account in the project, and combined with
`compute.instanceAdmin.v1` it is a full escalation: boot a VM as the most
privileged account available, read its token from the metadata server, and CI
has become that account. Scoped per account — each relay region plus the two
Cloud Run services — the ceiling is what those identities can already do, which
is very little by design.

`compute.instanceAdmin.v1` is still the broadest grant in the tree. No narrower
predefined role can roll a managed instance group; closing it means writing a
custom role, and that is on the list.

The lock files are committed. They pin exact provider builds and checksums so CI
and a laptop resolve to the same code and a compromised registry release cannot
be substituted silently. They are locked for `linux_amd64`, `linux_arm64`,
`darwin_amd64`, and `darwin_arm64`; adding a platform means
`terraform providers lock -platform=...`, and a lock file missing your CI's
platform fails in a way that reads like a network error.

## Websites and project subdomains

The static sites are `envs/web`: one bucket (`moqom-sites`) with a prefix per
site directory under `sites/`, one CDN backend bucket, and one global HTTPS load
balancer at `34.107.228.246` (`terraform output address`) whose URL map rewrites
each hostname onto its prefix. Publishing is `tools/publish-sites.sh <site>`; the
`publishing-sites` skill covers caching and verification.

A lab project's docs at `<project>.elliottwave.studio` is one line in
`envs/web/main.tf`, hostname to site directory:

```hcl
project_hosts = {
  "six10nine.elliottwave.studio" = "six10nine"
}
```

Each project hostname gets a Google-managed certificate of its own, attached to
the HTTPS proxy beside the shared one. The shared certificate
(`moqom-sites-bb025dc3`, the eight main hostnames) is never edited for a project:
changing a managed certificate's domains replaces it, and the replacement waits
on the new name's DNS. A proxy takes 15 certificates, so there is room for 13
project hosts before this needs Certificate Manager.

### Adding a project host

1. Add the line above and the site under `sites/<project>`.
2. Plan, from `envs/web`:

   ```sh
   terraform init -backend-config=backend.hcl
   terraform plan -out=project.tfplan
   ```

   The plan should create one `google_compute_managed_ssl_certificate.projects`
   and update `google_compute_url_map.sites` and
   `google_compute_target_https_proxy.sites` in place. If it replaces or destroys
   `google_compute_managed_ssl_certificate.sites` or `.apis`, stop: the live hosts
   would go without HTTPS while the replacement issues.
3. `terraform apply project.tfplan`. Nothing changes for the live hosts; the new
   certificate sits in `PROVISIONING`.
4. Publish the site so its first visitor finds it: `tools/publish-sites.sh <project>`.
   Named sites only; running the script with no arguments still publishes the
   four main sites and nothing else.
5. Add the DNS record at Hover, in the `elliottwave.studio` zone:

   | Type | Host | Value | TTL |
   | --- | --- | --- | --- |
   | A | `six10nine` | `34.107.228.246` | default |

   No AAAA: the address is IPv4 only. Remove any existing record for the same host
   first.
6. Wait for the certificate to turn `ACTIVE`, usually 15 to 60 minutes after the
   record resolves:

   ```sh
   gcloud compute ssl-certificates list --global --project moqom-503700 \
     --format="table(name,managed.status,managed.domainStatus)"
   ```

   Until then the host has no valid certificate. `elliottwave.studio` sends HSTS
   with `includeSubDomains`, so a browser that has visited it refuses the new
   host outright rather than offering a warning; that clears once the
   certificate is active. If a domain sits in `FAILED_NOT_VISIBLE` after DNS was
   fixed, bump `certificate_epoch`. That re-issues the shared certificate as
   well (new before old, with `create_before_destroy`), so it is a last resort.
7. Verify: `curl -sI https://six10nine.elliottwave.studio/ | rg -i '^(HTTP|cache-control)'`.

The hourly Cloud Build publish (`tools/cloudbuild-sites.yaml`) rebuilds only the
sites in its `_SITES`; add a project there if it shows build-time figures.

## 3D Viewer licence server

`envs/viewer-licensing` (module `modules/viewer-licensing`): the Cloud Run
service from `services/licensing` in the viewer repo, its Firestore database
(`viewer-licensing`, us-west1, with TTL on `events.expires` and
`requests.expires`), a service account limited to that database, secret
containers (`viewer-licensing-signing-key`, `-stripe-secret-key`,
`-stripe-webhook-secret`, and `-smtp-password` when SMTP is configured), an
Artifact Registry repository, and a serverless NEG behind a CDN backend
service. Not applied. The order is at the top of its `main.tf`.

It is served at `licensing.elliottwave.studio/v1/*` by the sites balancer:
`envs/web` adds the host to `apis` once `viewer_licensing_backend` is set, with
`own_certificate = true` so the plan creates one new certificate and leaves
`.apis` (api.moqom.cloud) alone. If a plan replaces `.apis`, stop. DNS: an A
record `licensing` -> `34.107.228.246` in the `elliottwave.studio` zone.

Request logging is off for this backend and excluded for the Cloud Run
service, because token requests carry licence keys in the query string.

## Cloudflare later

Nothing here is Cloudflare-shaped yet and nothing here prevents it. The pieces
that would move are the edge address and the DNS record — `modules/relay`'s
forwarding rule and `modules/dns-steering` — and the pieces that would not are
the instance template, the autoscaler, the control plane, and the data tier. If
Spectrum fronts the relays for true anycast, the relay module keeps its NLB as
the origin and the steering module is replaced rather than edited.

## What this does not do

- **No Cloud Armor or WAF.** Exactly one port of the control plane faces the
  internet — the join endpoint, on its own service so the admin API stays on an
  address that only exists inside the VPC — and nothing in front of it filters.
  It has to be reachable by anonymous callers: MOQOM's clients are phones and
  browsers carrying a device credential, and they have no Google identity to
  present. Authorisation is that credential plus, from there on, CAT and DPoP
  inside the application. What is missing is rate limiting and geo blocking,
  which need a global external Application Load Balancer in front; the join
  handler's own per-credential limiter is what stands in, and it bounds one
  credential rather than one source. The scanners in its logs are constant and
  harmless, and they are also the argument for the balancer.
- **No organisation policies, no VPC Service Controls, no CMEK key ring.** The
  `data` module accepts a `kms_key_name` but nothing creates one; key management
  belongs in an admin project with a different change cadence.
- **No synthetic media monitoring.** The uptime check covers the control plane
  only. Google's probers speak HTTP over TCP and cannot open a QUIC session, so
  verifying that a relay actually serves media needs a synthetic MoQ client,
  which belongs in the application repository.
- **No multi-region control plane.** A token mint is a millisecond operation and
  moving it closer to the viewer buys less than a multi-region Postgres costs.
- **Prod is not applied.** This line used to say nothing in the tree was, which
  stopped being true of dev some time ago and is worth separating: `envs/dev` is
  the live `moqom-503700` — cluster, database, both relay groups, both load
  balancers — and has been rolled repeatedly, so quota, organisation policy and
  IAM propagation are established facts there. `envs/prod` has never been
  initialised; it has no `backend.hcl` and no state, and it validates, which is a
  claim about syntax and schema and nothing else.

- **Applies to dev fight the cluster over annotations.** Autopilot writes the
  requests and limits it decided on back onto the control plane's Deployment, so
  every plan offers to delete them, and accepting restarts the pod for no reason.
  The two services are fixed — `ignore_changes` on the controller's own record of
  the backend it built, which matters because that record is attached to the
  address every relay dials — but the Deployment's `resources` block is not, and
  ignoring it wholesale would mean Terraform no longer states what the control
  plane asks for. Until that is settled, an apply meant for the relay tier should
  say `-target=module.relay`, which is now precise: with the services ignored,
  nothing in the control tier is dragged in behind it.
