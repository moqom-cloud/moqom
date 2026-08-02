locals {
  all_subnet_cidrs = concat(
    [for r in var.regions : r.relay_cidr],
    [for r in var.regions : r.serverless_cidr],
  )

  # Published by Google, and not one list but two.
  #
  # Most load balancers probe from 35.191.0.0/16 and 130.211.0.0/22. The
  # external passthrough Network Load Balancer -- which is what fronts the relay
  # tier, because it is the only thing that forwards a UDP datagram -- probes
  # from 35.191.0.0/16 and the two 209.85 ranges instead. Allowing only the
  # first pair produces a fleet that is healthy on the machine, answers 200 to
  # anyone who asks locally, and is marked unhealthy by its own load balancer
  # with "Timeout waiting for connect".
  #
  # Both sets are allowed here rather than one per rule: the surface is a single
  # health-check port that serves nothing but liveness, and being specific about
  # which Google prober may reach it buys nothing over being right.
  health_check_ranges = [
    "35.191.0.0/16",
    "130.211.0.0/22",
    "209.85.152.0/22",
    "209.85.204.0/22",
  ]

  # google_compute_firewall has no count/for_each on log_config, so build it
  # once and splat it into every rule.
  log_config = var.enable_firewall_logging ? [{ metadata = "INCLUDE_ALL_METADATA" }] : []
}

resource "google_compute_network" "this" {
  name    = var.name_prefix
  project = var.project_id

  auto_create_subnetworks         = false
  routing_mode                    = var.routing_mode
  delete_default_routes_on_create = false
}

resource "google_compute_subnetwork" "relay" {
  for_each = var.regions

  name    = "${var.name_prefix}-relay-${each.key}"
  project = var.project_id
  region  = each.key
  network = google_compute_network.this.id

  ip_cidr_range = each.value.relay_cidr

  # Relays have no external IPs. Private Google Access is what lets them reach
  # Artifact Registry, Cloud Logging, and Secret Manager without hairpinning
  # through Cloud NAT, which keeps NAT port pressure for genuinely external
  # traffic such as ACME.
  private_ip_google_access = true

  log_config {
    aggregation_interval = "INTERVAL_10_MIN"
    flow_sampling        = 0.1
    metadata             = "INCLUDE_ALL_METADATA"
  }
}

resource "google_compute_subnetwork" "serverless" {
  for_each = var.regions

  name    = "${var.name_prefix}-serverless-${each.key}"
  project = var.project_id
  region  = each.key
  network = google_compute_network.this.id

  ip_cidr_range            = each.value.serverless_cidr
  private_ip_google_access = true
}

# Private services access. Cloud SQL with a private IP lives in a Google-owned
# VPC peered to this one, and it draws its address from this range rather than
# from any subnet above.
resource "google_compute_global_address" "psa" {
  name    = "${var.name_prefix}-psa"
  project = var.project_id
  network = google_compute_network.this.id

  purpose       = "VPC_PEERING"
  address_type  = "INTERNAL"
  prefix_length = var.psa_prefix_length
}

resource "google_service_networking_connection" "psa" {
  network                 = google_compute_network.this.id
  service                 = "servicenetworking.googleapis.com"
  reserved_peering_ranges = [google_compute_global_address.psa.name]

  # Deleting the peering while a Cloud SQL instance still uses it hangs for the
  # full API timeout and then fails. Abandoning leaves the connection in place,
  # which is the honest outcome: the database outlives the Terraform run.
  deletion_policy = "ABANDON"
}

resource "google_compute_router" "nat" {
  for_each = var.enable_cloud_nat ? var.regions : {}

  name    = "${var.name_prefix}-nat-${each.key}"
  project = var.project_id
  region  = each.key
  network = google_compute_network.this.id
}

