locals {
  env         = "prod"
  name_prefix = "moqom-prod"

  labels = {
    environment = local.env
    managed-by  = "terraform"
    system      = "moqom"
  }

  network_regions = {
    for region, cfg in var.relay_regions : region => {
      relay_cidr      = cfg.relay_cidr
      serverless_cidr = cfg.serverless_cidr
    }
  }

  control_host = "api.${trimsuffix(var.dns_domain, ".")}"
  edge_host    = "edge.${var.dns_domain}"

  # The relays answer STUN on this port, at the same addresses phones reach
  # them on. One value for the firewall, the relays and the grant the control
  # plane hands out, so the three cannot disagree.
  relay_stun_port = 3478

  # Relays on IPv6 too, behind the same backends, so a device on an IPv6-only
  # or dual-stack network reaches one natively and STUN reports its public
  # IPv6 address for a direct path. One switch for the subnet, the firewall,
  # the relays and the AAAA record.
  relay_ipv6 = true

  relay_endpoints = { for region, relay in module.relay : region => relay.external_ip }

  secrets = {
    "${local.name_prefix}-cat-signing-key" = []
    "${local.name_prefix}-relay-tls-cert"  = []
    "${local.name_prefix}-relay-tls-key"   = []
  }
}

module "services" {
  source = "../../modules/project-services"

  project_id = var.project_id

  services = [
    "artifactregistry.googleapis.com",
    "cloudkms.googleapis.com",
    "cloudresourcemanager.googleapis.com",
    "compute.googleapis.com",
    "dns.googleapis.com",
    "iam.googleapis.com",
    "iamcredentials.googleapis.com",
    "logging.googleapis.com",
    "monitoring.googleapis.com",
    "run.googleapis.com",
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
  regions     = local.network_regions

  enable_firewall_logging = false
  enable_iap_ssh          = false

  create_dns_zone = true
  dns_zone_name   = "${local.name_prefix}-edge"
  dns_domain      = var.dns_domain

  relay_stun_port = local.relay_stun_port
  enable_ipv6     = local.relay_ipv6

  labels = local.labels

  depends_on = [module.services]
}

module "registry" {
  source = "../../modules/artifact-registry"

  project_id    = var.project_id
  location      = "us"
  repository_id = "moqom"

  immutable_tags       = true
  keep_tagged_versions = 50

  # Dry run stays on until the policy has been observed for a full retention
  # cycle. Deleting an image a MIG is pinned to does not fail the delete; it
  # fails the next autoscale event, hours or days later.
  cleanup_policy_dry_run = true

  labels = local.labels

  depends_on = [module.services]
}

module "federation" {
  source = "../../modules/federation"

  project_id  = var.project_id
  name_prefix = local.name_prefix
  pool_id     = "${local.name_prefix}-external"

  github = {
    owner        = var.github_owner
    repositories = var.github_repositories
    allowed_refs = var.github_deploy_refs
  }

  # Narrower than dev. Production CI ships revisions and rolls instance groups;
  # it cannot create service accounts, edit IAM, or reach the database, so the
  # worst a compromised workflow can do is deploy a bad image — which is
  # recoverable — rather than mint itself an identity, which is not.
  #
  # compute.instanceAdmin.v1 is still the widest grant here. There is no
  # narrower predefined role that can roll a managed instance group, so closing
  # it further means a custom role; until then the thing that keeps it from
  # being an escalation is that "act as" is scoped below rather than granted
  # project-wide.
  deployer_roles = [
    "roles/run.developer",
    "roles/compute.instanceAdmin.v1",
  ]

  # Exactly the identities a deploy has to launch workloads as: each relay
  # region, and the two Cloud Run services.
  deployer_impersonates = concat(
    [for relay in module.relay : relay.service_account_email],
    [
      module.control.service_account_email,
      module.director.service_account_email,
    ],
  )

  aws_accounts = var.byoc_aws_accounts

  byoc_roles = [
    "roles/monitoring.metricWriter",
    "roles/logging.logWriter",
  ]

  depends_on = [module.services]
}

module "data" {
  source = "../../modules/data"

  project_id     = var.project_id
  region         = var.primary_region
  name_prefix    = local.name_prefix
  network_id     = module.network.network_id
  psa_connection = module.network.psa_connection

  tier              = "db-custom-4-16384"
  availability_type = "REGIONAL"
  disk_size_gb      = 100

  backup_retention_days          = 30
  transaction_log_retention_days = 7
  enable_point_in_time_recovery  = true
  deletion_protection            = true

  recording_bucket_name     = var.recording_bucket_name
  recording_bucket_location = var.recording_bucket_location

  recording_lifecycle = {
    nearline_after_days = 30
    coldline_after_days = 90
    archive_after_days  = 180
    delete_after_days   = var.recording_retention_days
  }

  recording_bucket_force_destroy = false

  kms_key_name = var.kms_key_name
  secrets      = local.secrets
  labels       = local.labels

  depends_on = [module.services]
}

# One instance per region. Every relay tier is identical apart from sizing; the
# regions differ only in how much traffic they carry, which is the property that
# makes this a for_each rather than three hand-written stanzas.
module "relay" {
  source   = "../../modules/relay"
  for_each = var.relay_regions

