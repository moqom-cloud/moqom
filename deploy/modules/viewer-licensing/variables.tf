variable "project_id" {
  type = string
}

variable "region" {
  type    = string
  default = "us-west1"
}

variable "name" {
  type        = string
  description = "Cloud Run service, service account, NEG and backend service name; prefix of the secrets."
  default     = "viewer-licensing"
}

variable "image" {
  type        = string
  description = "Container image, built from services/licensing in the viewer repo."
}

variable "firestore_location" {
  type        = string
  description = "Firestore database location. Can't be changed after creation."
  default     = "us-west1"
}

variable "firestore_database" {
  type    = string
  default = "viewer-licensing"
}

variable "docs_url" {
  type    = string
  default = "https://viewer.elliottwave.studio"
}

variable "allowed_origins" {
  type        = list(string)
  description = "Origins allowed to call the browser-facing POST routes (checkout, licence page)."
  default     = ["https://viewer.elliottwave.studio"]
}

variable "stripe_price_yearly" {
  type        = string
  description = "Stripe price ID for the $99/year subscription. Not secret. Empty until the owner creates the price."
  default     = ""
}

variable "stripe_price_lifetime" {
  type        = string
  description = "Stripe price ID for the $500 one-time licence. Not secret."
  default     = ""
}

variable "smtp" {
  type = object({
    host     = string
    port     = optional(string, "587")
    username = string
    from     = optional(string, "3D Viewer <licensing@elliottwave.studio>")
  })
  description = "Outgoing mail. Null: no email is sent (the success page still shows the key). The password goes in the <name>-smtp-password secret."
  default     = null
}

variable "max_instances" {
  type    = number
  default = 3
}

variable "deletion_protection" {
  type    = bool
  default = true
}

variable "labels" {
  type    = map(string)
  default = {}
}
