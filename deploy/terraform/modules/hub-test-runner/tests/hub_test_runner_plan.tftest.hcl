# Coverage for modules/hub-test-runner under a mock google provider: no
# credentials, no API calls. Proves which objects the module creates, with
# which exact roles and scopes, that the audit-log precondition fails closed,
# that the alert objects appear only on request, and the input validations.
# It cannot prove that GCP accepts the grants, that the audit config really
# logs GenerateIdToken, or that the alert filter matches a real log entry;
# those are live checks at review.
#
# Every email here is fake.
mock_provider "google" {}

variables {
  project_id           = "example-project"
  hub_name             = "tfha-gke-h3"
  impersonators        = ["serviceAccount:orchestrator@example-project.iam.gserviceaccount.com"]
  backend_service_name = "tfha-gke-h3-hub-backend"
}

# Default for every run: the project audits iamcredentials DATA_READ.
# Runs that test the precondition override this again.
override_data {
  target = data.google_project_iam_policy.this
  values = {
    policy_data = "{\"auditConfigs\":[{\"service\":\"iamcredentials.googleapis.com\",\"auditLogConfigs\":[{\"logType\":\"DATA_READ\"}]}]}"
  }
}

# The provider validates service_account_id's format even against the mock,
# and outputs read email and unique_id, so pin them.
override_resource {
  target = google_service_account.uat
  values = {
    name      = "projects/example-project/serviceAccounts/tfha-gke-h3-uat@example-project.iam.gserviceaccount.com"
    email     = "tfha-gke-h3-uat@example-project.iam.gserviceaccount.com"
    unique_id = "100000000000000000001"
  }
}

override_resource {
  target = google_service_account.issuer
  values = {
    name      = "projects/example-project/serviceAccounts/tfha-gke-h3-issuer@example-project.iam.gserviceaccount.com"
    email     = "tfha-gke-h3-issuer@example-project.iam.gserviceaccount.com"
    unique_id = "100000000000000000002"
  }
}

run "enabled_plans_exactly_five_objects" {
  command = plan

  # 2 accounts + 1 token-creator grant on each + 1 IAP grant on -uat = 5.
  assert {
    condition     = google_service_account.uat.account_id == "tfha-gke-h3-uat" && google_service_account.issuer.account_id == "tfha-gke-h3-issuer"
    error_message = "the accounts must be <hub>-uat and <hub>-issuer."
  }

  assert {
    condition     = length(google_service_account_iam_member.uat_token_creator) == 1 && length(google_service_account_iam_member.issuer_token_creator) == 1
    error_message = "one token-creator grant per impersonator on each account."
  }

  assert {
    condition     = length(output.iam_grants) == 3
    error_message = "exactly three IAM grants (two token-creator, one IAP) with one impersonator."
  }

  assert {
    condition = alltrue([
      for g in values(google_service_account_iam_member.uat_token_creator) :
      g.role == "roles/iam.serviceAccountOpenIdTokenCreator" && g.member == "serviceAccount:orchestrator@example-project.iam.gserviceaccount.com"
    ])
    error_message = "the -uat grant must be roles/iam.serviceAccountOpenIdTokenCreator for the impersonator."
  }

  assert {
    condition = alltrue([
      for g in values(google_service_account_iam_member.issuer_token_creator) :
      g.role == "roles/iam.serviceAccountOpenIdTokenCreator" && g.member == "serviceAccount:orchestrator@example-project.iam.gserviceaccount.com"
    ])
    error_message = "the -issuer grant must be roles/iam.serviceAccountOpenIdTokenCreator for the impersonator."
  }

  assert {
    condition = (
      google_iap_web_backend_service_iam_member.uat.role == "roles/iap.httpsResourceAccessor" &&
      google_iap_web_backend_service_iam_member.uat.web_backend_service == "tfha-gke-h3-hub-backend" &&
      google_iap_web_backend_service_iam_member.uat.project == "example-project"
    )
    error_message = "IAP access must be roles/iap.httpsResourceAccessor on the hub's own backend service."
  }

  assert {
    condition     = length(google_logging_metric.foreign_token_mint) == 0 && length(google_monitoring_alert_policy.foreign_token_mint) == 0 && !output.alerts_enabled
    error_message = "no alert objects without notification channels."
  }
}

