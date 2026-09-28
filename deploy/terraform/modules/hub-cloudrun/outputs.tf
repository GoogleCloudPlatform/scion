output "service_uri" {
  description = "Cloud Run service URI. Asserted equal to the locally-computed deterministic URL (public_url) by check.service_uri_matches_computed (design §3.6, phase 2 hardening item 3a)."
  value       = google_cloud_run_v2_service.hub.uri
}

output "iap_audience" {
  description = "IAP resource-path audience used in settings.yaml auth.proxy.iap.audience."
  value       = local.iap_audience
}

output "bucket_name" {
  description = "Artifacts bucket name."
  value       = google_storage_bucket.artifacts.name
}
