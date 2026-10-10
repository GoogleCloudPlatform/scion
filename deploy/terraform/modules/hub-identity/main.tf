# Per-hub service accounts and additive IAM. All grants use
# google_*_iam_member exclusively: never *_iam_binding or
# *_iam_policy, which would be authoritative and strip the live stack's or
# other hubs' existing grants.

resource "google_service_account" "hub" {
  project      = var.project_id
  account_id   = "${var.hub_name}-hub"
  display_name = "Scion hub (${var.hub_name}) — Cloud Run runtime identity"
}

resource "google_service_account" "transport" {
  project      = var.project_id
  account_id   = "${var.hub_name}-transport"
  display_name = "Scion hub (${var.hub_name}) — transport identity, mints IAP OIDC tokens for agents"
}

resource "google_service_account" "agent" {
  project      = var.project_id
  account_id   = "${var.hub_name}-agent"
  display_name = "Scion hub (${var.hub_name}) — agent pod identity (Workload Identity)"
}

# --- Hub SA: project-level grants ---
#
# IAM scope rule: a separate rule from the "additive grants only" rule above
# governs what a grant *hands out*, not just how it's expressed. No
# project-wide grant may reach resources outside this hub's own names. A
# shared project can hold many hubs' Secret Manager secrets and IAP
# resources, including other live hubs' user_signing_key/agent_signing_key —
# a project-wide roles/secretmanager.admin would let this hub's SA read and
# delete them.
#
# Replaced by:
#  (a) per-secret secretAccessor on this hub's own Terraform-managed secrets
#      — <hub>-settings/-session-secret/-kubeconfig (hub-cloudrun) and
#      <hub>-db-password (cloudsql-database), not here;
#  (b) hub_secretmanager_admin_hub_prefixed below, a conditioned grant scoped
#      to exactly the resource-name prefix this hub's own secrets fall under
#      — never another hub's or the live stack's. It covers every scope —
#      hub, user, and project alike — under the hub-prefixed Secret Manager
#      naming from ptone/scion#2152: scion-<sha256(hub_name)[:12]>-*, hashing
#      hub_name alone rather than hub_name:scopeID.
#
#      Whether an IAM condition on a resource-name prefix covers
#      `secrets.create` (not just operations on an existing secret) was
#      verified empirically against the live API: it does, so no
#      pre-create fallback is needed.
#
# project_number (below) always comes from the shared-lookup data source via
# var.project_number, never a literal — a hardcoded project number in this
# expression would silently stop matching if this Terraform were ever
# pointed at a different project.
locals {
  hub_prefixed_secret_prefix = "projects/${var.project_number}/secrets/scion-${substr(sha256(var.hub_name), 0, 12)}-"
}

# condition{} fields (title, description, expression) are ForceNew: editing any of them, even
# description wording, destroys and re-creates this grant on live hubs. Put explanations in comments, not here.
resource "google_project_iam_member" "hub_secretmanager_admin_hub_prefixed" {
  project = var.project_id
  role    = "roles/secretmanager.admin"
  member  = "serviceAccount:${google_service_account.hub.email}"

  condition {
    title       = "${var.hub_name}-hub-prefixed-secrets"
    description = "This hub's hub-prefixed Secret Manager names (ptone/scion#2152), scion-<sha256(hub_name)[:12]>-* — covers hub, user, and project scope alike under one prefix computed from hub_name alone. Never another hub's or the live stack's."
    expression  = "resource.name.startsWith(\"${local.hub_prefixed_secret_prefix}\")"
  }
}

resource "google_project_iam_member" "hub_cloudsql_client" {
  project = var.project_id
  role    = "roles/cloudsql.client"
  member  = "serviceAccount:${google_service_account.hub.email}"
}

resource "google_project_iam_member" "hub_cloudsql_instance_user" {
  project = var.project_id
  role    = "roles/cloudsql.instanceUser"
  member  = "serviceAccount:${google_service_account.hub.email}"
}

