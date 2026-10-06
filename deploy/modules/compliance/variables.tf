variable "project_id" {
  type        = string
  description = "Project the controls apply to."
}

variable "name_prefix" {
  type        = string
  description = "Prefix for resource names, e.g. moqom-dev."
}

variable "region" {
  type        = string
  description = "Region for the compliance job and its scheduler."
}

variable "labels" {
  type        = map(string)
  description = "Labels applied to resources that take them."
  default     = {}
}

# --- Audit logging -----------------------------------------------------------

variable "audit_log_config" {
  type        = map(list(string))
  description = <<-EOT
    Service to audit log types. Admin Activity logs are always on and cannot be
    configured; this is Data Access. allServices gets writes and admin reads;
    DATA_READ is per service because on GKE an allServices DATA_READ is every
    get, list and watch against the Kubernetes API, which is a large bill for
    no assurance. Must agree with compliance.DefaultAuditLogs, which the
    nightly check enforces.
  EOT
  default = {
    "allServices"                  = ["ADMIN_READ", "DATA_WRITE"]
    "secretmanager.googleapis.com" = ["DATA_READ"]
    "cloudsql.googleapis.com"      = ["DATA_READ"]
    "iam.googleapis.com"           = ["DATA_READ"]
    "storage.googleapis.com"       = ["DATA_READ"]
  }
}

variable "audit_log_exempted_members" {
  type        = list(string)
  description = "Principals exempt from DATA_READ logging, for automated readers that would otherwise dominate the volume. Each exemption is a hole in the record, so keep it empty unless the bill says otherwise."
  default     = []
}

variable "audit_log_retention_days" {
  type        = number
  description = "Retention for the audit log bucket. 400 matches _Required and covers a twelve-month audit period with margin."
  default     = 400
}

variable "lock_audit_log_bucket" {
  type        = bool
  description = "Lock the audit log bucket's retention. IRREVERSIBLE: a locked bucket cannot be shortened or deleted until every entry ages out. Turn on once the configuration has settled."
  default     = false
}

variable "exclude_data_access_from_default" {
  type        = bool
  description = "Stop Data Access logs also landing in _Default (30 days). They are kept in the audit bucket; storing them twice doubles the bill."
  default     = true
}

variable "app_log_namespaces" {
  type        = list(string)
  description = "GKE namespaces whose container logs are copied into the audit bucket. The control plane logs every moderation action and key operation; 30 days in _Default is shorter than an audit period."
  default     = ["moqom"]
}

# --- Evidence ----------------------------------------------------------------

variable "evidence_bucket_name" {
  type        = string
  description = "Globally unique name for the evidence bucket."
}

variable "evidence_bucket_location" {
  type        = string
  description = "Location for the evidence bucket."
  default     = "US"
}

variable "evidence_retention_days" {
  type        = number
  description = "Objects cannot be deleted or overwritten until they are this old."
  default     = 400
}

variable "lock_evidence_retention" {
  type        = bool
  description = "Lock the evidence bucket's retention policy (Bucket Lock). IRREVERSIBLE: the bucket cannot be deleted, nor the period shortened, until every object ages out. This is what makes the evidence tamper-evident to an auditor; turn it on before the audit period starts."
  default     = false
}

variable "evidence_delete_after_days" {
  type        = number
  description = "Delete evidence after this many days. Three years covers an audit period plus the one before it plus a dispute."
  default     = 1095
}

variable "evidence_writers" {
  type        = list(string)
  description = "Members, besides the compliance job, that may add evidence (e.g. the Cloud Build service account, for SBOMs and scan output). Create only: no member here can delete or overwrite."
  default     = []
}

# --- Alerting ----------------------------------------------------------------

variable "notification_emails" {
  type        = list(string)
  description = "Addresses that receive security and compliance alerts."
  default     = ["elliott@moqom.cloud"]
}

variable "alert_enabled" {
  type        = bool
  description = "Create the alert policies. The metrics are created either way, so history exists when alerts are turned on."
  default     = true
}

variable "expected_secret_accessors" {
  type        = list(string)
  description = "Principal emails expected to read Secret Manager payloads (the relays at boot, the control plane). A read by anyone else alerts."
  default     = []
}

variable "control_namespace" {
  type        = string
  description = "Namespace the control plane runs in."
  default     = "moqom"
}

variable "control_error_threshold" {
  type        = number
  description = "Control-plane ERROR log lines in five minutes that fire an alert."
  default     = 10
}

variable "lb_5xx_threshold" {
  type        = number
  description = "Load balancer 5xx responses in five minutes that fire an alert."
  default     = 25
}

# --- Nightly check -----------------------------------------------------------

variable "compliance_image" {
  type        = string
  description = "Image for the compliance job (moqom/go/Dockerfile.compliance), pinned by digest. Null creates everything except the job and its schedule."
  default     = null
}

variable "compliance_schedule" {
  type        = string
  description = "Cron (UTC) for the check. Twice a day so the missed-run alert can wait a full day without racing the schedule."
  default     = "17 2,14 * * *"
}

variable "public_buckets" {
  type        = list(string)
  description = "Buckets that are public on purpose. Anything else public is CRITICAL."
  default     = []
}

variable "allowed_owners" {
  type        = list(string)
  description = "Members permitted roles/owner on the project."
  default     = ["user:elliott@moqom.cloud"]
}

variable "registry_locations" {
  type        = list(string)
  description = "Artifact Registry locations the check inspects."
  default     = ["us"]
}

variable "require_deletion_protection" {
  type        = bool
  description = "Treat a database without deletion protection as HIGH rather than LOW. True in prod."
  default     = false
}

variable "job_deletion_protection" {
  type        = bool
  description = "Deletion protection on the Cloud Run job."
  default     = false
}
