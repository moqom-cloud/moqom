variable "project_id" {
  type        = string
  description = "Project that owns the VPC."
}

variable "name_prefix" {
  type        = string
  description = "Prefix for every resource name, e.g. moqom-prod."
}

variable "regions" {
  type = map(object({
    relay_cidr      = string
    serverless_cidr = string
  }))
  description = <<-EOT
    Regions to build subnets in, keyed by region name. `relay_cidr` carries the
    relay MIG; `serverless_cidr` is consumed by Cloud Run direct VPC egress and
    must be at least a /26 — Cloud Run allocates addresses in blocks and will
    refuse anything smaller.
  EOT

  validation {
    condition     = alltrue([for r in var.regions : tonumber(split("/", r.serverless_cidr)[1]) <= 26])
    error_message = "Every serverless_cidr must be /26 or larger (a numerically smaller prefix length)."
  }
}

variable "routing_mode" {
  type        = string
  description = "VPC dynamic routing mode. GLOBAL so a Cloud Router in one region can advertise routes learned in another; relays are deployed per region but the control plane is not."
  default     = "GLOBAL"

  validation {
    condition     = contains(["GLOBAL", "REGIONAL"], var.routing_mode)
    error_message = "routing_mode must be GLOBAL or REGIONAL."
  }
}

variable "relay_target_tag" {
  type        = string
  description = "Network tag the relay instances carry. Every relay firewall rule targets this tag, so it must match the relay module's `network_tags`."
  default     = "moqom-relay"
}

variable "serverless_target_tag" {
  type        = string
  description = "Network tag applied to Cloud Run direct-VPC-egress interfaces, used to scope their egress rules."
  default     = "moqom-serverless"
}

variable "relay_udp_port" {
  type        = number
  description = "UDP port the relay serves QUIC and WebTransport on. 443 unless you enjoy explaining port numbers to corporate firewalls."
  default     = 443
}

variable "relay_health_check_port" {
  type        = number
  description = "TCP port the relay serves its load-balancer health probe on. Cannot be the QUIC port: GCP health checks cannot speak UDP."
  default     = 8081
}

variable "enable_relay_tcp_fallback" {
  type        = bool
  description = "Open TCP on `relay_udp_port` as well. Reserved for the MoQ-over-WebSocket fallback used where UDP/443 is blocked; leave off until that path exists, since an open port with nothing behind it is just a scan target."
  default     = false
}

variable "psa_prefix_length" {
  type        = number
  description = "Prefix length for the private services access range that Cloud SQL and other managed services allocate from. /20 leaves room; the range cannot be resized in place without recreating peered instances."
  default     = 20
}

variable "enable_cloud_nat" {
  type        = bool
  description = "Create a Cloud Router and Cloud NAT per region. Required for relays, which have no external IPs and still need to pull images and reach ACME."
  default     = true
}

variable "nat_udp_idle_timeout_sec" {
  type        = number
  description = "Cloud NAT UDP mapping lifetime. The 30s default is tuned for DNS, not for relay-to-relay QUIC, where a quiet upstream track would lose its mapping and force a path validation."
  default     = 120
}

variable "enable_iap_ssh" {
  type        = bool
  description = "Allow SSH from the IAP forwarding range to relays. Off in production: the relay is a container on a COS image and there is nothing to hand-edit."
  default     = false
}

variable "enable_firewall_logging" {
  type        = bool
  description = "Log firewall connections. Useful while tightening egress, expensive to leave on."
  default     = false
}

variable "create_dns_zone" {
  type        = bool
  description = "Create a public Cloud DNS zone. The environment root writes the per-region relay records into it; see the anycast note in deploy/README.md."
  default     = false
}

variable "dns_zone_name" {
  type        = string
  description = "Cloud DNS managed zone resource name. Ignored unless `create_dns_zone`."
  default     = ""
}

variable "dns_domain" {
  type        = string
  description = "Fully qualified DNS name for the zone, with trailing dot, e.g. moqom.cloud. Ignored unless `create_dns_zone`."
  default     = ""
}

variable "labels" {
  type        = map(string)
  description = "Labels applied to resources that accept them."
  default     = {}
}
