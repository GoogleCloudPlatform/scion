# Plan-mode coverage for configurations/hub-gke: the whole root composes and
# plans for a brand-new GKE hub on (mocked) shared infra, fully offline.
# mock_provider replaces google, kubernetes, helm and random with
# schema-driven fakes that never call an API.
#
# What this does NOT prove: that GCP accepts the LB/IAP resources, that the
# NEG controller creates the NEGs before the apply reads them (a mocked data
# source always "exists"), that the chart renders with these values (the
# chart's own helm-template checks, plus a manual render recorded in the
# change, cover that), or that the hub boots. Those are the operator's
# live checks.

mock_provider "google" {}
mock_provider "kubernetes" {}
mock_provider "helm" {}
mock_provider "random" {}

variables {
  project_id        = "tfha-test-project"
  region            = "us-central1"
  zone              = "us-central1-a"
  shared_prefix     = "tfha"
  shared_share_name = "scion"

  hub_name     = "tfha-gke-h3"
  state_prefix = "tfha/hubs/tfha-gke-h3"
  hostname     = "tfha-gke-h3.example.com"

  # Obviously fake: the variable has no default, and no registry has this digest.
  hub_image_digest = "sha256:feedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedface"

  iap_oauth_client_id = "123456789-abc.apps.googleusercontent.com"
  iap_members         = ["group:team@example.com"]
  admin_emails        = ["admin@example.com"]
}

# Same pins as configurations/hub's test: values that feed validations or
# asserted strings, and repeated blocks the mock cannot synthesize.
# name/email too: the targeted apply run below applies against the mock,
# and the provider validates service_account_id's format even then.
override_resource {
  target = module.hub_identity.google_service_account.hub
  values = {
    unique_id = "115656325337183068810"
    name      = "projects/tfha-test-project/serviceAccounts/tfha-gke-h3-hub@tfha-test-project.iam.gserviceaccount.com"
    email     = "tfha-gke-h3-hub@tfha-test-project.iam.gserviceaccount.com"
  }
}

override_resource {
  target = module.hub_identity.google_service_account.transport
  values = {
    name  = "projects/tfha-test-project/serviceAccounts/tfha-gke-h3-transport@tfha-test-project.iam.gserviceaccount.com"
    email = "tfha-gke-h3-transport@tfha-test-project.iam.gserviceaccount.com"
  }
}

override_resource {
  target = module.hub_identity.google_service_account.agent
  values = {
    name  = "projects/tfha-test-project/serviceAccounts/tfha-gke-h3-agent@tfha-test-project.iam.gserviceaccount.com"
    email = "tfha-gke-h3-agent@tfha-test-project.iam.gserviceaccount.com"
  }
}

override_resource {
  target = module.hub_lb.google_compute_backend_service.this
  values = {
    generated_id = 4242424242
  }
}

override_data {
  target = module.shared_lookup.data.google_project.this
  values = {
    number = "123456789012"
  }
}

override_data {
  target = module.shared_lookup.data.google_artifact_registry_repository.scion
  values = {
    registry_uri = "us-central1-docker.pkg.dev/tfha-test-project/tfha-scion"
  }
}

override_data {
  target = module.shared_lookup.data.google_sql_database_instance.this
  values = {
    name            = "tfha-pg"
    connection_name = "tfha-test-project:us-central1:tfha-pg"
  }
}

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
    name     = "tfha-agents"
    location = "us-central1"
    master_auth = [
      {
        cluster_ca_certificate = "bW9jay1jYS1jZXJ0"
        client_certificate     = ""
        client_key             = ""
      }
    ]
  }
}

override_data {
  target = data.google_container_cluster.agents
  values = {
    name           = "tfha-agents"
    node_locations = ["us-central1-c", "us-central1-a", "us-central1-b"]
    # Exactly the minimum for the NEG annotation's "zones" field.
    master_version = "1.36.2-gke.3104000"
    ip_allocation_policy = [
      {
        cluster_ipv4_cidr_block = "10.64.0.0/14"
      }
    ]
  }
}

