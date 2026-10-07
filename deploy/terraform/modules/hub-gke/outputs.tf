output "neg_name" {
  description = "Name of the zonal NEGs GKE creates for the hub. Deliberately UNKNOWN until the NEG Service exists (it is gated on the Service's uid), so a front door that reads the NEGs from this value defers those reads to apply on a fresh hub instead of failing at plan on NEGs that do not exist yet."
  value       = kubernetes_service_v1.neg.metadata[0].uid == null ? local.neg_name : local.neg_name
}

output "neg_annotation" {
  description = "The NEG Service's cloud.google.com/neg annotation (JSON), including the \"zones\" the NEG controller pre-provisions. For operators and tests."
  value       = kubernetes_service_v1.neg.metadata[0].annotations["cloud.google.com/neg"]
}

output "namespace" {
  description = "The hub's own namespace (<hub_name>-system), where the release, the NEG Service and the session secret live."
  value       = kubernetes_namespace_v1.system.metadata[0].name
}

output "ksa" {
  description = "The hub pod's Kubernetes service account. hub-identity's hub_workload_identity_ksa must equal this."
  value = {
    namespace = local.namespace
    name      = var.ksa_name
  }
}

output "bucket_name" {
  value = google_storage_bucket.artifacts.name
}

output "hub_installed" {
  description = "Whether the Helm release (the hub itself) is part of this configuration. false means iap_oauth_client_id is unset and only the hub's surroundings were created; see the README."
  value       = local.hub_installed
}

output "chart_values" {
  description = "Every non-secret value handed to the chart (the DB password is not here; it goes through set_sensitive)."
  value       = local.chart_values
}
