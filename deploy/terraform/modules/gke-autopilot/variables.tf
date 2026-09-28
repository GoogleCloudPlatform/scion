variable "project_id" {
  description = "GCP project ID."
  type        = string
}

variable "region" {
  description = "Region for the regional Autopilot cluster."
  type        = string
}

variable "name_prefix" {
  description = "Shared-infra name prefix. The cluster is named \"<name_prefix>-agents\"."
  type        = string
  default     = "tfha"

  validation {
    condition     = can(regex("^[a-z][a-z0-9]{1,7}$", var.name_prefix))
    error_message = "name_prefix must match ^[a-z][a-z0-9]{1,7}$: it is used verbatim in generated resource names, which have their own length and character-set limits."
  }
}

variable "network_id" {
  description = "Self link / ID of the VPC network (module network output network_id)."
  type        = string
}

variable "subnet_id" {
  description = "Self link / ID of the subnet (module network output subnet_id)."
  type        = string
}

variable "release_channel" {
  description = "GKE release channel."
  type        = string
  default     = "REGULAR"
}

variable "master_authorized_networks" {
  description = "CIDR blocks allowed to reach the public control-plane endpoint. Empty means Google-auth-only with no extra network restriction, which is the default for this module set: the control plane keeps a public endpoint, gated by Google identity rather than a private-endpoint/bastion setup."
  type        = list(string)
  default     = []
}

variable "deletion_protection" {
  description = "GKE cluster deletion protection. This is shared infra used by every hub namespace."
  type        = bool
  default     = true
}