run "fresh_gke_hub_plans_clean" {
  command = plan

  # Front-door contract outputs.
  assert {
    condition     = output.public_url == "https://tfha-gke-h3.example.com"
    error_message = "public_url must be https://<hostname>."
  }

  assert {
    condition     = output.chart_values.hub.baseUrl == output.public_url
    error_message = "the chart's hub.baseUrl must be the front's public_url."
  }

  assert {
    condition     = output.dns_record.name == "tfha-gke-h3.example.com" && output.dns_record.type == "A"
    error_message = "dns_record must be an A record for the hostname."
  }

  # The LB's load-bearing settings.
  assert {
    condition     = output.backend_service.timeout_sec == 86400
    error_message = "backend timeout_sec must be 86400."
  }

  assert {
    condition     = output.backend_service.health_check_path == "/readyz" && output.backend_service.health_check_port == 8080
    error_message = "health check must be HTTP /readyz on the pod port."
  }

  assert {
    condition     = output.neg_zones == tolist(["us-central1-a", "us-central1-b", "us-central1-c"])
    error_message = "NEG zones must come from the cluster's node_locations."
  }

  # The zones the front door reads are exactly the zones the NEG Service
  # asks the controller to pre-provision.
  assert {
    condition     = tolist(jsondecode(module.hub_gke.neg_annotation).zones) == output.neg_zones
    error_message = "the NEG annotation's zones must be the zones hub-lb reads NEGs from."
  }

  assert {
    condition     = module.hub_lb.neg_zones_read == output.neg_zones
    error_message = "hub-lb must read one NEG per neg_zones zone."
  }

  # O1: the health-check firewall reaches only the cluster's pod range.
  assert {
    condition     = module.hub_lb.firewall_destination_ranges == toset(["10.64.0.0/14"])
    error_message = "the health-check firewall's destination must be the cluster's pod range."
  }

  # The hub release.
  assert {
    condition     = output.hub_installed
    error_message = "with iap_oauth_client_id set the hub must be installed."
  }

  assert {
    condition     = output.hub_image == "us-central1-docker.pkg.dev/tfha-test-project/tfha-scion/scion-hub-gke@sha256:feedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedface"
    error_message = "hub image must be <AR repo>/scion-hub-gke pinned by hub_image_digest."
  }

  assert {
    condition     = !contains(keys(output.chart_values.image), "tag")
    error_message = "the hub image must carry no tag."
  }

  assert {
    condition     = output.chart_values.database.auth == "password" && output.chart_values.database.driver == "postgres"
    error_message = "database must be postgres with auth=password."
  }

  assert {
    condition = (
      output.chart_values.database.name == "tfha_gke_h3" &&
      output.chart_values.database.user == "tfha-gke-h3" &&
      output.chart_values.cloudsql.instanceConnectionName == "tfha-test-project:us-central1:tfha-pg"
    )
    error_message = "database name/user and the Cloud SQL connection must come from cloudsql-database and shared-lookup."
  }

  assert {
    condition = (
      output.chart_values.serviceAccount.name == "scion-hub" &&
      output.chart_values.runtime.namespace == "tfha-gke-h3" &&
      output.hub_namespace == "tfha-gke-h3-system" &&
      output.chart_values.agents.imageRegistry == "us-central1-docker.pkg.dev/tfha-test-project/tfha-scion"
    )
    error_message = "KSA, namespaces and agent image registry are not as designed."
  }

  # RBAC is owned by the chart, not agent-runtime-k8s.
  assert {
    condition     = output.hub_rbac_created_by_terraform == false
    error_message = "create_hub_rbac must be false for hub-gke: the chart owns the hub's RBAC."
  }
}

