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
      server     = data.google_filestore_instance.this.networks[0].ip_addresses[0]
      share_path = "/${var.share_name}"
    }

    gke = {
      name           = data.google_container_cluster.this.name
      location       = data.google_container_cluster.this.location
      endpoint       = data.google_container_cluster.this.endpoint
      ca_certificate = data.google_container_cluster.this.master_auth[0].cluster_ca_certificate
    }

    artifact_registry = {
      # registry_uri is the real resource's actual pull URL
      # (<region>-docker.pkg.dev/<project>/<repo>), not a manually
      # reconstructed string.
      repo_url = data.google_artifact_registry_repository.scion.registry_uri
    }
  }
}
