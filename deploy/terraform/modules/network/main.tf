# Custom-mode VPC, one subnet, a private services access (PSA) peering range
# for Cloud SQL, and Cloud NAT for GKE Autopilot's private nodes. No
# provider/backend blocks here: the calling configuration owns those (see
# deploy/terraform/README.md).

resource "google_compute_network" "this" {
  project                 = var.project_id
  name                    = "${var.name_prefix}-vpc"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "this" {
  project       = var.project_id
  name          = "${var.name_prefix}-subnet"
  region        = var.region
  network       = google_compute_network.this.id
  ip_cidr_range = var.subnet_cidr

  private_ip_google_access = true
}

# Reserved range for the VPC_PEERING service-networking connection used by
# Cloud SQL's private IP. NOT used by Filestore: the filestore module
# connects via DIRECT_PEERING, a separate mechanism that reserves its own
# range automatically and never touches this one (found in review — an
# earlier version of this comment claimed Filestore used it too).
resource "google_compute_global_address" "psa" {
  project       = var.project_id
  name          = "${var.name_prefix}-psa"
  purpose       = "VPC_PEERING"
  address_type  = "INTERNAL"
  prefix_length = var.psa_prefix_length
  network       = google_compute_network.this.id
}

resource "google_service_networking_connection" "psa" {
  network                 = google_compute_network.this.id
  service                 = "servicenetworking.googleapis.com"
  reserved_peering_ranges = [google_compute_global_address.psa.name]
}

# Cloud NAT for GKE Autopilot's private nodes (found in review — a fresh
# private-nodes cluster with no NAT has no path to the public internet at
# all). private_ip_google_access above only covers *.googleapis.com and
# Artifact Registry; it does nothing for git clones, package installs, or
# calls to any non-Google API, all of which agent pods do routinely. Without
# this, the cluster looks healthy at apply time and every such call from an
# agent pod hangs until it times out — a runtime failure long after a green
# apply, not a plan-time one.
resource "google_compute_router" "nat" {
  project = var.project_id
  name    = "${var.name_prefix}-router"
  region  = var.region
  network = google_compute_network.this.id
}

resource "google_compute_router_nat" "nat" {
  project                            = var.project_id
  name                               = "${var.name_prefix}-nat"
  router                             = google_compute_router.nat.name
  region                             = var.region
  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "ALL_SUBNETWORKS_ALL_IP_RANGES"
}