# Against the MOCK provider only: resolves the computed emails so the IAP
# member, the grant targets, iam_grants' account values and the outputs can
# be checked.
run "outputs_and_grant_targets" {
  command = apply

  assert {
    condition = (
      output.uat_email == "tfha-gke-h3-uat@example-project.iam.gserviceaccount.com" &&
      output.uat_unique_id == "100000000000000000001" &&
      output.issuer_email == "tfha-gke-h3-issuer@example-project.iam.gserviceaccount.com" &&
      output.issuer_unique_id == "100000000000000000002"
    )
    error_message = "outputs must be the email and unique_id of both accounts."
  }

  assert {
    condition     = google_iap_web_backend_service_iam_member.uat.member == "serviceAccount:tfha-gke-h3-uat@example-project.iam.gserviceaccount.com"
    error_message = "the IAP grant's member must be the -uat account, never -issuer."
  }

  # The issuer has no IAP grant. iam_grants derives each grant's account from
  # the grant resource itself (token-creator: service_account_id; IAP:
  # member), so these fail if a grant targeted or admitted the wrong account.
  # Apply run: the accounts' name and email are only resolved after apply.
  assert {
    condition     = [for g in output.iam_grants : g.account if startswith(g.resource, "backend_service:")] == ["uat"]
    error_message = "only -uat may hold an IAP grant; -issuer must have none."
  }

  assert {
    condition     = [for g in output.iam_grants : "${g.account}|${g.resource}"] == ["uat|service_account", "issuer|service_account", "uat|backend_service:tfha-gke-h3-hub-backend"]
    error_message = "iam_grants must report a token-creator grant on each account and the IAP grant for -uat only."
  }

  assert {
    condition = (
      alltrue([for g in values(google_service_account_iam_member.uat_token_creator) : g.service_account_id == "projects/example-project/serviceAccounts/tfha-gke-h3-uat@example-project.iam.gserviceaccount.com"]) &&
      alltrue([for g in values(google_service_account_iam_member.issuer_token_creator) : g.service_account_id == "projects/example-project/serviceAccounts/tfha-gke-h3-issuer@example-project.iam.gserviceaccount.com"])
    )
    error_message = "each token-creator grant must target its own account."
  }
}

run "two_impersonators_two_grants_each" {
  command = plan

  variables {
    impersonators = [
      "serviceAccount:orchestrator@example-project.iam.gserviceaccount.com",
      "serviceAccount:runner-2@example-project.iam.gserviceaccount.com",
    ]
  }

  assert {
    condition     = length(google_service_account_iam_member.uat_token_creator) == 2 && length(google_service_account_iam_member.issuer_token_creator) == 2
    error_message = "one token-creator grant per impersonator on each account."
  }
}

# --- Audit-log precondition ---

run "precondition_fails_without_audit_config" {
  command = plan

  override_data {
    target = data.google_project_iam_policy.this
    values = {
      policy_data = "{\"bindings\":[{\"role\":\"roles/viewer\",\"members\":[\"user:someone@example.com\"]}]}"
    }
  }

  expect_failures = [
    google_service_account_iam_member.uat_token_creator,
    google_service_account_iam_member.issuer_token_creator,
    google_iap_web_backend_service_iam_member.uat,
  ]
}

run "precondition_fails_with_admin_read_only" {
  command = plan

  override_data {
    target = data.google_project_iam_policy.this
    values = {
      policy_data = "{\"auditConfigs\":[{\"service\":\"iamcredentials.googleapis.com\",\"auditLogConfigs\":[{\"logType\":\"ADMIN_READ\"},{\"logType\":\"DATA_WRITE\"}]}]}"
    }
  }

  expect_failures = [
    google_service_account_iam_member.uat_token_creator,
    google_service_account_iam_member.issuer_token_creator,
    google_iap_web_backend_service_iam_member.uat,
  ]
}

run "precondition_fails_for_another_service" {
  command = plan

  override_data {
    target = data.google_project_iam_policy.this
    values = {
      policy_data = "{\"auditConfigs\":[{\"service\":\"storage.googleapis.com\",\"auditLogConfigs\":[{\"logType\":\"DATA_READ\"}]}]}"
    }
  }

  expect_failures = [
    google_service_account_iam_member.uat_token_creator,
    google_service_account_iam_member.issuer_token_creator,
    google_iap_web_backend_service_iam_member.uat,
  ]
}

# DATA_READ that exempts an impersonator leaves its calls unlogged.
run "precondition_fails_when_impersonator_exempted" {
  command = plan

  override_data {
    target = data.google_project_iam_policy.this
    values = {
      policy_data = "{\"auditConfigs\":[{\"service\":\"iamcredentials.googleapis.com\",\"auditLogConfigs\":[{\"logType\":\"DATA_READ\",\"exemptedMembers\":[\"serviceAccount:orchestrator@example-project.iam.gserviceaccount.com\"]}]}]}"
    }
  }

  expect_failures = [
    google_service_account_iam_member.uat_token_creator,
    google_service_account_iam_member.issuer_token_creator,
    google_iap_web_backend_service_iam_member.uat,
  ]
}

