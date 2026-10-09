output "relay_endpoints" {
  value       = { for region, relay in module.relay : region => relay.external_ip }
  description = "Address to dial on UDP/443, per region."
}

output "relay_endpoints_v6" {
  value       = { for region, relay in module.relay : region => relay.external_ipv6 if relay.external_ipv6 != null }
  description = "IPv6 address to dial on UDP/443 and ask STUN on UDP/3478, per region."
}

output "api_edge_address" {
  value       = module.api_edge.address
  description = "The IPv4 address api.moqom.cloud's A record points at; null until api_edge_image is set. No AAAA: the cluster is IPv4-only."
}

output "control_endpoint" {
  value       = module.control.endpoint
  description = "What a relay in this VPC puts in MOQOM_CONTROL_ENDPOINT. Internal, so nothing outside the VPC can reach it."
}

output "join_endpoint" {
  value       = module.control.public_address
  description = <<-EOT
    Where a phone asks for its own token, on TCP/8444.

    The one public address in the control tier. What a device presents here is
    the `device` credential from Secret Manager, and everything minted against
    it is bounded by that account's grants: publishing, in one tenant, never
    moderation and never ownership.
  EOT
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

output "ziti" {
  value = {
    controller = module.ziti.controller_url
    management = module.ziti.management_url
    router     = module.ziti.router_name
    instance   = module.ziti.instance_name
  }
  description = "The management overlay. The management API is reached from the cluster, or from a laptop through `gcloud compute start-iap-tunnel`; tools/ziti-provision.sh does the latter."
}

output "recorder_signer_account" {
  value       = module.recorder.signer_account
  description = "The account recording download links are signed as; the control plane's MOQOM_RECORDING_SIGNER_ACCOUNT when recorder_thumbprint is set."
}