resource "google_compute_router_nat" "this" {
  for_each = var.enable_cloud_nat ? var.regions : {}

  name    = "${var.name_prefix}-nat-${each.key}"
  project = var.project_id
  region  = each.key
  router  = google_compute_router.nat[each.key].name

  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "ALL_SUBNETWORKS_ALL_IP_RANGES"

  # Dynamic allocation keeps the floor low without capping a relay that is
  # briefly fanning out to many upstreams.
  enable_dynamic_port_allocation = true
  min_ports_per_vm               = 64
  max_ports_per_vm               = 8192

  udp_idle_timeout_sec = var.nat_udp_idle_timeout_sec

  log_config {
    enable = true
    filter = "ERRORS_ONLY"
  }
}

# --- Ingress -----------------------------------------------------------------

# The whole reason this deployment is not Cloud Run. An external passthrough
# NLB does not proxy: the client's source address arrives intact at the VM, so
# the firewall sees 0.0.0.0/0 rather than a load balancer range.
resource "google_compute_firewall" "relay_quic" {
  name    = "${var.name_prefix}-allow-relay-quic"
  project = var.project_id
  network = google_compute_network.this.name

  direction     = "INGRESS"
  priority      = 1000
  source_ranges = ["0.0.0.0/0"]
  target_tags   = [var.relay_target_tag]

  allow {
    protocol = "udp"
    ports    = [tostring(var.relay_udp_port)]
  }

  dynamic "log_config" {
    for_each = local.log_config
    content {
      metadata = log_config.value.metadata
    }
  }
}

resource "google_compute_firewall" "relay_tcp_fallback" {
  count = var.enable_relay_tcp_fallback ? 1 : 0

  name    = "${var.name_prefix}-allow-relay-tcp-fallback"
  project = var.project_id
  network = google_compute_network.this.name

  direction     = "INGRESS"
  priority      = 1000
  source_ranges = ["0.0.0.0/0"]
  target_tags   = [var.relay_target_tag]

  allow {
    protocol = "tcp"
    ports    = [tostring(var.relay_udp_port)]
  }
}

resource "google_compute_firewall" "health_checks" {
  name    = "${var.name_prefix}-allow-health-checks"
  project = var.project_id
  network = google_compute_network.this.name

  direction     = "INGRESS"
  priority      = 900
  source_ranges = local.health_check_ranges
  target_tags   = [var.relay_target_tag]

  allow {
    protocol = "tcp"
    ports    = [tostring(var.relay_health_check_port)]
  }
}

resource "google_compute_firewall" "internal" {
  name    = "${var.name_prefix}-allow-internal"
  project = var.project_id
  network = google_compute_network.this.name

  direction     = "INGRESS"
  priority      = 1000
  source_ranges = local.all_subnet_cidrs

  allow {
    protocol = "tcp"
  }

  allow {
    protocol = "udp"
  }

  allow {
    protocol = "icmp"
  }
}

resource "google_compute_firewall" "iap_ssh" {
  count = var.enable_iap_ssh ? 1 : 0

  name    = "${var.name_prefix}-allow-iap-ssh"
  project = var.project_id
  network = google_compute_network.this.name

  direction = "INGRESS"
  priority  = 1000
  # IAP TCP forwarding always originates here. Opening 22 to this range is not
  # the same as opening it to the internet; the IAM check happens upstream.
  source_ranges = ["35.235.240.0/20"]
  target_tags   = [var.relay_target_tag]

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
}

# --- Egress ------------------------------------------------------------------

# Deny-by-default is scoped to relays rather than applied network-wide. Relays
# run third-party media from the internet and are the machines an attacker
# would most like to use as a pivot; the control plane's egress is already
# constrained by Cloud Run's own model. Traffic to the metadata server is
# always permitted regardless of these rules, so no allow rule covers it.
resource "google_compute_firewall" "relay_deny_egress" {
  name    = "${var.name_prefix}-deny-relay-egress"
  project = var.project_id
  network = google_compute_network.this.name

  direction          = "EGRESS"
  priority           = 65000
  destination_ranges = ["0.0.0.0/0"]
  target_tags        = [var.relay_target_tag]

  deny {
    protocol = "all"
  }

  dynamic "log_config" {
    for_each = local.log_config
    content {
      metadata = log_config.value.metadata
    }
  }
}

