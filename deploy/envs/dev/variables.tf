variable "project_id" {
  type        = string
  description = "GCP project for the dev environment."
}

variable "region" {
  type        = string
  description = "Primary region. The cluster and the database live here, and so does one relay tier."
  default     = "us-central1"
}

variable "second_region" {
  type        = string
  description = "Second relay region. Dev runs two so the latency comparison the benchmarks make -- a relay beside the phones against one a region away -- is reproducible in the environment it was measured in."
  default     = "us-west2"
}

variable "relay_image" {
  type        = string
  description = "moqom-relay container image. A tag is acceptable in dev, where a surprise rollout is a useful signal rather than an incident."
}

variable "control_image" {
  type        = string
  description = "moqom-control container image."
}

variable "dns_domain" {
  type        = string
  description = "Zone apex with a trailing dot, e.g. dev.moqom.cloud. Null skips DNS entirely and clients dial the relay's address directly."
  default     = null
}

variable "recording_bucket_name" {
  type        = string
  description = "Globally unique recording bucket name."
}

variable "github_owner" {
  type        = string
  description = "GitHub organisation permitted to federate into this project."
}

variable "github_repositories" {
  type        = list(string)
  description = "owner/name repositories whose Actions may impersonate the deployer."
  default     = []
}

variable "notification_emails" {
  type        = list(string)
  description = "Alert recipients."
  default     = []
}

variable "billing_enabled" {
  type        = bool
  description = "Turn on prepaid billing: joins and minted tokens need a tenant in credit. Needs the two Stripe secrets to have versions first."
  default     = false
}

variable "billing_public_url" {
  type        = string
  description = "Where Stripe Checkout returns customers to."
  default     = "https://moqom.cloud"
}

variable "billing_cors_origins" {
  type        = list(string)
  description = "Browser origins allowed to call the billing API (the signup form)."
  default     = ["https://moqom.cloud", "https://www.moqom.cloud"]
}

variable "billing_exempt_tenants" {
  type        = list(string)
  description = "Tenants never gated on credit: MOQOM's own, and the test rigs."
  default     = ["moqom"]
}

variable "billing_smtp_host" {
  type        = string
  description = "SMTP submission server for spend alerts and leaked-key notices. Empty logs them instead of sending."
  default     = ""
}

variable "billing_smtp_port" {
  type        = string
  description = "587 for STARTTLS or 465 for implicit TLS. Plaintext is refused."
  default     = "587"
}

variable "billing_smtp_username" {
  type        = string
  description = "SMTP AUTH user. The password is the smtp-password secret, never a variable."
  default     = ""
}

variable "billing_email_from" {
  type        = string
  description = "Sender of billing and security email."
  default     = "MOQOM Billing <billing@moqom.cloud>"
}

variable "compliance_image" {
  type        = string
  description = "Compliance image (moqom/go/Dockerfile.compliance), by digest. Null leaves out the nightly check and the retention sweep; the logging, evidence bucket and alerts are created either way."
  default     = null
}

variable "compliance_alert_emails" {
  type        = list(string)
  description = "Recipients of security and compliance alerts."
  default     = ["elliott@moqom.cloud"]
}

variable "compliance_evidence_writers" {
  type        = list(string)
  description = "Members besides the compliance job that may add evidence, e.g. the Cloud Build service account for SBOMs."
  default     = []
}

variable "retention_dry_run" {
  type        = bool
  description = "Run the retention sweep in count-only mode."
  default     = false
}

variable "ziti_controller_image" {
  type        = string
  description = "openziti/ziti-controller, mirrored into Artifact Registry by tools/cloudbuild-ziti-mirror.yaml and pinned by digest."
}

variable "ziti_router_image" {
  type        = string
  description = "openziti/ziti-router, mirrored and pinned the same way."
}

variable "ziti_cli_image" {
  type        = string
  description = "openziti/ziti-cli, which the relays run as their tunneler. Mirrored and pinned the same way."
}

variable "ziti_sql_proxy_image" {
  type        = string
  description = "The Cloud SQL Auth Proxy the overlay VM runs for the operators' Postgres service. Mirrored and pinned the same way."
}

variable "database_operators" {
  type        = list(string)
  description = "Google accounts that may log in to Postgres as themselves, over the overlay's Postgres service."
  default     = []
}

variable "ziti_control_enabled" {
  type        = bool
  description = "Bind the control plane's gRPC and overlay API services on the OpenZiti overlay. Needs the control identity in the control secret (tools/sync-secrets.sh)."
  default     = false
}

variable "ziti_relay_enabled" {
  type        = bool
  description = "Run the overlay tunneler on every relay, with an identity minted per instance. Needs ziti_control_enabled."
  default     = false
}

variable "relay_link_via_overlay" {
  type        = bool
  description = "Relays reach the control plane through their tunneler rather than the internal load balancer. Needs ziti_relay_enabled."
  default     = false
}

variable "relay_link_overlay_only" {
  type        = bool
  description = "The control plane refuses RelayLink unless it arrived over the overlay: the internal TCP path is dark to relays. Turn on only once every relay is attached via the overlay."
  default     = false
}
