# configurations/hub must plan exactly as before the hub-gke opt-ins were
# added to its modules: hub-identity's hub_workload_identity_ksa (default
# null) and agent-runtime-k8s's create_hub_rbac (default true).
#
# configurations/hub passes neither argument, so each run below calls one of
# those modules with exactly the arguments configurations/hub's main.tf
# passes and nothing more, and asserts the defaults keep today's resources:
# the hub Role/RoleBinding with both subjects, no hub Workload Identity grant,
# and the same 7 hub IAM grant handles hub-cloudrun's boot_prerequisites
# keys on. (A root test cannot read resources inside child modules, and
# adding outputs to configurations/hub would itself change every hub's plan,
# so the modules are exercised directly.)
#
# Not provable under mock providers: that an EXISTING state keeps its RBAC
# objects. count moved kubernetes_role.hub / kubernetes_role_binding.hub to
# [0], and agent-runtime-k8s carries moved blocks for both, so applied state
# is re-addressed rather than destroyed. When this change was made, the
# verbose plan of fresh_hub_plan.tftest.hcl was diffed before and after: 47
# planned objects both times, and the only difference was those two
# addresses gaining [0].

mock_provider "google" {}
mock_provider "kubernetes" {}

run "agent_runtime_k8s_keeps_hub_rbac_by_default" {
  command = plan

  module {
    source = "../../modules/agent-runtime-k8s"
  }

  variables {
    hub_name         = "tfha-h2"
    project_id       = "tfha-test-project"
    hub_sa_email     = "tfha-h2-hub@tfha-test-project.iam.gserviceaccount.com"
    hub_sa_unique_id = "115656325337183068810"
    agent_sa_email   = "tfha-h2-agent@tfha-test-project.iam.gserviceaccount.com"
    nfs = {
      server     = "10.0.0.2"
      share_path = "/scion"
    }
    nfs_uid      = 1000
    nfs_gid      = 1000
    subpath_root = "projects"
    capacity     = "1Ti"
  }

  assert {
    condition     = output.hub_rbac_created && length(kubernetes_role.hub) == 1 && length(kubernetes_role_binding.hub) == 1
    error_message = "create_hub_rbac must default to true: Cloud Run hubs keep their Role/RoleBinding."
  }

  assert {
    condition     = toset([for s in kubernetes_role_binding.hub[0].subject : s.name]) == toset(["tfha-h2-hub@tfha-test-project.iam.gserviceaccount.com", "115656325337183068810"])
    error_message = "the RoleBinding must keep both subjects (hub GSA email and unique_id)."
  }
}

run "hub_identity_adds_no_workload_identity_grant_by_default" {
  command = plan

  module {
    source = "../../modules/hub-identity"
  }

  variables {
    project_id     = "tfha-test-project"
    project_number = "123456789012"
    hub_name       = "tfha-h2"
  }

  assert {
    condition     = length(google_service_account_iam_member.hub_workload_identity_user) == 0 && output.hub_workload_identity_member == null
    error_message = "hub_workload_identity_ksa must default to null and create no grant."
  }

  assert {
    condition     = length(output.hub_iam_grants) == 7
    error_message = "hub_iam_grants must stay the same 7 handles (hub-cloudrun keys boot ordering on them)."
  }
}
