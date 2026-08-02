output "relay_endpoints" {
  value       = { for region, relay in module.relay : region => relay.external_ip }
  description = "Address to dial on UDP/443, per region."
}

output "control_endpoint" {
  value       = module.control.endpoint
  description = "What a relay in this VPC puts in MOQOM_CONTROL_ENDPOINT. Internal, so nothing outside the VPC can reach it."
}

output "cluster" {
  value = {
    name     = module.gke.cluster_name
    location = module.gke.location
  }
  description = "Cluster to point kubectl at: gcloud container clusters get-credentials NAME --region LOCATION."
}

output "control_database_user" {
  value       = module.data.iam_usernames[module.gke.workload_service_accounts["control"]]
  description = "Postgres role the control plane logs in as. Cloud SQL derives it from the service account email, and it is the value PGUSER must carry."
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
