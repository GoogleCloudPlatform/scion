# availability_type REGIONAL by default is not reachable from
# configurations/hub's tests (fresh_hub_plan.tftest.hcl) — the hub root
# never touches cloudsql-instance directly, and shared-lookup's shared.sql
# contract exposes only instance_name/connection_name, not
# availability_type. That coverage lives here instead, against
# configurations/shared-infra directly.
#
# mock_provider replaces google and google-beta (the only two providers this
# root and its module tree use — see versions.tf) with a schema-driven fake,
# so this runs fully offline: no GCP project, no credentials, no registry
# access.

mock_provider "google" {}
mock_provider "google-beta" {}

variables {
  project_id = "tfha-test-project"
  region     = "us-central1"
  zone       = "us-central1-a"
}

# The mock provider defaults list(object) attributes it can't safely
# synthesize (nested repeated blocks) to an empty list, which breaks
# gke-autopilot's own [0]-indexing of "a real cluster always has at least
# one master_auth block" (true for any real instance, but not something the
# mock can assume) — same finding as fresh_hub_plan.tftest.hcl's override on
# shared-lookup's data.google_container_cluster, but here it's the managed
# resource itself (this root creates the cluster, not a lookup).
override_resource {
  target = module.gke_autopilot.google_container_cluster.this
  values = {
    master_auth = [
      {
        cluster_ca_certificate = "bW9jay1jYS1jZXJ0"
        client_certificate     = ""
        client_key             = ""
      }
    ]
  }
}

run "fresh_shared_infra_plans_clean" {
  command = plan

  # The default itself: nothing in this run's variables block overrides
  # sql_availability_type, so this is the value a fresh apply would actually
  # use.
  assert {
    condition     = var.sql_availability_type == "REGIONAL"
    error_message = "sql_availability_type must default to REGIONAL for HA."
  }

  # The value actually reaches the Cloud SQL instance resource, not just the
  # root variable — sql_availability_type output surfaces
  # module.cloudsql_instance.availability_type, which reads
  # google_sql_database_instance.this.settings[0].availability_type
  # directly.
  assert {
    condition     = output.sql_availability_type == "REGIONAL"
    error_message = "the Cloud SQL instance's settings.availability_type must be REGIONAL by default — if this disagrees with var.sql_availability_type's default, the value stopped flowing through module.cloudsql_instance."
  }
}
