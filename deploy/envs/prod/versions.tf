terraform {
  required_version = ">= 1.9"

  backend "gcs" {
    prefix = "envs/prod"
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
  region  = var.primary_region

  default_labels = local.labels
}
