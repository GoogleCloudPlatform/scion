variable "project_id" {
  description = "GCP project ID."
  type        = string
}

variable "project_number" {
  description = "GCP project number (shared.project_number from shared-lookup), used for the deterministic Cloud Run URL and IAP audience."
  type        = string
}

variable "region" {
  description = "Region for the Cloud Run service."
  type        = string
}

variable "hub_name" {
  description = "Hub name. The Cloud Run service, bucket, and secrets are all named/derived from this."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,15}$", var.hub_name))
    error_message = "hub_name must match ^[a-z][a-z0-9-]{2,15}$ (design §3.8)."
  }
}

variable "hub_image" {
  description = "Artifact Registry image URI for the hub container."
  type        = string
}

variable "image_registry" {
  description = "Registry the hub rewrites bare agent harness images against, rendered at the TOP LEVEL of settings.yaml (not under server: — design §3.4/§3.6). Without it, bare images like scion-claude:latest are never rewritten, GKE pulls them from Docker Hub where they don't exist, and agent start fails with ImagePullBackOff. Computed by the hub root from shared-lookup's real Artifact Registry resource; this module just renders whatever it's given."
  type        = string
}

variable "hub_sa_email" {
  description = "Hub service account email (Cloud Run runtime identity)."
  type        = string
}

variable "transport_sa_email" {
  description = "Transport service account email (mints IAP OIDC tokens for agents)."
  type        = string
}

variable "hub_iam_grants" {
  description = "hub-identity's hub-SA IAM grant resources' .id values, passed through purely to create a depends_on ordering (design §3.5) for this module's own time_sleep.iam_propagation: the Cloud Run service must not boot before these grants have had time to propagate, or it gets a 403 with no retry. Deliberately list(string) of .id, not list(any) of the full resource objects: google_project_iam_member and google_service_account_iam_member have different attribute shapes, and a list(any) of the full objects fails type unification at this exact variable boundary with \"all list elements must have the same type\" (tf-review; reproduced credential-free with two different hashicorp/random resource types before fixing). .id still carries the same dependency edge."
  type        = list(string)
}

variable "hub_iam_condition_expression" {
  description = "hub-identity's conditioned secretmanager.admin expression, used only as a time_sleep trigger so the propagation wait re-arms if the condition ever changes (e.g. a different hub_name) rather than protecting only the very first apply."
  type        = string
}

variable "boot_prerequisites" {
  description = "F-106 (design §9): map of real resource attributes (never bare input variables or computed strings) that the Cloud Run service must not boot before — the nfs-init Job's own identity (its Job actually finished, not just that its export path string is known), and the cloudsql-database/hub-identity resources this module doesn't otherwise reference directly. Consumed only by terraform_data.boot_prerequisites below, which google_cloud_run_v2_service.hub depends on; no data source may depend on it (see that resource's comment). Replaces a module-level depends_on that used to sit on this module's caller (configurations/hub/main.tf) — that forced Terraform to defer *every* resource and data source inside this module, including this module's old data.google_secret_manager_secret_version.db_password (removed in F-112; see var.db_password), whenever hub-identity/agent-runtime-k8s/cloudsql-database had any pending change, which made the settings secret_data unknown at plan time and forced a spurious replace of the settings secret version (F-106, vm-deploy caught this on a real apply)."
  type        = map(string)
}

variable "hub_scope_secret_hash" {
  description = "hub-identity's 12-char hub-scope secret hash (scion-hub-<hash>-*), used verbatim to pre-provision the OIDC signing key secret ID so it falls under the hub SA's existing conditioned secretmanager.admin grant. Not recomputed here — hub-identity is the one source of truth, shared with its IAM condition."
  type        = string
}

variable "network_name" {
  description = "VPC network name for Direct VPC egress (shared.network.name)."
  type        = string
}

variable "subnet_name" {
  description = "Subnet name for Direct VPC egress (shared.network.subnet_name)."
  type        = string
}

variable "sql_connection_name" {
  description = "Cloud SQL connection name (shared.sql.connection_name)."
  type        = string
}

variable "db_name" {
  description = "Per-hub database name (cloudsql-database output)."
  type        = string
}

variable "db_user" {
  description = "Per-hub database user (cloudsql-database output)."
  type        = string
}

