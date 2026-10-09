# The relay tier: a regional managed instance group behind an external
# passthrough Network Load Balancer on UDP/443.
#
# Why not Cloud Run, and why not a proxy load balancer
# ----------------------------------------------------
# MoQ is QUIC, and QUIC is UDP. Cloud Run, the global external Application Load
# Balancer, and the proxy Network Load Balancers all speak TCP or terminate
# HTTP; none of them will forward a UDP datagram. The Application LB does serve
# HTTP/3 to clients, but it terminates that QUIC connection at the Google front
# end and re-originates the request to the backend over HTTP/2 — which discards
# exactly what we need: the connection is no longer the client's, WebTransport's
# extended CONNECT does not survive it, and per-connection congestion state
# belongs to a machine we do not control. So the QUIC connection has to
# terminate on our own process, and the only GCP product that will deliver an
# unmodified UDP datagram to our own process is the external passthrough NLB.
#
# Why a MIG rather than GKE with a UDP Service
# --------------------------------------------
# A GKE `Service` of type LoadBalancer on UDP provisions the same passthrough
# NLB underneath, so this is a question about what runs the container, not about
# what carries the packets. The MIG wins here on three counts. The relay needs
# kernel-level tuning — socket buffer sizes, SO_REUSEPORT, gVNIC — which is
# natural on a VM and awkward through a Kubernetes abstraction. Draining a relay
# is measured in the lifetime of a live stream, not in the seconds a pod
# termination grace period assumes. And a self-hoster can lift this module and
# run the relay tier alone without adopting a cluster.
#
# The cost of that choice: scaling out is a VM boot rather than a pod schedule,
# roughly 60-90 seconds against 5, so `min_replicas` has to carry more headroom;
# there is no bin-packing, so a relay that under-uses its machine wastes the
# whole machine; and rollouts are MIG rolling updates, which are coarser than a
# Deployment. If the platform later grows sidecars, per-tenant isolation, or
# co-tenanted workloads, GKE becomes the better answer and the backend service
# and forwarding rule below stay exactly as they are.
#
# Reaching the nearest relay
# --------------------------
# GCP has no anycast UDP: `google_compute_forwarding_rule` for a passthrough NLB
# is regional, and there is no global equivalent for UDP. Two things get us most
# of the way. The external IP is Premium Tier, so an international client enters
# Google's network at the PoP nearest to it and crosses the backbone rather than
# the public internet for the rest of the trip. And the environment root points
# a Cloud DNS geolocation routing policy at each region's address, so resolution
# picks the nearest one. That leaves DNS TTL as the failover time and geography
# as a proxy for latency, both of which are worse than real anycast; the
# `moqom-director` steering service supersedes the DNS policy once it exists,
# because it can measure rather than guess. True anycast means Cloudflare
# Spectrum or BYOIP, which is why nothing below assumes the address is regional
# beyond the forwarding rule itself.

locals {
  name = "${var.name_prefix}-relay-${var.region}"

  # Confidential VMs cannot live migrate, so the maintenance policy is forced.
  # A relay that is terminated for host maintenance drops every session on it.
  on_host_maintenance = var.enable_confidential_compute ? "TERMINATE" : "MIGRATE"

  secret_ids = compact([
    var.tls_certificate_secret_id,
    var.tls_private_key_secret_id,
    var.credential_secret_id,
    var.link_ca_secret_id,
    var.link_client_cert_secret_id,
    var.link_client_key_secret_id,
    var.ticket_keys_secret_id,
  ])
}

resource "google_service_account" "relay" {
  # 30 characters is the hard limit, and a truncated region name can land on a
  # hyphen, which the API rejects for a reason that is not obvious from the
  # error.
  account_id   = trimsuffix(substr("${var.name_prefix}-relay-${var.region}", 0, 30), "-")
  project      = var.project_id
  display_name = "MOQOM relay (${var.region})"
  description  = "Runs the relay VMs. Holds no data-plane authority: media is authorised by CAT tokens the relay verifies, not by this identity."
}