resource "google_project_iam_member" "hub_container_cluster_viewer" {
  project = var.project_id
  role    = "roles/container.clusterViewer"
  member  = "serviceAccount:${google_service_account.hub.email}"
}

resource "google_project_iam_member" "hub_logging_log_writer" {
  project = var.project_id
  role    = "roles/logging.logWriter"
  member  = "serviceAccount:${google_service_account.hub.email}"
}

# Hub SA mints tokens as the transport SA (SCION_TRANSPORT_TOKEN injection
# depends on this; if missing, the failure is silent) and as itself
# (signBlob for GCS signed URLs).
resource "google_service_account_iam_member" "hub_mints_transport_tokens" {
  service_account_id = google_service_account.transport.name
  role               = "roles/iam.serviceAccountTokenCreator"
  member             = "serviceAccount:${google_service_account.hub.email}"
}

resource "google_service_account_iam_member" "hub_mints_own_tokens" {
  service_account_id = google_service_account.hub.name
  role               = "roles/iam.serviceAccountTokenCreator"
  member             = "serviceAccount:${google_service_account.hub.email}"
}

# --- Hub SA: Workload Identity (opt-in, hub-gke only) ---
#
# A hub running as a GKE pod reaches Google APIs as its KSA, and Workload
# Identity maps that KSA to the hub GSA only if the GSA grants
# roles/iam.workloadIdentityUser to exactly that KSA's member string. This
# is scoped to the one { namespace, name } pair, on the hub GSA resource
# only. It is never a project-level grant, which would let any KSA in the
# cluster's identity pool act as any SA in the project.
resource "google_service_account_iam_member" "hub_workload_identity_user" {
  count = var.hub_workload_identity_ksa == null ? 0 : 1

  service_account_id = google_service_account.hub.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${var.hub_workload_identity_ksa.namespace}/${var.hub_workload_identity_ksa.name}]"
}

# --- Transport SA ---
#
# NOT granted here: a project-wide roles/iap.httpsResourceAccessor, since
# it reaches every IAP-protected resource in the project, including any other
# live hub. Scoped instead to just this hub's own
# Cloud Run service, in hub-cloudrun's google_iap_web_cloud_run_service_iam_member
# (the same resource type already used there for human iap_members).

# --- Agent SA ---
#
# No Secret Manager role at all. The agent
# SA is the Workload Identity binding for agent pods, which run agent- and
# user-supplied code: a project-wide accessor would let any agent pod read
# scion-<hash>-user_signing_key/-agent_signing_key for the live hub (or any
# other hub sharing this project) and mint valid tokens for it — escalation
# from "runs an agent" to "administers an unrelated production hub", and
# unfixable with an IAM condition (user/project-scope secret names have no
# per-hub prefix — same root cause as the hub SA note above). The
# rendered settings' runtimes.remote.gke is therefore set to false: with
# gke: false, k8s_runtime.go puts resolved secrets into namespaced
# Kubernetes Secrets the hub writes, not a SecretProviderClass the agent
# pod's own WI identity reads — so Secret Manager access is never in the
# agent's reach at all, by construction, not just by omitted IAM. The agent
# SA keeps only its Workload Identity binding (agent-runtime-k8s), plus the
# project-level model-access role(s) below.
#
# The WI binding lets an agent authenticate AS this SA, but authentication
# isn't authorization — with no model-access role, Vertex calls 403.
# var.agent_sa_project_roles defaults to exactly roles/aiplatform.user, the
# minimum needed to call Vertex, validated against owner/editor/*.admin for
# the same reason the hub SA note above rejects a project-wide grant: this
# runs agent- and user-supplied code.
resource "google_project_iam_member" "agent_sa_project_roles" {
  for_each = toset(var.agent_sa_project_roles)

  project = var.project_id
  role    = each.value
  member  = "serviceAccount:${google_service_account.agent.email}"
}

