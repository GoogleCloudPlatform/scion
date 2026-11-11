provider "google" {
  project = var.project_id
  region  = var.region
}

provider "google-beta" {
  project = var.project_id
  region  = var.region
}

# The shared GKE cluster pre-exists (created by configurations/shared-infra),
# so this is the well-behaved case for the provider-from-cluster problem:
# nothing in this root creates or replaces the cluster.
data "google_client_config" "me" {}

provider "kubernetes" {
  host                   = "https://${module.shared_lookup.shared.gke.endpoint}"
  cluster_ca_certificate = base64decode(module.shared_lookup.shared.gke.ca_certificate)
  token                  = data.google_client_config.me.access_token

  # Autopilot's warden webhook stamps autopilot.gke.io/* annotations
  # (warden-version, resource-adjustment, ...) onto objects it admits —
  # agent pods and the namespace will collect these too, not just the
  # nfs-init Job. Handled provider-wide rather than per-resource
  # ignore_changes, since it's a cluster-wide Autopilot behavior confirmed
  # by live-planning against a real cluster.
  ignore_annotations = ["^autopilot\\.gke\\.io/.*"]
}

module "shared_lookup" {
  source = "../../modules/shared-lookup"

  project_id    = var.project_id
  region        = var.region
  zone          = var.zone
  shared_prefix = var.shared_prefix
  share_name    = var.shared_share_name
}

