# scripts/single-node-vm/tests/test_proxy_hardening.sh — coverage for the
# hardened-GCP-org defaults that apply to every deploy (restage of
# upstream GoogleCloudPlatform/scion#1928, by nhodson, commit 1 of 2):
# --shielded-secure-boot, the dedicated Cloud Run proxy service account
# (never the hub VM's own SA), and the one-step IAP Cloud Run deploy
# (--no-allow-unauthenticated --iap in the same call, with the IAP service
# agent granted roles/run.invoker and any legacy allUsers invoker binding
# removed) -- so there is never an allUsers invoker window.
#
# Runs upstream deploy.sh itself as a real subprocess against the stub
# `gcloud` on PATH, the same way test_deploy_base.sh and
# test_iap_fw_scope.sh do. Per-file isolation (see README.md "How it
# works") means this file cannot reuse those files' helpers -- they are
# defined here too, not shared, on purpose.
#
# This file has no shebang -- it is always `source`d, never executed --
# so shellcheck needs an explicit shell directive to know its dialect.
# shellcheck shell=bash

DEPLOY_SH="${TIER_DIR}/deploy.sh"
HUB="proxyhard"
INSTANCE_NAME="scion-hub-${HUB}"
PROXY_SERVICE="${INSTANCE_NAME}-iap-proxy"
SA_EMAIL="scion-hub-${HUB}@demo-project.iam.gserviceaccount.com"
PROXY_SA_EMAIL="scion-hub-${HUB}-proxy@demo-project.iam.gserviceaccount.com"
# The stub's "projects describe" case always answers this project number.
PROJECT_NUMBER="123456789012"
IAP_SA_EMAIL="service-${PROJECT_NUMBER}@gcp-sa-iap.iam.gserviceaccount.com"

# proxy_hardening_config_json HUB — a minimal, valid deploy.sh config:
# fields match deploy-config.example.json, with image source "registry"
# (needs no build/poll cycle on the VM) so a create-mode run reaching
# Phase 4 doesn't have to simulate Phase 3b's image build too.
proxy_hardening_config_json() {
  local hub="$1"
  cat <<EOF
{
  "hub_name": "${hub}",
  "project_id": "demo-project",
  "region": "us-central1",
  "machine_size": "small",
  "disk_size_gb": 200,
  "chat_plugins": [],
  "container_images": {"source": "registry", "registry": "us-docker.pkg.dev/demo-project/scion", "force_rebuild": false},
  "admin_email": "admin@example.com",
  "update_policy": "auto",
  "release_channel": "nightly"
}
EOF
}

# _start_deploy_bg CONFIG_FILE LOG_FILE — starts deploy.sh (create mode)
# in the background, in its own process group (job control on for this
# call only), and sets the global _DEPLOY_BG_PID to its PID. `--version`
# is always passed so deploy.sh never needs to shell out for a GitHub
# Releases lookup to pick one. See _stop_deploy_bg for why this is a
# plain function call, not a command substitution.
_start_deploy_bg() {
  local config_file="$1" log_file="$2"
  set -m
  bash "$DEPLOY_SH" --config "$config_file" --version v1.0.0-test \
    < /dev/null > "$log_file" 2>&1 &
  _DEPLOY_BG_PID=$!
  set +m
}

# _stop_deploy_bg PID -- sends SIGTERM to PID's entire process group (not
# just PID itself), so an in-flight child the stub gcloud spawns dies
# alongside deploy.sh. Reaps PID via `wait` and sets DEPLOY_RC to its exit
# status, then polls `kill -0` on the group until every member is
# actually gone, up to a bounded safety timeout.
_stop_deploy_bg() {
  local pid="$1"
  kill -TERM -- "-${pid}" 2>/dev/null || true
  wait "$pid" 2>/dev/null
  DEPLOY_RC=$?
  local waited_ms=0
  while kill -0 -- "-${pid}" 2>/dev/null && [[ "$waited_ms" -lt 5000 ]]; do
    sleep 0.05
    waited_ms=$((waited_ms + 50))
  done
}

