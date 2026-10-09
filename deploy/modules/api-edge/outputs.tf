output "address" {
  value       = local.enabled ? google_compute_address.edge[0].address : null
  description = "The IPv4 address api.moqom.cloud's A record points at, or null with no image."
}

output "service_account" {
  value = google_service_account.edge.email
}

output "acme_bucket" {
  value = google_storage_bucket.acme.name
}
