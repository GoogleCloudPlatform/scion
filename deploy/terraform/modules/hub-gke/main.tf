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

  conduit_external_relay  = var.conduit_relay_mode == "external"
  conduit_hop_tls_enabled = var.hub_replica_count > 1 || local.conduit_external_relay

  # Mirrors scion-hub.fullname for release name = var.hub_name (3..16 chars, so
  # never hits the >63-char truncation branch). Used as the leaf cert SAN and
  # passed explicitly as conduit.internalTls.serverName so the certificate and
  # the chart's internal Service (<fullname>-relay.<namespace>.svc) agree by
  # construction.
  chart_fullname = (
    strcontains(local.release_name, "scion-hub") && !endswith(local.release_name, "-scion-hub")
    ? local.release_name
    : "${local.release_name}-scion-hub"
  )
  conduit_tls_server_name  = "${local.chart_fullname}-relay.${local.namespace}.svc"
  conduit_tls_secret_name  = "${var.hub_name}-conduit-tls"
  conduit_peer_secret_name = "${var.hub_name}-conduit-peer"
  conduit_peer_secret_key  = "SCION_SERVER_HUB_CONDUIT_PEERSECRET"

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
  chart_values = merge(
    {
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
          # Only these two keys: the chart's other hub.securityContext settings
          # are fixed in its templates, and Helm merges this map with the
          # chart's defaults.
          securityContext = {
            runAsUser  = var.hub_run_as_uid
            runAsGroup = var.hub_run_as_gid
          }
        },
        var.db_password_rotation == "" ? {} : {
          podAnnotations = {
            "scion.dev/db-password-rotation" = var.db_password_rotation
          }
        },
        # Only when enabled, so existing hubs' values stay byte-identical by
        # default (the chart's own defaults are "" and false). hub.hubClass =
        # "test" is required by the chart whenever hub.testIdentities.enabled is
        # true; split into two merge maps so each conditional unifies with {} as
        # a homogeneous map. Unrelated to test login, which the chart does not
        # offer.
        var.test_identities_enabled ? {
          hubClass = "test"
        } : {},
        var.test_identities_enabled ? {
          testIdentities = {
            enabled = true
          }
        } : {},
        var.enable_hub_pdb ? {
          pdb = {
            enabled = true
          }
        } : {},
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
        gke       = var.agent_runtime_gke_mode
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
        # The instance created by modules/cloudsql-instance has no public IP, and
        # the GKE cluster is VPC-native on the same privately connected network,
        # so the proxy must connect over the private IP.
        privateIp = true
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

      workspaceStorage = {
        nfs = {
          enabled     = true
          server      = var.nfs.server
          export      = var.nfs.export
          mountRoot   = var.nfs_mount_root
          shareId     = var.hub_name
          pvName      = var.nfs.pv_name
          subpathRoot = var.nfs_subpath_root
        }
      }
    },
    var.hub_replica_count > 1 ? {
      replicaCount = var.hub_replica_count
    } : {},
    (local.conduit_hop_tls_enabled && !local.conduit_external_relay) ? {
      conduit = {
        internalTls = {
          enabled        = true
          existingSecret = local.conduit_tls_secret_name
          serverName     = local.conduit_tls_server_name
        }
      }
    } : {},
    local.conduit_external_relay ? {
      conduit = {
        relayMode          = "external"
        peerAuth           = "hmac"
        existingPeerSecret = local.conduit_peer_secret_name
        internalTls = {
          enabled        = true
          existingSecret = local.conduit_tls_secret_name
          serverName     = local.conduit_tls_server_name
        }
      }
    } : {},
    local.conduit_external_relay ? {
      relay = merge(
        {
          enabled      = true
          replicaCount = var.relay_replica_count
          serviceAccount = merge(
            {
              create = true
              name   = var.relay_ksa_name
            },
            var.relay_sa_email != null ? {
              gcpServiceAccount = var.relay_sa_email
            } : {},
          )
        },
        var.enable_relay_pdb ? {
          pdb = {
            enabled = true
          }
        } : {},
        var.relay_db_user != null ? {
          database = {
            user = var.relay_db_user
          }
        } : {},
        var.db_password_rotation != "" ? {
          podAnnotations = {
            "scion.dev/db-password-rotation" = var.db_password_rotation
          }
        } : {},
      )
    } : {},
  )

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

