variable "project_id" {
  description = "GCP project ID."
  type        = string
}

variable "instance_name" {
  description = "Name of the shared Cloud SQL instance (shared.sql.instance_name from shared-lookup)."
  type        = string
}

variable "hub_name" {
  description = "Hub name. The database is \"<hub_name with - replaced by _>\" and the user is \"<hub_name>\"."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,15}$", var.hub_name))
    error_message = "hub_name must match ^[a-z][a-z0-9-]{2,15}$ (design §3.8)."
  }
}

variable "deletion_policy" {
  description = "Optional google_sql_database deletion_policy override (e.g. \"ABANDON\"). Left unset (provider default) unless phase 1 validation shows DROP failing because the hub user owns objects; see the validation file's \"Design deltas\" section."
  type        = string
  default     = null
}

variable "hub_sa_email" {
  description = "Hub service account email, granted secretAccessor on this hub's db-password secret only (design §3.4 IAM scope rule — one of the 4 per-secret grants replacing the removed project-wide secretmanager.admin)."
  type        = string
}

variable "sql_connection_name" {
  description = "Cloud SQL connection name (shared.sql.connection_name from shared-lookup), used to build the DSN's ?host=/cloudsql/<connection name> suffix (Alt-F, design §6 OQ-11)."
  type        = string
}

variable "password_rotation" {
  description = "F-115 rotation design (12:35Z): set in the hub's tfvars file (not -var) to a new value (e.g. a date, \"2026-09-28\") to rotate the password; changing it replaces the password, updates the SQL user, writes new secret versions and rolls a new hub revision. Keep the marker in the tfvars file permanently once set — never remove it or reset it to \"\", either of which takes keepers back to null and triggers another, unplanned rotation. Default \"\" is a no-op for existing state (random_password.db's keepers stay null, so no diff)."
  type        = string
  default     = ""
}
