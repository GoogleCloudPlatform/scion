# scripts/single-node-vm/tests/test_hardened_org.sh — coverage for
# deploy.sh's hardened-GCP-org network handling (restage of upstream
# GoogleCloudPlatform/scion#1928, by nhodson, commit 2 of 2): the fail-fast
# default-network check, and the scoped tcp:8080 proxy-to-VM firewall rule
# that replaces an implicit dependency on default-allow-internal. See
# test_proxy_hardening.sh for the commit-1 defaults (shielded VM,
# dedicated proxy SA, one-step IAP deploy).
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
HUB="hardened"
HUB_TAG="scion-hub-${HUB}"
FW_8080_RULE_NAME="scion-hub-${HUB}-allow-proxy"
# The stub's "compute networks subnets describe" case defaults to this
# CIDR unless a test overrides it with set_subnet_cidr.
DEFAULT_SUBNET_CIDR="10.128.0.0/20"
# deploy.sh's own NETWORK_CHECK_MAX_ATTEMPTS -- kept in sync by hand (it's
# a readonly inside deploy.sh, not something a subprocess-driven test can
# read back). Used by the "gives up after the budget" test below to
# assert the exact describe count.
NETWORK_CHECK_MAX_ATTEMPTS=12
# deploy.sh's NETWORK_CHECK_RETRY_SECS is overridable via this exact env
# var precisely so this file's network-check tests (which otherwise
# sleep through the real ~60s budget) run fast. Only affects the sleep
# *duration* between retries, never the attempt count or the retry
# logic itself -- every test where the network is found on the first
# describe (the common case throughout this suite) never sleeps at all,
# override or not.
export SCION_TEST_NETWORK_RETRY_SECS=0.2

