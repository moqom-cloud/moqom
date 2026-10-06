# A self-hosted OpenZiti network for the management plane: one controller and
# one public edge router, as the official containers, on one small VM.
#
# One VM rather than the Helm charts on the cluster. The charts want
# cert-manager, trust-manager and a LoadBalancer Service per listener, and every
# one of those is an external address in a project whose global address quota is
# already spent; a VM needs one regional address and costs about a sixth of the
# Autopilot pods the same containers would ask for. What it gives up is the
# controller restarting somewhere else when the machine dies -- the state lives
# on a zonal disk that is snapshotted daily, and an outage of this VM stops new
# overlay connections, not media (which never touches it) and not RelayLink
# streams already up (which ride the router, and fall back to the relay's grace
# period when it goes too).
#
# Two web listeners rather than the default one. The client API, enrolment and
# the router control channel stay on controller_port, which every identity must
# reach and is therefore public. The management and fabric APIs move to
# management_port, which only the cluster's pods and IAP can reach: an admin
# password is then not the only thing between the internet and the policy
# store.

locals {
  name        = "${var.name_prefix}-ziti"
  router_name = coalesce(var.router_name, "${var.name_prefix}-router-1")

  cloud_init = templatefile("${path.module}/templates/cloud-init.yaml.tftpl", {
    project_id         = var.project_id
    advertised_address = google_compute_address.external.address
    controller_image   = var.controller_image
    router_image       = var.router_image
    registry_host      = coalesce(var.registry_host, "")
    controller_port    = var.controller_port
    management_port    = var.management_port
    router_port        = var.router_port
    router_name        = local.router_name
    password_secret    = var.admin_password_secret_id
    pki_secret         = var.pki_backup_secret_id
    sql_instance       = coalesce(var.sql_instance_connection_name, "")
    sql_proxy_image    = var.sql_proxy_image
    sql_proxy_port     = var.sql_proxy_port
    state_device       = "/dev/disk/by-id/google-ziti-state"
  })
}

resource "google_service_account" "ziti" {
  account_id   = local.name
  project      = var.project_id
  display_name = "MOQOM OpenZiti controller and router"
  description  = "Runs the overlay VM. Reads and writes the controller's admin password and PKI backup; connects the Cloud SQL proxy."
}

resource "google_project_iam_member" "ziti" {
  for_each = toset(concat(
    [
      "roles/logging.logWriter",
      "roles/monitoring.metricWriter",
    ],
    # The proxy's own connection; logging in to Postgres is the operator's
    # IAM identity, presented as the password, never this account's.
    var.sql_instance_connection_name == null ? [] : ["roles/cloudsql.client"],
  ))

  project = var.project_id
  role    = each.value
  member  = google_service_account.ziti.member
}

resource "google_artifact_registry_repository_iam_member" "ziti_pull" {
  count = var.artifact_registry_repository == null ? 0 : 1

  project    = var.artifact_registry_repository.project_id
  location   = var.artifact_registry_repository.location
  repository = var.artifact_registry_repository.repository
  role       = "roles/artifactregistry.reader"
  member     = google_service_account.ziti.member
}

# Read and add, on its own two secrets and nothing else. Adding is how the VM
# stores a password it generated, and the PKI it bootstrapped, without either
# passing through Terraform or a laptop.
resource "google_secret_manager_secret_iam_member" "ziti" {
  for_each = {
    for pair in setproduct(
      [var.admin_password_secret_id, var.pki_backup_secret_id],
      ["roles/secretmanager.secretAccessor", "roles/secretmanager.secretVersionAdder"],
    ) : "${pair[0]}:${pair[1]}" => { secret = pair[0], role = pair[1] }
  }

  project   = var.project_id
  secret_id = each.value.secret
  role      = each.value.role
  member    = google_service_account.ziti.member
}

# Regional, not global: the project's global external address quota is spent,
# and a VM's address only ever needs to be regional anyway.
resource "google_compute_address" "external" {
  name         = local.name
  project      = var.project_id
  region       = var.region
  address_type = "EXTERNAL"
  network_tier = "PREMIUM"
  labels       = var.labels
}

# Fixed so the control plane's management client and the firewall can name it.
resource "google_compute_address" "internal" {
  name         = "${local.name}-internal"
  project      = var.project_id
  region       = var.region
  address_type = "INTERNAL"
  subnetwork   = var.subnetwork
  labels       = var.labels
}

