# Regression coverage: prove configurations/hub composes and plans end to
# end, fully offline, for a brand-new hub with NOTHING pre-existing — no
# shared infra, no prior state, no Secret Manager secret already holding a
# version. mock_provider replaces every provider this root and its module
# tree use (google, google-beta, kubernetes, tls, random, time) with a
# schema-driven fake that never calls a real API, so there is no GCP project,
# no credentials, and no registry access needed to run this file.
#
# This guards against a real defect: a fresh hub's first plan used to read
# `data.google_secret_manager_secret_version.db_password` for a secret that
# only gets a version once `cloudsql-database` has actually created it. That
# data source's read happens at plan time, so on any hub that has never been
# applied before, the real Secret Manager API 404s ("not found or has no
# versions") before the secret ever exists — no fresh hub could plan.
#
# What this test does NOT prove (say so, don't fake it): Terraform's mock
# providers do not reproduce that failure mode — a mocked data source always
# synthesizes a schema-shaped value and succeeds, regardless of whether the
# "real" resource it names would exist. Re-adding a plan-time data source
# read of a not-yet-created secret would still pass this test unchanged,
# because the mock has no concept of "not found". This test can only prove
# the fixed configuration composes and plans cleanly under a fresh, fully
# mocked environment; the actual guarantee that the hazard is gone is
# structural — hub-cloudrun/main.tf no longer contains
# data.google_secret_manager_secret_version.db_password at all (var.db_password
# is a plain sensitive module input now — see that variable's description).

mock_provider "google" {}
mock_provider "google-beta" {}
mock_provider "kubernetes" {}
mock_provider "tls" {}
mock_provider "random" {}
mock_provider "time" {}

variables {
  project_id        = "tfha-test-project"
  region            = "us-central1"
  zone              = "us-central1-a"
  shared_prefix     = "tfha"
  shared_share_name = "scion"

  # A genuinely NEW hub, on the existing (mocked) shared infra — a second
  # hub's very first plan, with nothing about it ever applied before.
  hub_name     = "tfha-h2"
  state_prefix = "tfha/hubs/tfha-h2"

  hub_image           = "us-central1-docker.pkg.dev/tfha-test-project/tfha-scion/scion-hub:test"
  iap_oauth_client_id = "123456789-abc.apps.googleusercontent.com"
}

# Pin the handful of mocked attributes that feed cross-variable validations
# or downstream string composition this test asserts on — without these,
# the mock provider's schema-shaped-but-arbitrary values would either fail
# hub_sa_unique_id's "^[0-9]+$" validation (main.tf's own regex on real
# unique_ids) or make the asserted outputs unpredictable.
override_resource {
  target = module.hub_identity.google_service_account.hub
  values = {
    unique_id = "115656325337183068810"
  }
}

override_data {
  target = module.shared_lookup.data.google_project.this
  values = {
    number = "123456789012"
  }
}

# The mock provider defaults list(object) attributes it can't safely
# synthesize (nested repeated blocks) to an empty list, which breaks these
# two modules' own [0]-indexing of "the shared Filestore/GKE cluster always
# has at least one network/master_auth block" (true for any real instance,
# but not something the mock can assume) — supply one element each so
# shared-lookup's output "shared" resolves the way it does against real
# infra.
override_data {
  target = module.shared_lookup.data.google_filestore_instance.this
  values = {
    networks = [
      {
        ip_addresses      = ["10.0.0.2"]
        network           = "tfha-vpc"
        modes             = ["MODE_IPV4"]
        reserved_ip_range = "10.0.0.0/29"
        connect_mode      = "DIRECT_PEERING"
      }
    ]
  }
}

override_data {
  target = module.shared_lookup.data.google_container_cluster.this
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

run "fresh_h2_plans_clean" {
  command = plan

  # The plan must succeed at all — this is the actual regression assertion:
  # a brand-new hub's plan must not error. Before the fix, the real-world
  # equivalent of this run 404'd inside modules/hub-cloudrun/main.tf's data
  # source; that specific failure mode can't be reproduced under mock (see
  # the file header), but a clean plan here at least proves nothing else in
  # the wiring change (db_password_secret_id -> db_password) broke the
  # module's type contract, defaults, or cross-module references.
  assert {
    condition     = output.namespace == "tfha-h2"
    error_message = "namespace should equal hub_name for a fresh hub."
  }

  assert {
    condition     = output.iap_audience == "/projects/123456789012/locations/us-central1/services/tfha-h2"
    error_message = "iap_audience should be deterministically computed from project_number/region/hub_name at plan time — if this is unknown or wrong, something upstream of hub-cloudrun stopped being plan-time-known."
  }

  # The artifacts bucket's noncurrent-version lifecycle rule exists and is
  # scoped to ARCHIVED (noncurrent) object versions only. bucket_lifecycle_rules
  # returns local.artifacts_lifecycle_rules — a plain list of plain objects
  # driven from the same config local as the actual lifecycle_rule blocks —
  # rather than reading back google_storage_bucket.artifacts.lifecycle_rule's
  # provider-normalized resource attribute, so condition/action are read
  # directly, without the tolist() the provider's set-of-object schema would
  # otherwise require, and without echoing provider-normalized defaults that
  # would make the second plan non-empty.
  assert {
    condition = anytrue([
      for r in output.bucket_lifecycle_rules :
      r.condition.with_state == "ARCHIVED" && r.action.type == "Delete"
    ])
    error_message = "expected a Delete lifecycle rule on the artifacts bucket scoped to with_state = ARCHIVED (noncurrent versions only) — a rule without that scope could delete live/CURRENT data."
  }
}

# The database DSN (including the password) is passed to the hub as a
# pinned Secret Manager secret env var (SCION_SERVER_DATABASE_URL), not
# embedded in the rendered settings.yaml, so a credential never has to sit
# in the settings file. Ideally this test would also assert the rendered
# settings contain no "postgres://" and the service has that secret env,
# but that can't be done here: a `run` block's `module.<name>` reference
# only exposes that module's declared *outputs* (service_uri, iap_audience,
# bucket_name — none of which carry this data), never its internal
# resources' attributes, even under `command = plan`. Tried directly against
# `module.hub_cloudrun.google_secret_manager_secret_version.settings
# .secret_data` and `module.hub_cloudrun.google_cloud_run_v2_service.hub
# .template[0].containers[0].env` — both fail with "Unsupported attribute".
# Adding new hub-cloudrun/hub-root outputs purely so this test file can read
# them is declined as a production surface change for comparatively little
# gain, since the two facts this would assert are already directly visible
# by reading the diff: settings.yaml.tftpl's server.database block has no
# `url:` key any more (see that file), and google_cloud_run_v2_service.hub
# declares the SCION_SERVER_DATABASE_URL env block explicitly
# (hub-cloudrun/main.tf). The actual end-to-end proof that this matters —
# the hub's real config loader takes database.url from this env var when the
# file has none — needs a Go-level test against the real loader, which mock
# Terraform providers can't reach anyway (see this file's header on what
# mock_provider can't reproduce).
