output "db_name" {
  description = "Database name."
  value       = google_sql_database.this.name
}

output "db_user" {
  description = "Database user name."
  value       = google_sql_user.this.name
}

output "db_password" {
  description = "Database user password (sensitive)."
  value       = random_password.db.result
  sensitive   = true
}

output "password_secret_id" {
  description = "Secret Manager secret ID holding the database password."
  value       = google_secret_manager_secret.db_password.secret_id
}

output "dsn_secret_id" {
  description = "Secret Manager secret ID holding the full DSN. Consumed by hub-cloudrun's SCION_SERVER_DATABASE_URL secret env var."
  value       = google_secret_manager_secret.db_dsn.secret_id
}

output "dsn_secret_version" {
  description = "The DSN secret's version NUMBER (not \"latest\") — pinned by hub-cloudrun's secret env var so a revision's connection string never silently changes without a new revision (same reasoning as the settings/kubeconfig secret volume version pins)."
  value       = google_secret_manager_secret_version.db_dsn.version
}

output "relay_db_user" {
  description = "Optional least-privilege Conduit Relay database user name (\"<hub_name>-relay\"), or null when enable_relay_user is false."
  value       = one(google_sql_user.relay[*].name)
}

output "relay_db_password" {
  description = "Optional Conduit Relay database user password (sensitive), or null when enable_relay_user is false."
  value       = one(random_password.relay_db[*].result)
  sensitive   = true
}

output "relay_password_secret_id" {
  description = "Secret Manager secret ID holding the Conduit Relay database password, or null when enable_relay_user is false."
  value       = one(google_secret_manager_secret.relay_db_password[*].secret_id)
}

output "relay_dsn_secret_id" {
  description = "Secret Manager secret ID holding the Conduit Relay full DSN, or null when enable_relay_user is false."
  value       = one(google_secret_manager_secret.relay_db_dsn[*].secret_id)
}

output "relay_dsn_secret_version" {
  description = "Pinned Secret Manager version for the Conduit Relay DSN, or null when enable_relay_user is false."
  value       = one(google_secret_manager_secret_version.relay_db_dsn[*].version)
}
