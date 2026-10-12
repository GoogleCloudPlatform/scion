# Test-runner identities for one test hub: two service accounts an agent on
# another hub can impersonate to run user-surface tests against this hub,
# without carrying its own hub identity here.
#
#   <hub>-uat     IAP admits it to this hub's backend service only. The
#                 impersonator mints ID tokens for it to pass IAP.
#   <hub>-issuer  No IAP access. The impersonator mints ID tokens for it, for
#                 the test hub to trust as a test-identity issuer (hub-side
#                 trust is configured elsewhere).
#
# Neither account has any project-level role. All IAM is additive
# *_iam_member, never *_iam_binding or *_iam_policy.
#
# Every IAM member carries a precondition: the project must record DATA_READ
# audit logs for iamcredentials.googleapis.com (or allServices) and exempt no
# impersonator from them, so every token minted for these accounts is
# logged. See the README for why this only holds at provision time.

locals {
  # Bare emails of the impersonators, for the alert filter.
  impersonator_emails = sort([for m in toset(var.impersonators) : trimprefix(m, "serviceAccount:")])

  # The project's IAM policy as decoded JSON. try(): anything unreadable
  # counts as "no audit config", so the precondition fails closed.
  project_policy = try(jsondecode(data.google_project_iam_policy.this.policy_data), {})

  # Audit configs combine across services: GCP unions their log types and
  # their exempted members. So the two conditions are checked separately,
  # over the iamcredentials.googleapis.com and allServices configs together:
  #   (a) DATA_READ is enabled in at least one of them, and
  #   (b) no impersonator is exempted by any DATA_READ log config in either
  #       (an exempted caller's GenerateIdToken calls are not logged).
  # Members are compared lowercased on both sides. Only literal members are
  # compared: an exemption through a group: that contains an impersonator is
  # not detected.
  token_audit_configs = [
    for ac in try(local.project_policy.auditConfigs, []) : ac
    if contains(["iamcredentials.googleapis.com", "allServices"], try(ac.service, ""))
  ]

  token_data_read_log_configs = flatten([
    for ac in local.token_audit_configs : [
      for lc in try(ac.auditLogConfigs, []) : lc if try(lc.logType, "") == "DATA_READ"
    ]
  ])

  token_data_read_exempted = toset(flatten([
    for lc in local.token_data_read_log_configs : [for m in try(lc.exemptedMembers, []) : lower(m)]
  ]))

  data_read_audited = try(
    length(local.token_data_read_log_configs) > 0 &&
    length(setintersection(local.token_data_read_exempted, toset([for m in var.impersonators : lower(m)]))) == 0,
    false
  )

  audit_error = "project ${var.project_id} does not record DATA_READ audit logs for iamcredentials.googleapis.com (or allServices), or exempts an impersonator from them in either config, so ID tokens minted for the test-runner accounts would go unlogged. Set iamcredentials_data_access_logs = true in configurations/shared-infra and apply it first (or add an equivalent allServices DATA_READ audit config), then re-run this plan. See modules/hub-test-runner/README.md."
}

# Read at plan time: only var.project_id feeds it.
data "google_project_iam_policy" "this" {
  project = var.project_id
}

# --- Accounts ---
#
# No project roles, on purpose: these accounts exist only to be impersonated
# and, for -uat, to pass this hub's IAP.

resource "google_service_account" "uat" {
  project      = var.project_id
  account_id   = "${var.hub_name}-uat"
  display_name = "Scion test hub (${var.hub_name}): test-runner IAP identity"
  description  = "Impersonated by a dedicated test-runner service account to reach this hub through IAP. No project roles."
}

resource "google_service_account" "issuer" {
  project      = var.project_id
  account_id   = "${var.hub_name}-issuer"
  display_name = "Scion test hub (${var.hub_name}): test-identity issuer"
  description  = "Impersonated by a dedicated test-runner service account to mint test-identity tokens. No project roles and no IAP access."
}

# --- Token minting ---
#
# Scoped to each account, never project-wide
# roles/iam.serviceAccountOpenIdTokenCreator, which would let the
# impersonators mint ID tokens for every service account in the project,
# including other hubs' identities. OpenIdTokenCreator only: no access-token
# minting (serviceAccountTokenCreator) and no key creation.

resource "google_service_account_iam_member" "uat_token_creator" {
  for_each = toset(var.impersonators)

  service_account_id = google_service_account.uat.name
  role               = "roles/iam.serviceAccountOpenIdTokenCreator"
  member             = each.value

  lifecycle {
    precondition {
      condition     = local.data_read_audited
      error_message = local.audit_error
    }
  }
}