resource "google_compute_resource_policy" "snapshots" {
  name    = "${local.name}-state"
  project = var.project_id
  region  = var.region

  snapshot_schedule_policy {
    schedule {
      daily_schedule {
        days_in_cycle = 1
        start_time    = "10:00"
      }
    }
    retention_policy {
      max_retention_days    = var.snapshot_retention_days
      on_source_disk_delete = "KEEP_AUTO_SNAPSHOTS"
    }
    snapshot_properties {
      labels            = var.labels
      storage_locations = [var.region]
    }
  }
}

resource "google_compute_disk" "state" {
  name    = "${local.name}-state"
  project = var.project_id
  zone    = var.zone
  type    = "pd-balanced"
  size    = var.state_disk_size_gb
  labels  = var.labels

  # The controller's database is on this disk and nowhere else. A plan that
  # replaces it is a plan that forgets every identity and policy.
  lifecycle {
    prevent_destroy = true
  }
}

resource "google_compute_disk_resource_policy_attachment" "snapshots" {
  name    = google_compute_resource_policy.snapshots.name
  project = var.project_id
  zone    = var.zone
  disk    = google_compute_disk.state.name
}

resource "google_compute_instance" "ziti" {
  name         = local.name
  project      = var.project_id
  zone         = var.zone
  machine_type = var.machine_type
  tags         = [var.network_tag]
  labels       = var.labels

  allow_stopping_for_update = true

  boot_disk {
    auto_delete = true
    initialize_params {
      image = var.boot_image
      size  = 10
      type  = "pd-balanced"
    }
  }

  attached_disk {
    source      = google_compute_disk.state.id
    device_name = "ziti-state"
    mode        = "READ_WRITE"
  }

  network_interface {
    network    = var.network
    subnetwork = var.subnetwork
    network_ip = google_compute_address.internal.address

    access_config {
      nat_ip       = google_compute_address.external.address
      network_tier = "PREMIUM"
    }
  }

  metadata = {
    user-data                    = local.cloud_init
    google-logging-enabled       = "true"
    google-logging-use-fluentbit = "true"
    google-monitoring-enabled    = "true"
    block-project-ssh-keys       = "true"
    enable-oslogin               = "TRUE"
    cos-update-strategy          = "update_disabled"
  }

  service_account {
    email  = google_service_account.ziti.email
    scopes = ["https://www.googleapis.com/auth/cloud-platform"]
  }

  scheduling {
    automatic_restart   = true
    on_host_maintenance = "MIGRATE"
  }

  shielded_instance_config {
    enable_secure_boot          = true
    enable_vtpm                 = true
    enable_integrity_monitoring = true
  }

  depends_on = [
    google_secret_manager_secret_iam_member.ziti,
    google_project_iam_member.ziti,
  ]
}

# --- Firewall ----------------------------------------------------------------

# Every identity reaches these two: the client API to authenticate and enrol,
# the router to carry its circuits. Policy, not the network, decides who gets
# anywhere through them.
resource "google_compute_firewall" "public" {
  name    = "${local.name}-public"
  project = var.project_id
  network = var.network

  direction     = "INGRESS"
  priority      = 1000
  source_ranges = ["0.0.0.0/0"]
  target_tags   = [var.network_tag]

  allow {
    protocol = "tcp"
    ports    = [tostring(var.controller_port), tostring(var.router_port)]
  }
}

resource "google_compute_firewall" "management" {
  count = length(var.management_source_ranges) == 0 ? 0 : 1

  name    = "${local.name}-management"
  project = var.project_id
  network = var.network

  direction     = "INGRESS"
  priority      = 1000
  source_ranges = var.management_source_ranges
  target_tags   = [var.network_tag]

  allow {
    protocol = "tcp"
    ports    = [tostring(var.management_port)]
  }
}

resource "google_compute_firewall" "iap_ssh" {
  count = var.enable_iap_ssh ? 1 : 0

  name    = "${local.name}-iap-ssh"
  project = var.project_id
  network = var.network

  direction     = "INGRESS"
  priority      = 1000
  source_ranges = ["35.235.240.0/20"]
  target_tags   = [var.network_tag]

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
}
