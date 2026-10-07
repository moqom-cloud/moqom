locals {
  env         = "dev"
  name_prefix = "moqom-dev"

  labels = {
    environment = local.env
    managed-by  = "terraform"
    system      = "moqom"
  }

  # Two regions, which is a deliberate departure from "dev is single-region".
  # The latency work compares a relay near the phones against one a region away,
  # and a comparison you cannot reproduce in the environment you develop in is a
  # measurement you stop trusting. It also means the control plane is reached
  # across regions from the start, which is where an internal load balancer's
  # regional scope would otherwise be discovered late.
  regions = {
    (var.region) = {
      relay_cidr      = "10.20.0.0/20"
      serverless_cidr = "10.20.16.0/26"
    }
    (var.second_region) = {
      relay_cidr      = "10.24.0.0/20"
      serverless_cidr = "10.24.16.0/26"
    }
  }

  # The cluster's ranges, out of the same 10.20/10.21 space as everything else
  # here. Pods get a /16 because GKE hands each node a /24 out of it, so the
  # limit this sets is on node count rather than on pods.
  # One number, three places: what the control plane binds, what its load
  # balancer publishes, and what the relay tier's egress rule opens. Two of
  # those disagreeing produces a relay that cannot reach a control plane that is
  # working perfectly.
  control_grpc_port = 8443

  # Where a phone asks for a token. A port of its own because it is exposed
  # differently from everything else here -- publicly -- and a port shared with
  # the admin API would be an admin API on the internet.
  control_join_port = 8444

  cluster_node_cidr    = "10.20.32.0/22"
  cluster_pod_cidr     = "10.21.0.0/16"
  cluster_service_cidr = "10.22.0.0/20"

  control_host = var.dns_domain == null ? "" : "api.${trimsuffix(var.dns_domain, ".")}"
  edge_host    = var.dns_domain == null ? "" : "edge.${var.dns_domain}"

  secrets = {
    "${local.name_prefix}-cat-signing-key" = []
    "${local.name_prefix}-relay-tls-cert"  = []
    "${local.name_prefix}-relay-tls-key"   = []
    # The relay's bearer credential for RelayLink. It is the same kind of thing
    # as a signing key -- a string that grants access -- so it is held the same
    # way, in a container Terraform creates and never reads.
    "${local.name_prefix}-relay-credential" = []
    # The service account file the control plane authenticates backends
    # against. A file rather than a variable because it is a JSON document, and
    # because a process's environment is readable by more things than its files.
    "${local.name_prefix}-control-accounts" = []
    # The bearer token an operator's own calls to the admin API carry. Held in
    # the same place as the rest so that a laptop reads it rather than keeps it.
    "${local.name_prefix}-admin-credential" = []

    # The credential a phone holds to ask the join endpoint for a token. The only
    # secret here that is meant to leave the deployment, which is why the account
    # behind it can do exactly one thing: mint participant tokens in this tenant,
    # with no moderation and no ownership, whatever the request asks for.
    #
    # Long-lived, unlike the tokens it buys. That is the trade the endpoint makes:
    # a credential that lasts means a phone can be set up once, and it means a
    # phone that is lost stays able to join until this is rotated. Rotating it is
    # a new version here and a rerun of sync-secrets.sh.
    "${local.name_prefix}-device-credential" = []

    # RelayLink's mutual TLS: a private authority, and a certificate for each of
    # the three things that speak on that port.
    #
    # Private because the endpoint is an internal load balancer and no public
    # authority issues for an RFC1918 address, and because pinning one authority
    # is stronger than trusting every root a machine ships with. What it buys is
    # the direction the bearer credential never covered — a relay knowing that
    # the peer sending it verification keys, room state and moderation is the
    # control plane, rather than whatever is currently answering to its address.
    #
    # The authority's private key is here too, which is the part to be careful
    # about: whoever holds it can mint a certificate this deployment trusts.
    # tools/sync-secrets.sh is the only thing that reads it, and only to issue a
    # leaf that is missing.
    "${local.name_prefix}-link-ca"          = []
    "${local.name_prefix}-link-ca-key"      = []
    "${local.name_prefix}-link-server-cert" = []
    "${local.name_prefix}-link-server-key"  = []
    "${local.name_prefix}-link-relay-cert"  = []
    "${local.name_prefix}-link-relay-key"   = []
    "${local.name_prefix}-link-admin-cert"  = []
    "${local.name_prefix}-link-admin-key"   = []
    # The recorder workers' client certificate, issued by
    # tools/recorder-secrets.sh. Their key and credential are the recorder
    # module's own containers.
    "${local.name_prefix}-link-recorder-cert" = []
    "${local.name_prefix}-link-recorder-key"  = []

    # Stripe, pasted in from the dashboard; see tools/sync-secrets.sh. A
    # restricted key (rk_) with customers, checkout sessions, payment intents
    # and billing portal write is enough, and is what should be stored here.
    "${local.name_prefix}-stripe-secret-key"     = []
    "${local.name_prefix}-stripe-webhook-secret" = []

    # The HMAC pepper API key hashes are keyed with: base64, 32 bytes or more,
    # one per line, newest first. Created empty, and the control plane keeps
    # bare SHA-256 hashes until it has a version. Rotating is a new version
    # with the new pepper on the first line and the old one below it; a pepper
    # dropped while keys are still hashed under it stops those keys.
    "${local.name_prefix}-api-key-pepper" = []

    # The SMTP submission password for spend alerts and leaked-key notices.
    # Only read when var.billing_smtp_host is set.
    "${local.name_prefix}-smtp-password" = []

    # The relay fleet's direct-ticket keys: a JWK set of Ed25519 keys, the
    # signing key first. One set for every region, because a participant
    # verifies a peer's ticket with keys from its own relay.
    "${local.name_prefix}-relay-ticket-keys" = []

    # The OpenZiti controller's default admin password and a backup of its
    # certificate authorities. Both are written by the overlay VM itself on
    # first boot (modules/ziti grants it add on these two only), so neither
    # passes through Terraform or a laptop.
    "${local.name_prefix}-ziti-admin-password" = []
    "${local.name_prefix}-ziti-pki"            = []

    # The control plane's own overlay identity, enrolled by
    # tools/ziti-provision.sh and mounted by sync-secrets.sh. It binds the
    # control services and, being an overlay admin, mints the relays' and BYOC
    # relays' identities.
    "${local.name_prefix}-ziti-control-identity" = []

    # One-time enrolment tokens for operators' Ziti Desktop Edge, one per
    # person. Spent on first use; reissued by rerunning the provision script.
    "${local.name_prefix}-ziti-operator-elliott" = []
  }

  # Billing's settings, merged into the control plane's environment only when
  # var.billing_enabled is set. Off, nothing about the deployment changes.
  billing_env = var.billing_enabled ? {
    MOQOM_BILLING               = "on"
    MOQOM_BILLING_PUBLIC_URL    = var.billing_public_url
    MOQOM_BILLING_CORS_ORIGINS  = join(",", var.billing_cors_origins)
    MOQOM_BILLING_EXEMPT        = join(",", var.billing_exempt_tenants)
    MOQOM_STRIPE_SECRET_KEY     = "/etc/moqom/stripe-secret-key"
    MOQOM_STRIPE_WEBHOOK_SECRET = "/etc/moqom/stripe-webhook-secret"
    # Absent files are read as unset, so both work before their secrets have
    # versions: no pepper, and email logged instead of sent.
    MOQOM_KEY_PEPPER          = "/etc/moqom/api-key-pepper"
    MOQOM_SMTP_HOST           = var.billing_smtp_host
    MOQOM_SMTP_PORT           = var.billing_smtp_port
    MOQOM_SMTP_USERNAME       = var.billing_smtp_username
    MOQOM_SMTP_PASSWORD       = "/etc/moqom/smtp-password"
    MOQOM_BILLING_EMAIL_FROM  = var.billing_email_from
    MOQOM_BILLING_CONSOLE_URL = "${trimsuffix(var.billing_public_url, "/")}/account"
  } : {}

  # The name the control plane's certificate carries, and the name every client
  # verifies it against. Not the address: the load balancer's address is a fact
  # about today's infrastructure, and a certificate that names it has to be
  # reissued whenever that changes.
  link_server_name = "moqom-control"

  # One namespace for the control plane's components. The director will join it
  # when it exists.
  namespace = "moqom"

  # The Kubernetes secret the control plane's pod reads, populated out of band
  # from the Secret Manager entries above by tools/sync-secrets.sh. Named here
  # so the deployment can reference it; its contents never enter Terraform
  # state, which is the rule the rest of this tree follows for secret material.
  control_secret = "${local.name_prefix}-control"
}

