output "project_number" {
  description = "Numeric project number, used to build deterministic Cloud Run URLs and IAP service-agent identities."
  value       = data.google_project.this.number
}

output "enabled_services" {
  description = "The set of service APIs this module enabled."
  value       = [for s in google_project_service.this : s.service]
}
