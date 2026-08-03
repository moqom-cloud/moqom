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
  }

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

  backup_retention_days          = 3
  transaction_log_retention_days = 1
  deletion_protection            = false

  recording_bucket_name     = var.recording_bucket_name
  recording_bucket_location = var.region

  # Dev recordings are test artefacts. They go straight to Nearline and are
  # deleted after a week; nothing here is worth archiving.
  recording_lifecycle = {
    nearline_after_days = 1
    coldline_after_days = 0
    archive_after_days  = 0
    delete_after_days   = 7
  }

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

  env = {
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
  }

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
    MOQOM_CONTROL_ENDPOINT = module.control.endpoint

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
  }

  labels = merge(local.labels, { component = "relay" })

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
