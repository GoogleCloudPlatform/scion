output "namespace" {
  description = "Namespace name (equals hub_name)."
  value       = kubernetes_namespace.this.metadata[0].name
}

output "pv_name" {
  description = "PersistentVolume name (\"<hub_name>-nfs\"). This is the value settings.yaml's workspace_storage.nfs.shares[0].pv_name must equal — the hub code uses it as the literal PVC claim name."
  value       = kubernetes_persistent_volume.this.metadata[0].name
}

output "pvc_name" {
  description = "PersistentVolumeClaim name (equals pv_name)."
  value       = kubernetes_persistent_volume_claim.this.metadata[0].name
}

output "nfs_export" {
  description = "Full NFS export path for this hub's subdirectory (\"<share_path>/<hub_name>\"), for hub-cloudrun's Cloud Run NFS volume."
  value       = "${var.nfs.share_path}/${var.hub_name}"
}

output "nfs_init_job_id" {
  description = "kubernetes_job_v1.nfs_init's own id (\"<namespace>/<name>\") — a real, apply-time attribute of the Job resource itself, unlike nfs_export above (a plain string computable before the Job ever runs). Feeds hub-cloudrun's boot_prerequisites so the Cloud Run service has a genuine implicit dependency on the Job actually finishing (wait_for_completion = true above creates the per-hub NFS subdirectory) rather than on a path string that exists in config regardless of whether the mkdir/chown ran. Deliberately .id, not .metadata[0].uid: the provider sets .id from the create response's ObjectMeta before wait_for_completion runs, while the uid is left null in state until the next refresh, so keying on uid made terraform_data.boot_prerequisites show a spurious 0/1/0 on every fresh hub's second plan."
  value       = kubernetes_job_v1.nfs_init.id
}

output "hub_rbac_created" {
  description = "Whether this module created the hub's Role/RoleBinding (var.create_hub_rbac). Exposed so a root can assert which side owns the hub's namespace RBAC: this module for Cloud Run hubs, the Helm chart for hub-gke."
  value       = length(kubernetes_role.hub) > 0
}
