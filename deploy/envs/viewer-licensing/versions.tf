terraform {
  required_version = ">= 1.9"

  backend "gcs" {
    prefix = "envs/viewer-licensing"
  }

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 7.41"
    }
  }
}
