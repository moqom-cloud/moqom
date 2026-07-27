output "network_id" {
  value       = google_compute_network.this.id
  description = "Fully qualified VPC id."
}

output "network_name" {
  value       = google_compute_network.this.name
  description = "VPC name."
}

output "network_self_link" {
  value       = google_compute_network.this.self_link
  description = "VPC self link, for modules that want a URL rather than an id."
}

output "relay_subnets" {
  value       = { for k, s in google_compute_subnetwork.relay : k => s.id }
  description = "Relay subnet ids keyed by region."
}

output "serverless_subnets" {
  value       = { for k, s in google_compute_subnetwork.serverless : k => s.id }
  description = "Serverless subnet ids keyed by region, for Cloud Run direct VPC egress."
}

output "psa_range_name" {
  value       = google_compute_global_address.psa.name
  description = "Name of the private services access range."
}

output "psa_cidr" {
  value       = "${google_compute_global_address.psa.address}/${google_compute_global_address.psa.prefix_length}"
  description = "CIDR of the private services access range, for egress rules that need to reach Cloud SQL."
}

output "psa_connection" {
  value       = google_service_networking_connection.psa.id
  description = "Opaque handle to `depends_on` before creating any private-IP managed service."
}

output "relay_target_tag" {
  value       = var.relay_target_tag
  description = "Tag the relay module must apply for the firewall rules here to match."
}

output "serverless_target_tag" {
  value       = var.serverless_target_tag
  description = "Tag Cloud Run direct VPC egress interfaces must apply."
}

output "dns_zone_name" {
  value       = one(google_dns_managed_zone.edge[*].name)
  description = "Managed zone name, or null when `create_dns_zone` is false."
}

output "dns_domain" {
  value       = one(google_dns_managed_zone.edge[*].dns_name)
  description = "Zone DNS name with trailing dot, or null when `create_dns_zone` is false."
}
