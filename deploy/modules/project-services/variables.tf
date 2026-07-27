variable "project_id" {
  type        = string
  description = "Project to enable the APIs on."
}

variable "services" {
  type        = list(string)
  description = "Fully qualified service names, e.g. compute.googleapis.com."
}

variable "propagation_delay" {
  type        = string
  description = "How long to wait after enabling before dependents may proceed. See the comment in main.tf; shorten at your own risk."
  default     = "90s"
}
