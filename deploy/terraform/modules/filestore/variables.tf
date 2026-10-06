variable "project_id" {
  description = "GCP project ID."
  type        = string
}

variable "zone" {
  description = "Zone for the Filestore Basic instance (Basic tier is zonal)."
  type        = string
}

variable "name_prefix" {
  description = "Shared-infra name prefix. The instance is named \"<name_prefix>-nfs\"."
  type        = string
  default     = "tfha"

  validation {
    condition     = can(regex("^[a-z][a-z0-9]{1,7}$", var.name_prefix))
    error_message = "name_prefix must match ^[a-z][a-z0-9]{1,7}$: it is used verbatim in generated resource names, which have their own length and character-set limits."
  }
}

variable "network_name" {
  description = "Name of the VPC network to attach via direct peering (module network output network_name)."
  type        = string
}

variable "tier" {
  description = "Filestore service tier."
  type        = string
  default     = "BASIC_HDD"
}

variable "capacity_gb" {
  description = "Capacity in GiB. Basic tier minimum is 1024 (1 TiB)."
  type        = number
  default     = 1024
}

variable "share_name" {
  description = "Name of the single NFS share on the instance. Per-hub isolation is by subdirectory under this share, not by separate shares (Filestore Basic only supports one share per instance)."
  type        = string
  default     = "scion"
}

variable "deletion_protection" {
  description = "API-level deletion protection (google_filestore_instance.deletion_protection_enabled). This is shared infra: leave true except when intentionally tearing down the whole stack."
  type        = bool
  default     = true
}
