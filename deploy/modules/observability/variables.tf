variable "project_id" {
  type        = string
  description = "Project to create monitoring and logging resources in."
}

variable "name_prefix" {
  type        = string
  description = "Prefix for resource names, e.g. moqom-prod."
}

variable "log_bucket_location" {
  type        = string
  description = "Location for the log bucket."
  default     = "global"
}

variable "log_retention_days" {
  type        = number
  description = "Retention for the relay log bucket. Relay logs are the only record of who connected to what and when, so this doubles as the abuse-investigation window."
  default     = 30
}

variable "notification_emails" {
  type        = list(string)
  description = "Email addresses that receive alerts. Email is a fallback, not a pager; wire a real channel before anything carries traffic."
  default     = []
}

variable "alert_enabled" {
  type        = bool
  description = "Create alert policies. Off in ephemeral environments, where every teardown pages someone."
  default     = true
}

variable "relay_cpu_threshold" {
  type        = number
  description = "Relay CPU utilisation, as a fraction, that fires an alert. Set below the autoscaler's target: by the time a relay is here, scaling has already failed to keep up."
  default     = 0.8
}

variable "sql_cpu_threshold" {
  type        = number
  description = "Cloud SQL CPU utilisation, as a fraction, that fires an alert."
  default     = 0.85
}

variable "sql_disk_threshold" {
  type        = number
  description = "Cloud SQL disk utilisation, as a fraction, that fires an alert. Autoresize makes this a cost alarm as much as a capacity one."
  default     = 0.85
}

variable "cloud_run_error_rate_threshold" {
  type        = number
  description = "Server errors per second from a Cloud Run service that fires an alert."
  default     = 1
}

variable "cloud_run_services" {
  type        = list(string)
  description = "Cloud Run service names to alert on."
  default     = []
}

variable "sql_instance_id" {
  type        = string
  description = "Cloud SQL instance name to alert on. Null skips the database policies."
  default     = null
}

variable "uptime_check_host" {
  type        = string
  description = "Hostname for an external uptime check against the control plane. Null skips it."
  default     = null
}

variable "uptime_check_path" {
  type        = string
  description = "Path the uptime check requests."
  default     = "/healthz"
}

variable "labels" {
  type        = map(string)
  description = "Labels applied to resources that accept them."
  default     = {}
}
