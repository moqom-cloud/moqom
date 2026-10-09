terraform {
  required_version = ">= 1.9"

  backend "gcs" {
    prefix = "envs/web"
  }

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 7.41"
    }
    http = {
      source  = "hashicorp/http"
      version = "~> 3.4"
    }
  }
}
