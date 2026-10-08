# The 3D Viewer licence server: Stripe checkout and webhook, licence keys and
# signed tokens (source: services/licensing in the viewer repo). Cloud Run
# behind the sites load balancer, which routes licensing.elliottwave.studio/v1/*
# here (envs/web). Firestore holds the licences: no instance to run, and the
# traffic (one activation a week per WordPress site, one cached token fetch per
# page elsewhere) fits the free tier.
#
# Secrets are containers only. Their values are added by hand with
# `gcloud secrets versions add`, so they never pass through Terraform state.

locals {
  secrets = merge(
    {
      SIGNING_KEY           = "${var.name}-signing-key"
      STRIPE_SECRET_KEY     = "${var.name}-stripe-secret-key"
      STRIPE_WEBHOOK_SECRET = "${var.name}-stripe-webhook-secret"
    },
    var.smtp == null ? {} : { SMTP_PASSWORD = "${var.name}-smtp-password" },
  )

  env = merge(
    {
      STORE                 = "firestore"
      FIRESTORE_PROJECT     = var.project_id
      FIRESTORE_DATABASE    = google_firestore_database.this.name
      DOCS_URL              = var.docs_url
      ALLOWED_ORIGINS       = join(",", var.allowed_origins)
      STRIPE_PRICE_YEARLY   = var.stripe_price_yearly
      STRIPE_PRICE_LIFETIME = var.stripe_price_lifetime
    },
    var.smtp == null ? {} : {
      SMTP_HOST     = var.smtp.host
      SMTP_PORT     = var.smtp.port
      SMTP_USERNAME = var.smtp.username
      MAIL_FROM     = var.smtp.from
    },
  )
}

resource "google_service_account" "service" {
  project      = var.project_id
  account_id   = var.name
  display_name = "3D Viewer licence server"
  description  = "Runtime identity for the ${var.name} Cloud Run service."
}

resource "google_project_iam_member" "logging" {
  project = var.project_id
  role    = "roles/logging.logWriter"
  member  = google_service_account.service.member
}

# Read and write in this database only.
resource "google_project_iam_member" "firestore" {
  project = var.project_id
  role    = "roles/datastore.user"
  member  = google_service_account.service.member

  condition {
    title      = "${var.name} database only"
    expression = "resource.name == \"projects/${var.project_id}/databases/${google_firestore_database.this.name}\""
  }
}

resource "google_firestore_database" "this" {
  project     = var.project_id
  name        = var.firestore_database
  location_id = var.firestore_location
  type        = "FIRESTORE_NATIVE"
  # Licence creation and host moves run in transactions; pessimistic locking
  # makes concurrent webhook deliveries wait rather than retry.
  concurrency_mode                  = "PESSIMISTIC"
  point_in_time_recovery_enablement = "POINT_IN_TIME_RECOVERY_ENABLED"
  delete_protection_state           = var.deletion_protection ? "DELETE_PROTECTION_ENABLED" : "DELETE_PROTECTION_DISABLED"
  deletion_policy                   = var.deletion_protection ? "ABANDON" : "DELETE"
}

# Processed Stripe event IDs and pending emailed confirmations expire on their
# own. Firestore deletes expired documents within a day or so, so the server
# also checks the expiry itself.
resource "google_firestore_field" "ttl" {
  for_each = toset(["events", "requests"])

  project    = var.project_id
  database   = google_firestore_database.this.name
  collection = each.key
  field      = "expires"

  ttl_config {}
  index_config {}
}

resource "google_secret_manager_secret" "this" {
  for_each = local.secrets

  project   = var.project_id
  secret_id = each.value
  labels    = var.labels

  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_iam_member" "access" {
  for_each = local.secrets

  project   = var.project_id
  secret_id = google_secret_manager_secret.this[each.key].secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = google_service_account.service.member
}

resource "google_cloud_run_v2_service" "this" {
  project  = var.project_id
  name     = var.name
  location = var.region
  labels   = var.labels

  # Only the load balancer reaches it, so the CDN and the host's TLS
  # certificate are always in front.
  ingress             = "INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER"
  deletion_protection = var.deletion_protection

  template {
    service_account                  = google_service_account.service.email
    max_instance_request_concurrency = 80
    timeout                          = "30s"

    scaling {
      min_instance_count = 0
      max_instance_count = var.max_instances
    }

    containers {
      image = var.image

      ports {
        container_port = 8080
      }

      resources {
        limits = {
          cpu    = "1"
          memory = "256Mi"
        }
        cpu_idle          = true
        startup_cpu_boost = true
      }

      dynamic "env" {
        for_each = local.env
        content {
          name  = env.key
          value = env.value
        }
      }

      dynamic "env" {
        for_each = local.secrets
        content {
          name = env.key
          value_source {
            secret_key_ref {
              secret  = google_secret_manager_secret.this[env.key].secret_id
              version = "latest"
            }
          }
        }
      }

      startup_probe {
        period_seconds    = 3
        failure_threshold = 10
        http_get {
          path = "/v1/healthz"
        }
      }
    }
  }

  depends_on = [google_secret_manager_secret_iam_member.access]
}

# The load balancer's serverless NEG calls the service without a Google
# identity, so invocation is open; ingress limits it to the load balancer.
# Authorisation is in the application (Stripe signatures, keys, emailed links).
resource "google_cloud_run_v2_service_iam_member" "public" {
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.this.name
  role     = "roles/run.invoker"
  member   = "allUsers"
}

resource "google_compute_region_network_endpoint_group" "this" {
  project               = var.project_id
  name                  = var.name
  region                = var.region
  network_endpoint_type = "SERVERLESS"

  cloud_run {
    service = google_cloud_run_v2_service.this.name
  }
}

# Cloud CDN caches only what the server marks public: GET /v1/token (1 day in
# browsers, 7 days here) and its negative answers (1 hour). POSTs and
# everything else send no-store or nothing cacheable.
resource "google_compute_backend_service" "this" {
  project               = var.project_id
  name                  = var.name
  protocol              = "HTTPS"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  enable_cdn            = true

  backend {
    group = google_compute_region_network_endpoint_group.this.id
  }

  cdn_policy {
    cache_mode       = "USE_ORIGIN_HEADERS"
    negative_caching = false
    # Serve the last good token if Cloud Run is failing (verify the maximum
    # Cloud CDN accepts for this setting).
    serve_while_stale = 86400

    cache_key_policy {
      include_host         = true
      include_protocol     = true
      include_query_string = true
    }
  }

  # Off: request URLs carry licence keys (GET /v1/token?key=…). Keys aren't
  # secret, but the server promises not to log them.
  log_config {
    enable = false
  }
}

# Cloud Run writes a request log line with the full URL for every request.
# Drop those for this service, for the same reason.
resource "google_logging_project_exclusion" "requests" {
  project     = var.project_id
  name        = "${var.name}-request-urls"
  description = "Request logs for ${var.name} contain licence keys in query strings."
  filter      = "resource.type=\"cloud_run_revision\" AND resource.labels.service_name=\"${var.name}\" AND log_id(\"run.googleapis.com/requests\")"
}
