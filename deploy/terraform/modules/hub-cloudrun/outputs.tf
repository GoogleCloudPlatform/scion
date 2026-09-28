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

output "liveness_probe_path" {
  description = "HTTP path configured for the hub container's liveness probe (phase 2 hardening item 2). Exposed purely for fresh_hub_plan.tftest.hcl (item 6): a test run block can only assert on a submodule's declared outputs, never reach a nested resource's attribute directly (see that test file's header on the module.hub_cloudrun.<resource> \"Unsupported attribute\" finding)."
  value       = google_cloud_run_v2_service.hub.template[0].containers[0].liveness_probe[0].http_get[0].path
}

output "bucket_lifecycle_rules" {
  description = "The artifacts bucket's lifecycle_rule blocks verbatim (phase 2 hardening item 4), for fresh_hub_plan.tftest.hcl (item 6) to assert the noncurrent-version cleanup rule exists and is scoped to ARCHIVED objects only — never CURRENT/live data. Same test-visibility reason as liveness_probe_path above."
  value       = google_storage_bucket.artifacts.lifecycle_rule
}
