locals {
  channels = [for c in google_monitoring_notification_channel.email : c.id]
  enabled  = var.alert_enabled
}

resource "google_monitoring_notification_channel" "email" {
  for_each = toset(var.notification_emails)

  project      = var.project_id
  display_name = "MOQOM ${each.value}"
  type         = "email"

  labels = {
    email_address = each.value
  }
}

# --- Logs --------------------------------------------------------------------

resource "google_logging_project_bucket_config" "relay" {
  project        = var.project_id
  location       = var.log_bucket_location
  bucket_id      = "${var.name_prefix}-relay"
  retention_days = var.log_retention_days
  description    = "Relay logs, separated from _Default so retention can be set on the one log stream that matters without paying for it on all of them."
}

resource "google_logging_project_sink" "relay" {
  project     = var.project_id
  name        = "${var.name_prefix}-relay"
  destination = "logging.googleapis.com/${google_logging_project_bucket_config.relay.id}"

  filter = <<-EOT
    resource.type="gce_instance"
    labels."compute.googleapis.com/resource_name":"${var.name_prefix}-relay"
  EOT

  # A dedicated writer identity per sink, rather than the shared project one.
  # It costs nothing here and means a sink pointed at something outside this
  # project later can be revoked without touching the others.
  unique_writer_identity = true
}

# No bucketWriter binding, and none is missing. Cloud Logging routes to a log
# bucket in the same project internally: it creates no writer identity at all,
# so `writer_identity` comes back empty and a binding for it is rejected as an
# invalid member. A sink pointed at BigQuery, Cloud Storage, Pub/Sub, or another
# project does get one, and that is when this needs to come back.

# A session that ends in an error is the signal that matters and the one that is
# invisible in infrastructure metrics: CPU, bandwidth, and instance count all
# look healthy while every viewer is being disconnected mid-stream.
resource "google_logging_metric" "relay_session_errors" {
  project = var.project_id
  name    = "${var.name_prefix}/relay/session_errors"

  filter = <<-EOT
    resource.type="gce_instance"
    labels."compute.googleapis.com/resource_name":"${var.name_prefix}-relay"
    jsonPayload.event="session_closed"
    jsonPayload.error_code!=""
  EOT

  metric_descriptor {
    metric_kind = "DELTA"
    value_type  = "INT64"
    unit        = "1"

    labels {
      key         = "error_code"
      value_type  = "STRING"
      description = "MoQ session error code."
    }
  }

  label_extractors = {
    error_code = "EXTRACT(jsonPayload.error_code)"
  }
}

# --- Alerts ------------------------------------------------------------------

resource "google_monitoring_alert_policy" "relay_cpu" {
  count = local.enabled ? 1 : 0

  project      = var.project_id
  display_name = "${var.name_prefix} relay CPU saturated"
  combiner     = "OR"

  documentation {
    content   = "Relay CPU is above the alert threshold, which means autoscaling did not keep up. Check the autoscaler for a capped max_replicas before assuming a code regression."
    mime_type = "text/markdown"
  }

  conditions {
    display_name = "Relay instance CPU"

    condition_threshold {
      filter          = "resource.type = \"gce_instance\" AND metric.type = \"compute.googleapis.com/instance/cpu/utilization\" AND metadata.user_labels.component = \"relay\""
      comparison      = "COMPARISON_GT"
      threshold_value = var.relay_cpu_threshold
      duration        = "300s"

      aggregations {
        alignment_period     = "60s"
        per_series_aligner   = "ALIGN_MEAN"
        cross_series_reducer = "REDUCE_MEAN"
      }

      trigger {
        count = 1
      }
    }
  }

  notification_channels = local.channels
}

resource "google_monitoring_alert_policy" "relay_session_errors" {
  count = local.enabled ? 1 : 0

  project      = var.project_id
  display_name = "${var.name_prefix} relay session errors"
  combiner     = "OR"

  documentation {
    content   = "Sessions are terminating with MoQ error codes. Group by error_code before escalating: a spike in one code is a bug, a spread across codes is usually the network."
    mime_type = "text/markdown"
  }

  conditions {
    display_name = "Session close errors"

    condition_threshold {
      filter          = "resource.type = \"gce_instance\" AND metric.type = \"logging.googleapis.com/user/${google_logging_metric.relay_session_errors.name}\""
      comparison      = "COMPARISON_GT"
      threshold_value = 10
      duration        = "300s"

      aggregations {
        alignment_period     = "60s"
        per_series_aligner   = "ALIGN_DELTA"
        cross_series_reducer = "REDUCE_SUM"
      }
    }
  }

  notification_channels = local.channels
}

