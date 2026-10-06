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
  description = "Zones the hub's NEGs are created in (the NEG annotation's \"zones\") and read from: var.neg_zones, or the cluster's node_locations when that is null."
  value       = local.neg_zones
}