variable "db_password" {
  description = "Per-hub database password (cloudsql-database output db_password, itself random_password.db.result — the value the SQL user was actually created with). Embedded directly in the rendered settings.yaml DSN. Sensitive module input, not a Secret Manager data source (F-112): a data source's read runs at plan time, before cloudsql-database has necessarily created the secret version it would read, and nothing defers that read (F-106, design §9) — a brand-new hub's first plan 404'd on a secret that didn't exist yet. The value is already in state via random_password and the secret version cloudsql-database still writes for operator access, so passing it here directly has no new exposure. Alt-F's secret-env-var end state is phase 3."
  type        = string
  sensitive   = true
  nullable    = false
}

variable "nfs_server" {
  description = "Filestore server IP (shared.nfs.server)."
  type        = string
}

variable "nfs_export" {
  description = "This hub's NFS export path, e.g. \"/scion/<hub_name>\" (agent-runtime-k8s output nfs_export)."
  type        = string
}

variable "nfs_mount_root" {
  description = "Local mount root inside the Cloud Run container. The NFS volume is mounted at \"<nfs_mount_root>/<hub_name>\", which must equal <mount_root>/<share.id> in settings.yaml so NFSMountReconciler takes the already-mounted path (design §3.6, OQ-1)."
  type        = string
  default     = "/mnt/nfs"
}

variable "pv_name" {
  description = "PersistentVolume/claim name for this hub's workspace share (agent-runtime-k8s output pv_name). Embedded in settings.yaml workspace_storage.nfs.shares[0].pv_name."
  type        = string
}

variable "nfs_uid" {
  description = "uid the settings.yaml workspace_storage.nfs block advertises. Single-sourced with agent-runtime-k8s's nfs_uid in the hub root — hardcoding this here separately from what actually chowns the export would silently split the two (found in review)."
  type        = number
  default     = 1000
}

variable "nfs_gid" {
  description = "gid the settings.yaml workspace_storage.nfs block advertises. Single-sourced with agent-runtime-k8s's nfs_gid in the hub root."
  type        = number
  default     = 1000
}

variable "nfs_subpath_root" {
  description = "subpath_root the settings.yaml workspace_storage.nfs block advertises. Single-sourced with agent-runtime-k8s's subpath_root in the hub root."
  type        = string
  default     = "projects"
}

variable "namespace" {
  description = "GKE namespace for this hub's agents (agent-runtime-k8s output namespace, equals hub_name)."
  type        = string
}

variable "gke" {
  description = "Shared GKE cluster coordinates for the kubeconfig secret (shared.gke: { endpoint, ca_certificate })."
  type = object({
    endpoint       = string
    ca_certificate = string
  })
}

variable "iap_oauth_client_id" {
  description = "OAuth client ID, optional (design §3.4 \"IAP and the OAuth client\", ptone 21:55: creating/binding a custom IAP OAuth client is a post-apply console step, not a Terraform input — the IAP OAuth Admin API is shut down for new clients anyway). Not a secret, not managed by Terraform. Feeds exactly one thing: settings.yaml's auth.transport.oidc_audience (design §8: NOT the IAP resource path) — the audience agents' transport tokens must present over IAP. When null, the hub and IAP browser login still work, but agent transport is disabled (transportauth.FromEnv returns a nil token source with no audience) — the check block below warns. Two real values: the project's Google-managed OAuth client ID (read-only discovery, works immediately for in-org users — see the README), or a custom client created by hand in the console for cross-org sign-in."
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
  description = "Emails seeded as hub admins on first boot (settings.yaml server.hub.admin_emails)."
  type        = list(string)
  default     = []
}

variable "min_instances" {
  description = "Cloud Run min instance count. Phase 1 uses 1 (OQ-4: multi-replica in-process broker safety is unconfirmed)."
  type        = number
  default     = 1
}

variable "max_instances" {
  description = "Cloud Run max instance count."
  type        = number
  default     = 3
}

variable "cpu" {
  description = "CPU allocation per instance."
  type        = string
  default     = "1"
}

variable "memory" {
  description = "Memory allocation per instance."
  type        = string
  default     = "1Gi"
}

variable "timeout" {
  description = "Request timeout. 3600s (not the docs' 900s) for long-lived WebSockets/terminals (design §8)."
  type        = string
  default     = "3600s"
}

