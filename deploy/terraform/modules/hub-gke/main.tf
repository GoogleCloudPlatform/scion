# One Scion hub on the shared GKE Autopilot cluster, installed from the
# in-repo Helm chart (deploy/helm/scion-hub).
#
# Front-door contract: from the front door this module takes only
# var.public_url and var.iap_audience. It never references any other
# attribute of the load balancer, so another front that produces the same two
# values can replace it.
#
# Ordering within the root (one apply):
#   1. kubernetes_service_v1.neg (this module) -> GKE creates the zonal NEGs
#   2. the front door reads the NEGs (via the neg_name output, unknown until
#      the Service exists)
#   3. the front door's backend service -> iap_audience
#   4. helm_release.hub (this module), which needs iap_audience

locals {
  namespace    = "${var.hub_name}-system"
  release_name = var.hub_name
  neg_name     = "${var.hub_name}-hub-neg"
  chart_path   = coalesce(var.chart_path, "${path.module}/../../../helm/scion-hub")

  # The chart's selector labels: a tested, stable contract of the chart
  # (scion-hub.selectorLabels). name is the chart name (no nameOverride is
  # set below); instance is the release name.
  selector_labels = {
    "app.kubernetes.io/name"     = "scion-hub"
    "app.kubernetes.io/instance" = local.release_name
  }

  # Session secret key the chart reads from auth.existingSecret (its
  # default for auth.existingSecretKey).
  session_secret_key = "SCION_SERVER_SESSION_SECRET"

  hub_installed = var.iap_oauth_client_id != null

  # Same rules as hub-cloudrun's artifacts bucket: only ARCHIVED versions
  # expire, and stalled multipart uploads are aborted. See that module for
  # why no rule touches CURRENT objects and why with_state = "ANY" is
  # spelled out.
  artifacts_lifecycle_rules = [
    {
      condition = { with_state = "ARCHIVED", age = 30 }
      action    = { type = "Delete" }
    },
    {
      condition = { with_state = "ANY", age = 7 }
      action    = { type = "AbortIncompleteMultipartUpload" }
    },
  ]

  # Every non-secret chart value, typed. The DB password is the only value
  # not in here; it goes through set_sensitive on the release. Exposed as
  # the chart_values output, so tests and operators can read exactly what
  # was handed to Helm.
  chart_values = {
    image = {
      repository = var.image_repository
      digest     = var.image_digest
    }

    hub = merge(
      {
        hubId       = var.hub_name
        baseUrl     = var.public_url
        adminEmails = var.admin_emails
        webPort     = var.port
      },
      var.db_password_rotation == "" ? {} : {
        podAnnotations = {
          "scion.io/db-password-rotation" = var.db_password_rotation
        }
      },
    )

    serviceAccount = {
      create            = true
      name              = var.ksa_name
      gcpServiceAccount = var.hub_sa_email
    }

    rbac = {
      create = true
    }

    runtime = {
      namespace = var.runtime_namespace
    }

    agents = {
      imageRegistry = var.image_registry
    }

    database = {
      driver = "postgres"
      auth   = "password"
      name   = var.db_name
      user   = var.db_user
    }

    cloudsql = {
      enabled                = true
      instanceConnectionName = var.sql_connection_name
    }

    storage = {
      provider = "gcs"
      bucket   = google_storage_bucket.artifacts.name
    }

    secrets = {
      backend = "gcpsm"
      gcpsm = {
        projectId = var.project_id
      }
    }

    auth = {
      mode = "proxy"
      proxy = {
        provider = "iap"
        iap = {
          audience = var.iap_audience
        }
      }
      transport = {
        mode           = "iap"
        oidcAudience   = var.iap_oauth_client_id
        platformAuthSa = var.transport_sa_email
      }
      existingSecret = local.session_secret_name
    }
  }

  session_secret_name = "${var.hub_name}-session"
}

# --- Namespace ---

resource "kubernetes_namespace_v1" "system" {
  metadata {
    name = local.namespace
  }
}

