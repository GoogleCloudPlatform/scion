# Plan-mode coverage for modules/hub-gke under mock providers: no cluster,
# no credentials. Proves which resources exist in each first-install state
# and that the values handed to the chart are the ones the design pins. It
# cannot prove the chart renders with them (the chart's own helm-template
# checks do that) or that the hub boots; those are vm-deploy's live checks.
mock_provider "google" {}
mock_provider "kubernetes" {}
mock_provider "helm" {}
mock_provider "random" {}

variables {
  project_id          = "tfha-test-project"
  region              = "us-central1"
  hub_name            = "tfha-gke-h3"
  public_url          = "https://tfha-gke-h3.test.scion-ai.dev"
  iap_audience        = "/projects/123456789012/global/backendServices/4242424242"
  image_repository    = "us-central1-docker.pkg.dev/tfha-test-project/tfha-scion/scion-hub-gke"
  image_digest        = "sha256:538ffc64d7e9cd15a24136bf1493c0154bc386aec0b700c2a5c805092d3c2f96"
  hub_sa_email        = "tfha-gke-h3-hub@tfha-test-project.iam.gserviceaccount.com"
  transport_sa_email  = "tfha-gke-h3-transport@tfha-test-project.iam.gserviceaccount.com"
  image_registry      = "us-central1-docker.pkg.dev/tfha-test-project/tfha-scion"
  runtime_namespace   = "tfha-gke-h3"
  db_name             = "tfha_gke_h3"
  db_user             = "tfha-gke-h3"
  db_password         = "not-a-real-password"
  sql_connection_name = "tfha-test-project:us-central1:tfha-pg"
  iap_oauth_client_id = "123456789-abc.apps.googleusercontent.com"
}