module "services" {
  source = "../../modules/project-services"

  project_id = var.project_id

  services = [
    "artifactregistry.googleapis.com",
    "cloudresourcemanager.googleapis.com",
    "compute.googleapis.com",
    "container.googleapis.com",
    "dns.googleapis.com",
    "iam.googleapis.com",
    "iamcredentials.googleapis.com",
    "logging.googleapis.com",
    "monitoring.googleapis.com",
    "secretmanager.googleapis.com",
    "servicenetworking.googleapis.com",
    "sqladmin.googleapis.com",
    "storage.googleapis.com",
    "sts.googleapis.com",
  ]
}

module "network" {
  source = "../../modules/network"

  project_id  = var.project_id
  name_prefix = local.name_prefix
  regions     = local.regions

  # On in dev only. This is where egress rules get tightened, and doing it
  # without connection logs is guesswork.
  enable_firewall_logging = true
  enable_iap_ssh          = true

  # The relays dial the control plane's internal load balancer, which lives on
  # the cluster's node range. Named here rather than taken from the gke module
  # so the firewall does not have to wait for a cluster to exist.
  control_plane_cidrs = [local.cluster_node_cidr]
  control_plane_port  = local.control_grpc_port

  # Out to the overlay's controller and router, on its public address: the
  # relays have none of their own and reach it through Cloud NAT.
  overlay_cidrs = ["${module.ziti.external_address}/32"]

