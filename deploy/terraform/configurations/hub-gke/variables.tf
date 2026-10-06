variable "project_id" {
  description = "GCP project ID (must match the shared-infra apply)."
  type        = string
}

variable "region" {
  description = "Region the shared infra was created in (must match shared-infra)."
  type        = string
}

variable "zone" {
  description = "Zone the shared Filestore instance was created in (must match shared-infra). Used only to resolve shared infra; the hub's NEG zones always come from the cluster's node_locations."
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
  description = "The GCS backend prefix this apply's state lives at (e.g. \"tfha/hubs/tfha-gke-h3\"), passed alongside -backend-config=\"prefix=...\" at init time, so hub_name's validation can catch applying one hub's vars onto another hub's state."
  type        = string
}

variable "hub_name" {
  description = "This hub's name. Every hub-scoped resource derives from it."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,15}$", var.hub_name))
    error_message = "hub_name must match ^[a-z][a-z0-9-]{2,15}$."
  }

  validation {
    condition     = startswith(var.hub_name, "${var.shared_prefix}-")
    error_message = "hub_name (\"${var.hub_name}\") must start with \"${var.shared_prefix}-\" — this also rules out collisions with the live stack's own resource names."
  }

  validation {
    condition     = var.state_prefix == "${var.shared_prefix}/hubs/${var.hub_name}"
    error_message = "state_prefix (\"${var.state_prefix}\") does not match \"${var.shared_prefix}/hubs/${var.hub_name}\" for hub_name=\"${var.hub_name}\" — this looks like hub_name's variables are about to be applied onto a different hub's state. Re-run init with the matching -backend-config prefix, or fix -var hub_name/-var state_prefix."
  }
}

variable "hostname" {
  description = "Public hostname for this hub (e.g. tfha-gke-h3.example.com). DNS is external: after the apply, create the A record from the dns_record output. The managed certificate stays PROVISIONING until it resolves."
  type        = string
}

variable "hub_image_digest" {
  description = "Digest of the hub-gke image (image-build/cloudbuild-hub-gke.yaml) in the shared Artifact Registry repo, as <repo>/scion-hub-gke@<digest>. Required, with no default: a digest exists only in the registry it was pushed to, so each operator sets the digest of their own build. Pinned by digest only: no tag, never latest. Change this to roll a new hub build."
  type        = string

  validation {
    condition     = can(regex("^sha256:[0-9a-f]{64}$", var.hub_image_digest))
    error_message = "hub_image_digest must be sha256:<64 lowercase hex>."
  }
}

variable "image_registry" {
  description = "Registry the hub rewrites bare agent images against (the chart's agents.imageRegistry). Default null computes to the shared AR repo from shared-lookup."
  type        = string
  default     = null
}

variable "iap_oauth_client_id" {
  description = "The IAP OAuth client ID agents present as their transport token audience. Null means the hub itself is NOT installed (helm_release skipped; output hub_installed = false): the chart refuses to render without it. Everything around the hub is still created, including the load balancer with IAP. See the README's \"First install\"."
  type        = string
  default     = null

  validation {
    condition     = var.iap_oauth_client_id == null || can(regex("^[0-9a-zA-Z-]+\\.apps\\.googleusercontent\\.com$", var.iap_oauth_client_id))
    error_message = "iap_oauth_client_id must be null or end in .apps.googleusercontent.com."
  }
}

variable "iap_members" {
  description = "Users/groups granted roles/iap.httpsResourceAccessor on this hub's backend service only."
  type        = list(string)
  default     = []
}

variable "admin_emails" {
  description = "Emails seeded as hub admins on first boot."
  type        = list(string)
  default     = []
}

variable "db_password_rotation" {
  description = "Passed to cloudsql-database's password_rotation, and to hub-gke, which puts the marker (never the password) in a pod annotation so the pods roll with the new password. Same rules as configurations/hub: set it in this hub's tfvars file to a new value to rotate, and keep it there permanently."
  type        = string
  default     = ""
}

variable "enable_http_redirect" {
  description = "Also listen on port 80 and redirect to https."
  type        = bool
  default     = true
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
