# The cluster the control plane runs on.
#
# GKE rather than Cloud Run, and the reason is one connection. RelayLink is a
# bidirectional gRPC stream that a relay holds open for as long as it is alive,
# and the control plane pushes moderation directives down it. Cloud Run ends any
# request at sixty minutes, so every link would be severed hourly; more
# importantly, the registry of which relay is attached where is per-process, and
# Cloud Run gives no way for the replica handling a moderator's call to reach the
# replica holding that relay's stream. Pods are addressable and long-lived, which
# is what the design actually needs.
#
# Autopilot rather than Standard: there is nothing here that wants a node pool
# tuned by hand -- the workload is one small Go binary -- and the relay tier next
# door is where the kernel tuning lives, on VMs, for exactly that reason.

locals {
  # Named ranges rather than CIDRs on the cluster, because a range that GKE
  # created is a range Terraform does not know it owns, and the failure mode is
  # a subnet that cannot be deleted with no resource to blame.
  pods_range     = "${var.name_prefix}-pods"
  services_range = "${var.name_prefix}-services"
}

resource "google_compute_subnetwork" "cluster" {
  name    = "${var.name_prefix}-gke-${var.region}"
  project = var.project_id
  region  = var.region
  network = var.network

  ip_cidr_range = var.node_cidr

  # Nodes have no external addresses, so this is how they reach Artifact
  # Registry and the Google APIs the workload calls. Without it the cluster
  # comes up and then cannot pull its own image.
  private_ip_google_access = true

  secondary_ip_range {
    range_name    = local.pods_range
    ip_cidr_range = var.pod_cidr
  }

  secondary_ip_range {
    range_name    = local.services_range
    ip_cidr_range = var.service_cidr
  }
}

resource "google_container_cluster" "this" {
  name     = "${var.name_prefix}-cluster"
  project  = var.project_id
  location = var.region

  enable_autopilot    = true
  deletion_protection = var.deletion_protection

  network    = var.network
  subnetwork = google_compute_subnetwork.cluster.id

  # Regional, and that is not only about node failure: an Autopilot cluster
  # spreads a Deployment's replicas across zones on its own, so a zone going
  # away costs the replicas in it and nothing else.
  ip_allocation_policy {
    cluster_secondary_range_name  = local.pods_range
    services_secondary_range_name = local.services_range
  }

  private_cluster_config {
    enable_private_nodes = true

    # The control plane endpoint stays public, guarded by IAM and optionally by
    # the authorised networks below. A private endpoint would mean no
    # administration without a bastion or a VPN, which for one small cluster
    # buys less than it costs -- and Kubernetes API access is authenticated
    # either way.
    enable_private_endpoint = false
  }

  dynamic "master_authorized_networks_config" {
    for_each = length(var.authorized_networks) == 0 ? [] : [1]
    content {
      dynamic "cidr_blocks" {
        for_each = var.authorized_networks
        content {
          cidr_block   = cidr_blocks.value.cidr
          display_name = cidr_blocks.key
        }
      }
    }
  }

  release_channel {
    channel = var.release_channel
  }

  # Both of these are on because Autopilot will not create a cluster with them
  # off -- system monitoring at all, and Managed Prometheus since 1.25. Left
  # here rather than deleted because the variables document what is and is not
  # a choice, and a future reader should not have to rediscover it from a 400.
  #
  # The bill is smaller than it looks: GKE's own system metrics are not charged,
  # and Managed Prometheus charges for samples ingested. Nothing scrapes the
  # control plane, so nothing is ingested beyond the system set.
  monitoring_config {
    enable_components = var.monitoring_components

    managed_prometheus {
      enabled = var.managed_prometheus
    }
  }

  # A window rather than whenever Google likes. Autopilot upgrades nodes under
  # the workload, and a relay reattaching is cheap but a moderation directive
  # arriving during it is not.
  maintenance_policy {
    recurring_window {
      start_time = var.maintenance_start
      end_time   = var.maintenance_end
      recurrence = "FREQ=WEEKLY;BYDAY=TU,WE,TH"
    }
  }

  resource_labels = var.labels

  lifecycle {
    # Autopilot manages both, and a diff on either is Terraform arguing with the
    # platform about a value it does not control.
    ignore_changes = [node_config, resource_labels["goog-terraform-provisioned"]]
  }
}

# Health checks reach a pod, not a node port, when the load balancer is backed by
# network endpoint groups -- so the rule is written against the pod range and
# carries no target tag, because Autopilot does not let one be set on its nodes.
resource "google_compute_firewall" "health_checks" {
  name    = "${var.name_prefix}-gke-health"
  project = var.project_id
  network = var.network

  direction = "INGRESS"
  priority  = 1000

  # Google's health checkers and the internal load balancers both come from
  # these two ranges. They are Google-owned and fixed; a rule scoped to them is
  # not a rule open to the internet.
  source_ranges      = ["35.191.0.0/16", "130.211.0.0/22"]
  destination_ranges = [var.pod_cidr]

  allow {
    protocol = "tcp"
    ports    = [for port in var.health_check_ports : tostring(port)]
  }
}

# --- Workload identity -------------------------------------------------------

# The Google identities that pods act as.
#
# Created here rather than beside the workloads because the database has to know
# them before the workload exists: Cloud SQL grants IAM login by creating a
# Postgres role from a service account email, and the data module needs that
# email while the cluster is still being built. Keeping them with the cluster
# breaks what would otherwise be a cycle between the two.
resource "google_service_account" "workload" {
  for_each = var.workloads

  account_id   = substr("${var.name_prefix}-${each.key}", 0, 30)
  project      = var.project_id
  display_name = "MOQOM ${each.key}"
  description  = "Identity for the ${each.key} workload in ${google_container_cluster.this.name}."
}

resource "google_service_account_iam_member" "workload" {
  for_each = var.workloads

  service_account_id = google_service_account.workload[each.key].name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${each.value.namespace}/${each.value.service_account}]"
}

resource "google_project_iam_member" "workload" {
  for_each = {
    for pair in flatten([
      for name, workload in var.workloads : [
        for role in concat([
          "roles/logging.logWriter",
          "roles/monitoring.metricWriter",
          "roles/cloudtrace.agent",
        ], workload.roles) : { workload = name, role = role }
      ]
    ]) : "${pair.workload}:${pair.role}" => pair
  }

  project = var.project_id
  role    = each.value.role
  member  = google_service_account.workload[each.value.workload].member
}

resource "google_artifact_registry_repository_iam_member" "pull" {
  for_each = var.artifact_registry_repository == null ? {} : var.workloads

  project    = var.artifact_registry_repository.project_id
  location   = var.artifact_registry_repository.location
  repository = var.artifact_registry_repository.repository
  role       = "roles/artifactregistry.reader"
  member     = google_service_account.workload[each.key].member
}
