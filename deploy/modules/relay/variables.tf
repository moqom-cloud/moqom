variable "project_id" {
  type        = string
  description = "Project to build the relay tier in."
}

variable "region" {
  type        = string
  description = "Region for the MIG, the backend service, and the external IP. One module instance per region; the environment root fans out."
}

variable "name_prefix" {
  type        = string
  description = "Prefix for every resource name, e.g. moqom-prod. The region is appended, so two instances of this module never collide."
}

variable "network" {
  type        = string
  description = "VPC self link or id."
}

variable "subnetwork" {
  type        = string
  description = "Relay subnet self link or id in `region`."
}

variable "network_tags" {
  type        = list(string)
  description = "Tags applied to relay instances. Must include the network module's `relay_target_tag` or no traffic reaches them."
}

variable "image" {
  type        = string
  description = "Fully qualified relay container image, e.g. us-docker.pkg.dev/p/moqom/moqom-relay@sha256:... Prefer a digest: a MIG rolling update that pulls a moving tag gives you a fleet running two builds and no record of which."
}

variable "registry_host" {
  type        = string
  description = "Artifact Registry host to configure Docker credentials for, e.g. us-docker.pkg.dev."
}

variable "boot_image" {
  type        = string
  description = "Boot image for the relay VMs. Container-Optimized OS: read-only rootfs, automatic updates, and no package manager to drift."
  default     = "projects/cos-cloud/global/images/family/cos-121-lts"
}

variable "machine_type" {
  type        = string
  description = "Machine type. Relay capacity is bounded by packets per second and egress bandwidth long before it is bounded by cores, and GCP scales egress caps with vCPU count, so size on bandwidth."
}

variable "boot_disk_size_gb" {
  type        = number
  description = "Boot disk size. Only the container image and logs live here; recordings go straight to GCS."
  default     = 50
}

variable "boot_disk_type" {
  type        = string
  description = "Boot disk type."
  default     = "pd-balanced"
}

variable "min_replicas" {
  type        = number
  description = "Autoscaler floor. Never below the number of zones in the region if you want a zone failure to be survivable."
}

variable "max_replicas" {
  type        = number
  description = "Autoscaler ceiling."
}

variable "target_cpu_utilization" {
  type        = number
  description = "Autoscaler CPU target. Keep it low: CPU is a lagging indicator for a relay, because the machine saturates its NIC and starts dropping datagrams while the scheduler still looks idle."
  default     = 0.5

  validation {
    condition     = var.target_cpu_utilization > 0 && var.target_cpu_utilization <= 1
    error_message = "target_cpu_utilization is a fraction in (0, 1]."
  }
}

variable "scaling_metric" {
  type = object({
    name   = string
    target = number
    type   = string
  })
  description = "Optional Cloud Monitoring per-instance metric to scale on, e.g. active subscriptions. Far better than CPU; requires the relay to be exporting the metric already."
  default     = null
}

variable "autoscaler_cooldown_sec" {
  type        = number
  description = "How long a new instance is ignored by the autoscaler. Must cover VM boot, image pull, and the relay's own warm-up, or the autoscaler will keep adding instances that were already on their way."
  default     = 180
}

variable "scale_in_max_replicas" {
  type        = number
  description = "Most instances the autoscaler may remove inside `scale_in_window_sec`. Scaling in terminates live sessions, so this is deliberately slower than scaling out."
  default     = 1
}

variable "scale_in_window_sec" {
  type        = number
  description = "Window for `scale_in_max_replicas`."
  default     = 1800
}

variable "udp_port" {
  type        = number
  description = "Port for QUIC and, on the same socket, WebTransport over HTTP/3. Browsers will not dial a non-443 port through most corporate egress filters."
  default     = 443
}

variable "stun_port" {
  type        = number
  description = "Port the relay answers STUN Binding requests on, so a device behind NAT can learn the public address it offers a peer for a direct path. 3478 is the registered port and the one the control plane advertises by default. Null turns the responder off. Must match the network module's `relay_stun_port`."
  default     = 3478
  nullable    = true
}

variable "enable_ipv6" {
  type        = bool
  description = "Serve QUIC and STUN on an IPv6 address as well, behind the same backends. Needs the subnet to be dual stack: set the network module's `enable_ipv6` too."
  default     = false
}

variable "health_check_port" {
  type        = number
  description = "TCP port for the load-balancer health probe. See the comment in main.tf for why this cannot be the serving port."
  default     = 8081
}

variable "health_check_path" {
  type        = string
  description = "Path the relay serves its serving-readiness probe on. Must go unhealthy while draining. The relay calls this one /readyz."
  default     = "/readyz"
}

variable "liveness_check_path" {
  type        = string
  description = "Path the relay serves its liveness probe on. Must stay healthy while draining; it answers 'is this process wedged', not 'should I get new sessions'. The relay calls this one /healthz, which is the opposite of the convention the control plane uses -- so both are named here rather than assumed."
  default     = "/healthz"
}

