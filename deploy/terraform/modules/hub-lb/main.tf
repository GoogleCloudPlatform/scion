# The hub's front door: a Terraform-owned global external Application Load
# Balancer with IAP, in front of standalone zonal NEGs that the GKE NEG
# controller creates for the hub's pods.
#
#   Internet -> forwarding rule :443 -> target HTTPS proxy (managed cert)
#            -> URL map -> backend service (IAP, timeout_sec 86400)
#            -> zonal NEGs -> hub pod IP:port
#
# The LB lives here, not in a GKE Ingress or Gateway, so that the 24h backend
# timeout (WebSocket lifetime) and IAP stay under Terraform's control.
#
# No DNS resources: DNS is external. The managed certificate stays
# PROVISIONING until a human adds the A record from the dns_record output,
# and nothing in this module waits for that.
#
# All IAM is additive *_iam_member.

locals {
  prefix = "${var.name}-hub"

  # Google's documented source ranges for health checks and for the global
  # external ALB's proxies reaching NEG endpoints.
  health_check_source_ranges = ["35.191.0.0/16", "130.211.0.0/22"]

  # The certificate's name carries a hash of the hostname, so a hostname
  # change creates the new certificate before the old one is removed (the
  # target proxy still references the old one until it is updated).
  cert_name = "${local.prefix}-${substr(sha256(var.hostname), 0, 8)}"
}

# --- Address ---
#
# The IP is the one value a human copies into DNS by hand. Losing it means a
# new IP and a DNS change, and the certificate drops back to PROVISIONING
# until the record catches up. prevent_destroy makes `terraform destroy` of
# this hub fail on purpose; remove it deliberately to retire the hub.
resource "google_compute_global_address" "this" {
  project = var.project_id
  name    = "${local.prefix}-ip"

  lifecycle {
    prevent_destroy = true
  }
}

resource "google_compute_managed_ssl_certificate" "this" {
  project = var.project_id
  name    = local.cert_name

  managed {
    domains = [var.hostname]
  }

  lifecycle {
    create_before_destroy = true
  }
}

# --- Backends ---

# Standalone NEGs created by the GKE NEG controller from hub-gke's annotated
# Service, one per zone with the same name. var.neg_name is unknown until
# that Service exists, so on a fresh hub these reads happen during apply,
# after the Service is created. If the controller has not finished creating
# a NEG by then, the read fails with a not-found error. That race is
# deliberately NOT papered over with a sleep: re-running the apply succeeds
# once the NEGs exist. See the README.
data "google_compute_network_endpoint_group" "this" {
  for_each = toset(var.neg_zones)

  project = var.project_id
  zone    = each.value
  name    = var.neg_name
}

# Probes the pods directly (NEG endpoints are pod IP:port), on the hub's
# readiness endpoint, the same one the chart's readiness probe uses. Health
# checks are not subject to IAP.
resource "google_compute_health_check" "this" {
  project = var.project_id
  name    = "${local.prefix}-hc"

  check_interval_sec  = 10
  timeout_sec         = 5
  healthy_threshold   = 1
  unhealthy_threshold = 3

  http_health_check {
    port         = var.port
    request_path = "/readyz"
  }

  log_config {
    enable = false
  }
}

# Health checks and the ALB's proxies both reach the pods from these ranges.
# No target tags: Autopilot nodes carry no tags Terraform controls, so the
# rule is scoped by port and source range instead.
resource "google_compute_firewall" "health_check" {
  project   = var.project_id
  name      = "${local.prefix}-allow-lb-hc"
  network   = var.network
  direction = "INGRESS"

  source_ranges = local.health_check_source_ranges

  allow {
    protocol = "tcp"
    ports    = [tostring(var.port)]
  }
}

resource "google_compute_backend_service" "this" {
  project               = var.project_id
  name                  = "${local.prefix}-backend"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  protocol              = "HTTP"
  timeout_sec           = var.backend_timeout_sec
  health_checks         = [google_compute_health_check.this.id]

  dynamic "backend" {
    for_each = data.google_compute_network_endpoint_group.this
    content {
      group                 = backend.value.id
      balancing_mode        = "RATE"
      max_rate_per_endpoint = var.max_rate_per_endpoint
    }
  }

  # IAP with the project's Google-managed OAuth client: no oauth2_client_id
  # and no secret. Terraform creates no OAuth client and reads no secret.
  iap {
    enabled = true
  }

  log_config {
    enable = true
  }
}

# --- Frontend ---

resource "google_compute_url_map" "https" {
  project         = var.project_id
  name            = "${local.prefix}-urlmap"
  default_service = google_compute_backend_service.this.id
}

resource "google_compute_target_https_proxy" "this" {
  project          = var.project_id
  name             = "${local.prefix}-https-proxy"
  url_map          = google_compute_url_map.https.id
  ssl_certificates = [google_compute_managed_ssl_certificate.this.id]
}

resource "google_compute_global_forwarding_rule" "https" {
  project               = var.project_id
  name                  = "${local.prefix}-https"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  ip_protocol           = "TCP"
  ip_address            = google_compute_global_address.this.id
  port_range            = "443"
  target                = google_compute_target_https_proxy.this.id
}

resource "google_compute_url_map" "http_redirect" {
  count = var.enable_http_redirect ? 1 : 0

  project = var.project_id
  name    = "${local.prefix}-http-redirect"

  default_url_redirect {
    https_redirect         = true
    redirect_response_code = "MOVED_PERMANENTLY_DEFAULT"
    strip_query            = false
  }
}

resource "google_compute_target_http_proxy" "http_redirect" {
  count = var.enable_http_redirect ? 1 : 0

  project = var.project_id
  name    = "${local.prefix}-http-proxy"
  url_map = google_compute_url_map.http_redirect[0].id
}

resource "google_compute_global_forwarding_rule" "http_redirect" {
  count = var.enable_http_redirect ? 1 : 0

  project               = var.project_id
  name                  = "${local.prefix}-http"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  ip_protocol           = "TCP"
  ip_address            = google_compute_global_address.this.id
  port_range            = "80"
  target                = google_compute_target_http_proxy.http_redirect[0].id
}

# --- IAP access ---
#
# Scoped to this hub's backend service only, never project-wide
# roles/iap.httpsResourceAccessor, which would reach every IAP-protected
# resource in the project, including other hubs.
resource "google_iap_web_backend_service_iam_member" "members" {
  for_each = toset(var.iap_members)

  project             = var.project_id
  web_backend_service = google_compute_backend_service.this.name
  role                = "roles/iap.httpsResourceAccessor"
  member              = each.value
}

resource "google_iap_web_backend_service_iam_member" "transport" {
  project             = var.project_id
  web_backend_service = google_compute_backend_service.this.name
  role                = "roles/iap.httpsResourceAccessor"
  member              = "serviceAccount:${var.transport_sa_email}"
}
