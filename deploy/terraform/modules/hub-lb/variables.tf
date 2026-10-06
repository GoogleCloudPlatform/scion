variable "project_id" {
  description = "GCP project ID."
  type        = string
}

variable "project_number" {
  description = "GCP project number (shared.project_number from shared-lookup). Used only to build iap_audience; never a literal."
  type        = string

  validation {
    condition     = can(regex("^[0-9]+$", var.project_number))
    error_message = "project_number must be numeric."
  }
}

variable "name" {
  description = "Hub name. Every resource here is named \"<name>-hub-*\"."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,15}$", var.name))
    error_message = "name must match ^[a-z][a-z0-9-]{2,15}$."
  }
}

variable "hostname" {
  description = "Public hostname the hub is served at (the managed certificate's only domain). DNS is external: Terraform creates no record. The certificate stays PROVISIONING until a human points an A record for this name at the lb_ip output; nothing here waits for that."
  type        = string

  validation {
    condition     = can(regex("^([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?\\.)+[a-z]{2,63}$", var.hostname))
    error_message = "hostname must be a lowercase fully qualified DNS name with no scheme, port, path or trailing dot."
  }
}

variable "network" {
  description = "The shared VPC network (name or self link) the GKE cluster's pods live on. The health-check firewall rule is created on it."
  type        = string
}

variable "port" {
  description = "The hub pod's container port. The NEG endpoints are pod IP:port, so the health check and the firewall rule both target this port directly, not a Service port."
  type        = number
  default     = 8080
}

variable "neg_name" {
  description = "Name of the standalone zonal NEGs the GKE NEG controller creates for the hub (one per zone, all with this name). Pass a value that is UNKNOWN until the annotated Kubernetes Service exists (hub-gke's neg_name output is): that is what defers the NEG data reads below to apply time on a fresh hub, instead of reading NEGs that do not exist yet at plan time."
  type        = string
}

variable "neg_zones" {
  description = "Zones to read a NEG from, one data source each (the cluster's node_locations). Every zone listed must actually have the NEG, or the apply fails at the data read; see the README's NEG notes."
  type        = list(string)

  validation {
    condition     = length(var.neg_zones) > 0
    error_message = "neg_zones must list at least one zone."
  }
}

variable "max_rate_per_endpoint" {
  description = "RATE balancing target per pod endpoint (requests/second). GCE_VM_IP_PORT NEG backends on a global external ALB require a RATE target; it is a capacity hint for spreading load, not a hard cap."
  type        = number
  default     = 1000
}

variable "backend_timeout_sec" {
  description = "Backend service timeout. On a global external ALB this is also the maximum lifetime of a WebSocket, so it bounds every web terminal and agent control channel. 86400 (24h) is the platform maximum."
  type        = number
  default     = 86400

  validation {
    condition     = var.backend_timeout_sec >= 1 && var.backend_timeout_sec <= 86400
    error_message = "backend_timeout_sec must be between 1 and 86400."
  }
}

variable "enable_http_redirect" {
  description = "Also listen on port 80 on the same IP and redirect every request to https."
  type        = bool
  default     = true
}

variable "iap_members" {
  description = "Users/groups granted roles/iap.httpsResourceAccessor on this hub's backend service only (e.g. \"user:alice@example.com\", \"group:team@example.com\")."
  type        = list(string)
  default     = []
}

variable "transport_sa_email" {
  description = "The hub's transport SA (hub-identity). Granted roles/iap.httpsResourceAccessor on this backend service only, so the IAP OIDC tokens it mints for agents pass IAP."
  type        = string
}