resource "google_service_account_iam_member" "issuer_token_creator" {
  for_each = toset(var.impersonators)

  service_account_id = google_service_account.issuer.name
  role               = "roles/iam.serviceAccountOpenIdTokenCreator"
  member             = each.value

  lifecycle {
    precondition {
      condition     = local.data_read_audited
      error_message = local.audit_error
    }
  }
}

# --- IAP access ---
#
# Scoped to this hub's backend service only, never project-wide
# roles/iap.httpsResourceAccessor, which would reach every IAP-protected
# resource in the project, including other hubs. The -uat account only: the
# -issuer account has no IAP access.
resource "google_iap_web_backend_service_iam_member" "uat" {
  project             = var.project_id
  web_backend_service = var.backend_service_name
  role                = "roles/iap.httpsResourceAccessor"
  member              = "serviceAccount:${google_service_account.uat.email}"

  lifecycle {
    precondition {
      condition     = local.data_read_audited
      error_message = local.audit_error
    }
  }
}

# --- Optional alert: tokens minted by anyone else ---
#
# Counts GenerateIdToken calls on either account whose caller is not one of
# the impersonators. Only created when alert_notification_channels is set.
#
# Filter:
#
#   logName="projects/<project>/logs/cloudaudit.googleapis.com%2Fdata_access"
#   protoPayload.serviceName="iamcredentials.googleapis.com"
#   protoPayload.methodName:"GenerateIdToken"
#   resource.type="service_account"
#   resource.labels.email_id=("<uat email>" OR "<issuer email>")
#   NOT protoPayload.authenticationInfo.principalEmail=("<impersonator 1>" OR ...)
#
# methodName uses ":" (substring) so it matches whether the entry carries
# "GenerateIdToken" or a fully qualified method name. The target account is
# matched on the log entry's monitored resource (service_account, labelled
# with the account's email). Denied calls are logged too, so attempts by
# other principals also count.
#
# The mock-provider tests cannot check this against GCP. The first live
# enablement must confirm, against one real GenerateIdToken entry, that the
# call is logged under DATA_READ (the data_access log) and that this filter
# matches it.
locals {
  foreign_token_mint_filter = join("\n", [
    "logName=\"projects/${var.project_id}/logs/cloudaudit.googleapis.com%2Fdata_access\"",
    "protoPayload.serviceName=\"iamcredentials.googleapis.com\"",
    "protoPayload.methodName:\"GenerateIdToken\"",
    "resource.type=\"service_account\"",
    "resource.labels.email_id=(\"${google_service_account.uat.email}\" OR \"${google_service_account.issuer.email}\")",
    "NOT protoPayload.authenticationInfo.principalEmail=(${join(" OR ", [for e in local.impersonator_emails : "\"${e}\""])})",
  ])

  alerts_enabled = length(var.alert_notification_channels) > 0
}

resource "google_logging_metric" "foreign_token_mint" {
  count = local.alerts_enabled ? 1 : 0

  project     = var.project_id
  name        = "${var.hub_name}-test-runner-foreign-token-mint"
  description = "GenerateIdToken calls on ${var.hub_name}'s test-runner accounts by a principal outside the configured impersonators."
  filter      = local.foreign_token_mint_filter

  metric_descriptor {
    metric_kind = "DELTA"
    value_type  = "INT64"
    unit        = "1"
  }
}

# Fires on any matching call. The service_account log resource is not
# expected to have a Monitoring resource type of its own, so the metric's
# points are expected on resource.type="global". Confirm that on the metric
# descriptor after the first data point (part of the first live enablement
# above).
resource "google_monitoring_alert_policy" "foreign_token_mint" {
  count = local.alerts_enabled ? 1 : 0

  project               = var.project_id
  display_name          = "${var.hub_name}: test-runner token minted by an unexpected principal"
  combiner              = "OR"
  notification_channels = var.alert_notification_channels

  conditions {
    display_name = "GenerateIdToken by a non-impersonator"

    condition_threshold {
      filter          = "metric.type=\"logging.googleapis.com/user/${one(google_logging_metric.foreign_token_mint[*].name)}\" AND resource.type=\"global\""
      comparison      = "COMPARISON_GT"
      threshold_value = 0
      duration        = "0s"

      aggregations {
        alignment_period   = "60s"
        per_series_aligner = "ALIGN_SUM"
      }
    }
  }

  alert_strategy {
    auto_close = "1800s"
  }

  documentation {
    mime_type = "text/markdown"
    content   = "A principal outside the configured impersonators called GenerateIdToken on ${var.hub_name}'s test-runner accounts (${google_service_account.uat.email}, ${google_service_account.issuer.email}). Check the iamcredentials data-access logs for the caller."
  }
}
