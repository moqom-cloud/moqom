# The 3D Viewer licence server (services/licensing in the viewer repo). A root
# of its own, like envs/web: it shares nothing with the MOQOM cluster.
#
# Not applied yet. Order:
#
#   terraform init -backend-config=backend.hcl
#   terraform apply -target=module.services -target=module.registry \
#     -target=module.licensing.google_secret_manager_secret.this
#
# Cloud Run won't start a revision whose secrets have no version, so add them
# before the service exists (values from stdin or a file; never on the command
# line, never in tfvars):
#
#   gcloud secrets versions add viewer-licensing-signing-key --data-file=signing-key.seed
#   gcloud secrets versions add viewer-licensing-stripe-secret-key --data-file=-
#   gcloud secrets versions add viewer-licensing-stripe-webhook-secret --data-file=-
#   (and viewer-licensing-smtp-password if smtp is set)
#
# The webhook secret comes from the Stripe endpoint, which needs the public
# URL; until the route below exists, add a placeholder version and replace it.
#
#   docker build -t <registry>/server:<version> services/licensing   # from the viewer repo
#   docker push <registry>/server:<version>
#   terraform apply                                                   # with terraform.tfvars
#
# Then route licensing.elliottwave.studio to it from envs/web (see the
# viewer_licensing_backend variable there) and add the DNS record. A new
# secret version needs a new revision to be read: re-apply, or
# `gcloud run services update viewer-licensing --region us-west1`.

variable "project_id" {
  type    = string
  default = "moqom-503700"
}

variable "region" {
  type    = string
  default = "us-west1"
}

variable "image" {
  type        = string
  description = "Licence server image. The default is Google's sample container, so the infrastructure can be created before the first build."
  default     = "us-docker.pkg.dev/cloudrun/container/hello"
}

variable "stripe_price_yearly" {
  type        = string
  description = "Stripe price ID (price_…) for $99/year."
  default     = ""
}

variable "stripe_price_lifetime" {
  type        = string
  description = "Stripe price ID (price_…) for $500 once."
  default     = ""
}

variable "smtp" {
  type = object({
    host     = string
    port     = optional(string, "587")
    username = string
    from     = optional(string, "3D Viewer <licensing@elliottwave.studio>")
  })
  default = null
}

provider "google" {
  project = var.project_id
  default_labels = {
    system     = "viewer"
    component  = "licensing"
    managed-by = "terraform"
  }
}

module "services" {
  source = "../../modules/project-services"

  project_id = var.project_id
  services = [
    "artifactregistry.googleapis.com",
    "compute.googleapis.com",
    "firestore.googleapis.com",
    "logging.googleapis.com",
    "run.googleapis.com",
    "secretmanager.googleapis.com",
  ]
}

module "registry" {
  source = "../../modules/artifact-registry"

  project_id           = var.project_id
  location             = "us"
  repository_id        = "viewer-licensing"
  description          = "3D Viewer licence server images."
  immutable_tags       = true
  keep_tagged_versions = 10

  depends_on = [module.services]
}

module "licensing" {
  source = "../../modules/viewer-licensing"

  project_id            = var.project_id
  region                = var.region
  image                 = var.image
  stripe_price_yearly   = var.stripe_price_yearly
  stripe_price_lifetime = var.stripe_price_lifetime
  smtp                  = var.smtp

  depends_on = [module.services]
}

# The service account pulls from the registry.
resource "google_artifact_registry_repository_iam_member" "pull" {
  project    = var.project_id
  location   = module.registry.location
  repository = module.registry.repository_id
  role       = "roles/artifactregistry.reader"
  member     = "serviceAccount:${module.licensing.service_account}"
}

output "backend_service" {
  value = module.licensing.backend_service
}

output "registry" {
  value = module.registry.repository_url
}

output "secrets" {
  value = module.licensing.secrets
}
