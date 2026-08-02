output "cluster_name" {
  value       = google_container_cluster.this.name
  description = "Cluster name, for kubectl and for gcloud container clusters get-credentials."
}

output "location" {
  value       = google_container_cluster.this.location
  description = "Cluster location, which is a region because the cluster is regional."
}

output "endpoint" {
  value       = google_container_cluster.this.endpoint
  description = "Kubernetes API address, without a scheme."
}

output "ca_certificate" {
  value       = google_container_cluster.this.master_auth[0].cluster_ca_certificate
  description = "Base64 cluster CA, for configuring the kubernetes provider."
  sensitive   = true
}

output "subnetwork" {
  value       = google_compute_subnetwork.cluster.id
  description = "The subnet the nodes live in, for internal load balancers that must be created in it."
}

output "workload_service_accounts" {
  value       = { for name, account in google_service_account.workload : name => account.email }
  description = "Google service account email per workload, for granting access elsewhere."
}

output "workload_identity_bindings" {
  value = {
    for name, workload in var.workloads :
    name => {
      namespace       = workload.namespace
      service_account = workload.service_account
      annotation      = google_service_account.workload[name].email
    }
  }
  description = "What each Kubernetes service account must be annotated with for workload identity to work."
}