  create_dns_zone = var.dns_domain != null
  dns_zone_name   = "${local.name_prefix}-edge"
  dns_domain      = var.dns_domain == null ? "" : var.dns_domain

  labels = local.labels

  depends_on = [module.services]
}

module "registry" {
  source = "../../modules/artifact-registry"

  project_id    = var.project_id
  location      = "us"
  repository_id = "moqom-dev"

  # Off in dev: rebuilding the same tag is the normal inner loop here, and
  # immutability turns that into a fight with the registry.
  immutable_tags       = false
  keep_tagged_versions = 5

  # Dev is where a cleanup policy gets proven before prod inherits it.
  cleanup_policy_dry_run = false

  labels = local.labels

  depends_on = [module.services]
}

# The cluster, and the Google identities its pods act as.
#
# The identities are created here rather than beside the workloads because the
# database needs them first: Cloud SQL turns a service account into a Postgres
# role, and the data module must know the email while the cluster is still
# building. Putting them with the workloads would make data depend on control
# and control depend on data.
module "gke" {
  source = "../../modules/gke"

  project_id  = var.project_id
  region      = var.region
  name_prefix = local.name_prefix
  network     = module.network.network_id

  node_cidr    = local.cluster_node_cidr
  pod_cidr     = local.cluster_pod_cidr
  service_cidr = local.cluster_service_cidr

  workloads = {
    control = {
      namespace       = local.namespace
      service_account = "moqom-control"
      roles = [
        # IAM database authentication: `client` permits the connection,
        # `instanceUser` turns the access token into a Postgres login. Neither
        # can be scoped below the project, which is why this project holds
        # nothing experimental.
        "roles/cloudsql.client",
        "roles/cloudsql.instanceUser",
        # Read-only, for one question: which relay VMs still exist, so the
        # overlay identities of deleted ones are deleted too.
        "roles/compute.viewer",
      ]
    }
  }

  artifact_registry_repository = module.registry.repository_ref

  deletion_protection = false
  labels              = local.labels

  depends_on = [module.services]
}

module "data" {
  source = "../../modules/data"

  project_id     = var.project_id
  region         = var.region
  name_prefix    = local.name_prefix
  network_id     = module.network.network_id
  psa_connection = module.network.psa_connection

  tier              = "db-custom-1-3840"
  availability_type = "ZONAL"
  disk_size_gb      = 10

  databases = ["moqom"]

  # The control plane logs in as itself. No password is generated, stored or
  # rotated, because there is not one.
  iam_service_accounts = [module.gke.workload_service_accounts["control"]]

  # Operators, who reach the instance through the overlay's Postgres service
  # and log in as themselves. A login, not a grant: what they may read is
  # granted inside the database.
  iam_users = var.database_operators

  backup_retention_days          = 3
  transaction_log_retention_days = 1
  deletion_protection            = false

  recording_bucket_name     = var.recording_bucket_name
  recording_bucket_location = var.region

  recording_lifecycle = local.recording_lifecycle

  recording_bucket_force_destroy = true

  secrets = local.secrets
  labels  = local.labels

  depends_on = [module.services]
}

module "control" {
  source = "../../modules/workload"

  name      = "${local.name_prefix}-control"
  namespace = local.namespace