resource "google_project_iam_member" "relay" {
  for_each = toset([
    "roles/logging.logWriter",
    "roles/monitoring.metricWriter",
    "roles/stackdriver.resourceMetadata.writer",
  ])

  project = var.project_id
  role    = each.value
  member  = google_service_account.relay.member
}

resource "google_artifact_registry_repository_iam_member" "relay_pull" {
  count = var.artifact_registry_repository == null ? 0 : 1

  project    = var.artifact_registry_repository.project_id
  location   = var.artifact_registry_repository.location
  repository = var.artifact_registry_repository.repository
  role       = "roles/artifactregistry.reader"
  member     = google_service_account.relay.member
}

# objectCreator, not objectAdmin: a recording relay appends new objects and has
# no reason to be able to delete or overwrite one. Resumable uploads work with
# create alone.
resource "google_storage_bucket_iam_member" "relay_recordings" {
  count = var.recording_bucket == null ? 0 : 1

  bucket = var.recording_bucket
  role   = "roles/storage.objectCreator"
  member = google_service_account.relay.member
}

resource "google_secret_manager_secret_iam_member" "relay_secrets" {
  for_each = toset(local.secret_ids)

  project   = var.project_id
  secret_id = each.value
  role      = "roles/secretmanager.secretAccessor"
  member    = google_service_account.relay.member
}

resource "google_compute_instance_template" "relay" {
  name_prefix = "${local.name}-"
  project     = var.project_id
  region      = var.region

  machine_type = var.machine_type
  tags         = var.network_tags
  labels       = var.labels

  disk {
    source_image = var.boot_image
    disk_size_gb = var.boot_disk_size_gb
    disk_type    = var.boot_disk_type
    auto_delete  = true
    boot         = true
  }

  network_interface {
    network    = var.network
    subnetwork = var.subnetwork

    # No access_config, so no external IP. An external passthrough NLB delivers
    # to the instance's internal address and return traffic leaves with the
    # forwarding rule's address as its source; egress the relay originates goes
    # through Cloud NAT.
    #
    # gVNIC rather than the default virtio: the relay's load is small packets at
    # a high rate, which is precisely where the virtio driver's per-packet cost
    # becomes the bottleneck before bandwidth does.
    nic_type = "GVNIC"

    # An external IPv6 range on the NIC is what a passthrough load balancer's
    # IPv6 forwarding rule delivers to; there is no internal-only equivalent.
    # The VPC firewall still decides what reaches it, with the same rules as
    # IPv4, and the IPv6 egress deny keeps the relay from originating to the
    # internet over it.
    stack_type = var.enable_ipv6 ? "IPV4_IPV6" : "IPV4_ONLY"

    dynamic "ipv6_access_config" {
      for_each = var.enable_ipv6 ? [1] : []
      content {
        network_tier = "PREMIUM"
      }
    }
  }

  metadata = {
    user-data                 = local.cloud_init
    google-logging-enabled    = "true"
    google-monitoring-enabled = "true"
    block-project-ssh-keys    = "true"

    # With project keys blocked, this is what leaves a way in: OS Login gates
    # SSH on IAM rather than on a key someone pasted into project metadata, and
    # it removes access when the role is removed. Blocking project keys without
    # it produces a tier nobody can log into, which is only a security property
    # until the first relay boots and does not serve.
    enable-oslogin               = "TRUE"
    cos-update-strategy          = "update_disabled"
    google-logging-use-fluentbit = "true"
  }

  service_account {
    email = google_service_account.relay.email
    # Scopes are the legacy authorisation layer and predate IAM. Granting
    # cloud-platform here and constraining the service account's roles instead
    # is the current guidance; narrowing scopes on top only produces failures
    # that are hard to attribute.
    scopes = ["https://www.googleapis.com/auth/cloud-platform"]
  }

  scheduling {
    automatic_restart   = true
    on_host_maintenance = local.on_host_maintenance
    preemptible         = false
    provisioning_model  = "STANDARD"
  }

  shielded_instance_config {
    enable_secure_boot          = true
    enable_vtpm                 = true
    enable_integrity_monitoring = true
  }

  confidential_instance_config {
    enable_confidential_compute = var.enable_confidential_compute
  }

  # An instance template is immutable, so any change is a replacement, and the
  # MIG referencing it must be pointed at the new one before the old one can go.
  lifecycle {
    create_before_destroy = true
  }
}