# run_deploy_create CONFIG_JSON — runs deploy.sh (create mode) in the
# background and stops it once the `phase2-complete` sentinel appears
# (touched by the stub's `compute ssh` handler on its first invocation --
# see tests/lib/gcloud): by that point deploy.sh's entire Phase 2 (VM
# create/tag, router, NAT, firewall rule, both service accounts) has run
# to completion, since deploy.sh runs strictly sequentially and the
# stub's `compute ssh` fails immediately (no GCLOUD_STUB_SSH_SUCCEEDS),
# stopping it well short of the real SSH-readiness retry loop and every
# later phase. Sets DEPLOY_RC and DEPLOY_LOG.
run_deploy_create() {
  local config_json="$1" config_file
  config_file="$(mktemp)"
  printf '%s' "$config_json" > "$config_file"
  local sentinel="${GCLOUD_STUB_STATE_DIR}/phase2-complete"
  rm -f "$sentinel"
  local log_file
  log_file="$(mktemp)"
  local pid
  _start_deploy_bg "$config_file" "$log_file"
  pid="$_DEPLOY_BG_PID"
  local waited_ms=0
  while [[ ! -f "$sentinel" && "$waited_ms" -lt 30000 ]]; do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1
    waited_ms=$((waited_ms + 100))
  done
  if [[ ! -f "$sentinel" ]]; then
    _stop_deploy_bg "$pid"
    DEPLOY_RC=1
    DEPLOY_LOG="$(cat "$log_file")
FATAL: run_deploy_create: sentinel '${sentinel}' was never reached within 30000ms -- deploy.sh may be stuck, or this test's expectations no longer match its actual call sequence"
    rm -f "$config_file" "$log_file"
    return 1
  fi
  _stop_deploy_bg "$pid"
  DEPLOY_LOG="$(cat "$log_file")"
  rm -f "$config_file" "$log_file"
}

# run_deploy_create_through_phase4 CONFIG_JSON — runs deploy.sh (create
# mode) all the way through the new Phase 4 IAP-hardening calls (the
# one-step IAP deploy, the IAP service agent invoker grant, and the
# allUsers-invoker check/removal), then stops it. GCLOUD_STUB_SSH_SUCCEEDS=true
# makes every `compute ssh`/`compute scp` call succeed instead of failing
# immediately, so deploy.sh actually reaches Phase 4 instead of stopping
# at the SSH-readiness retry loop the way run_deploy_create's callers do.
# Stops at the `phase4-iap-hardening-done` sentinel (touched by the
# stub's `iap web add-iam-policy-binding` case -- see tests/lib/gcloud):
# deliberately the first gcloud call *after* the entire allUsers-check
# block (get-iam-policy, and the conditional remove-iam-policy-binding),
# not a call inside it -- stopping mid-block would race whichever of
# those two calls a test is asserting on against this function's own
# 100ms poll (see round-2 review finding B1). Reached well before the
# real (60s) IAP-enforcement sleep later in Phase 4. Killing deploy.sh
# during that sleep (if it gets there before this function's poll
# notices the sentinel) is just as fast as killing it before -- SIGTERM
# interrupts `sleep` immediately either way.
#
# Also handles deploy.sh exiting **on its own** before the sentinel
# appears (the allUsers hard-fail path deliberately `exit 1`s before
# reaching the sentinel) -- that is a real, valid completion, not a hung
# process, so it must produce the same real DEPLOY_RC/DEPLOY_LOG a normal
# sentinel-based stop would, not the synthetic "sentinel never reached"
# fatal message the genuine-timeout branch below produces.
run_deploy_create_through_phase4() {
  local config_json="$1" config_file
  config_file="$(mktemp)"
  printf '%s' "$config_json" > "$config_file"
  local sentinel="${GCLOUD_STUB_STATE_DIR}/phase4-iap-hardening-done"
  rm -f "$sentinel"
  local log_file
  log_file="$(mktemp)"
  local pid
  export GCLOUD_STUB_SSH_SUCCEEDS=true
  _start_deploy_bg "$config_file" "$log_file"
  pid="$_DEPLOY_BG_PID"
  local waited_ms=0
  local exited_on_own=false
  while [[ ! -f "$sentinel" && "$waited_ms" -lt 30000 ]]; do
    if ! kill -0 "$pid" 2>/dev/null; then
      exited_on_own=true
      break
    fi
    sleep 0.1
    waited_ms=$((waited_ms + 100))
  done
  if [[ "$exited_on_own" != "true" && ! -f "$sentinel" ]]; then
    _stop_deploy_bg "$pid"
    DEPLOY_RC=1
    DEPLOY_LOG="$(cat "$log_file")
FATAL: run_deploy_create_through_phase4: sentinel '${sentinel}' was never reached within 30000ms -- deploy.sh may be stuck, or this test's expectations no longer match its actual call sequence"
    rm -f "$config_file" "$log_file"
    unset GCLOUD_STUB_SSH_SUCCEEDS
    return 1
  fi
  # _stop_deploy_bg's `wait "$pid"` returns the process's real exit status
  # whether it already exited on its own or is being SIGTERM'd here, so
  # DEPLOY_RC is accurate either way.
  _stop_deploy_bg "$pid"
  DEPLOY_LOG="$(cat "$log_file")"
  rm -f "$config_file" "$log_file"
  unset GCLOUD_STUB_SSH_SUCCEEDS
}

