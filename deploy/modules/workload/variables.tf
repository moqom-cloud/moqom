variable "name" {
  type        = string
  description = "Component name. Names the deployment, the service, and the selector."
}

variable "namespace" {
  type        = string
  description = "Kubernetes namespace to run in."
}

variable "create_namespace" {
  type        = bool
  description = "Create the namespace. False when several components share one and another module made it."
  default     = true
}

variable "service_account" {
  type        = string
  description = "Kubernetes service account name. Must match what the cluster module bound for workload identity, or the pod gets no Google credentials and every API call fails as unauthenticated."
}

variable "google_service_account" {
  type        = string
  description = "Google service account email the pod acts as."
}

variable "image" {
  type        = string
  description = "Container image. Pin by digest in production: a tag can be moved under a running deployment, and then the thing serving is not the thing that was reviewed."
}

variable "replicas" {
  type        = number
  description = "How many pods. Two or more needs the shared bus for relay directives, or a moderation call handled by one replica cannot reach a relay attached to another."
  default     = 1

  validation {
    condition     = var.replicas >= 1
    error_message = "replicas must be at least one."
  }
}

variable "grpc_port" {
  type        = number
  description = "Port the gRPC API listens on."
  default     = 8443
}

variable "health_port" {
  type        = number
  description = "Port the probes listen on. Separate from the API port so it can stay unreachable from outside the cluster."
  default     = 8080
}

variable "liveness_path" {
  type        = string
  description = "Path that answers whether the process is alive, and does not depend on the database."
  default     = "/livez"
}

variable "readiness_path" {
  type        = string
  description = "Path that answers whether the process should be sent traffic, and does depend on the database."
  default     = "/healthz"
}

variable "env" {
  type        = map(string)
  description = "Plain environment variables."
  default     = {}
}

variable "secret_env" {
  type = map(object({
    secret = string
    key    = string
  }))
  description = "Environment variables read from Kubernetes secrets, keyed by variable name."
  default     = {}
}

variable "file_secrets" {
  type = map(object({
    secret     = string
    mount_path = string
  }))
  description = <<-EOT
    Secrets mounted as files, keyed by volume name. A file rather than a
    variable for anything long or structured: a process's environment is
    readable by more things than its files are, and the service account list is
    a JSON document rather than a word.
  EOT
  default     = {}
}

variable "cpu" {
  type        = string
  description = "CPU request, and limit. Autopilot rounds to its own increments."
  default     = "500m"
}

variable "memory" {
  type        = string
  description = "Memory request, and limit."
  default     = "512Mi"
}

variable "run_as_user" {
  type        = number
  description = "UID the container runs as. 65532 is the nonroot user in Google's distroless images; change it only if the image says otherwise."
  default     = 65532
}

variable "internal_load_balancer" {
  type        = bool
  description = "Expose on an internal load balancer. False leaves it as a cluster IP, reachable only from inside the cluster."
  default     = true
}

variable "transport_security" {
  type        = bool
  description = "Whether the workload terminates TLS on its own gRPC port, which decides the scheme in the endpoint output. This module does not configure it — the workload reads its certificate from a mounted secret — but callers are given the URL to dial, and a scheme that disagrees with what the process is doing is a link that never comes up."
  default     = false
}

variable "global_access" {
  type        = bool
  description = "Let clients in any region of the VPC reach the internal load balancer, not only its own. Required as soon as anything that dials it runs somewhere else."
  default     = true
}

variable "termination_grace_seconds" {
  type        = number
  description = "How long a pod has to drain. Must exceed the process's own shutdown sequence, which waits for load balancers to notice before closing streams."
  default     = 60
}

variable "pod_annotations" {
  type        = map(string)
  description = "Annotations on the pod template. Changing one restarts the pods, which is a way to force a roll."
  default     = {}
}

variable "labels" {
  type        = map(string)
  description = "Labels applied to everything this module creates."
  default     = {}
}
