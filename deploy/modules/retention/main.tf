# The data-retention sweep (moqom/go/cmd/moqom-retention) as a CronJob beside
# the control plane. It runs as the control plane's Kubernetes service account
# because that is the identity Cloud SQL already knows: a separate one would
# need its own IAM database user and its own grants on the same tables, which
# is two identities that must be kept in step to do one thing.
#
# What it removes and why is in docs/compliance/DATA-RETENTION.md. What it
# never touches (the ledger, tenants, the moderation log) is enforced in the
# binary, not here.

terraform {
  required_version = ">= 1.9"

  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.38"
    }
  }
}

variable "name" {
  type        = string
  description = "Name of the CronJob."
}

variable "namespace" {
  type        = string
  description = "Namespace to run in; the control plane's."
}

variable "service_account" {
  type        = string
  description = "Kubernetes service account, bound through Workload Identity to a Google service account with Cloud SQL IAM access."
}

variable "image" {
  type        = string
  description = "The compliance image (moqom/go/Dockerfile.compliance)."
}

variable "schedule" {
  type        = string
  description = "Cron (UTC)."
  default     = "41 3 * * *"
}

variable "env" {
  type        = map(string)
  description = "Connection settings: MOQOM_CONTROL_SQL_INSTANCE, PGUSER, PGDATABASE."
}

variable "dry_run" {
  type        = bool
  description = "Count what would be removed without removing it."
  default     = false
}

variable "args" {
  type        = list(string)
  description = "Extra flags, e.g. -participant-address-days=14."
  default     = []
}

resource "kubernetes_cron_job_v1" "this" {
  metadata {
    name      = var.name
    namespace = var.namespace
    labels = {
      "app.kubernetes.io/name"    = var.name
      "app.kubernetes.io/part-of" = "moqom"
    }
  }

  spec {
    schedule                      = var.schedule
    timezone                      = "Etc/UTC"
    concurrency_policy            = "Forbid"
    starting_deadline_seconds     = 3600
    successful_jobs_history_limit = 3
    failed_jobs_history_limit     = 3

    job_template {
      metadata {}
      spec {
        backoff_limit           = 1
        active_deadline_seconds = 1800

        template {
          metadata {
            labels = {
              "app.kubernetes.io/name" = var.name
            }
          }

          spec {
            service_account_name = var.service_account
            restart_policy       = "Never"

            security_context {
              run_as_non_root = true
              seccomp_profile {
                type = "RuntimeDefault"
              }
            }

            container {
              name    = "retention"
              image   = var.image
              command = ["/moqom-retention"]
              args    = concat(var.dry_run ? ["-dry-run"] : [], var.args)

              dynamic "env" {
                for_each = var.env
                content {
                  name  = env.key
                  value = env.value
                }
              }

              resources {
                requests = {
                  cpu    = "250m"
                  memory = "256Mi"
                }
                limits = {
                  cpu    = "250m"
                  memory = "256Mi"
                }
              }

              security_context {
                allow_privilege_escalation = false
                read_only_root_filesystem  = true
                capabilities {
                  drop = ["ALL"]
                }
              }
            }
          }
        }
      }
    }
  }
}

output "name" {
  value = kubernetes_cron_job_v1.this.metadata[0].name
}
