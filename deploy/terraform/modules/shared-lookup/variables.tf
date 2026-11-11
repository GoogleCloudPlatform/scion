variable "project_id" {
  description = "GCP project ID that hosts the shared infra."
  type        = string
}

variable "region" {
  description = "Region the shared infra was created in (Cloud SQL, GKE)."
  type        = string
}

variable "zone" {
  description = "Zone the shared Filestore instance was created in."
  type        = string
}

variable "shared_prefix" {
  description = "The name_prefix that configurations/shared-infra was applied with. Resolves shared resources by naming convention: \"<shared_prefix>-vpc\", \"<shared_prefix>-subnet\", \"<shared_prefix>-pg\", \"<shared_prefix>-nfs\", \"<shared_prefix>-agents\"."
  type        = string
  default     = "tfha"
}

variable "share_name" {
  description = "Name of the NFS share on the shared Filestore instance (module filestore's share_name, default \"scion\")."
  type        = string
  default     = "scion"
}
