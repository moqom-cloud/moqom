locals {
  # Konlet — the `gce-container-declaration` metadata key — is the usual way to
  # run a container on Container-Optimized OS, and it is the wrong tool here: it
  # exposes no hook for sysctls or ulimits, and a UDP server that keeps the
  # stock socket buffers will drop datagrams under exactly the load it was
  # bought to handle. cloud-init writes the tuning and a plain systemd unit
  # instead, which also makes the drain behaviour on SIGTERM explicit.
  cloud_init = templatefile("${path.module}/templates/cloud-init.yaml.tftpl", {
    image             = var.image
    registry_host     = var.registry_host
    container_env     = var.container_env
    health_check_port = var.health_check_port
    udp_port          = var.udp_port
    stun_port         = var.stun_port == null ? "" : tostring(var.stun_port)

    # The relay fetches these itself at boot, from the metadata server's token.
    # Empty strings rather than null because the template tests them, and a null
    # interpolates as the word "null".
    project_id                = var.project_id
    tls_certificate_secret_id = coalesce(var.tls_certificate_secret_id, "")
    tls_private_key_secret_id = coalesce(var.tls_private_key_secret_id, "")
    credential_secret_id      = coalesce(var.credential_secret_id, "")
    container_uid             = var.container_uid

    # The link's own certificates, which are a different thing from the two
    # above: those are what a phone verifies, these are what the control plane
    # verifies and what this relay verifies the control plane with.
    link_ca_secret_id          = coalesce(var.link_ca_secret_id, "")
    link_client_cert_secret_id = coalesce(var.link_client_cert_secret_id, "")
    link_client_key_secret_id  = coalesce(var.link_client_key_secret_id, "")

    ticket_keys_secret_id = coalesce(var.ticket_keys_secret_id, "")

    # The overlay tunneler, or nothing. See var.ziti.
    ziti_enabled         = var.ziti != null
    ziti_cli_image       = var.ziti == null ? "" : var.ziti.cli_image
    ziti_enroll_url      = var.ziti == null ? "" : var.ziti.enroll_url
    ziti_enroll_audience = var.ziti == null ? "" : var.ziti.enroll_audience
    ziti_poll_seconds    = var.ziti == null ? 60 : var.ziti.service_poll_seconds

    # GCE allows roughly 90 seconds between the delete call and the machine
    # going away, and a live stream is longer than that. This timeout governs
    # the tail of a shutdown that has already been drained at the load balancer;
    # it is not the drain itself. See deploy/README.md for the ordering.
    stop_timeout_sec = 90
  })
}
