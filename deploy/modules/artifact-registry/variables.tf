variable "project_id" {
  type        = string
  description = "Project that owns the repository."
}

variable "location" {
  type        = string
  description = "Repository location. A multi-region such as `us` costs a little more and removes a single-region dependency from every relay boot, which is a boot that happens during an autoscale event when you can least afford it."
}

variable "repository_id" {
  type        = string
  description = "Repository name, e.g. moqom."
}

variable "description" {
  type        = string
  description = "Human-readable repository description."
  default     = "MOQOM container images"
}

variable "immutable_tags" {
  type        = bool
  description = "Refuse to move or delete a tag once pushed. On, because the relay MIG is pinned to digests and a moving tag is how a fleet ends up running two builds nobody can name."
  default     = true
}

variable "keep_tagged_versions" {
  type        = number
  description = "Tagged versions retained per image. Enough to roll back through a bad week."
  default     = 20
}

variable "untagged_retention" {
  type        = string
  description = "How long an untagged version survives, as a Go duration. These are intermediate layers and abandoned builds."
  default     = "604800s"
}

variable "cleanup_policy_dry_run" {
  type        = bool
  description = "Report what cleanup would delete without deleting it. Leave on until you have read one cycle of the logs; a cleanup policy that eats the image a MIG is pinned to takes the tier down at the next autoscale, not at the moment of the mistake."
  default     = true
}

variable "readers" {
  type        = list(string)
  description = "IAM members granted pull access. Relay and control-plane service accounts."
  default     = []
}

variable "writers" {
  type        = list(string)
  description = "IAM members granted push access. CI deployers only; humans push through CI."
  default     = []
}

variable "labels" {
  type        = map(string)
  description = "Labels applied to the repository."
  default     = {}
}
