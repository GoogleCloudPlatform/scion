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
# audit logs for iamcredentials.googleapis.com (or allServices), so every
# token minted for these accounts is logged. See the README for why this only
# holds at provision time.

locals {
  # Bare emails of the impersonators, for the alert filter.
  impersonator_emails = sort([for m in var.impersonators : trimprefix(m, "serviceAccount:")])

  # The project's IAM policy as decoded JSON. try(): anything unreadable
  # counts as "no audit config", so the precondition fails closed.
  project_policy = try(jsondecode(data.google_project_iam_policy.this.policy_data), {})

  # Audit configs that make token minting visible: iamcredentials or
  # allServices, with a DATA_READ log config that exempts none of the
  # impersonators (an exempted caller's GenerateIdToken calls are not logged).
  data_read_audited = anytrue([
    for ac in try(local.project_policy.auditConfigs, []) : (
      contains(["iamcredentials.googleapis.com", "allServices"], try(ac.service, "")) &&
      anytrue([
        for lc in try(ac.auditLogConfigs, []) : (
          try(lc.logType, "") == "DATA_READ" &&
          length(setintersection(toset(try(lc.exemptedMembers, [])), toset(var.impersonators))) == 0
        )
      ])
    )
  ])

  audit_error = "project ${var.project_id} does not record DATA_READ audit logs for iamcredentials.googleapis.com (or allServices) without exempting an impersonator, so ID tokens minted for the test-runner accounts would go unlogged. Set iamcredentials_data_access_logs = true in configurations/shared-infra and apply it first (or add an equivalent allServices DATA_READ audit config), then re-run this plan. See modules/hub-test-runner/README.md."
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
# Exact filter (NEEDS CONFIRMATION against a real GenerateIdToken
# data-access log entry at review; methodName and the resource label in
# particular):
#
#   logName="projects/<project>/logs/cloudaudit.googleapis.com%2Fdata_access"
#   protoPayload.serviceName="iamcredentials.googleapis.com"
#   protoPayload.methodName:"GenerateIdToken"
#   (protoPayload.resourceName:"<uat email>" OR protoPayload.resourceName:"<issuer email>")
#   NOT protoPayload.authenticationInfo.principalEmail=("<impersonator 1>" OR ...)
#
# methodName uses ":" (substring) so it matches whether the entry carries
# "GenerateIdToken" or a fully qualified method name. resourceName is
# projects/-/serviceAccounts/<email>. Denied calls are logged too, so
# attempts by other principals also count.
locals {
  foreign_token_mint_filter = join("\n", [
    "logName=\"projects/${var.project_id}/logs/cloudaudit.googleapis.com%2Fdata_access\"",
    "protoPayload.serviceName=\"iamcredentials.googleapis.com\"",
    "protoPayload.methodName:\"GenerateIdToken\"",
    "(protoPayload.resourceName:\"${google_service_account.uat.email}\" OR protoPayload.resourceName:\"${google_service_account.issuer.email}\")",
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

# Fires on any matching call. The Monitoring filter names only the metric
# type (no resource.type), so it matches whatever monitored resource the log
# entries map to; confirm at review alongside the log filter above.
resource "google_monitoring_alert_policy" "foreign_token_mint" {
  count = local.alerts_enabled ? 1 : 0

  project               = var.project_id
  display_name          = "${var.hub_name}: test-runner token minted by an unexpected principal"
  combiner              = "OR"
  notification_channels = var.alert_notification_channels

  conditions {
    display_name = "GenerateIdToken by a non-impersonator"

    condition_threshold {
      filter          = "metric.type=\"logging.googleapis.com/user/${one(google_logging_metric.foreign_token_mint[*].name)}\""
      comparison      = "COMPARISON_GT"
      threshold_value = 0
      duration        = "0s"

      aggregations {
        alignment_period   = "60s"
        per_series_aligner = "ALIGN_SUM"
      }
    }
  }

  documentation {
    mime_type = "text/markdown"
    content   = "A principal outside the configured impersonators called GenerateIdToken on ${var.hub_name}'s test-runner accounts (${google_service_account.uat.email}, ${google_service_account.issuer.email}). Check the iamcredentials data-access logs for the caller."
  }
}
