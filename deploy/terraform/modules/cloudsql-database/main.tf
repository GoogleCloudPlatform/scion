# Per-hub database, user and password on the shared Cloud SQL instance
# (design §3.1, §3.4). No instance-level resources here.

resource "random_password" "db" {
  length  = 32
  special = false
}

resource "google_sql_database" "this" {
  project         = var.project_id
  instance        = var.instance_name
  name            = replace(var.hub_name, "-", "_")
  deletion_policy = var.deletion_policy
}

resource "google_sql_user" "this" {
  project  = var.project_id
  instance = var.instance_name
  name     = var.hub_name
  password = random_password.db.result
  type     = "BUILT_IN"
}

resource "google_secret_manager_secret" "db_password" {
  project   = var.project_id
  secret_id = "${var.hub_name}-db-password"

  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "db_password" {
  secret      = google_secret_manager_secret.db_password.id
  secret_data = random_password.db.result
}


# Redundant now that Alt-F has landed (design §6 OQ-11, ptone 09-28 00:03):
# the anticipated end state here turned out to be a dedicated <hub>-db-dsn
# secret holding the full DSN (below), not the hub SA reading this
# password-only secret directly — nothing reads db-password at runtime.
# Kept anyway per the brief (removing it is a delete, out of scope) — this
# grant is scoped to this hub's own secret either way, so it's harmless, not
# a live IAM-scope violation.
resource "google_secret_manager_secret_iam_member" "hub_reads_db_password" {
  secret_id = google_secret_manager_secret.db_password.secret_id
  project   = var.project_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${var.hub_sa_email}"
}

# --- DSN secret (Alt-F, design §6 OQ-11) ---
#
# The full Postgres DSN, built here (the module that owns the DB credential
# lifecycle) rather than embedded into hub-cloudrun's settings secret. Same
# format as the old settings.yaml.tftpl line: postgres://<user>:<urlencoded
# password>@/<db>?host=/cloudsql/<connection name>. Exposed to the Cloud Run
# hub container as a pinned secret env var (SCION_SERVER_DATABASE_URL,
# hub-cloudrun) instead of a plaintext line in a rendered settings file —
# after this, the settings secret holds no credential, so its old versions
# (kept, OQ-11 (a)) are harmless to retain, and traffic-shift rollback across
# a settings change works.
resource "google_secret_manager_secret" "db_dsn" {
  project   = var.project_id
  secret_id = "${var.hub_name}-db-dsn"

  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "db_dsn" {
  secret      = google_secret_manager_secret.db_dsn.id
  secret_data = "postgres://${google_sql_user.this.name}:${urlencode(random_password.db.result)}@/${google_sql_database.this.name}?host=/cloudsql/${var.sql_connection_name}"
}