# Exemptions union across configs: allServices exempting the impersonator
# leaves its calls unlogged even though iamcredentials exempts no one.
run "precondition_fails_when_all_services_exempts_impersonator" {
  command = plan

  override_data {
    target = data.google_project_iam_policy.this
    values = {
      policy_data = "{\"auditConfigs\":[{\"service\":\"allServices\",\"auditLogConfigs\":[{\"logType\":\"DATA_READ\",\"exemptedMembers\":[\"serviceAccount:orchestrator@example-project.iam.gserviceaccount.com\"]}]},{\"service\":\"iamcredentials.googleapis.com\",\"auditLogConfigs\":[{\"logType\":\"DATA_READ\"}]}]}"
    }
  }

  expect_failures = [
    google_service_account_iam_member.uat_token_creator,
    google_service_account_iam_member.issuer_token_creator,
    google_iap_web_backend_service_iam_member.uat,
  ]
}

# The mirror case: iamcredentials exempts the impersonator, allServices
# exempts no one.
run "precondition_fails_when_iamcredentials_exempts_impersonator" {
  command = plan

  override_data {
    target = data.google_project_iam_policy.this
    values = {
      policy_data = "{\"auditConfigs\":[{\"service\":\"allServices\",\"auditLogConfigs\":[{\"logType\":\"DATA_READ\"}]},{\"service\":\"iamcredentials.googleapis.com\",\"auditLogConfigs\":[{\"logType\":\"DATA_READ\",\"exemptedMembers\":[\"serviceAccount:orchestrator@example-project.iam.gserviceaccount.com\"]}]}]}"
    }
  }

  expect_failures = [
    google_service_account_iam_member.uat_token_creator,
    google_service_account_iam_member.issuer_token_creator,
    google_iap_web_backend_service_iam_member.uat,
  ]
}

# Exempted members are compared case-insensitively.
run "precondition_fails_when_impersonator_exempted_in_other_case" {
  command = plan

  override_data {
    target = data.google_project_iam_policy.this
    values = {
      policy_data = "{\"auditConfigs\":[{\"service\":\"iamcredentials.googleapis.com\",\"auditLogConfigs\":[{\"logType\":\"DATA_READ\",\"exemptedMembers\":[\"serviceAccount:Orchestrator@Example-Project.iam.gserviceaccount.com\"]}]}]}"
    }
  }

  expect_failures = [
    google_service_account_iam_member.uat_token_creator,
    google_service_account_iam_member.issuer_token_creator,
    google_iap_web_backend_service_iam_member.uat,
  ]
}

# Policy data that is not JSON: try() decodes it to no audit config, so the
# precondition fails closed.
run "precondition_fails_when_policy_unparseable" {
  command = plan

  override_data {
    target = data.google_project_iam_policy.this
    values = {
      policy_data = "not json"
    }
  }

  expect_failures = [
    google_service_account_iam_member.uat_token_creator,
    google_service_account_iam_member.issuer_token_creator,
    google_iap_web_backend_service_iam_member.uat,
  ]
}

# DATA_READ enabled in one config and an exemption for someone else in the
# other: (a) and (b) both hold.
run "precondition_passes_with_data_read_split_across_configs" {
  command = plan

  override_data {
    target = data.google_project_iam_policy.this
    values = {
      policy_data = "{\"auditConfigs\":[{\"service\":\"allServices\",\"auditLogConfigs\":[{\"logType\":\"ADMIN_READ\"}]},{\"service\":\"iamcredentials.googleapis.com\",\"auditLogConfigs\":[{\"logType\":\"DATA_READ\",\"exemptedMembers\":[\"user:someone-else@example.com\"]}]}]}"
    }
  }

  assert {
    condition     = length(output.iam_grants) == 3
    error_message = "DATA_READ in one of the two configs, exempting no impersonator, must satisfy the precondition."
  }
}

run "precondition_passes_with_all_services_data_read" {
  command = plan

  override_data {
    target = data.google_project_iam_policy.this
    values = {
      policy_data = "{\"auditConfigs\":[{\"service\":\"allServices\",\"auditLogConfigs\":[{\"logType\":\"ADMIN_READ\"},{\"logType\":\"DATA_READ\",\"exemptedMembers\":[\"user:someone-else@example.com\"]}]}]}"
    }
  }

  assert {
    condition     = length(output.iam_grants) == 3
    error_message = "allServices DATA_READ must satisfy the precondition."
  }
}

