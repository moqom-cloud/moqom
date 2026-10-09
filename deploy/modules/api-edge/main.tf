# The API's own TLS edge: cmd/moqom-api-edge in front of the control plane's
# public listener, on a regional address of its own.
#
# It replaces the sites' global Application Load Balancer for api.moqom.cloud
# because that balancer only negotiates classical key exchange. A passthrough
# network load balancer leaves the handshake to the pods, which accept only
# ML-KEM (pqtls.PublicCurves), and with externalTrafficPolicy=Local the pods
# see the client's own address.
#
# Certificates come from Let's Encrypt and are cached in a private bucket here,
# shared by the replicas so a restart or a second pod does not issue again.
#
# The identity and the bucket exist whatever the image; with no image nothing
# runs and no address is reserved, since an idle static address is billed.

locals {
  name            = "${var.name_prefix}-api-edge"
  service_account = "moqom-api-edge"
  selector        = { "app.kubernetes.io/name" = local.name }
  labels = merge(var.labels, {
    "app.kubernetes.io/name"       = local.name
    "app.kubernetes.io/managed-by" = "terraform"
  })

  https_port  = 8443
  http_port   = 8000
  health_port = 8080
  ca_mount    = "/etc/moqom-api-edge"
  enabled     = var.image != null
}

# The ACME account key and the certificate's private key. Private, versioned
# off (autocert rewrites the same few objects), readable only by the edge.
resource "google_storage_bucket" "acme" {
  name     = var.acme_bucket_name
  project  = var.project_id
  location = var.region

  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  force_destroy               = false

  labels = var.labels
}

resource "google_service_account" "edge" {
  project      = var.project_id
  account_id   = local.name
  display_name = "MOQOM API edge"
  description  = "Terminates TLS for the API. May read and write its ACME cache bucket and nothing else."
}

resource "google_service_account_iam_member" "edge_identity" {
  service_account_id = google_service_account.edge.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${var.namespace}/${local.service_account}]"
}

resource "google_storage_bucket_iam_member" "edge_cache" {
  bucket = google_storage_bucket.acme.name
  role   = "roles/storage.objectUser"
  member = google_service_account.edge.member
}

resource "kubernetes_service_account_v1" "edge" {
  metadata {
    name      = local.service_account
    namespace = var.namespace
    labels    = local.labels
    annotations = {
      "iam.gke.io/gcp-service-account" = google_service_account.edge.email
    }
  }
}

resource "google_compute_address" "edge" {
  count = local.enabled ? 1 : 0

  name         = local.name
  project      = var.project_id
  region       = var.region
  address_type = "EXTERNAL"
  network_tier = "PREMIUM"
  description  = "api.moqom.cloud: the A record points here."
  labels       = var.labels
}

