output "address" {
  description = "Point every site hostname's A record here."
  value       = google_compute_global_address.sites.address
}

output "bucket" {
  description = "Bucket the builds are uploaded to, one prefix per site key."
  value       = google_storage_bucket.sites.name
}

output "certificate" {
  description = "Managed certificate; PROVISIONING until DNS points at address."
  value       = google_compute_managed_ssl_certificate.sites.name
}

output "api_certificate" {
  description = "Managed certificate for the API hosts, or null without any."
  value       = one(google_compute_managed_ssl_certificate.apis[*].name)
}

output "api_host_certificates" {
  description = "API hostname to its own managed certificate (own_certificate = true)."
  value       = { for host, cert in google_compute_managed_ssl_certificate.api_hosts : host => cert.name }
}

output "project_certificates" {
  description = "Project hostname to its managed certificate; each is PROVISIONING until that hostname's A record points at address."
  value       = { for host, cert in google_compute_managed_ssl_certificate.projects : host => cert.name }
}
