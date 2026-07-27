# The control plane is request/response over HTTPS, so Cloud Run is the right
# shape for it: no long-lived connections, no UDP, no reason to own a VM. This
# is the half of MOQOM that a managed platform serves well, and the contrast
# with the relay module next door is the whole architectural argument.

resource "google_service_account" "service" {
  account_id   = substr(var.service_name, 0, 30)
  project      = var.project_id
  display_name = "MOQOM ${var.service_name}"
  description  = "Runtime identity for the ${var.service_name} Cloud Run service."
}

locals {
  postgres_username = trimsuffix(google_service_account.service.email, ".gserviceaccount.com")
}

resource "google_project_iam_member" "base" {
  for_each = toset(concat([
    "roles/logging.logWriter",
    "roles/monitoring.metricWriter",
    "roles/cloudtrace.agent",
  ], var.extra_roles))

  project = var.project_id
  role    = each.value
  member  = google_service_account.service.member
}

resource "google_artifact_registry_repository_iam_member" "pull" {
  count = var.artifact_registry_repository == null ? 0 : 1

  project    = var.artifact_registry_repository.project_id
  location   = var.artifact_registry_repository.location
  repository = var.artifact_registry_repository.repository
  role       = "roles/artifactregistry.reader"
  member     = google_service_account.service.member
}

# Cloud SQL turns a service account into a Postgres role by stripping the
# `.gserviceaccount.com` suffix from its email. The truncated form is what the
# application must send as its username; the access token is the password.
resource "google_sql_user" "iam" {
  count = var.sql_instance_name == null ? 0 : 1

  name     = local.postgres_username
  project  = var.project_id
  instance = var.sql_instance_name
  type     = "CLOUD_IAM_SERVICE_ACCOUNT"
}

resource "google_project_iam_member" "sql" {
  for_each = var.sql_instance_name == null ? toset([]) : toset([
    "roles/cloudsql.client",
    "roles/cloudsql.instanceUser",
  ])

  project = var.project_id
  role    = each.value
  member  = google_service_account.service.member
}

resource "google_storage_bucket_iam_member" "readable" {
  for_each = toset(var.readable_buckets)

  bucket = each.value
  role   = "roles/storage.objectViewer"
  member = google_service_account.service.member
}

# Access to the secrets this service actually mounts, and to nothing else. The
# grant lives with the reference rather than with the secret, so adding an
# environment variable cannot leave a service with a mount it is not permitted
# to read — the two are the same line of configuration.
resource "google_secret_manager_secret_iam_member" "secret_env" {
  for_each = var.secret_env

  project   = var.project_id
  secret_id = each.value
  role      = "roles/secretmanager.secretAccessor"
  member    = google_service_account.service.member
}

resource "google_cloud_run_v2_service" "this" {
  name     = var.service_name
  project  = var.project_id
  location = var.region

  ingress             = var.public_ingress ? "INGRESS_TRAFFIC_ALL" : "INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER"
  deletion_protection = var.deletion_protection
  labels              = var.labels

  template {
    service_account                  = google_service_account.service.email
    max_instance_request_concurrency = var.concurrency
    timeout                          = "${var.request_timeout_seconds}s"
    execution_environment            = "EXECUTION_ENVIRONMENT_GEN2"

    scaling {
      min_instance_count = var.min_instances
      max_instance_count = var.max_instances
    }

    # Direct VPC egress rather than a Serverless VPC Access connector. The
    # connector is a managed instance group you pay for and have to size; direct
    # egress attaches the instance to the subnet itself, which is both cheaper
    # and the only variant that supports network tags — and without tags there
    # is no way to write a firewall rule that names this service.
    vpc_access {
      egress = var.egress

      network_interfaces {
        network    = var.network
        subnetwork = var.subnetwork
        tags       = var.network_tags
      }
    }

    dynamic "volumes" {
      for_each = var.sql_instance_connection_name == null ? [] : [var.sql_instance_connection_name]
      content {
        name = "cloudsql"
        cloud_sql_instance {
          instances = [volumes.value]
        }
      }
    }

    containers {
      image = var.image

      ports {
        container_port = var.container_port
      }

      resources {
        limits = {
          cpu    = var.cpu
          memory = var.memory
        }

        # CPU is throttled between requests. The control plane has no background
        # work that must run outside a request, and always-allocated CPU costs
        # several times more.
        cpu_idle          = true
        startup_cpu_boost = true
      }

      dynamic "env" {
        for_each = var.env
        content {
          name  = env.key
          value = env.value
        }
      }

      # Injected rather than left to the caller because the correct value is a
      # mangled form of an identity created inside this module, and a caller
      # asking for it would be asking the module for its own output.
      dynamic "env" {
        for_each = var.sql_instance_name == null ? [] : [local.postgres_username]
        content {
          name  = "PGUSER"
          value = env.value
        }
      }

      dynamic "env" {
        for_each = var.secret_env
        content {
          name = env.key
          value_source {
            secret_key_ref {
              secret  = env.value
              version = "latest"
            }
          }
        }
      }

      dynamic "volume_mounts" {
        for_each = var.sql_instance_connection_name == null ? [] : [1]
        content {
          name       = "cloudsql"
          mount_path = "/cloudsql"
        }
      }

      startup_probe {
        initial_delay_seconds = 2
        period_seconds        = 3
        timeout_seconds       = 2
        failure_threshold     = 10

        http_get {
          path = var.health_check_path
          port = var.container_port
        }
      }

      liveness_probe {
        period_seconds    = 30
        timeout_seconds   = 5
        failure_threshold = 3

        http_get {
          path = var.health_check_path
          port = var.container_port
        }
      }
    }
  }

  traffic {
    type    = "TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST"
    percent = 100
  }

  # Cloud Run resolves secret mounts at revision creation, not at first request,
  # so a deploy that races the IAM binding fails outright rather than degrading.
  depends_on = [google_secret_manager_secret_iam_member.secret_env]
}

# Cloud Run IAM is a coarse gate in front of the application's own
# authorisation, not a replacement for it: MOQOM's clients are browsers and
# mobile apps carrying CAT tokens, and they have no Google identity to present.
# So a public API is public at this layer by necessity. The follow-up is a
# global external Application Load Balancer with Cloud Armor in front, which is
# what gives rate limiting and geo blocking; `public_ingress = false` plus an
# internal ingress setting is the switch that lets you move to it.
resource "google_cloud_run_v2_service_iam_member" "public" {
  count = var.public_ingress ? 1 : 0

  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.this.name
  role     = "roles/run.invoker"
  member   = "allUsers"
}

resource "google_cloud_run_v2_service_iam_member" "invokers" {
  for_each = var.public_ingress ? toset([]) : toset(var.invokers)

  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.this.name
  role     = "roles/run.invoker"
  member   = each.value
}
