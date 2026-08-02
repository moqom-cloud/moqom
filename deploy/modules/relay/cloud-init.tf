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

    # The relay fetches these itself at boot, from the metadata server's token.
    # Empty strings rather than null because the template tests them, and a null
    # interpolates as the word "null".
    project_id                = var.project_id
    tls_certificate_secret_id = coalesce(var.tls_certificate_secret_id, "")
    tls_private_key_secret_id = coalesce(var.tls_private_key_secret_id, "")
    credential_secret_id      = coalesce(var.credential_secret_id, "")
    container_uid             = var.container_uid

    # GCE allows roughly 90 seconds between the delete call and the machine
    # going away, and a live stream is longer than that. This timeout governs
    # the tail of a shutdown that has already been drained at the load balancer;
    # it is not the drain itself. See deploy/README.md for the ordering.
    stop_timeout_sec = 90
  })
}
