output "evidence_bucket" {
  description = "Bucket holding compliance reports, SBOMs and scan output."
  value       = google_storage_bucket.evidence.name
}

output "audit_log_bucket" {
  description = "Log bucket holding Data Access and application audit logs."
  value       = google_logging_project_bucket_config.audit.bucket_id
}

output "notification_channels" {
  description = "Notification channel IDs, for other modules' alert policies."
  value       = local.channels
}

output "compliance_service_account" {
  description = "Identity of the compliance job."
  value       = google_service_account.compliance.email
}

output "compliance_job" {
  description = "Cloud Run job name, or null if no image was given."
  value       = local.job_enabled ? google_cloud_run_v2_job.compliance[0].name : null
}
