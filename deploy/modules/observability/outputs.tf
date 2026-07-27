output "notification_channel_ids" {
  value       = local.channels
  description = "Notification channel ids, for alert policies defined outside this module."
}

output "relay_log_bucket_id" {
  value       = google_logging_project_bucket_config.relay.id
  description = "Log bucket holding relay logs."
}

output "relay_session_error_metric" {
  value       = google_logging_metric.relay_session_errors.name
  description = "Log-based metric name for relay session errors."
}