run "precondition_passes_with_iamcredentials_data_read" {
  command = plan

  # The file-level override: iamcredentials.googleapis.com DATA_READ.
  assert {
    condition     = length(output.iam_grants) == 3
    error_message = "iamcredentials DATA_READ must satisfy the precondition."
  }
}

# --- Optional alert ---

run "alert_objects_only_with_channels" {
  command = apply

  variables {
    alert_notification_channels = ["projects/example-project/notificationChannels/1234567890"]
  }

  assert {
    condition     = length(google_logging_metric.foreign_token_mint) == 1 && length(google_monitoring_alert_policy.foreign_token_mint) == 1 && output.alerts_enabled
    error_message = "notification channels must create one metric and one alert policy."
  }

  assert {
    condition     = google_monitoring_alert_policy.foreign_token_mint[0].notification_channels == tolist(["projects/example-project/notificationChannels/1234567890"])
    error_message = "the alert policy must notify the configured channels."
  }

  assert {
    condition = (
      strcontains(google_logging_metric.foreign_token_mint[0].filter, "logName=\"projects/example-project/logs/cloudaudit.googleapis.com%2Fdata_access\"") &&
      strcontains(google_logging_metric.foreign_token_mint[0].filter, "protoPayload.serviceName=\"iamcredentials.googleapis.com\"") &&
      strcontains(google_logging_metric.foreign_token_mint[0].filter, "protoPayload.methodName:\"GenerateIdToken\"") &&
      strcontains(google_logging_metric.foreign_token_mint[0].filter, "resource.type=\"service_account\"") &&
      strcontains(google_logging_metric.foreign_token_mint[0].filter, "resource.labels.email_id=(\"tfha-gke-h3-uat@example-project.iam.gserviceaccount.com\" OR \"tfha-gke-h3-issuer@example-project.iam.gserviceaccount.com\")") &&
      !strcontains(google_logging_metric.foreign_token_mint[0].filter, "protoPayload.resourceName") &&
      strcontains(google_logging_metric.foreign_token_mint[0].filter, "NOT protoPayload.authenticationInfo.principalEmail=(\"orchestrator@example-project.iam.gserviceaccount.com\")")
    )
    error_message = "the metric must count GenerateIdToken on either account by principals outside the impersonators."
  }

  assert {
    condition     = google_monitoring_alert_policy.foreign_token_mint[0].conditions[0].condition_threshold[0].filter == "metric.type=\"logging.googleapis.com/user/tfha-gke-h3-test-runner-foreign-token-mint\" AND resource.type=\"global\""
    error_message = "the alert policy must watch the module's log-based metric on the global resource."
  }

  assert {
    condition     = google_monitoring_alert_policy.foreign_token_mint[0].alert_strategy[0].auto_close == "1800s"
    error_message = "the alert policy must auto-close after 30 minutes without matches."
  }
}

# --- Validation ---

run "rejects_empty_impersonators" {
  command = plan

  variables {
    impersonators = []
  }

  expect_failures = [var.impersonators]
}

run "rejects_group_member" {
  command = plan

  variables {
    impersonators = ["group:testers@example.com"]
  }

  expect_failures = [var.impersonators]
}

run "rejects_user_member" {
  command = plan

  variables {
    impersonators = ["user:someone@example.com"]
  }

  expect_failures = [var.impersonators]
}

run "rejects_bare_email" {
  command = plan

  variables {
    impersonators = ["orchestrator@example-project.iam.gserviceaccount.com"]
  }

  expect_failures = [var.impersonators]
}

# The email part must be lowercase.
run "rejects_mixed_case_email" {
  command = plan

  variables {
    impersonators = ["serviceAccount:Orchestrator@example-project.iam.gserviceaccount.com"]
  }

  expect_failures = [var.impersonators]
}

# 24 characters: "<hub_name>-issuer" would be 31, over the 30 limit.
run "rejects_hub_name_too_long_for_issuer_id" {
  command = plan

  variables {
    hub_name = "abcdefghijklmnopqrstuvwx"
  }

  expect_failures = [var.hub_name]
}

# 23 characters: "<hub_name>-issuer" is exactly 30.
run "accepts_longest_hub_name" {
  command = plan

  variables {
    hub_name = "abcdefghijklmnopqrstuvw"
  }

  assert {
    condition     = length(google_service_account.issuer.account_id) == 30 && length(google_service_account.uat.account_id) == 27
    error_message = "a 23-character hub_name must give a 30-character -issuer ID."
  }
}
