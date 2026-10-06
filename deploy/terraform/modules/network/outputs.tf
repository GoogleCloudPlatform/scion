output "network_id" {
  description = "Self link / ID of the VPC network."
  value       = google_compute_network.this.id
}

output "network_name" {
  description = "Name of the VPC network."
  value       = google_compute_network.this.name
}

output "subnet_id" {
  description = "Self link / ID of the subnet."
  value       = google_compute_subnetwork.this.id
}

output "subnet_name" {
  description = "Name of the subnet."
  value       = google_compute_subnetwork.this.name
}

output "psa_connection" {
  description = "The google_service_networking_connection resource, used as a depends_on handle by cloudsql-instance, which requires the PSA peering to exist before it can attach a private IP. Filestore does not depend on this — see psa_prefix_length's description."
  value       = google_service_networking_connection.psa
}

output "nat_router_name" {
  description = "Cloud Router name backing Cloud NAT, exposed for debugging/observability. Nothing currently depends on this output."
  value       = google_compute_router.nat.name
}
