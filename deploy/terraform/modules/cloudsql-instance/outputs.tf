output "instance_name" {
  description = "Cloud SQL instance name (\"<name_prefix>-pg\")."
  value       = google_sql_database_instance.this.name
}

output "connection_name" {
  description = "Cloud SQL connection name, project:region:instance, used for the /cloudsql socket."
  value       = google_sql_database_instance.this.connection_name
}

output "private_ip" {
  description = "Private IP address of the instance."
  value       = google_sql_database_instance.this.private_ip_address
}

output "availability_type" {
  description = "The instance's actual settings.availability_type (ZONAL or REGIONAL), for shared-infra's tftest coverage of the REGIONAL default."
  value       = google_sql_database_instance.this.settings[0].availability_type
}

output "instance" {
  description = "The google_sql_database_instance resource, used as a depends_on handle by gke-autopilot, whose cluster is created after the services that attach to the VPC (see configurations/shared-infra/main.tf)."
  value       = google_sql_database_instance.this
}