# hardened_config_json HUB — a minimal, valid deploy.sh config: fields
# match deploy-config.example.json, with image source "build" (the
# default, needs no registry path) so a create-mode run never needs one.
hardened_config_json() {
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
# create/tag, router, NAT, both firewall rules, both service accounts) has
# run to completion, since deploy.sh runs strictly sequentially and the
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

# run_deploy_create_foreground CONFIG_JSON — runs deploy.sh (create mode)
# to completion in the foreground, for scenarios expected to exit quickly
# on their own (the fail-fast default-network / default-subnet checks),
# with no background process or sentinel-polling needed. Sets DEPLOY_RC
# and DEPLOY_LOG.
run_deploy_create_foreground() {
  local config_json="$1" config_file
  config_file="$(mktemp)"
  printf '%s' "$config_json" > "$config_file"
  DEPLOY_LOG="$(bash "$DEPLOY_SH" --config "$config_file" --version v1.0.0-test < /dev/null 2>&1)"
  DEPLOY_RC=$?
  rm -f "$config_file"
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
# Missing default VPC network: fail fast, before creating anything
# =====================================================================

test_hardened_org_missing_default_network_fails_before_creating_anything() {
  fresh_gcloud_state
  set_network_missing "default"
  run_deploy_create_foreground "$(hardened_config_json "$HUB")"

  assert_true "$([[ "$DEPLOY_RC" -ne 0 ]] && echo true || echo false)" \
    "deploy.sh must exit non-zero when the default network is missing"
  assert_contains "$DEPLOY_LOG" "No 'default' VPC network found" \
    "deploy.sh should explain that the default network is missing"

  local log
  log="$(gcloud_log)"
  for pattern in \
    "^iam service-accounts create" \
    "^compute routers create" \
    "^compute routers nats create" \
    "^compute firewall-rules create" \
    "^compute instances create"; do
    assert_eq "0" "$(echo "$log" | grep -c -- "$pattern" || true)" \
      "no resource creation call (${pattern}) may happen before the default-network check"
  done
}

test_hardened_org_present_default_network_does_not_block() {
  fresh_gcloud_state
  run_deploy_create "$(hardened_config_json "$HUB")"
  assert_contains "$DEPLOY_LOG" "Default VPC network found." \
    "deploy.sh should confirm the default network when it exists (the realistic, non-hardened-org default)"
}

# The network check must run AFTER services enable, not before: on a
# brand-new project in an ordinary org, compute.googleapis.com has never
# been enabled yet, and enabling it is what triggers the default
# network's creation. Checking first would report a false "missing
# network" on every fresh project (R1).
test_hardened_org_network_check_runs_after_services_enable() {
  fresh_gcloud_state
  run_deploy_create "$(hardened_config_json "$HUB")"
  local log services_enable_ln network_check_ln
  log="$(gcloud_log)"

  services_enable_ln="$(line_number "services enable " "$log")"
  network_check_ln="$(line_number "compute networks describe default" "$log")"
  assert_true "$([[ -n "$services_enable_ln" && -n "$network_check_ln" && "$services_enable_ln" -lt "$network_check_ln" ]] && echo true || echo false)" \
    "services enable must be called before the default-network check"

  # No hub *resource* may be created in between (the IAP service-identity
  # call that legitimately sits here per O1 is an idempotent, no-teardown
  # provisioning step, not a resource this contract cares about).
  local between pattern
  between="$(echo "$log" | sed -n "$((services_enable_ln + 1)),$((network_check_ln - 1))p")"
  for pattern in \
    "^iam service-accounts create" \
    "^compute routers create" \
    "^compute routers nats create" \
    "^compute firewall-rules create" \
    "^compute instances create"; do
    assert_not_contains "$between" "$(echo "$pattern" | tr -d '^')" \
      "no resource creation call (${pattern}) may happen between services enable and the network check"
  done
}

# A genuine non-"not found" error (permission denied, a transient API
# error, ...) must surface immediately with the real gcloud error text,
# not be treated as "network missing" and retried or mapped onto the
# hardened-org message (R1).
test_hardened_org_network_check_other_error_surfaces_immediately_not_retried() {
  fresh_gcloud_state
  set_network_describe_error "default" "gcloud-stub: ERROR: (gcloud.compute.networks.describe) PERMISSION_DENIED: simulated permission error"
  run_deploy_create_foreground "$(hardened_config_json "$HUB")"

  assert_true "$([[ "$DEPLOY_RC" -ne 0 ]] && echo true || echo false)" \
    "deploy.sh must exit non-zero on a genuine describe error"
  assert_contains "$DEPLOY_LOG" "Could not verify the default VPC network" \
    "deploy.sh should report this as a verification failure, not a missing network"
  assert_contains "$DEPLOY_LOG" "PERMISSION_DENIED: simulated permission error" \
    "deploy.sh should print the real gcloud error text"
  assert_not_contains "$DEPLOY_LOG" "compute.skipDefaultNetworkCreation" \
    "a permission error must not be mapped onto the hardened-org missing-network message"
  assert_eq "1" "$(gcloud_log | grep -c '^compute networks describe default' || true)" \
    "a non-'not found' error must not be retried"
}

# The network genuinely appears partway through the retry budget (the
# realistic async-propagation case R1 exists for), rather than being
# either immediately present or permanently absent.
test_hardened_org_network_check_retries_then_succeeds_within_budget() {
  fresh_gcloud_state
  set_network_appears_after "default" 2
  run_deploy_create "$(hardened_config_json "$HUB")"

  assert_eq "3" "$(gcloud_log | grep -c '^compute networks describe default' || true)" \
    "the check must retry exactly until the network appears: 2 misses plus the hit that succeeds"
  assert_contains "$DEPLOY_LOG" "Default VPC network found." \
    "deploy.sh must proceed once the network appears within the retry budget"
}

# The network never appears at all -- the check must give up after
# exactly NETWORK_CHECK_MAX_ATTEMPTS describes, not sooner (which would
# under-tolerate real propagation delay) or keep retrying forever (which
# would hang a genuinely hardened-org deploy).
test_hardened_org_network_check_gives_up_after_exactly_the_budget() {
  fresh_gcloud_state
  set_network_appears_after "default" 999
  run_deploy_create_foreground "$(hardened_config_json "$HUB")"

  assert_true "$([[ "$DEPLOY_RC" -ne 0 ]] && echo true || echo false)" \
    "deploy.sh must give up once the retry budget is exhausted"
  assert_contains "$DEPLOY_LOG" "No 'default' VPC network found" \
    "a network that never appears is reported the same way as one that's simply absent"
  assert_eq "$NETWORK_CHECK_MAX_ATTEMPTS" "$(gcloud_log | grep -c '^compute networks describe default' || true)" \
    "the check must stop after exactly NETWORK_CHECK_MAX_ATTEMPTS describes"
}

# N1/C2: a SERVICE_DISABLED error, in its realistic full shape (message
# plus a details block), must be retried the same way a plain not-found
# is -- compute.googleapis.com itself can still be settling right after
# being enabled, on a brand-new project -- not treated as a non-retryable
# "other error". See the two tests below for the other two shapes
# (bare-message-only, and the synthetic reason-only case), each isolating
# one of the two match conditions.
test_hardened_org_network_check_retries_on_service_disabled_too() {
  fresh_gcloud_state
  set_network_appears_after_service_disabled "default" 2
  run_deploy_create "$(hardened_config_json "$HUB")"

  assert_eq "3" "$(gcloud_log | grep -c '^compute networks describe default' || true)" \
    "a SERVICE_DISABLED error must be retried until the network appears, same as a not-found"
  assert_contains "$DEPLOY_LOG" "Default VPC network found." \
    "deploy.sh must proceed once the network appears within the retry budget"
}

# The prompt-disabled shape (bare message, no details block, so
# "SERVICE_DISABLED" itself is absent) must retry on the message fragment
# alone. Isolated from the full-shape test above so a mutation that only
# breaks the fragment match (and not the SERVICE_DISABLED match) is still
# caught: the full-shape fixture has both substrings, so it can't tell
# the two match arms apart on its own.
test_hardened_org_network_check_retries_on_service_disabled_bare_message() {
  fresh_gcloud_state
  set_network_appears_after_service_disabled "default" 2 "service-disabled-bare-message"
  run_deploy_create "$(hardened_config_json "$HUB")"

  assert_eq "3" "$(gcloud_log | grep -c '^compute networks describe default' || true)" \
    "the bare-message SERVICE_DISABLED shape (no details block) must still be retried on the message fragment alone"
  assert_contains "$DEPLOY_LOG" "Default VPC network found." \
    "deploy.sh must proceed once the network appears within the retry budget"
}

# The mirror of the above: a synthetic shape with only the machine-
# readable "reason: SERVICE_DISABLED" and none of the message text,
# isolating the other match arm. Not a shape real gcloud is known to
# produce for this call (see the stub's own comment) -- included so the
# SERVICE_DISABLED match itself stays meaningfully tested, not only ever
# exercised alongside the message fragment.
test_hardened_org_network_check_retries_on_service_disabled_reason_only() {
  fresh_gcloud_state
  set_network_appears_after_service_disabled "default" 2 "service-disabled-reason-only"
  run_deploy_create "$(hardened_config_json "$HUB")"

  assert_eq "3" "$(gcloud_log | grep -c '^compute networks describe default' || true)" \
    "a reason-only SERVICE_DISABLED shape (no message fragment) must still be retried on the reason match alone"
  assert_contains "$DEPLOY_LOG" "Default VPC network found." \
    "deploy.sh must proceed once the network appears within the retry budget"
}

# C2: without --quiet, gcloud may ask on stderr whether to enable the API
# and then read stdin. stderr is captured here, so on an interactive
# terminal the deploy would appear to freeze at this step waiting for an
# answer the operator can't see (an open, non-TTY stdin blocks the same
# way). --quiet takes the prompt's default answer immediately instead.
test_hardened_org_network_check_uses_quiet() {
  fresh_gcloud_state
  run_deploy_create "$(hardened_config_json "$HUB")"

  local describe_line
  describe_line="$(gcloud_log | grep '^compute networks describe default' | head -1)"
  assert_contains "$describe_line" "--quiet" \
    "the network describe call must pass --quiet so an API-enablement prompt can't hang the deploy"
}

# N5-e: shared by every test below that needs to pin exactly what
# interval the network-check retry loop passes to `sleep`, without
# actually waiting on it. Places a logging `sleep` shim first on PATH for
# one foreground, synchronous deploy.sh run, with $@ passed through to
# `env` ahead of the command (e.g. an override such as
# "SCION_TEST_NETWORK_RETRY_SECS=61", or "-u SCION_TEST_NETWORK_RETRY_SECS"
# to unset it). Sets DEPLOY_LOG, DEPLOY_RC, and SLEEP_SHIM_FIRST_LINE (the
# first logged `sleep` call's argument, empty if `sleep` was never
# called), and cleans up its own temp files before returning.
run_deploy_with_sleep_shim() {
  local sleep_shim_dir sleep_log config_file
  sleep_shim_dir="$(mktemp -d)"
  sleep_log="$(mktemp)"
  cat > "${sleep_shim_dir}/sleep" <<'SHIMEOF'
#!/usr/bin/env bash
echo "$*" >> "$SLEEP_SHIM_LOG"
exit 0
SHIMEOF
  chmod +x "${sleep_shim_dir}/sleep"

  config_file="$(mktemp)"
  printf '%s' "$(hardened_config_json "$HUB")" > "$config_file"
  DEPLOY_LOG="$(env "$@" \
    SLEEP_SHIM_LOG="$sleep_log" \
    PATH="${sleep_shim_dir}:${PATH}" \
    bash "$DEPLOY_SH" --config "$config_file" --version v1.0.0-test < /dev/null 2>&1)"
  DEPLOY_RC=$?
  rm -f "$config_file"

  SLEEP_SHIM_FIRST_LINE="$(head -1 "$sleep_log" 2>/dev/null)"
  rm -rf "$sleep_shim_dir"
  rm -f "$sleep_log"
}

# C1: nothing else in this file exercises the *production* default retry
# interval -- every other test in this file overrides it via
# SCION_TEST_NETWORK_RETRY_SECS (see the top of this file) for speed.
# Pin the default itself with the override explicitly unset.
test_hardened_org_network_check_default_retry_interval_is_5_seconds() {
  fresh_gcloud_state
  set_network_appears_after "default" 1

  run_deploy_with_sleep_shim -u SCION_TEST_NETWORK_RETRY_SECS

  assert_contains "$DEPLOY_LOG" "Default VPC network found." \
    "the deploy must still succeed once the network appears within the retry budget"
  assert_eq "5" "$SLEEP_SHIM_FIRST_LINE" \
    "the production default retry interval must be exactly 5 seconds when SCION_TEST_NETWORK_RETRY_SECS is unset"
}

# N4-b/N5-d: an invalid override must warn and fall back to the
# production default (5s) rather than reaching `sleep` verbatim and
# killing the deploy with a bare "invalid time interval" under set -e.
# "x5" (not "abc") specifically pins the regex's leading `^` anchor:
# without it, "x5" would still match "[0-9]+(\.[0-9]+)?$" (the trailing
# "5"), be treated as valid, and then fail arithmetic differently instead
# of being rejected here as invalid.
test_hardened_org_network_check_invalid_retry_secs_falls_back_to_default() {
  fresh_gcloud_state
  set_network_appears_after "default" 1

  run_deploy_with_sleep_shim SCION_TEST_NETWORK_RETRY_SECS=x5

  assert_contains "$DEPLOY_LOG" "Default VPC network found." \
    "the deploy must still succeed once the network appears within the retry budget"
  assert_contains "$DEPLOY_LOG" "not a valid non-negative number" \
    "an invalid SCION_TEST_NETWORK_RETRY_SECS must warn"
  assert_eq "5" "$SLEEP_SHIM_FIRST_LINE" \
    "an invalid override must fall back to the 5 second default, not reach sleep verbatim"
}

# N4-b/N5-b: an implausibly large override (e.g. a typo adding stray
# digits) must also be capped, so a huge value can't turn the retry loop
# into an effective hang. 18446744073709551650 (2^64 + 34) specifically
# pins the integer-part length guard that runs before the numeric
# comparison: without that guard, this value overflows bash's 64-bit
# arithmetic and wraps to 34, which is <= 60 and would be wrongly
# accepted. A merely-long value like a string of 9s would still be
# rejected by the numeric comparison alone (it doesn't wrap below 60), so
# it would not catch a dropped length guard.
test_hardened_org_network_check_oversized_retry_secs_falls_back_to_default() {
  fresh_gcloud_state
  set_network_appears_after "default" 1

  run_deploy_with_sleep_shim SCION_TEST_NETWORK_RETRY_SECS=18446744073709551650

  assert_contains "$DEPLOY_LOG" "Default VPC network found." \
    "the deploy must still succeed once the network appears within the retry budget"
  assert_contains "$DEPLOY_LOG" "out of range" \
    "an oversized SCION_TEST_NETWORK_RETRY_SECS must warn"
  assert_eq "5" "$SLEEP_SHIM_FIRST_LINE" \
    "an oversized override must fall back to the 5 second default, not reach sleep verbatim"
}

# N5-a: pin the accept/reject boundary at exactly 60, so a mutation that
# drops or loosens the ">60" comparison (while leaving the length guard
# alone) is still caught.
test_hardened_org_network_check_retry_secs_60_is_accepted() {
  fresh_gcloud_state
  set_network_appears_after "default" 1

  run_deploy_with_sleep_shim SCION_TEST_NETWORK_RETRY_SECS=60

  assert_contains "$DEPLOY_LOG" "Default VPC network found." \
    "the deploy must still succeed once the network appears within the retry budget"
  assert_not_contains "$DEPLOY_LOG" "out of range" \
    "60 is within the cap and must not warn"
  assert_eq "60" "$SLEEP_SHIM_FIRST_LINE" \
    "60 is within the cap and must be used as given, not replaced by the default"
}

test_hardened_org_network_check_retry_secs_61_is_rejected() {
  fresh_gcloud_state
  set_network_appears_after "default" 1

  run_deploy_with_sleep_shim SCION_TEST_NETWORK_RETRY_SECS=61

  assert_contains "$DEPLOY_LOG" "Default VPC network found." \
    "the deploy must still succeed once the network appears within the retry budget"
  assert_contains "$DEPLOY_LOG" "out of range" \
    "61 is over the cap and must warn"
  assert_eq "5" "$SLEEP_SHIM_FIRST_LINE" \
    "61 is over the cap and must fall back to the 5 second default"
}

# R5-1: a leading zero must not be misread as octal by the cap's
# arithmetic comparison (bash's `[[ -gt ]]` treats a leading-zero operand
# as octal unless the base is forced), which would otherwise raise a
# "value too great for base" error on the retry path and let the value
# through unvalidated instead of failing cleanly.
test_hardened_org_network_check_retry_secs_leading_zero_is_not_octal() {
  fresh_gcloud_state
  set_network_appears_after "default" 1

  run_deploy_with_sleep_shim SCION_TEST_NETWORK_RETRY_SECS=08

  assert_contains "$DEPLOY_LOG" "Default VPC network found." \
    "the deploy must still succeed once the network appears within the retry budget"
  assert_not_contains "$DEPLOY_LOG" "value too great for base" \
    "a leading zero must not be misread as an octal literal"
  assert_eq "08" "$SLEEP_SHIM_FIRST_LINE" \
    "08 is within the cap and must be used as given, not replaced by the default"
}

# =====================================================================
# Missing "default" subnet in the target region: fail fast before the VM
# =====================================================================

test_hardened_org_missing_default_subnet_fails_before_vm_create() {
  fresh_gcloud_state
  set_subnet_missing "default"
  run_deploy_create_foreground "$(hardened_config_json "$HUB")"

  assert_true "$([[ "$DEPLOY_RC" -ne 0 ]] && echo true || echo false)" \
    "deploy.sh must exit non-zero when the default subnet cannot be found"
  assert_contains "$DEPLOY_LOG" "Could not determine the IP range of the 'default' subnet" \
    "deploy.sh should explain that the default subnet's CIDR could not be determined"

  local log
  log="$(gcloud_log)"
  assert_eq "0" "$(echo "$log" | grep -c '^compute instances create' || true)" \
    "the VM must never be created when the default subnet's CIDR could not be determined"
  assert_eq "0" "$(echo "$log" | grep -c "^compute firewall-rules create ${FW_8080_RULE_NAME} " || true)" \
    "the proxy-to-VM firewall rule must never be created without a real CIDR to scope it to"
}

# =====================================================================
# Scoped tcp:8080 proxy-to-VM firewall rule
# =====================================================================

test_hardened_org_8080_rule_created_scoped_on_fresh_create() {
  fresh_gcloud_state
  run_deploy_create "$(hardened_config_json "$HUB")"
  local log fw_line
  log="$(gcloud_log)"

  assert_eq "1" "$(echo "$log" | grep -c "^compute firewall-rules create ${FW_8080_RULE_NAME} " || true)" \
    "a fresh create must create exactly one proxy-to-VM firewall rule"
  fw_line="$(echo "$log" | grep "^compute firewall-rules create ${FW_8080_RULE_NAME} " | head -1)"
  assert_contains "$fw_line" "--rules=tcp:8080" \
    "the proxy-to-VM rule must allow only tcp:8080"
  assert_contains "$fw_line" "--source-ranges=${DEFAULT_SUBNET_CIDR}" \
    "the proxy-to-VM rule must be scoped to the default subnet's own CIDR"
  assert_contains "$fw_line" "--target-tags=${HUB_TAG}" \
    "the proxy-to-VM rule must target only the hub VM's tag"
  assert_contains "$fw_line" "--network=default" \
    "the proxy-to-VM rule must be on the default network"
  assert_contains "$fw_line" "--description=" \
    "the proxy-to-VM rule must carry a description, following the #1979 rule style"

  # Must never fall back to the broad default-allow-internal rule.
  assert_eq "0" "$(echo "$log" | grep -c '^compute firewall-rules create default-allow-internal' || true)" \
    "deploy.sh must never create the broad default-allow-internal rule"
}

# Uses a deliberately non-default-looking CIDR (the stub's own default is
# already a realistic-looking value, "10.128.0.0/20", which a hardcoded
# constant in deploy.sh could accidentally match) to prove the rule's
# source range actually comes from the subnet lookup, not a literal.
test_hardened_org_8080_rule_uses_actual_subnet_cidr_not_a_hardcoded_value() {
  fresh_gcloud_state
  set_subnet_cidr "default" "10.77.4.0/22"
  run_deploy_create "$(hardened_config_json "$HUB")"
  local log fw_line
  log="$(gcloud_log)"

  fw_line="$(echo "$log" | grep "^compute firewall-rules create ${FW_8080_RULE_NAME} " | head -1)"
  assert_contains "$fw_line" "--source-ranges=10.77.4.0/22" \
    "the proxy-to-VM rule's source range must track the actual subnet CIDR, not a hardcoded default"
}

test_hardened_org_8080_rule_idempotent_rerun() {
  fresh_gcloud_state
  run_deploy_create "$(hardened_config_json "$HUB")"
  run_deploy_create "$(hardened_config_json "$HUB")"
  local log
  log="$(gcloud_log)"

  assert_eq "1" "$(echo "$log" | grep -c "^compute firewall-rules create ${FW_8080_RULE_NAME} " || true)" \
    "a re-run against an already-existing proxy-to-VM rule must not create a second one"
  assert_contains "$DEPLOY_LOG" "Firewall rule already exists: ${FW_8080_RULE_NAME}" \
    "deploy.sh should report the proxy-to-VM rule as already existing on the second run"
  # One combined describe per run (not-found on the first, exists-with-all-
  # three-fields-in-one-call on the second) -- not the four separate calls
  # (one existence check plus three per-field reads) this used to take.
  assert_eq "2" "$(echo "$log" | grep -c "^compute firewall-rules describe ${FW_8080_RULE_NAME} " || true)" \
    "each run must describe the proxy-to-VM rule exactly once, not once for existence plus once per drift field"
}

test_hardened_org_8080_rule_source_drift_warns_without_auto_update() {
  fresh_gcloud_state
  seed_firewall_rule_json "$FW_8080_RULE_NAME" \
    "Allow the Cloud Run IAP proxy (Direct VPC egress) to reach the Scion Hub VM on 8080" \
    "default" "INGRESS" "ALLOW" "tcp" "8080" "" "10.999.0.0/20" "${HUB_TAG}" "1000"
  run_deploy_create "$(hardened_config_json "$HUB")"
  local log
  log="$(gcloud_log)"

  assert_eq "0" "$(echo "$log" | grep -c "^compute firewall-rules create ${FW_8080_RULE_NAME} " || true)" \
    "an existing proxy-to-VM rule must not also be created"
  assert_eq "0" "$(echo "$log" | grep -c "^compute firewall-rules update ${FW_8080_RULE_NAME} " || true)" \
    "a drifted proxy-to-VM rule must not be auto-updated"
  assert_contains "$DEPLOY_LOG" "drifted from what this script expects" \
    "deploy.sh should warn that the existing rule has drifted"
  assert_contains "$DEPLOY_LOG" "source: 10.999.0.0/20" \
    "the warning should show the actual (drifted) source range"
}

# The drift check must also catch a rule whose port/protocol or target
# tags no longer match, not just its source range (O4): a rule
# hand-edited to a different port, or retargeted to a different tag, is
# just as broken for the proxy as a wrong source range would be.
# Tag-only drift: source range and port/protocol both match what
# deploy.sh expects, only the target tag has drifted. Isolated from the
# port-drift test below so neither can hide a broken comparison for the
# other field (a combined "port AND tag" fixture would still trigger the
# warning even if only one of the two comparisons actually worked).
test_hardened_org_8080_rule_drift_detects_tag_only() {
  fresh_gcloud_state
  seed_firewall_rule_json "$FW_8080_RULE_NAME" \
    "Allow the Cloud Run IAP proxy (Direct VPC egress) to reach the Scion Hub VM on 8080" \
    "default" "INGRESS" "ALLOW" "tcp" "8080" "" "${DEFAULT_SUBNET_CIDR}" "some-other-tag" "1000"
  run_deploy_create "$(hardened_config_json "$HUB")"
  local log
  log="$(gcloud_log)"

  assert_eq "0" "$(echo "$log" | grep -c "^compute firewall-rules update ${FW_8080_RULE_NAME} " || true)" \
    "a drifted proxy-to-VM rule must not be auto-updated"
  assert_contains "$DEPLOY_LOG" "drifted from what this script expects" \
    "deploy.sh should warn even though only the target tag (not the source range or port) drifted"
  assert_contains "$DEPLOY_LOG" "target tags: some-other-tag" \
    "the warning should show the actual (wrong) target tag"
}

# Port-only drift: source range and target tag both match, only the
# allowed port/protocol has drifted.
test_hardened_org_8080_rule_drift_detects_port_only() {
  fresh_gcloud_state
  seed_firewall_rule_json "$FW_8080_RULE_NAME" \
    "Allow the Cloud Run IAP proxy (Direct VPC egress) to reach the Scion Hub VM on 8080" \
    "default" "INGRESS" "ALLOW" "tcp" "9090" "" "${DEFAULT_SUBNET_CIDR}" "${HUB_TAG}" "1000"
  run_deploy_create "$(hardened_config_json "$HUB")"
  local log
  log="$(gcloud_log)"

  assert_eq "0" "$(echo "$log" | grep -c "^compute firewall-rules update ${FW_8080_RULE_NAME} " || true)" \
    "a drifted proxy-to-VM rule must not be auto-updated"
  assert_contains "$DEPLOY_LOG" "drifted from what this script expects" \
    "deploy.sh should warn even though only the allowed port (not the source range or target tag) drifted"
  assert_contains "$DEPLOY_LOG" "allowed: tcp:9090" \
    "the warning should show the actual (wrong) allowed port"
}

# An empty sourceRanges (an unscoped or hand-edited rule) is one shape
# that exercises deploy.sh's own reason for not splitting the combined
# describe with `IFS=$'\t' read`: bash's `read` collapses a leading empty
# field on a whitespace IFS character like tab instead of leaving it
# blank, which would shift every field after it and mislabel them in the
# warning below (though the drift *decision* itself would still come out
# right either way, since no real field value is empty).
test_hardened_org_8080_rule_drift_with_empty_source_range_labels_fields_correctly() {
  fresh_gcloud_state
  seed_firewall_rule_json "$FW_8080_RULE_NAME" \
    "Allow the Cloud Run IAP proxy (Direct VPC egress) to reach the Scion Hub VM on 8080" \
    "default" "INGRESS" "ALLOW" "tcp" "8080" "" "" "${HUB_TAG}" "1000"
  run_deploy_create "$(hardened_config_json "$HUB")"

  assert_contains "$DEPLOY_LOG" "drifted from what this script expects" \
    "deploy.sh should warn when the existing rule has no source range at all"
  assert_contains "$DEPLOY_LOG" "source: , allowed: tcp:8080, target tags: ${HUB_TAG}" \
    "an empty sourceRanges must not shift the allowed-ports and target-tags fields into its place"
}

# =====================================================================
# Teardown deletes the proxy-to-VM firewall rule
# =====================================================================

test_hardened_org_teardown_deletes_8080_rule() {
  fresh_gcloud_state
  run_deploy_create "$(hardened_config_json "$HUB")"
  run_deploy_delete "$(hardened_config_json "$HUB")"

  assert_eq "0" "$DEPLOY_RC" "teardown of everything create just made should exit 0"

  local log
  log="$(gcloud_log)"
  assert_eq "1" "$(echo "$log" | grep -c -- "^compute firewall-rules delete ${FW_8080_RULE_NAME} " || true)" \
    "teardown must delete the proxy-to-VM firewall rule exactly once"
  assert_false "$([[ -f "${GCLOUD_STUB_STATE_DIR}/firewall-rules/${FW_8080_RULE_NAME}.json" ]] && echo true)" \
    "the proxy-to-VM firewall-rule fixture should be gone after teardown"
  assert_contains "$DEPLOY_LOG" "Deleted firewall rule:      ${FW_8080_RULE_NAME}" \
    "the summary must report the proxy-to-VM firewall rule as deleted"
}
