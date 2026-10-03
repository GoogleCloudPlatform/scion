output "server_ip" {
  description = "IP address of the Filestore instance's NFS server."
  value       = google_filestore_instance.this.networks[0].ip_addresses[0]
}

output "share_name" {
  description = "Name of the NFS share (the export path is \"/<share_name>\")."
  value       = google_filestore_instance.this.file_shares[0].name
}
