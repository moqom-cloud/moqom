# Compliance controls that are infrastructure: what gets recorded, how long
# the record lasts, who is told when something changes, and the nightly job
# that checks the rest. docs/compliance/README.md maps each resource here to
# the controls it evidences.
#
# Two settings are deliberately one-way and off by default:
# lock_audit_log_bucket and lock_evidence_retention. Both are what an auditor
# wants to see, and both make the bucket undeletable for a year. Turn them on
# when the configuration has stopped changing, not before.

locals {
  audit_bucket_id = "${var.name_prefix}-audit"
  job_name        = "${var.name_prefix}-compliance"
  job_enabled     = var.compliance_image != null
  channels        = [for c in google_monitoring_notification_channel.email : c.id]

  # Content-free filters: the job, the relays and the control plane all log
  # structured events with an "event" field, and the alert policies match on
  # that rather than on message text that someone might reword.
  control = "resource.type=\"k8s_container\" AND resource.labels.namespace_name=\"${var.control_namespace}\""
}

resource "google_project_service" "this" {
  for_each = toset(["cloudscheduler.googleapis.com", "run.googleapis.com"])

  project            = var.project_id
  service            = each.value
  disable_on_destroy = false
}

# --- Audit logging -----------------------------------------------------------

resource "google_project_iam_audit_config" "this" {
  for_each = var.audit_log_config

  project = var.project_id
  service = each.key

  dynamic "audit_log_config" {
    for_each = each.value
    content {
      log_type         = audit_log_config.value
      exempted_members = audit_log_config.value == "DATA_READ" ? var.audit_log_exempted_members : []
    }
  }
}

# _Required already keeps Admin Activity and System Event logs for 400 days,
# locked, at no charge. This bucket holds what _Required does not: Data Access,
# policy denials, and the application's own audit trail.
resource "google_logging_project_bucket_config" "audit" {
  project        = var.project_id
  location       = "global"
  bucket_id      = local.audit_bucket_id
  retention_days = var.audit_log_retention_days
  locked         = var.lock_audit_log_bucket
  description    = "Data Access audit logs, policy denials and control-plane logs. Retention matches _Required."
}

resource "google_logging_project_sink" "audit" {
  project     = var.project_id
  name        = "${var.name_prefix}-audit"
  destination = "logging.googleapis.com/${google_logging_project_bucket_config.audit.id}"

  filter = join(" OR ", concat(
    [
      "log_id(\"cloudaudit.googleapis.com/data_access\")",
      "log_id(\"cloudaudit.googleapis.com/policy\")",
      "(resource.type=\"cloud_run_job\" AND resource.labels.job_name=\"${local.job_name}\")",
    ],
    [for ns in var.app_log_namespaces : "(resource.type=\"k8s_container\" AND resource.labels.namespace_name=\"${ns}\")"],
  ))

  unique_writer_identity = true
}

resource "google_logging_project_exclusion" "data_access" {
  count = var.exclude_data_access_from_default ? 1 : 0

  project     = var.project_id
  name        = "${var.name_prefix}-data-access-in-audit-bucket"
  description = "Data Access logs are kept in ${local.audit_bucket_id}; this stops them also being billed in _Default."
  filter      = "log_id(\"cloudaudit.googleapis.com/data_access\")"

  depends_on = [google_logging_project_sink.audit]
}

# --- Evidence ----------------------------------------------------------------

resource "google_storage_bucket" "evidence" {
  project                     = var.project_id
  name                        = var.evidence_bucket_name
  location                    = var.evidence_bucket_location
  storage_class               = "STANDARD"
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  force_destroy               = false
  labels                      = merge(var.labels, { purpose = "compliance-evidence" })

  versioning {
    enabled = true
  }

  # Nothing written here can be deleted or replaced before it is
  # evidence_retention_days old, by anyone, including the project owner.
  # Locked, not even the policy can be shortened.
  retention_policy {
    retention_period = var.evidence_retention_days * 86400
    is_locked        = var.lock_evidence_retention
  }

  lifecycle_rule {
    condition {
      age = 30
    }
    action {
      type          = "SetStorageClass"
      storage_class = "NEARLINE"
    }
  }

  lifecycle_rule {
    condition {
      age = 365
    }
    action {
      type          = "SetStorageClass"
      storage_class = "ARCHIVE"
    }
  }

  lifecycle_rule {
    condition {
      age = var.evidence_delete_after_days
    }
    action {
      type = "Delete"
    }
  }

  lifecycle {
    prevent_destroy = true
  }
}

