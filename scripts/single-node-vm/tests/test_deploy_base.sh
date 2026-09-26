# scripts/single-node-vm/tests/test_deploy_base.sh — a base smoke test:
# runs upstream deploy.sh itself as a real subprocess against the stub
# `gcloud` on PATH, through a fresh create and then a re-run against the
# state the fresh create left behind, and asserts on the key gcloud calls
# each makes. This is the harness's own proof that it works against
# deploy.sh on `main`, with no feature-specific code (hybrid tier, NAT
# reuse, IAP firewall scope, ...) involved at all.
#
# This file has no shebang -- it is always `source`d, never executed --
# so shellcheck needs an explicit shell directive to know its dialect.
# shellcheck shell=bash

DEPLOY_SH="${TIER_DIR}/deploy.sh"
HUB="basehub"
INSTANCE_NAME="scion-hub-${HUB}"
ROUTER_NAME="scion-hub-${HUB}-router"
NAT_NAME="scion-hub-${HUB}-nat"
FW_RULE_NAME="scion-hub-${HUB}-allow-iap-ssh"
SA_EMAIL="scion-hub-${HUB}@demo-project.iam.gserviceaccount.com"

# base_config_json HUB — a minimal, valid deploy.sh config: fields match
# deploy-config.example.json, with image source "build" (the default,
# needs no registry path) so a create-mode run never needs one.
base_config_json() {
  local hub="$1"
  cat <<EOF
{
  "hub_name": "${hub}",
  "project_id": "demo-project",
  "region": "us-central1",
  "machine_size": "small",
  "disk_size_gb": 200,
  "chat_plugins": [],
  "container_images": {"source": "build", "registry": "", "force_rebuild": false},
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
# create/tag, router, NAT, firewall rule, service account) has run to
# completion, since deploy.sh runs strictly sequentially and the stub's
# `compute ssh` fails immediately (no GCLOUD_STUB_SSH_SUCCEEDS), stopping
# it well short of the real SSH-readiness retry loop. The wait uses a 30s
# budget -- large enough to absorb realistic per-call latency on a loaded
# host. Sets DEPLOY_RC and DEPLOY_LOG.
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
    # If deploy.sh has already exited (a real regression that makes it
    # fail before ever reaching the sentinel, say), there is no point
    # waiting out the rest of the 30s budget to find that out.
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
# Fresh create
# =====================================================================

test_deploy_base_create_provisions_expected_resources() {
  fresh_gcloud_state
  run_deploy_create "$(base_config_json "$HUB")"
  local log
  log="$(gcloud_log)"

  assert_eq "1" "$(echo "$log" | grep -c "^compute instances create ${INSTANCE_NAME} " || true)" \
    "a fresh create must create exactly one VM"
  local create_line
  create_line="$(echo "$log" | grep "^compute instances create ${INSTANCE_NAME} " | head -1)"
  assert_contains "$create_line" "--machine-type=e2-standard-4" "small machine_size maps to e2-standard-4"
  assert_contains "$create_line" "--no-address" "the VM must have no public IP"
  assert_contains "$create_line" "--boot-disk-size=200GB" "disk_size_gb must become the boot-disk-size flag"
  assert_contains "$create_line" "--service-account=${SA_EMAIL}" "the VM must run as its own service account"

  assert_eq "1" "$(echo "$log" | grep -c "^compute routers create ${ROUTER_NAME} " || true)" \
    "a fresh create must create exactly one Cloud Router"
  assert_eq "1" "$(echo "$log" | grep -c "^compute routers nats create ${NAT_NAME} " || true)" \
    "a fresh create must create exactly one Cloud NAT"

  assert_eq "1" "$(echo "$log" | grep -c "^compute firewall-rules create ${FW_RULE_NAME} " || true)" \
    "a fresh create must create exactly one IAP SSH firewall rule"
  local fw_line
  fw_line="$(echo "$log" | grep "^compute firewall-rules create ${FW_RULE_NAME} " | head -1)"
  assert_contains "$fw_line" "--network=default" "the IAP SSH rule must be on the default network"
  assert_contains "$fw_line" "--rules=tcp:22" "the IAP SSH rule must allow only tcp:22"
  assert_contains "$fw_line" "--source-ranges=35.235.240.0/20" "the IAP SSH rule must be scoped to the IAP TCP forwarding range"

  assert_eq "1" "$(echo "$log" | grep -c "^iam service-accounts create scion-hub-${HUB} " || true)" \
    "a fresh create must create exactly one service account"

  local router_line nat_line fw_create_line vm_line
  router_line="$(echo "$log" | grep -n "^compute routers create ${ROUTER_NAME} " | head -1 | cut -d: -f1)"
  nat_line="$(echo "$log" | grep -n "^compute routers nats create ${NAT_NAME} " | head -1 | cut -d: -f1)"
  fw_create_line="$(echo "$log" | grep -n "^compute firewall-rules create ${FW_RULE_NAME} " | head -1 | cut -d: -f1)"
  vm_line="$(echo "$log" | grep -n "^compute instances create ${INSTANCE_NAME} " | head -1 | cut -d: -f1)"
  assert_true "$([[ -n "$router_line" && -n "$vm_line" && "$router_line" -lt "$vm_line" ]] && echo true || echo false)" \
    "the Cloud Router must be created before the VM"
  assert_true "$([[ -n "$nat_line" && -n "$vm_line" && "$nat_line" -lt "$vm_line" ]] && echo true || echo false)" \
    "the Cloud NAT must be created before the VM"
  assert_true "$([[ -n "$fw_create_line" && -n "$vm_line" && "$fw_create_line" -lt "$vm_line" ]] && echo true || echo false)" \
    "the IAP SSH firewall rule must be created before the VM"
}

# =====================================================================
# Re-run (idempotent create)
# =====================================================================

# Reuses the state a real create run left behind (router-exists, the NAT's
# own name-scoped marker, the VM/SA/firewall-rule fixtures) rather than
# hand-seeding it, so a regression that makes any single create-mode
# resource forget how to persist its own "already exists" state is caught
# here too, not just in whatever test happens to hand-seed that one
# resource.
test_deploy_base_rerun_is_idempotent() {
  fresh_gcloud_state
  run_deploy_create "$(base_config_json "$HUB")"
  run_deploy_create "$(base_config_json "$HUB")"
  local log
  log="$(gcloud_log)"

  assert_eq "1" "$(echo "$log" | grep -c '^compute instances create' || true)" \
    "a re-run against an already-existing VM must not create a second one"
  assert_eq "1" "$(echo "$log" | grep -c '^compute routers create' || true)" \
    "a re-run against an already-existing router must not create a second one"
  assert_eq "1" "$(echo "$log" | grep -c '^compute routers nats create' || true)" \
    "a re-run against an already-existing NAT must not create a second one"
  assert_eq "1" "$(echo "$log" | grep -c '^iam service-accounts create' || true)" \
    "a re-run against an already-existing service account must not create a second one"
  assert_eq "1" "$(echo "$log" | grep -c '^compute firewall-rules create' || true)" \
    "a re-run against an already-existing firewall rule must not create a second one"

  # DEPLOY_LOG is the second run's own output (run_deploy_create
  # overwrites it on every call), so these are the re-run's own messages,
  # not left over from the first.
  assert_contains "$DEPLOY_LOG" "VM already exists: ${INSTANCE_NAME}" \
    "deploy.sh should report the VM as already existing"
  assert_contains "$DEPLOY_LOG" "Cloud Router already exists: ${ROUTER_NAME}" \
    "deploy.sh should report the router as already existing"
  assert_contains "$DEPLOY_LOG" "Cloud NAT already exists: ${NAT_NAME}" \
    "deploy.sh should report the NAT as already existing"
  assert_contains "$DEPLOY_LOG" "Firewall rule already exists: ${FW_RULE_NAME}" \
    "deploy.sh should report the firewall rule as already existing"
  assert_contains "$DEPLOY_LOG" "Service account already exists: ${SA_EMAIL}" \
    "deploy.sh should report the service account as already existing"
}

# =====================================================================
# --delete
# =====================================================================

test_deploy_base_delete_on_clean_project_exits_zero() {
  fresh_gcloud_state
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_eq "0" "$DEPLOY_RC" \
    "tearing down a hub with nothing left to delete should still exit 0 (every delete is best-effort)"
  assert_eq "1" "$(gcloud_log | grep -c "^compute instances delete ${INSTANCE_NAME} " || true)" \
    "teardown must still attempt to delete the VM even when it's already gone"
}

# Runs a create, then a --delete against the exact state that create left
# behind (same $GCLOUD_STUB_STATE_DIR, no fresh_gcloud_state in between),
# so teardown is exercised against real resources instead of an empty
# project (see test_deploy_base_delete_on_clean_project_exits_zero above).
test_deploy_base_create_then_delete_removes_everything() {
  fresh_gcloud_state
  run_deploy_create "$(base_config_json "$HUB")"
  run_deploy_delete "$(base_config_json "$HUB")"

  assert_eq "0" "$DEPLOY_RC" "teardown of everything create just made should exit 0"

  local log
  log="$(gcloud_log)"
  for pattern in \
    "^run services delete ${INSTANCE_NAME}-iap-proxy " \
    "^compute instances delete ${INSTANCE_NAME} " \
    "^compute routers nats delete ${NAT_NAME} " \
    "^compute routers delete ${ROUTER_NAME} " \
    "^iam service-accounts delete ${SA_EMAIL} " \
    "^compute firewall-rules delete ${FW_RULE_NAME} "; do
    assert_eq "1" "$(echo "$log" | grep -c -- "$pattern" || true)" \
      "teardown must issue exactly one call matching: ${pattern}"
  done

  assert_eq "0" "$(find "${GCLOUD_STUB_STATE_DIR}/firewall-rules" -name '*.json' | wc -l | tr -d ' ')" \
    "no firewall-rule fixture should remain after teardown"
  assert_eq "0" "$(find "${GCLOUD_STUB_STATE_DIR}/instances" -mindepth 1 | wc -l | tr -d ' ')" \
    "no instance fixture should remain after teardown"
}
