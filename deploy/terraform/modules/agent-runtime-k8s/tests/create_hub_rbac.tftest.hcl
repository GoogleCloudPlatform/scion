# create_hub_rbac: default true keeps the hub Role/RoleBinding (every Cloud
# Run hub); false (hub-gke, whose chart owns the hub's RBAC) creates neither
# and leaves the rest of the module alone.
mock_provider "google" {}
mock_provider "kubernetes" {}

variables {
  hub_name         = "tfha-gke-h3"
  project_id       = "tfha-test-project"
  hub_sa_email     = "tfha-gke-h3-hub@tfha-test-project.iam.gserviceaccount.com"
  hub_sa_unique_id = "115656325337183068810"
  agent_sa_email   = "tfha-gke-h3-agent@tfha-test-project.iam.gserviceaccount.com"
  nfs = {
    server     = "10.0.0.2"
    share_path = "/scion"
  }
}

run "default_creates_hub_rbac" {
  command = plan

  assert {
    condition     = length(kubernetes_role.hub) == 1 && length(kubernetes_role_binding.hub) == 1 && output.hub_rbac_created
    error_message = "default must create the hub Role and RoleBinding."
  }
}

run "false_creates_no_hub_rbac" {
  command = plan

  variables {
    create_hub_rbac = false
  }

  assert {
    condition     = length(kubernetes_role.hub) == 0 && length(kubernetes_role_binding.hub) == 0 && !output.hub_rbac_created
    error_message = "create_hub_rbac = false must create neither the Role nor the RoleBinding."
  }

  assert {
    condition     = google_service_account_iam_member.agent_workload_identity_user.member == "serviceAccount:tfha-test-project.svc.id.goog[tfha-gke-h3/default]"
    error_message = "the agent Workload Identity binding is unaffected by create_hub_rbac."
  }
}
