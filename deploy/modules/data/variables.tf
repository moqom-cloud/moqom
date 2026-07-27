variable "project_id" {
  type        = string
  description = "Project that owns the database and buckets."
}

variable "region" {
  type        = string
  description = "Region for the Cloud SQL instance."
}

variable "name_prefix" {
  type        = string
  description = "Prefix for every resource name, e.g. moqom-prod."
}

variable "network_id" {
  type        = string
  description = "VPC the Cloud SQL instance is peered into for its private IP."
}

variable "psa_connection" {
  type        = string
  description = "Handle from the network module's `psa_connection`. Only used to order creation: an instance built before the peering exists gets a public IP or fails outright."
}

# --- Cloud SQL ---------------------------------------------------------------

variable "database_version" {
  type        = string
  description = "Cloud SQL Postgres version."
  default     = "POSTGRES_16"
}

variable "tier" {
  type        = string
  description = "Machine tier, e.g. db-custom-2-7680."
}

variable "availability_type" {
  type        = string
  description = "ZONAL or REGIONAL. REGIONAL doubles the cost and is the difference between a zone failure being a blip and being an outage of the entire control plane."

  validation {
    condition     = contains(["ZONAL", "REGIONAL"], var.availability_type)
    error_message = "availability_type must be ZONAL or REGIONAL."
  }
}

variable "disk_size_gb" {
  type        = number
  description = "Initial disk size. Cloud SQL disks grow and never shrink, so start small and let autoresize do the work."
  default     = 20
}

variable "disk_autoresize_limit_gb" {
  type        = number
  description = "Ceiling for automatic disk growth. Zero means unlimited, which turns a runaway write loop into an unbounded bill."
  default     = 500
}

variable "backup_retention_days" {
  type        = number
  description = "Retained automated backups."
  default     = 14
}

variable "transaction_log_retention_days" {
  type        = number
  description = "Write-ahead log retention, which sets how far back point-in-time recovery can reach. Cannot exceed backup retention."
  default     = 7
}

variable "enable_point_in_time_recovery" {
  type        = bool
  description = "Keep write-ahead logs for PITR. The failure this covers is a bad migration, which is more likely than losing the instance."
  default     = true
}

variable "maintenance_window" {
  type = object({
    day          = number
    hour         = number
    update_track = string
  })
  description = "Weekly maintenance window: day 1-7 with Monday as 1, hour 0-23 UTC, update_track `stable` or `canary`."
  default = {
    day          = 2
    hour         = 6
    update_track = "stable"
  }
}

variable "databases" {
  type        = list(string)
  description = "Logical databases to create."
  default     = ["moqom"]
}

variable "iam_service_accounts" {
  type        = list(string)
  description = "Service account emails that authenticate to Postgres with IAM rather than a password. See the comment in main.tf about the username mangling."
  default     = []
}

variable "database_flags" {
  type        = map(string)
  description = "Extra Postgres flags. `cloudsql.iam_authentication` is set unconditionally and cannot be overridden here."
  default     = {}
}

variable "query_insights" {
  type        = bool
  description = "Enable Query Insights. Cheap, and the alternative is diagnosing a slow endpoint from application logs."
  default     = true
}

variable "deletion_protection" {
  type        = bool
  description = "Block deletion of the instance. Two flags govern this — the API-level one here and Terraform's own `lifecycle.prevent_destroy`, which cannot be set from a variable — so read the comment in main.tf before relying on it."
}

# --- Recording storage -------------------------------------------------------

variable "recording_bucket_name" {
  type        = string
  description = "Globally unique name for the managed recording bucket."
}

variable "recording_bucket_location" {
  type        = string
  description = "Bucket location. Multi-region for durability, or a single region to keep recordings inside one jurisdiction; that is a compliance decision, not a technical one."
}

variable "recording_lifecycle" {
  type = object({
    nearline_after_days = optional(number, 30)
    coldline_after_days = optional(number, 90)
    archive_after_days  = optional(number, 365)
    delete_after_days   = optional(number, 0)
  })
  description = "Storage class transitions in days since object creation. `delete_after_days = 0` disables deletion. Transitions have minimum storage durations attached, so moving an object to Coldline and deleting it a week later costs more than leaving it on Standard."
  default     = {}
}

variable "recording_retention_seconds" {
  type        = number
  description = "Bucket retention policy in seconds. Zero disables it. Non-zero makes objects undeletable for the period, which is what a legal-hold offering needs and a liability otherwise."
  default     = 0
}

variable "recording_bucket_force_destroy" {
  type        = bool
  description = "Allow Terraform to delete a bucket that still has objects in it. Never true in production."
  default     = false
}

variable "recording_writers" {
  type        = list(string)
  description = "IAM members allowed to create objects in the recording bucket."
  default     = []
}

variable "recording_readers" {
  type        = list(string)
  description = "IAM members allowed to read recordings, e.g. the control plane issuing signed URLs."
  default     = []
}

variable "kms_key_name" {
  type        = string
  description = "Customer-managed encryption key for the bucket and the SQL instance. Null uses Google-managed keys."
  default     = null
}

# --- Secrets -----------------------------------------------------------------

variable "secrets" {
  type        = map(list(string))
  description = "Secret containers to create, keyed by secret id, with the list of IAM members granted accessor on each. No versions are created; see the comment in main.tf."
  default     = {}
}

variable "secret_replication_locations" {
  type        = list(string)
  description = "Regions to replicate secrets to. Empty means automatic replication, which is the right default unless a data residency rule says otherwise."
  default     = []
}

variable "labels" {
  type        = map(string)
  description = "Labels applied to resources that accept them."
  default     = {}
}
