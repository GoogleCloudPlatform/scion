variable "project_id" {
  description = "GCP project ID (e.g. my-project)."
  type        = string
}

variable "region" {
  description = "Region for regional resources (Cloud SQL, GKE, Artifact Registry)."
  type        = string
}

variable "zone" {
  description = "Zone for the Filestore Basic instance (Basic tier is zonal)."
  type        = string
}

variable "name_prefix" {
  description = "Shared-infra name prefix. Every resource this configuration creates derives its name from this value, so a name collision fails the apply instead of silently adopting an existing resource."
  type        = string
  default     = "tfha"

  validation {
    condition     = can(regex("^[a-z][a-z0-9]{1,7}$", var.name_prefix))
    error_message = "name_prefix must match ^[a-z][a-z0-9]{1,7}$."
  }
}

variable "subnet_cidr" {
  description = "Primary IP range for the shared subnet."
  type        = string
  default     = "10.10.0.0/20"
}

variable "psa_prefix_length" {
  description = "Prefix length of the PSA VPC peering range."
  type        = number
  default     = 16
}

variable "sql_tier" {
  description = "Cloud SQL machine tier."
  type        = string
  default     = "db-custom-1-3840"
}

variable "sql_availability_type" {
  description = "ZONAL for a smallest-viable/dev deployment; REGIONAL for HA (the default)."
  type        = string
  default     = "REGIONAL"

  validation {
    condition     = contains(["ZONAL", "REGIONAL"], var.sql_availability_type)
    error_message = "sql_availability_type must be ZONAL or REGIONAL."
  }
}

variable "sql_max_connections" {
  description = "Postgres max_connections. Must be sized across every hub sharing this instance — see the README's shared-infra trust domain and sizing section."
  type        = number
  default     = 200
}

variable "filestore_tier" {
  description = "Filestore service tier."
  type        = string
  default     = "BASIC_HDD"
}

variable "filestore_capacity_gb" {
  description = "Filestore capacity in GiB. Basic minimum is 1024."
  type        = number
  default     = 1024
}

variable "filestore_share_name" {
  description = "Name of the single NFS share. Hubs get subdirectories under it, not separate shares."
  type        = string
  default     = "scion"
}

variable "gke_release_channel" {
  description = "GKE release channel."
  type        = string
  default     = "REGULAR"
}

variable "gke_master_authorized_networks" {
  description = "CIDR blocks allowed to reach the public GKE control-plane endpoint. Empty (the default) means the public endpoint accepts only Google-authenticated callers, with no CIDR allowlist."
  type        = list(string)
  default     = []
}

variable "deletion_protection" {
  description = "API-level and Terraform-level deletion protection, applied uniformly to the Cloud SQL instance, Filestore instance and GKE cluster. Leave true always, except for the deliberate, single destroy apply documented in the README's destroy runbook — the destroy_guard interlock refuses to let this go false while any hub database still exists."
  type        = bool
  default     = true
}
