output "service_name" {
  value       = google_cloud_run_v2_service.this.name
  description = "Cloud Run service name."
}

output "uri" {
  value       = google_cloud_run_v2_service.this.uri
  description = "Default run.app URL. Fine for service-to-service; put a custom domain in front for clients."
}

output "service_account_email" {
  value       = google_service_account.service.email
  description = "Runtime service account email. Also the basis for the Postgres IAM username."
}

output "service_account_member" {
  value       = google_service_account.service.member
  description = "Runtime service account in IAM member form."
}