variable "enable_tcp_fallback_rule" {
  type        = bool
  description = "Also publish a TCP forwarding rule on the same address and port, for the MoQ-over-WebSocket fallback on UDP-hostile networks."
  default     = false
}

variable "connection_idle_timeout_sec" {
  type        = number
  description = "How long the load balancer keeps a UDP flow pinned to a backend without traffic. A subscriber on a paused track still sends QUIC acknowledgements, so this mostly governs how quickly a genuinely gone client frees state."
  default     = 600
}

variable "update_max_surge" {
  type        = number
  description = "Extra instances a rolling update may create. A regional MIG rejects anything between one and its zone count, so in practice this is three or zero; zero means the update takes capacity away first, which for a relay means dropping sessions."
  default     = 3

  validation {
    condition     = var.update_max_surge == 0 || var.update_max_surge >= 3
    error_message = "update_max_surge must be 0 or at least 3, the zone count of a regional MIG."
  }
}

variable "autohealing_initial_delay_sec" {
  type        = number
  description = "Grace period before autohealing starts judging a new instance. Must exceed boot plus image pull plus relay start, or the MIG will kill instances in a loop and the loop looks exactly like a crashing binary."
  default     = 300
}

variable "container_env" {
  type        = map(string)
  description = "Environment variables passed to the relay container. Non-secret configuration only; secrets are read from Secret Manager by the relay itself."
  default     = {}
}

variable "tls_certificate_secret_id" {
  type        = string
  description = "Secret Manager secret holding the PEM certificate chain the relay serves. A fleet needs one certificate distributed to N machines, which is a distribution problem, not an issuance problem — so the secret is the interface and renewal happens outside this module."
  default     = null
}

variable "tls_private_key_secret_id" {
  type        = string
  description = "Secret Manager secret holding the PEM private key."
  default     = null
}

variable "credential_secret_id" {
  type        = string
  description = "Secret Manager secret holding the bearer credential the relay presents to the control plane on RelayLink. Null means the relay attaches to nothing and enforces tokens only."
  default     = null
}

variable "link_ca_secret_id" {
  type        = string
  description = "Secret Manager secret holding the PEM authority that signed the control plane's certificate. This is what stops a relay taking verification keys, room state and moderation from whatever answers to the control plane's address; without it the link is authenticated in one direction only. Null leaves the link on whatever the endpoint's scheme implies."
  default     = null
}

variable "link_client_cert_secret_id" {
  type        = string
  description = "Secret Manager secret holding this tier's client certificate for the link, in PEM. Goes with link_ca_secret_id and link_client_key_secret_id: a control plane that requires a certificate refuses a relay that has none."
  default     = null
}

variable "link_client_key_secret_id" {
  type        = string
  description = "Secret Manager secret holding the private key for link_client_cert_secret_id."
  default     = null
}

variable "ticket_keys_secret_id" {
  type        = string
  description = "Secret Manager secret holding the fleet's direct-ticket keys, a JWK set of ML-DSA-87 keys. Every relay in a fleet must hold the same set, or a participant on one relay can't verify a peer's ticket from another. Null issues no tickets, which keeps everyone on this tier off direct paths."
  default     = null
}

variable "container_uid" {
  type        = number
  description = "UID the relay runs as inside its container. The fetched certificate and key are chowned to it, because a root-owned key is one the container cannot read."
  default     = 65532
}

variable "artifact_registry_repository" {
  type = object({
    project_id = string
    location   = string
    repository = string
  })
  description = "Repository to grant the relay service account pull access on. Null skips the binding, for self-hosters pulling from somewhere else."
  default     = null
}

variable "recording_bucket" {
  type        = string
  description = "GCS bucket the relay writes recording segments to. Null skips the binding."
  default     = null
}

variable "enable_confidential_compute" {
  type        = bool
  description = "Run relays as Confidential VMs. Media is end-to-end encrypted above the relay, so this buys defence in depth against the host rather than confidentiality of content; it also costs throughput."
  default     = false
}

variable "labels" {
  type        = map(string)
  description = "Labels applied to resources that accept them."
  default     = {}
}

variable "ziti" {
  type = object({
    # The tunneler sidecar: the ziti CLI image, pinned by digest.
    cli_image = string
    # Where the relay asks the control plane for its overlay identity, and the
    # audience its instance identity token is minted for.
    enroll_url      = string
    enroll_audience = string
    # Seconds between the tunneler's service polls.
    service_poll_seconds = optional(number, 60)
  })
  description = "Turns on the relay's OpenZiti tunneler: an identity minted per instance by the control plane at boot, a sidecar that intercepts the control plane's overlay service on loopback and hosts this relay's admin port and SSH for operators. Null leaves the relay on its TCP link alone."
  default     = null
}

variable "extra_rules" {
  type        = bool
  default     = true
  description = "Create the STUN and IPv6 forwarding rules. Each external forwarding rule counts against the project's IN_USE_ADDRESSES quota, IPv6 and shared-address ones included, so a project at its limit turns this off and keeps the QUIC rule alone."
}
