# Shared GKE Autopilot cluster. One cluster serves every hub, each isolated
# by its own namespace and namespaced RBAC (see the agent-runtime-k8s
# module). Workload Identity is implicit on Autopilot.

resource "google_container_cluster" "this" {
  project  = var.project_id
  name     = "${var.name_prefix}-agents"
  location = var.region

  enable_autopilot = true

  network    = var.network_id
  subnetwork = var.subnet_id

  release_channel {
    channel = var.release_channel
  }

  private_cluster_config {
    enable_private_nodes = true
  }

  secret_manager_config {
    enabled = true
  }

  dynamic "master_authorized_networks_config" {
    for_each = length(var.master_authorized_networks) > 0 ? [1] : []
    content {
      dynamic "cidr_blocks" {
        for_each = var.master_authorized_networks
        content {
          cidr_block = cidr_blocks.value
        }
      }
    }
  }

  deletion_protection = var.deletion_protection

  # Literal, not variable-driven. GKE has no API-level deletion protection at
  # all (container v1's Cluster has no such field) — the deletion_protection
  # attribute above is Terraform-only, and terraform destroy skips lifecycle
  # preconditions, so this is the one resource in the shared stack that most
  # needs this guard: without it, nothing in Terraform stops a direct
  # `terraform destroy -var deletion_protection=false`. Real protection
  # against an out-of-band `gcloud container clusters delete` needs an IAM
  # deny / org policy outside Terraform entirely; that is an operator
  # decision and a residual risk documented in the README, not something
  # this module set can enforce. A deliberate teardown of the whole shared
  # stack needs a one-line commit removing this lifecycle block, applied on
  # its own short-lived branch, never merged back.
  lifecycle {
    prevent_destroy = true
  }
}
