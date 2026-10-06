locals {
  instance_name = "${var.name_prefix}-pg"

  lifecycle_transitions = {
    NEARLINE = try(var.recording_lifecycle.nearline_after_days, 30)
    COLDLINE = try(var.recording_lifecycle.coldline_after_days, 90)
    ARCHIVE  = try(var.recording_lifecycle.archive_after_days, 365)
  }

  # Cloud SQL derives an IAM service account's Postgres role name by stripping
  # the `.gserviceaccount.com` suffix from the email, and the resulting name is
  # what the application must connect as. Getting this wrong produces a
  # "password authentication failed" error for an account that has no password.
  iam_usernames = {
    for email in var.iam_service_accounts :
    email => trimsuffix(email, ".gserviceaccount.com")
  }
}

resource "google_sql_database_instance" "this" {
  name    = local.instance_name
  project = var.project_id
  region  = var.region

  database_version    = var.database_version
  deletion_protection = var.deletion_protection

  encryption_key_name = var.kms_key_name

  settings {
    tier              = var.tier
    availability_type = var.availability_type
    edition           = "ENTERPRISE"
    user_labels       = var.labels

    disk_type             = "PD_SSD"
    disk_size             = var.disk_size_gb
    disk_autoresize       = true
    disk_autoresize_limit = var.disk_autoresize_limit_gb

    ip_configuration {
      # No public IP at all. The control plane reaches the instance over the
      # peered range using Cloud Run's direct VPC egress, so there is nothing
      # for an authorised-networks list to authorise.
      ipv4_enabled                                  = false
      private_network                               = var.network_id
      enable_private_path_for_google_cloud_services = true
      ssl_mode                                      = "ENCRYPTED_ONLY"
    }

    backup_configuration {
      enabled                        = true
      start_time                     = "03:00"
      location                       = null
      point_in_time_recovery_enabled = var.enable_point_in_time_recovery
      transaction_log_retention_days = var.transaction_log_retention_days

      backup_retention_settings {
        retained_backups = var.backup_retention_days
        retention_unit   = "COUNT"
      }
    }

    maintenance_window {
      day          = var.maintenance_window.day
      hour         = var.maintenance_window.hour
      update_track = var.maintenance_window.update_track
    }

    insights_config {
      query_insights_enabled  = var.query_insights
      query_string_length     = 1024
      record_application_tags = true
      record_client_address   = false
    }

    # IAM authentication is the reason no password for the application exists
    # anywhere in this configuration, including in Terraform state. Tokens are
    # short-lived and revoked by removing a role, which is a materially
    # different security posture from a password nobody can rotate without a
    # deploy.
    database_flags {
      name  = "cloudsql.iam_authentication"
      value = "on"
    }

    dynamic "database_flags" {
      for_each = var.database_flags
      content {
        name  = database_flags.key
        value = database_flags.value
      }
    }
  }

  depends_on = [var.psa_connection]

  lifecycle {
    # `prevent_destroy` takes a literal, not a variable, so it cannot follow
    # `var.deletion_protection`. Rather than have production quietly weaker than
    # it looks, this is unset here and the API-level flag is the enforcement
    # point; production environments should also carry an organisation policy or
    # a deletion lock, because a Terraform-side guard only binds Terraform.
    ignore_changes = [settings[0].disk_size]
  }
}

resource "google_sql_database" "this" {
  for_each = toset(var.databases)

  name     = each.value
  project  = var.project_id
  instance = google_sql_database_instance.this.name

  # A DROP DATABASE issued by a plan is not a recoverable event. Removing a
  # database should be a deliberate, out-of-band act.
  deletion_policy = "ABANDON"
}

resource "google_sql_user" "iam" {
  for_each = local.iam_usernames

  name     = each.value
  project  = var.project_id
  instance = google_sql_database_instance.this.name
  type     = "CLOUD_IAM_SERVICE_ACCOUNT"
}

resource "google_sql_user" "iam_user" {
  for_each = toset(var.iam_users)

  name     = each.value
  project  = var.project_id
  instance = google_sql_database_instance.this.name
  type     = "CLOUD_IAM_USER"
}

