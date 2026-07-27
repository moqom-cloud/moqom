terraform {
  required_version = ">= 1.9"

  # Deliberately no `backend` block. This root creates the bucket every other
  # root stores its state in, so its own state has nowhere to go but disk.
  # See deploy/README.md for what to do with the resulting file.

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
}