# Two health checks with different jobs, because conflating them makes a drain
# indistinguishable from a crash.
#
# `serving` decides load balancer membership. The relay fails it as soon as it
# is asked to drain, so the LB stops sending new flows immediately.
#
# `liveness` decides autohealing. It must keep passing throughout a drain, or
# the MIG would delete the very instance that is trying to finish its sessions
# gracefully. It answers only "is this process wedged".
#
# Both probe TCP. GCP has no UDP health check, so nothing here can observe the
# socket that actually matters — the relay's own handler has to assert that its
# QUIC listener is accepting before answering 200, otherwise the load balancer's
# view of the fleet is fiction.
resource "google_compute_region_health_check" "serving" {
  name    = "${local.name}-serving"
  project = var.project_id
  region  = var.region

  check_interval_sec  = 5
  timeout_sec         = 3
  healthy_threshold   = 2
  unhealthy_threshold = 2

  http_health_check {
    port         = var.health_check_port
    request_path = var.health_check_path
  }

  log_config {
    enable = true
  }
}

resource "google_compute_region_health_check" "liveness" {
  name    = "${local.name}-liveness"
  project = var.project_id
  region  = var.region

  # Ten minutes of consecutive failure before a relay is destroyed. Deleting a
  # machine holding live streams is the most expensive action in this module, so
  # it should require more evidence than removing it from the load balancer.
  #
  # Ten minutes as a minute apart ten times, rather than thirty seconds apart
  # twenty times: GCP caps the threshold at ten, so the interval carries the
  # rest. Coarser, and the same patience.
  check_interval_sec  = 60
  timeout_sec         = 10
  healthy_threshold   = 1
  unhealthy_threshold = 10

  http_health_check {
    port         = var.health_check_port
    request_path = var.liveness_check_path
  }
}

resource "google_compute_region_instance_group_manager" "relay" {
  name    = local.name
  project = var.project_id
  region  = var.region

  base_instance_name = local.name
  target_size        = null

  version {
    instance_template = google_compute_instance_template.relay.id
  }

  distribution_policy_target_shape = "EVEN"

  update_policy {
    type                           = "PROACTIVE"
    minimal_action                 = "REPLACE"
    most_disruptive_allowed_action = "REPLACE"
    replacement_method             = "SUBSTITUTE"

    # Surge first, then remove. `max_unavailable_fixed = 0` is the whole point:
    # capacity never dips below the current size during a rollout, because the
    # instances being replaced are holding sessions that cannot be re-established
    # somewhere else without the viewer noticing.
    #
    # A regional MIG rejects a non-zero surge unless redistribution is off, so
    # rebalancing across zones is suspended for the duration of the update.
    max_surge_fixed              = var.update_max_surge
    max_unavailable_fixed        = 0
    instance_redistribution_type = "NONE"

    # There is no `min_ready_sec` here: it exists only in google-beta for
    # regional groups. What gates the rollout instead is the serving health
    # check — two consecutive passes five seconds apart — which is a weaker
    # guarantee than a fixed soak, and the reason `update_max_surge` is small
    # enough that a bad build cannot replace the whole tier before anyone looks.
  }

  auto_healing_policies {
    health_check      = google_compute_region_health_check.liveness.id
    initial_delay_sec = var.autohealing_initial_delay_sec
  }

  # Terraform returning before instances exist is correct here: a relay takes
  # longer to become useful than any sensible apply timeout, and the health
  # checks are the real readiness signal.
  wait_for_instances = false

  lifecycle {
    # The autoscaler owns size. Without this, every plan after a scaling event
    # proposes to put it back.
    ignore_changes = [target_size]
  }
}

