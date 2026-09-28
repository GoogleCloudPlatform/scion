# Hub's bucket, secrets, settings render, Cloud Run v2 service and IAP
# (design §3.4, largest module). No provider/backend blocks: the calling
# configuration configures both `google` and `google-beta`.

locals {
  # Cloud Run's deterministic URL — computed from inputs, not read back from
  # the service resource, so there is no "deploy twice" cycle (design §3.6).
  public_url   = "https://${var.hub_name}-${var.project_number}.${var.region}.run.app"
  iap_audience = "/projects/${var.project_number}/locations/${var.region}/services/${var.hub_name}"

  nfs_mount_path = "${var.nfs_mount_root}/${var.hub_name}"

  # This module's own single point of truth for the hub's identity as it
  # flows into settings.yaml's hub_id and SCION_SERVER_HUB_HUBID below — both
  # must resolve to the exact same value ResolveHubID() (pkg/config/
  # hub_config.go) will see, or GCPBackend.Get computes a different
  # scion-hub-<hash>-* secret name than the one hub-identity actually
  # provisioned (split-brain secret lookup). The caller (configurations/hub)
  # feeds var.hub_name from its own local.hub_id for the same reason, so this
  # is the same value end to end, named at each layer that touches it.
  hub_id = var.hub_name

  # F-107: settings.yaml.tftpl used to render broker_id: ${hub_name}-broker
  # (e.g. "tfha-h1-broker"), and the Postgres store's runtime_brokers.id
  # column requires a UUID -- the co-located broker's registration failed
  # outright ('invalid input: invalid UUID "tfha-h1-broker"'), so no broker
  # ever registered and every agent start 422'd with "no runtime brokers
  # available". uuidv5 (not uuidv4/random) because it must be deterministic:
  # resolveBrokerID's fallback path generates a random UUID and persists it
  # to the ephemeral globalDir when none is configured, which races across
  # max_instances = 3 replicas each picking their own -- a config-supplied,
  # stable value is the only thing that keeps every replica agreeing on one
  # broker identity. Namespaced on hub_id + project_id so two hubs (or the
  # same hub_name reused in a different project) never collide.
  broker_id = uuidv5("dns", "${local.hub_id}.broker.${var.project_id}.scion")

  # Phase 2 hardening (item 3b, design §3.7 connection budget). Single
  # source of truth for the per-instance Postgres pool ceiling rendered into
  # settings.yaml's database.max_open_conns below, so the check block's
  # assumption can never silently drift from the literal actually shipped —
  # exactly the split-source-of-truth class of bug this design keeps
  # re-finding (F-106, F-107). Verified against pkg/config/hub_config.go's
  # applyDatabasePoolDefaults (Postgres default is 5 when unset) — this
  # module has always rendered an explicit 10, overriding that default.
  hub_max_open_conns = 10
}

# --- Bucket (artifacts, signed URLs) ---

resource "google_storage_bucket" "artifacts" {
  project                     = var.project_id
  name                        = "${var.project_id}-${var.hub_name}-artifacts"
  location                    = var.region
  uniform_bucket_level_access = true

  versioning {
    enabled = true
  }

  # Phase 2 hardening: this bucket holds templates, skills and harness
  # configs (pkg/storage's ResourceStorageURI family) referenced by live DB
  # rows, plus per-agent workspace files (WorkspaceStorageURI) that an
  # in-flight agent may still be reading or writing — none of that is safe
  # to expire on a blanket age/prefix rule, so no rule targets CURRENT
  # objects. Versioning is on (above), so overwriting or deleting a live
  # object leaves its prior content as a NONCURRENT (ARCHIVED) version
  # instead of freeing it — those accumulate forever with no rule. This
  # only ever touches ARCHIVED (non-live) versions, so it cannot delete live
  # data: the current version of every object is untouched regardless of
  # age. In-place update (lifecycle_rule is a normal, non-ForceNew field on
  # google_storage_bucket) — no bucket replace.
  lifecycle_rule {
    condition {
      with_state = "ARCHIVED"
      age        = 30
    }
    action {
      type = "Delete"
    }
  }

  # Belt-and-suspenders, also in-place and also CURRENT-object-safe: an
  # interrupted resumable/multipart upload has no live reader by
  # definition, so aborting it after a week reclaims storage with zero risk
  # to anything the hub or an agent might still be using.
  lifecycle_rule {
    condition {
      age = 7
    }
    action {
      type = "AbortIncompleteMultipartUpload"
    }
  }
}