locals {
  # Default computes to the shared AR repo from shared-lookup's real resource
  # data (not a manually reconstructed string), overridable via
  # var.image_registry for a variation.
  image_registry = coalesce(var.image_registry, module.shared_lookup.shared.artifact_registry.repo_url)

  # Single source of truth for this hub's identity, fed to hub-identity (whose
  # hub_name input drives the hub-prefixed Secret Manager naming, ptone/
  # scion#2152) and to hub-cloudrun (whose hub_name input drives settings.yaml's
  # hub_id and the SCION_SERVER_HUB_HUBID env var). ResolveHubID() in
  # pkg/config/hub_config.go prefers settings hub_id over the env var, so the
  # env var alone doesn't drive secret naming — but the two must never
  # diverge, or GCPBackend.Get computes a different scion-<hash>-* secret
  # name than the one actually provisioned here (split-brain secret lookup).
  # Routing both module calls through this one local, instead of each
  # referencing var.hub_name independently, is what keeps them in lockstep.
  hub_id = var.hub_name
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

module "hub_identity" {
  source = "../../modules/hub-identity"

  project_id     = var.project_id
  project_number = module.shared_lookup.shared.project_number
  hub_name       = local.hub_id
}

module "agent_runtime_k8s" {
  source = "../../modules/agent-runtime-k8s"

  hub_name         = var.hub_name
  project_id       = var.project_id
  hub_sa_email     = module.hub_identity.hub_sa_email
  hub_sa_unique_id = module.hub_identity.hub_sa_unique_id
  agent_sa_email   = module.hub_identity.agent_sa_email

  nfs = {
    server     = module.shared_lookup.shared.nfs.server
    share_path = module.shared_lookup.shared.nfs.share_path
  }

  nfs_uid      = var.nfs_uid
  nfs_gid      = var.nfs_gid
  subpath_root = var.nfs_subpath_root
  capacity     = var.nfs_capacity
}

module "hub_cloudrun" {
  source = "../../modules/hub-cloudrun"

  project_id                            = var.project_id
  project_number                        = module.shared_lookup.shared.project_number
  region                                = var.region
  hub_name                              = local.hub_id
  hub_image                             = var.hub_image
  image_registry                        = local.image_registry
  hub_sa_email                          = module.hub_identity.hub_sa_email
  transport_sa_email                    = module.hub_identity.transport_sa_email
  hub_iam_grants                        = module.hub_identity.hub_iam_grants
  hub_iam_condition_expression_prefixed = module.hub_identity.hub_iam_condition_expression_prefixed

  # Real resource attributes only, never a module reference or a
  # computed-string output. This map is what forces Cloud Run to wait for the
  # nfs-init Job, the database/user, and hub-identity's IAM grants to
  # propagate before the first revision boots. A module-level depends_on used
  # to express this instead, and that broke the settings secret on every
  # unrelated change to these three modules: a depends_on on the whole module
  # block defers every resource AND data source inside it, so an IAM-only
  # change under agent-runtime-k8s, cloudsql-database or hub-identity made
  # the settings secret's secret_data look unknown at plan time and forced a
  # spurious replace of the settings secret version (observed on a real
  # apply: an IAM-only change came out as 2 add / 0 change / 1 destroy
  # instead of 2/0/0). db_name/db_user (referenced directly in this map) and
  # hub_iam_grants (passed as a module argument above) already create real
  # dependency edges on their own (they're resource attributes, not just
  # variables) — bundled here too so all of the ordering requirements are
  # visible in one place, not split between incidental variable wiring and
  # this map. db_name/db_user are no longer separately passed to hub-cloudrun
  # as module arguments (the DSN is built entirely inside cloudsql-database
  # now — see hub-cloudrun's variables.tf); they still appear here purely for
  # this ordering edge, sourced straight from cloudsql-database's outputs.
  # The one requirement that had NO other edge at all was the nfs-init Job:
  # nfs_export (above) is a plain path string, known before the Job ever
  # runs, so nfs_init_job_id (a real attribute of the Job resource) is what
  # actually closes that gap. It is the Job's .id ("<namespace>/<name>"), not
  # its .metadata[0].uid: the provider sets .id from the create response
  # before wait_for_completion runs, but leaves uid null in state until the
  # next refresh, so keying on uid made this terraform_data show a spurious
  # 0/1/0 on every fresh hub's second plan even though the graph edge (and so
  # the ordering) was never actually broken.
  boot_prerequisites = {
    nfs_init_job   = module.agent_runtime_k8s.nfs_init_job_id
    db_name        = module.cloudsql_database.db_name
    db_user        = module.cloudsql_database.db_user
    hub_iam_grants = join(",", module.hub_identity.hub_iam_grants)
  }

  network_name = module.shared_lookup.shared.network.name
  subnet_name  = module.shared_lookup.shared.network.subnet_name

  # sql_connection_name is still needed by hub-cloudrun for the cloudsql
  # volume's cloud_sql_instance.instances even though it's no longer rendered
  # into settings.yaml. db_name/db_user/db_password are gone: the DSN is
  # built entirely inside cloudsql-database now and reaches hub-cloudrun only
  # as the two secret coordinates below.
  sql_connection_name = module.shared_lookup.shared.sql.connection_name
  dsn_secret_id       = module.cloudsql_database.dsn_secret_id
  dsn_secret_version  = module.cloudsql_database.dsn_secret_version

  nfs_server = module.shared_lookup.shared.nfs.server
  nfs_export = module.agent_runtime_k8s.nfs_export
  pv_name    = module.agent_runtime_k8s.pv_name
  namespace  = module.agent_runtime_k8s.namespace

  gke = {
    endpoint       = module.shared_lookup.shared.gke.endpoint
    ca_certificate = module.shared_lookup.shared.gke.ca_certificate
  }

  iap_oauth_client_id = var.iap_oauth_client_id
  iap_members         = var.iap_members
  admin_emails        = var.admin_emails

  min_instances = var.min_instances
  max_instances = var.max_instances
  cpu           = var.cpu
  memory        = var.memory
  timeout       = var.timeout

  # hub-cloudrun validates both against var.timeout itself.
  hub_write_timeout    = var.hub_write_timeout
  broker_write_timeout = var.broker_write_timeout

  # Single-sourced with agent-runtime-k8s above — passing the same three
  # values to both, rather than letting hub-cloudrun default them
  # independently, is what stops the NFS tree being chowned one way while
  # the hub is told another.
  nfs_uid          = var.nfs_uid
  nfs_gid          = var.nfs_gid
  nfs_subpath_root = var.nfs_subpath_root

  # Deliberately no module-level depends_on here: it used to cover the same
  # boot-ordering requirement (nfs-init Job, database/user, hub-identity's IAM
  # propagating, all before the first revision boots) that boot_prerequisites
  # above now covers — but a module-level depends_on defers every resource
  # AND data source inside hub-cloudrun, which had no actual ordering need on
  # these three modules. That made the settings secret's secret_data unknown
  # at plan time, forcing a spurious replace of the settings secret version
  # on every unrelated change to agent-runtime-k8s, cloudsql-database or
  # hub-identity (observed on a real apply: an IAM-only change came out 2 add
  # / 0 change / 1 destroy instead of 2/0/0). Do not reintroduce a depends_on
  # on this module block for this purpose — express any new ordering
  # requirement as another real resource attribute in boot_prerequisites
  # instead.
}