# run_deploy_delete CONFIG_JSON — runs `deploy.sh --delete` to completion
# (nothing in that path sleeps or retries). Sets DEPLOY_RC and DEPLOY_LOG.
run_deploy_delete() {
  local config_json="$1" config_file
  config_file="$(mktemp)"
  printf '%s' "$config_json" > "$config_file"
  DEPLOY_LOG="$(bash "$DEPLOY_SH" --delete --config "$config_file" < /dev/null 2>&1)"
  DEPLOY_RC=$?
  rm -f "$config_file"
}

# =====================================================================
# --shielded-secure-boot
# =====================================================================

test_proxy_hardening_vm_create_has_shielded_secure_boot() {
  fresh_gcloud_state
  run_deploy_create "$(proxy_hardening_config_json "$HUB")"
  local log create_line
  log="$(gcloud_log)"
  create_line="$(echo "$log" | grep "^compute instances create ${INSTANCE_NAME} " | head -1)"
  assert_contains "$create_line" "--shielded-secure-boot" \
    "the hub VM must be created with Secure Boot enabled"
}

test_proxy_hardening_services_enable_includes_iam_api() {
  fresh_gcloud_state
  run_deploy_create "$(proxy_hardening_config_json "$HUB")"
  local enable_line
  enable_line="$(gcloud_log | grep "^services enable " | head -1)"
  assert_contains "$enable_line" "iam.googleapis.com" \
    "iam.googleapis.com must be enabled (needed once a non-default service account is involved)"
}

# =====================================================================
# Dedicated proxy service account
# =====================================================================

test_proxy_hardening_proxy_sa_created_distinct_from_hub_sa() {
  fresh_gcloud_state
  run_deploy_create "$(proxy_hardening_config_json "$HUB")"
  local log create_line vm_line
  log="$(gcloud_log)"

  assert_eq "1" "$(echo "$log" | grep -c "^iam service-accounts create scion-hub-${HUB}-proxy " || true)" \
    "a fresh create must create exactly one proxy service account"
  create_line="$(echo "$log" | grep "^iam service-accounts create scion-hub-${HUB}-proxy " | head -1)"
  assert_not_contains "$create_line" "--role" \
    "the proxy service account must be created with no IAM role attached to the create call itself"

  # The hub VM must still run as its own SA, not the proxy SA.
  vm_line="$(echo "$log" | grep "^compute instances create ${INSTANCE_NAME} " | head -1)"
  assert_contains "$vm_line" "--service-account=${SA_EMAIL}" \
    "the hub VM must keep using its own service account, not the proxy SA"

  # The proxy SA must never appear as the --member of any project-level
  # role grant -- it should get zero project IAM roles.
  assert_eq "0" "$(echo "$log" | grep -c "^projects add-iam-policy-binding demo-project --member=serviceAccount:${PROXY_SA_EMAIL} " || true)" \
    "the proxy service account must not be granted any project IAM role"
}

