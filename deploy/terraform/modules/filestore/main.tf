# Shared Filestore Basic instance with one NFS share. Hubs get subdirectories
# under this share (created by each hub's nfs-init Job), not separate shares:
# Basic only supports one share per instance and has a 1 TiB minimum, so a
# share-per-hub would mean an instance-per-hub, defeating the point of
# sharing one Filestore instance across hubs to keep cost down.

resource "google_filestore_instance" "this" {
  project  = var.project_id
  name     = "${var.name_prefix}-nfs"
  location = var.zone
  tier     = var.tier

  deletion_protection_enabled = var.deletion_protection
  deletion_protection_reason  = var.deletion_protection ? "Protected by Terraform; see deploy/terraform/README.md's Destroy runbook to remove protection before destroying." : null

  file_shares {
    capacity_gb = var.capacity_gb
    name        = var.share_name
    # nfs_export_options left at provider default (no root squash): the
    # per-hub nfs-init Job needs root to mkdir/chown its subdirectory.
  }

  networks {
    network      = var.network_name
    modes        = ["MODE_IPV4"]
    connect_mode = "DIRECT_PEERING"
  }

  # Literal, not variable-driven: terraform destroy skips lifecycle
  # preconditions, so deletion_protection_enabled alone can't stop a direct
  # `terraform destroy -var deletion_protection=false` from removing this
  # once the API flag transition were to happen; this guards the operation
  # itself. A deliberate teardown of the whole shared stack needs a one-line
  # commit removing this lifecycle block, applied on its own short-lived
  # branch, never merged back.
  lifecycle {
    prevent_destroy = true
  }
}
