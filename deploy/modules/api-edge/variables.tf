variable "project_id" {
  type        = string
  description = "Project the edge runs in."
}

variable "region" {
  type        = string
  description = "The cluster's region; the address and the bucket are here."
}

variable "name_prefix" {
  type        = string
  description = "Prefix for resource names, e.g. moqom-dev."
}

variable "namespace" {
  type        = string
  description = "Kubernetes namespace, the control plane's: the upstream CA is read from its secret."
}

variable "image" {
  type        = string
  description = "The edge image (moqom/go/Dockerfile.api-edge). Null creates the identity and the bucket, and nothing that runs or bills."
  default     = null
}

variable "replicas" {
  type        = number
  description = "Pods, spread one per zone."
  default     = 2
}

variable "hosts" {
  type        = list(string)
  description = "Hostnames served. Let's Encrypt is asked for these and no others."
  default     = ["api.moqom.cloud"]
}

variable "upstream" {
  type        = string
  description = "The control plane's public listener inside the cluster, https://host:port."
}

variable "upstream_server_name" {
  type        = string
  description = "The name the control plane's link certificate carries."
}

variable "upstream_ca_secret" {
  type        = string
  description = "Kubernetes secret holding link-ca.pem, the authority the control plane's certificate chains to."
}

variable "redirect" {
  type        = string
  description = "Where paths outside /v1/ are sent."
  default     = "https://moqom.cloud/"
}

variable "acme_bucket_name" {
  type        = string
  description = "Private bucket for the ACME account and certificates."
}

variable "acme_directory" {
  type        = string
  description = "ACME directory. Let's Encrypt staging is https://acme-staging-v02.api.letsencrypt.org/directory."
  default     = "https://acme-v02.api.letsencrypt.org/directory"
}

variable "acme_email" {
  type        = string
  description = "Contact for the ACME account: expiry and policy notices. Empty registers without one."
  default     = ""
}

variable "cpu" {
  type    = string
  default = "250m"
}

variable "memory" {
  type    = string
  default = "256Mi"
}

variable "labels" {
  type    = map(string)
  default = {}
}