  service_account        = module.gke.workload_identity_bindings["control"].service_account
  google_service_account = module.gke.workload_service_accounts["control"]

  # tools/sync-secrets.sh makes the namespace, because the secret this
  # deployment mounts has to exist before the deployment does and the secret has
  # to live somewhere. Terraform owning it as well would be a race on first
  # apply and an import for every new environment.
  create_namespace = false

  image     = var.control_image
  grpc_port = local.control_grpc_port

  # The only public address in this deployment's control tier. See the module's
  # `public_port` and the join endpoint's own file for why one exists and why
  # opening it does not open the admin API beside it.
  public_port = local.control_join_port

  # The process holds the certificate rather than a terminator in front of it,
  # which is what lets a relay authenticate the control plane itself instead of
  # authenticating whatever is terminating for it.
  transport_security = true

  # One replica, and this is a correctness bound rather than a sizing choice.
  # The registry of which relay is attached to which process is in memory, so a
  # moderation call handled by one replica cannot push a directive to a relay
  # attached to another -- it degrades to the relay's next snapshot, and reports
  # `enforced: false` in the meantime. A second replica is safe once that
  # registry is shared; until then this is the honest number.
  replicas = 1

  cpu    = "500m"
  memory = "512Mi"

  env = merge(local.billing_env, {
    MOQOM_ENV                 = local.env
    MOQOM_CONTROL_ADDR        = ":${local.control_grpc_port}"
    MOQOM_CONTROL_HEALTH_ADDR = ":8080"
    MOQOM_CONTROL_ISSUER      = local.control_host == "" ? "moqom.cloud" : local.control_host
    MOQOM_CONTROL_STORE       = "postgres"

    # Cloud SQL over IAM. The connector resolves the instance and supplies a
    # short-lived token as the credential; PGUSER is the service account email
    # with `.gserviceaccount.com` removed, which is the role name Cloud SQL
    # derives and the one thing that is easy to get wrong here.
    MOQOM_CONTROL_SQL_INSTANCE = module.data.instance_connection_name
    PGUSER                     = module.data.iam_usernames[module.gke.workload_service_accounts["control"]]
    PGDATABASE                 = "moqom"

    MOQOM_CONTROL_ACCOUNTS = "/etc/moqom/accounts.json"

    # Mutual TLS on the gRPC port, from the same mounted secret. Both ends of
    # RelayLink are authenticated: the client certificate says the peer is one
    # of this deployment's relays, and the bearer credential it also presents
    # says which one and what it may do.
    #
    # This is the port's only transport now, so the admin tools present a
    # certificate too. There is no plaintext fallback on purpose — one would be
    # reached by anything that could not do the handshake, which is exactly the
    # set of callers this exists to exclude.
    MOQOM_CONTROL_TLS_CERT  = "/etc/moqom/link-cert.pem"
    MOQOM_CONTROL_TLS_KEY   = "/etc/moqom/link-key.pem"
    MOQOM_CONTROL_CLIENT_CA = "/etc/moqom/link-ca.pem"

    # The device-facing route, on its own port and its own address. It serves the
    # same certificate as the port above and, pointedly, asks for none in return:
    # requiring a client certificate here would mean every phone needed one issued
    # before it could ask for a credential, which is the bootstrapping problem this
    # endpoint exists to solve. What stands in for it is the device account, whose
    # grants are the ceiling on everything minted against it.
    MOQOM_CONTROL_JOIN_ADDR = ":${local.control_join_port}"

    # On, because this is the environment two phones are tested from and typing a
    # room name into one of them should produce a room. The cost is that the
    # device credential can create rooms as well as join them; in an environment
    # with paying tenants in it, this stays off and rooms are made through the
    # admin API by something that knows why.
    MOQOM_CONTROL_JOIN_CREATES_ROOMS = "1"
  }, local.control_overlay_env, local.control_recording_env)

  secret_env = {
    MOQOM_CONTROL_SIGNING_KEYS = {
      secret = local.control_secret
      key    = "signing-keys"
    }
  }

  file_secrets = {
    accounts = {
      secret     = local.control_secret
      mount_path = "/etc/moqom"
    }
  }

  labels = merge(local.labels, { component = "control" })
}

