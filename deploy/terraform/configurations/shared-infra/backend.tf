# Partial backend configuration: bucket and prefix are supplied at `init`
# time via -backend-config, not hardcoded, so the same configuration works
# for any project/prefix without editing this file.
#
#   terraform -chdir=deploy/terraform/configurations/shared-infra init \
#     -backend-config="bucket=<project>-<prefix>-tfstate" \
#     -backend-config="prefix=<prefix>/shared"
#
# See deploy/terraform/README.md for the full bootstrap sequence.
terraform {
  backend "gcs" {}
}
