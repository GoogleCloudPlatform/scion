# Shared Docker repo. Every hub pulls hub_image from here, each at its own
# tag; building and pushing images is a separate, out-of-band process, not
# something this module does.

resource "google_artifact_registry_repository" "this" {
  project       = var.project_id
  location      = var.region
  repository_id = "${var.name_prefix}-scion"
  format        = "DOCKER"
}
