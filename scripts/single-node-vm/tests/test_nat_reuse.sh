# scripts/single-node-vm/tests/test_nat_reuse.sh — Cloud NAT reuse
# detection (ptone/scion#2003 / #2004): deploy.sh must not die when a Cloud
# NAT gateway already exists on the VM's network+region.
#
# GCP's actual rule is stricter than "one all-subnets NAT per
# network+region": an ALL_SUBNETWORKS_* gateway (all subnets' full IP
# ranges, or all subnets' primary ranges) cannot coexist with ANY other NAT
# gateway on that network+region, not just one that already covers our
# subnet. deploy.sh therefore has three outcomes, decided before the
# service account, IAM bindings, or its own router/NAT are created:
#   1. no gateway on this network+region at all -> create ours, all-subnets
#   2. some gateway already covers our subnet ("default") -> reuse it
#   3. some gateway exists but doesn't cover "default" -> create ours,
#      scoped to just that subnet (--nat-custom-subnet-ip-ranges=default)
# plus fail-closed behavior when the detection itself can't be trusted.
#
# This file has no shebang -- it is always `source`d, never executed --
# so shellcheck needs an explicit shell directive to know its dialect.
# shellcheck shell=bash

DEPLOY_SH="${TIER_DIR}/deploy.sh"
HUB="natreuse"
REGION="us-central1"
ROUTER_NAME="scion-hub-${HUB}-router"
NAT_NAME="scion-hub-${HUB}-nat"