resource "google_monitoring_alert_policy" "sql_cpu" {
  count = local.enabled && var.sql_instance_id != null ? 1 : 0

  project      = var.project_id
  display_name = "${var.name_prefix} Cloud SQL CPU"
  combiner     = "OR"

  conditions {
    display_name = "Database CPU"

    condition_threshold {
      filter          = "resource.type = \"cloudsql_database\" AND resource.labels.database_id = \"${var.project_id}:${var.sql_instance_id}\" AND metric.type = \"cloudsql.googleapis.com/database/cpu/utilization\""
      comparison      = "COMPARISON_GT"
      threshold_value = var.sql_cpu_threshold
      duration        = "300s"

      aggregations {
        alignment_period   = "60s"
        per_series_aligner = "ALIGN_MEAN"
      }
    }
  }

  notification_channels = local.channels
}

resource "google_monitoring_alert_policy" "sql_disk" {
  count = local.enabled && var.sql_instance_id != null ? 1 : 0

  project      = var.project_id
  display_name = "${var.name_prefix} Cloud SQL disk"
  combiner     = "OR"

  documentation {
    content   = "Disk autoresize will keep this instance alive, so this is a warning about cost and about a query pattern that is writing more than expected, not an imminent outage."
    mime_type = "text/markdown"
  }

  conditions {
    display_name = "Database disk utilisation"

    condition_threshold {
      filter          = "resource.type = \"cloudsql_database\" AND resource.labels.database_id = \"${var.project_id}:${var.sql_instance_id}\" AND metric.type = \"cloudsql.googleapis.com/database/disk/utilization\""
      comparison      = "COMPARISON_GT"
      threshold_value = var.sql_disk_threshold
      duration        = "600s"

      aggregations {
        alignment_period   = "300s"
        per_series_aligner = "ALIGN_MEAN"
      }
    }
  }

  notification_channels = local.channels
}

resource "google_monitoring_alert_policy" "cloud_run_errors" {
  for_each = local.enabled ? toset(var.cloud_run_services) : toset([])

  project      = var.project_id
  display_name = "${var.name_prefix} ${each.value} server errors"
  combiner     = "OR"

  conditions {
    display_name = "5xx rate"

    condition_threshold {
      filter          = "resource.type = \"cloud_run_revision\" AND resource.labels.service_name = \"${each.value}\" AND metric.type = \"run.googleapis.com/request_count\" AND metric.labels.response_code_class = \"5xx\""
      comparison      = "COMPARISON_GT"
      threshold_value = var.cloud_run_error_rate_threshold
      duration        = "180s"

      aggregations {
        alignment_period     = "60s"
        per_series_aligner   = "ALIGN_RATE"
        cross_series_reducer = "REDUCE_SUM"
      }
    }
  }

  notification_channels = local.channels
}

# --- Uptime ------------------------------------------------------------------

# Checks the control plane only. There is no uptime check for the relay,
# because Google's probers speak HTTP over TCP and cannot open a QUIC session —
# the same limitation as the load balancer health checks. Verifying that the
# relay actually serves media needs a synthetic MoQ client, which belongs in the
# application repository rather than here.
resource "google_monitoring_uptime_check_config" "control_plane" {
  count = var.uptime_check_host == null ? 0 : 1

  project      = var.project_id
  display_name = "${var.name_prefix} control plane"
  timeout      = "10s"
  period       = "60s"

  http_check {
    path         = var.uptime_check_path
    port         = 443
    use_ssl      = true
    validate_ssl = true
  }

  monitored_resource {
    type = "uptime_url"
    labels = {
      project_id = var.project_id
      host       = var.uptime_check_host
    }
  }
}

resource "google_monitoring_alert_policy" "uptime" {
  count = local.enabled && var.uptime_check_host != null ? 1 : 0

  project      = var.project_id
  display_name = "${var.name_prefix} control plane unreachable"
  combiner     = "OR"

  conditions {
    display_name = "Uptime check failing"

    condition_threshold {
      filter          = "resource.type = \"uptime_url\" AND metric.type = \"monitoring.googleapis.com/uptime_check/check_passed\" AND metric.labels.check_id = \"${google_monitoring_uptime_check_config.control_plane[0].uptime_check_id}\""
      comparison      = "COMPARISON_LT"
      threshold_value = 1
      duration        = "300s"

      aggregations {
        alignment_period     = "300s"
        per_series_aligner   = "ALIGN_FRACTION_TRUE"
        cross_series_reducer = "REDUCE_MEAN"
      }
    }
  }

  notification_channels = local.channels
}
