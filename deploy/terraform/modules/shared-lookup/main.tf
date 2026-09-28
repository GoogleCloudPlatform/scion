# The hub/shared layer contract. This module resolves shared infra purely by
# naming convention through data sources — never through
# terraform_remote_state — so the hub root works against any conforming
# infra, including hand-built infra, and fails loudly (not silently) if a
# resource is missing. No adoption: this module only reads.

data "google_project" "this" {
  project_id = var.project_id
}

data "google_compute_network" "this" {
  project = var.project_id
  name    = "${var.shared_prefix}-vpc"
}

data "google_compute_subnetwork" "this" {
  project = var.project_id
  region  = var.region
  name    = "${var.shared_prefix}-subnet"
}

data "google_sql_database_instance" "this" {
  project = var.project_id
  name    = "${var.shared_prefix}-pg"
}

data "google_filestore_instance" "this" {
  project  = var.project_id
  location = var.zone
  name     = "${var.shared_prefix}-nfs"
}

data "google_container_cluster" "this" {
  project  = var.project_id
  location = var.region
  name     = "${var.shared_prefix}-agents"
}

# Without a top-level image_registry in settings.yaml, the hub never rewrites
# bare harness images (scion-claude:latest etc.) to the shared Artifact
# Registry repo, so GKE tries to pull them from Docker Hub where they don't
# exist, and agent pods fail to start with ImagePullBackOff.
data "google_artifact_registry_repository" "scion" {
  project       = var.project_id
  location      = var.region
  repository_id = "${var.shared_prefix}-scion"
}
