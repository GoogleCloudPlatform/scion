provider "google" {
  project = var.project_id
  region  = var.region
}

provider "google-beta" {
  project = var.project_id
  region  = var.region
}

module "project_services" {
  source = "../../modules/project-services"

  project_id = var.project_id
}

module "network" {
  source = "../../modules/network"

  project_id        = var.project_id
  region            = var.region
  name_prefix       = var.name_prefix
  subnet_cidr       = var.subnet_cidr
  psa_prefix_length = var.psa_prefix_length

  depends_on = [module.project_services]
}

module "cloudsql_instance" {
  source = "../../modules/cloudsql-instance"

  project_id          = var.project_id
  region              = var.region
  name_prefix         = var.name_prefix
  network_id          = module.network.network_id
  psa_connection      = module.network.psa_connection
  tier                = var.sql_tier
  availability_type   = var.sql_availability_type
  max_connections     = var.sql_max_connections
  deletion_protection = var.deletion_protection
}

module "filestore" {
  source = "../../modules/filestore"

  project_id          = var.project_id
  zone                = var.zone
  name_prefix         = var.name_prefix
  network_name        = module.network.network_name
  tier                = var.filestore_tier
  capacity_gb         = var.filestore_capacity_gb
  share_name          = var.filestore_share_name
  deletion_protection = var.deletion_protection

  depends_on = [module.network]
}

module "gke_autopilot" {
  source = "../../modules/gke-autopilot"

  project_id                 = var.project_id
  region                     = var.region
  name_prefix                = var.name_prefix
  network_id                 = module.network.network_id
  subnet_id                  = module.network.subnet_id
  release_channel            = var.gke_release_channel
  master_authorized_networks = var.gke_master_authorized_networks
  deletion_protection        = var.deletion_protection
}

module "artifact_registry" {
  source = "../../modules/artifact-registry"

  project_id  = var.project_id
  region      = var.region
  name_prefix = var.name_prefix

  depends_on = [module.project_services]
}

# --- Destroy guardrail ---
#
# GCP does not refuse to delete a Cloud SQL instance that still has hub
# databases on it (nor a Filestore instance with active clients, nor a GKE
# cluster with namespaces) — so the guardrail has to live here, not rely on
# the API to say no. To tear this stack down you must first apply with
# deletion_protection=false, and that apply fails while any hub database
# still exists on the shared instance. The protection-off state is therefore
# unreachable while hubs are present, which makes the subsequent `destroy`
# safe. A hub's database is the proxy for the whole hub: every hub root
# creates exactly one (cloudsql-database), and never manages the instance
# itself (reviewer check: google_sql_database_instance appears only in
# modules/cloudsql-instance).
#
# Required shape (reproduced offline on TF 1.9.8, and confirmed live against
# a real project — including a false alarm on this exact block from a stale
# checkout, retracted once re-run against the actual commit: 23-resource
# plan, exit 0, no 403): evaluated at PLAN, with NO module dependency,
# tolerant of the instance not existing yet. `instance =
# module.cloudsql_instance.instance_name` looks more correct (a resource
# reference instead of a literal), but it is exactly the bug: it defers this
# read to APPLY time, ordered *after* the instance update. On `apply -var
# deletion_protection=false` with hubs still present,
# `settings.deletion_protection_enabled` would flip off *before*
# the precondition below fails — precisely the state transition this guard
# exists to prevent. The literal name plus an existence check first is not a
# style choice; do not "fix" it back to a module reference.
#
# An intent-based alternative (`count = var.deletion_protection ? 0 : 1`,
# skipping the existence probe) was considered and rejected: this
# existence-based shape keys the guard on whether the instance is actually
# there, so it stays live and accurate during normal operation rather than
# only when someone is already trying to turn protection off.
data "google_sql_database_instances" "all" {
  project = var.project_id
}

locals {
  shared_pg_name   = "${var.name_prefix}-pg"
  shared_pg_exists = contains([for i in data.google_sql_database_instances.all.instances : i.name], local.shared_pg_name)
}

data "google_sql_databases" "on_instance" {
  count    = local.shared_pg_exists ? 1 : 0
  project  = var.project_id
  instance = local.shared_pg_name
}

locals {
  hub_dbs = local.shared_pg_exists ? [
    for d in data.google_sql_databases.on_instance[0].databases : d.name if d.name != "postgres"
  ] : []
}

# No depends_on, deliberately: it would create exactly the apply-time
# ordering this shape avoids.
resource "terraform_data" "destroy_guard" {
  input = var.deletion_protection

  lifecycle {
    precondition {
      condition     = var.deletion_protection || length(local.hub_dbs) == 0
      error_message = "destroy_guard: hubs still exist on this shared infra (databases: ${join(", ", local.hub_dbs)}). Destroy every hub root first."
    }
  }
}
