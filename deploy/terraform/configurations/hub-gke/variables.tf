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
  description = "uid that owns this hub's NFS export (agent-runtime-k8s chowns it) and that the hub process runs as (hub-gke's hub_run_as_uid). One variable for both, because the hub writes workspaces that agents later read."
  type        = number
  default     = 1000
}

variable "nfs_gid" {
  description = "gid that owns this hub's NFS export (agent-runtime-k8s chowns it) and that the hub process runs as (hub-gke's hub_run_as_gid). One variable for both, because the hub writes workspaces that agents later read."
  type        = number
  default     = 1000
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

variable "test_identities_enabled" {
  description = "Sets the chart's hub.hubClass = \"test\" and hub.testIdentities.enabled = true, which lets the hub issue test identities. For test hubs only. Default false leaves both chart values unset (the chart's defaults are \"\" and false). Unrelated to test login, which the chart does not offer."
  type        = bool
  default     = false
  nullable    = false
}

variable "test_runner" {
  description = <<-EOT
    Optional test-runner identities (modules/hub-test-runner) so an agent on
    another hub can run tests against this one. Off by default.
      enabled                     create <hub>-uat and <hub>-issuer and their grants
      impersonators               serviceAccount:<email> members (lowercase email) allowed to mint ID tokens for both
                                  accounts; validated only by modules/hub-test-runner (non-empty, serviceAccount:
                                  only), so with enabled = false they are not checked and have no effect
      alert_notification_channels non-empty adds a log-based metric and alert for token minting by anyone else
    Needs iamcredentials DATA_READ audit logs on the project (configurations/shared-infra's
    iamcredentials_data_access_logs); the plan fails without them.
  EOT
  type = object({
    enabled                     = optional(bool, false)
    impersonators               = optional(list(string), [])
    alert_notification_channels = optional(list(string), [])
  })
  default  = {}
  nullable = false
}

# --- High Availability & Separate Conduit Relay ---

variable "hub_replica_count" {
  description = "Number of Hub replicas (default 1; set >= 2 for multi-replica HA). Above 1, Terraform provisions a per-hub internal TLS CA and certificate Secret for the Conduit hop."
  type        = number
  default     = 1
  nullable    = false

  validation {
    condition     = var.hub_replica_count >= 1 && floor(var.hub_replica_count) == var.hub_replica_count
    error_message = "hub_replica_count must be an integer >= 1."
  }
}

variable "conduit_relay_mode" {
  description = "Conduit relay topology: \"in_process\" (default; each Hub pod runs its own relay) or \"external\" (separate --role relay Deployment with HMAC peer auth, internal TLS hop and dedicated <hub_name>-relay GSA/KSA)."
  type        = string
  default     = "in_process"
  nullable    = false

  validation {
    condition     = contains(["in_process", "external"], var.conduit_relay_mode)
    error_message = "conduit_relay_mode must be \"in_process\" or \"external\"."
  }
}

variable "relay_replica_count" {
  description = "Number of separate Conduit Relay replicas when conduit_relay_mode is \"external\"."
  type        = number
  default     = 2
  nullable    = false

  validation {
    condition     = var.relay_replica_count >= 1 && floor(var.relay_replica_count) == var.relay_replica_count
    error_message = "relay_replica_count must be an integer >= 1."
  }
}

variable "enable_hub_pdb" {
  description = "Enable the Hub Deployment's PodDisruptionBudget (hub.pdb.enabled). Requires hub_replica_count >= 2."
  type        = bool
  default     = false
  nullable    = false

  validation {
    condition     = !var.enable_hub_pdb || var.hub_replica_count >= 2
    error_message = "enable_hub_pdb = true requires hub_replica_count >= 2."
  }
}

variable "enable_relay_pdb" {
  description = "Enable the Conduit Relay Deployment's PodDisruptionBudget when conduit_relay_mode is \"external\" (relay.pdb.enabled). Requires conduit_relay_mode = \"external\" and relay_replica_count >= 2."
  type        = bool
  default     = false
  nullable    = false

  validation {
    condition     = !var.enable_relay_pdb || (var.conduit_relay_mode == "external" && var.relay_replica_count >= 2)
    error_message = "enable_relay_pdb = true requires conduit_relay_mode = \"external\" and relay_replica_count >= 2."
  }
}

variable "enable_relay_db_user" {
  description = "Create a dedicated Cloud SQL user \"<hub_name>-relay\" and wire it to separate Conduit Relay pods when conduit_relay_mode is \"external\". The chart also sets server.database.relay_role on the Hub so Hub schema migration applies the least-privilege PostgreSQL table grants automatically on first boot and every upgrade. google_sql_user.relay sets deletion_policy = \"ABANDON\" so turning enable_relay_db_user off (including rolling conduit_relay_mode back to \"in_process\") or destroying the hub does not fail on PostgreSQL's DROP ROLE privilege check; see README.md for the optional REVOKE + DROP ROLE cleanup. Default false reuses the hub's <hub_name> DB user so existing hubs see zero plan diff."
  type        = bool
  default     = false
  nullable    = false

  validation {
    condition     = !var.enable_relay_db_user || var.conduit_relay_mode == "external"
    error_message = "enable_relay_db_user = true requires conduit_relay_mode = \"external\" (in_process mode runs no separate relay pods, so a dedicated relay DB user and secrets would go unused)."
  }
}
