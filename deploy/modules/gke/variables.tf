variable "project_id" {
  type        = string
  description = "Project that owns the cluster."
}

variable "region" {
  type        = string
  description = "Region for the regional cluster and its subnet."
}

variable "name_prefix" {
  type        = string
  description = "Prefix for every name this module creates, e.g. moqom-dev."
}

variable "network" {
  type        = string
  description = "VPC id the cluster's subnet is created in."
}

variable "node_cidr" {
  type        = string
  description = "Primary range for cluster nodes. A /24 is plenty for Autopilot, which packs pods rather than machines."
}

variable "pod_cidr" {
  type        = string
  description = "Secondary range for pods. GKE allocates a /24 per node out of this by default, so size it for the node count you will ever reach, not the one you have."
}

variable "service_cidr" {
  type        = string
  description = "Secondary range for cluster IP services."
}

variable "workloads" {
  type = map(object({
    namespace       = string
    service_account = string
    roles           = optional(list(string), [])
  }))
  description = <<-EOT
    Workloads that need a Google identity, keyed by name. Each gets a service
    account bound to the named Kubernetes service account through workload
    identity, plus logging, monitoring and trace. `roles` adds anything else
    that workload needs -- Cloud SQL, a bucket -- and is the whole of its
    authority.
  EOT
  default     = {}
}

variable "artifact_registry_repository" {
  type = object({
    project_id = string
    location   = string
    repository = string
  })
  description = "Repository the workloads pull images from. Null when images come from somewhere else."
  default     = null
}

variable "authorized_networks" {
  type        = map(object({ cidr = string }))
  description = <<-EOT
    Networks permitted to reach the Kubernetes API, keyed by a name for the
    console. Empty means anywhere, which is not as alarming as it sounds --
    the endpoint requires a Google credential with a role on the cluster -- but
    narrowing it removes the API from internet-wide scanning, and an office or
    VPN range costs nothing to add.
  EOT
  default     = {}
}

variable "release_channel" {
  type        = string
  description = "RAPID, REGULAR or STABLE. Regular is the default for a reason: rapid moves faster than a small team reads release notes."
  default     = "REGULAR"

  validation {
    condition     = contains(["RAPID", "REGULAR", "STABLE"], var.release_channel)
    error_message = "release_channel must be RAPID, REGULAR or STABLE."
  }
}

variable "monitoring_components" {
  type        = list(string)
  description = "Which components report metrics. SYSTEM_COMPONENTS is mandatory on Autopilot; the rest bill per sample and are off unless something reads them."
  default     = ["SYSTEM_COMPONENTS"]
}

variable "managed_prometheus" {
  type        = bool
  description = "Managed Prometheus. Autopilot refuses to disable it, so this is effectively fixed; it exists for a Standard cluster built from this module later."
  default     = true
}

variable "health_check_ports" {
  type        = list(number)
  description = "Ports Google's health checkers and internal load balancers may reach on pods."
  default     = [8080, 8443]
}

variable "maintenance_start" {
  type        = string
  description = "RFC3339 start of the weekly maintenance window."
  default     = "2026-01-06T10:00:00Z"
}

variable "maintenance_end" {
  type        = string
  description = "RFC3339 end of the weekly maintenance window. Google requires at least four hours."
  default     = "2026-01-06T18:00:00Z"
}

variable "deletion_protection" {
  type        = bool
  description = "Refuse to destroy the cluster. On in production."
  default     = false
}

variable "labels" {
  type        = map(string)
  description = "Labels for the cluster."
  default     = {}
}
