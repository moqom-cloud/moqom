variable "project_id" {
  type        = string
  description = "GCP project for production."
}

variable "primary_region" {
  type        = string
  description = "Region the control plane and database live in. The relay tier is multi-region; the control plane is not, because a token mint is a millisecond operation and moving it closer to the viewer buys nothing that a multi-region Postgres would not cost more than."
  default     = "us-central1"
}

variable "relay_regions" {
  type = map(object({
    relay_cidr      = string
    serverless_cidr = string
    machine_type    = string
    min_replicas    = number
    max_replicas    = number
  }))
  description = <<-EOT
    Relay regions and their sizing, keyed by GCP region. The key is also the
    Cloud DNS geolocation used to steer clients, so adding a region here adds it
    to the steering policy in the same change.

    `min_replicas` should be at least three in any region that carries real
    traffic: a regional MIG spreads across three zones, and a floor below that
    means a zone failure removes a disproportionate share of capacity.
  EOT
}

variable "relay_image" {
  type        = string
  description = "moqom-relay image, by digest. A tag here means a rolling replacement can pull a build nobody approved."

  validation {
    condition     = can(regex("@sha256:[0-9a-f]{64}$", var.relay_image))
    error_message = "relay_image must be pinned to a digest in production."
  }
}

variable "control_image" {
  type        = string
  description = "moqom-control image, by digest."

  validation {
    condition     = can(regex("@sha256:[0-9a-f]{64}$", var.control_image))
    error_message = "control_image must be pinned to a digest in production."
  }
}

variable "director_image" {
  type        = string
  description = "moqom-director image, by digest."

  validation {
    condition     = can(regex("@sha256:[0-9a-f]{64}$", var.director_image))
    error_message = "director_image must be pinned to a digest in production."
  }
}

variable "dns_domain" {
  type        = string
  description = "Zone apex with a trailing dot, e.g. cloud.moqom.example. Not optional in production: without it there is no steering and every client is pinned to whichever address it was given."
}

variable "recording_bucket_name" {
  type        = string
  description = "Globally unique recording bucket name."
}

variable "recording_bucket_location" {
  type        = string
  description = "Recording bucket location. A multi-region such as US or EU; the choice is also a data residency commitment to customers."
  default     = "US"
}

variable "recording_retention_days" {
  type        = number
  description = "Days before a managed recording is deleted. Zero keeps recordings forever, which is a storage bill that compounds and a privacy exposure that never expires."
  default     = 365
}

variable "github_owner" {
  type        = string
  description = "GitHub organisation permitted to federate into this project."
}

variable "github_repositories" {
  type        = list(string)
  description = "owner/name repositories whose Actions may impersonate the deployer."
}

variable "github_deploy_refs" {
  type        = list(string)
  description = "Git refs allowed to deploy to production, e.g. [\"refs/heads/main\"]. Empty allows any ref in the listed repositories, which in production means any branch anyone can push."
  default     = ["refs/heads/main"]
}

variable "byoc_aws_accounts" {
  type        = map(string)
  description = "AWS account ids allowed to federate for self-hosted relays, keyed by a short provider name."
  default     = {}
}

variable "notification_emails" {
  type        = list(string)
  description = "Alert recipients. A stopgap; route these to a real on-call rotation."
}

variable "kms_key_name" {
  type        = string
  description = "CMEK for recordings and the database. Null uses Google-managed keys, which is adequate encryption at rest and inadequate as an answer to a customer questionnaire."
  default     = null
}
