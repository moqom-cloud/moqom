variable "project_id" {
  type        = string
  description = "Project that owns the Terraform state bucket. Usually a dedicated admin project, not one of the workload projects."
}

variable "region" {
  type        = string
  description = "Region for the provider default. The state bucket itself is dual-region or multi-region; see `state_bucket_location`."
}

variable "state_bucket_name" {
  type        = string
  description = "Globally unique name for the state bucket, e.g. moqom-tfstate-<org>."
}

variable "state_bucket_location" {
  type        = string
  description = "Location for the state bucket. Prefer a multi-region or dual-region so a single-region outage cannot lock every environment out of Terraform."
  default     = "US"
}

variable "state_admins" {
  type        = list(string)
  description = "IAM members granted full control of the state bucket, e.g. [\"group:sre@example.com\"]. Keep this to the humans who are allowed to break glass."
  default     = []
}

variable "state_writers" {
  type        = list(string)
  description = "IAM members granted read/write on state objects. This is where CI deployer service accounts go."
  default     = []
}

variable "labels" {
  type        = map(string)
  description = "Labels applied to the state bucket."
  default     = {}
}