test_proxy_hardening_proxy_sa_idempotent_rerun() {
  fresh_gcloud_state
  run_deploy_create "$(proxy_hardening_config_json "$HUB")"
  run_deploy_create "$(proxy_hardening_config_json "$HUB")"
  local log
  log="$(gcloud_log)"

  assert_eq "1" "$(echo "$log" | grep -c "^iam service-accounts create scion-hub-${HUB}-proxy " || true)" \
    "a re-run against an already-existing proxy service account must not create a second one"
  assert_contains "$DEPLOY_LOG" "Proxy service account already exists: ${PROXY_SA_EMAIL}" \
    "deploy.sh should report the proxy service account as already existing"
}

test_proxy_hardening_teardown_deletes_proxy_sa() {
  fresh_gcloud_state
  run_deploy_create "$(proxy_hardening_config_json "$HUB")"
  run_deploy_delete "$(proxy_hardening_config_json "$HUB")"

  assert_eq "0" "$DEPLOY_RC" "teardown of everything create just made should exit 0"

  local log
  log="$(gcloud_log)"
  assert_eq "1" "$(echo "$log" | grep -c -- "^iam service-accounts delete ${PROXY_SA_EMAIL} " || true)" \
    "teardown must delete the proxy service account exactly once"
  assert_false "$([[ -f "${GCLOUD_STUB_STATE_DIR}/service-accounts/${PROXY_SA_EMAIL}.json" ]] && echo true)" \
    "the proxy SA fixture should be gone after teardown"
  assert_contains "$DEPLOY_LOG" "Deleted proxy SA:          ${PROXY_SA_EMAIL}" \
    "the summary must report the proxy SA as deleted"
}

