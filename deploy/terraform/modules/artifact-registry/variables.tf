variable "project_id" {
  description = "GCP project ID."
  type        = string
}

variable "region" {
  description = "Region for the Artifact Registry repository."
  type        = string
}

variable "name_prefix" {
  description = "Shared-infra name prefix. The repo is named \"<name_prefix>-scion\"."
  type        = string
  default     = "tfha"

  validation {
    condition     = can(regex("^[a-z][a-z0-9]{1,7}$", var.name_prefix))
    error_message = "name_prefix must match ^[a-z][a-z0-9]{1,7}$: it is used verbatim in generated resource names, which have their own length and character-set limits."
  }
}
