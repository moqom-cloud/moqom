output "state_bucket_name" {
  value       = google_storage_bucket.state.name
  description = "Name to put in each environment's `backend \"gcs\"` block."
}

output "state_bucket_url" {
  value       = google_storage_bucket.state.url
  description = "gs:// URL of the state bucket."
}
