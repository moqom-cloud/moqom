output "pool_name" {
  value       = google_iam_workload_identity_pool.this.name
  description = "Fully qualified pool name, for building principalSet identifiers."
}

output "github_provider_name" {
  value       = one(google_iam_workload_identity_pool_provider.github[*].name)
  description = "Value for the `workload_identity_provider` input of google-github-actions/auth."
}

output "deployer_service_account_email" {
  value       = one(google_service_account.deployer[*].email)
  description = "Service account GitHub Actions impersonates."
}

output "deployer_service_account_member" {
  value       = one(google_service_account.deployer[*].member)
  description = "CI deployer in IAM member form, for bucket and repository bindings."
}

output "byoc_service_account_email" {
  value       = one(google_service_account.byoc[*].email)
  description = "Service account self-hosted relays outside GCP impersonate."
}

output "recording_writer_email" {
  value       = one(google_service_account.recording_writer[*].email)
  description = "Grant this roles/storage.objectCreator on a customer-owned GCS bucket to enable BYO-bucket recording."
}

output "recording_writer_unique_id" {
  value       = one(google_service_account.recording_writer[*].unique_id)
  description = "Numeric id of the recording writer. This is the `sub` claim in its Google-issued OIDC token, and the value an AWS role trust policy must match for BYO-bucket recording to S3."
}

output "aws_provider_names" {
  value       = { for k, p in google_iam_workload_identity_pool_provider.aws : k => p.name }
  description = "AWS federation providers, keyed by the name given in `aws_accounts`."
}

output "oidc_provider_names" {
  value       = { for k, p in google_iam_workload_identity_pool_provider.oidc : k => p.name }
  description = "Generic OIDC federation providers, keyed by the name given in `oidc_providers`."
}
