output "service_uri" {
  description = "Cloud Run service URI. An equality check against the locally-computed deterministic URL (public_url) was considered; check.service_uri_is_https's comment in main.tf explains why that specific equality assertion can't be a check block (it would hard-block every future fresh hub's first plan) and was not implemented that way."
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

output "bucket_lifecycle_rules" {
  description = "The artifacts bucket's lifecycle_rule blocks as configured, for fresh_hub_plan.tftest.hcl to assert the noncurrent-version cleanup rule exists and is scoped to ARCHIVED objects only — never CURRENT/live data. Returns local.artifacts_lifecycle_rules (config-derived, main.tf) rather than google_storage_bucket.artifacts.lifecycle_rule (the resource attribute): reading the resource attribute made this output change on every fresh deployment's first post-apply plan, because the provider normalises unset condition fields on refresh even though the resource itself has 0 changes (see the local's comment in main.tf)."
  value       = local.artifacts_lifecycle_rules
}
