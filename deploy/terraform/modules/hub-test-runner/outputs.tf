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

# account is derived from each grant resource's own attributes, so a grant
# that targets the wrong account shows up in this output: token-creator
# grants by service_account_id (the target account's resource name), the IAP
# grant by member (the account admitted). Anything that matches neither
# account reads "unknown".
locals {
  account_by_name = {
    (google_service_account.uat.name)    = "uat"
    (google_service_account.issuer.name) = "issuer"
  }

  account_by_member = {
    "serviceAccount:${google_service_account.uat.email}"    = "uat"
    "serviceAccount:${google_service_account.issuer.email}" = "issuer"
  }
}

output "iam_grants" {
  description = <<-EOT
    Every IAM grant this module makes, read from the grant resources, as a list of
    { account, role, member, resource }:
      account   the test-runner account the grant is about ("uat" or "issuer", else "unknown").
                Token-creator grants: the account the grant is ON (service_account_id),
                i.e. the account tokens can be minted for. IAP grant: the account the
                grant is FOR (member), i.e. the account IAP admits.
      role      the granted role.
      member    who receives the role: an impersonator for token-creator grants, the
                -uat account itself for the IAP grant.
      resource  what the grant is scoped to: "service_account" (the account above) or
                "backend_service:<name>".
    Values that depend on the accounts' computed name or email (account, and the IAP
    grant's member) are known after the accounts exist. For operators and tests.
  EOT
  value = concat(
    [for r in values(google_service_account_iam_member.uat_token_creator) : { account = lookup(local.account_by_name, r.service_account_id, "unknown"), role = r.role, member = r.member, resource = "service_account" }],
    [for r in values(google_service_account_iam_member.issuer_token_creator) : { account = lookup(local.account_by_name, r.service_account_id, "unknown"), role = r.role, member = r.member, resource = "service_account" }],
    [{ account = lookup(local.account_by_member, google_iap_web_backend_service_iam_member.uat.member, "unknown"), role = google_iap_web_backend_service_iam_member.uat.role, member = google_iap_web_backend_service_iam_member.uat.member, resource = "backend_service:${google_iap_web_backend_service_iam_member.uat.web_backend_service}" }],
  )
}

output "alerts_enabled" {
  description = "Whether the log-based metric and alert policy were created."
  value       = length(google_monitoring_alert_policy.foreign_token_mint) > 0
}
