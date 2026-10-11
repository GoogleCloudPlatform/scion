output "public_url" {
  description = "The hub's URL (https://<hostname>)."
  value       = module.hub_lb.public_url
}

output "iap_audience" {
  description = "The IAP JWT audience the hub verifies (/projects/<number>/global/backendServices/<id>)."
  value       = module.hub_lb.iap_audience
}

output "lb_ip" {
  value = module.hub_lb.lb_ip
}

output "dns_record" {
  description = "Create this record in your external DNS. The managed certificate stays PROVISIONING until it resolves."
  value       = module.hub_lb.dns_record
}

output "hub_installed" {
  description = "false means iap_oauth_client_id is unset and the hub itself (helm_release) was skipped. See the README's \"First install\"."
  value       = module.hub_gke.hub_installed
}

output "backend_service" {
  description = "Backend service name, timeout and health-check path."
  value       = module.hub_lb.backend_service
}

output "hub_image" {
  description = "The hub image the chart runs, repository@digest."
  value       = "${module.hub_gke.chart_values.image.repository}@${module.hub_gke.chart_values.image.digest}"
}

output "chart_values" {
  description = "Every non-secret value handed to the scion-hub chart."
  value       = module.hub_gke.chart_values
}

output "hub_namespace" {
  description = "Namespace of the hub's release and NEG Service (<hub_name>-system)."
  value       = module.hub_gke.namespace
}

output "agent_namespace" {
  description = "Namespace agent pods run in (<hub_name>)."
  value       = module.agent_runtime_k8s.namespace
}

output "hub_rbac_created_by_terraform" {
  description = "Whether agent-runtime-k8s created the hub Role/RoleBinding. Always false here: the chart owns the hub's RBAC."
  value       = module.agent_runtime_k8s.hub_rbac_created
}

output "bucket_name" {
  value = module.hub_gke.bucket_name
}

output "neg_zones" {
  description = "Zones the hub's NEGs are created in (the NEG annotation's \"zones\") and read from: always the cluster's node_locations."
  value       = local.neg_zones
}

# This hub's workspace share as the hub sees it. Its preconditions require it
# to be the share agent_runtime_k8s built: the hub mounts <server>:<export> at
# <mount_root>/<share_id> and agents mount the claim by subPath, so the hub and
# agents see one tree only when share_id is the agent namespace and the last
# segment of the export, and pv_name is the claim agent_runtime_k8s creates.
# With backend nfs the hub's /readyz stays 503 until that mount exists, so a
# mismatch here is a hub that never becomes ready. Output preconditions fail
# the plan (a check block would only warn) and add nothing to state.
output "nfs_workspace" {
  description = "This hub's NFS workspace share: { share_id, export, pv_name, mount_path }. mount_path is where the chart mounts the export in the hub pod (<mount_root>/<share_id>), the path the hub's /readyz checks."
  value = {
    share_id   = module.hub_gke.chart_values.workspaceStorage.nfs.shareId
    export     = module.hub_gke.chart_values.workspaceStorage.nfs.export
    pv_name    = module.hub_gke.chart_values.workspaceStorage.nfs.pvName
    mount_path = "${module.hub_gke.chart_values.workspaceStorage.nfs.mountRoot}/${module.hub_gke.chart_values.workspaceStorage.nfs.shareId}"
  }

  precondition {
    condition     = trimspace(module.shared_lookup.shared.nfs.server == null ? "" : module.shared_lookup.shared.nfs.server) != ""
    error_message = "the shared Filestore instance has no IP address, so the hub's NFS server would be empty. Check that configurations/shared-infra created <shared_prefix>-nfs and that shared_prefix is right."
  }

  precondition {
    condition     = startswith(module.hub_gke.chart_values.workspaceStorage.nfs.export, "/")
    error_message = "the hub's NFS export \"${module.hub_gke.chart_values.workspaceStorage.nfs.export}\" must be an absolute path starting with \"/\"."
  }

  precondition {
    condition = (
      module.hub_gke.chart_values.workspaceStorage.nfs.shareId == module.agent_runtime_k8s.namespace &&
      endswith(module.hub_gke.chart_values.workspaceStorage.nfs.export, "/${module.hub_gke.chart_values.workspaceStorage.nfs.shareId}") &&
      module.hub_gke.chart_values.workspaceStorage.nfs.pvName == module.agent_runtime_k8s.pvc_name &&
      module.agent_runtime_k8s.pvc_name == "${module.hub_gke.chart_values.workspaceStorage.nfs.shareId}-nfs"
    )
    error_message = "the hub's NFS shareId \"${module.hub_gke.chart_values.workspaceStorage.nfs.shareId}\" must equal the agent namespace (\"${module.agent_runtime_k8s.namespace}\") and the last segment of the export (\"${module.hub_gke.chart_values.workspaceStorage.nfs.export}\"), and pvName (\"${module.hub_gke.chart_values.workspaceStorage.nfs.pvName}\") must be the agent claim \"<shareId>-nfs\" (\"${module.agent_runtime_k8s.pvc_name}\"). Otherwise the hub and agents do not see the same workspace tree."
  }
}
