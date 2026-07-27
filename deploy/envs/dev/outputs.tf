output "relay_endpoint" {
  value       = module.relay.external_ip
  description = "Address to dial on UDP/443."
}

output "control_url" {
  value       = module.control.uri
  description = "Control plane run.app URL."
}

output "director_url" {
  value       = module.director.uri
  description = "Director run.app URL."
}

output "registry" {
  value       = module.registry.repository_url
  description = "Push images here."
}

output "github_workload_identity_provider" {
  value       = module.federation.github_provider_name
  description = "Feed to google-github-actions/auth as `workload_identity_provider`."
}

output "ci_service_account" {
  value       = module.federation.deployer_service_account_email
  description = "Feed to google-github-actions/auth as `service_account`."
}

output "recording_writer_service_account" {
  value       = module.federation.recording_writer_email
  description = "Identity a customer grants write access to their own bucket."
}

output "secrets_awaiting_population" {
  value       = keys(module.data.secret_ids)
  description = "Secret containers created empty. Nothing that reads them will start until they have a version."
}
