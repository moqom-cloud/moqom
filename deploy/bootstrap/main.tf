resource "google_storage_bucket" "state" {
  name     = var.state_bucket_name
  project  = var.project_id
  location = var.state_bucket_location

  # Terraform's own locking is advisory and lives in the bucket. Versioning is
  # the only thing that makes a corrupted or truncated state recoverable, and
  # the recovery is "copy the previous generation back", so keep enough
  # generations to survive a bad week rather than a bad hour.
  versioning {
    enabled = true
  }

  lifecycle_rule {
    condition {
      num_newer_versions = 30
    }
    action {
      type = "Delete"
    }
  }

  lifecycle_rule {
    condition {
      days_since_noncurrent_time = 365
    }
    action {
      type = "Delete"
    }
  }

  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"

  # State holds generated passwords, connection strings, and every resource
  # name in the estate. Treat deletion of this bucket as a two-step operation.
  force_destroy = false

  labels = var.labels
}

resource "google_storage_bucket_iam_member" "admins" {
  for_each = toset(var.state_admins)

  bucket = google_storage_bucket.state.name
  role   = "roles/storage.admin"
  member = each.value
}

# objectAdmin rather than admin: CI needs to write and delete state objects but
# has no business changing the bucket's retention, IAM, or lifecycle rules.
resource "google_storage_bucket_iam_member" "writers" {
  for_each = toset(var.state_writers)

  bucket = google_storage_bucket.state.name
  role   = "roles/storage.objectAdmin"
  member = each.value
}
