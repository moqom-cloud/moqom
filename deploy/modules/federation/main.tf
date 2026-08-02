# Workload Identity Federation, and the two directions people confuse
# ---------------------------------------------------------------------
# WIF lets an identity from outside GCP act as a GCP service account without a
# downloaded key. That is one direction, and it covers CI pushing images and a
# BYOC relay on EC2 or a customer's Kubernetes cluster reaching MOQOM-managed
# resources.
#
# BYO-bucket recording is the other direction and is not WIF at all:
#
#   Customer bucket on GCS. Nothing to federate. MOQOM already runs as a Google
#   identity, so the customer grants `recording_writer` below
#   roles/storage.objectCreator on their bucket and we write to it directly.
#   The output at the bottom exists to be pasted into their IAM policy.
#
#   Customer bucket on S3. The federation has to be configured on the AWS side:
#   an IAM OIDC provider trusting https://accounts.google.com, with a role trust
#   policy conditioned on the `sub` claim, which is the service account's unique
#   numeric id — not its email, because the email is mutable in a way the id is
#   not. No Terraform in our project can create that; we emit the id and the
#   customer builds the role. `recording_writer_unique_id` is that value.
#
# Everything else in this file is the first direction.

locals {
  # Whether anything outside GCP federates in at all. Derived from the inputs
  # rather than from the resources, so it is known at plan time.
  byoc_wanted = length(var.aws_accounts) + length(var.oidc_providers) > 0

  # Read back rather than assembled from the project number, because a
  # principalSet built from the wrong number is accepted by the API and simply
  # never matches, which presents as an authentication failure with no
  # diagnostic anywhere.
  pool_name = google_iam_workload_identity_pool.this.name

  github_repo_principals = var.github == null ? [] : [
    for repo in var.github.repositories :
    "principalSet://iam.googleapis.com/${local.pool_name}/attribute.repository/${repo}"
  ]

  github_ref_principals = var.github == null ? [] : flatten([
    for repo in var.github.repositories : [
      for ref in var.github.allowed_refs :
      "principalSet://iam.googleapis.com/${local.pool_name}/attribute.repository_ref/${repo}:${ref}"
    ]
  ])

  # Refs when they are specified, whole repositories when they are not.
  github_principals = length(local.github_ref_principals) > 0 ? local.github_ref_principals : local.github_repo_principals
}

resource "google_iam_workload_identity_pool" "this" {
  project                   = var.project_id
  workload_identity_pool_id = var.pool_id
  display_name              = var.name_prefix
  description               = "External identities permitted to act as MOQOM service accounts."
}

# --- GitHub Actions ----------------------------------------------------------

resource "google_iam_workload_identity_pool_provider" "github" {
  count = var.github == null ? 0 : 1

  project                            = var.project_id
  workload_identity_pool_id          = google_iam_workload_identity_pool.this.workload_identity_pool_id
  workload_identity_pool_provider_id = "github"
  display_name                       = "GitHub Actions"

  attribute_mapping = {
    "google.subject"             = "assertion.sub"
    "attribute.repository"       = "assertion.repository"
    "attribute.repository_owner" = "assertion.repository_owner"
    "attribute.repository_ref"   = "assertion.repository + \":\" + assertion.ref"
    "attribute.ref"              = "assertion.ref"
  }

  # Without this condition the provider trusts every token GitHub issues, from
  # every repository on the platform. The service account binding further down
  # restricts which repositories can impersonate it, but a provider open to all
  # of GitHub is one careless binding away from being a public credential, and
  # the careless binding will not be in this file.
  attribute_condition = "assertion.repository_owner == \"${var.github.owner}\""

  oidc {
    issuer_uri = "https://token.actions.githubusercontent.com"
  }
}

resource "google_service_account" "deployer" {
  count = var.github == null ? 0 : 1

  account_id   = substr("${var.name_prefix}-deployer", 0, 30)
  project      = var.project_id
  display_name = "MOQOM CI deployer"
  description  = "Impersonated by GitHub Actions through workload identity federation. Holds no key."
}

resource "google_service_account_iam_member" "deployer_federation" {
  for_each = toset(local.github_principals)

  service_account_id = google_service_account.deployer[0].name
  role               = "roles/iam.workloadIdentityUser"
  member             = each.value

  depends_on = [google_iam_workload_identity_pool_provider.github]
}

