output "service_uri" {
  description = "Cloud Run service URI. Design §3.6 named a check block asserting this equals the locally-computed deterministic URL (public_url) as a phase 2 item; check.service_uri_is_https's comment (main.tf, phase 2 hardening item 3a) explains why that specific equality assertion can't be a check block (it would hard-block every future fresh hub's first plan) and was not implemented that way."
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
