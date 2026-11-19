# Plan-mode coverage for optional Conduit Relay Cloud SQL user (enable_relay_user).
mock_provider "google" {}
mock_provider "random" {}

variables {
  project_id          = "tfha-test-project"
  instance_name       = "tfha-pg"
  hub_name            = "tfha-gke-h3"
  hub_sa_email        = "tfha-gke-h3-hub@tfha-test-project.iam.gserviceaccount.com"
  sql_connection_name = "tfha-test-project:us-central1:tfha-pg"
}

run "default_creates_no_relay_db_user" {
  command = plan

  assert {
    condition     = length(google_sql_user.relay) == 0 && length(random_password.relay_db) == 0
    error_message = "enable_relay_user defaults to false and must create no relay SQL user or password."
  }

  assert {
    condition = (
      output.relay_db_user == null &&
      nonsensitive(output.relay_db_password) == null &&
      output.relay_password_secret_id == null &&
      output.relay_dsn_secret_id == null &&
      output.relay_dsn_secret_version == null
    )
    error_message = "all relay outputs must be null when enable_relay_user is false."
  }
}

run "enable_relay_user_provisions_user_and_secrets" {
  command = plan

  variables {
    enable_relay_user = true
  }

  assert {
    condition     = length(google_sql_user.relay) == 1 && google_sql_user.relay[0].name == "tfha-gke-h3-relay"
    error_message = "enable_relay_user = true must create google_sql_user.relay named <hub_name>-relay."
  }

  assert {
    condition = (
      output.relay_db_user == "tfha-gke-h3-relay" &&
      output.relay_password_secret_id == "tfha-gke-h3-relay-db-password" &&
      output.relay_dsn_secret_id == "tfha-gke-h3-relay-db-dsn"
    )
    error_message = "relay DB user and Secret Manager secret IDs must match <hub_name>-relay naming."
  }
}