# --- NEG Service ---
#
# Terraform owns the Service that carries the NEG annotation, not the chart,
# so the front door can depend on it without depending on the Helm release
# (which itself needs the front door's iap_audience). It selects the chart's
# pods by the chart's selector labels. ClusterIP is all a standalone NEG
# needs. The NEG controller adds a cloud.google.com/neg-status annotation
# back onto this Service; the root's kubernetes provider ignores it.
#
# "zones" pre-provisions a NEG in every listed zone, with or without nodes
# there. Without it the controller creates NEGs only in zones that have
# nodes, and the front door's per-zone NEG reads fail for the others. Needs
# GKE 1.36.2-gke.3104000 or later (the calling configuration checks); a
# malformed value makes the controller silently fall back to nodes-only
# zones, with only a Warning event on this Service.
# https://docs.cloud.google.com/kubernetes-engine/docs/how-to/standalone-neg
resource "kubernetes_service_v1" "neg" {
  metadata {
    name      = "${var.hub_name}-neg"
    namespace = kubernetes_namespace_v1.system.metadata[0].name

    annotations = {
      "cloud.google.com/neg" = jsonencode({
        exposed_ports = {
          (tostring(var.port)) = {
            name = local.neg_name
          }
        }
        zones = var.neg_zones
      })
    }
  }

  spec {
    type     = "ClusterIP"
    selector = local.selector_labels

    port {
      name        = "http"
      port        = var.port
      target_port = var.port
      protocol    = "TCP"
    }
  }
}

# --- Session secret ---
#
# Generated once and kept in state; it does not rotate on helm upgrade (the
# chart refuses to generate one for that reason). Only the release reads it,
# so it is gated with the release.
resource "random_password" "session" {
  count = local.hub_installed ? 1 : 0

  length  = 64
  special = false
}

resource "kubernetes_secret_v1" "session" {
  count = local.hub_installed ? 1 : 0

  metadata {
    name      = local.session_secret_name
    namespace = kubernetes_namespace_v1.system.metadata[0].name
  }

  data = {
    (local.session_secret_key) = one(random_password.session[*].result)
  }

  type = "Opaque"
}

# --- Artifacts bucket ---
#
# Not gated on the release: it is durable storage, and creating it on the
# first apply surfaces a global bucket-name collision before anything else
# depends on the name.
resource "google_storage_bucket" "artifacts" {
  project                     = var.project_id
  name                        = "${var.project_id}-${var.hub_name}-artifacts"
  location                    = var.region
  uniform_bucket_level_access = true

  versioning {
    enabled = true
  }

  dynamic "lifecycle_rule" {
    for_each = local.artifacts_lifecycle_rules
    content {
      condition {
        with_state = lifecycle_rule.value.condition.with_state
        age        = lifecycle_rule.value.condition.age
      }
      action {
        type = lifecycle_rule.value.action.type
      }
    }
  }
}

resource "google_storage_bucket_iam_member" "hub_object_admin" {
  bucket = google_storage_bucket.artifacts.name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${var.hub_sa_email}"
}

# --- Release ---

# Carries var.boot_prerequisites (real resource IDs) so the release waits for
# them, without a module-level depends_on that would defer every resource in
# this module (see hub-cloudrun's boot_prerequisites for that history).
resource "terraform_data" "boot_prerequisites" {
  input = var.boot_prerequisites
}

# The chart refuses to render without auth.transport.oidcAudience on this
# shape, so with no client ID there is no release at all, rather than a
# release that fails to render. hub_installed reports which case a plan is
# in. The warning-level check for it (transport_audience_configured) lives in
# the calling configuration, where the operator sets the variable: a check
# inside a child module cannot be named in a root test's expect_failures.
resource "helm_release" "hub" {
  count = local.hub_installed ? 1 : 0

  name      = local.release_name
  namespace = kubernetes_namespace_v1.system.metadata[0].name
  chart     = local.chart_path

  values = [yamlencode(local.chart_values)]

  set_sensitive = [
    {
      name  = "database.password"
      value = var.db_password
      type  = "string"
    },
  ]

  wait    = true
  timeout = var.helm_timeout

  depends_on = [
    kubernetes_secret_v1.session,
    google_storage_bucket_iam_member.hub_object_admin,
    terraform_data.boot_prerequisites,
  ]
}
