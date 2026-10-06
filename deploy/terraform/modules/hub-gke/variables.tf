variable "project_id" {
  description = "GCP project ID."
  type        = string
}

variable "region" {
  description = "Region for the artifacts bucket."
  type        = string
}

variable "hub_name" {
  description = "Hub name. Used verbatim as the chart's hub.hubId (which drives hub-prefixed Secret Manager naming, so it must equal hub-identity's hub_name) and as the Helm release name."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,15}$", var.hub_name))
    error_message = "hub_name must match ^[a-z][a-z0-9-]{2,15}$."
  }
}

# --- Front-door contract ---
#
# These two are the ONLY values this module takes from the front door. Any
# front (this repo's hub-lb today, a Cloud Run IAP front later) that
# produces them can sit in front of this module. Do not add inputs that
# carry other front-door attributes.

variable "public_url" {
  description = "Front-door contract. The hub's external URL (https://...), rendered as the chart's hub.baseUrl."
  type        = string

  validation {
    condition     = can(regex("^https://[^\\s/]+$", var.public_url))
    error_message = "public_url must be https://<host> with no path or trailing slash."
  }
}

variable "iap_audience" {
  description = "Front-door contract. The IAP JWT audience the hub verifies (the chart's auth.proxy.iap.audience), e.g. /projects/<number>/global/backendServices/<id>."
  type        = string
}

# --- Image ---

variable "image_repository" {
  description = "Hub image repository without tag or digest, e.g. <artifact registry repo url>/scion-hub-gke (the root Dockerfile's hub-gke target)."
  type        = string

  validation {
    condition     = !can(regex("[@:]", element(split("/", var.image_repository), length(split("/", var.image_repository)) - 1)))
    error_message = "image_repository must not carry a tag or digest; pin the image with image_digest."
  }
}

variable "image_digest" {
  description = "Hub image digest (sha256:<64 hex>). Required: the hub is pinned by digest, never by a tag, and never by latest."
  type        = string

  validation {
    condition     = can(regex("^sha256:[0-9a-f]{64}$", var.image_digest))
    error_message = "image_digest must be sha256:<64 lowercase hex>."
  }
}

# --- Identity ---

variable "hub_sa_email" {
  description = "Hub GSA email (hub-identity). Bound to the chart's KSA via Workload Identity (serviceAccount.gcpServiceAccount) and granted objectAdmin on the artifacts bucket."
  type        = string
}

variable "transport_sa_email" {
  description = "Transport SA email (hub-identity), the chart's auth.transport.platformAuthSa."
  type        = string
}

variable "ksa_name" {
  description = "Name of the Kubernetes service account the chart creates for the hub pod. hub-identity's hub_workload_identity_ksa must name the same { namespace, name }; the root passes both from one local."
  type        = string
  default     = "scion-hub"
}

variable "iap_oauth_client_id" {
  description = "The IAP OAuth client ID agents present as their token audience (the chart's auth.transport.oidcAudience). Null skips the Helm release entirely: the chart refuses to render transport mode iap without it, and the hub's HA preflight accepts no other transport mode on this shape. Everything else (namespace, NEG Service, bucket) is still created. Set it and re-apply to install the hub; see the README."
  type        = string
  default     = null

  validation {
    condition     = var.iap_oauth_client_id == null || can(regex("^[0-9a-zA-Z-]+\\.apps\\.googleusercontent\\.com$", var.iap_oauth_client_id))
    error_message = "iap_oauth_client_id must be null or end in .apps.googleusercontent.com."
  }
}

variable "admin_emails" {
  description = "Emails seeded as hub admins (the chart's hub.adminEmails)."
  type        = list(string)
  default     = []
}

# --- Runtime ---

variable "image_registry" {
  description = "Registry the hub rewrites bare agent images against (the chart's agents.imageRegistry)."
  type        = string
}

variable "runtime_namespace" {
  description = "Namespace agent pods run in (agent-runtime-k8s's namespace output). The chart renders its Role/RoleBinding there for the hub's KSA. Pass the module output, not a literal, so the namespace exists before the release."
  type        = string
}

variable "port" {
  description = "Hub container port (the chart's hub.webPort). The NEG Service exposes this port, so NEG endpoints are pod IP:port."
  type        = number
  default     = 8080
}

# --- Database ---

variable "db_name" {
  description = "Postgres database name (cloudsql-database's db_name)."
  type        = string
}

variable "db_user" {
  description = "Postgres user (cloudsql-database's db_user)."
  type        = string
}

variable "db_password" {
  description = "Postgres password (cloudsql-database's db_password). Passed to the chart with set_sensitive only; it lands in the chart's settings Secret."
  type        = string
  sensitive   = true
}

variable "db_password_rotation" {
  description = "The root's db_password_rotation marker. The chart deliberately does not roll pods on a password-only change (its settings checksum excludes the credential), so this module puts the marker, never the password, in a pod annotation: changing the marker rotates the password and rolls the pods in the same apply. Empty adds no annotation."
  type        = string
  default     = ""
}

variable "sql_connection_name" {
  description = "Cloud SQL instance connection name (project:region:instance), the chart's cloudsql.instanceConnectionName."
  type        = string
}

# --- Chart ---

variable "chart_path" {
  description = "Path to the scion-hub Helm chart. Defaults to the in-repo chart."
  type        = string
  default     = null
}

variable "helm_timeout" {
  description = "Seconds Helm waits for the release to become ready. First boot runs schema migrations under a 300s startup-probe budget, plus image pull and Autopilot node provisioning."
  type        = number
  default     = 900
}

variable "boot_prerequisites" {
  description = "Real resource attributes (IDs, never module references) the hub pod must not start before, e.g. the hub GSA's Workload Identity grant and its project IAM grants. Recorded in a terraform_data the Helm release depends on, the same pattern as hub-cloudrun's boot_prerequisites."
  type        = map(string)
  default     = {}
}