resource "google_compute_region_autoscaler" "relay" {
  name    = local.name
  project = var.project_id
  region  = var.region
  target  = google_compute_region_instance_group_manager.relay.id

  autoscaling_policy {
    min_replicas    = var.min_replicas
    max_replicas    = var.max_replicas
    cooldown_period = var.autoscaler_cooldown_sec
    mode            = "ON"

    cpu_utilization {
      target = var.target_cpu_utilization
    }

    dynamic "metric" {
      for_each = var.scaling_metric == null ? [] : [var.scaling_metric]
      content {
        name   = metric.value.name
        target = metric.value.target
        type   = metric.value.type
      }
    }

    # Scaling out is cheap and scaling in is destructive, so they are not
    # symmetric. Removing an instance ends every session on it; the window here
    # is long enough that a traffic trough has to be sustained, not just a lull
    # between two live events.
    scale_in_control {
      time_window_sec = var.scale_in_window_sec

      max_scaled_in_replicas {
        fixed = var.scale_in_max_replicas
      }
    }
  }
}

resource "google_compute_address" "relay" {
  name    = local.name
  project = var.project_id
  region  = var.region

  address_type = "EXTERNAL"

  # Premium is not an upgrade here, it is the design. Standard Tier routes the
  # client over the public internet to the region; Premium admits it at the
  # nearest Google PoP and carries it on the backbone, which is the closest
  # thing to anycast available to a regional UDP forwarding rule.
  network_tier = "PREMIUM"

  labels = var.labels
}

resource "google_compute_region_backend_service" "relay" {
  name    = local.name
  project = var.project_id
  region  = var.region

  load_balancing_scheme = "EXTERNAL"
  protocol              = "UDP"
  health_checks         = [google_compute_region_health_check.serving.id]

  backend {
    group = google_compute_region_instance_group_manager.relay.instance_group

    # A passthrough load balancer does not measure backends, so it accepts only
    # this mode. It is a declaration rather than a policy: the provider's
    # default is UTILIZATION, which the API rejects for this kind of balancer.
    balancing_mode = "CONNECTION"
  }

  # QUIC survives a client changing address — that is what connection IDs are
  # for — but the load balancer cannot read a connection ID, so it hashes the
  # packet's addresses instead. A phone moving from wifi to cellular therefore
  # arrives at a different relay, which has no session state for it, and the
  # migration that QUIC was designed to make invisible becomes a reconnect.
  #
  # CLIENT_IP hashes new flows on the source address alone, so a NAT rebinding
  # that only changes the port still lands on the same relay. It does nothing
  # for a genuine address change; closing that properly needs QUIC-LB style
  # routable connection IDs so any relay can forward a stray packet to the one
  # holding the session.
  #
  # The cost of CLIENT_IP is load skew: everyone behind one carrier-grade NAT
  # hashes to the same backend. Watch per-instance session counts, not just CPU.
  session_affinity = "CLIENT_IP"

  connection_tracking_policy {
    # PER_CONNECTION rather than PER_SESSION, because GCP refuses to combine
    # PER_SESSION with the persistence below and the persistence is what makes
    # draining work. Little is lost: affinity for a new flow is still by client
    # address, so the rebinding case above behaves the same. What changes is
    # that an established flow is tracked by its full tuple, which is precisely
    # what lets it survive its backend going unhealthy.
    tracking_mode = "PER_CONNECTION"

    # Without this, a relay that fails its serving health check loses its
    # existing flows as well as its new ones, which turns a drain into a mass
    # disconnect. Persisting means draining works: no new flows arrive, and the
    # ones already there run to completion.
    connection_persistence_on_unhealthy_backends = "ALWAYS_PERSIST"

    # No idle timeout. An external passthrough load balancer rejects any value
    # but its own default, so `connection_idle_timeout_sec` governs the internal
    # case only and is deliberately not passed here -- setting it fails the
    # apply rather than being ignored.
  }

  log_config {
    enable      = true
    sample_rate = 0.1
  }
}

resource "google_compute_forwarding_rule" "quic" {
  name    = "${local.name}-quic"
  project = var.project_id
  region  = var.region

  load_balancing_scheme = "EXTERNAL"
  ip_protocol           = "UDP"
  ports                 = [tostring(var.udp_port)]
  ip_address            = google_compute_address.relay.id
  backend_service       = google_compute_region_backend_service.relay.id
  network_tier          = "PREMIUM"
}

