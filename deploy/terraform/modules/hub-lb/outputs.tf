# The front-door contract is public_url + iap_audience. hub-gke consumes
# exactly those two and nothing else, so a different front (for example a
# Cloud Run IAP front) can replace this module by producing the same pair.

output "public_url" {
  description = "Front-door contract. The hub's external URL, https://<hostname>."
  value       = "https://${var.hostname}"
}

output "iap_audience" {
  description = "Front-door contract. The audience IAP signs its JWT assertion for: /projects/<project number>/global/backendServices/<numeric backend service id>. Known after the backend service is created."
  value       = "/projects/${var.project_number}/global/backendServices/${google_compute_backend_service.this.generated_id}"
}

output "lb_ip" {
  description = "The load balancer's global IPv4 address."
  value       = google_compute_global_address.this.address
}

output "dns_record" {
  description = "The DNS record a human must create (DNS is external to this Terraform). The managed certificate stays PROVISIONING until it resolves."
  value = {
    name  = var.hostname
    type  = "A"
    value = google_compute_global_address.this.address
  }
}

output "backend_service" {
  description = "Backend service facts for operators and tests: its name, timeout and health-check path. Not part of the front-door contract; hub-gke must not consume it."
  value = {
    name              = google_compute_backend_service.this.name
    timeout_sec       = google_compute_backend_service.this.timeout_sec
    health_check_path = google_compute_health_check.this.http_health_check[0].request_path
    health_check_port = google_compute_health_check.this.http_health_check[0].port
  }
}

output "neg_zones_read" {
  description = "Zones a NEG is read from and attached as a backend, one per data source. For operators and tests; must equal the zones the NEG Service pre-provisions."
  value       = sort(keys(data.google_compute_network_endpoint_group.this))
}

output "firewall_destination_ranges" {
  description = "Destination ranges of the health-check firewall rule (the cluster's pod range). For operators and tests."
  value       = google_compute_firewall.health_check.destination_ranges
}
