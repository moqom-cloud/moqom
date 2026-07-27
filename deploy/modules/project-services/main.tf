resource "google_project_service" "this" {
  for_each = toset(var.services)

  project = var.project_id
  service = each.value

  # API disablement is project-wide and not reference counted. If dev and prod
  # share a project — or if anything outside Terraform uses the API — a destroy
  # here would take them down with it, so enablement is deliberately one-way.
  disable_on_destroy = false
}

# Enabling an API returns before the control plane will reliably serve it, and
# the first resource to touch it fails with a 403 that reads like a broken IAM
# binding. Ordering the API call is not enough; something has to wait out the
# propagation. A `depends_on` against this whole module covers it, since this
# resource is inside it, and `services_ready` exists for callers that want to
# depend on the wait alone.
resource "time_sleep" "propagation" {
  depends_on = [google_project_service.this]

  create_duration = var.propagation_delay
}