# F-110: shared bound-check support for hub_write_timeout/broker_write_timeout
# below, co-located here rather than in main.tf's locals so the whole F-110
# change (description, default, validation) reads as one unit. Terraform
# requires each variable's OWN validation condition to reference var.<self>
# directly (a static check, not just "produces a correct boolean") — so this
# local only covers the part that's genuinely shared (var.timeout's own
# format), and each variable's second validation block below still
# references itself directly via a ternary, not indirection through a local
# for its own value.
#
# Ternary, not `&&`, for the ACTUAL bound check (design note, found the hard
# way while writing this): HCL's `&&` short-circuits for its BOOLEAN RESULT,
# but does NOT stop a later operand's function-call ERROR just because an
# earlier operand is false — confirmed offline: `can(regex(...)) &&
# tonumber(bad_string)` still crashes plan with a raw "cannot convert ... to
# number" error, even though the left side is false. Only a genuine
# conditional (`cond ? a : b`) skips evaluating the untaken branch entirely,
# which is what actually avoids calling tonumber()/trimsuffix() on a value
# that failed the format check.
locals {
  cloud_run_timeout_fmt_ok = can(regex("^[0-9]+s$", var.timeout))
}

variable "hub_write_timeout" {
  description = "F-110 (design §9): the hub's http.Server WriteTimeout, rendered as server.hub.write_timeout in settings.yaml (pkg/config/settings_v1.go:552, mapped via time.ParseDuration at settings_v1.go:1617-1620). Defaults to 60s in Go (pkg/config/hub_config.go:672) when unset — too short for a slow agent create (cold Autopilot node or cold image pull; vm-deploy observed 89-114s server-side). The create succeeds, but the hub's response is silently discarded once WriteTimeout fires, and Cloud Run reports 503 for a request that actually worked. Must be a plain \"<N>s\" duration string — Cloud Run's own Duration format, and also a valid Go duration (time.ParseDuration accepts it) — and no larger than var.timeout, the Cloud Run request timeout: Cloud Run would cut the connection first otherwise, making a larger value meaningless. 300s (5 min) comfortably covers the observed 89-114s range with real margin. It does NOT reach the full ~10-minute ceiling waitForPodReady allows (pkg/runtime/k8s_runtime.go:1595-1596, GKE Autopilot can be slow) — the hub's own HTTP client to the co-located broker has a separate, hard-coded 120s timeout (pkg/hub/broker_http_transport.go:73, http.Client{Timeout: 120 * time.Second}, not settings-driven) that this variable cannot reach at all. That is a Go-level finding, reported to tf-lead, not fixed here."
  type        = string
  default     = "300s"

  validation {
    condition     = can(regex("^[0-9]+s$", var.hub_write_timeout))
    error_message = "hub_write_timeout must be a plain \"<N>s\" duration string, e.g. \"300s\"."
  }

  validation {
    condition = (
      can(regex("^[0-9]+s$", var.hub_write_timeout)) && local.cloud_run_timeout_fmt_ok
      ? tonumber(trimsuffix(var.hub_write_timeout, "s")) <= tonumber(trimsuffix(var.timeout, "s"))
      : false
    )
    error_message = "hub_write_timeout must not exceed var.timeout (the Cloud Run request timeout) — Cloud Run would cut the connection first otherwise, and var.timeout itself must be in \"<N>s\" form for this comparison."
  }
}

variable "broker_write_timeout" {
  description = "F-110 (design §9): the co-located broker's http.Server WriteTimeout, rendered as server.broker.write_timeout in settings.yaml (settings_v1.go:582, mapped at settings_v1.go:1681-1684). Defaults to 120s in Go (hub_config.go:685) when unset. vm-deploy's second observed create (\"foo\") took 113.66s — close enough to this default that a slightly slower create would independently fail here even after hub_write_timeout is raised, since the broker's own response write would be cut off before it ever reaches the hub. Same format/bound rule as hub_write_timeout, and the same 300s default for the same reason (comfortable margin over the observed range, still short of the ~10-minute waitForPodReady ceiling for the same hub_write_timeout reason — the hub-broker HTTP client's hard-coded 120s cap, not this value, is what actually stops mattering past ~120s today)."
  type        = string
  default     = "300s"

  validation {
    condition     = can(regex("^[0-9]+s$", var.broker_write_timeout))
    error_message = "broker_write_timeout must be a plain \"<N>s\" duration string, e.g. \"300s\"."
  }

  validation {
    condition = (
      can(regex("^[0-9]+s$", var.broker_write_timeout)) && local.cloud_run_timeout_fmt_ok
      ? tonumber(trimsuffix(var.broker_write_timeout, "s")) <= tonumber(trimsuffix(var.timeout, "s"))
      : false
    )
    error_message = "broker_write_timeout must not exceed var.timeout (the Cloud Run request timeout) — Cloud Run would cut the connection first otherwise, and var.timeout itself must be in \"<N>s\" form for this comparison."
  }
}

variable "extra_settings" {
  description = "Escape hatch for variations to deep-merge extra settings.yaml keys. Not wired up in phase 1 (seam reserved for phase 3)."
  type        = any
  default     = {}
}
