# Static websites behind one global HTTPS load balancer.
#
# One bucket with a prefix per site, one CDN-backed backend bucket, one address
# and one Google-managed certificate for every hostname. The load balancer maps
# each hostname onto its site's prefix. One backend bucket rather than one per
# site because the project quota allows three, and because nothing about four
# static sites needs four.
#
# Objects are uploaded under exact request paths by tools/publish-sites.sh
# (`site/pricing`, `site/pricing/` and `site/pricing/index.html`), so routing is
# a prefix rewrite and never depends on index-document behaviour.
#
# Nothing serves on a domain until its A record points at `address`, and the
# managed certificate does not issue until then either: applying this stages
# the sites without publishing them.
#
# Project subdomains (`project_hosts`) are routed the same way but each carries
# its own certificate beside the shared one, so adding a host never touches the
# certificate the live hosts are served with.

locals {
  hosts = sort(flatten([for site, config in var.sites : config.hosts]))

  aliased = {
    for site, config in var.sites : site => {
      canonical = config.hosts[0]
      aliases   = slice(config.hosts, 1, length(config.hosts))
    } if length(config.hosts) > 1
  }

  # Every bucket prefix the map routes to, whichever variable named it.
  served = toset(concat(
    keys(var.sites),
    values(var.project_hosts),
    [for cfg in values(var.product_hosts) : cfg.site],
  ))

  # Path matcher name per product host (host rules need a unique matcher when `/` is a page).
  product_matcher = {
    for host in keys(var.product_hosts) :
    host => "p-${replace(replace(host, ".", "-"), "*", "x")}"
  }

  # Shared pack only. Hosts with own_certificate=true never touch this list, so
  # a new subdomain's missing DNS cannot put bi/waves/create into PROVISIONING.
  product_host_list = sort([for h, cfg in var.product_hosts : h if !cfg.own_certificate])
  product_own_hosts = { for h, cfg in var.product_hosts : h => cfg if cfg.own_certificate }
}

resource "google_storage_bucket" "sites" {
  name     = "${var.name_prefix}-sites"
  location = var.location

  uniform_bucket_level_access = true
  public_access_prevention    = "inherited"
  force_destroy               = false

  versioning {
    enabled = true
  }

  lifecycle_rule {
    condition {
      num_newer_versions = 5
      with_state         = "ARCHIVED"
    }
    action {
      type = "Delete"
    }
  }
}

# Public read, which is what a website is. Writes stay with the deployer.
resource "google_storage_bucket_iam_member" "public" {
  bucket = google_storage_bucket.sites.name
  role   = "roles/storage.objectViewer"
  member = "allUsers"
}

resource "google_compute_backend_bucket" "sites" {
  name        = "${var.name_prefix}-sites"
  bucket_name = google_storage_bucket.sites.name
  enable_cdn  = true

  cdn_policy {
    cache_mode        = "CACHE_ALL_STATIC"
    default_ttl       = 300
    max_ttl           = 86400
    client_ttl        = 300
    negative_caching  = true
    serve_while_stale = 86400
  }

  custom_response_headers = [
    "Strict-Transport-Security: max-age=63072000; includeSubDomains; preload",
    "X-Content-Type-Options: nosniff",
    "Referrer-Policy: strict-origin-when-cross-origin",
    "Permissions-Policy: camera=(), microphone=(), geolocation=()",
    "X-Frame-Options: DENY",
  ]
}

# The same bucket behind a backend that lets any site frame it. Only the
# prefixes in embed_paths route here; a backend's headers can't vary by path.
resource "google_compute_backend_bucket" "embeds" {
  count = length(var.embed_paths) == 0 ? 0 : 1

  name        = "${var.name_prefix}-sites-embeds"
  bucket_name = google_storage_bucket.sites.name
  enable_cdn  = true

  cdn_policy {
    cache_mode        = "CACHE_ALL_STATIC"
    default_ttl       = 300
    max_ttl           = 86400
    client_ttl        = 300
    negative_caching  = true
    serve_while_stale = 86400
  }

  custom_response_headers = [
    "Strict-Transport-Security: max-age=63072000; includeSubDomains; preload",
    "X-Content-Type-Options: nosniff",
    "Referrer-Policy: strict-origin-when-cross-origin",
    "Permissions-Policy: camera=(), microphone=(), geolocation=()",
    "Content-Security-Policy: frame-ancestors *",
  ]
}

