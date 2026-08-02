variable "project_id" {
  type        = string
  description = "GCP project for the dev environment."
}

variable "region" {
  type        = string
  description = "Primary region. The cluster and the database live here, and so does one relay tier."
  default     = "us-central1"
}

variable "second_region" {
  type        = string
  description = "Second relay region. Dev runs two so the latency comparison the benchmarks make -- a relay beside the phones against one a region away -- is reproducible in the environment it was measured in."
  default     = "us-west2"
}

variable "relay_image" {
  type        = string
  description = "moqom-relay container image. A tag is acceptable in dev, where a surprise rollout is a useful signal rather than an incident."
}

variable "control_image" {
  type        = string
  description = "moqom-control container image."
}

variable "dns_domain" {
  type        = string
  description = "Zone apex with a trailing dot, e.g. dev.moqom.cloud. Null skips DNS entirely and clients dial the relay's address directly."
  default     = null
}

variable "recording_bucket_name" {
  type        = string
  description = "Globally unique recording bucket name."
}

variable "github_owner" {
  type        = string
  description = "GitHub organisation permitted to federate into this project."
}

variable "github_repositories" {
  type        = list(string)
  description = "owner/name repositories whose Actions may impersonate the deployer."
  default     = []
}

variable "notification_emails" {
  type        = list(string)
  description = "Alert recipients."
  default     = []
}
