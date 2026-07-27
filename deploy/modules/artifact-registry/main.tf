resource "google_artifact_registry_repository" "this" {
  project       = var.project_id
  location      = var.location
  repository_id = var.repository_id
  description   = var.description
  format        = "DOCKER"
  labels        = var.labels

  docker_config {
    immutable_tags = var.immutable_tags
  }

  cleanup_policy_dry_run = var.cleanup_policy_dry_run

  cleanup_policy {
    id     = "keep-tagged-releases"
    action = "KEEP"
    most_recent_versions {
      keep_count = var.keep_tagged_versions
    }
  }

  cleanup_policy {
    id     = "delete-untagged"
    action = "DELETE"
    condition {
      tag_state  = "UNTAGGED"
      older_than = var.untagged_retention
    }
  }
}

resource "google_artifact_registry_repository_iam_member" "readers" {
  for_each = toset(var.readers)

  project    = google_artifact_registry_repository.this.project
  location   = google_artifact_registry_repository.this.location
  repository = google_artifact_registry_repository.this.name
  role       = "roles/artifactregistry.reader"
  member     = each.value
}

# writer, not repoAdmin: CI pushes images and has no business deleting them or
# editing the cleanup policy that protects the digests production is pinned to.
resource "google_artifact_registry_repository_iam_member" "writers" {
  for_each = toset(var.writers)

  project    = google_artifact_registry_repository.this.project
  location   = google_artifact_registry_repository.this.location
  repository = google_artifact_registry_repository.this.name
  role       = "roles/artifactregistry.writer"
  member     = each.value
}