resource "google_compute_global_address" "sites" {
  name = "${var.name_prefix}-sites"
}

resource "google_compute_managed_ssl_certificate" "sites" {
  # Named by content so a change of hostnames creates the new certificate
  # before the old one is detached, rather than taking HTTPS down to re-issue.
  name = "${var.name_prefix}-sites-${substr(sha1(join(",", concat(local.hosts, [tostring(var.certificate_epoch)]))), 0, 8)}"

  managed {
    domains = local.hosts
  }

  lifecycle {
    create_before_destroy = true
  }
}

locals {
  shared_api_hosts = sort([for host, api in var.apis : host if !api.own_certificate])
  own_api_hosts    = { for host, api in var.apis : host => api if api.own_certificate }
}

# A certificate of its own: adding a name to the one above re-issues it, and
# every site would wait on the new name's DNS.
resource "google_compute_managed_ssl_certificate" "apis" {
  count = length(local.shared_api_hosts) == 0 ? 0 : 1

  name = "${var.name_prefix}-apis-${substr(sha1(join(",", concat(local.shared_api_hosts, [tostring(var.certificate_epoch)]))), 0, 8)}"

  managed {
    domains = local.shared_api_hosts
  }

  lifecycle {
    create_before_destroy = true
  }
}

# API hosts added after the shared API certificate went live, one certificate
# each, named like the project ones. They count towards the proxy's 15.
resource "google_compute_managed_ssl_certificate" "api_hosts" {
  for_each = local.own_api_hosts

  name = "${substr("${var.name_prefix}-${replace(each.key, ".", "-")}", 0, 54)}-${substr(sha1("${each.key},${var.certificate_epoch}"), 0, 8)}"

  managed {
    domains = [each.key]
  }

  lifecycle {
    create_before_destroy = true
  }
}

# One certificate per project hostname, for the same reason as the API's: a
# name on the shared certificate re-issues it, and a new name's DNS is never in
# place at apply time. A proxy holds at most 15 certificates, which with the two
# above leaves room for 13 project hosts.
resource "google_compute_managed_ssl_certificate" "projects" {
  for_each = var.project_hosts

  # Up to 63 characters, ending in the hash so it never ends in a hyphen.
  name = "${substr("${var.name_prefix}-${replace(each.key, ".", "-")}", 0, 54)}-${substr(sha1("${each.key},${var.certificate_epoch}"), 0, 8)}"

  managed {
    domains = [each.key]
  }

  lifecycle {
    create_before_destroy = true
  }
}

# Shared certificate for product subdomains that share the pack. Separate from
# project_hosts so adding a name does not burn a proxy certificate slot per host.
#
# CRITICAL (always-live): the https proxy must NOT point at this cert until it is
# ACTIVE. create_before_destroy alone is not enough — Terraform still swaps the
# proxy onto the new (PROVISIONING) cert and browsers get the wrong SNI
# (elliottwave.dev), which strips CSS/JS from every product host. Seen 14–15 Oct
# 2026 when sla.khensu.nu (NXDOMAIN) was added to the pack.
resource "google_compute_managed_ssl_certificate" "products" {
  count = length(local.product_host_list) == 0 ? 0 : 1

  name = "${var.name_prefix}-products-${substr(sha1(join(",", concat(local.product_host_list, [tostring(var.certificate_epoch)]))), 0, 8)}"

  managed {
    domains = local.product_host_list
  }

  lifecycle {
    create_before_destroy = true
  }
}

# One certificate per isolated product host (own_certificate = true).
resource "google_compute_managed_ssl_certificate" "product_hosts" {
  for_each = local.product_own_hosts

  name = "${substr("${var.name_prefix}-${replace(each.key, ".", "-")}", 0, 54)}-${substr(sha1("${each.key},${var.certificate_epoch}"), 0, 8)}"

  managed {
    domains = [each.key]
  }

  lifecycle {
    create_before_destroy = true
  }
}

