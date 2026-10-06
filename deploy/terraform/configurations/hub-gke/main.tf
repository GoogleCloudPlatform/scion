provider "google" {
  project = var.project_id
  region  = var.region
}

# The shared GKE cluster pre-exists (configurations/shared-infra); nothing in
# this root creates or replaces it, so configuring providers from it is safe.
data "google_client_config" "me" {}

provider "kubernetes" {
  host                   = "https://${module.shared_lookup.shared.gke.endpoint}"
  cluster_ca_certificate = base64decode(module.shared_lookup.shared.gke.ca_certificate)
  token                  = data.google_client_config.me.access_token

  # Autopilot's warden stamps autopilot.gke.io/* annotations (see
  # configurations/hub). The GKE NEG controller writes
  # cloud.google.com/neg-status back onto hub-gke's NEG Service; without
  # ignoring it every plan after the first would try to remove it.
  ignore_annotations = [
    "^autopilot\\.gke\\.io/.*",
    "^cloud\\.google\\.com/neg-status$",
  ]
}

provider "helm" {
  kubernetes = {
    host                   = "https://${module.shared_lookup.shared.gke.endpoint}"
    cluster_ca_certificate = base64decode(module.shared_lookup.shared.gke.ca_certificate)
    token                  = data.google_client_config.me.access_token
  }
}

module "shared_lookup" {
  source = "../../modules/shared-lookup"

  project_id    = var.project_id
  region        = var.region
  zone          = var.zone
  shared_prefix = var.shared_prefix
  share_name    = var.shared_share_name
}

# The NEG controller creates one standalone NEG per cluster zone. Read the
# zone list from the cluster itself rather than restating it.
data "google_container_cluster" "agents" {
  project  = var.project_id
  location = module.shared_lookup.shared.gke.location
  name     = module.shared_lookup.shared.gke.name
}

locals {
  # One source for the hub's identity: hub-identity's secret-prefix naming
  # and the chart's hub.hubId must never diverge (see configurations/hub).
  hub_id = var.hub_name

  image_registry = coalesce(var.image_registry, module.shared_lookup.shared.artifact_registry.repo_url)

  # The hub pod's KSA. hub-identity's Workload Identity grant and the
  # chart's serviceAccount.name both come from this one value.
  hub_ksa = {
    namespace = "${var.hub_name}-system"
    name      = "scion-hub"
  }

  neg_zones = sort(tolist(data.google_container_cluster.agents.node_locations))
}

module "hub_identity" {
  source = "../../modules/hub-identity"

  project_id     = var.project_id
  project_number = module.shared_lookup.shared.project_number
  hub_name       = local.hub_id

  hub_workload_identity_ksa = local.hub_ksa
}

module "cloudsql_database" {
  source = "../../modules/cloudsql-database"

  project_id          = var.project_id
  instance_name       = module.shared_lookup.shared.sql.instance_name
  hub_name            = var.hub_name
  hub_sa_email        = module.hub_identity.hub_sa_email
  sql_connection_name = module.shared_lookup.shared.sql.connection_name
  password_rotation   = var.db_password_rotation
}

module "agent_runtime_k8s" {
  source = "../../modules/agent-runtime-k8s"

  hub_name         = var.hub_name
  project_id       = var.project_id
  hub_sa_email     = module.hub_identity.hub_sa_email
  hub_sa_unique_id = module.hub_identity.hub_sa_unique_id
  agent_sa_email   = module.hub_identity.agent_sa_email

  # The chart grants its own KSA in this namespace (rbac.create with
  # runtime.namespace); the hub pod never presents the GSA identities this
  # module's Role would bind.
  create_hub_rbac = false

  nfs = {
    server     = module.shared_lookup.shared.nfs.server
    share_path = module.shared_lookup.shared.nfs.share_path
  }

  nfs_uid      = var.nfs_uid
  nfs_gid      = var.nfs_gid
  subpath_root = var.nfs_subpath_root
  capacity     = var.nfs_capacity
}

module "hub_lb" {
  source = "../../modules/hub-lb"

  project_id     = var.project_id
  project_number = module.shared_lookup.shared.project_number
  name           = var.hub_name
  hostname       = var.hostname
  network        = module.shared_lookup.shared.network.name
  port           = 8080

  # Unknown until hub-gke's NEG Service exists, which defers the NEG reads
  # to apply on a fresh hub (see the README's NEG race note).
  neg_name  = module.hub_gke.neg_name
  neg_zones = local.neg_zones

  enable_http_redirect = var.enable_http_redirect
  iap_members          = var.iap_members
  transport_sa_email   = module.hub_identity.transport_sa_email
}

module "hub_gke" {
  source = "../../modules/hub-gke"

  project_id = var.project_id
  region     = var.region
  hub_name   = local.hub_id

  # Front-door contract: these two and nothing else from hub_lb.
  public_url   = module.hub_lb.public_url
  iap_audience = module.hub_lb.iap_audience

  image_repository = "${module.shared_lookup.shared.artifact_registry.repo_url}/scion-hub-gke"
  image_digest     = var.hub_image_digest

  hub_sa_email        = module.hub_identity.hub_sa_email
  transport_sa_email  = module.hub_identity.transport_sa_email
  ksa_name            = local.hub_ksa.name
  iap_oauth_client_id = var.iap_oauth_client_id
  admin_emails        = var.admin_emails

  image_registry    = local.image_registry
  runtime_namespace = module.agent_runtime_k8s.namespace
  port              = 8080

  db_name              = module.cloudsql_database.db_name
  db_user              = module.cloudsql_database.db_user
  db_password          = module.cloudsql_database.db_password
  db_password_rotation = var.db_password_rotation
  sql_connection_name  = module.shared_lookup.shared.sql.connection_name

  # Real resource attributes only (see hub-gke's variable). The hub pod
  # reaches Secret Manager, Cloud SQL and GCS as the hub GSA through
  # Workload Identity, so the release waits for those grants to exist.
  boot_prerequisites = {
    hub_workload_identity = module.hub_identity.hub_workload_identity_member
    hub_iam_grants        = join(",", module.hub_identity.hub_iam_grants)
  }
}

# Same pattern as hub-cloudrun's check of the same name. Here an unset client
# ID means more than "transport disabled": the hub itself is not installed,
# because the chart refuses transport mode iap without it. Warns, never fails.
check "transport_audience_configured" {
  assert {
    condition     = var.iap_oauth_client_id != null
    error_message = "the hub is NOT installed (helm_release skipped, output hub_installed = false) until iap_oauth_client_id is set — see this configuration's README, \"First install\", to discover the Google-managed client ID and re-apply."
  }
}
