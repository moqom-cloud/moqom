variable "project_id" {
  type        = string
  description = "Project that owns the pool and the service accounts."
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

  # An owner that does not match the repositories is accepted by the API and
  # then matches nothing, because the provider's attribute condition and the
  # service account's principalSet are keyed on different halves of the same
  # fact. The result is a workflow that cannot authenticate and no log anywhere
  # saying why. Cheaper to reject here.
  validation {
    condition = var.github == null ? true : alltrue([
      for repo in var.github.repositories :
      startswith(repo, "${var.github.owner}/")
    ])
    error_message = "Every entry in github.repositories must be \"<owner>/<name>\" using github.owner as the owner. A repository under a different owner can never satisfy the provider's attribute condition."
  }
}

variable "deployer_roles" {
  type        = list(string)
  description = "Project roles for the CI deployer service account. Enumerated by the caller so the blast radius of CI is written down in the environment root rather than buried in a module. Do not put roles/iam.serviceAccountUser here; use deployer_impersonates, which grants it on named accounts instead of every account in the project."
  default     = []

  validation {
    condition     = !contains(var.deployer_roles, "roles/iam.serviceAccountUser")
    error_message = "roles/iam.serviceAccountUser at project scope lets CI launch a VM as any service account in the project, including more privileged ones, and read its token from the metadata server. List the specific accounts in deployer_impersonates instead."
  }
}

variable "deployer_impersonates" {
  type        = list(string)
  description = "Service account emails the CI deployer may run workloads as. Granted roles/iam.serviceAccountUser on each account individually rather than project-wide."
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