# STUN, on the same address and the same backends. A rule of its own rather
# than a second port on the one above, because a forwarding rule's ports cannot
# change in place and replacing the QUIC rule would drop every live session.
# The relay replies from the address the request was sent to, which is this
# one, so the device accepts the answer.
resource "google_compute_forwarding_rule" "stun" {
  count = var.stun_port != null && var.extra_rules ? 1 : 0

  name    = "${local.name}-stun"
  project = var.project_id
  region  = var.region

  load_balancing_scheme = "EXTERNAL"
  ip_protocol           = "UDP"
  ports                 = [tostring(var.stun_port)]
  ip_address            = google_compute_address.relay.id
  backend_service       = google_compute_region_backend_service.relay.id
  network_tier          = "PREMIUM"
}

# IPv6: one address, and the QUIC and STUN rules again on it, delivering to the
# same backend service. A passthrough IPv6 forwarding rule must name the subnet
# its backends' IPv6 ranges come from.
resource "google_compute_address" "relay_v6" {
  count = var.enable_ipv6 ? 1 : 0

  name    = "${local.name}-v6"
  project = var.project_id
  region  = var.region

  address_type       = "EXTERNAL"
  ip_version         = "IPV6"
  ipv6_endpoint_type = "NETLB"
  subnetwork         = var.subnetwork
  network_tier       = "PREMIUM"

  labels = var.labels
}

resource "google_compute_forwarding_rule" "quic_v6" {
  count = var.enable_ipv6 && var.extra_rules ? 1 : 0

  name    = "${local.name}-quic-v6"
  project = var.project_id
  region  = var.region

  load_balancing_scheme = "EXTERNAL"
  ip_protocol           = "UDP"
  ip_version            = "IPV6"
  ports                 = [tostring(var.udp_port)]
  ip_address            = google_compute_address.relay_v6[0].id
  subnetwork            = var.subnetwork
  backend_service       = google_compute_region_backend_service.relay.id
  network_tier          = "PREMIUM"
}

resource "google_compute_forwarding_rule" "stun_v6" {
  count = var.enable_ipv6 && var.stun_port != null && var.extra_rules ? 1 : 0

  name    = "${local.name}-stun-v6"
  project = var.project_id
  region  = var.region

  load_balancing_scheme = "EXTERNAL"
  ip_protocol           = "UDP"
  ip_version            = "IPV6"
  ports                 = [tostring(var.stun_port)]
  ip_address            = google_compute_address.relay_v6[0].id
  subnetwork            = var.subnetwork
  backend_service       = google_compute_region_backend_service.relay.id
  network_tier          = "PREMIUM"
}

# WebTransport and raw MoQ share this one rule: both are HTTP/3 or QUIC on the
# same UDP socket, distinguished by ALPN inside the connection, which is above
# the layer a passthrough LB looks at. No separate ingress is needed for browser
# clients.

resource "google_compute_forwarding_rule" "tcp_fallback" {
  count = var.enable_tcp_fallback_rule ? 1 : 0

  name    = "${local.name}-tcp"
  project = var.project_id
  region  = var.region

  load_balancing_scheme = "EXTERNAL"
  ip_protocol           = "TCP"
  ports                 = [tostring(var.udp_port)]
  ip_address            = google_compute_address.relay.id
  backend_service       = google_compute_region_backend_service.tcp_fallback[0].id
  network_tier          = "PREMIUM"
}

# A separate backend service because a backend service carries exactly one
# protocol, even when the two forwarding rules share an address.
resource "google_compute_region_backend_service" "tcp_fallback" {
  count = var.enable_tcp_fallback_rule ? 1 : 0

  name    = "${local.name}-tcp"
  project = var.project_id
  region  = var.region

  load_balancing_scheme = "EXTERNAL"
  protocol              = "TCP"
  health_checks         = [google_compute_region_health_check.serving.id]

  backend {
    group = google_compute_region_instance_group_manager.relay.instance_group
  }

  session_affinity = "CLIENT_IP"
}
