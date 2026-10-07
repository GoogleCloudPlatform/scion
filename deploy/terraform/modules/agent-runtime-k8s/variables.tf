variable "hub_name" {
  description = "Hub name. Also the namespace name, and the base for the PV/PVC/StorageClass names (\"<hub_name>-nfs\") and the nfs-init Job's generated name prefix (\"<hub_name>-nfs-init-\")."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,15}$", var.hub_name))
    error_message = "hub_name must match ^[a-z][a-z0-9-]{2,15}$."
  }
}

variable "project_id" {
  description = "GCP project ID (used to build the Workload Identity member string)."
  type        = string
}

variable "hub_sa_email" {
  description = "Hub service account email. Bound as the RBAC Role's subject (GKE maps the Google identity to an RBAC User of this name)."
  type        = string
}

variable "hub_sa_unique_id" {
  description = "Hub SA's numeric unique_id (hub-identity output). Bound as a SECOND subject on the RoleBinding alongside hub_sa_email: the hub's k8s client falls back to pkg/k8s/client.go's fallbackToGCEAuth (the kubeconfig names the gke-gcloud-auth-plugin exec, absent from the image), which requests only the cloud-platform scope, not userinfo.email — so GKE identifies the caller by this numeric ID, not the SA's email, and the email-only subject never matches. Kept alongside the email subject (not replacing it) so the binding still matches if the client is ever fixed upstream to request userinfo.email."
  type        = string

  validation {
    condition     = can(regex("^[0-9]+$", var.hub_sa_unique_id))
    error_message = "hub_sa_unique_id must be a non-empty numeric string (GCP service account unique_id, e.g. \"115656325337183068810\")."
  }
}

variable "agent_sa_email" {
  description = "Agent service account email. Bound to the namespace's default KSA via Workload Identity."
  type        = string
}

variable "nfs" {
  description = "Shared Filestore coordinates (shared.nfs from shared-lookup): { server, share_path }."
  type = object({
    server     = string
    share_path = string
  })
}

variable "nfs_uid" {
  description = "uid the hub and agent pods run as; must match the export ownership the init Job sets."
  type        = number
  default     = 1000
}

variable "nfs_gid" {
  description = "gid the hub and agent pods run as; must match the export ownership the init Job sets."
  type        = number
  default     = 1000
}

variable "subpath_root" {
  description = "Subdirectory under the hub's NFS export that holds per-project workspaces."
  type        = string
  default     = "projects"

  validation {
    condition     = can(regex("^[A-Za-z0-9_-]{1,63}$", var.subpath_root))
    error_message = "subpath_root must match ^[A-Za-z0-9_-]{1,63}$ (letters, digits, underscore, dash; 1-63 characters)."
  }
}

variable "capacity" {
  description = "Advertised PV/PVC capacity. Filestore Basic doesn't enforce per-subdirectory quotas, so this is a Kubernetes-API-level number, not a hard limit."
  type        = string
  default     = "1Ti"
}

variable "init_job_image" {
  description = "Image for the nfs-init Job. Needs only a shell and coreutils (mkdir, chown); busybox is sufficient and avoids depending on hub_image. Pinned by digest, not a mutable tag (found in review: a tag pulled from Docker Hub on an Autopilot node is subject to both rate limits and tag mutation). Digest is for busybox:1.36 (Docker-Content-Digest from registry-1.docker.io as of this commit); re-verify if bumping."
  type        = string
  default     = "busybox:1.36@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662"
}

variable "create_hub_rbac" {
  description = "Create the hub's namespaced Role and RoleBinding (subjects: the hub GSA's email and numeric unique_id). Default true, which is what every Cloud Run hub needs and leaves its plan unchanged. hub-gke passes false: its Helm chart grants the hub pod's own Kubernetes service account in this namespace (rbac.create with runtime.namespace), and a second Role here would be an unused grant to identities the pod never presents."
  type        = bool
  default     = true
}
