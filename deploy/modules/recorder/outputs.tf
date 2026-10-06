output "worker_service_account" {
  value       = google_service_account.worker.email
  description = "The account the workers run as."
}

output "signer_account" {
  value       = google_service_account.reader.email
  description = "Set as the control plane's MOQOM_RECORDING_SIGNER_ACCOUNT."
}

output "secret_ids" {
  value       = { for name, secret in google_secret_manager_secret.this : name => secret.secret_id }
  description = "Secret Manager entries to populate: the recorder key and the worker credential."
}
