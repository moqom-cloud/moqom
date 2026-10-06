# One control-plane component running on the cluster.
#
# The Kubernetes objects rather than the cluster: a namespace, an identity, a
# deployment, and the way in. Instantiated once per component, the same way the
# Cloud Run module next door was, so adding the director later is a module block
# and not a new shape.
#
# The way in is an internal load balancer by default, and that is the important
# decision here. The only thing that dials this is a relay, relays live in the
# same VPC, and an internal address means the moderation stream never crosses the
# public internet and needs no certificate, no DNS record, and no public
# endpoint to defend. A relay outside this VPC -- a customer running their own --
# needs a public endpoint with a real certificate, and that is a deliberate
# addition rather than the default.

locals {
  labels = merge(var.labels, {
    "app.kubernetes.io/name"       = var.name
    "app.kubernetes.io/component"  = "control-plane"
    "app.kubernetes.io/managed-by" = "terraform"
  })

  selector = { "app.kubernetes.io/name" = var.name }
}

resource "kubernetes_namespace_v1" "this" {
  count = var.create_namespace ? 1 : 0

  metadata {
    name   = var.namespace
    labels = var.labels
  }
}

resource "kubernetes_service_account_v1" "this" {
  metadata {
    name      = var.service_account
    namespace = var.namespace
    labels    = local.labels

    # The whole of workload identity on this side: a pod using this account gets
    # tokens for that Google service account, with no key file anywhere.
    annotations = {
      "iam.gke.io/gcp-service-account" = var.google_service_account
    }
  }

  depends_on = [kubernetes_namespace_v1.this]
}

resource "kubernetes_deployment_v1" "this" {
  metadata {
    name      = var.name
    namespace = var.namespace
    labels    = local.labels
  }

  spec {
    replicas = var.replicas

    selector {
      match_labels = local.selector
    }

    strategy {
      type = "RollingUpdate"
      rolling_update {
        # Surge before removing. A replica holds relay links, and taking one
        # away before its replacement is ready means those relays reconnect into
        # a cluster with less capacity than it had.
        max_surge       = 1
        max_unavailable = 0
      }
    }

    template {
      metadata {
        labels      = merge(local.labels, local.selector)
        annotations = var.pod_annotations
      }

      spec {
        service_account_name = kubernetes_service_account_v1.this.metadata[0].name

        # Long enough for a graceful stop to finish. The process drains its
        # health endpoint, waits for the load balancer to notice, and then stops
        # the gRPC server; a grace period shorter than that turns a rollout into
        # a set of severed streams.
        termination_grace_period_seconds = var.termination_grace_seconds

        # Spread across zones. Autopilot will do this on its own for a
        # multi-replica deployment, and saying so makes it a property of the
        # configuration rather than a behaviour of the platform.
        topology_spread_constraint {
          max_skew           = 1
          topology_key       = "topology.kubernetes.io/zone"
          when_unsatisfiable = "ScheduleAnyway"
          label_selector {
            match_labels = local.selector
          }
        }

        container {
          name  = var.name
          image = var.image

          # Always, because the images here carry a moving tag. The kubelet's
          # default for any tag but `:latest` is to reuse whatever the node
          # already has under that name, so pushing a new `:dev` and restarting
          # the deployment gives a pod that is new, healthy, and running last
          # week's binary — with nothing in the rollout to say so. That cost an
          # hour once already, chasing a config change that was in the image and
          # not in the container.
          #
          # The alternative is naming an image by digest, which is what a release
          # should do and would make this line redundant. Until then this is what
          # makes a restart mean what it appears to mean.
          image_pull_policy = "Always"

          port {
            name           = "grpc"
            container_port = var.grpc_port
          }

          port {
            name           = "health"
            container_port = var.health_port
          }

          dynamic "port" {
            for_each = var.public_port == null ? [] : [var.public_port]
            content {
              name           = "join"
              container_port = port.value
            }
          }

          dynamic "env" {
            for_each = var.env
            content {
              name  = env.key
              value = env.value
            }
          }

          dynamic "env" {
            for_each = var.secret_env
            content {
              name = env.key
              value_from {
                secret_key_ref {
                  name = env.value.secret
                  key  = env.value.key
                }
              }
            }
          }

          dynamic "volume_mount" {
            for_each = var.file_secrets
            content {
              name       = volume_mount.key
              mount_path = volume_mount.value.mount_path
              read_only  = true
            }
          }

          resources {
            requests = {
              cpu    = var.cpu
              memory = var.memory
            }
            # Autopilot sets limits equal to requests unless told otherwise, and
            # for a workload with no burst to speak of that is the honest shape:
            # what it asks for is what it gets billed for and what it can use.
            limits = {
              cpu    = var.cpu
              memory = var.memory
            }
          }

          # Three probes with three jobs. Startup covers a slow first
          # connection to the database without making the liveness threshold
          # generous forever; liveness only asks whether the process is stuck,
          # because restarting a replica whose database is unreachable fixes
          # nothing and empties the cluster; readiness is what decides traffic.
          startup_probe {
            http_get {
              path = var.liveness_path
              port = var.health_port
            }
            period_seconds    = 3
            failure_threshold = 20
          }

          liveness_probe {
            http_get {
              path = var.liveness_path
              port = var.health_port
            }
            period_seconds    = 30
            timeout_seconds   = 5
            failure_threshold = 3
          }

          readiness_probe {
            http_get {
              path = var.readiness_path
              port = var.health_port
            }
            period_seconds    = 10
            timeout_seconds   = 3
            failure_threshold = 3
          }

          security_context {
            run_as_non_root = true

            # Numeric, and that is not a style choice. A distroless image
            # declares `USER nonroot`, and the kubelet cannot prove a name is
            # not root without resolving it inside the image, so it refuses to
            # start the container at all. 65532 is what that name resolves to.
            run_as_user  = var.run_as_user
            run_as_group = var.run_as_user

            allow_privilege_escalation = false
            read_only_root_filesystem  = true
            capabilities {
              drop = ["ALL"]
            }
          }
        }

        dynamic "volume" {
          for_each = var.file_secrets
          content {
            name = volume.key
            secret {
              secret_name = volume.value.secret
            }
          }
        }
      }
    }
  }

  # Terraform waits for the rollout, so a broken image fails the apply rather
  # than leaving a green plan over a crash-looping pod.
  wait_for_rollout = true

  depends_on = [kubernetes_namespace_v1.this]

  lifecycle {
    # Autopilot writes back the requests and limits it decided on, and warden
    # writes the version that admitted the pod. Both are records of what the
    # cluster did, so every subsequent plan offers to delete them and the cluster
    # writes them again. Ignored for the same reason as the services below: a plan
    # about an image should be about an image.
    ignore_changes = [
      metadata[0].annotations["autopilot.gke.io/resource-adjustment"],
      metadata[0].annotations["autopilot.gke.io/warden-version"],
    ]
  }
}