resource "google_storage_bucket_iam_member" "evidence_writer" {
  for_each = toset(var.evidence_writers)

  bucket = google_storage_bucket.evidence.name
  role   = "roles/storage.objectCreator"
  member = each.value
}

# --- Alerting ----------------------------------------------------------------

resource "google_monitoring_notification_channel" "email" {
  for_each = toset(var.notification_emails)

  project      = var.project_id
  display_name = "${var.name_prefix} security: ${each.value}"
  type         = "email"
  labels = {
    email_address = each.value
  }
}

locals {
  metrics = {
    iam-changes = {
      description = "IAM policy, role and service account key changes anywhere in the project."
      filter      = "log_id(\"cloudaudit.googleapis.com/activity\") AND protoPayload.methodName=~\"(?i)(setiampolicy|setiampermissions|createserviceaccountkey|uploadserviceaccountkey|deleteserviceaccountkey|createserviceaccount$|createrole|updaterole|deleterole)\""
      severity    = "WARNING"
      threshold   = 0
      doc         = "Someone changed who can do what. Check the principal in the audit log against CHANGE-MANAGEMENT.md: an expected change has a matching commit in deploy/."
      labels      = {}
    }
    logging-tamper = {
      description = "Changes to log sinks, buckets, exclusions and Data Access configuration, and to the evidence bucket's settings."
      filter      = "log_id(\"cloudaudit.googleapis.com/activity\") AND ((protoPayload.serviceName=\"logging.googleapis.com\" AND protoPayload.methodName=~\"(?i)(sink|bucket|exclusion|deletelog)\" AND NOT protoPayload.methodName=~\"(?i)(get|list)\") OR (protoPayload.methodName=\"SetIamPolicy\" AND protoPayload.serviceData.policyDelta.auditConfigDeltas:*) OR (protoPayload.serviceName=\"storage.googleapis.com\" AND protoPayload.methodName=\"storage.buckets.update\" AND resource.labels.bucket_name=\"${var.evidence_bucket_name}\"))"
      severity    = "CRITICAL"
      threshold   = 0
      doc         = "The record itself changed. Treat as SEV-1 until the change is traced to a reviewed commit (INCIDENT-RESPONSE.md)."
      labels      = {}
    }
    secret-access-unexpected = {
      description = "Secret payload reads by principals outside the expected set."
      filter = join(" AND ", concat(
        ["log_id(\"cloudaudit.googleapis.com/data_access\")", "protoPayload.serviceName=\"secretmanager.googleapis.com\"", "protoPayload.methodName=~\"AccessSecretVersion$\""],
        [for p in var.expected_secret_accessors : "protoPayload.authenticationInfo.principalEmail!=\"${p}\""],
      ))
      severity  = "ERROR"
      threshold = 0
      doc       = "A secret was read by someone other than the workloads that use it. If it was you (sync-secrets, debugging), note it; otherwise rotate the secret (POLICIES.md, key management) and open an incident."
      labels    = { principal = "EXTRACT(protoPayload.authenticationInfo.principalEmail)" }
    }
    firewall-changes = {
      description = "VPC firewall rule inserts, patches and deletes. GKE creates k8s-fw rules for LoadBalancer Services, so a deploy can trigger this."
      filter      = "log_id(\"cloudaudit.googleapis.com/activity\") AND resource.type=\"gce_firewall_rule\" AND protoPayload.methodName=~\"compute.firewalls.(insert|patch|update|delete)\""
      severity    = "WARNING"
      threshold   = 0
      doc         = "The network perimeter changed. Expected after a terraform apply or a Service change; otherwise investigate (NETWORK-SECURITY.md)."
      labels      = { principal = "EXTRACT(protoPayload.authenticationInfo.principalEmail)" }
    }
    stripe-webhook-rejected = {
      description = "Stripe webhooks rejected for a bad signature."
      filter      = "${local.control} AND jsonPayload.event=\"stripe_webhook_signature_rejected\""
      severity    = "WARNING"
      threshold   = 3
      doc         = "Webhooks are failing signature checks. Either the signing secret is out of date after a rotation, or someone is forging payment events. Compare with the Stripe dashboard's webhook delivery log."
      labels      = {}
    }
    control-errors = {
      description = "ERROR-level log lines from the control plane."
      filter      = "${local.control} AND jsonPayload.level=\"ERROR\""
      severity    = "ERROR"
      threshold   = var.control_error_threshold
      doc         = "The control plane is failing requests. Check its logs and Cloud SQL health (BCP-DR.md)."
      labels      = {}
    }
    compliance-drift = {
      description = "New findings at or above the alert threshold since the previous compliance report."
      filter      = "resource.type=\"cloud_run_job\" AND resource.labels.job_name=\"${local.job_name}\" AND jsonPayload.event=\"compliance_drift\""
      severity    = "ERROR"
      threshold   = 0
      doc         = "The nightly check found something new. The report is in the evidence bucket under compliance/reports/; each finding names the control it affects."
      labels      = { rule = "EXTRACT(jsonPayload.rule)" }
    }
    compliance-summary = {
      description = "Completed compliance runs. Its absence is the alert."
      filter      = "resource.type=\"cloud_run_job\" AND resource.labels.job_name=\"${local.job_name}\" AND jsonPayload.event=\"compliance_summary\""
      severity    = null
      threshold   = null
      doc         = null
      labels      = {}
    }
    retention-failed = {
      description = "Data retention sweeps that failed."
      filter      = "${local.control} AND jsonPayload.event=\"retention_sweep_failed\""
      severity    = "ERROR"
      threshold   = 0
      doc         = "Personal data is being kept past its retention period (DATA-RETENTION.md). Check the moqom-retention CronJob's logs."
      labels      = {}
    }
  }
}

