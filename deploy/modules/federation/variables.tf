variable "project_id" {
  type        = string
  description = "Project that owns the pool and the service accounts."
}

variable "project_number" {
  type        = string
  description = "Numeric project number. Principal identifiers use the number, not the id, and there is no way to derive one from the other inside Terraform without a data source round trip."
}

variable "name_prefix" {
  type        = string
  description = "Prefix for resource names, e.g. moqom-prod."
}

variable "pool_id" {
  type        = string
  description = "Workload identity pool id. Pool ids are soft-deleted for 30 days and cannot be reused inside that window, so do not name them after something you might recreate."
}

variable "github" {
  type = object({
    repositories = list(string)
    owner        = string
    allowed_refs = optional(list(string), [])
  })
  description = <<-EOT
    GitHub Actions federation for CI. `owner` scopes the provider's attribute
    condition and is not optional — see the comment in main.tf. `repositories`
    are `owner/name` strings allowed to impersonate the deployer.
    `allowed_refs` further restricts to specific git refs, e.g.
    ["refs/heads/main"]; empty allows any ref in the listed repositories.
  EOT
  default     = null
}

variable "deployer_roles" {
  type        = list(string)
  description = "Project roles for the CI deployer service account. Enumerated by the caller so the blast radius of CI is written down in the environment root rather than buried in a module."
  default     = []
}

variable "aws_accounts" {
  type        = map(string)
  description = "AWS account ids allowed to federate, keyed by a short provider name. For BYOC relays running on EC2 that need to reach MOQOM-managed resources."
  default     = {}
}

variable "oidc_providers" {
  type = map(object({
    issuer_uri          = string
    allowed_audiences   = optional(list(string), [])
    attribute_condition = string
  }))
  description = "Generic OIDC providers, keyed by provider name. For self-hosted Kubernetes clusters presenting projected service account tokens. `attribute_condition` is mandatory; a provider without one trusts every identity the issuer will ever mint."
  default     = {}
}

variable "byoc_roles" {
  type        = list(string)
  description = "Project roles for the BYOC service account that federated non-GCP relays impersonate."
  default     = []
}

variable "create_recording_writer" {
  type        = bool
  description = "Create the service account customers grant access to their own recording bucket. See the BYO-bucket section of deploy/README.md."
  default     = true
}
