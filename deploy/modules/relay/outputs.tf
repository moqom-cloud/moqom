output "external_ip" {
  value       = google_compute_address.relay.address
  description = "Regional anycast-adjacent address clients dial on UDP/443. The environment root points DNS at this."
}

output "region" {
  value       = var.region
  description = "Region this relay tier serves, echoed so callers can key on the module output alone."
}

output "service_account_email" {
  value       = google_service_account.relay.email
  description = "Relay service account. Grant this access to a customer bucket for BYO-bucket recording."
}

output "service_account_member" {
  value       = google_service_account.relay.member
  description = "Relay service account in IAM member form."
}

output "instance_group_manager_id" {
  value       = google_compute_region_instance_group_manager.relay.id
  description = "Regional MIG id, for rollout tooling and monitoring filters."
}

output "instance_group" {
  value       = google_compute_region_instance_group_manager.relay.instance_group
  description = "Instance group URL backing the load balancer."
}

output "backend_service_id" {
  value       = google_compute_region_backend_service.relay.id
  description = "UDP backend service id."
}

output "instance_template_id" {
  value       = google_compute_instance_template.relay.id
  description = "Current instance template. Changes on every image or configuration change and triggers a rolling update."
}
