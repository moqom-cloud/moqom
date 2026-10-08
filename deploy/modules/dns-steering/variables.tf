variable "project_id" {
  type        = string
  description = "Project that owns the managed zone."
}

variable "zone_name" {
  type        = string
  description = "Cloud DNS managed zone resource name."
}

variable "record_name" {
  type        = string
  description = "Fully qualified record name with a trailing dot, e.g. edge.moqom.cloud."
}

variable "endpoints" {
  type        = map(string)
  description = "Relay addresses keyed by GCP region. The region key doubles as the geolocation policy's location, which is how Cloud DNS decides which client is nearest."
}

variable "endpoints_v6" {
  type        = map(string)
  description = "Relay IPv6 addresses keyed by GCP region, published as AAAA records with the same steering as `endpoints`. Empty publishes no AAAA record."
  default     = {}
}

variable "ttl" {
  type        = number
  description = "Record TTL in seconds. This is also the failover time, because a geolocation policy has no health checking — see the comment in main.tf."
  default     = 60
}