# --- Conduit internal hop TLS & external relay peer secret ---
#
# Provisioned when hub_replica_count > 1 or conduit_relay_mode == "external".
# The leaf certificate carries SAN <fullname>-relay.<namespace>.svc (matching
# conduit.internalTls.serverName and the chart's internal Service), signed by a
# per-hub CA stored alongside tls.crt/tls.key in kubernetes_secret_v1.conduit_tls.
resource "tls_private_key" "conduit_ca" {
  count = local.hub_installed && local.conduit_hop_tls_enabled ? 1 : 0

  algorithm   = "ECDSA"
  ecdsa_curve = "P256"
}

resource "tls_self_signed_cert" "conduit_ca" {
  count = local.hub_installed && local.conduit_hop_tls_enabled ? 1 : 0

  private_key_pem   = tls_private_key.conduit_ca[0].private_key_pem
  is_ca_certificate = true

  subject {
    common_name = "${var.hub_name}-conduit-ca"
  }

  validity_period_hours = 87600 # 10 years
  early_renewal_hours   = 2160  # 90 days

  allowed_uses = [
    "cert_signing",
    "crl_signing",
  ]
}

resource "tls_private_key" "conduit_leaf" {
  count = local.hub_installed && local.conduit_hop_tls_enabled ? 1 : 0

  algorithm   = "ECDSA"
  ecdsa_curve = "P256"
}

resource "tls_cert_request" "conduit_leaf" {
  count = local.hub_installed && local.conduit_hop_tls_enabled ? 1 : 0

  private_key_pem = tls_private_key.conduit_leaf[0].private_key_pem

  subject {
    common_name = local.conduit_tls_server_name
  }

  dns_names = [
    local.conduit_tls_server_name,
    "${local.conduit_tls_server_name}.cluster.local",
  ]
}

resource "tls_locally_signed_cert" "conduit_leaf" {
  count = local.hub_installed && local.conduit_hop_tls_enabled ? 1 : 0

  cert_request_pem   = tls_cert_request.conduit_leaf[0].cert_request_pem
  ca_private_key_pem = tls_private_key.conduit_ca[0].private_key_pem
  ca_cert_pem        = tls_self_signed_cert.conduit_ca[0].cert_pem

  validity_period_hours = 8760 # 1 year
  early_renewal_hours   = 720  # 30 days

  allowed_uses = [
    "key_encipherment",
    "digital_signature",
    "server_auth",
    "client_auth",
  ]
}

resource "kubernetes_secret_v1" "conduit_tls" {
  count = local.hub_installed && local.conduit_hop_tls_enabled ? 1 : 0

  metadata {
    name      = local.conduit_tls_secret_name
    namespace = kubernetes_namespace_v1.system.metadata[0].name
  }

  data = {
    "tls.crt" = tls_locally_signed_cert.conduit_leaf[0].cert_pem
    "tls.key" = tls_private_key.conduit_leaf[0].private_key_pem
    "ca.crt"  = tls_self_signed_cert.conduit_ca[0].cert_pem
  }

  type = "kubernetes.io/tls"
}

# Shared relay-peer HMAC secret for external relay mode (48 octets, distinct
# from the 64-octet session secret).
resource "random_password" "conduit_peer" {
  count = local.hub_installed && local.conduit_external_relay ? 1 : 0

  length  = 48
  special = false
}

resource "kubernetes_secret_v1" "conduit_peer" {
  count = local.hub_installed && local.conduit_external_relay ? 1 : 0

  metadata {
    name      = local.conduit_peer_secret_name
    namespace = kubernetes_namespace_v1.system.metadata[0].name
  }

  data = {
    (local.conduit_peer_secret_key) = random_password.conduit_peer[0].result
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

  set_sensitive = concat(
    [
      {
        name  = "database.password"
        value = var.db_password
        type  = "string"
      },
    ],
    local.conduit_external_relay && var.relay_db_user != null ? [
      {
        name  = "relay.database.password"
        value = var.relay_db_password
        type  = "string"
      },
    ] : [],
  )

  wait    = true
  timeout = var.helm_timeout

  depends_on = [
    kubernetes_secret_v1.session,
    kubernetes_secret_v1.conduit_tls,
    kubernetes_secret_v1.conduit_peer,
    google_storage_bucket_iam_member.hub_object_admin,
    terraform_data.boot_prerequisites,
  ]
}
