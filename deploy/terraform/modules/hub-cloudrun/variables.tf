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
    error_message = "hub_name must match ^[a-z][a-z0-9-]{2,15}$."
  }
}

variable "hub_image" {
  description = "Artifact Registry image URI for the hub container."
  type        = string
}

variable "image_registry" {
  description = "Registry the hub rewrites bare agent harness images against, rendered at the TOP LEVEL of settings.yaml (not under server:). Without it, bare images like scion-claude:latest are never rewritten, GKE pulls them from Docker Hub where they don't exist, and agent start fails with ImagePullBackOff. Computed by the hub root from shared-lookup's real Artifact Registry resource; this module just renders whatever it's given."
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
  description = "hub-identity's hub-SA IAM grant resources' .id values, passed through purely to create a depends_on ordering for this module's own time_sleep.iam_propagation: the Cloud Run service must not boot before these grants have had time to propagate, or it gets a 403 with no retry. Deliberately list(string) of .id, not list(any) of the full resource objects: google_project_iam_member and google_service_account_iam_member have different attribute shapes, and a list(any) of the full objects fails type unification at this exact variable boundary with \"all list elements must have the same type\" (reproduced credential-free with two different hashicorp/random resource types before fixing). .id still carries the same dependency edge."
  type        = list(string)
}

variable "hub_iam_condition_expression_prefixed" {
  description = "hub-identity's hub-prefixed conditioned secretmanager.admin expression (ptone/scion#2152), used only as a time_sleep trigger so the propagation wait re-arms if the condition ever changes (e.g. a different hub_name) rather than protecting only the very first apply."
  type        = string
}

variable "boot_prerequisites" {
  description = "Map of real resource attributes (never bare input variables or computed strings) that the Cloud Run service must not boot before — the nfs-init Job's own identity (its Job actually finished, not just that its export path string is known), and the cloudsql-database/hub-identity resources this module doesn't otherwise reference directly. Consumed only by terraform_data.boot_prerequisites below, which google_cloud_run_v2_service.hub depends on; no data source may depend on it (see that resource's comment). Replaces a module-level depends_on that used to sit on this module's caller (configurations/hub/main.tf) — that forced Terraform to defer *every* resource and data source inside this module, including a data source that used to read the DB password (since removed, along with the sensitive var.db_password input it was replaced by, once the DSN moved entirely into a dedicated secret — see var.dsn_secret_id/var.dsn_secret_version), whenever hub-identity/agent-runtime-k8s/cloudsql-database had any pending change. That made the settings secret_data unknown at plan time and forced a spurious replace of the settings secret version on a real apply."
  type        = map(string)
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
  description = "Cloud SQL connection name (shared.sql.connection_name). Still needed here for the cloudsql volume's cloud_sql_instance.instances even though it's no longer rendered into settings.yaml."
  type        = string
}

# db_name/db_user/db_password are gone: an earlier embedded-DSN-in-
# settings.yaml approach they existed for was removed. The DSN is now built
# entirely inside cloudsql-database and reaches this module only as the two
# secret coordinates below — this module never sees the plaintext password,
# user or database name at all.

variable "dsn_secret_id" {
  description = "Secret Manager secret ID holding the full DSN (cloudsql-database output dsn_secret_id). Used both for the SCION_SERVER_DATABASE_URL secret env var on google_cloud_run_v2_service.hub and for this module's own hub_reads_dsn accessor grant."
  type        = string
}

variable "dsn_secret_version" {
  description = "The DSN secret's version NUMBER (cloudsql-database output dsn_secret_version) — pinned, not \"latest\", same reasoning as the settings/kubeconfig secret volume version pins: a revision's connection string must never silently change without a new revision."
  type        = string
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
  description = "Local mount root inside the Cloud Run container. The NFS volume is mounted at \"<nfs_mount_root>/<hub_name>\", which must equal <mount_root>/<share.id> in settings.yaml so the runtime broker's NFSMountReconciler treats the volume as already mounted rather than trying (and failing, as a non-root container) to mount it itself."
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
  description = "OAuth client ID, optional. Creating/binding a custom IAP OAuth client is a post-apply console step, not a Terraform input — the IAP OAuth Admin API is shut down for new clients anyway. Not a secret, not managed by Terraform. Feeds exactly one thing: settings.yaml's auth.transport.oidc_audience (NOT the IAP resource path) — the audience agents' transport tokens must present over IAP. When null, the hub and IAP browser login still work, but agent transport is disabled (transportauth.FromEnv returns a nil token source with no audience) — the check block below warns. Two real values: the project's Google-managed OAuth client ID (read-only discovery, works immediately for in-org users — see the README), or a custom client created by hand in the console for cross-org sign-in."
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
  description = "Cloud Run min instance count. Kept at 1: multi-replica safety of the in-process runtime broker is not fully proven — see max_instances below for the one confirmed defect a multi-instance steady state makes worse."
  type        = number
  default     = 1
}

