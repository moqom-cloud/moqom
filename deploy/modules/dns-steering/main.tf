# Sends a client to the nearest relay region.
#
# This is the compensating control for GCP having no anycast UDP. A Cloud DNS
# geolocation policy resolves the edge name to whichever region's forwarding
# rule is closest to the resolver, and Premium Tier networking then admits the
# client at the nearest Google PoP, so the public-internet leg is short even
# when the region is not.
#
# Two things it does not do. Health-checked routing policies in Cloud DNS only
# support internal load balancers, so a region that goes dark keeps being handed
# out until a human or an external controller rewrites the record; the TTL is
# the floor on that, not the whole of it. And geography is a proxy for latency
# that resolver location makes unreliable — a client on a public resolver looks
# like it lives wherever that resolver terminates.
#
# Both are solved by the `moqom-director` steering service, which can measure
# rather than infer and can hand a client a specific relay after a probe. Until
# that exists, this is the honest approximation, and it stays afterwards as the
# bootstrap endpoint the director itself is discovered through.

resource "google_dns_record_set" "single" {
  count = length(var.endpoints) > 1 ? 0 : 1

  project      = var.project_id
  managed_zone = var.zone_name
  name         = var.record_name
  type         = "A"
  ttl          = var.ttl
  rrdatas      = values(var.endpoints)
}

resource "google_dns_record_set" "geo" {
  count = length(var.endpoints) > 1 ? 1 : 0

  project      = var.project_id
  managed_zone = var.zone_name
  name         = var.record_name
  type         = "A"
  ttl          = var.ttl

  routing_policy {
    dynamic "geo" {
      for_each = var.endpoints
      content {
        location = geo.key
        rrdatas  = [geo.value]
      }
    }
  }
}

# The same steering for IPv6. A client with both looks up both and races them
# (Happy Eyeballs), so a region that serves only IPv4 is still reached.
resource "google_dns_record_set" "single_v6" {
  count = length(var.endpoints_v6) == 1 ? 1 : 0

  project      = var.project_id
  managed_zone = var.zone_name
  name         = var.record_name
  type         = "AAAA"
  ttl          = var.ttl
  rrdatas      = values(var.endpoints_v6)
}

resource "google_dns_record_set" "geo_v6" {
  count = length(var.endpoints_v6) > 1 ? 1 : 0

  project      = var.project_id
  managed_zone = var.zone_name
  name         = var.record_name
  type         = "AAAA"
  ttl          = var.ttl

  routing_policy {
    dynamic "geo" {
      for_each = var.endpoints_v6
      content {
        location = geo.key
        rrdatas  = [geo.value]
      }
    }
  }
}
