output "service_uri" {
  value = module.hub_cloudrun.service_uri
}

output "iap_audience" {
  value = module.hub_cloudrun.iap_audience
}

output "bucket_name" {
  value = module.hub_cloudrun.bucket_name
}

output "bucket_lifecycle_rules" {
  value = module.hub_cloudrun.bucket_lifecycle_rules
}

output "namespace" {
  value = module.agent_runtime_k8s.namespace
}

output "pvc_name" {
  value = module.agent_runtime_k8s.pvc_name
}

output "nfs_export" {
  value = module.agent_runtime_k8s.nfs_export
}