# _expected_proxy_sa_name HUB_NAME — reimplements deploy.sh's
# proxy_sa_name algorithm independently, so these tests cross-check
# actual gcloud_log entries against a computation that doesn't just trust
# whatever deploy.sh happened to do.
_expected_proxy_sa_name() {
  local hub_name="$1" name="scion-hub-${1}-proxy"
  if [[ ${#name} -le 30 ]]; then
    printf '%s' "$name"
    return
  fi
  local hash
  hash="$(printf '%s' "$hub_name" | openssl dgst -sha256 -r | cut -d' ' -f1 | cut -c1-4)"
  printf '%s' "scion-hub-${hub_name:0:9}-${hash}-proxy"
}

# A 20-char hub name (the maximum deploy.sh allows) always exceeds the
# 30-char SA-id limit once "-proxy" is appended, so this always exercises
# the truncated/hashed path, not the short-name passthrough.
test_proxy_hardening_proxy_sa_long_hub_name_create_then_delete_same_email() {
  fresh_gcloud_state
  local hub="abcdefghijklmnopqrst"
  local expected_sa
  expected_sa="$(_expected_proxy_sa_name "$hub")"

  run_deploy_create "$(proxy_hardening_config_json "$hub")"
  local log
  log="$(gcloud_log)"
  assert_eq "1" "$(echo "$log" | grep -c "^iam service-accounts create ${expected_sa} " || true)" \
    "the long hub name's proxy SA must be created under the expected (hashed) name"

  run_deploy_delete "$(proxy_hardening_config_json "$hub")"
  log="$(gcloud_log)"
  assert_eq "1" "$(echo "$log" | grep -c -- "^iam service-accounts delete ${expected_sa}@demo-project.iam.gserviceaccount.com " || true)" \
    "teardown must delete the exact same (hashed) proxy SA the create path used -- not a differently-truncated name"
}

# Two hub names sharing the first 9 (and more) characters must not
# collide on the same truncated proxy SA name -- see proxy_sa_name's own
# comment for why a plain positional truncation is unsafe here.
test_proxy_hardening_proxy_sa_shared_prefix_long_names_get_distinct_sas() {
  fresh_gcloud_state
  local hub_a="engineering-team-aaa" hub_b="engineering-team-bbb"
  local sa_a sa_b
  sa_a="$(_expected_proxy_sa_name "$hub_a")"
  sa_b="$(_expected_proxy_sa_name "$hub_b")"

  assert_true "$([[ "$sa_a" != "$sa_b" ]] && echo true || echo false)" \
    "two hub names sharing a long common prefix must derive different proxy SA names"

  run_deploy_create "$(proxy_hardening_config_json "$hub_a")"
  run_deploy_create "$(proxy_hardening_config_json "$hub_b")"
  local log
  log="$(gcloud_log)"
  assert_eq "1" "$(echo "$log" | grep -c "^iam service-accounts create ${sa_a} " || true)" \
    "hub A's proxy SA must be created under its own distinct name"
  assert_eq "1" "$(echo "$log" | grep -c "^iam service-accounts create ${sa_b} " || true)" \
    "hub B's proxy SA must be created under its own distinct name"
}

# =====================================================================
# Phase 4: one-step IAP Cloud Run deploy
# =====================================================================

test_proxy_hardening_one_step_iap_deploy_no_separate_update_call() {
  fresh_gcloud_state
  run_deploy_create_through_phase4 "$(proxy_hardening_config_json "$HUB")"
  local log deploy_line
  log="$(gcloud_log)"

  assert_eq "1" "$(echo "$log" | grep -c "^beta run deploy ${PROXY_SERVICE} " || true)" \
    "the proxy must be deployed exactly once via the one-step IAP path"
  deploy_line="$(echo "$log" | grep "^beta run deploy ${PROXY_SERVICE} " | head -1)"
  assert_contains "$deploy_line" "--no-allow-unauthenticated" \
    "the deploy call must pass --no-allow-unauthenticated"
  assert_contains "$deploy_line" "--iap" \
    "the deploy call must pass --iap"
  assert_not_contains "$deploy_line" " --allow-unauthenticated " \
    "the deploy call must never pass the bare --allow-unauthenticated flag"

  assert_eq "0" "$(echo "$log" | grep -c '^beta run services update' || true)" \
    "there must be no later, separate 'beta run services update --iap' call -- IAP is enabled in the one deploy call"
  assert_eq "0" "$(echo "$log" | grep -c '^run deploy' || true)" \
    "the plain (non-beta) 'run deploy' must never be used for the proxy"
}

test_proxy_hardening_proxy_deploy_uses_proxy_sa_not_hub_sa() {
  fresh_gcloud_state
  run_deploy_create_through_phase4 "$(proxy_hardening_config_json "$HUB")"
  local log deploy_line
  log="$(gcloud_log)"

  deploy_line="$(echo "$log" | grep "^beta run deploy ${PROXY_SERVICE} " | head -1)"
  assert_contains "$deploy_line" "--service-account=${PROXY_SA_EMAIL}" \
    "the proxy must be deployed with the dedicated proxy service account"
  assert_not_contains "$deploy_line" "--service-account=${SA_EMAIL} " \
    "the proxy must never be deployed with the hub VM's own service account"
}

# =====================================================================
# Phase 4: IAP service agent invoker grant
# =====================================================================

test_proxy_hardening_iap_service_agent_granted_invoker() {
  fresh_gcloud_state
  run_deploy_create_through_phase4 "$(proxy_hardening_config_json "$HUB")"
  local log binding_line
  log="$(gcloud_log)"

  assert_eq "1" "$(echo "$log" | grep -c "^run services add-iam-policy-binding ${PROXY_SERVICE} " || true)" \
    "the IAP service agent invoker binding must be granted exactly once"
  binding_line="$(echo "$log" | grep "^run services add-iam-policy-binding ${PROXY_SERVICE} " | head -1)"
  assert_contains "$binding_line" "--member=serviceAccount:${IAP_SA_EMAIL}" \
    "the binding must target the project's own IAP service agent"
  assert_contains "$binding_line" "--role=roles/run.invoker" \
    "the binding must grant roles/run.invoker"
  assert_contains "$binding_line" "--condition=None" \
    "the binding must pass --condition=None, matching this deploy family's IAM-binding style"
}

# N2: the PR body calls this grant a "hard-fail safety net" -- prove it
# actually is one. The call is unguarded (no `if`/`|| true`), so
# deploy.sh's own `set -euo pipefail` is what stops the deploy; this
# confirms that actually happens rather than the failure being silently
# swallowed somewhere.
test_proxy_hardening_iap_agent_grant_failure_stops_deploy() {
  fresh_gcloud_state
  set_run_service_add_binding_will_fail "$PROXY_SERVICE"
  run_deploy_create_through_phase4 "$(proxy_hardening_config_json "$HUB")"

  assert_true "$([[ "$DEPLOY_RC" -ne 0 ]] && echo true || echo false)" \
    "a failed IAP-agent invoker grant must stop the deploy"
  assert_eq "0" "$(gcloud_log | grep -c '^run services get-iam-policy' || true)" \
    "deploy.sh must not proceed to the allUsers check when the invoker grant itself failed"
}

# n6: the IAP service agent identity must actually be provisioned, with
# --project, and in the right place relative to the two calls it exists
# between.
test_proxy_hardening_iap_identity_created_before_invoker_grant() {
  fresh_gcloud_state
  run_deploy_create_through_phase4 "$(proxy_hardening_config_json "$HUB")"
  local log identity_line enable_ln identity_ln grant_ln

  log="$(gcloud_log)"
  assert_eq "1" "$(echo "$log" | grep -c '^beta services identity create ' || true)" \
    "the IAP service agent identity must be provisioned exactly once"
  identity_line="$(echo "$log" | grep '^beta services identity create ' | head -1)"
  assert_contains "$identity_line" "--service=iap.googleapis.com" \
    "the identity call must target the IAP API"
  assert_contains "$identity_line" "--project=" \
    "the identity create call must carry --project"

  enable_ln="$(line_number "services enable " "$log")"
  identity_ln="$(line_number "beta services identity create " "$log")"
  grant_ln="$(line_number "run services add-iam-policy-binding" "$log")"
  assert_true "$([[ -n "$enable_ln" && -n "$identity_ln" && "$enable_ln" -lt "$identity_ln" ]] && echo true || echo false)" \
    "services enable must run before the IAP identity is provisioned"
  assert_true "$([[ -n "$identity_ln" && -n "$grant_ln" && "$identity_ln" -lt "$grant_ln" ]] && echo true || echo false)" \
    "the IAP identity must be provisioned before the invoker grant that depends on it existing"
}

# =====================================================================
# Phase 4: allUsers invoker-binding removal (upgrade path)
# =====================================================================

test_proxy_hardening_allusers_absent_no_removal_attempted() {
  fresh_gcloud_state
  run_deploy_create_through_phase4 "$(proxy_hardening_config_json "$HUB")"
  local log get_policy_line

  log="$(gcloud_log)"
  assert_eq "1" "$(echo "$log" | grep -c "^run services get-iam-policy ${PROXY_SERVICE} " || true)" \
    "deploy.sh must always check for a lingering allUsers binding, exactly once"
  get_policy_line="$(echo "$log" | grep "^run services get-iam-policy ${PROXY_SERVICE} " | head -1)"
  assert_contains "$get_policy_line" "--filter=bindings.role=roles/run.invoker AND bindings.members=allUsers" \
    "the policy check must be scoped to exactly allUsers on roles/run.invoker, not any binding"
  assert_eq "0" "$(echo "$log" | grep -c "^run services remove-iam-policy-binding ${PROXY_SERVICE} " || true)" \
    "no allUsers binding means no removal call is ever made"
  assert_contains "$DEPLOY_LOG" "No allUsers invoker binding present." \
    "deploy.sh should report that no allUsers binding was found, not warn about one"
}

test_proxy_hardening_allusers_present_gets_removed() {
  fresh_gcloud_state
  seed_run_service_allusers_invoker "$PROXY_SERVICE"
  run_deploy_create_through_phase4 "$(proxy_hardening_config_json "$HUB")"
  local log remove_line

  log="$(gcloud_log)"
  assert_eq "1" "$(echo "$log" | grep -c "^run services remove-iam-policy-binding ${PROXY_SERVICE} " || true)" \
    "a present allUsers binding must be removed exactly once"
  remove_line="$(echo "$log" | grep "^run services remove-iam-policy-binding ${PROXY_SERVICE} " | head -1)"
  assert_contains "$remove_line" "--member=allUsers" \
    "the removal must target the allUsers member"
  assert_contains "$remove_line" "--role=roles/run.invoker" \
    "the removal must target the roles/run.invoker role"
  assert_contains "$DEPLOY_LOG" "Found a legacy allUsers invoker binding" \
    "deploy.sh should warn that it found and is removing a legacy binding"
  assert_contains "$DEPLOY_LOG" "allUsers invoker binding removed." \
    "deploy.sh should report the allUsers binding as removed"
  # Proves deploy.sh actually continued past the removal (not that it
  # merely logged "removed" and then died) -- the deployer IAP binding is
  # the very next gcloud call after this whole block, and also this
  # test's own stop sentinel (see run_deploy_create_through_phase4).
  assert_eq "1" "$(echo "$log" | grep -c '^iap web add-iam-policy-binding' || true)" \
    "deploy.sh must proceed to the next phase after a successful removal"
}

test_proxy_hardening_allusers_present_removal_failure_is_fatal() {
  fresh_gcloud_state
  seed_run_service_allusers_invoker "$PROXY_SERVICE"
  set_run_service_remove_binding_will_fail "$PROXY_SERVICE"
  run_deploy_create_through_phase4 "$(proxy_hardening_config_json "$HUB")"

  # Exactly 1, not just "non-zero": a SIGTERM'd deploy.sh (say, if the
  # `exit 1` regressed to a warning and deploy.sh ran on to the harness's
  # own kill) would return 143, which the review's round-2 mutation n3
  # showed a bare "!= 0" check can't tell apart from a real, on-purpose
  # failure. See run_deploy_create_through_phase4's exited_on_own
  # handling -- this only reads DEPLOY_RC==1 correctly because the
  # sentinel now sits *after* this whole block, not inside it.
  assert_eq "1" "$DEPLOY_RC" \
    "deploy.sh must stop itself with exit 1, not merely warn and let the harness's own SIGTERM produce a nonzero code"
  assert_contains "$DEPLOY_LOG" "Refusing to leave this service reachable by allUsers" \
    "deploy.sh should explain why it stopped"
  assert_contains "$DEPLOY_LOG" "gcloud run services get-iam-policy" \
    "the manual-verification hint must use the real command name (not the nonexistent 'gcloud run get-iam-policy')"
  assert_eq "0" "$(gcloud_log | grep -c '^iap web add-iam-policy-binding' || true)" \
    "deploy.sh must never reach the next phase when it can't confirm allUsers was actually removed"
}

# B3: a failure to even *read* the policy must fail closed, the same way
# a confirmed-present-but-unremovable binding does -- an unreadable
# policy is not "verified clean".
test_proxy_hardening_allusers_get_policy_failure_is_fatal() {
  fresh_gcloud_state
  set_run_service_get_policy_will_fail "$PROXY_SERVICE"
  run_deploy_create_through_phase4 "$(proxy_hardening_config_json "$HUB")"

  assert_eq "1" "$DEPLOY_RC" \
    "deploy.sh must stop if it cannot read the IAM policy to check for allUsers"
  assert_contains "$DEPLOY_LOG" "Could not read the IAM policy for ${PROXY_SERVICE}" \
    "deploy.sh should explain that the policy read itself failed, distinct from a confirmed allUsers binding"
  assert_contains "$DEPLOY_LOG" "simulated failure reading IAM policy for service [${PROXY_SERVICE}]" \
    "deploy.sh should also show gcloud's own error text, not just its own summary line"
  assert_eq "0" "$(gcloud_log | grep -c '^run services remove-iam-policy-binding' || true)" \
    "deploy.sh must not attempt a removal when it doesn't actually know the policy state"
  assert_eq "0" "$(gcloud_log | grep -c '^iap web add-iam-policy-binding' || true)" \
    "deploy.sh must never reach the next phase when the policy couldn't be verified"
}
