variable "project_id" {
  description = "GCP project ID the test hub runs in. Both service accounts are created here."
  type        = string
}

variable "hub_name" {
  description = "The test hub's name. The accounts are <hub_name>-uat and <hub_name>-issuer, so it must leave room for the longer suffix inside the 30-character service-account ID limit."
  type        = string

  # Service-account IDs are 6-30 characters, start with a letter and end
  # with a letter or digit. "-issuer" is the longer suffix (7 characters), so
  # hub_name may be at most 23. Both suffixes end in a letter, and 3 + "-uat"
  # already meets the 6-character minimum.
  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,22}$", var.hub_name))
    error_message = "hub_name must match ^[a-z][a-z0-9-]{2,22}$: \"<hub_name>-issuer\" must fit the 30-character service-account ID limit."
  }
}

variable "impersonators" {
  description = "IAM members granted roles/iam.serviceAccountOpenIdTokenCreator on both accounts, as serviceAccount:<email> with a lowercase email. Each must be a dedicated service account assigned only to the testing agent; the module cannot check that (see the README)."
  type        = list(string)

  validation {
    condition     = length(var.impersonators) > 0
    error_message = "impersonators must list at least one serviceAccount: member."
  }

  validation {
    condition     = alltrue([for m in var.impersonators : can(regex("^serviceAccount:[a-z0-9._+-]+@[a-z0-9.-]+$", m))])
    error_message = "every impersonator must be serviceAccount:<email>, with the email in lowercase. group:, user: and other member types are rejected: a group or human could mint tokens without a dedicated, auditable identity."
  }
}

variable "backend_service_name" {
  description = "Name of the test hub's IAP-protected backend service. roles/iap.httpsResourceAccessor for the -uat account is granted on this backend service only."
  type        = string
}

variable "alert_notification_channels" {
  description = "Cloud Monitoring notification channel IDs (projects/<project>/notificationChannels/<id>). Non-empty creates a log-based metric and an alert policy for GenerateIdToken calls on either account by a principal outside impersonators. Empty (the default) creates neither."
  type        = list(string)
  default     = []
  nullable    = false
}
