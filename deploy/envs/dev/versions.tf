terraform {
  required_version = ">= 1.9"

  # Partial configuration: the bucket comes from backend.hcl at init time, so
  # the same source tree can be pointed at a different state bucket by a fork or
  # a self-hoster without editing a tracked file.
  #
  #   terraform init -backend-config=backend.hcl
  #
  backend "gcs" {
    prefix = "envs/dev"
  }

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 7.41"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.38"
    }
  }
}

provider "google" {
  project = var.project_id
  region  = var.region

  default_labels = local.labels
}

data "google_client_config" "this" {}

# Configured from the cluster this root creates, which means the cluster has to
# exist before anything Kubernetes-shaped can be planned:
#
#   terraform apply -target=module.gke
#   terraform apply
#
# The alternative is a second root and a second state for the workloads, which
# splits one environment across two applies permanently rather than once at
# creation. The token is the caller's own, so whoever runs Terraform needs a
# role on the cluster as well as on the project.
provider "kubernetes" {
  host                   = "https://${module.gke.endpoint}"
  cluster_ca_certificate = base64decode(module.gke.ca_certificate)
  token                  = data.google_client_config.this.access_token
}
