output "repo_url" {
  description = "Repository URL, e.g. \"<region>-docker.pkg.dev/<project>/<name_prefix>-scion\"."
  value       = "${google_artifact_registry_repository.this.location}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.this.repository_id}"
}
