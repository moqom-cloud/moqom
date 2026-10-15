# The public websites. A root of its own so publishing a page never plans
# against the cluster, the database or the relays.
#
#   terraform init -backend-config=backend.hcl
#   terraform apply
#   ../../tools/publish-sites.sh            # upload the builds
#
# Then point each hostname's A record at `terraform output address`; the
# certificate issues once every name resolves here.

variable "project_id" {
  type    = string
  default = "moqom-503700"
}

provider "google" {
  project = var.project_id
  default_labels = {
    system     = "moqom"
    component  = "sites"
    managed-by = "terraform"
  }
}

# The billing API on the same balancer, served by the dev control plane through
# the network endpoint groups envs/dev creates (kubernetes_service_v1.control_api),
# on the control plane's hooks listener.
# Apply envs/dev first. GKE makes a group only in a zone where a node has run
# the pod, and keeps it after, so this lists the zones in the service's
# cloud.google.com/neg-status annotation; a pod rescheduled into a new zone
# needs that zone added here and an apply.
variable "control_api_zones" {
  type    = list(string)
  default = ["us-west1-c"]
}

data "google_compute_network_endpoint_group" "control_api" {
  for_each = toset(var.control_api_zones)

  name = "moqom-dev-control-hooks"
  zone = each.key
}

# The pod's hooks listener serves TLS on this port with a private certificate.
# The balancer encrypts to it and, like every Google front end, does not
# verify it.
resource "google_compute_health_check" "control_api" {
  name = "moqom-control-api"

  https_health_check {
    port         = 8445
    request_path = "/healthz"
  }
}

resource "google_compute_backend_service" "control_api" {
  name                  = "moqom-control-api"
  protocol              = "HTTPS"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  # Signup makes several Stripe calls in a row.
  timeout_sec   = 90
  health_checks = [google_compute_health_check.control_api.id]

  dynamic "backend" {
    for_each = data.google_compute_network_endpoint_group.control_api
    content {
      group                 = backend.value.id
      balancing_mode        = "RATE"
      max_rate_per_endpoint = 100
    }
  }

  log_config {
    enable      = true
    sample_rate = 1
  }
}

# hooks.moqom.cloud: where Stripe and GitHub deliver webhooks. Their senders
# do not offer ML-KEM, which api.moqom.cloud's edge requires, so they stay on
# this balancer. Nothing a user sends or receives passes through it: each
# delivery carries its sender's signature, which the control plane checks
# before reading the body, and every other request is refused at three
# layers -- the URL map routes only the two paths, the policy below allows
# only a POST to them, and the control plane's own hooks-only gate
# (MOQOM_CONTROL_HOOKS_HOST, MOQOM_CONTROL_HOOKS_STRIPE_IPS) answers anything
# else for this host, and a Stripe path from anyone but Stripe, with 404.
locals {
  hooks_host        = "hooks.moqom.cloud"
  stripe_hooks_path = "/v1/stripe/webhook"
  github_hooks_path = "/v1/github/secret-scanning"
}

# Read on every plan, so a change to Stripe's published list lands with the
# next apply. Stripe announces changes ahead of making them.
data "http" "stripe_webhook_ips" {
  url = "https://stripe.com/files/ips/ips_webhooks.json"

  lifecycle {
    postcondition {
      condition     = self.status_code == 200 && length(jsondecode(self.response_body).WEBHOOKS) > 0
      error_message = "Stripe's webhook address list did not load."
    }
  }
}

locals {
  stripe_webhook_ranges = [for ip in jsondecode(data.http.stripe_webhook_ips.response_body).WEBHOOKS : "${ip}/32"]
}

# Off until the project has Cloud Armor quota (requested: 2 policies, 10
# rules). The control plane's gate enforces the same paths and Stripe senders
# without it.
variable "hooks_cloud_armor" {
  type    = bool
  default = false
}

resource "google_compute_security_policy" "hooks" {
  count = var.hooks_cloud_armor ? 1 : 0

  name        = "moqom-hooks"
  description = "hooks.moqom.cloud: a POST to one of two webhook paths, nothing else."
  type        = "CLOUD_ARMOR"

  # Stripe, from Stripe's own addresses only. A basic match holds ten ranges.
  dynamic "rule" {
    for_each = chunklist(local.stripe_webhook_ranges, 10)
    content {
      priority = 100 + rule.key
      action   = "allow"
      match {
        versioned_expr = "SRC_IPS_V1"
        config {
          src_ip_ranges = rule.value
        }
      }
      description = "Stripe webhook senders"
    }
  }

  # Before every allow: rules run lowest priority number first.
  rule {
    priority = 50
    action   = "deny(404)"
    match {
      expr {
        expression = "!(request.method == 'POST' && request.path == '${local.github_hooks_path}') && !(request.method == 'POST' && request.path == '${local.stripe_hooks_path}')"
      }
    }
    description = "Anything but a POST to a webhook path"
  }

  # GitHub publishes no fixed range for secret-scanning reports; its ECDSA
  # signature is the check, and this bounds how often anyone can make the
  # control plane run it.
  rule {
    priority = 200
    action   = "throttle"
    match {
      expr {
        expression = "request.method == 'POST' && request.path == '${local.github_hooks_path}'"
      }
    }
    rate_limit_options {
      conform_action = "allow"
      exceed_action  = "deny(429)"
      enforce_on_key = "IP"
      rate_limit_threshold {
        count        = 30
        interval_sec = 60
      }
    }
    description = "GitHub secret-scanning reports, rate limited per address"
  }

  rule {
    priority = 2147483647
    action   = "deny(404)"
    match {
      versioned_expr = "SRC_IPS_V1"
      config {
        src_ip_ranges = ["*"]
      }
    }
    description = "Everything else"
  }
}