resource "kubernetes_deployment_v1" "edge" {
  count = local.enabled ? 1 : 0

  metadata {
    name      = local.name
    namespace = var.namespace
    labels    = local.labels
  }

  spec {
    replicas = var.replicas

    selector {
      match_labels = local.selector
    }

    strategy {
      type = "RollingUpdate"
      rolling_update {
        max_surge       = 1
        max_unavailable = 0
      }
    }

    template {
      metadata {
        labels = merge(local.labels, local.selector)
      }

      spec {
        service_account_name = kubernetes_service_account_v1.edge.metadata[0].name

        # The process fails readiness for MOQOM_API_EDGE_DRAIN, then allows
        # twenty seconds for requests in flight.
        termination_grace_period_seconds = 45

        # One replica per zone, so a zone going away leaves the other serving.
        topology_spread_constraint {
          max_skew           = 1
          topology_key       = "topology.kubernetes.io/zone"
          when_unsatisfiable = "DoNotSchedule"
          label_selector {
            match_labels = local.selector
          }
        }

        container {
          name              = "api-edge"
          image             = var.image
          image_pull_policy = "Always"

          port {
            name           = "https"
            container_port = local.https_port
          }
          port {
            name           = "http"
            container_port = local.http_port
          }
          port {
            name           = "health"
            container_port = local.health_port
          }

          dynamic "env" {
            for_each = {
              MOQOM_API_EDGE_HOSTS                = join(",", var.hosts)
              MOQOM_API_EDGE_UPSTREAM             = var.upstream
              MOQOM_API_EDGE_UPSTREAM_CA          = "${local.ca_mount}/link-ca.pem"
              MOQOM_API_EDGE_UPSTREAM_SERVER_NAME = var.upstream_server_name
              MOQOM_API_EDGE_REDIRECT             = var.redirect
              MOQOM_API_EDGE_ACME_DIRECTORY       = var.acme_directory
              MOQOM_API_EDGE_ACME_EMAIL           = var.acme_email
              MOQOM_API_EDGE_ACME_BUCKET          = google_storage_bucket.acme.name
              MOQOM_API_EDGE_HTTPS_ADDR           = ":${local.https_port}"
              MOQOM_API_EDGE_HTTP_ADDR            = ":${local.http_port}"
              MOQOM_API_EDGE_HEALTH_ADDR          = ":${local.health_port}"
            }
            content {
              name  = env.key
              value = env.value
            }
          }

          volume_mount {
            name       = "upstream-ca"
            mount_path = local.ca_mount
            read_only  = true
          }

          resources {
            requests = {
              cpu    = var.cpu
              memory = var.memory
            }
            limits = {
              cpu    = var.cpu
              memory = var.memory
            }
          }

          liveness_probe {
            http_get {
              path = "/livez"
              port = local.health_port
            }
            period_seconds    = 30
            timeout_seconds   = 5
            failure_threshold = 3
          }

          readiness_probe {
            http_get {
              path = "/healthz"
              port = local.health_port
            }
            period_seconds    = 5
            timeout_seconds   = 3
            failure_threshold = 2
          }

          security_context {
            run_as_non_root            = true
            run_as_user                = 65532
            run_as_group               = 65532
            allow_privilege_escalation = false
            read_only_root_filesystem  = true
            capabilities {
              drop = ["ALL"]
            }
          }
        }

        # Only the authority's certificate from the control plane's secret
        # (tools/sync-secrets.sh), never its keys.
        volume {
          name = "upstream-ca"
          secret {
            secret_name = var.upstream_ca_secret
            items {
              key  = "link-ca.pem"
              path = "link-ca.pem"
            }
          }
        }
      }
    }
  }

  wait_for_rollout = true

  lifecycle {
    ignore_changes = [
      metadata[0].annotations["autopilot.gke.io/resource-adjustment"],
      metadata[0].annotations["autopilot.gke.io/warden-version"],
    ]
  }
}

resource "kubernetes_pod_disruption_budget_v1" "edge" {
  count = local.enabled ? 1 : 0

  metadata {
    name      = local.name
    namespace = var.namespace
    labels    = local.labels
  }

  spec {
    max_unavailable = "1"
    selector {
      match_labels = local.selector
    }
  }
}

# A passthrough network load balancer: TCP to the pods, the handshake theirs.
# GKE's service controller opens the firewall for 80, 443 and its own health
# check, which with Local asks each node whether it holds a ready pod.
resource "kubernetes_service_v1" "edge" {
  count = local.enabled ? 1 : 0

  metadata {
    name      = local.name
    namespace = var.namespace
    labels    = local.labels
  }

  spec {
    type     = "LoadBalancer"
    selector = local.selector

    load_balancer_ip = google_compute_address.edge[0].address

    # The client's address rather than a node's: X-Forwarded-For is built
    # from it, and billing's per-address limits key on that.
    external_traffic_policy = "Local"

    port {
      name        = "https"
      port        = 443
      target_port = local.https_port
      protocol    = "TCP"
    }

    # HTTP-01 and the redirect to HTTPS.
    port {
      name        = "http"
      port        = 80
      target_port = local.http_port
      protocol    = "TCP"
    }
  }

  wait_for_load_balancer = true

  lifecycle {
    ignore_changes = [
      metadata[0].annotations["networking.gke.io/backend-service"],
      metadata[0].annotations["networking.gke.io/target-pool"],
    ]
  }
}