  project_id  = var.project_id
  region      = each.key
  name_prefix = local.name_prefix

  network      = module.network.network_id
  subnetwork   = module.network.relay_subnets[each.key]
  network_tags = [module.network.relay_target_tag]
  stun_port    = local.relay_stun_port
  enable_ipv6  = local.relay_ipv6

  image         = var.relay_image
  registry_host = module.registry.registry_host

  machine_type = each.value.machine_type
  min_replicas = each.value.min_replicas
  max_replicas = each.value.max_replicas

  # Below the CPU level at which a relay starts dropping datagrams, with enough
  # margin to absorb a boot. Relays are added faster than viewers arrive or they
  # are not added in time at all.
  target_cpu_utilization = 0.5

  update_max_surge = 3

  tls_certificate_secret_id = "${local.name_prefix}-relay-tls-cert"
  tls_private_key_secret_id = "${local.name_prefix}-relay-tls-key"

  artifact_registry_repository = module.registry.repository_ref
  recording_bucket             = module.data.recording_bucket

  container_env = {
    MOQOM_ENV              = local.env
    MOQOM_REGION           = each.key
    MOQOM_LOG_LEVEL        = "info"
    MOQOM_RECORDING_BUCKET = module.data.recording_bucket
    MOQOM_CONTROL_HOST     = local.control_host
  }

  labels = merge(local.labels, { component = "relay" })

  depends_on = [module.data]
}

module "control" {
  source = "../../modules/control-plane"

  project_id   = var.project_id
  region       = var.primary_region
  service_name = "${local.name_prefix}-control"
  image        = var.control_image

  network      = module.network.network_id
  subnetwork   = module.network.serverless_subnets[var.primary_region]
  network_tags = [module.network.serverless_target_tag]

  # A cold start on the token endpoint is a viewer waiting to join a stream that
  # has already begun.
  min_instances = 2
  max_instances = 50
  cpu           = "2"
  memory        = "1Gi"
  concurrency   = 80

  deletion_protection = true

  sql_instance_name = module.data.instance_name

  env = {
    MOQOM_ENV       = local.env
    MOQOM_LOG_LEVEL = "info"
    # Cloud Run terminates TLS in front of the container, so the process itself
    # serves plaintext. The binary refuses that unless it is said here.
    MOQOM_CONTROL_ALLOW_PLAINTEXT = "true"
    PGHOST                        = module.data.private_ip
    PGDATABASE                    = "moqom"
    PGSSLMODE                     = "verify-full"
    PGIAMAUTH                     = "true"

    # The STUN server named in every direct grant, which the SDKs ask by
    # default for the public address they offer a peer. The steered edge name,
    # since every relay answers.
    MOQOM_CONTROL_STUN = "${trimsuffix(local.edge_host, ".")}:${local.relay_stun_port}"
  }

  secret_env = {
    MOQOM_CAT_SIGNING_KEY = module.data.secret_ids["${local.name_prefix}-cat-signing-key"]
  }

  readable_buckets             = [module.data.recording_bucket]
  artifact_registry_repository = module.registry.repository_ref

  labels = merge(local.labels, { component = "control" })
}

module "director" {
  source = "../../modules/control-plane"

  project_id   = var.project_id
  region       = var.primary_region
  service_name = "${local.name_prefix}-director"
  image        = var.director_image

  network      = module.network.network_id
  subnetwork   = module.network.serverless_subnets[var.primary_region]
  network_tags = [module.network.serverless_target_tag]

  min_instances = 2
  max_instances = 20
  cpu           = "1"
  memory        = "512Mi"

  deletion_protection = true

  env = {
    MOQOM_ENV = local.env

    # The DNS policy steers by geography; the director steers by measurement.
    # Handing it the full endpoint set is what lets it override a geographic
    # guess when a region is degraded, which is the failure mode DNS routing
    # cannot see.
    MOQOM_RELAY_ENDPOINTS = jsonencode(local.relay_endpoints)
  }

  artifact_registry_repository = module.registry.repository_ref

  labels = merge(local.labels, { component = "director" })
}

module "observability" {
  source = "../../modules/observability"

  project_id  = var.project_id
  name_prefix = local.name_prefix

  log_retention_days = 90

  alert_enabled       = true
  notification_emails = var.notification_emails

  sql_instance_id = module.data.instance_name
  cloud_run_services = [
    module.control.service_name,
    module.director.service_name,
  ]

  uptime_check_host = local.control_host

  labels = local.labels
}

module "edge_dns" {
  source = "../../modules/dns-steering"

  project_id  = var.project_id
  zone_name   = module.network.dns_zone_name
  record_name = local.edge_host
  endpoints   = local.relay_endpoints
  endpoints_v6 = {
    for region, relay in module.relay : region => relay.external_ipv6 if relay.external_ipv6 != null
  }

  # Short, because this TTL is the failover time for a region going dark and
  # there is no health checking behind a geolocation policy.
  ttl = 30
}