# 443/tcp to anywhere covers Artifact Registry, Cloud Logging, Secret Manager,
# and ACME. Narrowing to the Private Google Access ranges would be tighter but
# would also break certificate renewal, which talks to a CA on the public
# internet.
resource "google_compute_firewall" "relay_egress_https" {
  name    = "${var.name_prefix}-allow-relay-egress-https"
  project = var.project_id
  network = google_compute_network.this.name

  direction          = "EGRESS"
  priority           = 1000
  destination_ranges = ["0.0.0.0/0"]
  target_tags        = [var.relay_target_tag]

  allow {
    protocol = "tcp"
    ports    = ["443"]
  }
}

# NTP, which the egress default would otherwise deny.
#
# The relay boots with a chrony configuration pointing at Cloudflare rather than
# at the metadata server, because the clients take their time from Cloudflare
# and mixing a leap-smearing source with a non-smearing one puts a relay up to
# half a second away from the phones publishing to it. Blocking 123 does not
# undo that decision, it just makes it fail: chrony cannot reach anything, the
# clock free-runs, and every timecode the relay stamps is wrong by however far
# the VM has drifted since boot.
resource "google_compute_firewall" "relay_egress_ntp" {
  name    = "${var.name_prefix}-allow-relay-egress-ntp"
  project = var.project_id
  network = google_compute_network.this.name

  direction          = "EGRESS"
  priority           = 1000
  destination_ranges = ["0.0.0.0/0"]
  target_tags        = [var.relay_target_tag]

  allow {
    protocol = "udp"
    ports    = ["123"]
  }
}

# The relay's one call outward that is not media: the long-lived RelayLink
# stream to the control plane, which carries moderation and roster changes.
#
# It needs its own rule because the egress default here is deny and the HTTPS
# rule above only opens 443, while the control plane's internal load balancer
# answers on 8443. Without this a relay starts, serves, passes its health check,
# and silently enforces nothing but what the tokens themselves say -- the
# failure is a reconnect loop in the logs and no change in behaviour anyone
# watching the load balancer would notice.
resource "google_compute_firewall" "relay_egress_control" {
  count = length(var.control_plane_cidrs) == 0 ? 0 : 1

  name    = "${var.name_prefix}-allow-relay-egress-control"
  project = var.project_id
  network = google_compute_network.this.name

  direction          = "EGRESS"
  priority           = 1000
  destination_ranges = var.control_plane_cidrs
  target_tags        = [var.relay_target_tag]

  allow {
    protocol = "tcp"
    ports    = [tostring(var.control_plane_port)]
  }
}

# Relay-to-relay QUIC for upstream subscription, restricted to the VPC. A relay
# that needs to fetch a track it does not hold pulls it from a sibling, not
# from an arbitrary internet host.
resource "google_compute_firewall" "relay_egress_mesh" {
  name    = "${var.name_prefix}-allow-relay-egress-mesh"
  project = var.project_id
  network = google_compute_network.this.name

  direction          = "EGRESS"
  priority           = 1000
  destination_ranges = local.all_subnet_cidrs
  target_tags        = [var.relay_target_tag]

  allow {
    protocol = "udp"
    ports    = [tostring(var.relay_udp_port)]
  }
}

# --- DNS ---------------------------------------------------------------------

resource "google_dns_managed_zone" "edge" {
  count = var.create_dns_zone ? 1 : 0

  name        = var.dns_zone_name
  project     = var.project_id
  dns_name    = var.dns_domain
  description = "Relay edge records. Steering policy is written by the environment root, not here."

  visibility = "public"

  labels = var.labels
}
