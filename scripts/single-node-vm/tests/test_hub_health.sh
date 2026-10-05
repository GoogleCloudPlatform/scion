# scripts/single-node-vm/tests/test_hub_health.sh — function-level tests
# for deploy.sh's hub_health_status / wait_for_hub_health (ptone/scion#1094):
# /healthz always returns HTTP 200, so the post-install health checks read
# the body's top-level status. healthy passes; degraded at the last attempt
# passes with a warning naming the checks; unhealthy or no answer fails.
#
# The two functions are extracted from deploy.sh by name and eval'd, the
# same function-level style as the other tests that source a single helper
# (see README.md); deploy.sh itself is never run. `gcloud compute ssh` goes
# to the stub (tests/lib/gcloud), whose /healthz answer is set through
# GCLOUD_STUB_HEALTHZ_BODY.
#
# This file has no shebang -- it is always `source`d, never executed --
# so shellcheck needs an explicit shell directive to know its dialect.
# shellcheck shell=bash

DEPLOY_SH="${TIER_DIR}/deploy.sh"

# _load_hub_health_fns -- defines hub_health_status and wait_for_hub_health
# from deploy.sh, plus the globals they read, with a fast retry budget.
_load_hub_health_fns() {
  eval "$(sed -n '/^hub_health_status() {/,/^}/p; /^wait_for_hub_health() {/,/^}/p' "$DEPLOY_SH")"
  INSTANCE_NAME="scion-hub-healthhub"
  ZONE="us-central1-a"
  PROJECT_ID="demo-project"
  HEALTH_CHECK_MAX_ATTEMPTS=3
  HEALTH_CHECK_RETRY_SECS=0
  GREEN="" RESET=""
  fresh_gcloud_state
  export GCLOUD_STUB_SSH_SUCCEEDS=true
}

# _run_wait LABEL -- runs wait_for_hub_health; sets WAIT_RC and WAIT_OUT
# (stdout+stderr).
_run_wait() {
  WAIT_OUT="$(wait_for_hub_health "$1" 2>&1)"
  WAIT_RC=$?
}

test_hub_health_status_reads_top_level_status_only() {
  _load_hub_health_fns
  assert_eq "healthy" "$(hub_health_status '{"status":"healthy","checks":{"database":"healthy"}}')" "standalone healthy"
  assert_eq "degraded" "$(hub_health_status '{"status":"degraded","web":{"status":"ok"},"hub":{"status":"healthy"}}')" \
    "a nested healthy hub must not make a degraded composite read healthy"
  assert_eq "unhealthy" "$(hub_health_status '{"status":"unhealthy","checks":{"database":"unhealthy"}}')" "unhealthy"
  assert_eq "unknown" "$(hub_health_status '')" "no answer"
  assert_eq "unknown" "$(hub_health_status '{"status":"ok"}')" "non-scion body"
}

test_wait_for_hub_health_healthy_passes() {
  _load_hub_health_fns
  unset GCLOUD_STUB_HEALTHZ_BODY
  _run_wait "Health check"
  assert_eq "0" "$WAIT_RC" "healthy must pass"
  assert_contains "$WAIT_OUT" "Health check passed." "healthy prints the pass line"
  assert_eq "1" "$(grep -c 'curl -s http://localhost:8080/healthz' "$GCLOUD_STUB_LOG" || true)" \
    "healthy on the first attempt polls once"
}

test_wait_for_hub_health_degraded_passes_with_warning() {
  _load_hub_health_fns
  export GCLOUD_STUB_HEALTHZ_BODY='{"status":"degraded","web":{"status":"ok"},"hub":{"status":"degraded","checks":{"colocated_broker":"unhealthy: registration failed"}}}'
  _run_wait "Health check"
  unset GCLOUD_STUB_HEALTHZ_BODY
  assert_eq "0" "$WAIT_RC" "degraded means up, so the deploy continues"
  assert_contains "$WAIT_OUT" "DEGRADED" "degraded prints a visible warning"
  assert_contains "$WAIT_OUT" "colocated_broker" "the warning includes the response naming the failing check"
  assert_not_contains "$WAIT_OUT" "Health check passed." "degraded is not reported as a clean pass"
  assert_eq "3" "$(grep -c 'curl -s http://localhost:8080/healthz' "$GCLOUD_STUB_LOG" || true)" \
    "keeps polling for healthy until the last attempt before settling for degraded"
}

test_wait_for_hub_health_unhealthy_fails() {
  _load_hub_health_fns
  export GCLOUD_STUB_HEALTHZ_BODY='{"status":"unhealthy","checks":{"database":"unhealthy"}}'
  _run_wait "Health check"
  unset GCLOUD_STUB_HEALTHZ_BODY
  assert_eq "1" "$WAIT_RC" "unhealthy must fail"
  assert_contains "$WAIT_OUT" "last status: unhealthy" "names the status"
  assert_contains "$WAIT_OUT" "database" "includes the last response"
}

test_wait_for_hub_health_no_answer_fails() {
  _load_hub_health_fns
  export GCLOUD_STUB_HEALTHZ_BODY=''
  _run_wait "Post-restart health check"
  unset GCLOUD_STUB_HEALTHZ_BODY
  assert_eq "1" "$WAIT_RC" "no answer must fail"
  assert_contains "$WAIT_OUT" "Post-restart health check did not pass" "uses the label"
  assert_contains "$WAIT_OUT" "last status: unknown" "names the status"
}