resource "google_compute_backend_service" "control_hooks" {
  name                  = "moqom-control-hooks"
  protocol              = "HTTPS"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  timeout_sec           = 30
  health_checks         = [google_compute_health_check.control_api.id]
  security_policy       = one(google_compute_security_policy.hooks[*].id)

  dynamic "backend" {
    for_each = data.google_compute_network_endpoint_group.control_api
    content {
      group                 = backend.value.id
      balancing_mode        = "RATE"
      max_rate_per_endpoint = 20
    }
  }

  # Records the sender's address, path and status: Stripe's and GitHub's
  # servers, or whoever was refused. No user's request is ever routed here.
  log_config {
    enable      = true
    sample_rate = 1
  }
}

# The 3D Viewer licence server (envs/viewer-licensing), on the same balancer.
# Empty until that env is applied; then set it to its backend_service output:
#
#   terraform apply -var viewer_licensing_backend=$(cd ../viewer-licensing && terraform output -raw backend_service)
#
# and add the A record `licensing` -> address in the elliottwave.studio zone.
# The host gets its own certificate, so api.moqom.cloud's is not re-issued.
variable "viewer_licensing_backend" {
  type    = string
  default = ""
}

module "sites" {
  source = "../../modules/sites"

  name_prefix       = "moqom"
  certificate_epoch = 1
  # Port-80 redirect spends an in-use address; quota is 4 and HTTPS already uses one.
  http_redirect = false

  apis = merge(
    {
      # Moving to the API's own post-quantum TLS edge (envs/dev module.api_edge,
      # cmd/moqom-api-edge). Remove this entry, with control_api's backend and
      # health check above, after api.moqom.cloud's A record at Hover points at
      # the edge; see deploy/README.md.
      "api.moqom.cloud" = {
        backend_service = google_compute_backend_service.control_api.id
        redirect_host   = "moqom.cloud"
      }
      (local.hooks_host) = {
        backend_service = google_compute_backend_service.control_hooks.id
        paths           = [local.stripe_hooks_path, local.github_hooks_path]
        own_certificate = true
      }
    },
    var.viewer_licensing_backend == "" ? {} : {
      "licensing.elliottwave.studio" = {
        backend_service = var.viewer_licensing_backend
        redirect_host   = "viewer.elliottwave.studio"
        own_certificate = true
      }
    },
  )

  sites = {
    "moqom-cloud"        = { hosts = ["moqom.cloud", "www.moqom.cloud"] }
    "moqom-dev"          = { hosts = ["moqom.dev", "www.moqom.dev"] }
    "elliottwave-studio" = { hosts = ["elliottwave.studio", "www.elliottwave.studio"] }
    "elliottwave-dev"    = { hosts = ["elliottwave.dev", "www.elliottwave.dev"] }
  }

  # A lab project's docs: hostname to its directory under sites/. One line per
  # project; see deploy/README.md for the DNS record each needs.
  project_hosts = {
    "six10nine.elliottwave.studio" = "six10nine"
    "khensu.nu"                    = "khensu-nu"
    "www.khensu.nu"                = "khensu-nu"
    "dev.khensu.nu"                = "khensu-dev"
  }

  # Product subdomains. DNS A → sites address; `/` rewrites to the page under the site prefix.
  # nu.Fi is fi.khensu.nu (not nufi). create → /creators; memory → /memory.
  # moments.khensu.nu is staged in GCP DNS and the site page is live at /moments; add it here
  # only after Hover has an A record for moments.khensu.nu → the sites address (otherwise the
  # shared products certificate stays PROVISIONING and product hosts fall back to the wrong cert).
  product_hosts = {
    "webe.khensu.nu"    = { site = "khensu-nu", page = "webe" }
    "void.khensu.nu"    = { site = "khensu-void" }
    "media.khensu.nu"   = { site = "khensu-nu", page = "media" }
    "memory.khensu.nu"  = { site = "khensu-nu", page = "memory" }
    "create.khensu.nu"  = { site = "khensu-nu", page = "creators" }
    "waves.khensu.nu"   = { site = "khensu-nu", page = "waves" }
    "fi.khensu.nu"      = { site = "khensu-nu", page = "nufi" }
    "bi.khensu.nu"      = { site = "khensu-nu", page = "bi" }
    "shield.khensu.nu"  = { site = "khensu-nu", page = "shield" }
    "network.khensu.nu" = { site = "khensu-nu", page = "network" }
    # sla.khensu.nu: live at khensu.nu/sla. When Hover has A → 34.107.228.246,
    # add ONLY with own_certificate = true (never onto the shared products pack
    # first). A NXDOMAIN / PROVISIONING shared cert takes down bi, waves, create
    # (wrong SNI → unstyled sites). Seen 14–15 Oct 2026. Module waits for ACTIVE
    # before proxy swap; still never add a host that does not dig correctly.
  }

  # Site directory to the one prefix other sites may frame in an iframe.
  embed_paths = {}
}

output "address" {
  value = module.sites.address
}

output "bucket" {
  value = module.sites.bucket
}

output "certificate" {
  value = module.sites.certificate
}

output "api_certificate" {
  value = module.sites.api_certificate
}

output "api_host_certificates" {
  value = module.sites.api_host_certificates
}

output "project_certificates" {
  value = module.sites.project_certificates
}