resource "google_project_iam_member" "deployer" {
  for_each = var.github == null ? toset([]) : toset(var.deployer_roles)

  project = var.project_id
  role    = each.value
  member  = google_service_account.deployer[0].member
}

# `roles/iam.serviceAccountUser` on named accounts, never on the project.
#
# Deploying the relay means replacing an instance template, and an instance
# template names the service account its VMs run as, so CI genuinely needs to
# act as the relay identity. Granting that at project scope, though, means CI
# can act as *every* identity in the project — and it already holds
# `compute.instanceAdmin.v1`. Those two together are a complete escalation: boot
# a VM as the most privileged account in the project, read its access token off
# the metadata server, and CI is now that account. Neither role looks alarming
# on its own, which is what makes the pair worth spelling out.
#
# Scoped per account, the ceiling is whatever the listed identities can already
# do. The relay account, by design, can do very little.
resource "google_service_account_iam_member" "deployer_acts_as" {
  for_each = var.github == null ? {} : var.deployer_impersonates

  service_account_id = "projects/${var.project_id}/serviceAccounts/${each.value}"
  role               = "roles/iam.serviceAccountUser"
  member             = google_service_account.deployer[0].member
}

# --- BYOC: relays running outside GCP ----------------------------------------

resource "google_iam_workload_identity_pool_provider" "aws" {
  for_each = var.aws_accounts

  project                            = var.project_id
  workload_identity_pool_id          = google_iam_workload_identity_pool.this.workload_identity_pool_id
  workload_identity_pool_provider_id = each.key
  display_name                       = "AWS ${each.value}"

  attribute_mapping = {
    "google.subject"    = "assertion.arn"
    "attribute.aws_arn" = "assertion.arn"
    "attribute.account" = "assertion.account"
  }

  attribute_condition = "assertion.account == \"${each.value}\""

  aws {
    account_id = each.value
  }
}

resource "google_iam_workload_identity_pool_provider" "oidc" {
  for_each = var.oidc_providers

  project                            = var.project_id
  workload_identity_pool_id          = google_iam_workload_identity_pool.this.workload_identity_pool_id
  workload_identity_pool_provider_id = each.key
  display_name                       = "OIDC ${each.key}"

  attribute_mapping = {
    "google.subject"   = "assertion.sub"
    "attribute.issuer" = "assertion.iss"
  }

  attribute_condition = each.value.attribute_condition

  oidc {
    issuer_uri        = each.value.issuer_uri
    allowed_audiences = each.value.allowed_audiences
  }
}

resource "google_service_account" "byoc" {
  count = local.byoc_wanted ? 1 : 0

  account_id   = substr("${var.name_prefix}-byoc", 0, 30)
  project      = var.project_id
  display_name = "MOQOM BYOC relay"
  description  = "Impersonated by self-hosted relays outside GCP. Deliberately separate from the in-project relay identity so a compromised customer environment cannot reach anything a managed relay can."
}

resource "google_project_iam_member" "byoc" {
  # Conditioned on the same variables the account itself is, rather than on
  # whether the account exists. They say the same thing, but a count on another
  # resource is not knowable until that resource is created, and Terraform
  # cannot plan a for_each whose size it does not yet know.
  for_each = local.byoc_wanted ? toset(var.byoc_roles) : toset([])

  project = var.project_id
  role    = each.value
  member  = google_service_account.byoc[0].member
}

# The impersonation bindings for `byoc` are deliberately not created here.
# Which external principal may act as this account is a per-customer decision
# with a per-customer blast radius, and encoding it in the shared environment
# root means every onboarding is a change to production infrastructure. Bind it
# per tenant, out of band, with a principalSet scoped to that tenant's account
# or issuer.

# --- BYO-bucket recording ----------------------------------------------------

resource "google_service_account" "recording_writer" {
  count = var.create_recording_writer ? 1 : 0

  account_id   = substr("${var.name_prefix}-rec-writer", 0, 30)
  project      = var.project_id
  display_name = "MOQOM recording writer"
  description  = "The identity customers grant write access to their own recording bucket. Holds no project roles: its only authority is what a customer has chosen to give it."
}
