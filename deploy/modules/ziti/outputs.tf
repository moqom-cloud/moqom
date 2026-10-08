output "external_address" {
  value       = google_compute_address.external.address
  description = "The controller's and router's public address. Identities are issued against it, so changing it means re-enrolling every one."
}

output "internal_address" {
  value       = google_compute_address.internal.address
  description = "The VM's VPC address, which is how the cluster reaches the management API."
}

output "controller_url" {
  value       = "https://${google_compute_address.external.address}:${var.controller_port}"
  description = "Client API, as written into every enrolled identity."
}

output "management_url" {
  value       = "https://${google_compute_address.internal.address}:${var.management_port}"
  description = "Management API from inside the VPC. Its certificate names the external address, so a client verifies against that name."
}

output "router_name" {
  value       = local.router_name
  description = "The edge router, and its tunneler identity."
}

output "service_account_email" {
  value = google_service_account.ziti.email
}

output "instance_name" {
  value = google_compute_instance.ziti.name
}

output "ports" {
  value = {
    controller = var.controller_port
    management = var.management_port
    router     = var.router_port
  }
}