# The billing API, reached by browsers at api.moqom.cloud through the sites'
# global load balancer (envs/web), which holds the publicly trusted certificate
# the join port's private one cannot be. The balancer reaches the pod through
# this service's standalone network endpoint groups, one per zone, named here so
# envs/web can find them. The balancer only routes /v1/ to it.
resource "kubernetes_service_v1" "control_api" {
  count = var.billing_enabled ? 1 : 0

  metadata {
    name      = "${local.name_prefix}-control-api"
    namespace = local.namespace
    labels    = merge(local.labels, { component = "control" })
    annotations = {
      "cloud.google.com/neg" = jsonencode({
        exposed_ports = { (tostring(local.control_join_port)) = { name = "${local.name_prefix}-control-api" } }
      })
    }
  }

  spec {
    type     = "ClusterIP"
    selector = { "app.kubernetes.io/name" = "${local.name_prefix}-control" }

    port {
      name        = "api"
      port        = local.control_join_port
      target_port = local.control_join_port
      protocol    = "TCP"
    }
  }

  lifecycle {
    ignore_changes = [metadata[0].annotations["cloud.google.com/neg-status"]]
  }
}

# Google's front ends and health checkers, to the pods on the API port. The
# billing handler trusts X-Forwarded-For only from these ranges.
resource "google_compute_firewall" "control_api" {
  count = var.billing_enabled ? 1 : 0

  name      = "${local.name_prefix}-control-api-proxies"
  project   = var.project_id
  network   = module.network.network_id
  direction = "INGRESS"
  priority  = 1000

  source_ranges      = ["35.191.0.0/16", "130.211.0.0/22"]
  destination_ranges = [local.cluster_pod_cidr]

  allow {
    protocol = "tcp"
    ports    = [tostring(local.control_join_port)]
  }
}

# The relays out to the control plane's public join port, through Cloud NAT:
# usage reports go to /v1/usage there, and a relay VM asks /relay/enroll for its
# overlay identity. The relay egress default is deny and the network module
# cannot name this address -- it is the public service's, which comes from the
# cluster the network module is an input to -- so the rule is here.
resource "google_compute_firewall" "relay_egress_join" {
  name      = "${local.name_prefix}-allow-relay-egress-join"
  project   = var.project_id
  network   = module.network.network_id
  direction = "EGRESS"
  priority  = 1000

  destination_ranges = ["${module.control.public_address}/32"]
  target_tags        = [module.network.relay_target_tag]

  allow {
    protocol = "tcp"
    ports    = [tostring(local.control_join_port)]
  }
}

locals {
  # The control plane on the management overlay. The identity file is in the
  # mounted secret (tools/sync-secrets.sh copies it from Secret Manager), and
  # binding the two services needs nothing else: no port opens for either.
  # The management API is reached on the controller's internal address and
  # checked against the public one its certificate names.
  control_overlay_env = var.ziti_control_enabled ? {
    MOQOM_CONTROL_ZITI_IDENTITY               = "/etc/moqom/ziti-control.json"
    MOQOM_CONTROL_ZITI_SERVICE                = "${local.name_prefix}-control"
    MOQOM_CONTROL_ZITI_API_SERVICE            = "${local.name_prefix}-api"
    MOQOM_CONTROL_ZITI_MANAGEMENT_URL         = "https://${module.ziti.internal_address}:${module.ziti.ports.management}/edge/management/v1"
    MOQOM_CONTROL_ZITI_MANAGEMENT_SERVER_NAME = module.ziti.external_address
    MOQOM_CONTROL_BYOC_PREFIX                 = "${local.name_prefix}-byoc-"

    # Which VMs may be minted a relay identity: this project's, named as the
    # relay MIGs name them, running as a relay tier's account. Built from the
    # naming rather than read from module.relay, which depends on this module.
    MOQOM_CONTROL_RELAY_ENROLL_AUDIENCE  = local.relay_enroll_audience
    MOQOM_CONTROL_RELAY_PROJECT          = var.project_id
    MOQOM_CONTROL_RELAY_INSTANCE_PREFIX  = "${local.name_prefix}-relay-"
    MOQOM_CONTROL_RELAY_SERVICE_ACCOUNTS = join(",", [for region in keys(local.regions) : "${trimsuffix(substr("${local.name_prefix}-relay-${region}", 0, 30), "-")}@${var.project_id}.iam.gserviceaccount.com"])

    # Off until relays are proven over the overlay; then RelayLink over the
    # internal TCP path is refused.
    MOQOM_CONTROL_RELAY_OVERLAY_ONLY = var.relay_link_overlay_only ? "1" : "0"
  } : {}

  relay_enroll_audience = "${local.name_prefix}-relay-enroll"

  # Dev recordings are test artefacts. They go straight to Nearline and are
  # deleted after a week; nothing here is worth archiving.
  recording_lifecycle = {
    nearline_after_days = 1
    coldline_after_days = 0
    archive_after_days  = 0
    delete_after_days   = 7
  }

  # Hosted recording, on once the recorder's thumbprint is known. The bucket's
  # location and lifecycle go with it so storage is billed at the price of the
  # class each object is actually in.
  control_recording_env = var.recorder_thumbprint == null ? {} : {
    MOQOM_RECORDING_BUCKET          = module.data.recording_bucket
    MOQOM_RECORDER_THUMBPRINT       = var.recorder_thumbprint
    MOQOM_RECORDING_SIGNER_ACCOUNT  = module.recorder.signer_account
    MOQOM_RECORDING_BUCKET_LOCATION = var.region
    MOQOM_RECORDING_LIFECYCLE = join(",", [
      "nearline=${local.recording_lifecycle.nearline_after_days}",
      "coldline=${local.recording_lifecycle.coldline_after_days}",
      "archive=${local.recording_lifecycle.archive_after_days}",
      "delete=${local.recording_lifecycle.delete_after_days}",
    ])
  }
}