resource "google_logging_metric" "this" {
  for_each = local.metrics

  project     = var.project_id
  name        = "${var.name_prefix}-${each.key}"
  description = each.value.description
  filter      = each.value.filter

  metric_descriptor {
    metric_kind = "DELTA"
    value_type  = "INT64"
    unit        = "1"

    dynamic "labels" {
      for_each = lookup(each.value, "labels", {})
      content {
        key        = labels.key
        value_type = "STRING"
      }
    }
  }

  label_extractors = lookup(each.value, "labels", {})
}

resource "google_monitoring_alert_policy" "metric" {
  for_each = var.alert_enabled ? { for k, v in local.metrics : k => v if v.threshold != null } : {}

  project               = var.project_id
  display_name          = "${var.name_prefix}: ${each.key}"
  combiner              = "OR"
  severity              = each.value.severity
  notification_channels = local.channels

  conditions {
    display_name = each.value.description
    condition_threshold {
      # Monitoring refuses a filter without a resource type. These metrics count
      # entries from pods, relay VMs, audit logs and the compliance job alike.
      filter          = "metric.type=\"logging.googleapis.com/user/${google_logging_metric.this[each.key].name}\" AND resource.type=one_of(\"k8s_container\",\"gce_instance\",\"audited_resource\",\"cloud_run_job\",\"global\",\"project\")"
      comparison      = "COMPARISON_GT"
      threshold_value = each.value.threshold
      duration        = "0s"
      aggregations {
        alignment_period   = "300s"
        per_series_aligner = "ALIGN_SUM"
      }
      trigger {
        count = 1
      }
    }
  }

  alert_strategy {
    auto_close = "1800s"
  }

  documentation {
    mime_type = "text/markdown"
    content   = each.value.doc
  }
}

resource "google_monitoring_alert_policy" "lb_5xx" {
  count = var.alert_enabled ? 1 : 0

  project               = var.project_id
  display_name          = "${var.name_prefix}: load balancer 5xx"
  combiner              = "OR"
  severity              = "ERROR"
  notification_channels = local.channels

  conditions {
    display_name = "HTTPS load balancer 5xx responses"
    condition_threshold {
      filter          = "metric.type=\"loadbalancing.googleapis.com/https/request_count\" AND resource.type=\"https_lb_rule\" AND metric.labels.response_code_class=500"
      comparison      = "COMPARISON_GT"
      threshold_value = var.lb_5xx_threshold
      duration        = "0s"
      aggregations {
        alignment_period     = "300s"
        per_series_aligner   = "ALIGN_SUM"
        cross_series_reducer = "REDUCE_SUM"
        group_by_fields      = ["resource.label.forwarding_rule_name"]
      }
    }
  }

  documentation {
    mime_type = "text/markdown"
    content   = "A load balancer is returning server errors. Check the backend it names."
  }
}

# --- Nightly check -----------------------------------------------------------

resource "google_service_account" "compliance" {
  project      = var.project_id
  account_id   = local.job_name
  display_name = "Compliance check (read-only)"
  description  = "Reads project configuration and writes compliance reports. Cannot change anything it inspects."
}

