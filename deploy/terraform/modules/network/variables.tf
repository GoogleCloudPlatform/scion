variable "project_id" {
  description = "GCP project ID to create the network in."
  type        = string
}

variable "region" {
  description = "Region for the subnet."
  type        = string
}

variable "name_prefix" {
  description = "Shared-infra name prefix. Every resource this module creates derives its name from this value (e.g. \"<name_prefix>-vpc\", \"<name_prefix>-subnet\", \"<name_prefix>-psa\")."
  type        = string
  default     = "tfha"

  validation {
    condition     = can(regex("^[a-z][a-z0-9]{1,7}$", var.name_prefix))
    error_message = "name_prefix must match ^[a-z][a-z0-9]{1,7}$: it is used verbatim in generated resource names, which have their own length and character-set limits."
  }
}

variable "subnet_cidr" {
  description = "Primary IP range for the subnet."
  type        = string
  default     = "10.10.0.0/20"
}

variable "psa_prefix_length" {
  description = "Prefix length of the reserved range used for the private services access (PSA) VPC peering connection. Consumed only by Cloud SQL's private IP — Filestore connects via DIRECT_PEERING and reserves its own range separately."
  type        = number
  default     = 16
}
