output "service_name" {
  value       = kubernetes_service_v1.this.metadata[0].name
  description = "Service name, for in-cluster addressing."
}

output "cluster_address" {
  value       = "${kubernetes_service_v1.this.metadata[0].name}.${var.namespace}.svc.cluster.local:${var.grpc_port}"
  description = "How something else in the cluster reaches this component."
}

output "internal_address" {
  value = var.internal_load_balancer ? try(
    kubernetes_service_v1.this.status[0].load_balancer[0].ingress[0].ip, null
  ) : null
  description = "Address of the internal load balancer, or null when there is not one. This is what a relay in the VPC dials."
}

output "endpoint" {
  value = var.internal_load_balancer ? try(
    "${var.transport_security ? "https" : "http"}://${kubernetes_service_v1.this.status[0].load_balancer[0].ingress[0].ip}:${var.grpc_port}", null
  ) : null
  description = <<-EOT
    The URL a relay puts in MOQOM_CONTROL_ENDPOINT.

    The scheme follows transport_security, and an address rather than a name
    because there is no private DNS in front of this load balancer. What the
    certificate is verified against is therefore a name the client is told
    separately -- see the relay's MOQOM_CONTROL_SERVER_NAME -- which also means
    the load balancer can be given a new address without reissuing anything.
  EOT
}