# --- Hosted recording --------------------------------------------------------
#
# The recorder pool. docs/RECORDING.md is the design; modules/recorder says
# what each identity may touch. tools/recorder-secrets.sh fills the secrets
# and the Kubernetes secret the workers mount, and prints the thumbprint that
# turns recording on above.
module "recorder" {
  source = "../../modules/recorder"

  project_id  = var.project_id
  name_prefix = local.name_prefix
  namespace   = local.namespace

  image    = var.recorder_image
  replicas = var.recorder_replicas

  recording_bucket        = module.data.recording_bucket
  control_service_account = module.gke.workload_service_accounts["control"]

  # In-cluster, by the service's name, which the link certificate carries.
  control_endpoint    = "https://${local.name_prefix}-control.${local.namespace}.svc.cluster.local:${local.control_grpc_port}"
  control_server_name = local.link_server_name

  # The relay in the cluster's region, on its public address, pinned to the
  # certificate the phones pin.
  relay = "${module.relay[var.region].external_ip}:443"

  kubernetes_secret = "${local.name_prefix}-recorder"

  labels = merge(local.labels, { component = "recorder" })
}

module "relay" {
  source   = "../../modules/relay"
  for_each = local.regions

  project_id  = var.project_id
  region      = each.key
  name_prefix = local.name_prefix

  network      = module.network.network_id
  subnetwork   = module.network.relay_subnets[each.key]
  network_tags = [module.network.relay_target_tag]

  image         = var.relay_image
  registry_host = module.registry.registry_host

  machine_type = "n2d-standard-2"
  min_replicas = 1
  max_replicas = 3

  # Three even though the tier is one machine: a regional MIG rejects a surge
  # smaller than its zone count, so this is a floor rather than a choice.
  update_max_surge = 3

  # One certificate across both regions, because the phones pin its hash: a
  # per-region certificate would mean a per-region hash and a client that has to
  # know which relay it is about to reach before it reaches it.
  tls_certificate_secret_id = "${local.name_prefix}-relay-tls-cert"
  tls_private_key_secret_id = "${local.name_prefix}-relay-tls-key"
  credential_secret_id      = "${local.name_prefix}-relay-credential"

  # The link's own certificates, which are not the ones above: those are what a
  # phone pins, these are what the control plane checks and what this tier
  # checks the control plane with.
  link_ca_secret_id          = "${local.name_prefix}-link-ca"
  link_client_cert_secret_id = "${local.name_prefix}-link-relay-cert"
  link_client_key_secret_id  = "${local.name_prefix}-link-relay-key"

  ticket_keys_secret_id = "${local.name_prefix}-relay-ticket-keys"

  artifact_registry_repository = module.registry.repository_ref
  recording_bucket             = module.data.recording_bucket

