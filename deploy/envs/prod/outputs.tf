output "relay_endpoints" {
  value       = local.relay_endpoints
  description = "Relay addresses by region, all serving QUIC and WebTransport on UDP/443."
}

output "edge_hostname" {
  value       = module.edge_dns.record_name
  description = "Name clients dial. Resolves per geography; see the dns-steering module for what that does and does not guarantee."
}

output "control_url" {
  value       = module.control.uri
  description = "Control plane run.app URL. Clients should use the custom domain, not this."
}

output "director_url" {
  value       = module.director.uri
  description = "Director run.app URL."
}

output "registry" {
  value       = module.registry.repository_url
  description = "Image repository."
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
  description = "Grant this roles/storage.objectCreator on a customer-owned GCS bucket for BYO-bucket recording."
}

output "recording_writer_unique_id" {
  value       = module.federation.recording_writer_unique_id
  description = "The `sub` claim a customer's AWS role trust policy must match for BYO-bucket recording to S3."
}

output "nameservers" {
  value       = module.network.dns_zone_name
  description = "Managed zone holding the edge records. Delegate to its nameservers before any of the steering above takes effect."
}

output "secrets_awaiting_population" {
  value       = keys(module.data.secret_ids)
  description = "Secret containers created empty. The relay will not serve TLS and the control plane will not mint tokens until these have versions."
}
