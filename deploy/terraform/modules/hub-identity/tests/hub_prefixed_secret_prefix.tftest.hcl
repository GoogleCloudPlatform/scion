# Regression coverage for the hub-prefixed secretmanager.admin condition
# (ptone/scion#2152): pins the exact resource-name prefix this module
# computes from hub_name, so a future change to the formula (a different
# hash length, a different scion-<hash>- shape, or an accidental copy-paste
# of the legacy hub-scope prefix) fails loudly here instead of only showing
# up as a live 403 or an over-broad grant.
#
# Known vector, computed independently of Terraform's sha256():
#   printf %s tfha-h1 | sha256sum
#   -> a9be7bcccaaeee9f7f357624c44c123af8b31131a380bf6326a97cf850e5e5ef
# First 12 hex chars: a9be7bcccaae. The pinned formula (main.tf) is
# "scion-" + substr(lowercase hex sha256(hub_name), 0, 12) + "-", applied to
# the raw hub_name bytes (not "hub_name:hub_name" — that's the legacy,
# hub-scope-only hash, asserted separately below).
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

  # The legacy, pre-#2152 hub-scope prefix must be unaffected by adding the
  # new grant — same known hub_name, different hash input
  # ("hub_name:hub_name", not bare hub_name), so this must NOT equal the
  # prefixed condition above.
  assert {
    condition     = output.hub_iam_condition_expression != output.hub_iam_condition_expression_prefixed
    error_message = "the legacy hub-scope prefix and the new hub-prefixed prefix must differ — they hash different inputs and gate different grants."
  }
}