  # These are the variables the relay binary actually reads. An earlier version
  # of this file named ports that nothing looks for; a relay started with those
  # binds its defaults and the mismatch shows up as a health check that never
  # passes.
  container_env = {
    MOQOM_RELAY_REGION = each.key
    RUST_LOG           = "info"

    # Where moderation comes from, and now over TLS in both directions: the
    # certificates below say which relay this is, and which control plane that
    # is. The authority is a private one, because no public authority will
    # issue for an address that exists only inside this VPC.
    #
    # Over the overlay once relay_link_via_overlay is on: the tunneler beside
    # the relay listens on loopback and carries the same mutual TLS to the
    # control plane's dark service, so the name check below is unchanged.
    MOQOM_CONTROL_ENDPOINT = var.relay_link_via_overlay ? "https://127.0.0.1:18443" : module.control.endpoint

    # Whose tokens to believe, and the same string the control plane stamps into
    # every one it mints. No MOQOM_RELAY_JWKS to go with it: the keys arrive over
    # the link above and are replaced on every rotation, where a file would go
    # stale at exactly the moment the two have to agree. What this pins is that a
    # token minted by some other deployment is refused even if it happens to
    # carry a key identifier this one also uses.
    MOQOM_RELAY_ISSUER = local.control_host == "" ? "moqom.cloud" : local.control_host

    # What the control plane's certificate has to say it is. The endpoint above
    # is an address, and an address is not an identity.
    MOQOM_CONTROL_SERVER_NAME = local.link_server_name

    MOQOM_RECORDING_BUCKET = module.data.recording_bucket

    # Hosted usage goes to the billing listener on the control plane's public
    # address, whose certificate is from the link authority and names that IP.
    # Reached through Cloud NAT; the relay account must be is_relay with no tenant.
    MOQOM_RELAY_USAGE_URL = "https://${module.control.public_address}:${local.control_join_port}/v1/usage"
    MOQOM_RELAY_USAGE_CA  = "/etc/moqom/link/ca.pem"

    # Every session signs for its own token. The relay recomputes the thumbprint of
    # the key that signed the proof and matches it to the token's `cnf.jkt`, so a
    # copied token is worth nothing without the Secure Enclave it was bound to.
    #
    # `required` rather than the binary's `observed` default, and that default is
    # the right one for a fleet being upgraded a build at a time while this is not:
    # dev is two phones on a desk, both on the current build, and leaving it at
    # `observed` would mean the property is configured everywhere and enforced
    # nowhere -- which reads as done and is not. It is also the only setting under
    # which a client that quietly stopped sending proofs gets noticed here rather
    # than in production.
    #
    # Note that `observed` would not have been permissive about a *wrong* proof
    # either; it admits a session that offers none. The difference this makes is to
    # the older builds, and there are none left to protect.
    MOQOM_RELAY_POSSESSION = "required"

    # MOQOM_RELAY_DSCP is deliberately unset. Turning it on asks the network for an interactive
    # class on the relay's egress, which is the half of the phone's radio hop that the client's
    # own marking does not cover -- marking the uplink was worth 15.6ms at the 95th percentile of
    # link latency and 8.2ms of glass-to-glass, and each frame crosses that radio twice.
    #
    # It was deployed here with AF41 and measured, and the codepoint does not arrive. The relay
    # marks correctly: the instances ran the marking build with the value set and stayed healthy,
    # which a failed socket option would not have allowed. But replies read off the wire at the
    # far end came back with a type-of-service byte of zero on ten consecutive sockets, each
    # landing on its own backend. Something between here and a home network clears it, and whether
    # that is Google's fabric on egress or the ISP was not established.
    #
    # So the setting stays off, and the code behind it stays: it costs nothing idle and is the
    # part that would be hard to write again on the day a path honours it.
  }

  # The overlay tunneler: an identity minted for each instance at boot by the
  # control plane, on the join port beside /relay/link.
  ziti = var.ziti_relay_enabled ? {
    cli_image       = var.ziti_cli_image
    enroll_url      = "https://${module.control.public_address}:${local.control_join_port}/relay/enroll"
    enroll_audience = local.relay_enroll_audience
  } : null

  labels = merge(local.labels, { component = "relay" })

  depends_on = [module.data]
}

# --- Management overlay ------------------------------------------------------
#
# A self-hosted OpenZiti network carrying the management plane: RelayLink, the
# admin API, relay admin ports and operator access. Media never touches it.
# docs/OPENZITI-AND-P2P.md is the reasoning; tools/ziti-provision.sh makes the
# services, policies and identities on it.
module "ziti" {
  source = "../../modules/ziti"

  project_id  = var.project_id
  region      = var.region
  zone        = "${var.region}-b"
  name_prefix = local.name_prefix

  network    = module.network.network_id
  subnetwork = module.network.relay_subnets[var.region]

  controller_image             = var.ziti_controller_image
  router_image                 = var.ziti_router_image
  sql_proxy_image              = var.ziti_sql_proxy_image
  registry_host                = module.registry.registry_host
  artifact_registry_repository = module.registry.repository_ref