resource "kubernetes_service_v1" "this" {
  metadata {
    name      = var.name
    namespace = var.namespace
    labels    = local.labels

    annotations = var.internal_load_balancer ? {
      "networking.gke.io/load-balancer-type" = "Internal"

      # An internal load balancer is regional, and by default only clients in
      # its own region may use it. The relay tier is not in one region, so
      # without this the relays in every other region cannot reach the control
      # plane at all -- and the failure is a connection timeout to an address
      # that exists, resolves, and answers from next door.
      "networking.gke.io/internal-load-balancer-allow-global-access" = tostring(var.global_access)
      # Backed by network endpoint groups, so the balancer sends traffic to a
      # pod rather than to a node that then forwards it. One hop fewer, and
      # health checks that describe the thing actually serving.
      "cloud.google.com/neg" = jsonencode({ ingress = true })
    } : {}
  }

  spec {
    type     = var.internal_load_balancer ? "LoadBalancer" : "ClusterIP"
    selector = local.selector

    port {
      name        = "grpc"
      port        = var.grpc_port
      target_port = var.grpc_port
      protocol    = "TCP"
    }

    port {
      name        = "health"
      port        = var.health_port
      target_port = var.health_port
      protocol    = "TCP"
    }
  }

  # Without this the plan completes before the address exists and the output is
  # empty, which is the value everything downstream is waiting for.
  wait_for_load_balancer = var.internal_load_balancer

  lifecycle {
    # GKE's service controller records the backend it built in an annotation here,
    # so every plan after the first one offers to delete it. Accepting that offer
    # patches a live load balancer to remove the controller's own note about
    # itself, and the controller puts it back -- which is churn on the address the
    # entire relay tier dials, in exchange for nothing.
    #
    # It is also the reason an unrelated apply cannot be trusted to be unrelated.
    # A rollout of the relay reads this module's endpoint output, which makes this
    # resource a dependency, which drags the annotation diff into a plan that was
    # supposed to be about an image tag. Ignoring it here is what keeps a targeted
    # apply targeted.
    ignore_changes = [
      metadata[0].annotations["networking.gke.io/backend-service"],
      metadata[0].annotations["networking.gke.io/target-pool"],
    ]
  }
}

# The public way in, for the one route that has to be reachable from a phone.
#
# A second service rather than another port on the one above, and that is the
# whole point: this one selects the same pods and publishes exactly one port, so
# the admin surface stays on an address that only exists inside the VPC. Two
# services with different exposure is the only way Kubernetes lets that be said.
resource "kubernetes_service_v1" "public" {
  count = var.public_port == null ? 0 : 1

  metadata {
    name      = "${var.name}-public"
    namespace = var.namespace
    labels    = local.labels

    annotations = {
      # Backed by network endpoint groups, so traffic reaches a pod directly and
      # the health check describes the process serving rather than a node.
      "cloud.google.com/neg" = jsonencode({ ingress = true })
    }
  }

  spec {
    type     = "LoadBalancer"
    selector = local.selector

    # Local keeps the caller's address instead of the node's, which is what the
    # per-address limits behind this port (signup, join) key on. Cluster would
    # put every caller on the internet behind one bucket.
    external_traffic_policy = "Local"

    port {
      name        = "join"
      port        = var.public_port
      target_port = var.public_port
      protocol    = "TCP"
    }
  }

  wait_for_load_balancer = true

  # As above. This one is an external balancer, so the controller's note is about
  # a target pool rather than a backend service, and the reasoning is identical.
  lifecycle {
    ignore_changes = [
      metadata[0].annotations["networking.gke.io/backend-service"],
      metadata[0].annotations["networking.gke.io/target-pool"],
    ]
  }
}

# A budget rather than a hope. Autopilot drains nodes for upgrades on its own
# schedule, and without this it may take every replica of a two-replica
# deployment at once.
resource "kubernetes_pod_disruption_budget_v1" "this" {
  count = var.replicas > 1 ? 1 : 0

  metadata {
    name      = var.name
    namespace = var.namespace
    labels    = local.labels
  }

  spec {
    max_unavailable = "1"
    selector {
      match_labels = local.selector
    }
  }

  depends_on = [kubernetes_namespace_v1.this]
}
