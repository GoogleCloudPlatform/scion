variable "project_id" {
  description = "GCP project ID."
  type        = string
}

variable "region" {
  description = "Region for the Cloud SQL instance."
  type        = string
}

variable "name_prefix" {
  description = "Shared-infra name prefix. The instance is named \"<name_prefix>-pg\"."
  type        = string
  default     = "tfha"

  validation {
    condition     = can(regex("^[a-z][a-z0-9]{1,7}$", var.name_prefix))
    error_message = "name_prefix must match ^[a-z][a-z0-9]{1,7}$."
  }
}

variable "network_id" {
  description = "Self link / ID of the VPC network to attach via private IP (from the network module)."
  type        = string
}

variable "psa_connection" {
  description = "The google_service_networking_connection resource from the network module. Passed through purely to create a depends_on ordering: the PSA peering must exist before the private-IP Cloud SQL instance is created."
  type        = any
  default     = null
}

variable "tier" {
  description = "Cloud SQL machine tier."
  type        = string
  default     = "db-custom-1-3840"
}

variable "edition" {
  description = "Cloud SQL edition."
  type        = string
  default     = "ENTERPRISE"
}

variable "availability_type" {
  description = "ZONAL for a smallest-viable/dev stack; REGIONAL for HA (the default)."
  type        = string
  default     = "REGIONAL"

  validation {
    condition     = contains(["ZONAL", "REGIONAL"], var.availability_type)
    error_message = "availability_type must be ZONAL or REGIONAL."
  }
}

variable "max_connections" {
  description = "Postgres max_connections database flag. Must be sized for the sum of all hubs' connection pools."
  type        = number
  default     = 200
}

variable "deletion_protection" {
  description = "Terraform + GCP deletion protection on the instance. This is shared infra: leave true except when intentionally tearing down the whole stack."
  type        = bool
  default     = true
}
