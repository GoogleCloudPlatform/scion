# Per-hub database, user and password on the shared Cloud SQL instance.
# No instance-level resources here.

resource "random_password" "db" {
  length  = 32
  special = false

  # var.password_rotation defaults to "",
  # which keeps keepers null — a null-to-null comparison on every plan
  # against existing state, so the default is a no-op. Setting
  # password_rotation to any new value (e.g. a date) changes the keepers
  # map, which replaces this resource and cascades: sql_user password
  # update, then CBD-replaced db_password/db_dsn secret versions, then a
  # new hub revision (see the lifecycle blocks below and the module
  # README/runbook comment).
  keepers = var.password_rotation == "" ? null : {
    rotation = var.password_rotation
  }
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

  # secret_data changing always forces a
  # replace (Secret Manager versions are add-only), and without
  # create_before_destroy the default destroy-then-create order would
  # delete this version before the replacement exists. CBD makes the new
  # version exist first, so the running revision never references a
  # destroyed version mid-apply (same reasoning as hub-cloudrun's settings
  # secret lifecycle block, which pins a Cloud Run revision to a specific
  # secret version).
  lifecycle {
    create_before_destroy = true
  }
}


# Redundant now that the hub reads its DB credential via a full DSN secret
# instead (below): the anticipated end state here turned out to be a
# dedicated <hub>-db-dsn secret holding the full DSN, not the hub SA reading
# this password-only secret directly — nothing reads db-password at runtime.
# Kept anyway (removing it is a delete, out of scope) — this grant is scoped
# to this hub's own secret either way, so it's harmless, not a live
# IAM-scope violation.
resource "google_secret_manager_secret_iam_member" "hub_reads_db_password" {
  secret_id = google_secret_manager_secret.db_password.secret_id
  project   = var.project_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${var.hub_sa_email}"
}

# --- DSN secret ---
#
# The full Postgres DSN, built here (the module that owns the DB credential
# lifecycle) rather than embedded into hub-cloudrun's settings secret. Same
# format as the old settings.yaml.tftpl line: postgres://<user>:<urlencoded
# password>@/<db>?host=/cloudsql/<connection name>. Exposed to the Cloud Run
# hub container as a pinned secret env var (SCION_SERVER_DATABASE_URL,
# hub-cloudrun) instead of a plaintext line in a rendered settings file —
# after this, the settings secret holds no credential, so its old versions
# (kept intentionally) are harmless to retain, and traffic-shift rollback
# across a settings change works.
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

  # Same reasoning as db_password above —
  # CBD keeps this DSN version alive (and the running revision's pinned
  # secret env var valid) until the replacement version exists.
  lifecycle {
    create_before_destroy = true
  }
}

# --- Password rotation runbook ---
#
# To rotate this hub's DB password: set password_rotation to a new value
# (e.g. a date) in the hub's tfvars file — not with -var — wired from the
# hub root's db_password_rotation variable, and apply. Keep the marker in
# the tfvars file permanently; never remove it or reset it to "", either of
# which takes keepers back to null and triggers another, unplanned
# rotation on the next apply. Apply order per hub:
#   random_password.db replace
#     -> google_sql_user.this password update (in place)
#     -> google_secret_manager_secret_version.db_password/db_dsn CBD-replaced
#     -> hub-cloudrun's Cloud Run service picks up the new pinned dsn
#        secret version and rolls a new revision
#     -> the old secret versions are then destroyed
# Expected per hub: 3 add / 2 change / 3 destroy (random_password.db is
# state-only). Known window: from the sql_user password update until the
# new revision is Ready, the old revision's *new* DB connections fail,
# while its existing pooled connections survive.

# --- Optional separate Conduit Relay user (<hub_name>-relay) ---
#
# Least-privilege PostgreSQL grants required after the hub's first Ent
# schema migration creates the tables under owner "<hub_name>" (run once
# connected to database "<hub_name with - replaced by _>" as "<hub_name>" or
# postgres):
#
#   GRANT CONNECT ON DATABASE "<db_name>" TO "<hub_name>-relay";
#   GRANT USAGE ON SCHEMA public TO "<hub_name>-relay";
#   GRANT SELECT, INSERT, UPDATE, DELETE
#     ON TABLE relay_instances, conduit_sessions TO "<hub_name>-relay";
#   GRANT SELECT, INSERT, UPDATE
#     ON TABLE conduit_principal_epochs TO "<hub_name>-relay";
#   GRANT SELECT ON TABLE hub_settings TO "<hub_name>-relay";

resource "random_password" "relay_db" {
  count = var.enable_relay_user ? 1 : 0

  length  = 32
  special = false

  keepers = var.password_rotation == "" ? null : {
    rotation = var.password_rotation
  }
}

resource "google_sql_user" "relay" {
  count = var.enable_relay_user ? 1 : 0

  project  = var.project_id
  instance = var.instance_name
  name     = "${var.hub_name}-relay"
  password = random_password.relay_db[0].result
  type     = "BUILT_IN"
}

resource "google_secret_manager_secret" "relay_db_password" {
  count = var.enable_relay_user ? 1 : 0

  project   = var.project_id
  secret_id = "${var.hub_name}-relay-db-password"

  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "relay_db_password" {
  count = var.enable_relay_user ? 1 : 0

  secret      = google_secret_manager_secret.relay_db_password[0].id
  secret_data = random_password.relay_db[0].result

  lifecycle {
    create_before_destroy = true
  }
}

resource "google_secret_manager_secret" "relay_db_dsn" {
  count = var.enable_relay_user ? 1 : 0

  project   = var.project_id
  secret_id = "${var.hub_name}-relay-db-dsn"

  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "relay_db_dsn" {
  count = var.enable_relay_user ? 1 : 0

  secret      = google_secret_manager_secret.relay_db_dsn[0].id
  secret_data = "postgres://${google_sql_user.relay[0].name}:${urlencode(random_password.relay_db[0].result)}@/${google_sql_database.this.name}?host=/cloudsql/${var.sql_connection_name}"

  lifecycle {
    create_before_destroy = true
  }
}
