# scripts/single-node-vm/tests/test_google_sudoers_warn.sh — verifies that
# deploy.sh checks whether the scion service user is in google-sudoers after
# cloud-init completes and warns loudly when it is.
#
# This file has no shebang -- it is always `source`d, never executed --
# so shellcheck needs an explicit shell directive to know its dialect.
# shellcheck shell=bash

test_deploy_warns_when_scion_in_google_sudoers() {
  local deploy_sh="${TIER_DIR}/deploy.sh"
  local block
  block="$(sed -n '/Check that scion service user is not in google-sudoers/,/^fi$/p' "$deploy_sh")"
  assert_contains "$block" "id -nG scion" "deploy.sh checks scion group membership"
  assert_contains "$block" "google-sudoers" "deploy.sh checks for google-sudoers"
  assert_contains "$block" "sudo gpasswd -d scion google-sudoers" "deploy.sh warning includes remediation command"

  # Execute the extracted check against stub gcloud in both modes.
  fresh_gcloud_state
  export GCLOUD_STUB_SSH_SUCCEEDS=true
  export GCLOUD_STUB_SSH_SCION_GOOGLE_SUDOERS=true
  local out_warn
  out_warn="$( (
    # shellcheck disable=SC2034 # read by the eval'd deploy.sh check
    INSTANCE_NAME="scion-hub-test" ZONE="us-central1-a" PROJECT_ID="demo-project"
    # shellcheck disable=SC2329 # invoked indirectly via eval "$block"
    warn() { echo "WARNING: $*" >&2; }
    eval "$block"
  ) 2>&1)"
  assert_contains "$out_warn" "WARNING:" "warns when scion is in google-sudoers"
  assert_contains "$out_warn" "google-sudoers" "warning names google-sudoers"

  export GCLOUD_STUB_SSH_SCION_GOOGLE_SUDOERS=false
  local out_clean
  out_clean="$( (
    # shellcheck disable=SC2034 # read by the eval'd deploy.sh check
    INSTANCE_NAME="scion-hub-test" ZONE="us-central1-a" PROJECT_ID="demo-project"
    # shellcheck disable=SC2329 # invoked indirectly via eval "$block"
    warn() { echo "WARNING: $*" >&2; }
    eval "$block"
  ) 2>&1)"
  assert_eq "" "$out_clean" "prints no warning when scion is not in google-sudoers"
  unset GCLOUD_STUB_SSH_SCION_GOOGLE_SUDOERS
}