# iap_audience depends on the backend service's generated_id, unknown in
# plan mode under Terraform 1.9. This run applies against the MOCK
# providers only (no API calls), targeted at the Helm release, which pulls
# in the whole chain the B1 ordering relies on: NEG Service -> NEG reads ->
# backend service -> release. The target leaves out the global address,
# whose prevent_destroy would otherwise fail the test's own teardown.
run "front_contract_reaches_the_chart" {
  command = apply

  plan_options {
    target = [module.hub_gke.helm_release.hub]
  }

  assert {
    condition     = output.iap_audience == "/projects/123456789012/global/backendServices/4242424242"
    error_message = "iap_audience must be /projects/<number>/global/backendServices/<numeric id>."
  }

  assert {
    condition     = output.chart_values.auth.proxy.iap.audience == output.iap_audience
    error_message = "the chart's auth.proxy.iap.audience must be the front's iap_audience."
  }
}

run "no_client_id_skips_only_the_hub" {
  command = plan

  variables {
    iap_oauth_client_id = null
  }

  # The copied check warns on an unset client ID.
  expect_failures = [check.transport_audience_configured]

  assert {
    condition     = output.hub_installed == false
    error_message = "with no client ID, hub_installed must be false (helm_release skipped)."
  }

  # The LB, IAP and NEG path are still built on the first apply.
  assert {
    condition     = output.public_url == "https://tfha-gke-h3.example.com" && output.backend_service.timeout_sec == 86400
    error_message = "the front door must still be planned without a client ID."
  }
}

run "hub_name_must_match_state_prefix" {
  command = plan

  variables {
    state_prefix = "tfha/hubs/tfha-other"
  }

  expect_failures = [var.hub_name]
}

# Older than 1.36.2-gke.3104000 in the last field only. A string comparison
# would pass "1.36.2-gke.999999" (it sorts after "...3104000"); the numeric
# compare must not.
run "old_gke_build_fails_plan" {
  command = plan

  override_data {
    target = data.google_container_cluster.agents
    values = {
      name           = "tfha-agents"
      node_locations = ["us-central1-a"]
      master_version = "1.36.2-gke.999999"
      ip_allocation_policy = [
        {
          cluster_ipv4_cidr_block = "10.64.0.0/14"
        }
      ]
    }
  }

  expect_failures = [data.google_container_cluster.agents]
}

# Minor 9 < 36 numerically, though "9" sorts after "3" as a string.
run "old_gke_minor_fails_plan" {
  command = plan

  override_data {
    target = data.google_container_cluster.agents
    values = {
      name           = "tfha-agents"
      node_locations = ["us-central1-a"]
      master_version = "1.9.10-gke.9999999"
      ip_allocation_policy = [
        {
          cluster_ipv4_cidr_block = "10.64.0.0/14"
        }
      ]
    }
  }

  expect_failures = [data.google_container_cluster.agents]
}

run "unparseable_gke_version_fails_plan" {
  command = plan

  override_data {
    target = data.google_container_cluster.agents
    values = {
      name           = "tfha-agents"
      node_locations = ["us-central1-a"]
      master_version = "latest"
      ip_allocation_policy = [
        {
          cluster_ipv4_cidr_block = "10.64.0.0/14"
        }
      ]
    }
  }

  expect_failures = [data.google_container_cluster.agents]
}

# Newer in an earlier field with a smaller later field: 1.37.0-gke.1 passes.
run "newer_gke_minor_passes" {
  command = plan

  override_data {
    target = data.google_container_cluster.agents
    values = {
      name           = "tfha-agents"
      node_locations = ["us-central1-a"]
      master_version = "1.37.0-gke.1"
      ip_allocation_policy = [
        {
          cluster_ipv4_cidr_block = "10.64.0.0/14"
        }
      ]
    }
  }

  assert {
    condition     = output.neg_zones == tolist(["us-central1-a"])
    error_message = "a newer control plane must plan cleanly."
  }
}
