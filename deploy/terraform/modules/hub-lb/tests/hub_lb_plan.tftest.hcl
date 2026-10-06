# Plan-mode coverage for modules/hub-lb under a mock google provider: no
# credentials, no API calls. Proves the load-bearing settings of the front
# door are what the design says they are. It cannot prove the GCP APIs
# accept them, the managed certificate provisions, or the NEG controller
# creates the NEGs in time; those are vm-deploy's live checks.
mock_provider "google" {}

variables {
  project_id         = "tfha-test-project"
  project_number     = "123456789012"
  name               = "tfha-gke-h3"
  hostname           = "tfha-gke-h3.test.scion-ai.dev"
  network            = "tfha-vpc"
  neg_name           = "tfha-gke-h3-hub-neg"
  neg_zones          = ["us-central1-a", "us-central1-b", "us-central1-c"]
  transport_sa_email = "tfha-gke-h3-transport@tfha-test-project.iam.gserviceaccount.com"
  iap_members        = ["user:someone@example.com", "group:team@example.com"]
}

override_resource {
  target = google_compute_backend_service.this
  values = {
    generated_id = 4242424242
  }
}

override_resource {
  target = google_compute_global_address.this
  values = {
    address = "203.0.113.10"
  }
}

run "front_door_plan" {
  command = plan

  assert {
    condition     = google_compute_backend_service.this.timeout_sec == 86400
    error_message = "backend timeout_sec must be 86400 (it is the WebSocket lifetime)."
  }

  assert {
    condition     = google_compute_backend_service.this.load_balancing_scheme == "EXTERNAL_MANAGED"
    error_message = "backend service must be EXTERNAL_MANAGED (global external ALB)."
  }

  assert {
    condition     = google_compute_backend_service.this.iap[0].enabled == true
    error_message = "IAP must be enabled on the backend service."
  }

  assert {
    condition = (
      google_compute_backend_service.this.iap[0].oauth2_client_id == null &&
      google_compute_backend_service.this.iap[0].oauth2_client_secret == null
    )
    error_message = "IAP must use the Google-managed client: no oauth2_client_id or secret."
  }

  assert {
    condition     = length(data.google_compute_network_endpoint_group.this) == 3
    error_message = "one NEG read (and so one backend) per zone expected."
  }

  assert {
    condition     = google_compute_health_check.this.http_health_check[0].request_path == "/readyz"
    error_message = "health check must probe /readyz."
  }

  assert {
    condition     = google_compute_health_check.this.http_health_check[0].port == 8080
    error_message = "health check must target the pod port."
  }

  assert {
    condition     = toset(google_compute_firewall.health_check.source_ranges) == toset(["35.191.0.0/16", "130.211.0.0/22"])
    error_message = "firewall must allow exactly the Google health-check/proxy ranges."
  }

  assert {
    condition     = google_compute_global_forwarding_rule.https.port_range == "443"
    error_message = "HTTPS forwarding rule must listen on 443."
  }

  assert {
    condition     = length(google_compute_global_forwarding_rule.http_redirect) == 1
    error_message = "HTTP->HTTPS redirect is on by default."
  }

  assert {
    condition     = google_compute_managed_ssl_certificate.this.managed[0].domains == tolist(["tfha-gke-h3.test.scion-ai.dev"])
    error_message = "managed certificate must cover exactly the hostname."
  }

  assert {
    condition     = length(google_iap_web_backend_service_iam_member.members) == 2
    error_message = "one IAP accessor grant per iap_members entry."
  }

  assert {
    condition     = google_iap_web_backend_service_iam_member.transport.member == "serviceAccount:tfha-gke-h3-transport@tfha-test-project.iam.gserviceaccount.com"
    error_message = "the transport SA must be an IAP accessor on this backend service."
  }

  assert {
    condition     = output.public_url == "https://tfha-gke-h3.test.scion-ai.dev"
    error_message = "public_url must be https://<hostname>."
  }

  # lb_ip and the record's value are known only after apply (see the
  # targeted run below for why the address cannot be applied here); the
  # record's name and type are known at plan.
  assert {
    condition     = output.dns_record.name == "tfha-gke-h3.test.scion-ai.dev" && output.dns_record.type == "A"
    error_message = "dns_record must be an A record for the hostname."
  }
}

# iap_audience needs the backend service's generated_id, which is unknown in
# plan mode (Terraform 1.9 has no override_during = plan). So this run
# applies against the MOCK provider (still no API calls), targeted at the
# backend service. The target leaves out google_compute_global_address,
# whose prevent_destroy would otherwise make the test's own teardown fail.
run "iap_audience_shape" {
  command = apply

  plan_options {
    target = [google_compute_backend_service.this]
  }

  assert {
    condition     = output.iap_audience == "/projects/123456789012/global/backendServices/4242424242"
    error_message = "iap_audience must be /projects/<number>/global/backendServices/<numeric id>."
  }
}

run "redirect_can_be_disabled" {
  command = plan

  variables {
    enable_http_redirect = false
  }

  assert {
    condition     = length(google_compute_global_forwarding_rule.http_redirect) == 0 && length(google_compute_url_map.http_redirect) == 0
    error_message = "enable_http_redirect = false must create no port-80 listener."
  }
}

run "bad_hostname_rejected" {
  command = plan

  variables {
    hostname = "https://tfha-gke-h3.test.scion-ai.dev/"
  }

  expect_failures = [var.hostname]
}