resource "google_storage_bucket_iam_member" "hub_object_admin" {
  bucket = google_storage_bucket.artifacts.name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${var.hub_sa_email}"
}

# --- Session secret ---

resource "random_id" "session_secret" {
  byte_length = 32
}

resource "google_secret_manager_secret" "session_secret" {
  project   = var.project_id
  secret_id = "${var.hub_name}-session-secret"

  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "session_secret" {
  secret      = google_secret_manager_secret.session_secret.id
  secret_data = random_id.session_secret.b64_std
}

resource "google_secret_manager_secret_iam_member" "hub_reads_session_secret" {
  secret_id = google_secret_manager_secret.session_secret.secret_id
  project   = var.project_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${var.hub_sa_email}"
}

# --- Database DSN (Alt-F, design §6 OQ-11): the full DSN is built and held
# in cloudsql-database's <hub>-db-dsn secret (the module that owns the DB
# credential lifecycle) — see var.dsn_secret_id/var.dsn_secret_version and
# the SCION_SERVER_DATABASE_URL secret env var on google_cloud_run_v2_service
# .hub below. This module never sees the plaintext DSN or password at all
# any more, only the two secret coordinates needed to point Cloud Run at
# cloudsql-database's secret version. (History: F-112 had this module take
# db_password as a sensitive module input and embed it directly into the
# settings secret's rendered DSN, to avoid a plan-time data-source read of a
# not-yet-created secret, F-106. Alt-F removes the embedding altogether
# rather than fixing that read, so var.db_password/db_user/db_name are gone
# too — see cloudsql-database's dsn_secret_id/dsn_secret_version outputs
# instead.) ---

# --- Rendered settings.yaml ---

resource "google_secret_manager_secret" "settings" {
  project   = var.project_id
  secret_id = "${var.hub_name}-settings"

  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "settings" {
  secret = google_secret_manager_secret.settings.id
  secret_data = templatefile("${path.module}/templates/settings.yaml.tftpl", {
    project_id           = var.project_id
    region               = var.region
    hub_name             = local.hub_id
    public_url           = local.public_url
    iap_audience         = local.iap_audience
    admin_emails         = var.admin_emails
    bucket               = google_storage_bucket.artifacts.name
    iap_oauth_client_id  = var.iap_oauth_client_id
    transport_sa_email   = var.transport_sa_email
    nfs_mount_root       = var.nfs_mount_root
    nfs_server           = var.nfs_server
    nfs_export           = var.nfs_export
    pv_name              = var.pv_name
    namespace            = var.namespace
    nfs_uid              = var.nfs_uid
    nfs_gid              = var.nfs_gid
    nfs_subpath_root     = var.nfs_subpath_root
    image_registry       = var.image_registry
    broker_id            = local.broker_id
    broker_name          = "${local.hub_id}-broker"
    hub_write_timeout    = var.hub_write_timeout
    broker_write_timeout = var.broker_write_timeout
    max_open_conns       = local.hub_max_open_conns
  })

  # F-107: secret_data changing always forces a replace (Secret Manager
  # versions are add-only in the real API; there is no in-place update of an
  # existing version's data). Without create_before_destroy, Terraform's
  # default destroy-then-create order would delete this version before the
  # replacement exists, and the running revision (still mounting the old
  # version by number, see the volume item below) would lose it out from
  # under it. create_before_destroy makes the new version exist first; the
  # Cloud Run service below is updated to point at it (a new revision), and
  # only then is the old version destroyed.
  lifecycle {
    create_before_destroy = true

    # F-107: a lifecycle precondition, not a top-level check block — check
    # blocks only warn, they don't fail plan/apply, so they don't actually
    # stop the "invalid UUID" regression this exists to catch. A
    # precondition on the resource that renders broker_id into settings.yaml
    # does fail the plan.
    precondition {
      condition     = can(regex("^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$", local.broker_id))
      error_message = "local.broker_id must be a canonical UUID (F-107): the Postgres store's runtime_brokers.id column rejects anything else, and settings.yaml's broker_id is rendered directly from this value."
    }
  }
}

resource "google_secret_manager_secret_iam_member" "hub_reads_settings" {
  secret_id = google_secret_manager_secret.settings.secret_id
  project   = var.project_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${var.hub_sa_email}"
}

# --- DSN accessor (Alt-F, design §6 OQ-11) ---
#
# The DSN secret itself lives in cloudsql-database (the module that owns the
# DB credential lifecycle); the grant lives here, next to the other three
# per-secret accessors this container reads directly at boot, which keeps
# the time_sleep.iam_propagation wiring below in one place (all of it in
# this module) rather than threading another grant .id up through the hub
# root and back down. var.dsn_secret_id is a real cloudsql-database output
# attribute (not a bare string), so this also creates the ordering edge that
# ensures the secret exists before this grant references it.
resource "google_secret_manager_secret_iam_member" "hub_reads_dsn" {
  secret_id = var.dsn_secret_id
  project   = var.project_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${var.hub_sa_email}"
}

# --- Kubeconfig (endpoint + CA only; the hub authenticates with its ambient
# Google token, not a static credential — setup-gcp.md L237-260's form,
# verbatim, including the gke-gcloud-auth-plugin exec stanza: the plugin is
# absent in the image, so the Go k8s client falls back to GCE metadata auth) ---

resource "google_secret_manager_secret" "kubeconfig" {
  project   = var.project_id
  secret_id = "${var.hub_name}-kubeconfig"

  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "kubeconfig" {
  secret = google_secret_manager_secret.kubeconfig.id
  secret_data = templatefile("${path.module}/templates/kubeconfig.yaml.tftpl", {
    cluster_name   = var.hub_name
    endpoint       = var.gke.endpoint
    ca_certificate = var.gke.ca_certificate
  })

  # F-107: same reasoning as google_secret_manager_secret_version.settings
  # above — equally trivial to apply here (same templatefile-secret-version
  # shape), so applied for the same reason, not just for settings.
  lifecycle {
    create_before_destroy = true
  }
}

resource "google_secret_manager_secret_iam_member" "hub_reads_kubeconfig" {
  secret_id = google_secret_manager_secret.kubeconfig.secret_id
  project   = var.project_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${var.hub_sa_email}"
}

# --- OIDC signing key (design §3.4, added 09-25 after tfha-h1's first boot) ---
#
# SCION_REQUIRE_STABLE_SIGNING_KEY=true (below) makes pkg/hub/oidckeys.go:467
# refuse to generate the RSA OIDC signing key at boot — the agent and user
# signing keys are derived from the session secret, but the OIDC key has no
# derivation path, so with no key pre-provisioned no new hub could start.
# Terraform pre-provisions it instead of the hub generating it.
#
# The secret ID is built directly from hub-identity's hub_scope_secret_hash
# (not recomputed here), so it lands under the hub SA's existing conditioned
# secretmanager.admin grant (scion-hub-<hash>-*) with no new IAM: the hub
# finds it through GCPBackend.Get's no-DB-record path (computes the name,
# reads accessLatestVersion), then backs it up to the store. Set() on an
# existing secret only adds versions and never rewrites labels, so later
# hub rotations (RotateKey) and boot re-syncs cause no Terraform drift.
resource "tls_private_key" "oidc_signing_key" {
  algorithm = "RSA"
  rsa_bits  = 2048
}

resource "google_secret_manager_secret" "oidc_signing_key" {
  project   = var.project_id
  secret_id = "scion-hub-${var.hub_scope_secret_hash}-oidc_signing_key"

  # Mirrors pkg/secret/gcpbackend.go's buildLabels for this secret's real
  # identity, so it shows up in the console exactly as if the hub had
  # created it itself.
  labels = {
    "scion-scope"    = "hub"
    "scion-scope-id" = local.hub_id
    "scion-type"     = "internal"
    "scion-name"     = "oidc_signing_key"
    "scion-target"   = "oidc_signing_key"
    "scion-hub-name" = local.hub_id
  }

  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "oidc_signing_key" {
  secret = google_secret_manager_secret.oidc_signing_key.id
  # Must be PKCS#8 ("-----BEGIN PRIVATE KEY-----"): decodePEMPrivateKey
  # rejects the PKCS#1 form (private_key_pem, "-----BEGIN RSA PRIVATE KEY-----").
  secret_data = tls_private_key.oidc_signing_key.private_key_pem_pkcs8
}

# --- IAM propagation guard (design §3.5, from OQ-8's ~70s measurement) ---
#
# IAM changes are eventually consistent, and the hub creates its
# scion-hub-<h12>-* signing keys AT BOOT. A service created immediately
# after its IAM would crash-loop with 403s on the very first apply. This is
# purely a timer: it depends on every hub-SA IAM grant (hub-identity), waits,
# and the Cloud Run service below depends on it. triggers include the
# condition expression, so the sleep re-arms if that condition is ever
# changed (e.g. a different hub) — a create-only sleep would protect only
# the first apply and silently stop protecting a later condition change.
# See the README troubleshooting note: a 403 on scion-hub-<h12>-... shortly
# after a first apply is IAM propagation. Re-apply. Do NOT widen the
# condition to work around it.
# F-106 (design §9): the caller used to express this module's ordering
# requirements (agent-runtime-k8s's nfs-init Job, cloudsql-database's DB/
# user, hub-identity's IAM grants) as a module-level depends_on on the
# `module "hub_cloudrun"` block itself. A module-level depends_on defers
# EVERY resource and data source inside the module — including this
# module's old data.google_secret_manager_secret_version.db_password, which
# had no actual ordering need on those modules — whenever anything in them
# has a pending change. That made db_password's secret_data unknown at plan
# time, which made settings' secret_data (which embeds it) unknown too,
# forcing a spurious replace of the settings secret version on every
# unrelated change to those three modules (vm-deploy caught this on a real
# apply: a plan that should have been a pure IAM-member add came out 2 add /
# 0 change / 1 destroy). terraform_data.boot_prerequisites below is the
# replacement: var.boot_prerequisites carries only real resource attributes
# (never a module reference), so only the one resource that actually needs
# to wait — the Cloud Run service, via its own depends_on below — is
# affected. No data source may depend on terraform_data.boot_prerequisites,
# or this regresses right back to the same bug for whatever data source
# does. (That specific data source is gone now — F-112 replaced it with
# var.db_password, a sensitive module input with no read of its own; Alt-F
# later removed var.db_password itself too, since the DSN it fed is no
# longer embedded in settings.yaml at all — see var.dsn_secret_id/
# var.dsn_secret_version instead. Either way, the rule stands for any data
# source this module gains in the future.)
resource "terraform_data" "boot_prerequisites" {
  input = var.boot_prerequisites
}

resource "time_sleep" "iam_propagation" {
  create_duration = "120s"

  triggers = {
    condition = var.hub_iam_condition_expression
    hub_sa    = var.hub_sa_email
    # Alt-F (design §6 OQ-11): on an EXISTING hub (h1/h2), condition/hub_sa
    # above are unchanged by this rollout, so without this trigger the sleep
    # would not re-arm and the new hub_reads_dsn grant would not be waited
    # for before the revision carrying SCION_SERVER_DATABASE_URL rolls out —
    # a first-boot 403 on the DSN secret, same failure mode §3.5 exists to
    # prevent. .id going from nonexistent to a real value on the apply that
    # introduces this grant is itself a triggers change, which is exactly
    # what forces this time_sleep to replace (destroy + re-create, sleeping
    # again) instead of being silently skipped.
    dsn_accessor = google_secret_manager_secret_iam_member.hub_reads_dsn.id
  }

  # §3.5 says ALL hub-SA IAM members, not just hub-identity's project-level
  # ones: Cloud Run checks secret access at revision *create* time, so this
  # module's own per-secret accessor grants (settings/kubeconfig/session
  # secret/DSN — all four read directly by the running container) matter
  # just as much as the project-level grants passed in from hub-identity.
  # cloudsql-database's db-password accessor is deliberately NOT included:
  # nothing reads db-password directly at runtime (Alt-F's DSN secret is a
  # separate, dedicated secret — see cloudsql-database/main.tf's
  # hub_reads_db_password comment) — nothing to wait on there.
  depends_on = [
    var.hub_iam_grants,
    google_secret_manager_secret_iam_member.hub_reads_settings,
    google_secret_manager_secret_iam_member.hub_reads_kubeconfig,
    google_secret_manager_secret_iam_member.hub_reads_session_secret,
    google_secret_manager_secret_iam_member.hub_reads_dsn,
  ]
}

# --- Cloud Run v2 service ---
# provider = google-beta: iap_enabled requires it (see versions.tf).

resource "google_cloud_run_v2_service" "hub" {
  provider = google-beta

  project              = var.project_id
  name                 = var.hub_name
  location             = var.region
  ingress              = "INGRESS_TRAFFIC_ALL"
  iap_enabled          = true
  invoker_iam_disabled = false # never disable (deploy.sh L145)
  launch_stage         = "GA"
  deletion_protection  = false # this is a hub, not shared infra; hub destroy must be able to remove it

  # F-109: SERVICE-level scaling (top-level, sibling of template{}), not
  # REVISION-level. min_instance_count here is divided among only the
  # revision(s) actually serving traffic — a retired revision at 0% traffic
  # gets none of it. Before this, min_instance_count lived solely in
  # template.scaling below (revision-level), which Cloud Run keeps warm
  # regardless of traffic share: tfha-h1-00001-g5q (broken v1 settings, 0%
  # traffic after the F-107 rollover) was still running its scheduler
  # against the shared Postgres, because its own revision-level min kept it
  # alive. Verified against the pinned google-beta ~> 8.4 schema: top-level
  # scaling and min_instance_count both exist there (provider schema, not
  # assumed).
  scaling {
    min_instance_count = var.min_instances
  }

  template {
    service_account       = var.hub_sa_email
    execution_environment = "EXECUTION_ENVIRONMENT_GEN2" # required for NFS volumes; provider 8.4 rejects the short "GEN2" form
    session_affinity      = true
    timeout               = var.timeout

    # F-109: max stays here (per-revision ceiling; unrelated to the
    # retired-revision problem above). min is pinned to the literal 0, not
    # left unset: template.scaling.min_instance_count is optional but NOT
    # computed in the provider schema (unlike max_instance_count, which is
    # both), so Cloud Run's own "defaults to 0" only applies API-side —
    # leaving it unset risks the same class of bug F-104 already taught us
    # (the API echoing a value Terraform never declared, reintroducing a
    # perpetual diff). Declaring 0 explicitly here makes config and state
    # agree on every plan. The service-level scaling block above is what
    # actually governs how many instances are kept warm.
    scaling {
      min_instance_count = 0
      max_instance_count = var.max_instances
    }

    vpc_access {
      egress = "PRIVATE_RANGES_ONLY"
      network_interfaces {
        network    = var.network_name
        subnetwork = var.subnet_name
      }
    }

    containers {
      image = var.hub_image

      resources {
        cpu_idle          = false
        startup_cpu_boost = true
        limits = {
          cpu    = var.cpu
          memory = var.memory
        }
      }

      env {
        name  = "HOME"
        value = "/home/scion"
      }
      env {
        name  = "SCION_REQUIRE_STABLE_SIGNING_KEY"
        value = "true"
      }
      env {
        name  = "KUBECONFIG"
        value = "/etc/scion/kubeconfig.yaml"
      }
      env {
        name  = "SCION_K8S_NAMESPACE"
        value = var.namespace
      }
      env {
        # Pulled forward from phase 2 (design §3.4, 09-25): must equal
        # settings.yaml's hub_id exactly, from the same local.hub_id value —
        # two sources of truth for the same identity is exactly the class of
        # bug this whole project keeps finding. ResolveHubID() prefers
        # settings hub_id over this env var, so this alone doesn't drive
        # secret naming, but it must never diverge from it. Also forces the
        # new revision this change needs.
        name  = "SCION_SERVER_HUB_HUBID"
        value = local.hub_id
      }
      env {
        name = "SCION_SERVER_SESSION_SECRET"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.session_secret.secret_id
            version = "latest"
          }
        }
      }
      env {
        # Alt-F (design §6 OQ-11): the full DSN, sourced from cloudsql-
        # database's dedicated <hub>-db-dsn secret — settings.yaml's
        # server.database no longer has a url key at all (see
        # settings.yaml.tftpl). Verified against the real hub loader
        # (pkg/config/hub_config.go's loadGlobalConfigFromSettings ->
        # applyEnvOverrides, the path cmd/server_foreground.go's hosted
        # startup actually uses): this env var fills cfg.Database.URL when
        # the file has no server.database.url. Pinned to the version NUMBER
        # cloudsql-database created this apply (not "latest", same reasoning
        # as the settings/kubeconfig secret volume version pins below) — a
        # revision's connection string must never silently change without a
        # new revision.
        name = "SCION_SERVER_DATABASE_URL"
        value_source {
          secret_key_ref {
            secret  = var.dsn_secret_id
            version = var.dsn_secret_version
          }
        }
      }

      # mount_path is the parent directory; the secret's item path below
      # supplies the filename, so the resulting file lands at exactly
      # /run/secrets/settings.yaml — the literal file entrypoint.sh checks
      # for (`if [ -f /run/secrets/settings.yaml ]`), not a directory of
      # that name. Mounting mount_path=/run/secrets/settings.yaml directly
      # would create a directory there instead, breaking that check.
      volume_mounts {
        name       = "settings"
        mount_path = "/run/secrets"
      }
      volume_mounts {
        name       = "kubeconfig"
        mount_path = "/etc/scion"
      }
      volume_mounts {
        name       = "nfs"
        mount_path = local.nfs_mount_path
      }
      # cloudsql is declared LAST in both this list and volumes{} below, on
      # purpose (F-104, vm-deploy): the Cloud Run v2 API always returns the
      # cloud_sql_instance volume/mount last regardless of request order, and
      # the provider diffs volumes/volume_mounts positionally, not by name —
      # declaring it anywhere else here is a permanent one-item drift on
      # every plan. Not an ignore_changes candidate: this makes the
      # configuration match reality instead of hiding the mismatch.
      volume_mounts {
        name       = "cloudsql"
        mount_path = "/cloudsql"
      }

      startup_probe {
        http_get {
          path = "/readyz"
          port = 8080
        }
        # Sized for Autopilot cold scale-up (5-10 min for the first agent
        # pod is a separate concern; this is the hub's own boot, which is
        # much faster, but we still allow generous headroom for a cold
        # Cloud SQL/Filestore mount on first revision).
        initial_delay_seconds = 5
        period_seconds        = 5
        timeout_seconds       = 3
        failure_threshold     = 30
      }

      # Phase 2 hardening: liveness probe. Deliberately /healthz, NOT
      # /readyz. route_metadata.go labels them explicitly
      # ("/healthz": RouteID "health.liveness"; "/readyz": RouteID
      # "health.readiness"), and handleHealthz (handlers_health.go) always
      # answers 200 regardless of the database check's result — it only
      # reports "degraded" in the JSON body — while handleReadyz 503s
      # whenever store.Ping fails. A liveness probe on /readyz would let a
      # transient Cloud SQL blip (a Postgres restart during the REGIONAL
      # failover this same phase enables, a maintenance window, a brief
      # connection-budget squeeze) fail liveness on every instance at once
      # and have Cloud Run restart them all simultaneously — the exact
      # thundering-herd this probe must not cause. The startup probe above
      # stays on /readyz: the DB must be reachable before Cloud Run ever
      # routes traffic to a fresh revision, but once serving, a DB blip
      # should surface as readiness/error-rate, not a container restart.
      #
      # F-104 does not apply to probes the way it applies to volumes/
      # volume_mounts: liveness_probe and startup_probe are each a single
      # nested block in the provider schema (MaxItems: 1), not a list the
      # API can reorder, so there is nothing for the provider to diff
      # positionally. Confirmed against the pinned google-beta ~> 8.4
      # schema (same source read for F-109's scaling-block check) — no
      # list semantics apply here, and adding this second block does not
      # reproduce F-104's drift.
      liveness_probe {
        http_get {
          path = "/healthz"
          port = 8080
        }
        initial_delay_seconds = 10
        period_seconds        = 10
        timeout_seconds       = 3
        failure_threshold     = 3
      }
    }

    volumes {
      name = "settings"
      secret {
        secret = google_secret_manager_secret.settings.secret_id
        items {
          path = "settings.yaml"
          # F-107: pinned to the specific version this apply created, not
          # "latest" — "latest" lets a revision silently start reading a
          # newer settings render with no new revision and no record of
          # which settings.yaml it's actually running. Pinning here is what
          # makes create_before_destroy above meaningful: the service
          # updates (new revision) to point at the new version's number
          # before the old version is destroyed, instead of both racing to
          # resolve "latest" during the swap.
          version = google_secret_manager_secret_version.settings.version
        }
      }
    }
    volumes {
      name = "kubeconfig"
      secret {
        secret = google_secret_manager_secret.kubeconfig.secret_id
        items {
          path    = "kubeconfig.yaml"
          version = google_secret_manager_secret_version.kubeconfig.version
        }
      }
    }
    volumes {
      name = "nfs"
      nfs {
        server = var.nfs_server
        path   = var.nfs_export
      }
    }
    # Declared last — see the matching volume_mounts comment above (F-104).
    volumes {
      name = "cloudsql"
      cloud_sql_instance {
        instances = [var.sql_connection_name]
      }
    }
  }

  lifecycle {
    ignore_changes = [client, client_version]
  }

  # Explicit ordering (tf-review B2): nothing in the attributes above
  # actually links the service to the secret *versions* or the hub SA's
  # *IAM* propagating — only to the secret resources' IDs, which exist as
  # soon as the (empty) secret is created, version or no version, IAM or no
  # IAM. A revision that boots before its settings/kubeconfig version exists
  # or before the hub SA can read them fails with no retry. B2's other half —
  # the nfs-init Job finishing and cloudsql-database/hub-identity being
  # ready — is terraform_data.boot_prerequisites above, not a module-level
  # depends_on on this module's caller (F-106; see that resource's comment).
  # The DSN secret VERSION doesn't need a matching depends_on entry here the
  # way settings/kubeconfig's versions do: it lives in a different module
  # (cloudsql-database), and its version number reaches this resource only
  # via var.dsn_secret_version, a real cross-module output attribute used
  # directly in the SCION_SERVER_DATABASE_URL env block above — that
  # attribute reference alone already creates the graph edge (same reasoning
  # as boot_prerequisites' db_name/db_user entries).
  depends_on = [
    google_secret_manager_secret_version.settings,
    google_secret_manager_secret_version.kubeconfig,
    google_secret_manager_secret_version.session_secret,
    google_secret_manager_secret_version.oidc_signing_key,
    google_secret_manager_secret_iam_member.hub_reads_settings,
    google_secret_manager_secret_iam_member.hub_reads_kubeconfig,
    google_secret_manager_secret_iam_member.hub_reads_session_secret,
    google_secret_manager_secret_iam_member.hub_reads_dsn,
    time_sleep.iam_propagation,
    terraform_data.boot_prerequisites,
  ]
}

resource "google_cloud_run_v2_service_iam_member" "iap_invoker" {
  provider = google-beta
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.hub.name
  role     = "roles/run.invoker"
  member   = "serviceAccount:service-${var.project_number}@gcp-sa-iap.iam.gserviceaccount.com"
}

resource "google_cloud_run_v2_service_iam_member" "transport_invoker" {
  provider = google-beta
  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.hub.name
  role     = "roles/run.invoker"
  member   = "serviceAccount:${var.transport_sa_email}"
}

# --- IAP OAuth client: NOT managed by Terraform (ptone decision, 21:55) ---
#
# `iap_enabled = true` above turns on direct IAP using the project's
# Google-managed OAuth client, which works immediately for in-org users —
# no client to create or bind. Terraform manages no google_iap_settings and
# reads no OAuth client secret (OQ-2 is moot: this was the "is the
# API-acceptance risk real" question for a resource that no longer exists
# in this module — see phase1-validation.md for the retired investigation).
# iap_oauth_client_id feeds exactly one thing, purely as data: the
# settings.yaml transport.oidc_audience agents present over IAP (see the
# variable's description and the check block below). A custom, console-
# created OAuth client for cross-org sign-in is a post-apply user step
# (README "IAP OAuth client") — Terraform never writes IAP OAuth settings,
# so it can never overwrite the live hub's.
check "transport_audience_configured" {
  assert {
    condition     = var.iap_oauth_client_id != null
    error_message = "transport auth disabled until iap_oauth_client_id is set — see the README's \"IAP OAuth client\" section to discover the Google-managed client ID (or create a custom one for cross-org) and re-apply."
  }
}

# Phase 2 hardening (item 3a). Warn-level, post-apply sanity: the actual
# service URI Cloud Run hands back should be https (IAP requires it; a plain
# http URI would mean IAP/the GFE path is misconfigured) and should equal
# the deterministic URL this module computed *before* creating the service
# (local.public_url) — the same identity triple (project_number, region,
# hub_name) local.iap_audience is built from. If the real URI ever disagreed
# with local.public_url, it would mean Cloud Run's actual URL-generation
# scheme diverged from what this module assumes, and local.iap_audience
# (settings.yaml's auth.proxy.iap.audience, which agents must match) would
# be assuming the wrong resource. outputs.tf's service_uri description has
# named this exact check as a phase 2 item since phase 1.
# NOT asserting google_cloud_run_v2_service.hub.uri == local.public_url here
# (found while writing this check, reproduced credential-free with a
# throwaway random_pet resource + a bare check block before touching this
# module): a top-level check block's condition — unlike a resource
# lifecycle{ postcondition {} }, which Terraform explicitly defers to apply
# time for the resource's own not-yet-known attributes — must be a KNOWN
# value at the moment Terraform evaluates it, and errors the entire plan
# ("Check block assertion known after apply") if it isn't, rather than
# merely warning. .uri is unknown until apply for any NEWLY CREATED service
# (every fresh hub's first plan, e.g. tfha-h3's), so that assertion would
# hard-block every future new hub's `terraform plan` — an F-112-class
# regression (design doc §9: "no fresh hub can plan") introduced by this
# very check, and exactly the failure mode fresh_hub_plan.tftest.hcl (item 6)
# exists to catch; it does, which is how this was found instead of shipped.
# It would NOT reproduce on the live h1/h2 update plan (.uri is already
# known from refreshed state, since neither hub's service is being
# replaced), but a check this module ships must hold for every hub,
# including ones that don't exist yet. Reported to tf-lead as a phase 2
# item 3a finding, not implemented as literally specified for that reason.
# What's left, verifiable and warn-level: the URI format this module commits
# to before the service exists at all.
check "service_uri_is_https" {
  assert {
    condition     = startswith(local.public_url, "https://")
    error_message = "local.public_url (the deterministic URL this module's settings.yaml and IAP audience assume) is not https: ${local.public_url}"
  }
}

# Phase 2 hardening (item 3b, design §3.7). Warn-level, post-apply sanity
# for the shared Cloud SQL instance's connection budget. See
# var.max_connections_budget's description for why this is a per-hub
# stand-in rather than a true sum-across-hubs check.
check "connection_budget" {
  assert {
    condition     = var.max_instances * local.hub_max_open_conns <= var.max_connections_budget
    error_message = "connection budget exceeded: max_instances (${var.max_instances}) * database.max_open_conns (${local.hub_max_open_conns}) = ${var.max_instances * local.hub_max_open_conns}, which is over this hub's max_connections_budget (${var.max_connections_budget}). Lower max_instances, or raise max_connections_budget only after confirming headroom against the shared Cloud SQL instance's max_connections across every hub attached to it (design §3.7)."
  }
}

resource "google_iap_web_cloud_run_service_iam_member" "members" {
  for_each               = toset(var.iap_members)
  project                = var.project_id
  location               = var.region
  cloud_run_service_name = google_cloud_run_v2_service.hub.name
  role                   = "roles/iap.httpsResourceAccessor"
  member                 = each.value
}

# Transport SA's IAP accessor, scoped to this hub's own service only (design
# §3.4 — moved here from hub-identity's project-wide grant, which reached
# every IAP-protected resource in the project including the live hubs).
resource "google_iap_web_cloud_run_service_iam_member" "transport" {
  project                = var.project_id
  location               = var.region
  cloud_run_service_name = google_cloud_run_v2_service.hub.name
  role                   = "roles/iap.httpsResourceAccessor"
  member                 = "serviceAccount:${var.transport_sa_email}"
}