# nat_reuse_config_json HUB — same shape as test_deploy_base.sh's
# base_config_json, private to this file per the isolation rule (README:
# "a test file may use only lib/harness.sh's shared helpers plus what it
# defines itself").
nat_reuse_config_json() {
  local hub="$1"
  cat <<EOF
{
  "hub_name": "${hub}",
  "project_id": "demo-project",
  "region": "${REGION}",
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

# _router_json ROUTER_NAME NAT_NAME NETWORK_SUFFIX REGION SRT
#   [SUBNETWORKS_JSON [NAT_TYPE]] — one routers-list.json entry with a
# single NAT. SUBNETWORKS_JSON, if given, is embedded verbatim as the
# NAT's "subnetworks" array. NAT_TYPE, if given, is embedded as the NAT's
# "type" field (e.g. "PRIVATE"); omitted by default, matching a real
# PUBLIC NAT's response shape (deploy.sh's own jq defaults a missing type
# to PUBLIC).
_router_json() {
  local router_name="$1" nat_name="$2" network_suffix="$3" region="$4" srt="$5" subnetworks="${6:-}" nat_type="${7:-}"
  local subnetworks_field="" type_field=""
  if [[ -n "$subnetworks" ]]; then
    subnetworks_field=",\"subnetworks\":${subnetworks}"
  fi
  if [[ -n "$nat_type" ]]; then
    type_field=",\"type\":\"${nat_type}\""
  fi
  cat <<EOF
{"name":"${router_name}",
 "region":"https://www.googleapis.com/compute/v1/projects/demo-project/regions/${region}",
 "network":"https://www.googleapis.com/compute/v1/projects/demo-project/global/networks/${network_suffix}",
 "nats":[{"name":"${nat_name}","sourceSubnetworkIpRangesToNat":"${srt}"${subnetworks_field}${type_field}}]}
EOF
}

# _start_deploy_bg / _stop_deploy_bg / run_deploy_create: same pattern as
# test_deploy_base.sh's (private per-file copy -- see isolation rule
# above). Runs deploy.sh (create mode) in the background and stops it once
# the `phase2-complete` sentinel appears, by which point Phase 2 (service
# account, IAM, router, NAT, firewall rule) has run to completion.
_start_deploy_bg() {
  local config_file="$1" log_file="$2"
  set -m
  bash "$DEPLOY_SH" --config "$config_file" --version v1.0.0-test \
    < /dev/null > "$log_file" 2>&1 &
  _DEPLOY_BG_PID=$!
  set +m
}

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

# run_deploy_create_sync CONFIG_JSON — for the fail-closed tests below,
# which exit before Phase 2 (some before any prompt or gcloud call at
# all) and so never reach the `phase2-complete` sentinel: a plain
# foreground run is enough, no background process or polling needed.
run_deploy_create_sync() {
  local config_json="$1" config_file
  config_file="$(mktemp)"
  printf '%s' "$config_json" > "$config_file"
  DEPLOY_LOG="$(bash "$DEPLOY_SH" --config "$config_file" --version v1.0.0-test < /dev/null 2>&1)"
  DEPLOY_RC=$?
  rm -f "$config_file"
}

# run_deploy_delete CONFIG_JSON — mirrors test_deploy_base.sh's helper of
# the same name (private per-file copy; --delete never blocks).
run_deploy_delete() {
  local config_json="$1" config_file
  config_file="$(mktemp)"
  printf '%s' "$config_json" > "$config_file"
  DEPLOY_LOG="$(bash "$DEPLOY_SH" --delete --config "$config_file" < /dev/null 2>&1)"
  DEPLOY_RC=$?
  rm -f "$config_file"
}

# _dir_without_jq DEST — populates DEST with a symlink to every real
# executable found in $HARNESS_SAFE_PATH (harness.sh's PATH snapshot from
# before any test could touch it), except any file literally named "jq".
# Used to simulate jq being absent for a single subprocess invocation
# without disturbing $PATH for any other test (each test_* function
# already runs in its own subshell -- see tests/README.md "How it
# works" -- so a local PATH= assignment here never leaks).
_dir_without_jq() {
  local dest="$1" d f base
  local IFS=':'
  for d in $HARNESS_SAFE_PATH; do
    [[ -d "$d" ]] || continue
    for f in "$d"/*; do
      [[ -e "$f" ]] || continue
      base="${f##*/}"
      [[ "$base" == "jq" ]] && continue
      [[ -e "${dest}/${base}" ]] && continue
      ln -s "$f" "${dest}/${base}" 2>/dev/null || true
    done
  done
}

# =====================================================================
# Positive paths: no gateway, and a gateway that already covers us
# (reuse) in both ALL_SUBNETWORKS_* modes (R1).
# =====================================================================

test_nat_reuse_no_gateway_creates_our_own_all_subnets_nat() {
  fresh_gcloud_state
  run_deploy_create "$(nat_reuse_config_json "$HUB")"
  local log
  log="$(gcloud_log)"

  assert_eq "1" "$(echo "$log" | grep -c "^compute routers nats create ${NAT_NAME} " || true)" \
    "must create exactly one Cloud NAT when no gateway exists on this network+region"
  local nat_line
  nat_line="$(echo "$log" | grep "^compute routers nats create ${NAT_NAME} " | head -1)"
  assert_contains "$nat_line" "--nat-all-subnet-ip-ranges" \
    "with no existing gateway, our own NAT must use all-subnets mode"
  assert_not_contains "$nat_line" "--nat-custom-subnet-ip-ranges" \
    "with no existing gateway, our own NAT must not be scoped"
}

test_nat_reuse_foreign_all_ip_ranges_nat_is_reused() {
  fresh_gcloud_state
  seed_routers_list "[$(_router_json "other-team-router" "other-team-nat" "default" "$REGION" "ALL_SUBNETWORKS_ALL_IP_RANGES")]"
  run_deploy_create "$(nat_reuse_config_json "$HUB")"
  local log
  log="$(gcloud_log)"

  assert_eq "0" "$(echo "$log" | grep -c '^compute routers create' || true)" \
    "a foreign all-subnets NAT must be reused, not duplicated with our own router"
  assert_eq "0" "$(echo "$log" | grep -c '^compute routers nats create' || true)" \
    "a foreign all-subnets NAT must be reused, not duplicated with our own NAT"
  assert_contains "$DEPLOY_LOG" "Reusing Cloud Router: other-team-router" \
    "deploy.sh must log which router it's reusing"
  assert_contains "$DEPLOY_LOG" "Reusing Cloud NAT:    other-team-nat" \
    "deploy.sh must log which NAT it's reusing"
}

# R1: ALL_SUBNETWORKS_ALL_PRIMARY_IP_RANGES (a normal Console choice, "all
# subnets' primary ranges only") already covers the VM's primary IP and
# must be detected exactly like ALL_SUBNETWORKS_ALL_IP_RANGES above.
test_nat_reuse_foreign_all_primary_ip_ranges_nat_is_reused() {
  fresh_gcloud_state
  seed_routers_list "[$(_router_json "other-team-router" "other-team-nat" "default" "$REGION" "ALL_SUBNETWORKS_ALL_PRIMARY_IP_RANGES")]"
  run_deploy_create "$(nat_reuse_config_json "$HUB")"
  local log
  log="$(gcloud_log)"

  assert_eq "0" "$(echo "$log" | grep -c '^compute routers create' || true)" \
    "an ALL_SUBNETWORKS_ALL_PRIMARY_IP_RANGES NAT must be reused too"
  assert_eq "0" "$(echo "$log" | grep -c '^compute routers nats create' || true)" \
    "an ALL_SUBNETWORKS_ALL_PRIMARY_IP_RANGES NAT must be reused, not duplicated"
  assert_contains "$DEPLOY_LOG" "Reusing Cloud NAT:    other-team-nat" \
    "deploy.sh must report reusing the ALL_PRIMARY NAT"
}

# A LIST_OF_SUBNETWORKS entry (not just an ALL_SUBNETWORKS_* NAT) that
# explicitly forwards "default"'s primary range also covers us and must be
# reused -- this is the positive side of $listCovers; the only other LIST
# test (secondary_only, below) exercises just the negative side, so
# without this one, $listCovers could be hardcoded to `false` and every
# test would still pass (round-2 review, RR1).
test_nat_reuse_foreign_list_entry_covering_default_primary_is_reused() {
  fresh_gcloud_state
  local covering_default='[{"name":"https://www.googleapis.com/compute/v1/projects/demo-project/regions/us-central1/subnetworks/default","sourceIpRangesToNat":["PRIMARY_IP_RANGE"]}]'
  seed_routers_list "[$(_router_json "other-team-router" "other-team-nat" "default" "$REGION" "LIST_OF_SUBNETWORKS" "$covering_default")]"
  run_deploy_create "$(nat_reuse_config_json "$HUB")"
  local log
  log="$(gcloud_log)"

  assert_eq "0" "$(echo "$log" | grep -c '^compute routers create' || true)" \
    "a LIST_OF_SUBNETWORKS entry that forwards 'default's primary range must be reused, not duplicated"
  assert_eq "0" "$(echo "$log" | grep -c '^compute routers nats create' || true)" \
    "a LIST_OF_SUBNETWORKS entry that forwards 'default's primary range must be reused, not duplicated"
  assert_contains "$DEPLOY_LOG" "Reusing Cloud NAT:    other-team-nat" \
    "deploy.sh must report reusing the LIST-mode NAT"
}

# A covering NAT that isn't the first row jq emits must still be found --
# without scanning every row, only the first router/NAT in the list would
# ever be considered (round-2 review, RR1).
test_nat_reuse_covering_nat_on_second_router_is_reused() {
  fresh_gcloud_state
  local other_subnetworks='[{"name":"https://www.googleapis.com/compute/v1/projects/demo-project/regions/us-central1/subnetworks/other-subnet","sourceIpRangesToNat":["ALL_IP_RANGES"]}]'
  local first second
  first="$(_router_json "first-router" "first-nat" "default" "$REGION" "LIST_OF_SUBNETWORKS" "$other_subnetworks")"
  second="$(_router_json "second-router" "second-nat" "default" "$REGION" "ALL_SUBNETWORKS_ALL_IP_RANGES")"
  seed_routers_list "[${first},${second}]"
  run_deploy_create "$(nat_reuse_config_json "$HUB")"
  local log
  log="$(gcloud_log)"

  assert_eq "0" "$(echo "$log" | grep -c '^compute routers create' || true)" \
    "the covering NAT on the second router must be reused, not shadowed by the first (non-covering) one"
  assert_eq "0" "$(echo "$log" | grep -c '^compute routers nats create' || true)" \
    "the covering NAT on the second router must be reused, not shadowed by the first (non-covering) one"
  assert_contains "$DEPLOY_LOG" "Reusing Cloud Router: second-router" \
    "must pick the router whose NAT actually covers us, not just the first one listed"
  assert_contains "$DEPLOY_LOG" "Reusing Cloud NAT:    second-nat" \
    "must pick the NAT that actually covers us, not just the first one listed"
}

# N1: a Private NAT (NCC/hybrid connectivity, not internet egress) must
# never be treated as covering our subnet, and must not even count as "a
# gateway exists" for the scoped-create decision -- a Private and a
# PUBLIC NAT can coexist on the same subnet, so its presence must not
# change what deploy.sh creates at all.
test_nat_reuse_private_nat_is_ignored_entirely() {
  fresh_gcloud_state
  seed_routers_list "[$(_router_json "other-team-router" "other-team-nat" "default" "$REGION" "ALL_SUBNETWORKS_ALL_IP_RANGES" "" "PRIVATE")]"
  run_deploy_create "$(nat_reuse_config_json "$HUB")"
  local log nat_line
  log="$(gcloud_log)"
  nat_line="$(echo "$log" | grep "^compute routers nats create ${NAT_NAME} " | head -1)"

  assert_not_contains "$DEPLOY_LOG" "Reusing Cloud NAT" \
    "a Private NAT must never be reused for internet egress"
  assert_eq "1" "$(echo "$log" | grep -c "^compute routers create ${ROUTER_NAME} " || true)" \
    "our own router must still be created; a Private NAT elsewhere doesn't block us"
  assert_contains "$nat_line" "--nat-all-subnet-ip-ranges" \
    "a Private NAT must not even count as 'a gateway exists' -- our own NAT stays all-subnets, not scoped"
}

# =====================================================================
# R2: a foreign gateway that does NOT cover our subnet still blocks an
# all-subnets create -- our own NAT must be created scoped to just
# "default" instead.
# =====================================================================

test_nat_reuse_foreign_noncovering_nat_creates_scoped_nat() {
  fresh_gcloud_state
  local other_subnetworks='[{"name":"https://www.googleapis.com/compute/v1/projects/demo-project/regions/us-central1/subnetworks/other-subnet","sourceIpRangesToNat":["ALL_IP_RANGES"]}]'
  seed_routers_list "[$(_router_json "other-team-router" "other-team-nat" "default" "$REGION" "LIST_OF_SUBNETWORKS" "$other_subnetworks")]"
  run_deploy_create "$(nat_reuse_config_json "$HUB")"
  local log nat_line
  log="$(gcloud_log)"
  nat_line="$(echo "$log" | grep "^compute routers nats create ${NAT_NAME} " | head -1)"

  assert_eq "1" "$(echo "$log" | grep -c "^compute routers create ${ROUTER_NAME} " || true)" \
    "our own router must still be created (the foreign gateway is on a different router)"
  assert_contains "$nat_line" "--nat-custom-subnet-ip-ranges=default" \
    "with a foreign non-covering gateway present, our NAT must be scoped to subnet 'default'"
  assert_not_contains "$nat_line" "--nat-all-subnet-ip-ranges" \
    "an all-subnets NAT can't coexist with the foreign gateway, so it must not be requested"
}

# O1: a LIST_OF_SUBNETWORKS entry for "default" whose sourceIpRangesToNat
# only forwards secondary ranges does NOT give the VM's primary IP egress,
# so it must not count as coverage -- this is the same "foreign gateway
# exists, doesn't cover us" outcome as the test above, just reached via a
# different (mis)configuration.
test_nat_reuse_secondary_only_list_entry_is_not_coverage() {
  fresh_gcloud_state
  local secondary_only_default='[{"name":"https://www.googleapis.com/compute/v1/projects/demo-project/regions/us-central1/subnetworks/default","sourceIpRangesToNat":["LIST_OF_SECONDARY_IP_RANGES"]}]'
  seed_routers_list "[$(_router_json "other-team-router" "other-team-nat" "default" "$REGION" "LIST_OF_SUBNETWORKS" "$secondary_only_default")]"
  run_deploy_create "$(nat_reuse_config_json "$HUB")"
  local log nat_line
  log="$(gcloud_log)"
  nat_line="$(echo "$log" | grep "^compute routers nats create ${NAT_NAME} " | head -1)"

  assert_not_contains "$DEPLOY_LOG" "Reusing Cloud NAT" \
    "a secondary-ranges-only entry for 'default' must not be treated as reuse coverage"
  assert_contains "$nat_line" "--nat-custom-subnet-ip-ranges=default" \
    "must fall back to a scoped create, the same as any other non-covering foreign gateway"
}

# =====================================================================
# R3: network/region must be checked exactly -- a foreign all-subnets NAT
# on a different network or region must never be reused (or even count as
# "a gateway exists" for the scoped-create decision).
# =====================================================================

test_nat_reuse_wrong_network_default_vpc_is_not_reused() {
  fresh_gcloud_state
  seed_routers_list "[$(_router_json "other-team-router" "other-team-nat" "default-vpc" "$REGION" "ALL_SUBNETWORKS_ALL_IP_RANGES")]"
  run_deploy_create "$(nat_reuse_config_json "$HUB")"
  local log nat_line
  log="$(gcloud_log)"
  nat_line="$(echo "$log" | grep "^compute routers nats create ${NAT_NAME} " | head -1)"

  assert_not_contains "$DEPLOY_LOG" "Reusing Cloud NAT" \
    "an all-subnets NAT on network 'default-vpc' must not be mistaken for one on 'default'"
  assert_eq "1" "$(echo "$log" | grep -c "^compute routers create ${ROUTER_NAME} " || true)" \
    "our own router must be created since the foreign one is on a different network"
  assert_contains "$nat_line" "--nat-all-subnet-ip-ranges" \
    "a gateway on a different network doesn't block our all-subnets NAT"
}

test_nat_reuse_wrong_region_is_not_reused() {
  fresh_gcloud_state
  seed_routers_list "[$(_router_json "other-team-router" "other-team-nat" "default" "us-east1" "ALL_SUBNETWORKS_ALL_IP_RANGES")]"
  run_deploy_create "$(nat_reuse_config_json "$HUB")"
  local log nat_line
  log="$(gcloud_log)"
  nat_line="$(echo "$log" | grep "^compute routers nats create ${NAT_NAME} " | head -1)"

  assert_not_contains "$DEPLOY_LOG" "Reusing Cloud NAT" \
    "an all-subnets NAT in a different region must not be mistaken for one in ours"
  assert_eq "1" "$(echo "$log" | grep -c "^compute routers create ${ROUTER_NAME} " || true)" \
    "our own router must be created since the foreign one is in a different region"
  assert_contains "$nat_line" "--nat-all-subnet-ip-ranges" \
    "a gateway in a different region doesn't block our all-subnets NAT"
}

# =====================================================================
# Our own router/NAT, seeded into the routers-list fixture (as a real
# `gcloud compute routers list` would show them after a prior run),
# must be recognized as ours -- not mistaken for a foreign gateway to
# reuse or to scope around.
# =====================================================================

test_nat_reuse_own_router_in_list_is_not_treated_as_foreign() {
  fresh_gcloud_state
  seed_routers_list "[$(_router_json "$ROUTER_NAME" "$NAT_NAME" "default" "$REGION" "ALL_SUBNETWORKS_ALL_IP_RANGES")]"
  set_router_exists
  set_nat_exists "$NAT_NAME"
  run_deploy_create "$(nat_reuse_config_json "$HUB")"
  local log
  log="$(gcloud_log)"

  assert_eq "0" "$(echo "$log" | grep -c '^compute routers create' || true)" \
    "our own already-existing router must not be re-created"
  assert_eq "0" "$(echo "$log" | grep -c '^compute routers nats create' || true)" \
    "our own already-existing NAT must not be re-created"
  assert_contains "$DEPLOY_LOG" "Cloud Router already exists: ${ROUTER_NAME}" \
    "must go through the normal idempotent describe-by-name path"
  assert_contains "$DEPLOY_LOG" "Cloud NAT already exists: ${NAT_NAME}" \
    "must go through the normal idempotent describe-by-name path"
  assert_not_contains "$DEPLOY_LOG" "Reusing Cloud" \
    "our own router/NAT appearing in the list must never be logged as 'reusing' someone else's"
}

# =====================================================================
# R4: fail-closed paths. Each must abort before the service account (or
# any other Phase 2 resource) is created.
# =====================================================================

test_nat_reuse_missing_jq_fails_before_any_gcloud_call() {
  fresh_gcloud_state
  local no_jq_dir
  no_jq_dir="$(mktemp -d)"
  _dir_without_jq "$no_jq_dir"

  local config_file
  config_file="$(mktemp)"
  printf '%s' "$(nat_reuse_config_json "$HUB")" > "$config_file"

  local out rc
  out="$(PATH="$no_jq_dir" bash "$DEPLOY_SH" --config "$config_file" --version v1.0.0-test < /dev/null 2>&1)"
  rc=$?
  rm -f "$config_file"
  rm -rf "$no_jq_dir"

  assert_eq "1" "$rc" "deploy.sh must exit non-zero when jq is missing"
  assert_contains "$out" "jq is required" "must report the missing jq prerequisite"
  assert_eq "0" "$(gcloud_call_count)" \
    "no gcloud call at all should happen before the Phase 1 jq check"
}

test_nat_reuse_routers_list_failure_fails_closed_before_sa() {
  fresh_gcloud_state
  set_routers_list_will_fail
  run_deploy_create_sync "$(nat_reuse_config_json "$HUB")"

  assert_eq "1" "$DEPLOY_RC" "deploy.sh must exit non-zero when the routers list call fails"
  assert_contains "$DEPLOY_LOG" "Could not list Cloud Routers" \
    "must report the list failure"
  assert_contains "$DEPLOY_LOG" "gcloud-stub: simulated routers list failure" \
    "the underlying gcloud error must surface, not be discarded"
  assert_eq "0" "$(gcloud_log | grep -c '^iam service-accounts create' || true)" \
    "must not create a service account when the NAT check fails closed"
  assert_eq "0" "$(gcloud_log | grep -c '^compute routers create' || true)" \
    "must not create a router when the NAT check fails closed"
}

test_nat_reuse_routers_list_empty_output_fails_closed() {
  fresh_gcloud_state
  set_routers_list_returns_empty
  run_deploy_create_sync "$(nat_reuse_config_json "$HUB")"

  assert_eq "1" "$DEPLOY_RC" \
    "deploy.sh must treat an empty (but exit-0) routers list response as a failure, not as 'no routers'"
  assert_contains "$DEPLOY_LOG" "Cloud Router list returned no output" \
    "must report the empty-output case specifically"
  assert_eq "0" "$(gcloud_log | grep -c '^iam service-accounts create' || true)" \
    "must not create a service account when the NAT check fails closed"
  assert_eq "0" "$(gcloud_log | grep -c '^compute routers create' || true)" \
    "must not create a router when the NAT check fails closed"
}

# jq itself failing to parse the routers-list response (truncated/invalid
# JSON -- a realistic shape for a transient API hiccup or an SDK-version
# mismatch) must fail closed the same way a list-command failure does, not
# silently fall through with an empty NAT_ROWS (which would read as "no
# gateway" and create an all-subnets NAT unconditionally -- round-2
# review, RR1).
test_nat_reuse_unparsable_routers_list_fails_closed_before_sa() {
  fresh_gcloud_state
  seed_routers_list '{"truncated":'
  run_deploy_create_sync "$(nat_reuse_config_json "$HUB")"

  assert_eq "1" "$DEPLOY_RC" "deploy.sh must exit non-zero when the routers list output can't be parsed"
  assert_contains "$DEPLOY_LOG" "Could not parse Cloud Router/NAT config" \
    "must report the parse failure"
  assert_eq "0" "$(gcloud_log | grep -c '^iam service-accounts create' || true)" \
    "must not create a service account when the NAT check fails closed"
  assert_eq "0" "$(gcloud_log | grep -c '^compute routers create' || true)" \
    "must not create a router when the NAT check fails closed"
}

# =====================================================================
# --delete must never delete a reused (foreign) NAT/router.
# =====================================================================

test_nat_reuse_delete_never_deletes_a_reused_nat_or_router() {
  fresh_gcloud_state
  seed_routers_list "[$(_router_json "other-team-router" "other-team-nat" "default" "$REGION" "ALL_SUBNETWORKS_ALL_IP_RANGES")]"
  run_deploy_create "$(nat_reuse_config_json "$HUB")"
  run_deploy_delete "$(nat_reuse_config_json "$HUB")"

  assert_eq "0" "$DEPLOY_RC" "teardown must still exit 0 even though our own NAT/router were never created"
  local log
  log="$(gcloud_log)"
  assert_eq "1" "$(echo "$log" | grep -c "^compute routers nats delete ${NAT_NAME} " || true)" \
    "teardown must attempt to delete our own (never-created) NAT name"
  assert_eq "1" "$(echo "$log" | grep -c "^compute routers delete ${ROUTER_NAME} " || true)" \
    "teardown must attempt to delete our own (never-created) router name"
  assert_eq "0" "$(echo "$log" | grep -c 'other-team' || true)" \
    "teardown must never reference the reused foreign router/NAT by name"
}