# Block proxy updates until the shared products cert is ACTIVE. Without this,
# every product host (bi, waves, …) goes SSL-wrong for the whole provision window.
resource "null_resource" "products_cert_active" {
  count = length(local.product_host_list) == 0 ? 0 : 1

  triggers = {
    cert_id   = google_compute_managed_ssl_certificate.products[0].id
    cert_name = google_compute_managed_ssl_certificate.products[0].name
  }

  provisioner "local-exec" {
    interpreter = ["/bin/bash", "-c"]
    command     = <<-EOT
      set -euo pipefail
      name="${google_compute_managed_ssl_certificate.products[0].name}"
      echo "Waiting for products cert $name to become ACTIVE (proxy stays on the previous cert until then)…"
      for i in $(seq 1 180); do
        st=$(gcloud compute ssl-certificates describe "$name" --global --format='value(managed.status)' 2>/dev/null || echo MISSING)
        echo "  try $i: $st"
        if [ "$st" = "ACTIVE" ]; then
          exit 0
        fi
        sleep 20
      done
      echo "FATAL: $name still not ACTIVE after 60m — refusing to point the https proxy at a PROVISIONING cert" >&2
      exit 1
    EOT
  }
}

resource "google_compute_url_map" "sites" {
  name            = "${var.name_prefix}-sites"
  default_service = google_compute_backend_bucket.sites.id

  lifecycle {
    precondition {
      condition     = length(setintersection(toset(keys(var.project_hosts)), toset(concat(local.hosts, keys(var.apis), keys(var.product_hosts))))) == 0
      error_message = "A project host is already a site, API or product host; a URL map takes each host once."
    }
    precondition {
      condition     = length(setintersection(toset(keys(var.product_hosts)), toset(concat(local.hosts, keys(var.apis))))) == 0
      error_message = "A product host is already a site or API host; a URL map takes each host once."
    }
  }

  dynamic "host_rule" {
    for_each = var.apis
    content {
      hosts        = [host_rule.key]
      path_matcher = "api-${replace(host_rule.key, ".", "-")}"
    }
  }

  dynamic "path_matcher" {
    for_each = var.apis
    content {
      name            = "api-${replace(path_matcher.key, ".", "-")}"
      default_service = path_matcher.value.redirect_host == null ? path_matcher.value.backend_service : null
      dynamic "default_url_redirect" {
        for_each = path_matcher.value.redirect_host == null ? [] : [path_matcher.value.redirect_host]
        content {
          host_redirect          = default_url_redirect.value
          path_redirect          = "/"
          https_redirect         = true
          strip_query            = true
          redirect_response_code = "FOUND"
        }
      }
      path_rule {
        paths   = path_matcher.value.paths
        service = path_matcher.value.backend_service
      }
    }
  }

  # The first host of each site is canonical; the rest (www) redirect to it
  # permanently, keeping path and query.
  dynamic "host_rule" {
    for_each = var.sites
    content {
      hosts        = [host_rule.value.hosts[0]]
      path_matcher = host_rule.key
    }
  }

  dynamic "host_rule" {
    for_each = local.aliased
    content {
      hosts        = host_rule.value.aliases
      path_matcher = "${host_rule.key}-alias"
    }
  }

  dynamic "host_rule" {
    for_each = var.project_hosts
    content {
      hosts        = [host_rule.key]
      path_matcher = host_rule.value
    }
  }

  dynamic "host_rule" {
    for_each = var.product_hosts
    content {
      hosts        = [host_rule.key]
      path_matcher = local.product_matcher[host_rule.key]
    }
  }

  dynamic "path_matcher" {
    for_each = local.aliased
    content {
      name = "${path_matcher.key}-alias"
      default_url_redirect {
        host_redirect          = path_matcher.value.canonical
        https_redirect         = true
        strip_query            = false
        redirect_response_code = "MOVED_PERMANENTLY_DEFAULT"
      }
    }
  }

  dynamic "path_matcher" {
    for_each = local.served
    content {
      name            = path_matcher.key
      default_service = google_compute_backend_bucket.sites.id

      dynamic "route_rules" {
        for_each = contains(keys(var.embed_paths), path_matcher.key) ? [var.embed_paths[path_matcher.key]] : []
        content {
          priority = 0
          service  = google_compute_backend_bucket.embeds[0].id
          match_rules {
            prefix_match = route_rules.value
          }
          route_action {
            url_rewrite {
              path_prefix_rewrite = "/${path_matcher.key}${route_rules.value}"
            }
          }
        }
      }

      route_rules {
        priority = 1
        service  = google_compute_backend_bucket.sites.id
        match_rules {
          full_path_match = "/"
        }
        route_action {
          url_rewrite {
            path_prefix_rewrite = "/${path_matcher.key}/index.html"
          }
        }
      }

      route_rules {
        priority = 2
        service  = google_compute_backend_bucket.sites.id
        match_rules {
          prefix_match = "/"
        }
        route_action {
          url_rewrite {
            path_prefix_rewrite = "/${path_matcher.key}/"
          }
        }
      }

      default_custom_error_response_policy {
        error_service = google_compute_backend_bucket.sites.id
        error_response_rule {
          match_response_codes   = ["403", "404"]
          path                   = "/${path_matcher.key}/404.html"
          override_response_code = 404
        }
      }
    }
  }

  # Product hosts: `/` is the product page; everything else (assets) is the site prefix.
  dynamic "path_matcher" {
    for_each = var.product_hosts
    content {
      name            = local.product_matcher[path_matcher.key]
      default_service = google_compute_backend_bucket.sites.id

      route_rules {
        priority = 1
        service  = google_compute_backend_bucket.sites.id
        match_rules {
          full_path_match = "/"
        }
        route_action {
          # Always the publish-sites.sh alias object (`site` or `site/page`), not `…/index.html`.
          # Rewriting `/` to `…/index.html` makes GCS 301 to that path on the product host, which
          # then gets the asset prefix rewrite and 404s.
          url_rewrite {
            path_prefix_rewrite = path_matcher.value.page == null ? "/${path_matcher.value.site}" : "/${path_matcher.value.site}/${path_matcher.value.page}"
          }
        }
      }

      route_rules {
        priority = 2
        service  = google_compute_backend_bucket.sites.id
        match_rules {
          prefix_match = "/"
        }
        route_action {
          url_rewrite {
            path_prefix_rewrite = "/${path_matcher.value.site}/"
          }
        }
      }

      default_custom_error_response_policy {
        error_service = google_compute_backend_bucket.sites.id
        error_response_rule {
          match_response_codes   = ["403", "404"]
          path                   = "/${path_matcher.value.site}/404.html"
          override_response_code = 404
        }
      }
    }
  }
}

