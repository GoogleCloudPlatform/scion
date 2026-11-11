output "name" {
  description = "Cluster name."
  value       = google_container_cluster.this.name
}

output "location" {
  description = "Cluster location (region, since this is a regional Autopilot cluster)."
  value       = google_container_cluster.this.location
}

output "endpoint" {
  description = "Cluster control-plane endpoint (IP), used to build the kubernetes provider host and hub kubeconfig."
  value       = google_container_cluster.this.endpoint
}

output "ca_certificate" {
  description = "Base64-encoded cluster CA certificate."
  # try(), not a bare [0] index: a real GKE cluster always
  # populates master_auth with exactly one element, so this never returns
  # null against real infra — but google_container_cluster.master_auth is a
  # purely Computed repeated block with no config-driven element count, and
  # Terraform's test-mocking framework (terraform test, both plan and apply)
  # cannot synthesize an element for a block like that: mock_resource/
  # override_resource "values" only fill in per-element attributes for
  # elements that already exist, and there is no config-driven count here to
  # seed one from (confirmed against the docs' DynamoDB replica example, and
  # reproduced directly here: neither override_resource nor mock_resource
  # defaults changed master_auth's length from empty under mock, with either
  # plan or apply). Every mocked run of this module therefore saw
  # master_auth as an empty list and errored ("Invalid index") on this
  # output specifically — unconditionally, since Terraform evaluates every
  # declared module output regardless of whether the caller uses it, which
  # made configurations/shared-infra untestable under mock providers at all,
  # not just for whatever a given test happens to assert on. try() makes
  # this output (and therefore this module) usable in mock-provider tests
  # without changing its value against real infra.
  value = try(google_container_cluster.this.master_auth[0].cluster_ca_certificate, null)
}
