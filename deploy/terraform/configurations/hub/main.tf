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
# nothing in this root creates or replaces the cluster (design §3.5).
data "google_client_config" "me" {}

provider "kubernetes" {
  host                   = "https://${module.shared_lookup.shared.gke.endpoint}"
  cluster_ca_certificate = base64decode(module.shared_lookup.shared.gke.ca_certificate)
  token                  = data.google_client_config.me.access_token

  # Autopilot's warden webhook stamps autopilot.gke.io/* annotations
  # (warden-version, resource-adjustment, ...) onto objects it admits —
  # agent pods and the namespace will collect these too, not just the
  # nfs-init Job. Handled provider-wide rather than per-resource
  # ignore_changes, since it's a cluster-wide Autopilot behavior (design
  # §3.4/§3.5, added after vm-deploy live-planned this against the real
  # cluster).
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
  # image_registry design §3.4: default computes to the shared AR repo from
  # shared-lookup's real resource data (not a manually reconstructed
  # string), overridable via var.image_registry for a variation.
  image_registry = coalesce(var.image_registry, module.shared_lookup.shared.artifact_registry.repo_url)

  # Single source of truth for this hub's identity, fed to hub-identity (whose
  # hub_name input drives hub_scope_secret_hash, the OIDC signing key's secret
  # ID) and to hub-cloudrun (whose hub_name input drives settings.yaml's
  # hub_id and the SCION_SERVER_HUB_HUBID env var). ResolveHubID() in
  # pkg/config/hub_config.go prefers settings hub_id over the env var, so the
  # env var alone doesn't drive secret naming — but the two must never
  # diverge, or GCPBackend.Get computes a different scion-hub-<hash>-* secret
  # name than the one actually provisioned here (split-brain secret lookup).
  # Routing both module calls through this one local, instead of each
  # referencing var.hub_name independently, is what keeps them in lockstep
  # (tf-lead/vm-deploy, OIDC signing key task).
  hub_id = var.hub_name
}

module "cloudsql_database" {
  source = "../../modules/cloudsql-database"

  project_id    = var.project_id
  instance_name = module.shared_lookup.shared.sql.instance_name
  hub_name      = var.hub_name
  hub_sa_email  = module.hub_identity.hub_sa_email
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

  project_id                   = var.project_id
  project_number               = module.shared_lookup.shared.project_number
  region                       = var.region
  hub_name                     = local.hub_id
  hub_image                    = var.hub_image
  image_registry               = local.image_registry
  hub_sa_email                 = module.hub_identity.hub_sa_email
  transport_sa_email           = module.hub_identity.transport_sa_email
  hub_iam_grants               = module.hub_identity.hub_iam_grants
  hub_iam_condition_expression = module.hub_identity.hub_iam_condition_expression
  hub_scope_secret_hash        = module.hub_identity.hub_scope_secret_hash

  # F-106 (design §9): real resource attributes only, never a module
  # reference or a computed-string output — see hub-cloudrun's
  # boot_prerequisites variable and terraform_data.boot_prerequisites for why
  # a module-level depends_on used to sit where this argument now is, and why
  # it broke the settings secret on every unrelated change to these three
  # modules (vm-deploy, real apply: an IAM-only change came out as an
  # unrelated destroy/replace of the settings secret version). db_name/
  # db_user and hub_iam_grants above already create real dependency edges on
  # their own (they're resource attributes, not just variables) — bundled
  # here too so all of B2's ordering requirements are visible in one place,
  # not split between incidental variable wiring and this map. The one
  # requirement that had NO other edge at all was the nfs-init Job: nfs_export
  # (above) is a plain path string, known before the Job ever runs, so
  # nfs_init_job_id (a real attribute of the Job resource, only known once
  # wait_for_completion's wait is over) is what actually closes that gap.
  boot_prerequisites = {
    nfs_init_job   = module.agent_runtime_k8s.nfs_init_job_id
    db_name        = module.cloudsql_database.db_name
    db_user        = module.cloudsql_database.db_user
    hub_iam_grants = join(",", module.hub_identity.hub_iam_grants)
  }

  network_name = module.shared_lookup.shared.network.name
  subnet_name  = module.shared_lookup.shared.network.subnet_name

  sql_connection_name = module.shared_lookup.shared.sql.connection_name
  db_name             = module.cloudsql_database.db_name
  db_user             = module.cloudsql_database.db_user
  db_password         = module.cloudsql_database.db_password

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

  # F-110 (design §9): hub-cloudrun validates both against var.timeout itself.
  hub_write_timeout    = var.hub_write_timeout
  broker_write_timeout = var.broker_write_timeout

  # Single-sourced with agent-runtime-k8s above (design §3.4) — passing the
  # same three values to both, rather than letting hub-cloudrun default them
  # independently, is what stops the NFS tree being chowned one way while
  # the hub is told another.
  nfs_uid          = var.nfs_uid
  nfs_gid          = var.nfs_gid
  nfs_subpath_root = var.nfs_subpath_root

  # No module-level depends_on here (F-106, design §9): it used to cover the
  # same B2 ordering requirement (nfs-init Job, database/user, hub-identity's
  # IAM propagating, all before the first revision boots) that
  # boot_prerequisites above now covers — but a module-level depends_on defers
  # every resource AND data source inside hub-cloudrun, including
  # hub-cloudrun's old data.google_secret_manager_secret_version.db_password
  # (removed in F-112 — db_password above is now a plain sensitive module
  # input, not a data source read), which had no actual ordering need on
  # these three modules. That made the settings secret's secret_data unknown
  # at plan time, forcing a spurious replace of the settings secret version
  # on every unrelated change to agent-runtime-k8s, cloudsql-database or
  # hub-identity (vm-deploy, real apply: an IAM-only change came out 2 add /
  # 0 change / 1 destroy instead of 2/0/0). Do not reintroduce a depends_on
  # on this module block for this purpose — express any new B2-class
  # ordering need as another real resource attribute in boot_prerequisites
  # instead.
}
