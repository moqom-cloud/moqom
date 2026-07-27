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
  }
}

provider "google" {
  project = var.project_id
  region  = var.region

  default_labels = local.labels
}
