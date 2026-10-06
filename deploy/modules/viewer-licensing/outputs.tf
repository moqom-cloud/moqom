output "backend_service" {
  value       = google_compute_backend_service.this.id
  description = "Set envs/web's viewer_licensing_backend to this to route licensing.elliottwave.studio/v1/* here."
}

output "service" {
  value = google_cloud_run_v2_service.this.name
}

output "service_account" {
  value = google_service_account.service.email
}

output "secrets" {
  value       = { for env, secret in google_secret_manager_secret.this : env => secret.secret_id }
  description = "Environment variable to secret ID. Add the values with `gcloud secrets versions add`."
}

output "firestore_database" {
  value = google_firestore_database.this.name
}