# instanceUser is what turns an access token into a Postgres login; client is
# what permits the connection itself. Neither can be scoped below the project —
# Cloud SQL exposes no resource-level IAM for them — so the containment boundary
# for the database is the project, and that is the argument for the control
# plane not sharing a project with anything experimental.
resource "google_project_iam_member" "sql_access" {
  for_each = {
    for pair in setproduct(var.iam_service_accounts, ["roles/cloudsql.client", "roles/cloudsql.instanceUser"]) :
    "${pair[0]}:${pair[1]}" => { email = pair[0], role = pair[1] }
  }

  project = var.project_id
  role    = each.value.role
  member  = "serviceAccount:${each.value.email}"
}

# --- Recording storage -------------------------------------------------------

resource "google_storage_bucket" "recordings" {
  name     = var.recording_bucket_name
  project  = var.project_id
  location = var.recording_bucket_location

  storage_class               = "STANDARD"
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  force_destroy               = var.recording_bucket_force_destroy

  # Recordings are written once and never modified. Versioning would double the
  # bill to protect against an overwrite that the relay's IAM role does not
  # permit in the first place.
  versioning {
    enabled = false
  }

  dynamic "lifecycle_rule" {
    for_each = { for class, days in local.lifecycle_transitions : class => days if days > 0 }
    content {
      condition {
        age = lifecycle_rule.value
      }
      action {
        type          = "SetStorageClass"
        storage_class = lifecycle_rule.key
      }
    }
  }

  dynamic "lifecycle_rule" {
    for_each = try(var.recording_lifecycle.delete_after_days, 0) > 0 ? [var.recording_lifecycle.delete_after_days] : []
    content {
      condition {
        age = lifecycle_rule.value
      }
      action {
        type = "Delete"
      }
    }
  }

  # Abandoned multipart uploads are invisible in the object listing and billed
  # anyway. A relay that dies mid-recording leaves exactly these.
  lifecycle_rule {
    condition {
      days_since_noncurrent_time = 7
    }
    action {
      type = "Delete"
    }
  }

  dynamic "retention_policy" {
    for_each = var.recording_retention_seconds > 0 ? [var.recording_retention_seconds] : []
    content {
      retention_period = retention_policy.value
      is_locked        = false
    }
  }

  dynamic "encryption" {
    for_each = var.kms_key_name == null ? [] : [var.kms_key_name]
    content {
      default_kms_key_name = encryption.value
    }
  }

  labels = var.labels
}

resource "google_storage_bucket_iam_member" "recording_writers" {
  for_each = toset(var.recording_writers)

  bucket = google_storage_bucket.recordings.name
  role   = "roles/storage.objectCreator"
  member = each.value
}

resource "google_storage_bucket_iam_member" "recording_readers" {
  for_each = toset(var.recording_readers)

  bucket = google_storage_bucket.recordings.name
  role   = "roles/storage.objectViewer"
  member = each.value
}

# --- Secrets -----------------------------------------------------------------

# Containers only. No `google_secret_manager_secret_version` anywhere in this
# repository, because a version's payload is stored in plaintext in Terraform
# state, and state lives in a bucket with a wider audience than the secret does.
# Populate these with `gcloud secrets versions add`, or from a CI job holding a
# federated token; the application reads the latest version at start-up.
resource "google_secret_manager_secret" "this" {
  for_each = var.secrets

  project   = var.project_id
  secret_id = each.key
  labels    = var.labels

  dynamic "replication" {
    for_each = length(var.secret_replication_locations) == 0 ? [1] : []
    content {
      auto {}
    }
  }

  dynamic "replication" {
    for_each = length(var.secret_replication_locations) == 0 ? [] : [1]
    content {
      user_managed {
        dynamic "replicas" {
          for_each = var.secret_replication_locations
          content {
            location = replicas.value
          }
        }
      }
    }
  }
}

resource "google_secret_manager_secret_iam_member" "accessors" {
  for_each = {
    for pair in flatten([
      for secret_id, members in var.secrets : [
        for member in members : { secret_id = secret_id, member = member }
      ]
    ]) : "${pair.secret_id}:${pair.member}" => pair
  }

  project   = var.project_id
  secret_id = google_secret_manager_secret.this[each.value.secret_id].secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = each.value.member
}
