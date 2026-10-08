variable "project_id" {
  type        = string
  description = "Project the recorder pool runs in."
}

variable "name_prefix" {
  type        = string
  description = "Prefix for resource names, e.g. moqom-dev."
}

variable "namespace" {
  type        = string
  description = "Kubernetes namespace, normally the control plane's."
}

variable "image" {
  type        = string
  description = "The recorder image (moqom-engine/Dockerfile.recorder), pinned by digest. Null creates the identities and grants and no Deployment."
  default     = null
}

variable "replicas" {
  type        = number
  description = "Workers. Each records one room at a time, so this is the number of rooms that can be recorded at once; a recording nobody has claimed waits as PENDING."
  default     = 1
}

variable "recording_bucket" {
  type        = string
  description = "The bucket recordings go to (module.data.recording_bucket). Granted here, not created: the data module owns its lifecycle and retention."
}

variable "control_service_account" {
  type        = string
  description = "The control plane's Google service account email. It is allowed to sign download links as the reader account, and nothing else here."
}

variable "control_endpoint" {
  type        = string
  description = "The control plane's gRPC endpoint, https://host:port."
}

variable "control_server_name" {
  type        = string
  description = "The name on the control plane's certificate."
  default     = "moqom-control"
}

variable "relay" {
  type        = string
  description = "The relay the workers record through, host:port."
}

variable "relay_name" {
  type        = string
  description = "The name the relay's certificate is checked against. Null uses the host part of var.relay."
  default     = null
}

variable "kubernetes_secret" {
  type        = string
  description = <<-EOT
    The Kubernetes secret the pod mounts, populated out of band from the Secret
    Manager entries below, as the control plane's is. Keys: recorder-key.pem
    (the ES256 PKCS#8 key), credential (the is_recorder service-account
    credential), link-ca.pem, link-cert.pem, link-key.pem (RelayLink's private
    authority and a client certificate it issued), and relay-cert.pem when the
    relay's certificate is pinned.
  EOT
}

variable "pin_relay_certificate" {
  type        = bool
  description = "Verify the relay against relay-cert.pem from the secret rather than the system roots."
  default     = true
}

variable "spool_size" {
  type        = string
  description = "Size limit of the spool volume. A worker holds at most the open segments of one room plus whatever is waiting to upload; with the default 256 MiB segments, 10Gi is a room of about thirty tracks with headroom."
  default     = "10Gi"
}

variable "segment_mib" {
  type        = number
  description = "Rotate a track's file at the next group boundary past this many MiB."
  default     = 256
}

variable "cpu" {
  type        = string
  description = "CPU per worker. The recorder copies bytes; it does not decode or encode."
  default     = "500m"
}

variable "memory" {
  type        = string
  description = "Memory per worker. Bounded by track count, not by recording length: about 64 objects buffered per track and the CMAF reorder window."
  default     = "512Mi"
}

variable "termination_grace_seconds" {
  type        = number
  description = "Long enough to close the open segments, upload them and report. A pod killed sooner leaves its last segments in the spool and the recording resumes elsewhere from the next one."
  default     = 120
}

variable "run_as_user" {
  type        = number
  description = "Numeric UID the container runs as; 65532 is distroless nonroot."
  default     = 65532
}

variable "labels" {
  type        = map(string)
  description = "Labels applied to resources that take them."
  default     = {}
}
