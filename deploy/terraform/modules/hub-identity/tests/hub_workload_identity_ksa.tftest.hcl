# The hub_workload_identity_ksa opt-in: off by default (every Cloud Run hub's
# plan is unchanged), and when set, exactly one grant on the hub GSA for
# exactly the named KSA.
mock_provider "google" {}

variables {
  project_id     = "tfha-test-project"
  project_number = "123456789012"
  hub_name       = "tfha-gke-h3"
}

run "default_creates_no_hub_workload_identity_grant" {
  command = plan

  assert {
    condition     = length(google_service_account_iam_member.hub_workload_identity_user) == 0
    error_message = "hub_workload_identity_ksa defaults to null and must then create no workloadIdentityUser grant on the hub GSA."
  }

  assert {
    condition     = output.hub_workload_identity_member == null
    error_message = "hub_workload_identity_member must be null when the opt-in is unset."
  }
}

run "opt_in_grants_exactly_one_ksa_on_the_hub_gsa" {
  command = plan

  variables {
    hub_workload_identity_ksa = {
      namespace = "tfha-gke-h3-system"
      name      = "scion-hub"
    }
  }

  assert {
    condition     = length(google_service_account_iam_member.hub_workload_identity_user) == 1
    error_message = "the opt-in must create exactly one workloadIdentityUser grant."
  }

  assert {
    condition     = google_service_account_iam_member.hub_workload_identity_user[0].role == "roles/iam.workloadIdentityUser"
    error_message = "the opt-in grant must be roles/iam.workloadIdentityUser."
  }

  assert {
    condition     = google_service_account_iam_member.hub_workload_identity_user[0].member == "serviceAccount:tfha-test-project.svc.id.goog[tfha-gke-h3-system/scion-hub]"
    error_message = "the opt-in member must be the named KSA's Workload Identity principal and nothing broader."
  }
}

run "malformed_ksa_is_rejected" {
  command = plan

  variables {
    hub_workload_identity_ksa = {
      namespace = "Bad_Namespace"
      name      = "scion-hub"
    }
  }

  expect_failures = [var.hub_workload_identity_ksa]
}

# The pre-existing guard stays: a project-level workloadIdentityUser for the
# agent SA is still refused.
run "project_level_workload_identity_user_still_refused" {
  command = plan

  variables {
    agent_sa_project_roles = ["roles/iam.workloadIdentityUser"]
  }

  expect_failures = [var.agent_sa_project_roles]
}

run "default_creates_no_relay_identity" {
  command = plan

  assert {
    condition = (
      length(google_service_account.relay) == 0 &&
      output.relay_sa_email == null &&
      length(output.relay_iam_grants) == 0 &&
      output.relay_workload_identity_member == null
    )
    error_message = "enable_relay_identity defaults to false and must create no relay GSA or IAM grants."
  }
}

run "enable_relay_identity_grants_least_privilege_only" {
  command = plan

  variables {
    enable_relay_identity = true
    relay_workload_identity_ksa = {
      namespace = "tfha-gke-h3-system"
      name      = "scion-relay"
    }
  }

  assert {
    condition     = length(google_service_account.relay) == 1 && google_service_account.relay[0].account_id == "tfha-gke-h3-relay"
    error_message = "enable_relay_identity must create google_service_account.relay with account_id <hub_name>-relay."
  }

  assert {
    condition     = length(output.relay_iam_grants) == 3
    error_message = "relay GSA must receive exactly 3 project roles (cloudsql.client, cloudsql.instanceUser, logging.logWriter)."
  }

  assert {
    condition     = google_service_account_iam_member.relay_workload_identity_user[0].member == "serviceAccount:tfha-test-project.svc.id.goog[tfha-gke-h3-system/scion-relay]"
    error_message = "relay Workload Identity binding must target the exact relay KSA."
  }
}

run "malformed_relay_ksa_is_rejected" {
  command = plan

  variables {
    enable_relay_identity = true
    relay_workload_identity_ksa = {
      namespace = "tfha-gke-h3-system"
      name      = "Bad_Relay_KSA"
    }
  }

  expect_failures = [var.relay_workload_identity_ksa]
}
