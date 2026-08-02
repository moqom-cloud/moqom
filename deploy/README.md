# deploy

Terraform for MOQOM on GCP. Two tiers with genuinely different shapes: a relay
tier of VMs behind a UDP load balancer, and a control plane on Cloud Run.

```
bootstrap/                 State bucket. Run once, by hand, before anything else.
modules/
  project-services/        API enablement, and the wait that makes it usable.
  network/                 VPC, subnets, firewall, Cloud NAT, private services access.
  artifact-registry/       Container images and their retention.
  relay/                   Instance template, regional MIG, autoscaler, UDP NLB.
  control-plane/           One Cloud Run service, instantiated per component.
  data/                    Cloud SQL Postgres, recording bucket, secret containers.
  federation/              Workload identity for CI and for non-GCP relays.
  observability/           Log routing, log-based metrics, alerts, uptime check.
  dns-steering/            Geolocation routing across relay regions.
envs/
  dev/                     One region, small, no alerts, no deletion protection.
  prod/                    Three regions, HA database, digests pinned, alerts on.
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

`terraform apply` finishes with three empty secrets. This is deliberate: a
`google_secret_manager_secret_version` stores its payload in plaintext in
Terraform state, and state has a wider audience than any of these keys should.

```sh
gcloud secrets versions add moqom-prod-cat-signing-key --data-file=-
gcloud secrets versions add moqom-prod-relay-tls-cert --data-file=cert.pem
gcloud secrets versions add moqom-prod-relay-tls-key  --data-file=key.pem
```

The relay will not serve TLS and the control plane will not mint tokens until
these have versions. Certificate renewal is out of scope here on purpose: a
fleet of N relays needs one certificate distributed to N machines, which is a
distribution problem rather than an issuance problem, and Secret Manager is the
distribution mechanism. Whatever runs ACME writes here.

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

They also need the firewall rules — UDP/443 from anywhere to the relay tag, TCP
on the health port from the prober ranges, and egress for NTP and for the
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
was addressed to. The relay unit opens UDP/443 and the health port with
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

The relay is BUSL-1.1. Self-hosting for your own applications and users is
permitted, including commercially; see [LICENSING.md](../LICENSING.md).

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

## Cloudflare later

Nothing here is Cloudflare-shaped yet and nothing here prevents it. The pieces
that would move are the edge address and the DNS record — `modules/relay`'s
forwarding rule and `modules/dns-steering` — and the pieces that would not are
the instance template, the autoscaler, the control plane, and the data tier. If
Spectrum fronts the relays for true anycast, the relay module keeps its NLB as
the origin and the steering module is replaced rather than edited.

## What this does not do

- **No Cloud Armor or WAF.** The control plane is public Cloud Run with
  `allUsers` invoker, because MOQOM's clients are browsers and mobile apps
  carrying CAT tokens and have no Google identity to present. Authorisation is
  CAT plus DPoP inside the application. Rate limiting and geo blocking need a
  global external Application Load Balancer in front; `public_ingress = false`
  is the switch that starts that migration.
- **No organisation policies, no VPC Service Controls, no CMEK key ring.** The
  `data` module accepts a `kms_key_name` but nothing creates one; key management
  belongs in an admin project with a different change cadence.
- **No synthetic media monitoring.** The uptime check covers the control plane
  only. Google's probers speak HTTP over TCP and cannot open a QUIC session, so
  verifying that a relay actually serves media needs a synthetic MoQ client,
  which belongs in the application repository.
- **No multi-region control plane.** A token mint is a millisecond operation and
  moving it closer to the viewer buys less than a multi-region Postgres costs.
- **Nothing is applied.** No plan in this tree has been run against a real
  project; it validates, which is a claim about syntax and schema and not about
  quota, organisation policy, or IAM propagation.
