output "repository_id" {
  value       = google_artifact_registry_repository.this.repository_id
  description = "Repository short name."
}

output "repository_name" {
  value       = google_artifact_registry_repository.this.name
  description = "Repository resource name, for IAM bindings made elsewhere."
}

output "location" {
  value       = google_artifact_registry_repository.this.location
  description = "Repository location."
}

output "registry_host" {
  value       = "${google_artifact_registry_repository.this.location}-docker.pkg.dev"
  description = "Docker registry host, for `docker-credential-gcr configure-docker`."
}

output "repository_url" {
  value       = "${google_artifact_registry_repository.this.location}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.this.repository_id}"
  description = "Prefix to push images to."
}

output "repository_ref" {
  value = {
    project_id = var.project_id
    location   = google_artifact_registry_repository.this.location
    repository = google_artifact_registry_repository.this.repository_id
  }
  description = "Shape the relay module's `artifact_registry_repository` expects."
}
