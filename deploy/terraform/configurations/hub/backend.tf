# Partial backend configuration, one state per hub:
#
#   terraform -chdir=deploy/terraform/configurations/hub init \
#     -backend-config="bucket=<project>-<prefix>-tfstate" \
#     -backend-config="prefix=<prefix>/hubs/<hub_name>"
#
# The prefix must be unique per hub and must match -var hub_name (see the
# state_prefix check in variables.tf/main.tf) — Terraform cannot read its
# own backend config, so the README wrapper passes both explicitly. It must
# be impossible to silently apply hub B's vars onto hub A's state.
terraform {
  backend "gcs" {}
}
