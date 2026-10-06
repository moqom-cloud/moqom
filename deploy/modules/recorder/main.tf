# The hosted recorder pool: workers that claim recordings from the control
# plane, join the room through a relay as a viewer, and write what they receive
# to the recording bucket. docs/RECORDING.md describes the formats and the
# lease; this is where the workers run and what they are allowed to touch.
#
# Two identities, and the split is the point. The workers write and cannot
# read, so a compromised worker cannot list or fetch any tenant's recordings.
# Reading is a separate account nothing runs as: the control plane signs
# download links as it, through IAM, and a link opens one object for minutes.

locals {
  name            = "${var.name_prefix}-recorder"
  service_account = "moqom-recorder"
  secret_mount    = "/etc/moqom-recorder"

  labels = merge(var.labels, {
    "app.kubernetes.io/name"       = local.name
    "app.kubernetes.io/component"  = "recorder"
    "app.kubernetes.io/managed-by" = "terraform"
  })
  selector = { "app.kubernetes.io/name" = local.name }
}

# --- Identities --------------------------------------------------------------

resource "google_service_account" "worker" {
  project      = var.project_id
  account_id   = local.name
  display_name = "Hosted recorder workers"
  description  = "Writes recordings to the recording bucket. Cannot read them back."
}

resource "google_service_account_iam_member" "worker_identity" {
  service_account_id = google_service_account.worker.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${var.namespace}/${local.service_account}]"
}

# Create only. A resumable upload's status query and its final chunk are both
# covered by create; nothing a worker does needs get, list or delete.
#
# Bucket-wide, because one pool serves every tenant and a condition on the
# object prefix cannot follow an assignment. Tenant separation in the bucket is
# the prefix the control plane hands out, which begins with the tenant and is
# escaped so a room name cannot climb out of it.
resource "google_storage_bucket_iam_member" "worker_writes" {
  bucket = var.recording_bucket
  role   = "roles/storage.objectCreator"
  member = google_service_account.worker.member
}

resource "google_service_account" "reader" {
  project      = var.project_id
  account_id   = "${var.name_prefix}-rec-reader"
  display_name = "Recording download links"
  description  = "Signs V4 download links for recordings. Nothing runs as this account; the control plane signs as it through IAM."
}

resource "google_storage_bucket_iam_member" "reader_reads" {
  bucket = var.recording_bucket
  role   = "roles/storage.objectViewer"
  member = google_service_account.reader.member
}

# signBlob as the reader, which is what a V4 link needs and all it needs.
resource "google_service_account_iam_member" "control_signs" {
  service_account_id = google_service_account.reader.name
  role               = "roles/iam.serviceAccountTokenCreator"
  member             = "serviceAccount:${var.control_service_account}"
}

# --- Secrets -----------------------------------------------------------------

# Containers only, as everywhere in this tree: a version's payload would sit in
# Terraform state. `moqom-recorder thumbprint --generate` makes the key and
# prints the thumbprint the control plane needs; the key goes in the first of
# these, the account credential in the second.
resource "google_secret_manager_secret" "this" {
  for_each = toset(["recorder-key", "recorder-credential"])

  project   = var.project_id
  secret_id = "${var.name_prefix}-${each.value}"
  labels    = var.labels

  replication {
    auto {}
  }
}

# --- Workers -----------------------------------------------------------------

resource "kubernetes_service_account_v1" "this" {
  metadata {
    name      = local.service_account
    namespace = var.namespace
    labels    = local.labels
    annotations = {
      "iam.gke.io/gcp-service-account" = google_service_account.worker.email
    }
  }
}

resource "kubernetes_deployment_v1" "this" {
  count = var.image == null ? 0 : 1

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

    # A worker going away hands its recording back and the next claim resumes
    # it under a new attempt, so a rollout costs each recording one handover.
    # Surging keeps the pool's capacity while that happens.
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
        service_account_name             = kubernetes_service_account_v1.this.metadata[0].name
        termination_grace_period_seconds = var.termination_grace_seconds

        topology_spread_constraint {
          max_skew           = 1
          topology_key       = "topology.kubernetes.io/zone"
          when_unsatisfiable = "ScheduleAnyway"
          label_selector {
            match_labels = local.selector
          }
        }

        container {
          name              = "recorder"
          image             = var.image
          image_pull_policy = "Always"
          args              = ["worker"]

          env {
            name  = "MOQOM_CONTROL"
            value = var.control_endpoint
          }
          env {
            name  = "MOQOM_CONTROL_SERVER_NAME"
            value = var.control_server_name
          }
          env {
            name  = "MOQOM_CONTROL_CA"
            value = "${local.secret_mount}/link-ca.pem"
          }
          env {
            name  = "MOQOM_CONTROL_TLS_CERT"
            value = "${local.secret_mount}/link-cert.pem"
          }
          env {
            name  = "MOQOM_CONTROL_TLS_KEY"
            value = "${local.secret_mount}/link-key.pem"
          }
          env {
            name  = "MOQOM_CONTROL_CREDENTIAL_FILE"
            value = "${local.secret_mount}/credential"
          }
          env {
            name  = "MOQOM_RELAY"
            value = var.relay
          }
          dynamic "env" {
            for_each = var.relay_name == null ? [] : [var.relay_name]
            content {
              name  = "MOQOM_RELAY_NAME"
              value = env.value
            }
          }
          dynamic "env" {
            for_each = var.pin_relay_certificate ? ["${local.secret_mount}/relay-cert.pem"] : []
            content {
              name  = "MOQOM_RELAY_CERT"
              value = env.value
            }
          }
          env {
            name  = "MOQOM_RECORDER_KEY"
            value = "${local.secret_mount}/recorder-key.pem"
          }
          env {
            name  = "MOQOM_RECORDER_SPOOL"
            value = "/var/spool/moqom-recorder"
          }
          env {
            name  = "MOQOM_RECORDER_SEGMENT_MIB"
            value = tostring(var.segment_mib)
          }
          # The worker's lease is in the control plane under this name, so it
          # has to be the pod's and nothing a restart would reuse.
          env {
            name = "MOQOM_WORKER_ID"
            value_from {
              field_ref {
                field_path = "metadata.name"
              }
            }
          }
          env {
            name  = "RUST_LOG"
            value = "moqom_recorder=info"
          }

          volume_mount {
            name       = "secrets"
            mount_path = local.secret_mount
            read_only  = true
          }
          volume_mount {
            name       = "spool"
            mount_path = "/var/spool/moqom-recorder"
          }

          resources {
            requests = {
              cpu               = var.cpu
              memory            = var.memory
              ephemeral-storage = var.spool_size
            }
            limits = {
              cpu               = var.cpu
              memory            = var.memory
              ephemeral-storage = var.spool_size
            }
          }

          # No probes. The worker serves nothing; a stuck one stops renewing,
          # loses its lease, and the recording moves to another worker, which
          # is the recovery a liveness restart would have bought.

          security_context {
            run_as_non_root            = true
            run_as_user                = var.run_as_user
            run_as_group               = var.run_as_user
            allow_privilege_escalation = false
            read_only_root_filesystem  = true
            capabilities {
              drop = ["ALL"]
            }
          }
        }

        volume {
          name = "secrets"
          secret {
            secret_name = var.kubernetes_secret
          }
        }

        # emptyDir rather than a persistent volume: what is in the spool when a
        # pod is lost has either been uploaded or is lost with it, and the next
        # attempt's files carry on from the next segment rather than this one.
        volume {
          name = "spool"
          empty_dir {
            size_limit = var.spool_size
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
