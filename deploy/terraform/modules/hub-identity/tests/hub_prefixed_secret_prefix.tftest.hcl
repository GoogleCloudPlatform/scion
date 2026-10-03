# Regression coverage for the hub-prefixed secretmanager.admin condition
# (ptone/scion#2152): pins the exact resource-name prefix this module
# computes from hub_name, so a future change to the formula (a different
# hash length, a different scion-<hash>- shape) fails loudly here instead of
# only showing up as a live 403 or an over-broad grant.
#
# Known vector, computed independently of Terraform's sha256():
#   printf %s tfha-h1 | sha256sum
#   -> a9be7bcccaaeee9f7f357624c44c123af8b31131a380bf6326a97cf850e5e5ef
# First 12 hex chars: a9be7bcccaae. The pinned formula (main.tf) is
# "scion-" + substr(lowercase hex sha256(hub_name), 0, 12) + "-", applied to
# the raw hub_name bytes.
mock_provider "google" {}

variables {
  project_id     = "tfha-test-project"
  project_number = "123456789012"
  hub_name       = "tfha-h1"
}

run "hub_prefixed_condition_matches_known_vector" {
  command = plan

  assert {
    condition     = output.hub_iam_condition_expression_prefixed == "projects/123456789012/secrets/scion-a9be7bcccaae-"
    error_message = "hub_iam_condition_expression_prefixed must equal \"projects/<project_number>/secrets/scion-\" + first 12 hex chars of sha256(hub_name) + \"-\" — got a different prefix for the tfha-h1 known vector."
  }

  # Regression guard for the legacy hub-scope grant's removal: hub_iam_grants
  # used to bundle 8 .id values (the legacy hub_secretmanager_admin_hub_scope
  # grant plus 7 others); pinning the count here fails loudly if that grant
  # (or any other) is ever silently reintroduced or dropped.
  assert {
    condition     = length(output.hub_iam_grants) == 7
    error_message = "hub_iam_grants should bundle exactly 7 IAM grant IDs now that the legacy hub-scope secretmanager.admin grant is gone — update this count if you intentionally added or removed a grant."
  }
}