# roles/viewer reads configuration but not secret payloads, object contents or
# table data; securityReviewer adds the IAM policies viewer cannot list. If
# either falls short the report says so (COL-001) rather than passing quietly.
resource "google_project_iam_member" "compliance" {
  for_each = toset(["roles/viewer", "roles/iam.securityReviewer"])

  project = var.project_id
  role    = each.value
  member  = google_service_account.compliance.member
}

resource "google_storage_bucket_iam_member" "compliance" {
  for_each = toset(["roles/storage.objectCreator", "roles/storage.objectViewer"])

  bucket = google_storage_bucket.evidence.name
  role   = each.value
  member = google_service_account.compliance.member
}

resource "google_cloud_run_v2_job" "compliance" {
  count = local.job_enabled ? 1 : 0

  project             = var.project_id
  name                = local.job_name
  location            = var.region
  deletion_protection = var.job_deletion_protection
  labels              = var.labels

  template {
    task_count = 1
    template {
      service_account = google_service_account.compliance.email
      timeout         = "900s"
      max_retries     = 1

      containers {
        image = var.compliance_image
        args = [
          "-project=${var.project_id}",
          "-evidence-bucket=${google_storage_bucket.evidence.name}",
          "-audit-log-bucket=${local.audit_bucket_id}",
          "-public-buckets=${join(",", var.public_buckets)}",
          "-allowed-owners=${join(",", var.allowed_owners)}",
          "-registry-locations=${join(",", var.registry_locations)}",
          "-require-deletion-protection=${var.require_deletion_protection}",
          "-domains=${join(",", var.domains)}",
        ]
        resources {
          limits = {
            cpu    = "1"
            memory = "512Mi"
          }
        }
      }
    }
  }

  depends_on = [google_project_service.this, google_project_iam_member.compliance]
}

resource "google_service_account" "scheduler" {
  count = local.job_enabled ? 1 : 0

  project      = var.project_id
  account_id   = "${var.name_prefix}-compliance-cron"
  display_name = "Compliance check scheduler"
  description  = "Starts the compliance job. Can do nothing else."
}

resource "google_cloud_run_v2_job_iam_member" "scheduler" {
  count = local.job_enabled ? 1 : 0

  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_job.compliance[0].name
  role     = "roles/run.invoker"
  member   = google_service_account.scheduler[0].member
}

resource "google_cloud_scheduler_job" "compliance" {
  count = local.job_enabled ? 1 : 0

  project          = var.project_id
  region           = var.region
  name             = local.job_name
  description      = "Runs the compliance check."
  schedule         = var.compliance_schedule
  time_zone        = "Etc/UTC"
  attempt_deadline = "320s"

  retry_config {
    retry_count = 1
  }

  http_target {
    http_method = "POST"
    uri         = "https://run.googleapis.com/v2/projects/${var.project_id}/locations/${var.region}/jobs/${local.job_name}:run"

    oauth_token {
      service_account_email = google_service_account.scheduler[0].email
      scope                 = "https://www.googleapis.com/auth/cloud-platform"
    }
  }

  depends_on = [google_cloud_run_v2_job_iam_member.scheduler]
}

resource "google_monitoring_alert_policy" "job_failed" {
  count = var.alert_enabled && local.job_enabled ? 1 : 0

  project               = var.project_id
  display_name          = "${var.name_prefix}: compliance check failed"
  combiner              = "OR"
  severity              = "ERROR"
  notification_channels = local.channels

  conditions {
    display_name = "Compliance job execution failed"
    condition_threshold {
      filter          = "metric.type=\"run.googleapis.com/job/completed_execution_count\" AND resource.type=\"cloud_run_job\" AND resource.labels.job_name=\"${local.job_name}\" AND metric.labels.result=\"failed\""
      comparison      = "COMPARISON_GT"
      threshold_value = 0
      duration        = "0s"
      aggregations {
        alignment_period   = "300s"
        per_series_aligner = "ALIGN_SUM"
      }
    }
  }

  # A check that stops running looks exactly like a check that keeps passing.
  conditions {
    display_name = "No compliance report in 23 hours"
    condition_absent {
      filter   = "metric.type=\"logging.googleapis.com/user/${google_logging_metric.this["compliance-summary"].name}\""
      duration = "82800s"
      aggregations {
        alignment_period   = "3600s"
        per_series_aligner = "ALIGN_SUM"
      }
    }
  }

  documentation {
    mime_type = "text/markdown"
    content   = "The compliance check failed or has not reported. Until it runs, controls marked Automated are unverified. Check the job's execution logs in Cloud Run."
  }
}