variable "max_instances" {
  description = "Cloud Run max instance count. Default 3. Multi-instance operation needs a hub image built from a commit containing GoogleCloudPlatform/scion#2046 (fixes ptone/scion#2090): when the instance that owns broker affinity goes away (scale-in, revision retire, graceful shutdown), it stamps every project-provider row for the shared broker_id offline, and #2046 adds a per-instance recurring handler that re-marks a broker's provider rows online on every scheduler tick (default 1 minute) for any broker it still holds a live connection to, so there can be a short window - up to about a minute, plus up to 30s of scheduler jitter - before providers are restored. On a hub image built before #2046, set max_instances = 1: without that handler, nothing on the surviving instances stamps the affected rows back online, and agent-create fails with \"Default runtime broker is unavailable\" until an instance (re)connects."
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
  description = "Request timeout. 3600s (not the docs' 900s) for long-lived WebSockets/terminals."
  type        = string
  default     = "3600s"
}

# Shared bound-check support for hub_write_timeout/broker_write_timeout
# below, co-located here rather than in main.tf's locals so the whole
# write-timeout change (description, default, validation) reads as one unit.
# Terraform
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
  description = "The hub's http.Server WriteTimeout, rendered as server.hub.write_timeout in settings.yaml (pkg/config/settings_v1.go:552, mapped via time.ParseDuration at settings_v1.go:1617-1620). Defaults to 60s in Go (pkg/config/hub_config.go:672) when unset — too short for a slow agent create (cold Autopilot node or cold image pull; observed 89-114s server-side). The create succeeds, but the hub's response is silently discarded once WriteTimeout fires, and Cloud Run reports 503 for a request that actually worked. Must be a plain \"<N>s\" duration string — Cloud Run's own Duration format, and also a valid Go duration (time.ParseDuration accepts it) — and no larger than var.timeout, the Cloud Run request timeout: Cloud Run would cut the connection first otherwise, making a larger value meaningless. 300s (5 min) comfortably covers the observed 89-114s range with real margin. It does NOT reach the full ~10-minute ceiling waitForPodReady allows (pkg/runtime/k8s_runtime.go:1595-1596, GKE Autopilot can be slow) — the hub's own HTTP client to the co-located broker has a separate, hard-coded 120s timeout (pkg/hub/broker_http_transport.go:73, http.Client{Timeout: 120 * time.Second}, not settings-driven) that this variable cannot reach at all. That is a Go-level limitation this module cannot work around."
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
  description = "The co-located broker's http.Server WriteTimeout, rendered as server.broker.write_timeout in settings.yaml (settings_v1.go:582, mapped at settings_v1.go:1681-1684). Defaults to 120s in Go (hub_config.go:685) when unset. An observed agent create took 113.66s — close enough to this default that a slightly slower create would independently fail here even after hub_write_timeout is raised, since the broker's own response write would be cut off before it ever reaches the hub. Same format/bound rule as hub_write_timeout, and the same 300s default for the same reason (comfortable margin over the observed range, still short of the ~10-minute waitForPodReady ceiling for the same hub_write_timeout reason — the hub-broker HTTP client's hard-coded 120s cap, not this value, is what actually stops mattering past ~120s today)."
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

variable "max_connections_budget" {
  description = "A per-hub ceiling on this hub's worst-case Postgres connection usage (max_instances * the rendered database.max_open_conns), checked below. The hub root has no visibility into sibling hubs on the same shared Cloud SQL instance (shared-lookup's contract exposes only the instance's own coordinates, not who else is attached to it), so the true budget — sum across every hub on the instance <= the shared instance's max_connections (default 200, configurations/shared-infra's sql_max_connections) — cannot be computed from inside a single hub root. This variable is the operator-supplied stand-in: it is one hub's share of that 200, not the total. Default 40 assumes up to 5 similarly-sized hubs on the shared instance (matching the README's \"roughly 5 dev hubs with headroom\" sizing note). At the module's own max_instances default (3 — see that variable's description) a hub tops out at 30, still leaving headroom under this 40 budget; a hub pinned to max_instances=1 (see that variable's description) uses at most 10. 5 hubs at the 40-budget ceiling would use exactly 200. Operators adding more or larger hubs must lower this per-hub or raise the shared instance's max_connections and pass a matching value here."
  type        = number
  default     = 40
}

# Declared ahead of the seam that will consume it (a future settings-merge
# feature is not implemented yet), not dead code left behind by a removal.
# Kept so the module's variable interface already matches the design
# without a breaking change later.
# tflint-ignore: terraform_unused_declarations
variable "extra_settings" {
  description = "Escape hatch for variations to deep-merge extra settings.yaml keys. Not wired up yet — reserved for a future settings-merge seam."
  type        = any
  default     = {}
}
