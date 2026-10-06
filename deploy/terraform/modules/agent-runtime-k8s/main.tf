# Per-hub Kubernetes resources on the shared GKE Autopilot cluster. The
# kubernetes provider is configured in the hub root from
# shared.gke; this module declares no provider blocks.

resource "kubernetes_namespace" "this" {
  metadata {
    name = var.hub_name
  }
}

# Minimum RBAC from kubernetes.md L169-215, cross-checked against
# pkg/runtime/k8s_runtime.go's actual API calls. Namespaced: hub A gets no
# rights in hub B's namespace (a project-wide container.developer role,
# as used in some non-Terraform deployment docs, would remove that
# isolation).
#
# create_hub_rbac = false skips this Role and its RoleBinding below. Only
# hub-gke does that: its Helm chart renders its own Role/RoleBinding in this
# namespace for the hub pod's Kubernetes service account (rbac.create,
# runtime.namespace), and a hub pod authenticates to the API server as that
# KSA, not as the hub GSA's email or unique_id, so these subjects would match
# nothing there.
resource "kubernetes_role" "hub" {
  count = var.create_hub_rbac ? 1 : 0

  metadata {
    name      = "${var.hub_name}-hub"
    namespace = kubernetes_namespace.this.metadata[0].name
  }

  rule {
    api_groups = [""]
    resources  = ["pods"]
    verbs      = ["create", "get", "list", "watch", "delete"]
  }

  rule {
    api_groups = [""]
    resources  = ["pods/exec"]
    verbs      = ["create"]
  }

  rule {
    api_groups = [""]
    resources  = ["pods/log"]
    verbs      = ["get"]
  }

  rule {
    api_groups = [""]
    resources  = ["secrets"]
    verbs      = ["create", "get", "list", "delete"]
  }

  rule {
    api_groups = ["secrets-store.csi.x-k8s.io"]
    resources  = ["secretproviderclasses"]
    verbs      = ["create", "get", "list", "delete"]
  }

  rule {
    api_groups = [""]
    resources  = ["persistentvolumeclaims"]
    verbs      = ["create", "get", "list", "delete"]
  }
}

# GKE maps the hub SA's Google identity to an RBAC User of the same name —
# in principle. In practice: the hub's k8s client
# authenticates via pkg/k8s/client.go's fallbackToGCEAuth (the kubeconfig
# this module's caller renders names the gke-gcloud-auth-plugin exec, which
# the hub image doesn't have), and that fallback requests only the
# cloud-platform OAuth scope — not userinfo.email. Without userinfo.email,
# GKE can't resolve the caller's email and instead identifies it by the SA's
# numeric unique_id, so an email-only subject here never matches: every
# heartbeat logged `pods is forbidden: User "115656325337183068810"`, and
# pod create would have been denied the same way. Two subjects, both kept:
# the unique_id one is what actually matches today; the email one is kept so
# this binding needs no further change if the client is ever fixed upstream
# to request userinfo.email — dropping it now would just trade today's
# outage for a silent one later.
resource "kubernetes_role_binding" "hub" {
  count = var.create_hub_rbac ? 1 : 0

  metadata {
    name      = "${var.hub_name}-hub"
    namespace = kubernetes_namespace.this.metadata[0].name
  }

  role_ref {
    api_group = "rbac.authorization.k8s.io"
    kind      = "Role"
    name      = one(kubernetes_role.hub[*].metadata[0].name)
  }

  # Matches nothing today (GKE can't see this email without
  # userinfo.email), but kept for when the upstream client fix lands.
  subject {
    api_group = "rbac.authorization.k8s.io"
    kind      = "User"
    name      = var.hub_sa_email
  }

  # This is the subject that actually matches while the hub's k8s
  # client only presents the cloud-platform scope.
  subject {
    api_group = "rbac.authorization.k8s.io"
    kind      = "User"
    name      = var.hub_sa_unique_id
  }
}

# Both resources gained count = var.create_hub_rbac ? 1 : 0, which moves
# their addresses to [0]. Without these moved blocks every already-applied
# hub would plan a destroy and re-create of its RBAC, a window in which the
# hub's pod operations are denied.
moved {
  from = kubernetes_role.hub
  to   = kubernetes_role.hub[0]
}

moved {
  from = kubernetes_role_binding.hub
  to   = kubernetes_role_binding.hub[0]
}

# Agent pods run as the namespace's pre-existing "default" KSA (setup-gcp.md
# annotates this same KSA, not a dedicated one) bound to the agent GSA via
# Workload Identity, implicit on Autopilot.
resource "kubernetes_annotations" "default_ksa_workload_identity" {
  api_version = "v1"
  kind        = "ServiceAccount"

  metadata {
    name      = "default"
    namespace = kubernetes_namespace.this.metadata[0].name
  }

  annotations = {
    "iam.gke.io/gcp-service-account" = var.agent_sa_email
  }

  depends_on = [kubernetes_namespace.this]
}

resource "google_service_account_iam_member" "agent_workload_identity_user" {
  service_account_id = "projects/${var.project_id}/serviceAccounts/${var.agent_sa_email}"
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${var.hub_name}/default]"
}