  # The control plane mints identities from the cluster; an operator's
  # provisioning run arrives through IAP.
  management_source_ranges = [local.cluster_pod_cidr, local.cluster_node_cidr, "35.235.240.0/20"]

  admin_password_secret_id = "${local.name_prefix}-ziti-admin-password"
  pki_backup_secret_id     = "${local.name_prefix}-ziti-pki"

  sql_instance_connection_name = module.data.instance_connection_name

  labels = merge(local.labels, { component = "ziti" })

  depends_on = [module.data]
}

module "federation" {
  source = "../../modules/federation"

  project_id  = var.project_id
  name_prefix = local.name_prefix
  pool_id     = "${local.name_prefix}-external"

  github = {
    owner        = var.github_owner
    repositories = var.github_repositories
    allowed_refs = []
  }

  # Dev CI deploys images and rolls the relay tier. It cannot touch IAM,
  # networking, or the database. Same shape as prod deliberately: dev is where a
  # broken deploy is found, and it only finds the permission-shaped ones if the
  # permissions match.
  deployer_roles = [
    "roles/container.developer",
    "roles/compute.instanceAdmin.v1",
  ]

  deployer_impersonates = merge(
    { control = module.gke.workload_service_accounts["control"] },
    { for region, relay in module.relay : "relay-${region}" => relay.service_account_email },
  )

  depends_on = [module.services]
}

module "observability" {
  source = "../../modules/observability"

  project_id  = var.project_id
  name_prefix = local.name_prefix

  log_retention_days = 7

  # Dev alerts on nothing. Every teardown and every broken branch would
  # otherwise page whoever is on the notification list, and an alert channel
  # people mute is worse than no channel.
  alert_enabled       = false
  notification_emails = var.notification_emails

  sql_instance_id = module.data.instance_name

  # Empty: the control plane moved to the cluster, and there is nothing on Cloud
  # Run any more. Cluster and workload metrics come from GKE's own pipeline.
  cloud_run_services = []

  # Nothing public to check yet. The control plane answers on an internal
  # address, and an uptime check runs from outside.
  uptime_check_host = null

  labels = local.labels
}

module "edge_dns" {
  source = "../../modules/dns-steering"

  count = var.dns_domain == null ? 0 : 1

  project_id  = var.project_id
  zone_name   = module.network.dns_zone_name
  record_name = local.edge_host
  endpoints   = { for region, relay in module.relay : region => relay.external_ip }
  ttl         = 60
}

# --- Compliance --------------------------------------------------------------
#
# Audit logging, the evidence bucket, security alerts, and the nightly check
# that reports on everything above. docs/compliance/ is the description; this
# is the part that does it. Unlike observability, dev alerts here: this is the
# only environment, so it is the one an auditor would look at.

module "compliance" {
  source = "../../modules/compliance"

  project_id  = var.project_id
  name_prefix = local.name_prefix
  region      = var.region

  evidence_bucket_name = "${local.name_prefix}-evidence-${var.project_id}"
  evidence_writers     = var.compliance_evidence_writers

  # Off until the configuration has settled. Both are irreversible for a year.
  lock_audit_log_bucket   = false
  lock_evidence_retention = false

  notification_emails = var.compliance_alert_emails

  # The relays read their TLS key and RelayLink credential at boot. Anyone
  # else reading a payload is the alert.
  # The overlay VM reads its own admin password and PKI backup at boot.
  expected_secret_accessors = concat(
    [for relay in module.relay : relay.service_account_email],
    [module.ziti.service_account_email],
  )

  compliance_image   = var.compliance_image
  public_buckets     = ["moqom-sites"]
  allowed_owners     = ["user:elliott@moqom.cloud"]
  registry_locations = ["us"]

  labels = local.labels
}

module "retention" {
  source = "../../modules/retention"

  count = var.compliance_image == null ? 0 : 1

  name            = "${local.name_prefix}-retention"
  namespace       = local.namespace
  service_account = module.gke.workload_identity_bindings["control"].service_account
  image           = var.compliance_image
  dry_run         = var.retention_dry_run

  env = {
    MOQOM_CONTROL_SQL_INSTANCE = module.data.instance_connection_name
    PGUSER                     = module.data.iam_usernames[module.gke.workload_service_accounts["control"]]
    PGDATABASE                 = "moqom"
  }

  depends_on = [module.control]
}
