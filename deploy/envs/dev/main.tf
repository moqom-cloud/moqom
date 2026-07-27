locals {
  env         = "dev"
  name_prefix = "moqom-dev"

  labels = {
    environment = local.env
    managed-by  = "terraform"
    system      = "moqom"
  }

  regions = {
    (var.region) = {
      relay_cidr      = "10.20.0.0/20"
      serverless_cidr = "10.20.16.0/26"
    }
  }

  control_host = var.dns_domain == null ? "" : "api.${trimsuffix(var.dns_domain, ".")}"
  edge_host    = var.dns_domain == null ? "" : "edge.${var.dns_domain}"

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
  regions     = local.regions

  # On in dev only. This is where egress rules get tightened, and doing it
  # without connection logs is guesswork.
  enable_firewall_logging = true
  enable_iap_ssh          = true

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

  # Dev CI deploys images and revisions. It cannot touch IAM, networking, or the
  # database. Same shape as prod deliberately: dev is where a broken deploy is
  # found, and it only finds the permission-shaped ones if the permissions
  # match.
  deployer_roles = [
    "roles/run.developer",
    "roles/compute.instanceAdmin.v1",
  ]

  deployer_impersonates = [
    module.relay.service_account_email,
    module.control.service_account_email,
    module.director.service_account_email,
  ]

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

module "relay" {
  source = "../../modules/relay"

  project_id  = var.project_id
  region      = var.region
  name_prefix = local.name_prefix

  network      = module.network.network_id
  subnetwork   = module.network.relay_subnets[var.region]
  network_tags = [module.network.relay_target_tag]

  image         = var.relay_image
  registry_host = module.registry.registry_host

  machine_type = "n2d-standard-2"
  min_replicas = 1
  max_replicas = 3

  # Three even though the tier is one machine: a regional MIG rejects a surge
  # smaller than its zone count, so this is a floor rather than a choice.
  update_max_surge = 3

  tls_certificate_secret_id = "${local.name_prefix}-relay-tls-cert"
  tls_private_key_secret_id = "${local.name_prefix}-relay-tls-key"

  artifact_registry_repository = module.registry.repository_ref
  recording_bucket             = module.data.recording_bucket

  container_env = {
    MOQOM_ENV              = local.env
    MOQOM_REGION           = var.region
    MOQOM_LOG_LEVEL        = "debug"
    MOQOM_RECORDING_BUCKET = module.data.recording_bucket
    MOQOM_CONTROL_HOST     = local.control_host
  }

  labels = merge(local.labels, { component = "relay" })

  depends_on = [module.data]
}

module "control" {
  source = "../../modules/control-plane"

  project_id   = var.project_id
  region       = var.region
  service_name = "${local.name_prefix}-control"
  image        = var.control_image

  network      = module.network.network_id
  subnetwork   = module.network.serverless_subnets[var.region]
  network_tags = [module.network.serverless_target_tag]

  min_instances = 0
  max_instances = 3
  cpu           = "1"
  memory        = "512Mi"

  deletion_protection = false

  sql_instance_name = module.data.instance_name

  env = {
    MOQOM_ENV       = local.env
    MOQOM_LOG_LEVEL = "debug"
    PGHOST          = module.data.private_ip
    PGDATABASE      = "moqom"
    PGSSLMODE       = "require"
    PGIAMAUTH       = "true"
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
  region       = var.region
  service_name = "${local.name_prefix}-director"
  image        = var.director_image

  network      = module.network.network_id
  subnetwork   = module.network.serverless_subnets[var.region]
  network_tags = [module.network.serverless_target_tag]

  min_instances = 0
  max_instances = 2
  cpu           = "1"
  memory        = "256Mi"

  deletion_protection = false

  env = {
    MOQOM_ENV = local.env

    # The director hands clients a relay. With one region it has nothing to
    # choose between, which is the point of keeping dev single-region: the
    # steering logic has to be correct in the degenerate case first.
    MOQOM_RELAY_ENDPOINTS = jsonencode({ (var.region) = module.relay.external_ip })
  }

  artifact_registry_repository = module.registry.repository_ref

  labels = merge(local.labels, { component = "director" })
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
  cloud_run_services = [
    module.control.service_name,
    module.director.service_name,
  ]

  uptime_check_host = local.control_host == "" ? null : local.control_host

  labels = local.labels
}

module "edge_dns" {
  source = "../../modules/dns-steering"

  count = var.dns_domain == null ? 0 : 1

  project_id  = var.project_id
  zone_name   = module.network.dns_zone_name
  record_name = local.edge_host
  endpoints   = { (var.region) = module.relay.external_ip }
  ttl         = 60
}