run "client_id_set_installs_the_hub" {
  command = plan

  assert {
    condition     = length(helm_release.hub) == 1 && output.hub_installed
    error_message = "with iap_oauth_client_id set, the Helm release must be planned and hub_installed true."
  }

  assert {
    condition     = helm_release.hub[0].namespace == "tfha-gke-h3-system" && helm_release.hub[0].name == "tfha-gke-h3"
    error_message = "release must be named after the hub, in <hub>-system."
  }

  assert {
    condition     = helm_release.hub[0].values == tolist([yamlencode(output.chart_values)])
    error_message = "the release's values must be exactly the chart_values output."
  }

  assert {
    condition     = output.chart_values.image.digest == "sha256:538ffc64d7e9cd15a24136bf1493c0154bc386aec0b700c2a5c805092d3c2f96" && !contains(keys(output.chart_values.image), "tag")
    error_message = "the hub image must be pinned by digest, with no tag."
  }

  assert {
    condition     = output.chart_values.image.repository == "us-central1-docker.pkg.dev/tfha-test-project/tfha-scion/scion-hub-gke"
    error_message = "image repository must be <AR repo>/scion-hub-gke."
  }

  assert {
    condition     = output.chart_values.database.driver == "postgres" && output.chart_values.database.auth == "password"
    error_message = "database must be postgres with auth=password."
  }

  assert {
    condition     = !contains(keys(output.chart_values.database), "password")
    error_message = "the DB password must not be in the plain values; it goes through set_sensitive."
  }

  assert {
    condition     = output.chart_values.auth.proxy.iap.audience == "/projects/123456789012/global/backendServices/4242424242"
    error_message = "auth.proxy.iap.audience must be the front's iap_audience."
  }

  assert {
    condition     = output.chart_values.hub.baseUrl == "https://tfha-gke-h3.test.scion-ai.dev" && output.chart_values.hub.hubId == "tfha-gke-h3"
    error_message = "hub.baseUrl must be the front's public_url and hub.hubId the hub name."
  }

  assert {
    condition = (
      output.chart_values.auth.transport.mode == "iap" &&
      output.chart_values.auth.transport.oidcAudience == "123456789-abc.apps.googleusercontent.com" &&
      output.chart_values.auth.transport.platformAuthSa == "tfha-gke-h3-transport@tfha-test-project.iam.gserviceaccount.com"
    )
    error_message = "transport must be iap with the client ID as oidcAudience and the transport SA as platformAuthSa."
  }

  assert {
    condition     = output.chart_values.auth.existingSecret == kubernetes_secret_v1.session[0].metadata[0].name
    error_message = "the chart must read the Terraform-managed session secret."
  }

  assert {
    condition = (
      output.chart_values.secrets.backend == "gcpsm" &&
      output.chart_values.storage.provider == "gcs" &&
      output.chart_values.cloudsql.enabled &&
      output.chart_values.serviceAccount.name == "scion-hub" &&
      output.chart_values.serviceAccount.gcpServiceAccount == "tfha-gke-h3-hub@tfha-test-project.iam.gserviceaccount.com" &&
      output.chart_values.rbac.create &&
      output.chart_values.runtime.namespace == "tfha-gke-h3"
    )
    error_message = "gcpsm/gcs/cloudsql/serviceAccount/rbac/runtime values are not as designed."
  }

  assert {
    condition     = !contains(keys(output.chart_values.hub), "podAnnotations")
    error_message = "no rotation marker means no rotation annotation."
  }

  assert {
    condition = jsondecode(kubernetes_service_v1.neg.metadata[0].annotations["cloud.google.com/neg"]) == {
      exposed_ports = { "8080" = { name = "tfha-gke-h3-hub-neg" } }
    }
    error_message = "the NEG Service must expose port 8080 as a standalone NEG named <hub>-hub-neg."
  }

  assert {
    condition = kubernetes_service_v1.neg.spec[0].selector == tomap({
      "app.kubernetes.io/name"     = "scion-hub"
      "app.kubernetes.io/instance" = "tfha-gke-h3"
    })
    error_message = "the NEG Service must select the chart's pods by its selector labels."
  }

  assert {
    condition     = google_storage_bucket.artifacts.name == "tfha-test-project-tfha-gke-h3-artifacts" && google_storage_bucket.artifacts.uniform_bucket_level_access
    error_message = "artifacts bucket must be <project>-<hub>-artifacts with UBLA."
  }
}

run "client_id_null_skips_the_release" {
  command = plan

  variables {
    iap_oauth_client_id = null
  }

  # The copied check block warns; it does not fail the plan.
  expect_failures = [check.transport_audience_configured]

  assert {
    condition     = length(helm_release.hub) == 0 && !output.hub_installed
    error_message = "with no client ID, no Helm release may be planned and hub_installed must be false."
  }

  assert {
    condition     = length(kubernetes_secret_v1.session) == 0 && length(random_password.session) == 0
    error_message = "the session secret is only needed by the release and is gated with it."
  }

  # The release's surroundings are still created, so the front door can be
  # built and the NEG race measured on the first apply.
  assert {
    condition     = kubernetes_service_v1.neg.metadata[0].namespace == "tfha-gke-h3-system"
    error_message = "the NEG Service must still be planned without a client ID."
  }
}

run "rotation_marker_rolls_pods" {
  command = plan

  variables {
    db_password_rotation = "2026-10-06"
  }

  assert {
    condition     = output.chart_values.hub.podAnnotations["scion.io/db-password-rotation"] == "2026-10-06"
    error_message = "a rotation marker must land in a pod annotation so the pods roll with the new password."
  }
}

run "tag_in_repository_rejected" {
  command = plan

  variables {
    image_repository = "us-central1-docker.pkg.dev/tfha-test-project/tfha-scion/scion-hub-gke:latest"
  }

  expect_failures = [var.image_repository]
}

run "non_digest_rejected" {
  command = plan

  variables {
    image_digest = "latest"
  }

  expect_failures = [var.image_digest]
}
