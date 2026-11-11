# One typed object: the stable seam between the shared and hub layers. A
# future variation (dedicated, non-shared infra) builds the same shape from
# resources instead of data sources.
output "shared" {
  description = "Typed view of the shared infra, resolved by naming convention."
  value = {
    project_id     = var.project_id
    project_number = data.google_project.this.number
    region         = var.region

    network = {
      id          = data.google_compute_network.this.id
      name        = data.google_compute_network.this.name
      subnet_name = data.google_compute_subnetwork.this.name
    }

    sql = {
      instance_name   = data.google_sql_database_instance.this.name
      connection_name = data.google_sql_database_instance.this.connection_name
    }

    nfs = {
      # try(), not a bare [0][0] index: networks is a purely Computed
      # repeated block on the Filestore data source (no config-driven
      # element count), so Terraform's mock-provider test framework can't
      # synthesize an element for it without an explicit override_data —
      # same shape of problem as gke.ca_certificate below (see
      # gke-autopilot/outputs.tf for the full writeup). null, not "": a
      # blank server string would let downstream NFS volume blocks look
      # superficially valid and fail later with a confusing mount error;
      # null instead fails at the point of use with a clear "required
      # argument" error the first time this output is actually consumed
      # without the data being populated.
      server     = try(data.google_filestore_instance.this.networks[0].ip_addresses[0], null)
      share_path = "/${var.share_name}"
    }

    gke = {
      name     = data.google_container_cluster.this.name
      location = data.google_container_cluster.this.location
      endpoint = data.google_container_cluster.this.endpoint
      # try(), consistent with gke-autopilot/outputs.tf's ca_certificate
      # output: master_auth is a purely Computed repeated block the mock
      # provider can't populate on its own. null fallback (not ""), so a
      # missing value fails loudly at base64decode() rather than silently
      # producing an empty-but-valid-looking cluster_ca_certificate.
      ca_certificate = try(data.google_container_cluster.this.master_auth[0].cluster_ca_certificate, null)
    }

    artifact_registry = {
      # registry_uri is the real resource's actual pull URL
      # (<region>-docker.pkg.dev/<project>/<repo>), not a manually
      # reconstructed string.
      repo_url = data.google_artifact_registry_repository.scion.registry_uri
    }
  }
}
