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
# the network endpoint groups envs/dev creates (kubernetes_service_v1.control_api).
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

  name = "moqom-dev-control-api"
  zone = each.key
}

# The pod serves TLS on this port with a private certificate. The balancer
# encrypts to it and, like every Google front end, does not verify it.
resource "google_compute_health_check" "control_api" {
  name = "moqom-control-api"

  https_health_check {
    port         = 8444
    request_path = "/v1/rates"
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
  http_redirect     = true

  apis = merge(
    {
      "api.moqom.cloud" = {
        backend_service = google_compute_backend_service.control_api.id
        redirect_host   = "moqom.cloud"
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
