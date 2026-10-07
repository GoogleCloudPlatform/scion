# Partial backend configuration, one state per hub, same rules as
# configurations/hub:
#
#   terraform -chdir=deploy/terraform/configurations/hub-gke init \
#     -backend-config="bucket=<project>-<prefix>-tfstate" \
#     -backend-config="prefix=<prefix>/hubs/<hub_name>"
#
# The prefix must be unique per hub and must match -var hub_name (see
# state_prefix in variables.tf). Terraform cannot read its own backend
# config, so both are passed explicitly; applying hub B's vars onto hub A's
# state must be impossible. Cloud Run hubs and GKE hubs share the
# <prefix>/hubs/ namespace, so a hub_name is unique across both.
terraform {
  backend "gcs" {}
}
