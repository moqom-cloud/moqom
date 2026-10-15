variable "name_prefix" {
  type        = string
  description = "Prefix for every resource name."
}

variable "location" {
  type        = string
  description = "Bucket location. A multi-region, since the CDN serves from the edge anyway."
  default     = "US"
}

variable "sites" {
  type = map(object({
    hosts = list(string)
  }))
  description = "Site key to the hostnames it answers on. The key names the bucket."
}

variable "project_hosts" {
  type        = map(string)
  description = "Hostname to the site directory under sites/ it serves, e.g. a lab project's docs at <project>.elliottwave.studio. Each hostname gets a certificate of its own, so adding one never re-issues the certificate the other sites are on."
  default     = {}

  validation {
    condition     = alltrue([for site in values(var.project_hosts) : can(regex("^[a-z][-a-z0-9]{0,62}$", site))])
    error_message = "Site directories are lowercase letters, digits and hyphens, starting with a letter: they name the bucket prefix and the URL map's path matcher."
  }
}

variable "product_hosts" {
  type = map(object({
    site = string
    # When set, `/` on this host serves `/{site}/{page}/`; assets still come from `/{site}/`.
    page = optional(string)
    # Own cert (like project_hosts): adding this host never recreates the shared
    # products certificate. Use for any new subdomain until dig A → sites LB is
    # proven, or forever when you cannot risk the shared pack going PROVISIONING.
    own_certificate = optional(bool, false)
  }))
  description = "Product subdomains (e.g. waves.khensu.nu → khensu-nu/waves). Shared certificate by default; set own_certificate to isolate a host from the pack."
  default     = {}

  validation {
    condition = alltrue([
      for cfg in values(var.product_hosts) :
      can(regex("^[a-z][-a-z0-9]{0,62}$", cfg.site)) && (cfg.page == null || can(regex("^[a-z][-a-z0-9]{0,62}$", cfg.page)))
    ])
    error_message = "product_hosts site and page are lowercase letters, digits and hyphens, starting with a letter."
  }
}

variable "embed_paths" {
  type        = map(string)
  description = "Site directory to the one path prefix other sites may frame, e.g. { viewer = \"/embed/\" }. Everything else keeps X-Frame-Options: DENY."
  default     = {}

  validation {
    condition     = alltrue([for prefix in values(var.embed_paths) : can(regex("^/[-a-z0-9/]*/$", prefix)) && prefix != "/"])
    error_message = "An embed prefix starts and ends with a slash and is not the whole site."
  }
}

variable "apis" {
  type = map(object({
    backend_service = string
    # Where anything outside paths is sent, so the API host serves nothing
    # else. Null sends it to the backend service too, for a host whose
    # security policy answers the rest.
    redirect_host = optional(string)
    paths         = optional(list(string), ["/v1/*"])
    # A certificate of its own instead of a place on the shared API one, so
    # adding the host never re-issues the certificate live APIs are using.
    own_certificate = optional(bool, false)
  }))
  description = "API hostname to the backend service that serves its /v1/ routes. Certified separately from the sites, so a name still waiting on DNS never holds up theirs."
  default     = {}
}

variable "http_redirect" {
  type        = bool
  description = "Serve port 80 as a redirect to HTTPS. Costs one in-use address from the project quota."
  default     = false
}

variable "certificate_epoch" {
  description = "Bump to re-issue the managed certificate, e.g. after DNS was fixed while domains sat in FAILED_NOT_VISIBLE backoff."
  type        = number
  default     = 0
}
