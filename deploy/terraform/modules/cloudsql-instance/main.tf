# Shared Cloud SQL for PostgreSQL 16 instance, private IP only. Per-hub
# databases, users and passwords are created by the cloudsql-database module,
# not here.

resource "google_sql_database_instance" "this" {
  project             = var.project_id
  name                = "${var.name_prefix}-pg"
  region              = var.region
  database_version    = "POSTGRES_16"
  deletion_protection = var.deletion_protection

  settings {
    tier              = var.tier
    edition           = var.edition
    availability_type = var.availability_type

    deletion_protection_enabled = var.deletion_protection

    ip_configuration {
      ipv4_enabled    = false
      private_network = var.network_id
    }

    database_flags {
      name  = "max_connections"
      value = tostring(var.max_connections)
    }

    backup_configuration {
      enabled                        = true
      point_in_time_recovery_enabled = true
      start_time                     = "03:00"

      backup_retention_settings {
        retained_backups = 7
        retention_unit   = "COUNT"
      }
    }
  }

  depends_on = [var.psa_connection]

  # Literal, not variable-driven (Terraform doesn't allow that): the
  # deletion_protection variable/attributes above guard the state
  # *transition*, but terraform destroy skips lifecycle preconditions
  # entirely (confirmed against the live API), so a direct
  # `terraform destroy -var deletion_protection=false` on the shared root
  # would otherwise partially succeed here. This guards the *operation*:
  # any plan that would destroy this resource errors before anything is
  # applied. Intentional teardown needs a one-line commit removing this on a
  # teardown branch that is never merged.
  lifecycle {
    prevent_destroy = true
  }
}