# The Filestore export root is root:root 0755. The per-hub subdirectory (and
# its "projects" subpath) must exist and be owned by nfs_uid:nfs_gid *before*
# Cloud Run mounts it, or the hub's Cloud Run revision fails to start.
# Mounts the share ROOT with an inline nfs volume (Autopilot allows this for
# a Job; no PV needed for the init step itself).
#
# Deliberately no ttl_seconds_after_finished: a TTL garbage-
# collects the Job, and every later plan would then re-create it, which
# breaks the "second plan shows no changes" expectation for a stable
# deployment. The completed Job stays in the
# namespace; it costs nothing on Autopilot.
resource "kubernetes_job_v1" "nfs_init" {
  metadata {
    generate_name = "${var.hub_name}-nfs-init-"
    namespace     = kubernetes_namespace.this.metadata[0].name
  }

  spec {
    backoff_limit = 3

    template {
      # No metadata.name here: the kubernetes_job_v1 provider schema rejects
      # spec.template.metadata.name together with the Job's own
      # metadata.generate_name ("Conflicting configuration arguments"). The
      # ConflictsWith is the absolute path metadata.0.generate_name, i.e. a
      # provider schema quirk, not a Kubernetes rule. The pod template needs
      # no name of its own; Kubernetes names the pods it creates from this
      # template.
      metadata {}

      spec {
        restart_policy = "OnFailure"

        security_context {
          run_as_user  = 0
          run_as_group = 0
        }

        container {
          name    = "nfs-init"
          image   = var.init_job_image
          command = ["sh", "-c"]
          # subpath_root is passed via env rather than interpolated straight
          # into the shell script text, so the script body itself is a fixed
          # string regardless of the configured value. The variable also has
          # its own validation block.
          args = [
            "mkdir -p \"/mnt/share/${var.hub_name}/$SUBPATH_ROOT\" && chown ${var.nfs_uid}:${var.nfs_gid} \"/mnt/share/${var.hub_name}\" \"/mnt/share/${var.hub_name}/$SUBPATH_ROOT\""
          ]

          env {
            name  = "SUBPATH_ROOT"
            value = var.subpath_root
          }

          volume_mount {
            name       = "share-root"
            mount_path = "/mnt/share"
          }
        }

        volume {
          name = "share-root"
          nfs {
            server = var.nfs.server
            path   = var.nfs.share_path
          }
        }
      }
    }
  }

  wait_for_completion = true

  timeouts {
    create = "5m"
  }

  # Autopilot's warden webhook injects fields into the pod template at
  # create time (a seccomp profile, dropped container capabilities,
  # allow_privilege_escalation, and an arch toleration) that aren't in this
  # config. Since the pod template is immutable, every subsequent plan sees
  # those injected values as drift and wants to replace the whole Job —
  # breaking the "second plan, no changes" expectation for a stable
  # deployment. Ignore exactly the three spec
  # paths Autopilot injects into; nothing broader, and none of these values
  # are declared in config on purpose (live-plan testing against a real
  # Autopilot cluster confirmed the spec-level paths alone are sufficient
  # here — container[0].resources showed no drift and was dropped from this
  # list).
  #
  # The warden also stamps two annotations on the object
  # (autopilot.gke.io/warden-version, .../resource-adjustment), which is
  # handled provider-wide in configurations/hub's kubernetes provider block
  # (ignore_annotations), not per-resource here — it's a cluster-wide
  # Autopilot behavior that will also touch agent pods and the namespace,
  # not something specific to this one Job.
  #
  # Caveat: ignoring the whole security_context blocks means
  # our own declared run_as_user/run_as_group = 0 is also frozen after
  # first apply — if that ever needs to change, this ignore_changes entry
  # has to be edited (or removed and reapplied) alongside it. Narrower
  # per-field paths aren't an option here: the warden injects into the same
  # blocks we set fields in, so there's no way to ignore only its fields
  # and not ours within security_context.
  lifecycle {
    ignore_changes = [
      spec[0].template[0].spec[0].security_context,
      spec[0].template[0].spec[0].container[0].security_context,
      spec[0].template[0].spec[0].toleration,
    ]
  }
}

# storage_class_name = "" does not bind on GKE: the cluster's default-class
# admission controller sets an unset/empty PVC storage class to the default
# (standard-rwo), which never matches this static NFS PV. A dedicated
# no-provisioner StorageClass, referenced explicitly by both the PV and the
# PVC, is the fix — it's the standard pattern for static/pre-provisioned
# volumes.
resource "kubernetes_storage_class_v1" "nfs" {
  metadata {
    name = "${var.hub_name}-nfs"
  }

  storage_provisioner = "kubernetes.io/no-provisioner"
  reclaim_policy      = "Retain"
  volume_binding_mode = "Immediate"
}

resource "kubernetes_persistent_volume" "this" {
  metadata {
    name = "${var.hub_name}-nfs"
  }

  spec {
    capacity = {
      storage = var.capacity
    }
    access_modes                     = ["ReadWriteMany"]
    persistent_volume_reclaim_policy = "Retain"
    storage_class_name               = kubernetes_storage_class_v1.nfs.metadata[0].name
    mount_options                    = ["vers=3", "hard", "nconnect=4"]

    persistent_volume_source {
      nfs {
        server = var.nfs.server
        path   = "${var.nfs.share_path}/${var.hub_name}"
      }
    }
  }

  depends_on = [kubernetes_job_v1.nfs_init]
}

resource "kubernetes_persistent_volume_claim" "this" {
  metadata {
    name      = "${var.hub_name}-nfs"
    namespace = kubernetes_namespace.this.metadata[0].name
  }

  spec {
    access_modes       = ["ReadWriteMany"]
    storage_class_name = kubernetes_storage_class_v1.nfs.metadata[0].name
    volume_name        = kubernetes_persistent_volume.this.metadata[0].name

    resources {
      requests = {
        storage = var.capacity
      }
    }
  }

  wait_until_bound = true
}