resource "google_compute_ssl_policy" "sites" {
  name            = "${var.name_prefix}-sites"
  profile         = "MODERN"
  min_tls_version = "TLS_1_2"
}

resource "google_compute_target_https_proxy" "sites" {
  name    = "${var.name_prefix}-sites"
  url_map = google_compute_url_map.sites.id
  # Do not attach a replaced products cert until it is ACTIVE (null_resource).
  depends_on = [null_resource.products_cert_active]
  # The shared certificate stays first: it is what a client without SNI gets.
  ssl_certificates = concat(
    [google_compute_managed_ssl_certificate.sites.id],
    google_compute_managed_ssl_certificate.apis[*].id,
    [for host in sort(keys(local.own_api_hosts)) : google_compute_managed_ssl_certificate.api_hosts[host].id],
    [for host in sort(keys(var.project_hosts)) : google_compute_managed_ssl_certificate.projects[host].id],
    google_compute_managed_ssl_certificate.products[*].id,
    [for host in sort(keys(local.product_own_hosts)) : google_compute_managed_ssl_certificate.product_hosts[host].id],
  )
  ssl_policy = google_compute_ssl_policy.sites.id
}

resource "google_compute_global_forwarding_rule" "https" {
  name                  = "${var.name_prefix}-sites-https"
  target                = google_compute_target_https_proxy.sites.id
  ip_address            = google_compute_global_address.sites.id
  port_range            = "443"
  load_balancing_scheme = "EXTERNAL_MANAGED"
}

# Port 80 exists only to send people to 443. Optional because each forwarding
# rule spends one of the project's in-use address quota, which is four on a
# new project. .dev is HSTS-preloaded as a whole TLD, so only the .cloud and
# .studio hostnames lose anything without it.
resource "google_compute_url_map" "redirect" {
  count = var.http_redirect ? 1 : 0

  name = "${var.name_prefix}-sites-redirect"

  default_url_redirect {
    https_redirect         = true
    strip_query            = false
    redirect_response_code = "MOVED_PERMANENTLY_DEFAULT"
  }
}

resource "google_compute_target_http_proxy" "redirect" {
  count = var.http_redirect ? 1 : 0

  name    = "${var.name_prefix}-sites-redirect"
  url_map = google_compute_url_map.redirect[0].id
}

resource "google_compute_global_forwarding_rule" "http" {
  count = var.http_redirect ? 1 : 0

  name                  = "${var.name_prefix}-sites-http"
  target                = google_compute_target_http_proxy.redirect[0].id
  ip_address            = google_compute_global_address.sites.id
  port_range            = "80"
  load_balancing_scheme = "EXTERNAL_MANAGED"
}
