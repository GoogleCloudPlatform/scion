output "uat_email" {
  description = "Email of <hub>-uat, the account IAP admits to this hub's backend service."
  value       = google_service_account.uat.email
}

output "uat_unique_id" {
  description = "Numeric unique ID of <hub>-uat."
  value       = google_service_account.uat.unique_id
}

output "issuer_email" {
  description = "Email of <hub>-issuer, the test-identity issuer account (no IAP access)."
  value       = google_service_account.issuer.email
}

output "issuer_unique_id" {
  description = "Numeric unique ID of <hub>-issuer."
  value       = google_service_account.issuer.unique_id
}

output "iam_grants" {
  description = "Every IAM grant this module makes, as { account, role, member, resource }, read from the resources. account, role and resource are known at plan time; the IAP grant's member is the -uat email, known after apply. For operators and tests."
  value = concat(
    [for r in values(google_service_account_iam_member.uat_token_creator) : { account = "uat", role = r.role, member = r.member, resource = "service_account" }],
    [for r in values(google_service_account_iam_member.issuer_token_creator) : { account = "issuer", role = r.role, member = r.member, resource = "service_account" }],
    [{ account = "uat", role = google_iap_web_backend_service_iam_member.uat.role, member = google_iap_web_backend_service_iam_member.uat.member, resource = "backend_service:${google_iap_web_backend_service_iam_member.uat.web_backend_service}" }],
  )
}

output "alerts_enabled" {
  description = "Whether the log-based metric and alert policy were created."
  value       = length(google_monitoring_alert_policy.foreign_token_mint) > 0
}
