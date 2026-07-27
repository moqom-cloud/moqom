variable "project_id" {
  type        = string
  description = "Project to deploy the service into."
}

variable "region" {
  type        = string
  description = "Cloud Run region."
}

variable "service_name" {
  type        = string
  description = "Cloud Run service name. This module deploys one service; instantiate it once per control-plane component."
}

variable "image" {
  type        = string
  description = "Container image, ideally by digest."
}

variable "network" {
  type        = string
  description = "VPC for direct egress."
}

variable "subnetwork" {
  type        = string
  description = "Serverless subnet in `region`. Must be at least a /26."
}

variable "network_tags" {
  type        = list(string)
  description = "Tags applied to the direct-egress network interfaces, so firewall rules can name this service."
  default     = []
}

variable "egress" {
  type        = string
  description = "PRIVATE_RANGES_ONLY sends only RFC1918 traffic through the VPC and lets everything else out through Google's own path. ALL_TRAFFIC routes public egress through Cloud NAT too, which is what a fixed source address for a partner allowlist requires."
  default     = "PRIVATE_RANGES_ONLY"

  validation {
    condition     = contains(["PRIVATE_RANGES_ONLY", "ALL_TRAFFIC"], var.egress)
    error_message = "egress must be PRIVATE_RANGES_ONLY or ALL_TRAFFIC."
  }
}

variable "min_instances" {
  type        = number
  description = "Instances kept warm. Zero is free and gives every first request after a lull a cold start, which for token minting is a login that feels broken."
  default     = 1
}

variable "max_instances" {
  type        = number
  description = "Instance ceiling. Also the ceiling on concurrent Postgres connections, since each instance holds a pool."
  default     = 10
}

variable "cpu" {
  type        = string
  description = "CPU limit per instance, e.g. \"1\"."
  default     = "1"
}

variable "memory" {
  type        = string
  description = "Memory limit per instance, e.g. \"512Mi\"."
  default     = "512Mi"
}

variable "concurrency" {
  type        = number
  description = "Requests served concurrently per instance."
  default     = 80
}

variable "request_timeout_seconds" {
  type        = number
  description = "Per-request timeout. The control plane is request/response; anything approaching this is a bug, not a slow client."
  default     = 30
}

variable "container_port" {
  type        = number
  description = "Port the container listens on."
  default     = 8080
}

variable "health_check_path" {
  type        = string
  description = "Path for the startup and liveness probes."
  default     = "/healthz"
}

variable "env" {
  type        = map(string)
  description = "Plain environment variables. Non-secret configuration only."
  default     = {}
}

variable "secret_env" {
  type        = map(string)
  description = "Environment variables sourced from Secret Manager, mapping variable name to secret id. Always resolved at `latest`, so rotating a secret takes effect on the next instance start rather than on the next deploy."
  default     = {}
}

variable "sql_instance_connection_name" {
  type        = string
  description = "project:region:instance for the Cloud SQL volume mount. Null skips it; with private IP and direct VPC egress the socket mount is redundant, and it is here only for tooling that insists on the Unix socket."
  default     = null
}

variable "public_ingress" {
  type        = bool
  description = "Accept traffic from the internet. Authorisation is CAT plus DPoP inside the application, not Cloud Run IAM, so a public service is not an unauthenticated one — but see the note in main.tf about what IAM invoker still buys."
  default     = true
}

variable "invokers" {
  type        = list(string)
  description = "IAM members granted run.invoker when `public_ingress` is false."
  default     = []
}

variable "extra_roles" {
  type        = list(string)
  description = "Additional project-level roles for the service account, beyond logging, monitoring, and tracing. Keep this list short and justify each entry in the environment root."
  default     = []
}

variable "artifact_registry_repository" {
  type = object({
    project_id = string
    location   = string
    repository = string
  })
  description = "Repository to grant pull access on. Null skips the binding."
  default     = null
}

variable "deletion_protection" {
  type        = bool
  description = "Block deletion of the Cloud Run service."
  default     = true
}

variable "labels" {
  type        = map(string)
  description = "Labels applied to the service."
  default     = {}
}
