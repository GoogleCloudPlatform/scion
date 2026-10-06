variable "project_id" {
  description = "GCP project ID (must match the shared-infra apply)."
  type        = string
}

variable "region" {
  description = "Region the shared infra was created in (must match shared-infra)."
  type        = string
}

variable "zone" {
  description = "Zone the shared Filestore instance was created in (must match shared-infra)."
  type        = string
}

variable "shared_prefix" {
  description = "The name_prefix configurations/shared-infra was applied with. Resolves shared infra by naming convention (shared-lookup)."
  type        = string
  default     = "tfha"
}

variable "shared_share_name" {
  description = "Filestore share_name from shared-infra (module filestore's share_name, default \"scion\")."
  type        = string
  default     = "scion"
}

variable "state_prefix" {
  description = "The GCS backend prefix this apply's state actually lives at (e.g. \"tfha/hubs/tfha-h1\"), passed alongside -backend-config=\"prefix=...\" at init time. Terraform cannot read its own backend config, so this is how hub_name's validation below catches applying hub_name=X's vars onto a different hub's state file."
  type        = string
}

variable "hub_name" {
  description = "This hub's name. Every hub-scoped resource derives from it."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,15}$", var.hub_name))
    error_message = "hub_name must match ^[a-z][a-z0-9-]{2,15}$."
  }

  # Denylist by construction: hub_name must live in this project prefix's
  # own namespace. Without this, values like "scion-hub" or "postgres" pass
  # the regex above and collide with the live stack's own Cloud Run service
  # name or the destroy_guard's literal database filter (found in review).
  validation {
    condition     = startswith(var.hub_name, "${var.shared_prefix}-")
    error_message = "hub_name (\"${var.hub_name}\") must start with \"${var.shared_prefix}-\" — this also rules out collisions with the live stack's own resource names."
  }

  # Cross-variable validation (needs Terraform >= 1.9, see versions.tf):
  # state_prefix must exactly match this hub's expected backend prefix. A
  # `check` block was considered and rejected: it only warns, so a mismatched
  # apply would still proceed and silently apply hub_name=X's variables onto
  # a different hub's state.
  validation {
    condition     = var.state_prefix == "${var.shared_prefix}/hubs/${var.hub_name}"
    error_message = "state_prefix (\"${var.state_prefix}\") does not match \"${var.shared_prefix}/hubs/${var.hub_name}\" for hub_name=\"${var.hub_name}\" — this looks like hub_name's variables are about to be applied onto a different hub's state. Re-run init with the matching -backend-config prefix, or fix -var hub_name/-var state_prefix."
  }
}

variable "db_password_rotation" {
  description = "Passed through to cloudsql-database's password_rotation. Set in this hub's tfvars file (not -var) to a new value (e.g. a date) to rotate this hub's DB password; changing it replaces the password, updates the SQL user, writes new secret versions and rolls a new hub revision. Keep the marker in the tfvars file permanently once set — never remove it or reset it to \"\", either of which triggers another, unplanned rotation. Default \"\" is a no-op for existing state. See the README's \"Rotating a hub's DB password\" section."
  type        = string
  default     = ""
}

variable "hub_image" {
  description = "Artifact Registry image URI for the hub container (built and pushed after the shared-infra apply creates the repo)."
  type        = string
}

variable "image_registry" {
  description = "Registry the hub rewrites bare agent harness images against (settings.yaml top-level image_registry: without it, bare images like scion-claude:latest are never rewritten, GKE pulls them from Docker Hub where they don't exist, and agent start fails with ImagePullBackOff). Default null computes to the shared AR repo (<region>-docker.pkg.dev/<project>/<shared_prefix>-scion) from shared-lookup; override only for a variation that publishes agent images elsewhere."
  type        = string
  default     = null
}

variable "iap_oauth_client_id" {
  description = "OAuth client ID, optional. Not a Terraform-managed prerequisite — see the README's \"IAP OAuth client\" section: discover the project's Google-managed client ID (works immediately for in-org users) or create a custom one in the console for cross-org, then re-apply. Null means the hub and IAP browser login work, but agent transport is disabled."
  type        = string
  default     = null

  validation {
    condition     = var.iap_oauth_client_id == null || can(regex("^[0-9a-zA-Z-]+\\.apps\\.googleusercontent\\.com$", var.iap_oauth_client_id))
    error_message = "iap_oauth_client_id must be null or end in .apps.googleusercontent.com."
  }
}

variable "iap_members" {
  description = "Users/groups granted roles/iap.httpsResourceAccessor on this hub's Cloud Run service only."
  type        = list(string)
  default     = []
}

variable "admin_emails" {
  description = "Emails seeded as hub admins on first boot."
  type        = list(string)
  default     = []
}

variable "min_instances" {
  description = "Cloud Run min instance count. Kept at 1: on the k8s runtime each instance runs its own control-channel client and heartbeat loop, and while dispatch, status and the scheduler are already safe across replicas, a multi-instance steady state makes the ptone/scion#2090 scale-in defect (see max_instances below) fire more often."
  type        = number
  default     = 1
}

variable "max_instances" {
  description = "Cloud Run max instance count. Default 3 — see hub-cloudrun's max_instances description: multi-instance operation needs a hub image built from a commit containing GoogleCloudPlatform/scion#2046 (fixes ptone/scion#2090). On an older hub image without that fix, set max_instances = 1."
  type        = number
  default     = 3
}

variable "cpu" {
  type    = string
  default = "1"
}

variable "memory" {
  type    = string
  default = "1Gi"
}

variable "timeout" {
  description = "Request timeout. 3600s for long-lived WebSockets/terminals."
  type        = string
  default     = "3600s"
}

variable "hub_write_timeout" {
  description = "Hub http.Server WriteTimeout, rendered into settings.yaml (server.hub.write_timeout). See hub-cloudrun's own variable for the full rationale and its validation (format + bounded by var.timeout) — this is a plain pass-through."
  type        = string
  default     = "300s"
}

variable "broker_write_timeout" {
  description = "Co-located broker http.Server WriteTimeout, rendered into settings.yaml (server.broker.write_timeout). See hub-cloudrun's own variable for the full rationale and its validation (format + bounded by var.timeout) — this is a plain pass-through."
  type        = string
  default     = "300s"
}

variable "nfs_uid" {
  type    = number
  default = 1000
}

variable "nfs_gid" {
  type    = number
  default = 1000
}

variable "nfs_subpath_root" {
  type    = string
  default = "projects"
}

variable "nfs_capacity" {
  description = "Advertised PV/PVC capacity (bookkeeping only; Filestore Basic doesn't enforce per-subdirectory quotas)."
  type        = string
  default     = "1Ti"
}
