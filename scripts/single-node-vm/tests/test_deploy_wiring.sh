# scripts/single-node-vm/tests/test_deploy_wiring.sh — tests that run
# deploy.sh itself as a real subprocess against the stub `gcloud` on
# PATH, to cover the wiring between deploy.sh and hybrid-tier.sh that a
# function-level test (test_hybrid_tier.sh) cannot reach: whether
# deploy.sh actually calls hybrid_discover before its first resource
# create after the VPC check, gates
# the hybrid-tier network tag (on VM create and add-tags) on
# HYBRID_ENABLED, and puts the ownership check before any delete in
# --delete mode. No test here contacts GCP.
#
# This file has no shebang -- it is always `source`d, never executed --
# so shellcheck needs an explicit shell directive to know its dialect.
# shellcheck shell=bash
#
# Driving deploy.sh: each test writes a JSON config file and runs
# `bash deploy.sh --config FILE [--delete]` as a real subprocess, with
# PATH already pointing at the stub gcloud (run.sh exports it) and stdin
# redirected from /dev/null so deploy.sh's own `[[ ! -t 0 ]]` checks take
# the non-interactive branch. --delete-mode tests run to completion in
# well under a second, since nothing in that path sleeps or retries.

DEPLOY_SH="${TIER_DIR}/deploy.sh"
HUB="demohub"
MARKER="scion-deployment=${HUB}"
INSTANCE_NAME="scion-hub-${HUB}"

# base_config_json HUB [GKE_TARGET_JSON_FRAGMENT]
#
# GKE_TARGET_JSON_FRAGMENT, if given, must be a leading-comma JSON
# fragment like `, "gke_target": {...}` to splice in before the closing
# brace.
# IMAGE_SOURCE/IMAGE_REGISTRY default to "build"/"" (the pre-existing
# fixture shape); tier-on create tests must override to "registry" plus
# a real-looking path, since the tier now refuses source=build.
base_config_json() {
  local hub="$1" extra="${2:-}" image_source="${3:-build}" image_registry="${4:-}"
  cat <<EOF
{
  "hub_name": "${hub}",
  "project_id": "demo-project",
  "region": "us-central1",
  "machine_size": "small",
  "disk_size_gb": 200,
  "chat_plugins": [],
  "container_images": {"source": "${image_source}", "registry": "${image_registry}", "force_rebuild": false},
  "admin_email": "admin@example.com",
  "update_policy": "auto",
  "release_channel": "nightly"${extra}
}
EOF
}

hybrid_config_fragment() {
  echo ", \"gke_target\": {\"name\": \"mycluster\", \"location\": \"us-central1\", \"project\": \"demo-project\"}"
}

# run_deploy_delete CONFIG_JSON — runs `deploy.sh --delete` to completion
# (nothing in that path sleeps or retries). Sets DEPLOY_RC and DEPLOY_LOG.
# Never touches the caller's shell options: the subprocess's own exit
# status is captured through `$?` right after the command substitution,
# with no `set +e`/`set -e` pair of our own that could leak -- run.sh
# deliberately runs every test without -e, and a stray `set -e` left
# behind here would turn it on for the rest of the test, so a later
# command that fails as an ordinary, expected non-zero result (a `grep`
# with no match, say) would kill the test's subshell before any
# assertion ran.
run_deploy_delete() {
  local config_json="$1" config_file
  config_file="$(mktemp)"
  printf '%s' "$config_json" > "$config_file"
  DEPLOY_LOG="$(bash "$DEPLOY_SH" --delete --config "$config_file" < /dev/null 2>&1)"
  DEPLOY_RC=$?
  rm -f "$config_file"
}

# run_deploy_delete_interactive PYTHON_OVERRIDE STDIN_TEXT — runs
# `deploy.sh --delete` with no --config at all, piping STDIN_TEXT to
# answer its interactive prompts (hub name, region, the confirmation).
# Unlike run_deploy_delete, deploy.sh never touches $PYTHON on this path
# before the teardown ownership check: --config is what triggers the
# unconditional Python preflight and config_get's own use
# of $PYTHON, and neither runs here. PYTHON_OVERRIDE, if non-empty, is
# exported for deploy.sh's own process only, to simulate a missing
# interpreter; it does not affect the stub, which reads
# GCLOUD_STUB_PYTHON instead (see tests/lib/gcloud) for exactly this
# reason. Same no-stray-set-e discipline as run_deploy_delete above.
run_deploy_delete_interactive() {
  local python_override="$1" stdin_text="$2"
  if [[ -n "$python_override" ]]; then
    DEPLOY_LOG="$(PYTHON="$python_override" bash "$DEPLOY_SH" --delete <<<"$stdin_text" 2>&1)"
  else
    DEPLOY_LOG="$(bash "$DEPLOY_SH" --delete <<<"$stdin_text" 2>&1)"
  fi
  DEPLOY_RC=$?
}

# _start_deploy_bg CONFIG_FILE LOG_FILE [ENV_ASSIGNMENT...] -- starts
# deploy.sh (create mode) in the background, in its own process group
# (job control on for this call only), and sets the global _DEPLOY_BG_PID
# to its PID. `set -m` makes bash give a backgrounded job its own process
# group, with the PGID equal to the job's own PID -- that's what lets
# _stop_deploy_bg below signal every process deploy.sh has spawned, not
# just deploy.sh itself. _DEPLOY_BG_PID is a global, not an echoed return
# value: a caller doing `pid="$(_start_deploy_bg ...)"` would run this
# whole function in a command-substitution subshell, and a subshell that
# enabled job control sends SIGHUP to its own background jobs the moment
# it exits -- which happens immediately here, right after backgrounding,
# killing deploy.sh before it ever gets going. A plain function call has
# no such subshell, so callers must call this directly, then read
# _DEPLOY_BG_PID afterward, not wrap the call in `$(...)`.
#
# Also snapshots $TMPDIR's own top-level entries to LOG_FILE.before,
# before deploy.sh ever runs, so _stop_deploy_bg below can tell what it
# left behind directly in $TMPDIR -- deploy.sh does its own mktemp-based
# error-file bookkeeping internally (SQUASH_SSH_ERR and friends), each a
# bare file alongside (not inside) $GCLOUD_STUB_STATE_DIR, with cleanup
# later in its own linear flow; killed mid-flight, whichever of those it
# was between creating and removing at that moment stays on disk. Scoped
# to top-level entries only (not a recursive find): the stub's own state
# directories legitimately gain new files while deploy.sh runs (sentinels,
# JSON fixtures) that the calling test still needs to read after this
# returns, so descending into them and diffing would delete state the
# test hasn't finished using yet, not just orphaned garbage. Same
# snapshot-diff technique run_expect_fail uses for the analogous
# command-substitution-subshell case; written to a file rather than a
# variable for the same subshell-visibility reason _DEPLOY_BG_PID is a
# global.
_start_deploy_bg() {
  local config_file="$1" log_file="$2"
  shift 2
  PATH="$HARNESS_SAFE_PATH" find "${TMPDIR:-/tmp}" -mindepth 1 -maxdepth 1 2>/dev/null \
    | PATH="$HARNESS_SAFE_PATH" sort > "${log_file}.before"
  local -a launcher=()
  if [[ "${DEPLOY_BG_DEFAULT_SIGINT:-}" == "true" ]]; then
    # A shell cannot un-ignore a signal that was ignored when it started,
    # and a suite launched in the background from a non-interactive shell
    # inherits SIGINT ignored. Reset SIGINT to its default before exec so
    # a SIGINT test does not depend on how the suite was started.
    launcher=("$PYTHON" -c 'import os, signal, sys; signal.signal(signal.SIGINT, signal.SIG_DFL); os.execvp(sys.argv[1], sys.argv[1:])')
  fi
  set -m
  ${launcher[@]+"${launcher[@]}"} env "$@" bash "$DEPLOY_SH" --config "$config_file" --version v1.0.0-test \
    < /dev/null > "$log_file" 2>&1 &
  _DEPLOY_BG_PID=$!
  set +m
}

# _stop_deploy_bg PID LOG_FILE -- sends SIGTERM to PID's entire process
# group (not just PID itself), so an in-flight child the stub gcloud
# spawns dies alongside deploy.sh instead of continuing to run -- and
# possibly write into this test's just-removed state directory -- after
# this function returns. Reaps PID via `wait` (bash can only wait on its
# own direct children) and sets DEPLOY_RC to its exit status, then polls
# `kill -0` on the group until every member is actually gone, up to a
# bounded safety timeout, since SIGTERM delivery and process teardown
# aren't instantaneous even once sent. Finally sweeps $TMPDIR for
# anything new since _start_deploy_bg's snapshot (LOG_FILE.before) and
# removes it -- deploy.sh's own temp files, not just a stub subprocess's.
_stop_deploy_bg() {
  local pid="$1" log_file="$2"
  kill -TERM -- "-${pid}" 2>/dev/null || true
  wait "$pid" 2>/dev/null
  DEPLOY_RC=$?
  local waited_ms=0
  while kill -0 -- "-${pid}" 2>/dev/null && [[ "$waited_ms" -lt 5000 ]]; do
    sleep 0.05
    waited_ms=$((waited_ms + 50))
  done
  local before_tmp after_tmp
  before_tmp="$(cat "${log_file}.before" 2>/dev/null || true)"
  after_tmp="$(PATH="$HARNESS_SAFE_PATH" find "${TMPDIR:-/tmp}" -mindepth 1 -maxdepth 1 2>/dev/null | PATH="$HARNESS_SAFE_PATH" sort)"
  rm -f "${log_file}.before"
  if [[ "$after_tmp" != "$before_tmp" ]]; then
    PATH="$HARNESS_SAFE_PATH" comm -13 <(printf '%s\n' "$before_tmp") <(printf '%s\n' "$after_tmp") | while IFS= read -r _new_tmp_entry; do
      [[ -n "$_new_tmp_entry" ]] && rm -rf "$_new_tmp_entry"
    done
  fi
}

# run_deploy_create CONFIG_JSON — runs `deploy.sh` (create mode) in the
# background and stops it once the `phase2-complete` sentinel appears
# (touched by the stub's `compute ssh` handler on its first invocation --
# see tests/lib/gcloud): by that point VM create/tag and the internal-IP
# calls and guard have run. The wait uses a 30s budget --
# large enough to absorb realistic per-call latency on a loaded host --
# and fails the whole call loudly (returns 1, with the partial log
# annotated) if the sentinel is never reached at all, rather than
# silently letting the caller assert against a partial log as if nothing
# were wrong. A test that needs to know one specific, later call has
# completed (past Phase 2, e.g. into the settings.yaml writes) should use
# run_deploy_create_wait_for below instead, which waits on a
# call-specific sentinel. Sets DEPLOY_RC and DEPLOY_LOG. Same
# no-stray-set-e discipline as run_deploy_delete above.
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
    sleep 0.1
    waited_ms=$((waited_ms + 100))
  done
  if [[ ! -f "$sentinel" ]]; then
    _stop_deploy_bg "$pid" "$log_file"
    DEPLOY_RC=1
    DEPLOY_LOG="$(cat "$log_file")
FATAL: run_deploy_create: sentinel '${sentinel}' was never reached within 30000ms -- deploy.sh may be stuck, or this test's expectations no longer match its actual call sequence"
    rm -f "$config_file" "$log_file"
    return 1
  fi
  _stop_deploy_bg "$pid" "$log_file"
  DEPLOY_LOG="$(cat "$log_file")"
  rm -f "$config_file" "$log_file"
}

# run_deploy_create_wait_for CONFIG_JSON SENTINEL_FILENAME — like
# run_deploy_create, but waits on SENTINEL_FILENAME (relative to
# $GCLOUD_STUB_STATE_DIR) instead of phase2-complete, with the same 30s
# fail-loud budget.
run_deploy_create_wait_for() {
  local config_json="$1" sentinel_filename="$2" config_file
  config_file="$(mktemp)"
  printf '%s' "$config_json" > "$config_file"
  local sentinel="${GCLOUD_STUB_STATE_DIR}/${sentinel_filename}"
  rm -f "$sentinel"
  local log_file
  log_file="$(mktemp)"
  local pid
  _start_deploy_bg "$config_file" "$log_file"
  pid="$_DEPLOY_BG_PID"
  local waited_ms=0
  while [[ ! -f "$sentinel" && "$waited_ms" -lt 30000 ]]; do
    sleep 0.1
    waited_ms=$((waited_ms + 100))
  done
  if [[ ! -f "$sentinel" ]]; then
    _stop_deploy_bg "$pid" "$log_file"
    DEPLOY_RC=1
    DEPLOY_LOG="$(cat "$log_file")
FATAL: run_deploy_create_wait_for: sentinel '${sentinel}' was never reached within 30000ms -- deploy.sh may be stuck, or this test's expectations no longer match its actual call sequence"
    rm -f "$config_file" "$log_file"
    return 1
  fi
  _stop_deploy_bg "$pid" "$log_file"
  DEPLOY_LOG="$(cat "$log_file")"
  rm -f "$config_file" "$log_file"
}

# run_deploy_create_to_settings_yaml CONFIG_JSON — like run_deploy_create,
# but opts the stub `gcloud` into actually succeeding on `compute ssh`/
# `compute scp` (GCLOUD_STUB_SSH_SUCCEEDS=true) and polls for a LATER
# sentinel: the settings.yaml dev-mode write in Phase 3, which is the
# earliest point past VM creation where deploy.sh has already made the
# hybrid-tier's NFS squash-identity and export SSH calls (if the tier is
# on) and computed HYBRID_SHARED_DIR_STORAGE_YAML, but before Phase 3b's
# image build/push work (which this stub does not simulate at all).
# Every "compute ssh"/"compute scp" call the stub actually receives is
# already in $GCLOUD_STUB_LOG (gcloud_log), one call per line including
# its full --command= text, so assertions on the rendered NFS/export
# scripts and the settings.yaml heredoc read that log directly rather
# than needing a separate one.
run_deploy_create_to_settings_yaml() {
  _run_deploy_create_to_settings_yaml_impl "$1" "dev"
}

# run_deploy_create_to_proxy_settings_yaml CONFIG_JSON — like
# run_deploy_create_to_settings_yaml, but runs all the way through
# Cloud Run deploy and IAP enablement to the Phase-5 proxy-mode
# settings.yaml (re)write instead of stopping at the earlier dev-mode
# one. IAP_ENFORCEMENT_WAIT_SECS is overridden to 0 so this doesn't
# block on deploy.sh's real 60s wait for IAP enforcement to activate.
run_deploy_create_to_proxy_settings_yaml() {
  _run_deploy_create_to_settings_yaml_impl "$1" "proxy"
}

# _run_deploy_create_to_settings_yaml_impl CONFIG_JSON MODE ("dev" or
# "proxy") — shared implementation. Sets DEPLOY_RC, DEPLOY_LOG, and
# DEPLOY_REACHED_SETTINGS_YAML. Waits up to 30 s for the dev-mode write
# and 120 s for the proxy-mode write, which comes after several times as
# many gcloud calls (a tier-on run through Phase 5 makes the most); set
# DEPLOY_TEST_BUDGET_MS to override both on a slow machine. The wait ends
# as soon as the sentinel appears, so a larger budget costs nothing on a
# passing run.
_run_deploy_create_to_settings_yaml_impl() {
  local config_json="$1" mode="$2" config_file budget_ms
  config_file="$(mktemp)"
  printf '%s' "$config_json" > "$config_file"
  local sentinel
  if [[ "$mode" == "proxy" ]]; then
    sentinel="${GCLOUD_STUB_STATE_DIR}/settings-yaml-proxy-mode-written"
    budget_ms="${DEPLOY_TEST_BUDGET_MS:-120000}"
  else
    sentinel="${GCLOUD_STUB_STATE_DIR}/settings-yaml-dev-mode-written"
    budget_ms="${DEPLOY_TEST_BUDGET_MS:-30000}"
  fi
  rm -f "$sentinel"
  local log_file
  log_file="$(mktemp)"
  local pid
  _start_deploy_bg "$config_file" "$log_file" \
    GCLOUD_STUB_SSH_SUCCEEDS=true IAP_ENFORCEMENT_WAIT_SECS=0
  pid="$_DEPLOY_BG_PID"
  local waited_ms=0
  while [[ ! -f "$sentinel" && "$waited_ms" -lt "$budget_ms" ]]; do
    sleep 0.1
    waited_ms=$((waited_ms + 100))
  done
  # No quiescence poll: the settings.yaml sentinel fires only once the
  # full --command= string (the whole heredoc) has already been logged
  # by the SSH dispatcher, so there's nothing later this call needs to
  # wait for -- stopping here also keeps a dev-mode run from ever
  # reaching the later proxy-mode write, which is the only thing that
  # made the two writes distinguishable by log content rather than by
  # which one this call actually asked for.
  _stop_deploy_bg "$pid" "$log_file"
  DEPLOY_LOG="$(cat "$log_file")"
  DEPLOY_REACHED_SETTINGS_YAML="$([[ -f "$sentinel" ]] && echo true || echo false)"
  rm -f "$config_file" "$log_file"
}

# run_deploy_create_to_cloud_run_deploy CONFIG_JSON — extends past the
# settings.yaml stopping point above, all the way into Phase 4's Cloud
# Run IAP proxy deploy (`gcloud beta run deploy`). The stub logs every
# invocation unconditionally before dispatching on it (see
# tests/lib/gcloud's header), so as soon as that call appears in
# $GCLOUD_STUB_LOG, its full argv (label args and all) is already
# there. That makes the log itself the sentinel: no separate touched
# file is needed, and deploy.sh is stopped as soon as the line appears.
run_deploy_create_to_cloud_run_deploy() {
  local config_json="$1" config_file
  config_file="$(mktemp)"
  printf '%s' "$config_json" > "$config_file"
  local log_file
  log_file="$(mktemp)"
  local pid
  _start_deploy_bg "$config_file" "$log_file" GCLOUD_STUB_SSH_SUCCEEDS=true
  pid="$_DEPLOY_BG_PID"
  local waited_ms=0
  while ! grep -q '^beta run deploy ' "$GCLOUD_STUB_LOG" 2>/dev/null && [[ "$waited_ms" -lt 30000 ]]; do
    sleep 0.1
    waited_ms=$((waited_ms + 100))
  done
  if ! grep -q '^beta run deploy ' "$GCLOUD_STUB_LOG" 2>/dev/null; then
    _stop_deploy_bg "$pid" "$log_file"
    DEPLOY_RC=1
    DEPLOY_LOG="$(cat "$log_file")
FATAL: run_deploy_create_to_cloud_run_deploy: 'beta run deploy' was never logged within 30000ms"
    rm -f "$config_file" "$log_file"
    return 1
  fi
  # No quiescence poll: the tests using this helper read only the
  # deploy call's own logged argv, which is complete once it appears.
  _stop_deploy_bg "$pid" "$log_file"
  DEPLOY_LOG="$(cat "$log_file")"
  rm -f "$config_file" "$log_file"
}

# The systemd unit install ('compute ssh ... tee ... scion-hub.service')
# runs in Phase 3, well before the Cloud Run deploy call this helper
# stops at, so by the time run_deploy_create_to_cloud_run_deploy returns,
# the unit's full rendered content -- read verbatim off disk by deploy.sh
# via `$(cat config-templates/scion-hub.service)` -- is already in
# gcloud_log. A daemon started with no WorkingDirectory= inherits
# whatever cwd its supervisor happens to have (systemd defaults to "/"),
# which is not a writable or meaningful location for the scion user's
# on-disk operational state; the unit must pin it to that user's home
# directory instead.
test_deploy_create_systemd_unit_has_working_directory() {
  fresh_gcloud_state
  run_deploy_create_to_cloud_run_deploy "$(base_config_json "$HUB")"
  local log
  log="$(gcloud_log)"
  # Anchored to the whole line (grep -cx), not a substring match: a
  # substring check here would still pass with the line commented out
  # (# WorkingDirectory=/home/scion) or with a typo'd path
  # (WorkingDirectory=/home/scion/typo), neither of which actually sets the
  # unit's working directory.
  assert_eq "1" "$(printf '%s\n' "$log" | grep -cx 'WorkingDirectory=/home/scion' || true)" \
    "the installed scion-hub.service unit must set WorkingDirectory=/home/scion, matching its User=scion, so the Hub daemon's working directory is never /"
}

# =====================================================================
# --delete wiring
# =====================================================================

test_deploy_delete_invalid_hub_name_refused() {
  fresh_gcloud_state
  run_deploy_delete "$(base_config_json "Not_A_Valid_Hub_Name")"
  assert_eq "1" "$DEPLOY_RC" "an invalid hub name must be refused on the --delete path too"
  assert_eq "0" "$(gcloud_log | grep -c . || true)" "an invalid hub name must be refused before any gcloud call"
}

test_deploy_delete_tier_off_no_rules_exits_zero() {
  fresh_gcloud_state
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_eq "0" "$DEPLOY_RC" "tier-off teardown with no existing hybrid rules should exit 0"
}

test_deploy_delete_unmarked_exits_nonzero_before_any_delete() {
  fresh_gcloud_state
  seed_firewall_rule_desc_only "scion-hub-${HUB}-nfs-allow" "unrelated-rule-not-ours"
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_eq "1" "$DEPLOY_RC" "an unmarked hybrid rule name match must fail the teardown run"
  assert_eq "0" "$(gcloud_log | grep -c ' delete' || true)" \
    "no delete call of any kind should be logged before the abort"
}

test_deploy_delete_hybrid_list_before_any_delete() {
  fresh_gcloud_state
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-allow" "scion-deployment=${HUB}" \
    "default" "INGRESS" "ALLOW" "tcp" "2049" "gke-x-node" "" "scion-hub-${HUB}-nfs" "900"
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-deny" "scion-deployment=${HUB}" \
    "default" "INGRESS" "DENY" "tcp" "2049" "" "0.0.0.0/0" "scion-hub-${HUB}-nfs" "950"
  run_deploy_delete "$(base_config_json "$HUB")"
  local log list_line delete_line
  log="$(gcloud_log)"
  list_line="$(line_number 'firewall-rules list' "$log")"
  delete_line="$(line_number ' delete' "$log")"
  assert_true "$([[ -n "$list_line" && -n "$delete_line" && "$list_line" -lt "$delete_line" ]] && echo true || echo false)" \
    "the hybrid ownership list call must precede the first delete call"
}

# Re-running teardown after the VM is already gone (no instance seeded,
# so `instances delete` fails as not-found and the project-wide list must
# positively confirm "gone") must actually delete both hybrid rules and
# succeed -- this is the exact recovery path deploy.sh's own "Keeping the
# hybrid-tier firewall rules... Re-run teardown after the VM is
# deleted" message tells the operator to use.
test_deploy_delete_vm_already_gone_rules_deleted() {
  fresh_gcloud_state
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-allow" "scion-deployment=${HUB}" \
    "default" "INGRESS" "ALLOW" "tcp" "2049" "gke-x-node" "" "scion-hub-${HUB}-nfs" "900"
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-deny" "scion-deployment=${HUB}" \
    "default" "INGRESS" "DENY" "tcp" "2049" "" "0.0.0.0/0" "scion-hub-${HUB}-nfs" "950"
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_eq "0" "$DEPLOY_RC" "re-running teardown after the VM is already gone must succeed"
  assert_eq "2" "$(gcloud_log | grep -c "firewall-rules delete scion-hub-${HUB}-nfs-" || true)" \
    "both hybrid rules must be deleted once the VM is positively confirmed gone"
}

test_deploy_delete_hybrid_rules_deleted_after_vm() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "us-central1-b"
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-allow" "scion-deployment=${HUB}" \
    "default" "INGRESS" "ALLOW" "tcp" "2049" "gke-x-node" "" "scion-hub-${HUB}-nfs" "900"
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-deny" "scion-deployment=${HUB}" \
    "default" "INGRESS" "DENY" "tcp" "2049" "" "0.0.0.0/0" "scion-hub-${HUB}-nfs" "950"
  run_deploy_delete "$(base_config_json "$HUB")"
  local log vm_line hybrid_line
  log="$(gcloud_log)"
  vm_line="$(line_number "compute instances delete ${INSTANCE_NAME}" "$log")"
  hybrid_line="$(line_number "firewall-rules delete scion-hub-${HUB}-nfs-" "$log")"
  assert_true "$([[ -n "$vm_line" && -n "$hybrid_line" && "$vm_line" -lt "$hybrid_line" ]] && echo true || echo false)" \
    "the VM delete must precede the hybrid firewall-rule deletes"
}

test_deploy_delete_hybrid_order_allow_before_deny() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "us-central1-b"
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-allow" "scion-deployment=${HUB}" \
    "default" "INGRESS" "ALLOW" "tcp" "2049" "gke-x-node" "" "scion-hub-${HUB}-nfs" "900"
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-deny" "scion-deployment=${HUB}" \
    "default" "INGRESS" "DENY" "tcp" "2049" "" "0.0.0.0/0" "scion-hub-${HUB}-nfs" "950"
  run_deploy_delete "$(base_config_json "$HUB")"
  local log allow_line deny_line
  log="$(gcloud_log)"
  allow_line="$(line_number "firewall-rules delete scion-hub-${HUB}-nfs-allow" "$log")"
  deny_line="$(line_number "firewall-rules delete scion-hub-${HUB}-nfs-deny" "$log")"
  assert_true "$([[ -n "$allow_line" && -n "$deny_line" && "$allow_line" -lt "$deny_line" ]] && echo true || echo false)" \
    "the allow rule must be deleted before the deny rule (an allow must never briefly exist unpaired)"
}

test_deploy_delete_hub_deny_deleted_before_nfs_deny() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "us-central1-b"
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-allow" "scion-deployment=${HUB}" \
    "default" "INGRESS" "ALLOW" "tcp" "2049" "gke-x-node" "" "scion-hub-${HUB}-nfs" "900"
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-deny" "scion-deployment=${HUB}" \
    "default" "INGRESS" "DENY" "tcp" "2049" "" "0.0.0.0/0" "scion-hub-${HUB}-nfs" "950"
  seed_firewall_rule_json "scion-hub-${HUB}-hub-deny" "scion-deployment=${HUB}" \
    "default" "INGRESS" "DENY" "all" "" "" "10.52.0.0/14" "scion-hub-${HUB}-nfs" "950"
  run_deploy_delete "$(base_config_json "$HUB")"
  local log hub_deny_line deny_line
  log="$(gcloud_log)"
  hub_deny_line="$(line_number "firewall-rules delete scion-hub-${HUB}-hub-deny" "$log")"
  deny_line="$(line_number "firewall-rules delete scion-hub-${HUB}-nfs-deny" "$log")"
  assert_true "$([[ -n "$hub_deny_line" && -n "$deny_line" && "$hub_deny_line" -lt "$deny_line" ]] && echo true || echo false)" \
    "the hub-deny rule must be deleted before the nfs-deny rule, the same as the nfs-allow rule"
  assert_eq "1" "$(gcloud_log | grep -c "firewall-rules delete scion-hub-${HUB}-hub-deny" || true)" \
    "the hub-deny rule must actually be deleted during teardown, not just left in place"
}

test_deploy_delete_vm_failure_keeps_hybrid_rules() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "us-central1-b"
  set_instance_delete_will_fail "$INSTANCE_NAME"
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-allow" "scion-deployment=${HUB}" \
    "default" "INGRESS" "ALLOW" "tcp" "2049" "gke-x-node" "" "scion-hub-${HUB}-nfs" "900"
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-deny" "scion-deployment=${HUB}" \
    "default" "INGRESS" "DENY" "tcp" "2049" "" "0.0.0.0/0" "scion-hub-${HUB}-nfs" "950"
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_eq "1" "$DEPLOY_RC" "a VM delete failure must make teardown exit non-zero"
  assert_eq "0" "$(gcloud_log | grep -c "firewall-rules delete scion-hub-${HUB}-nfs-" || true)" \
    "the hybrid rules must not be deleted when the VM delete failed"
  assert_contains "$DEPLOY_LOG" "still exists" "deploy.sh should report why the rules were kept"
  # name=NAME is an equality match on the instance name.
  assert_contains "$(gcloud_log)" \
    "compute instances list --project=demo-project --filter=name=${INSTANCE_NAME} --format=value(name)" \
    "the VM-gone check must list the VM by exact name, in exactly this filter form"
}

test_deploy_delete_list_failure_aborts_before_any_delete() {
  fresh_gcloud_state
  set_firewall_list_will_fail
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_eq "1" "$DEPLOY_RC" "a failed ownership list call must abort teardown"
  assert_eq "0" "$(gcloud_log | grep -c ' delete' || true)" \
    "no delete call of any kind should be logged when the list call itself failed"
}

# Internal IP reservation teardown wiring: the deploy.sh-level equivalent
# of the hybrid_internal_ip_teardown_check/_delete function-level tests
# in test_hybrid_tier.sh, covering the wiring those tests can't reach --
# whether deploy.sh actually calls the check before any delete, gates the
# reservation's delete on VM_GONE, and reports it in the final summary.
test_deploy_delete_internal_ip_unmarked_aborts_before_any_delete() {
  fresh_gcloud_state
  seed_address_unmarked "scion-hub-${HUB}-internal-ip" "10.128.0.42"
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_eq "1" "$DEPLOY_RC" "an unmarked internal-IP reservation name match must fail the teardown run"
  assert_eq "0" "$(gcloud_log | grep -c ' delete' || true)" \
    "no delete call of any kind should be logged before the abort"
}

test_deploy_delete_internal_ip_list_failure_aborts_before_any_delete() {
  fresh_gcloud_state
  set_address_list_will_fail "scion-hub-${HUB}-internal-ip"
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_eq "1" "$DEPLOY_RC" \
    "an unconfirmable internal-IP reservation state must fail the teardown run, not be treated as absent"
  assert_eq "0" "$(gcloud_log | grep -c ' delete' || true)" \
    "no delete call of any kind should be logged before the abort"
}

test_deploy_delete_internal_ip_deleted_when_vm_already_gone() {
  fresh_gcloud_state
  seed_address "scion-hub-${HUB}-internal-ip" "10.128.0.42" "scion-deployment=${HUB}"
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_eq "0" "$DEPLOY_RC" "teardown must succeed once the VM is confirmed gone"
  assert_eq "1" "$(gcloud_log | grep -c "addresses delete scion-hub-${HUB}-internal-ip" || true)" \
    "the internal IP reservation must be deleted once the VM is positively confirmed gone"
  assert_contains "$DEPLOY_LOG" "Deleted internal IP:       scion-hub-${HUB}-internal-ip" \
    "the summary must report the reservation as deleted"
}

test_deploy_delete_internal_ip_delete_failure_reflected_in_exit_code() {
  fresh_gcloud_state
  seed_address "scion-hub-${HUB}-internal-ip" "10.128.0.42" "scion-deployment=${HUB}"
  set_address_delete_will_fail "scion-hub-${HUB}-internal-ip"
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_eq "1" "$DEPLOY_RC" \
    "an internal-IP delete failure, with the VM confirmed gone, must fail the whole teardown's exit code"
  assert_contains "$DEPLOY_LOG" "Kept internal IP:          scion-hub-${HUB}-internal-ip (delete failed:" \
    "the summary must report the reservation as Kept, with the delete-failed reason"
}

test_deploy_delete_internal_ip_kept_when_vm_not_gone() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "us-central1-b"
  set_instance_delete_will_fail "$INSTANCE_NAME"
  seed_address "scion-hub-${HUB}-internal-ip" "10.128.0.42" "scion-deployment=${HUB}"
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_eq "1" "$DEPLOY_RC" "a VM delete failure must make teardown exit non-zero"
  assert_eq "0" "$(gcloud_log | grep -c "addresses delete scion-hub-${HUB}-internal-ip" || true)" \
    "the internal IP reservation must not be deleted when the VM delete failed"
  assert_contains "$DEPLOY_LOG" "isn't confirmed yet" \
    "deploy.sh should report why the reservation was kept"
  assert_contains "$DEPLOY_LOG" "SKIPPED internal IP:       scion-hub-${HUB}-internal-ip (VM not confirmed gone)" \
    "the summary must report the reservation as SKIPPED, distinct from an attempted-and-failed delete"
}

test_deploy_delete_internal_ip_kept_when_firewall_rule_delete_failed() {
  fresh_gcloud_state
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-allow" "scion-deployment=${HUB}" \
    "default" "INGRESS" "ALLOW" "tcp" "2049" "gke-x-node" "" "scion-hub-${HUB}-nfs" "900"
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-deny" "scion-deployment=${HUB}" \
    "default" "INGRESS" "DENY" "tcp" "2049" "" "0.0.0.0/0" "scion-hub-${HUB}-nfs" "950"
  set_firewall_delete_will_fail "scion-hub-${HUB}-nfs-allow"
  seed_address "scion-hub-${HUB}-internal-ip" "10.128.0.42" "scion-deployment=${HUB}"
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_eq "1" "$DEPLOY_RC" "a hybrid firewall rule delete failure must make teardown exit non-zero"
  assert_eq "0" "$(gcloud_log | grep -c "addresses delete scion-hub-${HUB}-internal-ip" || true)" \
    "the internal IP reservation must not be deleted when a hybrid firewall rule failed to delete"
  assert_contains "$DEPLOY_LOG" "SKIPPED internal IP:       scion-hub-${HUB}-internal-ip (a hybrid-tier firewall rule failed to delete)" \
    "the summary must report the reservation as SKIPPED, with the reason it was never attempted"
}

test_deploy_delete_internal_ip_order_after_vm() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "us-central1-b"
  seed_address "scion-hub-${HUB}-internal-ip" "10.128.0.42" "scion-deployment=${HUB}"
  run_deploy_delete "$(base_config_json "$HUB")"
  local log vm_line addr_line
  log="$(gcloud_log)"
  vm_line="$(line_number "compute instances delete ${INSTANCE_NAME}" "$log")"
  addr_line="$(line_number "addresses delete scion-hub-${HUB}-internal-ip" "$log")"
  assert_true "$([[ -n "$vm_line" && -n "$addr_line" && "$vm_line" -lt "$addr_line" ]] && echo true || echo false)" \
    "the VM delete must precede the internal IP reservation delete"
}

# =====================================================================
# Create-mode wiring
# =====================================================================

test_deploy_create_tier_off_no_nfs_tag_no_container_calls() {
  fresh_gcloud_state
  run_deploy_create "$(base_config_json "$HUB")"
  local log create_line
  log="$(gcloud_log)"
  assert_eq "0" "$(echo "$log" | grep -c 'container clusters' || true)" \
    "tier-off create must make zero container/cluster gcloud calls"
  create_line="$(echo "$log" | grep 'compute instances create' | head -1)"
  assert_true "$([[ -n "$create_line" ]] && echo true || echo false)" \
    "the VM create call should have been logged before the run stopped"
  assert_contains "$create_line" "--tags=scion-hub-${HUB} " \
    "tier-off VM create carries only the hub tag in its --tags flag"
  assert_not_contains "$create_line" "scion-hub-${HUB}-nfs" \
    "tier-off VM create must not include the hybrid-tier network tag"
  assert_contains "$create_line" "--machine-type=e2-standard-4" "tier-off create keeps its base machine-type flag"
  assert_contains "$create_line" "--no-address" "tier-off create keeps its base --no-address flag"
  assert_contains "$create_line" "--boot-disk-size=200GB" "tier-off create keeps its base disk-size flag"
}

test_deploy_create_tier_on_tags_new_vm() {
  fresh_gcloud_state
  seed_cluster "mycluster" "default"
  run_deploy_create_wait_for "$(base_config_json "$HUB" "$(hybrid_config_fragment)" "registry" "us-docker.pkg.dev/demo-project/scion")" \
    "instances-create-completed"
  local log create_line
  log="$(gcloud_log)"
  create_line="$(echo "$log" | grep 'compute instances create' | head -1)"
  assert_contains "$create_line" "--tags=scion-hub-${HUB},scion-hub-${HUB}-nfs" \
    "tier-on VM create should carry the hub tag and the hybrid-tier network tag in one --tags flag"
  local allow_create_line
  allow_create_line="$(echo "$log" | grep 'firewall-rules create.*nfs-allow' | head -1)"
  assert_contains "$allow_create_line" "--network=default" \
    "the hybrid firewall rule must be created on the hub's actual network, not a hardcoded or wrong one"
}

# Counts the tokens of one logged gcloud call that are the given flag,
# in either its --flag=value or its bare --flag form.
count_flag_in_call() {
  local call="$1" flag="$2"
  printf '%s\n' "$call" | tr ' ' '\n' | grep -cE -- "^${flag}(=|\$)" || true
}

# Each of --tags and --description must reach the recorded VM create and
# router create call at most once, so that two code paths adding the same
# flag cannot combine into a duplicate.
test_deploy_create_single_tags_and_description_flags() {
  fresh_gcloud_state
  seed_cluster "mycluster" "default"
  run_deploy_create_wait_for "$(base_config_json "$HUB" "$(hybrid_config_fragment)" "registry" "us-docker.pkg.dev/demo-project/scion")" \
    "instances-create-completed"
  local log instance_line router_line
  log="$(gcloud_log)"
  instance_line="$(echo "$log" | grep 'compute instances create' | head -1)"
  router_line="$(echo "$log" | grep 'compute routers create' | head -1)"
  assert_true "$([[ -n "$instance_line" && -n "$router_line" ]] && echo true || echo false)" \
    "both the VM create and the router create calls should have been logged"
  assert_contains "$instance_line" "--tags=scion-hub-${HUB},scion-hub-${HUB}-nfs" \
    "the VM create should carry the hub tag and the hybrid-tier network tag in one --tags flag"
  assert_true "$([[ "$(count_flag_in_call "$instance_line" --tags)" -le 1 ]] && echo true || echo false)" \
    "the VM create must carry at most one --tags flag"
  assert_true "$([[ "$(count_flag_in_call "$instance_line" --description)" -le 1 ]] && echo true || echo false)" \
    "the VM create must carry at most one --description flag"
  assert_true "$([[ "$(count_flag_in_call "$router_line" --description)" -le 1 ]] && echo true || echo false)" \
    "the router create must carry at most one --description flag"
  assert_true "$([[ "$(count_flag_in_call "$router_line" --tags)" -le 1 ]] && echo true || echo false)" \
    "the router create must carry at most one --tags flag"
}

test_deploy_create_tier_on_existing_vm_gets_add_tags() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "us-central1-b"
  seed_cluster "mycluster" "default"
  run_deploy_create "$(base_config_json "$HUB" "$(hybrid_config_fragment)" "registry" "us-docker.pkg.dev/demo-project/scion")"
  local log
  log="$(gcloud_log)"
  assert_eq "0" "$(echo "$log" | grep -c 'compute instances create' || true)" \
    "an existing VM must not be recreated"
  local add_tags_line
  add_tags_line="$(echo "$log" | grep 'instances add-tags' | grep -F -- "--tags=scion-hub-${HUB}-nfs" | head -1)"
  assert_contains "$add_tags_line" "add-tags ${INSTANCE_NAME}" \
    "an existing VM should get the hybrid-tier tag via add-tags"
  assert_contains "$add_tags_line" "--tags=scion-hub-${HUB}-nfs" "add-tags should carry the hybrid-tier network tag"
  assert_contains "$add_tags_line" "--zone=" "add-tags must target a specific zone, not the ambient default"
}

test_deploy_create_discovery_before_first_create() {
  fresh_gcloud_state
  seed_cluster "mycluster" "default"
  run_deploy_create "$(base_config_json "$HUB" "$(hybrid_config_fragment)" "registry" "us-docker.pkg.dev/demo-project/scion")"
  local log discover_line first_create_line network_check_line
  log="$(gcloud_log)"
  discover_line="$(line_number 'container clusters describe' "$log")"
  # `beta services identity create` provisions the Google-managed IAP
  # service agent, not a resource of this deployment, and runs before
  # the default VPC network check; every other create must follow
  # discovery.
  first_create_line="$(echo "$log" | grep -n ' create ' | grep -v ':beta services identity create ' | head -1 | cut -d: -f1)"
  assert_true "$([[ -n "$discover_line" && -n "$first_create_line" && "$discover_line" -lt "$first_create_line" ]] && echo true || echo false)" \
    "cluster discovery must happen before the first create call of any kind"
  network_check_line="$(line_number 'compute networks describe default' "$log")"
  assert_true "$([[ -n "$network_check_line" && -n "$discover_line" && "$network_check_line" -lt "$discover_line" ]] && echo true || echo false)" \
    "the default VPC network check must run before cluster discovery"
}

test_deploy_create_removes_temp_kubeconfig_on_exit() {
  fresh_gcloud_state
  seed_cluster "mycluster" "default"
  run_deploy_create "$(base_config_json "$HUB" "$(hybrid_config_fragment)" "registry" "us-docker.pkg.dev/demo-project/scion")"
  local kubeconfig_path
  kubeconfig_path="$(kubectl_log | head -1 | sed -n 's/^KUBECONFIG=\([^ ]*\) .*/\1/p')"
  assert_true "$([[ -n "$kubeconfig_path" ]] && echo true || echo false)" \
    "the k8s preflight must have actually used a task-private KUBECONFIG, or this test can't check it was cleaned up"
  assert_false "$([[ -n "$kubeconfig_path" && -f "$kubeconfig_path" ]] && echo true)" \
    "the temporary kubeconfig deploy.sh's own EXIT trap creates must be removed once deploy.sh exits, not left behind in \$TMPDIR"
}

# run_deploy_create_interrupted_in_allusers_check CONFIG_JSON DEPLOY_TMPDIR [SIGNAL]
# -- runs deploy.sh (create mode) with TMPDIR=DEPLOY_TMPDIR and every
# `compute ssh`/`compute scp` succeeding, so it runs on into Phase 4.
# The stub holds the allUsers `run services get-iam-policy` read open;
# once that call has started, deploy.sh's process group is sent SIGNAL
# (TERM by default, or INT; for INT, deploy.sh starts with SIGINT at its
# default disposition). At that point the allUsers stderr temp file
# exists and, with the tier on, so does the temporary kubeconfig. Sets
# DEPLOY_RC (143 after TERM, 130 after INT), DEPLOY_LOG and
# INTERRUPT_REACHED (true once the read had started).
run_deploy_create_interrupted_in_allusers_check() {
  local config_json="$1" deploy_tmpdir="$2" signal="${3:-TERM}" config_file log_file pid
  config_file="$(mktemp)"
  log_file="$(mktemp)"
  printf '%s' "$config_json" > "$config_file"
  local sentinel="${GCLOUD_STUB_STATE_DIR}/run-services-get-policy-started"
  rm -f "$sentinel"
  touch "${GCLOUD_STUB_STATE_DIR}/run-services/${INSTANCE_NAME}-iap-proxy.get-policy-blocks"
  if [[ "$signal" == "INT" ]]; then
    DEPLOY_BG_DEFAULT_SIGINT=true _start_deploy_bg "$config_file" "$log_file" GCLOUD_STUB_SSH_SUCCEEDS=true TMPDIR="$deploy_tmpdir"
  else
    _start_deploy_bg "$config_file" "$log_file" GCLOUD_STUB_SSH_SUCCEEDS=true TMPDIR="$deploy_tmpdir"
  fi
  pid="$_DEPLOY_BG_PID"
  local waited_ms=0
  while [[ ! -f "$sentinel" && "$waited_ms" -lt 30000 ]]; do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1
    waited_ms=$((waited_ms + 100))
  done
  INTERRUPT_REACHED=false
  [[ -f "$sentinel" ]] && INTERRUPT_REACHED=true
  if [[ "$INTERRUPT_REACHED" == "true" && "$signal" != "TERM" ]]; then
    # Signal first and give deploy.sh time to exit on that signal alone;
    # _stop_deploy_bg's own SIGTERM below then only reaps it. If SIGNAL
    # did not end it, that SIGTERM does, and DEPLOY_RC shows 143.
    kill "-${signal}" -- "-${pid}" 2>/dev/null || true
    waited_ms=0
    while kill -0 "$pid" 2>/dev/null && [[ "$waited_ms" -lt 10000 ]]; do
      sleep 0.05
      waited_ms=$((waited_ms + 50))
    done
  fi
  _stop_deploy_bg "$pid" "$log_file"
  DEPLOY_LOG="$(cat "$log_file")"
  rm -f "$config_file" "$log_file"
}

# run_deploy_create_until_exit CONFIG_JSON DEPLOY_TMPDIR [ENV_ASSIGNMENT...]
# -- runs deploy.sh (create mode) with TMPDIR=DEPLOY_TMPDIR, every
# `compute ssh`/`compute scp` succeeding and no IAP enforcement wait,
# and lets it exit on its own: at the end of a full run, or at whichever
# failure the test seeded. Sets DEPLOY_RC, DEPLOY_LOG and EXITED_ON_OWN
# (false if deploy.sh was still running after 60s and had to be
# stopped).
run_deploy_create_until_exit() {
  local config_json="$1" deploy_tmpdir="$2" config_file log_file pid
  shift 2
  config_file="$(mktemp)"
  log_file="$(mktemp)"
  printf '%s' "$config_json" > "$config_file"
  _start_deploy_bg "$config_file" "$log_file" GCLOUD_STUB_SSH_SUCCEEDS=true IAP_ENFORCEMENT_WAIT_SECS=0 \
    TMPDIR="$deploy_tmpdir" "$@"
  pid="$_DEPLOY_BG_PID"
  local waited_ms=0
  while kill -0 "$pid" 2>/dev/null && [[ "$waited_ms" -lt 60000 ]]; do
    sleep 0.1
    waited_ms=$((waited_ms + 100))
  done
  EXITED_ON_OWN=true
  kill -0 "$pid" 2>/dev/null && EXITED_ON_OWN=false
  _stop_deploy_bg "$pid" "$log_file"
  DEPLOY_LOG="$(cat "$log_file")"
  rm -f "$config_file" "$log_file"
}

# _first_kubeconfig_path -- the KUBECONFIG path on the first kubectl call
# deploy.sh made, i.e. the temporary kubeconfig it created.
_first_kubeconfig_path() {
  kubectl_log | head -1 | sed -n 's/^KUBECONFIG=\([^ ]*\) .*/\1/p'
}

# _assert_tier_on_temp_files_removed DEPLOY_TMPDIR CONTEXT -- the
# temporary kubeconfig was created under DEPLOY_TMPDIR, no longer
# exists, and nothing else is left in DEPLOY_TMPDIR either (the allUsers
# stderr file, once created, included).
_assert_tier_on_temp_files_removed() {
  local deploy_tmpdir="$1" context="$2" kubeconfig_path leftover
  kubeconfig_path="$(_first_kubeconfig_path)"
  assert_contains "$kubeconfig_path" "$deploy_tmpdir/" \
    "the tier-on run must have used a temporary kubeconfig under deploy.sh's own TMPDIR (${context})"
  assert_false "$([[ -n "$kubeconfig_path" && -e "$kubeconfig_path" ]] && echo true)" \
    "the temporary kubeconfig must be removed when deploy.sh exits (${context})"
  leftover="$(find "$deploy_tmpdir" -mindepth 1 2>/dev/null)"
  assert_eq "" "$leftover" \
    "no temp file (the kubeconfig, the allUsers stderr file, or any other) may be left in deploy.sh's TMPDIR (${context})"
}

# _assert_deploy_tmpdir_empty DEPLOY_TMPDIR CONTEXT
_assert_deploy_tmpdir_empty() {
  local deploy_tmpdir="$1" context="$2" leftover
  leftover="$(find "$deploy_tmpdir" -mindepth 1 2>/dev/null)"
  assert_eq "" "$leftover" \
    "the allUsers stderr file (or any other temp file) must not be left in deploy.sh's TMPDIR (${context})"
}

# The EXIT trap removes the kubeconfig and the allUsers stderr file: an
# interrupt during the allUsers policy read leaves neither
# the temporary kubeconfig nor the allUsers stderr file (nor anything
# else) in deploy.sh's TMPDIR.
test_deploy_create_tier_on_interrupt_in_phase4_removes_temp_files() {
  fresh_gcloud_state
  seed_cluster "mycluster" "default"
  local deploy_tmpdir
  deploy_tmpdir="$(mktemp -d)"
  run_deploy_create_interrupted_in_allusers_check \
    "$(base_config_json "$HUB" "$(hybrid_config_fragment)" "registry" "us-docker.pkg.dev/demo-project/scion")" \
    "$deploy_tmpdir"
  assert_eq "true" "$INTERRUPT_REACHED" \
    "deploy.sh must reach the Phase 4 allUsers policy read, or this test can't check cleanup there: ${DEPLOY_LOG}"
  assert_eq "143" "$DEPLOY_RC" "deploy.sh must have ended on SIGTERM"
  _assert_tier_on_temp_files_removed "$deploy_tmpdir" "SIGTERM in Phase 4"
  rm -rf "$deploy_tmpdir"
}

test_deploy_create_tier_on_sigint_in_phase4_removes_temp_files() {
  fresh_gcloud_state
  seed_cluster "mycluster" "default"
  local deploy_tmpdir
  deploy_tmpdir="$(mktemp -d)"
  run_deploy_create_interrupted_in_allusers_check \
    "$(base_config_json "$HUB" "$(hybrid_config_fragment)" "registry" "us-docker.pkg.dev/demo-project/scion")" \
    "$deploy_tmpdir" INT
  assert_eq "true" "$INTERRUPT_REACHED" \
    "deploy.sh must reach the Phase 4 allUsers policy read, or this test can't check cleanup there: ${DEPLOY_LOG}"
  assert_eq "130" "$DEPLOY_RC" "deploy.sh must have ended on SIGINT alone"
  _assert_tier_on_temp_files_removed "$deploy_tmpdir" "SIGINT in Phase 4"
  rm -rf "$deploy_tmpdir"
}

# A tier-on deploy that fails on its own in Phase 2, after the temporary
# kubeconfig has been created and used (the k8s preflight cannot read
# the persistent volume), exits 1 and leaves nothing in its TMPDIR.
test_deploy_create_tier_on_phase2_failure_after_kubeconfig_removes_temp_files() {
  fresh_gcloud_state
  seed_cluster "mycluster" "default"
  echo "Error from server (Forbidden): simulated PERMISSION_DENIED error getting persistentvolume" \
    > "${KUBECTL_STUB_STATE_DIR}/pv/scion-hub-${HUB}-shared.json.get-error"
  local deploy_tmpdir
  deploy_tmpdir="$(mktemp -d)"
  run_deploy_create_until_exit \
    "$(base_config_json "$HUB" "$(hybrid_config_fragment)" "registry" "us-docker.pkg.dev/demo-project/scion")" \
    "$deploy_tmpdir"
  assert_eq "true" "$EXITED_ON_OWN" "deploy.sh must exit on its own: ${DEPLOY_LOG}"
  assert_eq "1" "$DEPLOY_RC" "deploy.sh must exit 1 when the k8s preflight fails"
  assert_contains "$DEPLOY_LOG" "Could not check persistent volume scion-hub-${HUB}-shared" \
    "deploy.sh must stop at the Phase 2 k8s preflight, after the kubeconfig was created"
  assert_eq "0" "$(gcloud_log | grep -c '^compute instances create' || true)" \
    "the failure must happen in Phase 2, before the VM is created"
  _assert_tier_on_temp_files_removed "$deploy_tmpdir" "Phase 2 error exit"
  rm -rf "$deploy_tmpdir"
}

# A tier-on deploy that fails on its own in Phase 4 when the allUsers
# policy read fails: both the kubeconfig and the allUsers stderr file
# are gone after the exit 1.
test_deploy_create_tier_on_phase4_allusers_read_failure_removes_temp_files() {
  fresh_gcloud_state
  seed_cluster "mycluster" "default"
  touch "${GCLOUD_STUB_STATE_DIR}/run-services/${INSTANCE_NAME}-iap-proxy.get-policy-fail"
  local deploy_tmpdir
  deploy_tmpdir="$(mktemp -d)"
  run_deploy_create_until_exit \
    "$(base_config_json "$HUB" "$(hybrid_config_fragment)" "registry" "us-docker.pkg.dev/demo-project/scion")" \
    "$deploy_tmpdir"
  assert_eq "true" "$EXITED_ON_OWN" "deploy.sh must exit on its own: ${DEPLOY_LOG}"
  assert_eq "1" "$DEPLOY_RC" "deploy.sh must exit 1 when the allUsers policy read fails"
  assert_contains "$DEPLOY_LOG" "Could not read the IAM policy for ${INSTANCE_NAME}-iap-proxy" \
    "deploy.sh must stop at the allUsers policy read"
  _assert_tier_on_temp_files_removed "$deploy_tmpdir" "Phase 4 error exit"
  rm -rf "$deploy_tmpdir"
}

# A full tier-on deploy exits 0 and leaves nothing in its TMPDIR.
test_deploy_create_tier_on_full_run_removes_temp_files() {
  fresh_gcloud_state
  seed_cluster "mycluster" "default"
  local deploy_tmpdir
  deploy_tmpdir="$(mktemp -d)"
  run_deploy_create_until_exit \
    "$(base_config_json "$HUB" "$(hybrid_config_fragment)" "registry" "us-docker.pkg.dev/demo-project/scion")" \
    "$deploy_tmpdir"
  assert_eq "true" "$EXITED_ON_OWN" "deploy.sh must exit on its own: ${DEPLOY_LOG}"
  assert_eq "0" "$DEPLOY_RC" "a full tier-on deploy against the stub must exit 0: ${DEPLOY_LOG}"
  assert_eq "1" "$(gcloud_log | grep -c '^run services get-iam-policy' || true)" \
    "the run must pass through the allUsers policy read"
  _assert_tier_on_temp_files_removed "$deploy_tmpdir" "normal exit"
  rm -rf "$deploy_tmpdir"
}

test_deploy_create_tier_off_interrupt_in_phase4_removes_temp_files() {
  fresh_gcloud_state
  local deploy_tmpdir
  deploy_tmpdir="$(mktemp -d)"
  run_deploy_create_interrupted_in_allusers_check \
    "$(base_config_json "$HUB" "" "registry" "us-docker.pkg.dev/demo-project/scion")" "$deploy_tmpdir"
  assert_eq "true" "$INTERRUPT_REACHED" \
    "deploy.sh must reach the Phase 4 allUsers policy read, or this test can't check cleanup there: ${DEPLOY_LOG}"
  assert_eq "143" "$DEPLOY_RC" "deploy.sh must have ended on SIGTERM"
  _assert_deploy_tmpdir_empty "$deploy_tmpdir" "SIGTERM in Phase 4"
  rm -rf "$deploy_tmpdir"
}

test_deploy_create_tier_off_sigint_in_phase4_removes_temp_files() {
  fresh_gcloud_state
  local deploy_tmpdir
  deploy_tmpdir="$(mktemp -d)"
  run_deploy_create_interrupted_in_allusers_check \
    "$(base_config_json "$HUB" "" "registry" "us-docker.pkg.dev/demo-project/scion")" "$deploy_tmpdir" INT
  assert_eq "true" "$INTERRUPT_REACHED" \
    "deploy.sh must reach the Phase 4 allUsers policy read, or this test can't check cleanup there: ${DEPLOY_LOG}"
  assert_eq "130" "$DEPLOY_RC" "deploy.sh must have ended on SIGINT alone"
  _assert_deploy_tmpdir_empty "$deploy_tmpdir" "SIGINT in Phase 4"
  rm -rf "$deploy_tmpdir"
}

test_deploy_create_tier_off_allusers_read_failure_removes_temp_files() {
  fresh_gcloud_state
  touch "${GCLOUD_STUB_STATE_DIR}/run-services/${INSTANCE_NAME}-iap-proxy.get-policy-fail"
  local deploy_tmpdir
  deploy_tmpdir="$(mktemp -d)"
  run_deploy_create_until_exit \
    "$(base_config_json "$HUB" "" "registry" "us-docker.pkg.dev/demo-project/scion")" "$deploy_tmpdir"
  assert_eq "true" "$EXITED_ON_OWN" "deploy.sh must exit on its own: ${DEPLOY_LOG}"
  assert_eq "1" "$DEPLOY_RC" "deploy.sh must exit 1 when the allUsers policy read fails"
  assert_contains "$DEPLOY_LOG" "Could not read the IAM policy for ${INSTANCE_NAME}-iap-proxy" \
    "deploy.sh must stop at the allUsers policy read"
  _assert_deploy_tmpdir_empty "$deploy_tmpdir" "error exit"
  rm -rf "$deploy_tmpdir"
}

test_deploy_create_tier_off_full_run_removes_temp_files() {
  fresh_gcloud_state
  local deploy_tmpdir
  deploy_tmpdir="$(mktemp -d)"
  run_deploy_create_until_exit \
    "$(base_config_json "$HUB" "" "registry" "us-docker.pkg.dev/demo-project/scion")" "$deploy_tmpdir"
  assert_eq "true" "$EXITED_ON_OWN" "deploy.sh must exit on its own: ${DEPLOY_LOG}"
  assert_eq "0" "$DEPLOY_RC" "a full tier-off deploy against the stub must exit 0: ${DEPLOY_LOG}"
  assert_eq "1" "$(gcloud_log | grep -c '^run services get-iam-policy' || true)" \
    "the run must pass through the allUsers policy read"
  _assert_deploy_tmpdir_empty "$deploy_tmpdir" "normal exit"
  rm -rf "$deploy_tmpdir"
}

# deploy.sh registers exactly one EXIT trap: a second registration would
# replace, not extend, the first.
test_deploy_sh_registers_a_single_exit_trap() {
  local count
  count="$(grep -cE '^[[:space:]]*trap .* EXIT[[:space:]]*$' "$DEPLOY_SH" || true)"
  assert_eq "1" "$count" "deploy.sh must register exactly one EXIT trap"
}

# =====================================================================
# Base markers on create: additive, create-only, never checked or
# adopted on. This tier-off behavior is intentional: markers apply on
# every fresh deploy regardless of whether the hybrid tier is on, and
# never on redeploys of an existing resource. Base teardown is
# unaffected either way.
# =====================================================================

test_deploy_create_base_markers_present_on_fresh_create() {
  fresh_gcloud_state
  run_deploy_create "$(base_config_json "$HUB")"
  local log
  log="$(gcloud_log)"
  local instance_line sa_line proxy_sa_line router_line fw_line proxy_fw_line
  instance_line="$(echo "$log" | grep 'compute instances create' | head -1)"
  sa_line="$(echo "$log" | grep "service-accounts create ${INSTANCE_NAME} " | head -1)"
  proxy_sa_line="$(echo "$log" | grep "service-accounts create ${INSTANCE_NAME}-proxy " | head -1)"
  router_line="$(echo "$log" | grep 'compute routers create' | head -1)"
  fw_line="$(echo "$log" | grep 'firewall-rules create.*allow-iap-ssh' | head -1)"
  proxy_fw_line="$(echo "$log" | grep 'firewall-rules create.*allow-proxy' | head -1)"
  assert_contains "$instance_line" "--labels=scion-deployment=${HUB}" "the VM must carry the marker label on create"
  assert_contains "$sa_line" "--description=scion-deployment=${HUB}" "the service account must carry the marker description on create"
  assert_contains "$proxy_sa_line" "--description=scion-deployment=${HUB}" "the Cloud Run proxy service account must carry the marker description on create"
  assert_contains "$router_line" "--description=scion-deployment=${HUB}" "the router must carry the marker description on create"
  assert_contains "$fw_line" "scion-deployment=${HUB}" \
    "the IAP SSH firewall rule's description must include the marker token alongside its existing text"
  assert_contains "$fw_line" "Allow SSH via IAP tunneling" \
    "the IAP SSH firewall rule's existing description text must be preserved, not replaced by the marker"
  assert_contains "$proxy_fw_line" "scion-deployment=${HUB}" \
    "the proxy-to-VM firewall rule's description must include the marker token alongside its existing text"
  assert_contains "$proxy_fw_line" "Allow the Cloud Run IAP proxy" \
    "the proxy-to-VM firewall rule's existing description text must be preserved, not replaced by the marker"
}

test_deploy_create_base_markers_absent_when_tier_off_no_tags() {
  # Same fixture as test_deploy_create_tier_off_no_nfs_tag_no_container_calls,
  # but checking the marker is present regardless -- this is the
  # deliberate tier-off exception, not something gated on the tier.
  fresh_gcloud_state
  run_deploy_create "$(base_config_json "$HUB")"
  assert_eq "1" "$(gcloud_log | grep -c -- "--labels=scion-deployment=${HUB}" || true)" \
    "the base marker must be present even with the hybrid tier off"
}

# Markers must never appear on an adopt/existing-resource path: this
# reuses the existing-VM fixture (no hybrid tier involved) to prove the
# instance marker is create-only.
test_deploy_create_base_markers_absent_on_existing_vm() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "us-central1-b"
  run_deploy_create "$(base_config_json "$HUB")"
  assert_eq "0" "$(gcloud_log | grep -c 'compute instances create' || true)" \
    "an existing VM must not be recreated (and so never gets the create-only marker call)"
}

test_deploy_create_base_markers_absent_on_existing_router_and_sa() {
  fresh_gcloud_state
  set_router_exists
  set_service_account_exists
  run_deploy_create "$(base_config_json "$HUB")"
  assert_eq "0" "$(gcloud_log | grep -c 'compute routers create' || true)" \
    "an existing router must not be recreated (and so never gets the create-only marker call)"
  assert_eq "0" "$(gcloud_log | grep -c 'service-accounts create' || true)" \
    "an existing service account must not be recreated (and so never gets the create-only marker call)"
  assert_eq "0" "$(gcloud_log | grep -c 'service-accounts update' || true)" \
    "adopting an existing service account must not re-describe/update it -- create-only, like every other base marker"
}

# =====================================================================
# API check: when the enabled-API list can be read, enable only what's
# missing, never the whole list when nothing needs it, and add
# container.googleapis.com when the tier is on.
# =====================================================================

test_deploy_create_api_check_all_enabled_no_enable_call() {
  fresh_gcloud_state
  seed_enabled_apis compute.googleapis.com run.googleapis.com iap.googleapis.com \
    cloudbuild.googleapis.com artifactregistry.googleapis.com aiplatform.googleapis.com \
    iam.googleapis.com iamcredentials.googleapis.com
  run_deploy_create "$(base_config_json "$HUB")"
  assert_eq "0" "$(gcloud_log | grep -c 'services enable' || true)" \
    "nothing missing must mean no enable call at all, not an enable call with zero APIs"
}

test_deploy_create_api_check_missing_enables_exact_set() {
  fresh_gcloud_state
  seed_enabled_apis compute.googleapis.com iap.googleapis.com artifactregistry.googleapis.com \
    aiplatform.googleapis.com
  run_deploy_create "$(base_config_json "$HUB")"
  local enable_line
  enable_line="$(gcloud_log | grep 'services enable' | head -1)"
  assert_eq "1" "$(gcloud_log | grep -c 'services enable' || true)" "exactly one enable call for the missing set"
  assert_contains "$enable_line" "run.googleapis.com" "the enable call must include a missing API"
  assert_contains "$enable_line" "cloudbuild.googleapis.com" "the enable call must include the other missing API"
  assert_contains "$enable_line" "iam.googleapis.com" "the enable call must include iam.googleapis.com when it is missing"
  assert_not_contains "$enable_line" "compute.googleapis.com" "an already-enabled API must not be re-enabled"
  assert_not_contains "$enable_line" "artifactregistry.googleapis.com" "an already-enabled API must not be re-enabled"
}

test_deploy_create_api_check_tier_on_adds_container() {
  fresh_gcloud_state
  seed_enabled_apis compute.googleapis.com run.googleapis.com iap.googleapis.com \
    cloudbuild.googleapis.com artifactregistry.googleapis.com aiplatform.googleapis.com \
    iam.googleapis.com iamcredentials.googleapis.com
  seed_cluster "mycluster" "default"
  run_deploy_create "$(base_config_json "$HUB" "$(hybrid_config_fragment)" "registry" "us-docker.pkg.dev/demo-project/scion")"
  local enable_line
  enable_line="$(gcloud_log | grep 'services enable' | head -1)"
  assert_eq "1" "$(gcloud_log | grep -c 'services enable' || true)" "exactly one enable call, for container only"
  assert_contains "$enable_line" "container.googleapis.com" \
    "the hybrid tier needs container.googleapis.com enabled"
  assert_not_contains "$enable_line" "compute.googleapis.com" \
    "the base APIs are already enabled and must not be re-enabled"
}

test_deploy_create_api_check_list_failure_tier_off_falls_back_to_unconditional_enable() {
  fresh_gcloud_state
  set_services_list_will_fail
  run_deploy_create "$(base_config_json "$HUB")"
  local enable_line
  enable_line="$(gcloud_log | grep 'services enable' | head -1)"
  assert_contains "$enable_line" "compute.googleapis.com" \
    "tier-off must fall back to enabling the whole base list when it can't list what's already enabled"
  assert_contains "$enable_line" "artifactregistry.googleapis.com" "the fallback enable call must include every base API"
  local create_line
  create_line="$(gcloud_log | grep 'compute instances create' | head -1)"
  assert_true "$([[ -n "$create_line" ]] && echo true || echo false)" \
    "the run must still reach VM creation despite the list failure, tier off"
}

test_deploy_create_api_check_list_failure_tier_on_fails_actionably() {
  fresh_gcloud_state
  set_services_list_will_fail
  local config_file
  config_file="$(mktemp)"
  printf '%s' "$(base_config_json "$HUB" "$(hybrid_config_fragment)" "registry" "us-docker.pkg.dev/demo-project/scion")" > "$config_file"
  local log rc
  log="$(timeout 60 bash "$DEPLOY_SH" --config "$config_file" --version v1.0.0-test < /dev/null 2>&1)"
  rc=$?
  rm -f "$config_file"
  assert_true "$([[ "$rc" -ne 0 && "$rc" -ne 124 ]] && echo true || echo false)" \
    "the tier-on run must fail fast on a list failure, not hang until the create-mode timeout"
  assert_eq "0" "$(gcloud_log | grep -c 'compute instances create' || true)" \
    "nothing should be created when the API check can't tell what's missing, tier on"
  assert_contains "$log" "serviceusage" "the message should explain why an unknown API state is refused, not just fail silently"
}

# =====================================================================
# GKE nodes can't pull from the VM's local Docker store: tier-on refuses
# container_images.source=build (and any localhost/ registry) before any
# create. The refusal check itself doesn't distinguish config-mode from
# interactive-mode input -- both set the same IMAGE_SOURCE/IMAGE_REGISTRY
# variables the check reads -- so this is covered once, in config mode,
# where it can run fast and deterministically; the interactive image
# prompt feeds the identical two variables.
# =====================================================================

test_deploy_create_tier_on_source_build_refused() {
  fresh_gcloud_state
  local config_file
  config_file="$(mktemp)"
  printf '%s' "$(base_config_json "$HUB" "$(hybrid_config_fragment)")" > "$config_file"
  local log rc
  log="$(timeout 60 bash "$DEPLOY_SH" --config "$config_file" --version v1.0.0-test < /dev/null 2>&1)"
  rc=$?
  rm -f "$config_file"
  assert_true "$([[ "$rc" -ne 0 && "$rc" -ne 124 ]] && echo true || echo false)" \
    "tier-on with source=build must be refused fast, not hang until the create-mode timeout"
  assert_eq "0" "$(gcloud_log | grep -c 'compute instances create' || true)" "nothing should be created"
  assert_contains "$log" "cannot pull images" "the message should explain why"
}

test_deploy_create_tier_on_loopback_registry_refused_with_correct_message() {
  fresh_gcloud_state
  local config_file
  config_file="$(mktemp)"
  printf '%s' "$(base_config_json "$HUB" "$(hybrid_config_fragment)" "registry" "localhost:5000/scion")" > "$config_file"
  local log rc
  log="$(timeout 60 bash "$DEPLOY_SH" --config "$config_file" --version v1.0.0-test < /dev/null 2>&1)"
  rc=$?
  rm -f "$config_file"
  assert_true "$([[ "$rc" -ne 0 && "$rc" -ne 124 ]] && echo true || echo false)" \
    "tier-on with a loopback registry must be refused fast, not hang until the create-mode timeout"
  assert_eq "0" "$(gcloud_log | grep -c 'compute instances create' || true)" "nothing should be created"
  assert_contains "$log" "names this VM itself" "the message must explain the loopback problem, not claim source is 'build'"
  assert_not_contains "$log" "source is 'build'" \
    "the loopback-registry branch must not use the source=build error message (container_images.source is 'registry' here)"
}

test_deploy_create_tier_off_source_build_unaffected() {
  fresh_gcloud_state
  run_deploy_create "$(base_config_json "$HUB")"
  assert_true "$([[ -f "${GCLOUD_STUB_STATE_DIR}/vm-create-happened" ]] && echo true || echo false)" \
    "tier-off must still reach VM creation with the default source=build config, unaffected by this refusal"
}

# =====================================================================
# VM-gone must be a positive, project-wide answer when
# hybrid rules are queued, and tier-off teardown keeps warning and
# continuing on a VM delete failure.
# =====================================================================

test_deploy_delete_vm_uncertain_after_transient_list_error_keeps_rules() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "us-central1-b"
  set_instance_delete_will_fail "$INSTANCE_NAME"
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-allow" "scion-deployment=${HUB}" \
    "default" "INGRESS" "ALLOW" "tcp" "2049" "gke-x-node" "" "scion-hub-${HUB}-nfs" "900"
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-deny" "scion-deployment=${HUB}" \
    "default" "INGRESS" "DENY" "tcp" "2049" "" "0.0.0.0/0" "scion-hub-${HUB}-nfs" "950"
  set_instances_list_will_fail
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_eq "1" "$DEPLOY_RC" "an unconfirmable VM state must fail the run"
  assert_eq "0" "$(gcloud_log | grep -c "firewall-rules delete scion-hub-${HUB}-nfs-" || true)" \
    "the hybrid rules must not be deleted when the VM's fate can't be confirmed"
  assert_contains "$DEPLOY_LOG" "could not confirm" "should explain that the VM's state is unknown, not assumed gone"
}

# A zone being UNREACHABLE during the VM-gone check must not read as "the
# VM is gone": real gcloud downgrades that to a warning and exit 0 with
# the VM silently missing from the list, unless
# CLOUDSDK_COMPUTE_ALLOW_PARTIAL_ERROR=false is set on the call, which the
# stub enforces (see set_instances_list_zone_unreachable).
test_deploy_delete_vm_zone_unreachable_partial_list_keeps_rules() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "us-central1-b"
  set_instance_delete_will_fail "$INSTANCE_NAME"
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-allow" "scion-deployment=${HUB}" \
    "default" "INGRESS" "ALLOW" "tcp" "2049" "gke-x-node" "" "scion-hub-${HUB}-nfs" "900"
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-deny" "scion-deployment=${HUB}" \
    "default" "INGRESS" "DENY" "tcp" "2049" "" "0.0.0.0/0" "scion-hub-${HUB}-nfs" "950"
  set_instances_list_zone_unreachable
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_eq "1" "$DEPLOY_RC" \
    "a partial AggregatedList result from an unreachable zone must not be read as the VM being gone"
  assert_eq "0" "$(gcloud_log | grep -c "firewall-rules delete scion-hub-${HUB}-nfs-" || true)" \
    "the hybrid rules must not be deleted when a zone is unreachable and the VM's fate can't be confirmed"
  assert_contains "$DEPLOY_LOG" "could not confirm" \
    "should explain that the VM's state is unknown, not assumed gone, for the unreachable-zone case too"
}

test_deploy_delete_tier_off_vm_failure_warns_and_continues() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "us-central1-b"
  set_instance_delete_will_fail "$INSTANCE_NAME"
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_eq "0" "$DEPLOY_RC" \
    "with no hybrid rules queued, a VM delete failure must warn and continue exactly as before this tier existed"
  # The stub's generic "simulated delete failure" text carries none of
  # _hybrid_gcloud_not_found's required tokens, so this must be classified
  # as an unconfirmed failure (Kept), not misreported as a positive
  # not-found -- the fake VM is still "present" in the stub's own state.
  assert_contains "$DEPLOY_LOG" "  Kept GCE VM:                ${INSTANCE_NAME}" \
    "a VM delete failure that isn't a confirmed not-found must be reported as Kept, not Not found"
  assert_not_contains "$DEPLOY_LOG" "Not found GCE VM" \
    "must never claim the VM is gone from an ambiguous delete error alone"
}

# A base-resource delete that fails with an error other than not-found
# prints that error on its own step line, consistent with the summary's
# Kept line.
test_deploy_delete_tier_off_base_delete_error_prints_error() {
  fresh_gcloud_state
  touch "${GCLOUD_STUB_STATE_DIR}/run-services/${INSTANCE_NAME}-iap-proxy.delete-error"
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_eq "0" "$DEPLOY_RC" "a base-resource delete error must warn and continue"
  assert_contains "$DEPLOY_LOG" "Cloud Run service ${INSTANCE_NAME}-iap-proxy could not be deleted: gcloud-stub: ERROR: (gcloud.run.services.delete) PERMISSION_DENIED" \
    "the step line must carry the delete's own error text"
  assert_not_contains "$DEPLOY_LOG" "Cloud Run service ${INSTANCE_NAME}-iap-proxy not found or already deleted" \
    "a delete error that is not a not-found must not be reported as not found on the step line"
  assert_contains "$DEPLOY_LOG" "  Kept Cloud Run service:     ${INSTANCE_NAME}-iap-proxy" \
    "the summary must report the service as Kept"
}

# ---------------------------------------------------------------------
# Every base-resource delete classifies its own error text: a positive
# not-found reads as "not found or already deleted" and "Not found" in
# the summary; any other error prints "could not be deleted: <text>" on
# that step and "Kept" in the summary. Either way teardown continues
# and exits 0. The error text below carries no 404 or NOT_FOUND token.
# ---------------------------------------------------------------------

BASE_SA_EMAIL="scion-hub-${HUB}@demo-project.iam.gserviceaccount.com"
BASE_PROXY_SA_EMAIL="scion-hub-${HUB}-proxy@demo-project.iam.gserviceaccount.com"
BASE_FW_IAP_SSH="scion-hub-${HUB}-allow-iap-ssh"
BASE_FW_PROXY="scion-hub-${HUB}-allow-proxy"

# _base_delete_error_text CMD — a delete error that is not a not-found.
_base_delete_error_text() {
  printf 'ERROR: (gcloud.%s) Could not fetch resource: - Backend error, try again later. (code=503)' "$1"
}

# _base_delete_not_found_text CMD NAME — a positive not-found.
_base_delete_not_found_text() {
  printf "ERROR: (gcloud.%s) Could not fetch resource: - The resource '%s' was not found (code=404)" "$1" "$2"
}

# _assert_base_delete_kept STEP SUMMARY_LINE ERROR_TEXT
_assert_base_delete_kept() {
  local step="$1" summary="$2" text="$3"
  assert_eq "0" "$DEPLOY_RC" "${step}: a base-resource delete error must warn and continue"
  assert_contains "$DEPLOY_LOG" "${step} could not be deleted: ${text}" \
    "${step}: the step line must carry the delete's own error text"
  assert_not_contains "$DEPLOY_LOG" "${step} not found or already deleted" \
    "${step}: a delete error that is not a not-found must not be reported as not found"
  assert_contains "$DEPLOY_LOG" "$summary" "${step}: the summary must report it as Kept"
}

# _assert_base_delete_not_found STEP SUMMARY_LINE
_assert_base_delete_not_found() {
  local step="$1" summary="$2"
  assert_eq "0" "$DEPLOY_RC" "${step}: a not-found delete must warn and continue"
  assert_contains "$DEPLOY_LOG" "${step} not found or already deleted." \
    "${step}: a positive not-found must be reported as not found on the step line"
  assert_not_contains "$DEPLOY_LOG" "${step} could not be deleted" \
    "${step}: a positive not-found must not be reported as a delete error"
  assert_contains "$DEPLOY_LOG" "$summary" "${step}: the summary must report it as Not found"
}

test_deploy_delete_tier_off_cloud_run_delete_not_found_reports_not_found() {
  fresh_gcloud_state
  set_run_service_delete_error "${INSTANCE_NAME}-iap-proxy" \
    "$(_base_delete_not_found_text run.services.delete "${INSTANCE_NAME}-iap-proxy")"
  run_deploy_delete "$(base_config_json "$HUB")"
  _assert_base_delete_not_found "Cloud Run service ${INSTANCE_NAME}-iap-proxy" \
    "  Not found Cloud Run service: ${INSTANCE_NAME}-iap-proxy"
}

test_deploy_delete_tier_off_nat_delete_error_prints_error() {
  local text
  text="$(_base_delete_error_text compute.routers.nats.delete)"
  fresh_gcloud_state
  set_nat_delete_error "${INSTANCE_NAME}-nat" "$text"
  run_deploy_delete "$(base_config_json "$HUB")"
  _assert_base_delete_kept "Cloud NAT ${INSTANCE_NAME}-nat" \
    "  Kept Cloud NAT:             ${INSTANCE_NAME}-nat" "$text"
}

test_deploy_delete_tier_off_nat_delete_not_found_reports_not_found() {
  fresh_gcloud_state
  set_nat_delete_error "${INSTANCE_NAME}-nat" \
    "$(_base_delete_not_found_text compute.routers.nats.delete "${INSTANCE_NAME}-nat")"
  run_deploy_delete "$(base_config_json "$HUB")"
  _assert_base_delete_not_found "Cloud NAT ${INSTANCE_NAME}-nat" \
    "  Not found Cloud NAT:        ${INSTANCE_NAME}-nat"
}

test_deploy_delete_tier_off_router_delete_error_prints_error() {
  local text
  text="$(_base_delete_error_text compute.routers.delete)"
  fresh_gcloud_state
  set_router_delete_error "$text"
  run_deploy_delete "$(base_config_json "$HUB")"
  _assert_base_delete_kept "Cloud Router ${INSTANCE_NAME}-router" \
    "  Kept Cloud Router:          ${INSTANCE_NAME}-router" "$text"
}

test_deploy_delete_tier_off_router_delete_not_found_reports_not_found() {
  fresh_gcloud_state
  set_router_delete_error \
    "$(_base_delete_not_found_text compute.routers.delete "${INSTANCE_NAME}-router")"
  run_deploy_delete "$(base_config_json "$HUB")"
  _assert_base_delete_not_found "Cloud Router ${INSTANCE_NAME}-router" \
    "  Not found Cloud Router:     ${INSTANCE_NAME}-router"
}

test_deploy_delete_tier_off_vm_sa_delete_error_prints_error() {
  local text
  text="$(_base_delete_error_text iam.service-accounts.delete)"
  fresh_gcloud_state
  set_service_account_delete_will_fail "$BASE_SA_EMAIL"
  set_service_account_delete_error_text "$BASE_SA_EMAIL" "$text"
  run_deploy_delete "$(base_config_json "$HUB")"
  _assert_base_delete_kept "Service account ${BASE_SA_EMAIL}" \
    "  Kept service account:       ${BASE_SA_EMAIL}" "$text"
}

test_deploy_delete_tier_off_vm_sa_delete_not_found_reports_not_found() {
  fresh_gcloud_state
  set_service_account_delete_will_fail "$BASE_SA_EMAIL"
  set_service_account_delete_error_text "$BASE_SA_EMAIL" \
    "ERROR: (gcloud.iam.service-accounts.delete) NOT_FOUND: Unknown service account"
  run_deploy_delete "$(base_config_json "$HUB")"
  _assert_base_delete_not_found "Service account ${BASE_SA_EMAIL}" \
    "  Not found service account:  ${BASE_SA_EMAIL}"
}

test_deploy_delete_tier_off_proxy_sa_delete_error_prints_error() {
  local text
  text="$(_base_delete_error_text iam.service-accounts.delete)"
  fresh_gcloud_state
  set_service_account_delete_will_fail "$BASE_PROXY_SA_EMAIL"
  set_service_account_delete_error_text "$BASE_PROXY_SA_EMAIL" "$text"
  run_deploy_delete "$(base_config_json "$HUB")"
  _assert_base_delete_kept "Proxy service account ${BASE_PROXY_SA_EMAIL}" \
    "  Kept proxy SA:              ${BASE_PROXY_SA_EMAIL}" "$text"
}

test_deploy_delete_tier_off_proxy_sa_delete_not_found_reports_not_found() {
  fresh_gcloud_state
  set_service_account_delete_will_fail "$BASE_PROXY_SA_EMAIL"
  set_service_account_delete_error_text "$BASE_PROXY_SA_EMAIL" \
    "ERROR: (gcloud.iam.service-accounts.delete) NOT_FOUND: Unknown service account"
  run_deploy_delete "$(base_config_json "$HUB")"
  _assert_base_delete_not_found "Proxy service account ${BASE_PROXY_SA_EMAIL}" \
    "  Not found proxy SA:         ${BASE_PROXY_SA_EMAIL}"
}

test_deploy_delete_tier_off_iap_ssh_rule_delete_error_prints_error() {
  local text
  text="$(_base_delete_error_text compute.firewall-rules.delete)"
  fresh_gcloud_state
  set_firewall_delete_will_fail "$BASE_FW_IAP_SSH"
  set_firewall_delete_error_text "$BASE_FW_IAP_SSH" "$text"
  run_deploy_delete "$(base_config_json "$HUB")"
  _assert_base_delete_kept "Firewall rule ${BASE_FW_IAP_SSH}" \
    "  Kept firewall rule:         ${BASE_FW_IAP_SSH}" "$text"
}

test_deploy_delete_tier_off_iap_ssh_rule_delete_not_found_reports_not_found() {
  fresh_gcloud_state
  set_firewall_delete_will_fail "$BASE_FW_IAP_SSH"
  set_firewall_delete_error_text "$BASE_FW_IAP_SSH" \
    "$(_base_delete_not_found_text compute.firewall-rules.delete "$BASE_FW_IAP_SSH")"
  run_deploy_delete "$(base_config_json "$HUB")"
  _assert_base_delete_not_found "Firewall rule ${BASE_FW_IAP_SSH}" \
    "  Not found firewall rule:    ${BASE_FW_IAP_SSH}"
}

test_deploy_delete_tier_off_proxy_rule_delete_error_prints_error() {
  local text
  text="$(_base_delete_error_text compute.firewall-rules.delete)"
  fresh_gcloud_state
  set_firewall_delete_will_fail "$BASE_FW_PROXY"
  set_firewall_delete_error_text "$BASE_FW_PROXY" "$text"
  run_deploy_delete "$(base_config_json "$HUB")"
  _assert_base_delete_kept "Firewall rule ${BASE_FW_PROXY}" \
    "  Kept firewall rule:         ${BASE_FW_PROXY}" "$text"
}

test_deploy_delete_tier_off_proxy_rule_delete_not_found_reports_not_found() {
  fresh_gcloud_state
  set_firewall_delete_will_fail "$BASE_FW_PROXY"
  set_firewall_delete_error_text "$BASE_FW_PROXY" \
    "$(_base_delete_not_found_text compute.firewall-rules.delete "$BASE_FW_PROXY")"
  run_deploy_delete "$(base_config_json "$HUB")"
  _assert_base_delete_not_found "Firewall rule ${BASE_FW_PROXY}" \
    "  Not found firewall rule:    ${BASE_FW_PROXY}"
}

test_deploy_delete_tier_off_vm_not_found_error_reports_not_found() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "us-central1-b"
  set_instance_delete_will_fail "$INSTANCE_NAME"
  set_instance_delete_error_text "$INSTANCE_NAME" \
    "ERROR: (gcloud.compute.instances.delete) Could not fetch resource: - The resource 'projects/x/zones/us-central1-b/instances/${INSTANCE_NAME}' was not found (code=404)"
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_eq "0" "$DEPLOY_RC" "a confirmed not-found VM delete error must still warn and continue"
  assert_contains "$DEPLOY_LOG" "Not found GCE VM:           ${INSTANCE_NAME}" \
    "a delete error that is a positive not-found must still be reported as Not found"
}

# =====================================================================
# A tier-off redeploy against an existing VM makes no hybrid
# mutating calls at all -- not even an add-tags for the hybrid-tier tag.
# =====================================================================

test_deploy_create_tier_off_existing_vm_untouched() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "us-central1-b"
  run_deploy_create "$(base_config_json "$HUB")"
  local log
  log="$(gcloud_log)"
  assert_eq "0" "$(echo "$log" | grep -c 'compute instances create' || true)" \
    "an existing VM must not be recreated"
  assert_eq "0" "$(echo "$log" | grep 'instances add-tags' | grep -c -- "scion-hub-${HUB}-nfs" || true)" \
    "tier-off redeploy on an existing VM must not add the hybrid-tier tag"
  assert_eq "0" "$(echo "$log" | grep -c 'firewall-rules.*nfs-' || true)" \
    "tier-off redeploy must make no hybrid firewall-rule calls"
  assert_eq "0" "$(echo "$log" | grep -c 'container clusters' || true)" \
    "tier-off redeploy must make no cluster calls"
}

# =====================================================================
# The deploy-level teardown result (exit code, summary) is pinned
# for a hybrid rule delete failure specifically, distinct from a VM
# delete failure.
# =====================================================================

test_deploy_delete_hybrid_rule_delete_failure_exits_nonzero_excluded_from_summary() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "us-central1-b"
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-allow" "scion-deployment=${HUB}" \
    "default" "INGRESS" "ALLOW" "tcp" "2049" "gke-x-node" "" "scion-hub-${HUB}-nfs" "900"
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-deny" "scion-deployment=${HUB}" \
    "default" "INGRESS" "DENY" "tcp" "2049" "" "0.0.0.0/0" "scion-hub-${HUB}-nfs" "950"
  set_firewall_delete_will_fail "scion-hub-${HUB}-nfs-allow"
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_eq "1" "$DEPLOY_RC" "a hybrid rule delete failure must fail the run (distinct from a VM delete failure)"
  assert_not_contains "$DEPLOY_LOG" "Deleted firewall rule:     scion-hub-${HUB}-nfs-allow" \
    "the summary must not claim the failed rule was deleted"
}

# The "no python needed when nothing matched" case is pinned at the
# function level (test_teardown_check_no_python_needed_when_nothing_
# matches in test_hybrid_tier.sh) and, here, at the deploy.sh level too,
# via the interactive (no --config) path: without --config, deploy.sh
# never touches $PYTHON before the teardown ownership check runs, so a
# missing interpreter is not fatal as long as there is nothing for the
# check to actually parse.
test_deploy_delete_interactive_no_python_no_rules_exits_zero() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "us-central1-b"
  run_deploy_delete_interactive "/nonexistent/python3" "$(printf '%s\n' "$HUB" "us-central1" "y")"
  assert_eq "0" "$DEPLOY_RC" \
    "an interactive --delete with no config and no hybrid rules must not need python at all"
  assert_contains "$DEPLOY_LOG" "Deleted: ${INSTANCE_NAME}" "the base VM delete must still complete"
  assert_eq "1" "$(gcloud_log | grep -c "compute firewall-rules list" || true)" \
    "the teardown ownership check must actually run on the interactive path too, not be skipped"
}

# An unmarked same-name rule must still abort the teardown before any
# delete on the interactive (no --config) path, exactly as it does with
# --config -- the ownership check runs the same way regardless of how the
# run is driven.
test_deploy_delete_interactive_unmarked_exits_nonzero_before_any_delete() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "us-central1-b"
  seed_firewall_rule_desc_only "scion-hub-${HUB}-nfs-allow" "unrelated-rule-not-ours"
  run_deploy_delete_interactive "" "$(printf '%s\n' "$HUB" "us-central1" "y")"
  assert_eq "1" "$DEPLOY_RC" "an unmarked hybrid rule name match must fail the interactive teardown too"
  assert_eq "0" "$(gcloud_log | grep -c ' delete' || true)" \
    "no delete call of any kind should be logged before the abort"
}

# =====================================================================
# Kubernetes objects (marker-scoped teardown wiring): tier-off inertness, the
# cluster-gone/cluster-error distinction, and the abort-before-any-
# delete rule for an unmarked PV/PVC.
# =====================================================================

K8S_NS_D="scion-hub-${HUB}"
K8S_PVC_D="scion-hub-${HUB}-shared"
K8S_PV_D="scion-hub-${HUB}-shared"

test_deploy_create_k8s_preflight_unmarked_pv_aborts_before_any_create() {
  fresh_gcloud_state
  seed_cluster "mycluster" "default"
  seed_k8s_pv_unmarked "$K8S_PV_D"
  local deploy_tmpdir
  deploy_tmpdir="$(mktemp -d)"
  run_deploy_create_until_exit \
    "$(base_config_json "$HUB" "$(hybrid_config_fragment)" "registry" "us-docker.pkg.dev/demo-project/scion")" \
    "$deploy_tmpdir"
  rm -rf "$deploy_tmpdir"
  assert_eq "true" "$EXITED_ON_OWN" "deploy.sh must exit on its own: ${DEPLOY_LOG}"
  assert_true "$([[ "$DEPLOY_RC" -ne 0 ]] && echo true || echo false)" \
    "an unmarked PV must fail the whole run, not just the eventual k8s object creation"
  assert_eq "0" "$(gcloud_log | grep -c 'compute instances create' || true)" \
    "the VM must never be created when the k8s preflight refuses"
  assert_eq "0" "$(gcloud_log | grep -c 'compute addresses create' || true)" \
    "the internal IP must never be reserved when the k8s preflight refuses"
  assert_eq "0" "$(gcloud_log | grep -c 'firewall-rules create' || true)" \
    "no hybrid firewall rule may be created when the k8s preflight refuses"
}

test_deploy_delete_k8s_tier_off_zero_kubectl_calls() {
  fresh_gcloud_state
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_eq "0" "$DEPLOY_RC" "tier-off teardown must succeed as before"
  assert_eq "0" "$(kubectl_log | grep -c . || true)" "tier-off teardown must make zero kubectl calls"
}

test_deploy_delete_k8s_cluster_not_found_continues() {
  fresh_gcloud_state
  run_deploy_delete "$(base_config_json "$HUB" "$(hybrid_config_fragment)")"
  assert_eq "0" "$DEPLOY_RC" "a genuinely-gone cluster must not fail the teardown"
  assert_eq "0" "$(kubectl_log | grep -c . || true)" \
    "a NOT_FOUND cluster means its k8s objects went with it -- no kubectl call should even be attempted"
  assert_contains "$DEPLOY_LOG" "went with it" "should explain why nothing was checked"
}

test_deploy_delete_k8s_cluster_describe_error_aborts_before_any_delete() {
  fresh_gcloud_state
  set_cluster_describe_error "mycluster"
  run_deploy_delete "$(base_config_json "$HUB" "$(hybrid_config_fragment)")"
  assert_eq "1" "$DEPLOY_RC" "an unconfirmable cluster state must fail the teardown"
  assert_eq "0" "$(gcloud_log | grep -c ' delete' || true)" "no delete call of any kind should happen before the abort"
}

test_deploy_delete_k8s_cluster_permission_masked_not_read_as_gone() {
  fresh_gcloud_state
  set_cluster_describe_permission_masked "mycluster"
  run_deploy_delete "$(base_config_json "$HUB" "$(hybrid_config_fragment)")"
  assert_eq "1" "$DEPLOY_RC" \
    "a permission-denied cluster-describe error that also says 'not found' must not be read as the cluster being gone"
  assert_eq "0" "$(gcloud_log | grep -c ' delete' || true)" "no delete call of any kind should happen before the abort"
}

test_deploy_delete_k8s_all_marked_deletes_all_three() {
  fresh_gcloud_state
  seed_cluster "mycluster" "default"
  seed_k8s_pvc "$K8S_PVC_D" "$K8S_NS_D" "$HUB" "$K8S_PV_D"
  seed_k8s_pv "$K8S_PV_D" "$HUB" "10.128.0.5" "/srv/scion-shared" "$K8S_NS_D" "$K8S_PVC_D"
  seed_k8s_namespace "$K8S_NS_D" "$HUB"
  run_deploy_delete "$(base_config_json "$HUB" "$(hybrid_config_fragment)")"
  assert_eq "0" "$DEPLOY_RC" "an all-marked k8s teardown must succeed"
  assert_eq "1" "$(kubectl_log | grep -c 'delete pvc' || true)" "the PVC must be deleted"
  assert_eq "1" "$(kubectl_log | grep -c 'delete pv ' || true)" "the PV must be deleted"
  assert_eq "1" "$(kubectl_log | grep -c 'delete namespace' || true)" "the namespace must be deleted"
}

test_deploy_delete_k8s_delete_failure_stops_all_downstream_deletes() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "us-central1-b"
  seed_cluster "mycluster" "default"
  seed_k8s_pvc "$K8S_PVC_D" "$K8S_NS_D" "$HUB" "$K8S_PV_D"
  seed_k8s_pv "$K8S_PV_D" "$HUB" "10.128.0.5" "/srv/scion-shared" "$K8S_NS_D" "$K8S_PVC_D"
  seed_k8s_namespace "$K8S_NS_D" "$HUB"
  set_k8s_delete_will_fail "pvc" "${K8S_NS_D}__${K8S_PVC_D}"
  run_deploy_delete "$(base_config_json "$HUB" "$(hybrid_config_fragment)")"
  assert_eq "1" "$DEPLOY_RC" "a kubectl delete failure must fail the whole teardown"
  assert_eq "0" "$(kubectl_log | grep -c 'delete pv ' || true)" \
    "the PV must not be deleted once the PVC delete ahead of it failed"
  assert_eq "0" "$(kubectl_log | grep -c 'delete namespace' || true)" \
    "the namespace must not be deleted once an earlier k8s delete failed"
  assert_eq "0" "$(gcloud_log | grep -c 'instances delete' || true)" \
    "the VM must not be deleted when a hybrid-tier Kubernetes object failed to delete"
  assert_eq "0" "$(gcloud_log | grep -c 'run services delete' || true)" \
    "Cloud Run must not be deleted when a hybrid-tier Kubernetes object failed to delete"
  assert_eq "0" "$(gcloud_log | grep -c 'firewall-rules delete' || true)" \
    "no firewall rule may be deleted when a hybrid-tier Kubernetes object failed to delete"
}

test_deploy_delete_k8s_failure_reports_hybrid_resources_skipped_not_kept() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "us-central1-b"
  seed_cluster "mycluster" "default"
  seed_k8s_pvc "$K8S_PVC_D" "$K8S_NS_D" "$HUB" "$K8S_PV_D"
  seed_k8s_pv "$K8S_PV_D" "$HUB" "10.128.0.5" "/srv/scion-shared" "$K8S_NS_D" "$K8S_PVC_D"
  seed_k8s_namespace "$K8S_NS_D" "$HUB"
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-allow" "scion-deployment=${HUB}" \
    "default" "INGRESS" "ALLOW" "tcp" "2049" "gke-x-node" "" "scion-hub-${HUB}-nfs" "900"
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-deny" "scion-deployment=${HUB}" \
    "default" "INGRESS" "DENY" "tcp" "2049" "" "0.0.0.0/0" "scion-hub-${HUB}-nfs" "950"
  seed_address "scion-hub-${HUB}-internal-ip" "10.128.0.42" "scion-deployment=${HUB}"
  set_k8s_delete_will_fail "pvc" "${K8S_NS_D}__${K8S_PVC_D}"
  run_deploy_delete "$(base_config_json "$HUB" "$(hybrid_config_fragment)")"
  assert_eq "1" "$DEPLOY_RC" "a kubectl delete failure must fail the whole teardown"
  assert_contains "$DEPLOY_LOG" "SKIPPED firewall rule:      scion-hub-${HUB}-nfs-allow (a hybrid-tier Kubernetes object failed to delete)" \
    "a hybrid firewall rule never attempted because k8s failed first must be reported SKIPPED, not Kept"
  assert_contains "$DEPLOY_LOG" "SKIPPED firewall rule:      scion-hub-${HUB}-nfs-deny (a hybrid-tier Kubernetes object failed to delete)" \
    "the deny rule must be reported the same way"
  assert_contains "$DEPLOY_LOG" "SKIPPED internal IP:       scion-hub-${HUB}-internal-ip (a hybrid-tier Kubernetes object failed to delete)" \
    "the internal IP reservation must be reported SKIPPED, not Kept (delete failed: unknown error), when it was never attempted"
  assert_not_contains "$DEPLOY_LOG" "Kept internal IP" \
    "must never claim a delete was attempted and failed when it was never attempted at all"
}

test_deploy_delete_vm_not_gone_reports_hybrid_rules_skipped() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "us-central1-b"
  set_instance_delete_will_fail "$INSTANCE_NAME"
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-allow" "scion-deployment=${HUB}" \
    "default" "INGRESS" "ALLOW" "tcp" "2049" "gke-x-node" "" "scion-hub-${HUB}-nfs" "900"
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-deny" "scion-deployment=${HUB}" \
    "default" "INGRESS" "DENY" "tcp" "2049" "" "0.0.0.0/0" "scion-hub-${HUB}-nfs" "950"
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_eq "1" "$DEPLOY_RC" "a VM delete failure must fail the whole teardown"
  assert_contains "$DEPLOY_LOG" "SKIPPED firewall rule:      scion-hub-${HUB}-nfs-allow (VM not confirmed gone)" \
    "a hybrid rule never attempted because the VM isn't confirmed gone must be reported SKIPPED, not Kept"
  assert_contains "$DEPLOY_LOG" "SKIPPED firewall rule:      scion-hub-${HUB}-nfs-deny (VM not confirmed gone)" \
    "the deny rule must be reported the same way"
}

test_deploy_delete_hybrid_rule_delete_failure_reports_downstream_rule_kept() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "us-central1-b"
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-allow" "scion-deployment=${HUB}" \
    "default" "INGRESS" "ALLOW" "tcp" "2049" "gke-x-node" "" "scion-hub-${HUB}-nfs" "900"
  seed_firewall_rule_json "scion-hub-${HUB}-nfs-deny" "scion-deployment=${HUB}" \
    "default" "INGRESS" "DENY" "tcp" "2049" "" "0.0.0.0/0" "scion-hub-${HUB}-nfs" "950"
  set_firewall_delete_will_fail "scion-hub-${HUB}-nfs-allow"
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_eq "1" "$DEPLOY_RC" "a hybrid firewall rule delete failure must fail the whole teardown"
  assert_contains "$DEPLOY_LOG" "Kept firewall rule:         scion-hub-${HUB}-nfs-allow (delete failed, or not attempted after an earlier rule's delete failed)" \
    "the rule whose delete actually failed must be reported Kept"
  assert_contains "$DEPLOY_LOG" "Kept firewall rule:         scion-hub-${HUB}-nfs-deny (delete failed, or not attempted after an earlier rule's delete failed)" \
    "the deny rule, never attempted because the allow rule ahead of it failed, must also be reported Kept, not silently dropped from the summary"
}

test_deploy_delete_k8s_deletes_precede_vm_delete() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "us-central1-b"
  seed_cluster "mycluster" "default"
  seed_k8s_pvc "$K8S_PVC_D" "$K8S_NS_D" "$HUB" "$K8S_PV_D"
  seed_k8s_pv "$K8S_PV_D" "$HUB" "10.128.0.5" "/srv/scion-shared" "$K8S_NS_D" "$K8S_PVC_D"
  seed_k8s_namespace "$K8S_NS_D" "$HUB"
  run_deploy_delete "$(base_config_json "$HUB" "$(hybrid_config_fragment)")"
  assert_eq "0" "$DEPLOY_RC" "an all-marked k8s teardown followed by the VM must succeed"
  local k8s_line vm_line
  k8s_line="$(line_number "Deleting hybrid-tier Kubernetes objects" "$DEPLOY_LOG")"
  vm_line="$(line_number "Deleting GCE VM" "$DEPLOY_LOG")"
  assert_true "$([[ -n "$k8s_line" && -n "$vm_line" && "$k8s_line" -lt "$vm_line" ]] && echo true || echo false)" \
    "the hybrid-tier Kubernetes deletes must run, and be logged, before the VM delete starts"
}

test_deploy_delete_k8s_unmarked_pv_aborts_before_any_delete() {
  fresh_gcloud_state
  seed_cluster "mycluster" "default"
  seed_k8s_pv_unmarked "$K8S_PV_D"
  run_deploy_delete "$(base_config_json "$HUB" "$(hybrid_config_fragment)")"
  assert_eq "1" "$DEPLOY_RC" "an unmarked PV must abort the whole teardown"
  assert_eq "0" "$(gcloud_log | grep -c ' delete' || true)" "no delete call of any kind should happen before the abort"
  assert_eq "0" "$(kubectl_log | grep -c 'delete' || true)" "no kubectl delete call should happen before the abort either"
}

# =====================================================================
# Past VM create, into Phase 3: extends coverage past the phase2-complete
# sentinel, using GCLOUD_STUB_SSH_SUCCEEDS so the stub actually answers
# `compute ssh`/`compute scp` instead of failing immediately. Stops at
# the settings.yaml dev-mode write -- the earliest point where the
# hybrid tier's NFS squash-identity/export SSH calls (if the tier is on)
# and the HYBRID_SHARED_DIR_STORAGE_YAML computation have both already
# happened, but before Phase 3b's image build/push work, which this
# stub does not simulate.
# =====================================================================

test_deploy_create_squash_script_non_numeric_output_fails_before_any_write() {
  fresh_gcloud_state
  seed_cluster "mycluster" "default"
  local config_file
  config_file="$(mktemp)"
  printf '%s' "$(base_config_json "$HUB" "$(hybrid_config_fragment)" "registry" "us-docker.pkg.dev/demo-project/scion")" > "$config_file"
  local log rc
  log="$(GCLOUD_STUB_SSH_SUCCEEDS=true GCLOUD_STUB_SSH_SQUASH_IDS="abc" \
    timeout 60 bash "$DEPLOY_SH" --config "$config_file" --version v1.0.0-test < /dev/null 2>&1)"
  rc=$?
  rm -f "$config_file"
  assert_true "$([[ "$rc" -ne 0 && "$rc" -ne 124 ]] && echo true || echo false)" \
    "non-numeric output from the squash-identity script must fail deploy.sh, not hang or be silently accepted"
  assert_contains "$log" "Unexpected output from the NFS squash identity script" "the message should explain why"
  assert_not_contains "$log" "schema_version" "settings.yaml must never be written after this refusal"
  assert_eq "0" "$(echo "$log" | grep -c 'exports.d' || true)" \
    "the NFS export must never be written after this refusal"
}

test_deploy_create_squash_script_uid_zero_fails_before_any_write() {
  fresh_gcloud_state
  seed_cluster "mycluster" "default"
  local config_file
  config_file="$(mktemp)"
  printf '%s' "$(base_config_json "$HUB" "$(hybrid_config_fragment)" "registry" "us-docker.pkg.dev/demo-project/scion")" > "$config_file"
  local log rc
  log="$(GCLOUD_STUB_SSH_SUCCEEDS=true GCLOUD_STUB_SSH_SQUASH_IDS="0:1001" \
    timeout 60 bash "$DEPLOY_SH" --config "$config_file" --version v1.0.0-test < /dev/null 2>&1)"
  rc=$?
  rm -f "$config_file"
  assert_true "$([[ "$rc" -ne 0 && "$rc" -ne 124 ]] && echo true || echo false)" \
    "a squash uid of 0 must fail deploy.sh, not be accepted as the anonymous NFS uid"
  assert_contains "$log" "refusing to export with root as the anonymous uid" "the message should explain why"
  assert_not_contains "$log" "schema_version" "settings.yaml must never be written after this refusal"
  assert_eq "0" "$(echo "$log" | grep -c 'exports.d' || true)" \
    "the NFS export must never be written after this refusal"
}

test_deploy_create_tier_off_reaches_settings_yaml_with_no_hybrid_ssh_calls() {
  fresh_gcloud_state
  run_deploy_create_to_settings_yaml "$(base_config_json "$HUB")"
  assert_eq "true" "$DEPLOY_REACHED_SETTINGS_YAML" "a tier-off create must reach the settings.yaml write"
  local log
  log="$(gcloud_log)"
  assert_eq "0" "$(echo "$log" | grep -c 'useradd -r -M -N -g scion' || true)" \
    "tier off must never run the NFS squash-identity script"
  assert_eq "0" "$(echo "$log" | grep -c 'mkfs.ext4' || true)" \
    "tier off must never run the NFS export script"
  assert_not_contains "$log" "shared_dir_storage" \
    "the tier-off settings.yaml write must not carry the shared_dir_storage block"
  assert_eq "0" "$(kubectl_log | grep -c . || true)" "tier off must make zero kubectl calls"
}

# settings_yaml_template_tier_off N — prints the Nth settings.yaml
# heredoc body in deploy.sh's source (1 is the Phase-3 dev-mode write, 2
# the Phase-5 proxy-mode write) with the hybrid-tier splices cut out of
# the source text. The three splices (${HYBRID_X:+${HYBRID_X}<newline>})
# are cut as one exact fragment that must start its own line and be
# followed directly by the listen_port line, not expanded with empty
# variables. A splice that would leave a stray line or character behind,
# or any text between the splices, on their lines, or between them and
# the listen_port line, therefore stays in the template and fails the
# comparison with the render. If the fragment is not found, the output
# is an error line, not the template, which also fails the comparison.
# The identity-mode splice (${HYBRID_GCP_IDENTITY_YAML:-"<comment and
# key>"}) is cut the same way: its opening must directly follow the
# image_registry line, and its close must directly follow the
# "default_gcp_identity_mode: passthrough" line and precede "server:",
# leaving the comment and key text between them.
settings_yaml_template_tier_off() {
  local n="$1" body var group="" registry_line key_line open close key_server
  body="$(awk -v n="$n" "/<< 'SETTINGSEOF'/{c++; if (c == n) {flag=1; next}} /^SETTINGSEOF\$/{flag=0} flag" "$DEPLOY_SH"; echo x)"
  body="${body%x}"
  registry_line="image_registry: \\\"\${IMAGE_REGISTRY}\\\""$'\n'
  key_line=$'\n''default_gcp_identity_mode: passthrough'$'\n'
  open="${registry_line}\${HYBRID_GCP_IDENTITY_YAML:-\""
  close="${key_line}"'"}server:'$'\n'
  key_server="${key_line}server:"$'\n'
  if [[ "$body" != *"$open"*"$close"* ]]; then
    echo "settings.yaml heredoc ${n} in deploy.sh has no identity-mode splice around a passthrough key"
    return 1
  fi
  body="${body/"$open"/$registry_line}"
  body="${body/"$close"/$key_server}"
  for var in HYBRID_AUTH_TRANSPORT_YAML HYBRID_USER_ACCESS_YAML HYBRID_SHARED_DIR_STORAGE_YAML; do
    group="${group}\${${var}:+\${${var}}"$'\n'"}"
  done
  group=$'\n'"${group}  listen_port: 8080"$'\n'
  if [[ "$body" != *"$group"* ]]; then
    echo "settings.yaml heredoc ${n} in deploy.sh has no contiguous hybrid-tier splice fragment before listen_port"
    return 1
  fi
  body="${body/"$group"/$'\n'  listen_port: 8080$'\n'}"
  printf '%s' "$body"
}

# render_settings_yaml_template TEMPLATE — expands TEMPLATE the way the
# double-quoted --command="..." argument that carries it in deploy.sh
# does, using the caller's variables.
render_settings_yaml_template() {
  eval "printf '%s' \"$1\""
}

# The test above only checks that a couple of hybrid-specific strings are
# absent; it says nothing about whether the rest of the rendered
# settings.yaml is exactly right, or whether the ${HYBRID_*:+...} splices
# leave any stray blank line or indentation artifact behind when they
# expand to nothing. This extracts the actual heredoc body deploy.sh sent
# over SSH (between the `<< 'SETTINGSEOF'` and closing `SETTINGSEOF`
# lines in the captured --command= text) and compares it byte-for-byte
# against deploy.sh's own heredoc template with the hybrid-tier splices
# cut out, rendered with this test's config values.
test_deploy_create_tier_off_settings_yaml_byte_identical() {
  fresh_gcloud_state
  run_deploy_create_to_settings_yaml "$(base_config_json "$HUB")"
  assert_eq "true" "$DEPLOY_REACHED_SETTINGS_YAML" "a tier-off create must reach the settings.yaml write"
  local log actual template expected
  log="$(gcloud_log)"
  # Command substitution strips ALL trailing newlines, which would hide
  # a stray trailing blank line the splice mechanism might leave behind.
  # Appending a sentinel character and stripping only that (not "$(...)"
  # itself) preserves any real trailing newline in both actual and
  # expected.
  actual="$(echo "$log" | awk "/<< 'SETTINGSEOF'/{flag=1; next} /^SETTINGSEOF\$/{flag=0} flag"; echo x)"
  actual="${actual%x}"
  template="$(settings_yaml_template_tier_off 1; echo x)"
  template="${template%x}"
  assert_not_contains "$template" "HYBRID_" \
    "the dev-mode heredoc template must carry no hybrid-tier reference outside the splices"
  expected="$(IMAGE_REGISTRY="localhost/scion" HUB_NAME="demohub" ADMIN_EMAIL="admin@example.com" \
    RELEASE_CHANNEL="nightly" UPDATE_POLICY="auto" render_settings_yaml_template "$template"; echo x)"
  expected="${expected%x}"
  assert_contains "$expected" "  listen_port: 8080" "the rendered template must reach the listen_port line"
  assert_eq "$expected" "$actual" \
    "a tier-off settings.yaml must be byte-for-byte identical to a render with no hybrid-tier splice at all"
  # The comparison above takes the fixed template text from deploy.sh
  # itself, so these pin a few rendered values and the auth block's end.
  assert_contains "$actual" '    name: "demohub"' "the dev-mode write must carry the hub name"
  assert_contains "$actual" '    release_channel: "nightly"' "the dev-mode write must carry the release channel"
  assert_contains "$actual" '    update_policy: "auto"' "the dev-mode write must carry the update policy"
  assert_contains "$actual" $'  auth:\n    mode: dev\n  listen_port: 8080\n' \
    "in the dev-mode write, the listen_port line must directly follow the auth block"
}

# The dev-mode write above is what deploy.sh's own Phase 3 uses on every
# create; production auth is proxy mode (IAP), written later in Phase 5
# once the Cloud Run proxy exists -- an entirely separate heredoc in
# deploy.sh, unpinned by the dev-mode test above.
test_deploy_create_tier_off_proxy_settings_yaml_byte_identical() {
  fresh_gcloud_state
  run_deploy_create_to_proxy_settings_yaml \
    "$(base_config_json "$HUB" "" "registry" "us-docker.pkg.dev/demo-project/scion")"
  assert_eq "true" "$DEPLOY_REACHED_SETTINGS_YAML" "a tier-off create must reach the Phase-5 proxy-mode settings.yaml write"
  local log actual template expected
  log="$(gcloud_log)"
  # Two settings.yaml writes are logged by this point (Phase 3 dev-mode,
  # then Phase 5 proxy-mode); take the LAST one.
  actual="$(echo "$log" | awk "/<< 'SETTINGSEOF'/{flag=1; buf=\"\"; next} /^SETTINGSEOF\$/{flag=0; last=buf} flag{buf=buf \$0 ORS} END{printf \"%s\", last}"; echo x)"
  actual="${actual%x}"
  template="$(settings_yaml_template_tier_off 2; echo x)"
  template="${template%x}"
  assert_not_contains "$template" "HYBRID_" \
    "the proxy-mode heredoc template must carry no hybrid-tier reference outside the splices"
  expected="$(IMAGE_REGISTRY="us-docker.pkg.dev/demo-project/scion" HUB_NAME="demohub" ADMIN_EMAIL="admin@example.com" \
    RELEASE_CHANNEL="nightly" UPDATE_POLICY="auto" \
    IAP_AUDIENCE="/projects/123456789012/locations/us-central1/services/scion-hub-demohub-iap-proxy" \
    render_settings_yaml_template "$template"; echo x)"
  expected="${expected%x}"
  assert_contains "$expected" "    mode: proxy" "the rendered template must be the proxy-mode write"
  assert_eq "$expected" "$actual" \
    "a tier-off proxy-mode settings.yaml must be byte-for-byte identical to a render with no hybrid-tier splice at all"
  # As in the dev-mode test, these pin a few rendered values and the auth
  # block's end independently of the template.
  assert_contains "$actual" '    name: "demohub"' "the proxy-mode write must carry the hub name"
  assert_contains "$actual" '    release_channel: "nightly"' "the proxy-mode write must carry the release channel"
  assert_contains "$actual" '    update_policy: "auto"' "the proxy-mode write must carry the update policy"
  assert_contains "$actual" $'        audience: "/projects/123456789012/locations/us-central1/services/scion-hub-demohub-iap-proxy"\n  listen_port: 8080\n' \
    "in the proxy-mode write, the listen_port line must directly follow the auth block"
}

test_deploy_create_tier_on_proxy_settings_yaml_has_shared_dir_storage_block() {
  fresh_gcloud_state
  seed_cluster "mycluster" "default"
  run_deploy_create_to_proxy_settings_yaml \
    "$(base_config_json "$HUB" "$(hybrid_config_fragment)" "registry" "us-docker.pkg.dev/demo-project/scion")"
  assert_eq "true" "$DEPLOY_REACHED_SETTINGS_YAML" "a tier-on create must reach the Phase-5 proxy-mode settings.yaml write"
  local log proxy_heredoc
  log="$(gcloud_log)"
  # The log also contains Phase 3's dev-mode write, which carries its own
  # shared_dir_storage block -- extracting only the LAST (proxy-mode)
  # heredoc keeps this test from passing merely because the earlier,
  # unrelated dev-mode write still has the block.
  proxy_heredoc="$(echo "$log" | awk "/<< 'SETTINGSEOF'/{flag=1; buf=\"\"; next} /^SETTINGSEOF\$/{flag=0; last=buf} flag{buf=buf \$0 ORS} END{printf \"%s\", last}"; echo x)"
  proxy_heredoc="${proxy_heredoc%x}"
  assert_contains "$proxy_heredoc" "mode: proxy" "the proxy-mode write must actually carry proxy auth, not dev"
  assert_eq "1" "$(printf '%s' "$proxy_heredoc" | grep -cx 'default_gcp_identity_mode: block' || true)" \
    "the tier-on proxy-mode write must carry exactly one top-level default_gcp_identity_mode: block line"
  assert_eq "1" "$(printf '%s' "$proxy_heredoc" | grep -cE '^[[:space:]]*default_gcp_identity_mode[[:space:]]*:' || true)" \
    "the tier-on proxy-mode write must carry exactly one default_gcp_identity_mode key"
  assert_not_contains "$proxy_heredoc" "passthrough" \
    "the tier-on proxy-mode write must carry no passthrough identity-mode comment or value"
  assert_contains "$proxy_heredoc" "shared_dir_storage:" \
    "the tier-on proxy-mode settings.yaml write must carry the shared_dir_storage block -- an IAP-mode hub must not silently lose it"
  assert_contains "$proxy_heredoc" 'server: "10.128.0.9"' \
    "the proxy-mode shared_dir_storage server field must be the reserved internal IP"
  # hybrid_k8s_ensure_objects (Phase 4, after VM_IP is final) is what
  # actually creates the PV/PVC a tier-on deploy depends on -- nothing
  # before this point in the flow asserts that it was ever called at all.
  assert_true "$([[ -f "${KUBECTL_STUB_STATE_DIR}/pv/scion-hub-${HUB}-shared.json" ]] && echo true || echo false)" \
    "hybrid_k8s_ensure_objects must actually create the PV when the tier is on"
  assert_true "$([[ -f "${KUBECTL_STUB_STATE_DIR}/pvc/scion-hub-${HUB}__scion-hub-${HUB}-shared.json" ]] && echo true || echo false)" \
    "hybrid_k8s_ensure_objects must actually create the PVC when the tier is on"
}

test_deploy_create_tier_on_reaches_settings_yaml_with_correct_nfs_and_block() {
  fresh_gcloud_state
  seed_cluster "mycluster" "default"
  run_deploy_create_to_settings_yaml \
    "$(base_config_json "$HUB" "$(hybrid_config_fragment)" "registry" "us-docker.pkg.dev/demo-project/scion")"
  assert_eq "true" "$DEPLOY_REACHED_SETTINGS_YAML" "a tier-on create must reach the settings.yaml write"
  local log dev_heredoc
  log="$(gcloud_log)"
  # Extracted the same way the proxy-mode test extracts its own heredoc
  # (see test_deploy_create_tier_on_proxy_settings_yaml_has_shared_dir_storage_block),
  # but taking the FIRST occurrence rather than the last: this call stops
  # right at the dev-mode sentinel, so in practice the log holds only
  # this one heredoc, but asserting against the specifically-extracted
  # dev-mode block (not the whole log) keeps that true by construction
  # rather than by this call happening to stop early enough.
  dev_heredoc="$(echo "$log" | awk "/<< 'SETTINGSEOF'/{flag=1; buf=\"\"; next} /^SETTINGSEOF\$/{flag=0; if (!seen) {first=buf; seen=1}} flag{buf=buf \$0 ORS} END{printf \"%s\", first}"; echo x)"
  dev_heredoc="${dev_heredoc%x}"
  assert_eq "1" "$(printf '%s' "$dev_heredoc" | grep -cx 'default_gcp_identity_mode: block' || true)" \
    "the tier-on dev-mode write must carry exactly one top-level default_gcp_identity_mode: block line"
  assert_eq "1" "$(printf '%s' "$dev_heredoc" | grep -cE '^[[:space:]]*default_gcp_identity_mode[[:space:]]*:' || true)" \
    "the tier-on dev-mode write must carry exactly one default_gcp_identity_mode key"
  assert_not_contains "$dev_heredoc" "passthrough" \
    "the tier-on dev-mode write must carry no passthrough identity-mode comment or value"

  assert_contains "$log" "useradd -r -M -N -g scion -s /usr/sbin/nologin scion-nfs" \
    "the squash-identity script must actually be sent over SSH when the tier is on"

  # The default node-subnet CIDR the gcloud stub serves when a test
  # doesn't override it via seed_subnet -- see tests/lib/gcloud.
  assert_contains "$log" "10.128.0.0/20" \
    "the export line's CIDR must be the discovered node subnet, wired through correctly from hybrid_discover"
  assert_contains "$log" "anonuid=997,anongid=1001" \
    "the export line's anonuid/anongid must be exactly what the squash script returned, wired through unchanged"

  assert_contains "$dev_heredoc" "shared_dir_storage:" \
    "the tier-on settings.yaml write must carry the shared_dir_storage block"
  assert_contains "$dev_heredoc" 'mount_root: "/srv"' \
    "mount_root must be the export root's parent, not the full export root"
  assert_contains "$dev_heredoc" 'id: "scion-shared"' \
    "the share id must be the export root's base name"
  assert_contains "$dev_heredoc" 'pv_name: "scion-hub-demohub-shared"' \
    "pv_name must carry the resolved PVC name"

  assert_true "$([[ -f "${KUBECTL_STUB_STATE_DIR}/namespace/scion-hub-${HUB}.json" ]] && echo true || echo false)" \
    "hybrid_k8s_preflight must have created the namespace in Phase 2, well before this Phase 3 stopping point"

  # The reserved internal IP -- not the VM's ephemeral describe
  # IP -- must be what the VM is actually created with, and the same
  # value the settings.yaml write and the PV would use, so GKE agents,
  # the Docker broker, and the reserved address all agree on one number.
  # The stub's `addresses create` defaults to 10.128.0.9, distinct from
  # `instances describe`'s own default of 10.128.0.5, so the two are
  # never accidentally indistinguishable in this assertion.
  local create_line
  create_line="$(echo "$log" | grep 'compute instances create' | head -1)"
  assert_contains "$create_line" "--private-network-ip=10.128.0.9" \
    "the new VM must be created pinned to the reserved internal IP, not left to get an ephemeral one"
  assert_contains "$dev_heredoc" 'server: "10.128.0.9"' \
    "the settings.yaml shared_dir_storage server field must be the reserved internal IP"

  # The internal IP guard's post-create half re-describes the
  # reservation; this call must actually have happened by this point,
  # proving deploy.sh calls the guard at all (nothing else in this flow
  # describes the internal-IP address after it's created).
  assert_contains "$log" "compute addresses describe scion-hub-${HUB}-internal-ip" \
    "the internal IP guard must re-describe the internal IP reservation after create"
}

test_deploy_create_tier_on_wires_configured_image_size_to_export_script() {
  fresh_gcloud_state
  seed_cluster "mycluster" "default"
  # A non-default size: 20 is also what every other fixture happens to
  # produce (the config default), so it can't distinguish "the configured
  # value was used" from "a hard-coded value was used".
  run_deploy_create_to_settings_yaml \
    "$(base_config_json "$HUB" ", \"gke_target\": {\"name\": \"mycluster\", \"location\": \"us-central1\", \"project\": \"demo-project\", \"shared_dir_image_size_gb\": 37}" "registry" "us-docker.pkg.dev/demo-project/scion")"
  assert_eq "true" "$DEPLOY_REACHED_SETTINGS_YAML" "a tier-on create must reach the settings.yaml write"
  assert_contains "$(gcloud_log)" "fallocate -l 37G" \
    "deploy.sh must pass the configured gke_target.shared_dir_image_size_gb through to the export script, not a fixed default"
}

test_deploy_create_cloud_run_deploy_carries_marker_label_on_fresh_create() {
  fresh_gcloud_state
  run_deploy_create_to_cloud_run_deploy "$(base_config_json "$HUB" "" "registry" "us-docker.pkg.dev/demo-project/scion")"
  local run_deploy_line
  run_deploy_line="$(gcloud_log | grep '^beta run deploy ' | head -1)"
  assert_contains "$run_deploy_line" "--labels=scion-deployment=${HUB}" \
    "the Cloud Run IAP proxy must carry the marker label on its first deploy -- PROXY_SERVICE_LABEL_ARGS must actually reach the run deploy call"
}

test_deploy_create_cloud_run_deploy_no_label_when_service_already_exists() {
  fresh_gcloud_state
  seed_run_service_exists "${INSTANCE_NAME}-iap-proxy"
  run_deploy_create_to_cloud_run_deploy "$(base_config_json "$HUB" "" "registry" "us-docker.pkg.dev/demo-project/scion")"
  local run_deploy_line
  run_deploy_line="$(gcloud_log | grep '^beta run deploy ' | head -1)"
  assert_not_contains "$run_deploy_line" "--labels=" \
    "redeploying an existing Cloud Run service must not add the create-only marker label"
}

test_deploy_create_tier_on_existing_vm_promotes_current_ip() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "us-central1-b"
  seed_cluster "mycluster" "default"
  run_deploy_create_wait_for "$(base_config_json "$HUB" "$(hybrid_config_fragment)" "registry" "us-docker.pkg.dev/demo-project/scion")" \
    "addresses-create-completed"
  local log
  log="$(gcloud_log)"
  assert_contains "$log" "addresses create scion-hub-${HUB}-internal-ip" \
    "an existing VM's current IP must be promoted to a static reservation"
  assert_contains "$log" "--addresses=10.128.0.5" \
    "must promote the VM's actual current IP (the stub's instances-describe default), not a fresh/different one"
}

# =====================================================================
# Settings writes parsed as YAML, and the agent transport wiring.
# =====================================================================
# The settings tests above compare substrings or bytes. These parse the
# captured settings.yaml writes with the koanf YAML parser the hub uses
# to load its settings (tests/lib/settings-yaml-to-json.go, run through
# the repository's own Go module), so a splice that produces invalid or
# mis-nested YAML fails here even when every expected substring is
# present.

# _settings_heredoc_nth LOG N — the body of the Nth (1-based)
# settings.yaml heredoc in LOG.
_settings_heredoc_nth() {
  echo "$1" | awk -v want="$2" "/<< 'SETTINGSEOF'/{flag=1; n++; buf=\"\"; next} /^SETTINGSEOF\$/{flag=0; if (n == want) printf \"%s\", buf} flag{buf=buf \$0 ORS}"
}

# _settings_yaml_json YAML — prints YAML parsed by the hub's settings
# parser, as JSON. Non-zero, with the parser's error on stderr, if YAML
# does not parse.
_settings_yaml_json() {
  local go_bin repo_root
  go_bin="$(command -v go || true)"
  if [[ -z "$go_bin" && -x /usr/local/go/bin/go ]]; then
    go_bin=/usr/local/go/bin/go
  fi
  if [[ -z "$go_bin" ]]; then
    echo "settings YAML parse: no Go toolchain found (needed to run tests/lib/settings-yaml-to-json.go)" >&2
    return 2
  fi
  repo_root="$(cd "${TIER_DIR}/../.." && pwd)"
  printf '%s' "$1" | (cd "$repo_root" && "$go_bin" run -buildvcs=false \
    scripts/single-node-vm/tests/lib/settings-yaml-to-json.go)
}

# _json_get JSON PATH — the value at dotted PATH (list indexes as
# numbers, e.g. server.hub.admin_emails.0), or "<missing>".
_json_get() {
  printf '%s' "$1" | "$PYTHON" -c '
import json, sys
node = json.load(sys.stdin)
for part in sys.argv[1].split("."):
    if isinstance(node, list) and part.isdigit() and int(part) < len(node):
        node = node[int(part)]
    elif isinstance(node, dict) and part in node:
        node = node[part]
    else:
        print("<missing>")
        sys.exit(0)
print(node if isinstance(node, str) else json.dumps(node))
' "$2"
}

# _json_leaf_paths JSON — every scalar leaf of JSON as a dotted path,
# one per line, sorted. Map keys join with ".", list elements append
# "[i]" (e.g. server.hub.admin_emails[0]). An empty map or list counts
# as a leaf, so a key whose value is empty is still listed.
_json_leaf_paths() {
  printf '%s' "$1" | "$PYTHON" -c '
import json, sys
out = []
def walk(node, path):
    if isinstance(node, dict) and node:
        for k, v in node.items():
            walk(v, path + "." + k if path else k)
    elif isinstance(node, list) and node:
        for i, v in enumerate(node):
            walk(v, "%s[%d]" % (path, i))
    else:
        out.append(path)
walk(json.load(sys.stdin), "")
print("\n".join(sorted(out)))
'
}

TRANSPORT_TEST_CLIENT_ID="999999999-Verbatim_Client-ID.apps.googleusercontent.com"

test_deploy_create_tier_on_settings_writes_parse_as_yaml() {
  fresh_gcloud_state
  seed_cluster "mycluster" "default"
  set_iap_client_id "$TRANSPORT_TEST_CLIENT_ID"
  run_deploy_create_to_proxy_settings_yaml \
    "$(base_config_json "$HUB" "$(hybrid_config_fragment)" "registry" "us-docker.pkg.dev/demo-project/scion")"
  assert_eq "true" "$DEPLOY_REACHED_SETTINGS_YAML" "a tier-on create must reach the Phase-5 proxy-mode settings.yaml write"
  local log transport_sa n yaml json mode
  log="$(gcloud_log)"
  transport_sa="$(hybrid_transport_sa_name "$HUB")@demo-project.iam.gserviceaccount.com"
  assert_eq "2" "$(echo "$log" | grep -c "<< 'SETTINGSEOF'" || true)" \
    "a create through Phase 5 must write settings.yaml twice (dev mode, then proxy mode)"
  for n in 1 2; do
    if [[ "$n" == 1 ]]; then mode="dev"; else mode="proxy"; fi
    yaml="$(_settings_heredoc_nth "$log" "$n")"
    json="$(_settings_yaml_json "$yaml")"
    assert_eq "0" "$?" "the ${mode}-mode settings.yaml write must parse as YAML"
    assert_eq "$mode" "$(_json_get "$json" server.auth.mode)" "${mode}-mode write: server.auth.mode"
    assert_eq "block" "$(_json_get "$json" default_gcp_identity_mode)" \
      "${mode}-mode write: default_gcp_identity_mode is block with the tier on"
    assert_eq "antigravity" "$(_json_get "$json" default_harness_config)" \
      "${mode}-mode write: default_harness_config"
    assert_eq "iap" "$(_json_get "$json" server.auth.transport.mode)" "${mode}-mode write: server.auth.transport.mode"
    assert_eq "$TRANSPORT_TEST_CLIENT_ID" "$(_json_get "$json" server.auth.transport.oidc_audience)" \
      "${mode}-mode write: server.auth.transport.oidc_audience must be the discovered client ID, verbatim"
    assert_eq "$transport_sa" "$(_json_get "$json" server.auth.transport.platform_auth_sa)" \
      "${mode}-mode write: server.auth.transport.platform_auth_sa must be the transport SA"
    assert_eq "invite_only" "$(_json_get "$json" server.auth.user_access_mode)" \
      "${mode}-mode write: server.auth.user_access_mode defaults to invite_only with the tier on"
    assert_eq "<missing>" "$(_json_get "$json" server.auth.authorized_domains)" \
      "${mode}-mode write: no authorized_domains unless the config sets them"
    assert_eq "admin@example.com" "$(_json_get "$json" server.hub.admin_emails.0)" \
      "${mode}-mode write: the admin stays in admin_emails"
    assert_eq "nfs" "$(_json_get "$json" server.shared_dir_storage.backend)" \
      "${mode}-mode write: server.shared_dir_storage.backend"
    assert_eq "10.128.0.9" "$(_json_get "$json" server.shared_dir_storage.nfs.shares.0.server)" \
      "${mode}-mode write: the shared_dir_storage share server is the reserved internal IP"
    assert_eq "8080" "$(_json_get "$json" server.listen_port)" \
      "${mode}-mode write: server.listen_port stays a server-level key after the splices"
  done
}

test_deploy_create_tier_off_settings_writes_parse_as_yaml() {
  fresh_gcloud_state
  run_deploy_create_to_proxy_settings_yaml \
    "$(base_config_json "$HUB" "" "registry" "us-docker.pkg.dev/demo-project/scion")"
  assert_eq "true" "$DEPLOY_REACHED_SETTINGS_YAML" "a tier-off create must reach the Phase-5 proxy-mode settings.yaml write"
  local log n yaml json mode expected_leaves
  log="$(gcloud_log)"
  # The complete set of keys each tier-off write carries, as leaf paths.
  # Together with the value pins below, this fails a test when a key is
  # added to, dropped from or renamed in the tier-off template.
  local common_leaves="default_gcp_identity_mode
default_harness_config
image_registry
schema_version
server.auth.mode
server.hub.admin_emails[0]
server.hub.name
server.listen_port
server.maintenance.deployment_tier
server.maintenance.release_channel
server.maintenance.update_policy
server.secrets.backend
server.storage.local_path"
  for n in 1 2; do
    if [[ "$n" == 1 ]]; then mode="dev"; else mode="proxy"; fi
    yaml="$(_settings_heredoc_nth "$log" "$n")"
    json="$(_settings_yaml_json "$yaml")"
    assert_eq "0" "$?" "the tier-off ${mode}-mode settings.yaml write must parse as YAML"
    if [[ "$mode" == "proxy" ]]; then
      expected_leaves="$(printf '%s\n%s\n%s' "$common_leaves" \
        "server.auth.proxy.iap.audience" "server.auth.proxy.provider" | LC_ALL=C sort)"
    else
      expected_leaves="$(printf '%s' "$common_leaves" | LC_ALL=C sort)"
    fi
    assert_eq "$expected_leaves" "$(_json_leaf_paths "$json")" \
      "tier-off ${mode}-mode write: exactly the expected set of keys"
    assert_eq "$mode" "$(_json_get "$json" server.auth.mode)" "tier-off ${mode}-mode write: server.auth.mode"
    assert_eq "<missing>" "$(_json_get "$json" server.auth.transport)" \
      "tier-off ${mode}-mode write: no transport block"
    assert_eq "<missing>" "$(_json_get "$json" server.auth.user_access_mode)" \
      "tier-off ${mode}-mode write: no user_access_mode"
    assert_eq "<missing>" "$(_json_get "$json" server.shared_dir_storage)" \
      "tier-off ${mode}-mode write: no shared_dir_storage block"
    assert_eq "8080" "$(_json_get "$json" server.listen_port)" "tier-off ${mode}-mode write: server.listen_port"
    # The fixed keys of the template, pinned by value here rather than
    # read back from deploy.sh, so an edit to the template text outside
    # the hybrid-tier splices fails a test.
    assert_eq "1" "$(_json_get "$json" schema_version)" "tier-off ${mode}-mode write: schema_version"
    assert_eq "us-docker.pkg.dev/demo-project/scion" "$(_json_get "$json" image_registry)" \
      "tier-off ${mode}-mode write: image_registry"
    assert_eq "passthrough" "$(_json_get "$json" default_gcp_identity_mode)" \
      "tier-off ${mode}-mode write: default_gcp_identity_mode"
    assert_eq "antigravity" "$(_json_get "$json" default_harness_config)" \
      "tier-off ${mode}-mode write: default_harness_config"
    assert_eq "$HUB" "$(_json_get "$json" server.hub.name)" "tier-off ${mode}-mode write: server.hub.name"
    assert_eq "admin@example.com" "$(_json_get "$json" server.hub.admin_emails.0)" \
      "tier-off ${mode}-mode write: server.hub.admin_emails"
    assert_eq "binary" "$(_json_get "$json" server.maintenance.deployment_tier)" \
      "tier-off ${mode}-mode write: server.maintenance.deployment_tier"
    assert_eq "nightly" "$(_json_get "$json" server.maintenance.release_channel)" \
      "tier-off ${mode}-mode write: server.maintenance.release_channel"
    assert_eq "auto" "$(_json_get "$json" server.maintenance.update_policy)" \
      "tier-off ${mode}-mode write: server.maintenance.update_policy"
    assert_eq "/home/scion/.scion/workspace-storage" "$(_json_get "$json" server.storage.local_path)" \
      "tier-off ${mode}-mode write: server.storage.local_path"
    assert_eq "local" "$(_json_get "$json" server.secrets.backend)" \
      "tier-off ${mode}-mode write: server.secrets.backend"
    if [[ "$mode" == "proxy" ]]; then
      assert_eq "iap" "$(_json_get "$json" server.auth.proxy.provider)" \
        "tier-off proxy-mode write: server.auth.proxy.provider"
      assert_eq "/projects/123456789012/locations/us-central1/services/scion-hub-${HUB}-iap-proxy" \
        "$(_json_get "$json" server.auth.proxy.iap.audience)" \
        "tier-off proxy-mode write: server.auth.proxy.iap.audience"
    else
      assert_eq "<missing>" "$(_json_get "$json" server.auth.proxy)" \
        "tier-off dev-mode write: no auth.proxy block"
    fi
  done
}

test_deploy_create_tier_on_grants_transport_sa_roles() {
  fresh_gcloud_state
  seed_cluster "mycluster" "default"
  set_iap_client_id "$TRANSPORT_TEST_CLIENT_ID"
  run_deploy_create_to_proxy_settings_yaml \
    "$(base_config_json "$HUB" "$(hybrid_config_fragment)" "registry" "us-docker.pkg.dev/demo-project/scion")"
  assert_eq "true" "$DEPLOY_REACHED_SETTINGS_YAML" "a tier-on create must reach the Phase-5 proxy-mode settings.yaml write"
  local log transport_sa token_line iap_line
  log="$(gcloud_log)"
  transport_sa="$(hybrid_transport_sa_name "$HUB")@demo-project.iam.gserviceaccount.com"
  token_line="$(echo "$log" | grep "^iam service-accounts add-iam-policy-binding ${transport_sa} " || true)"
  assert_eq "1" "$(printf '%s' "$token_line" | grep -c . || true)" \
    "deploy.sh must grant exactly one role binding on the transport SA"
  assert_contains "$token_line" "--role=roles/iam.serviceAccountOpenIdTokenCreator" \
    "the transport SA binding must use the ID-token-only role"
  assert_contains "$token_line" "--member=serviceAccount:scion-hub-${HUB}@demo-project.iam.gserviceaccount.com" \
    "the transport SA binding's member must be the hub VM's runtime SA"
  iap_line="$(echo "$log" | grep '^iap web add-iam-policy-binding ' | grep -F -- "--member=serviceAccount:${transport_sa}" || true)"
  assert_eq "1" "$(printf '%s' "$iap_line" | grep -c . || true)" \
    "Phase 4 must grant the transport SA IAP access exactly once"
  assert_contains "$iap_line" "--role=roles/iap.httpsResourceAccessor" \
    "the transport SA's IAP grant must use the accessor role"
  assert_contains "$iap_line" "--service=scion-hub-${HUB}-iap-proxy" \
    "the transport SA's IAP grant must be on the hub's own Cloud Run service"
  assert_contains "$log" "iap settings get --project=demo-project --resource-type=iap_web" \
    "the client ID must be read from the project-level IAP settings"
}

test_deploy_create_tier_off_no_transport_calls() {
  fresh_gcloud_state
  run_deploy_create_to_proxy_settings_yaml \
    "$(base_config_json "$HUB" "" "registry" "us-docker.pkg.dev/demo-project/scion")"
  assert_eq "true" "$DEPLOY_REACHED_SETTINGS_YAML" "a tier-off create must reach the Phase-5 proxy-mode settings.yaml write"
  local log
  log="$(gcloud_log)"
  assert_eq "0" "$(echo "$log" | grep -c '^iap settings get' || true)" "tier off must not read IAP settings"
  assert_eq "0" "$(echo "$log" | grep -c 'scion-tp-' || true)" "tier off must not create or grant a transport SA"
}

# _run_deploy_create_expect_refusal CONFIG_JSON — runs a create that is
# expected to stop at validation, with a timeout so a missed refusal
# fails fast. Sets DEPLOY_RC and DEPLOY_LOG.
_run_deploy_create_expect_refusal() {
  local config_file
  config_file="$(mktemp)"
  printf '%s' "$1" > "$config_file"
  DEPLOY_LOG="$(timeout 60 bash "$DEPLOY_SH" --config "$config_file" --version v1.0.0-test < /dev/null 2>&1)"
  DEPLOY_RC=$?
  rm -f "$config_file"
}

test_deploy_create_tier_on_service_account_admin_email_refused() {
  fresh_gcloud_state
  seed_cluster "mycluster" "default"
  _run_deploy_create_expect_refusal \
    "$(base_config_json "$HUB" "$(hybrid_config_fragment)" "registry" "us-docker.pkg.dev/demo-project/scion" \
      | sed 's/admin@example.com/robot@demo-project.iam.gserviceaccount.com/')"
  assert_true "$([[ "$DEPLOY_RC" -ne 0 && "$DEPLOY_RC" -ne 124 ]] && echo true || echo false)" \
    "tier on with a service-account admin_email must be refused before any create"
  assert_contains "$DEPLOY_LOG" "is a service account" "the message should say what is wrong with admin_email"
  assert_eq "0" "$(gcloud_log | grep -c ' create ' || true)" "nothing should be created"
}

test_deploy_create_tier_on_open_user_access_mode_refused() {
  fresh_gcloud_state
  seed_cluster "mycluster" "default"
  _run_deploy_create_expect_refusal \
    "$(base_config_json "$HUB" "$(hybrid_config_fragment), \"user_access_mode\": \"open\"" "registry" "us-docker.pkg.dev/demo-project/scion")"
  assert_true "$([[ "$DEPLOY_RC" -ne 0 && "$DEPLOY_RC" -ne 124 ]] && echo true || echo false)" \
    "tier on with user_access_mode open must be refused before any create"
  assert_contains "$DEPLOY_LOG" "user_access_mode 'open' is not supported" "the message should name the mode"
  assert_eq "0" "$(gcloud_log | grep -c ' create ' || true)" "nothing should be created"
}

test_deploy_create_tier_on_domain_restricted_written_to_settings() {
  fresh_gcloud_state
  seed_cluster "mycluster" "default"
  run_deploy_create_to_proxy_settings_yaml \
    "$(base_config_json "$HUB" "$(hybrid_config_fragment), \"user_access_mode\": \"domain_restricted\", \"authorized_domains\": [\"Example.com\"]" "registry" "us-docker.pkg.dev/demo-project/scion")"
  assert_eq "true" "$DEPLOY_REACHED_SETTINGS_YAML" "a tier-on create must reach the Phase-5 proxy-mode settings.yaml write"
  local log json
  log="$(gcloud_log)"
  json="$(_settings_yaml_json "$(_settings_heredoc_nth "$log" 2)")"
  assert_eq "0" "$?" "the proxy-mode settings.yaml write must parse as YAML"
  assert_eq "domain_restricted" "$(_json_get "$json" server.auth.user_access_mode)" \
    "a configured domain_restricted mode reaches settings.yaml"
  assert_eq '["example.com"]' "$(_json_get "$json" server.auth.authorized_domains)" \
    "the configured domains reach settings.yaml, lowercased"
  assert_eq "iap" "$(_json_get "$json" server.auth.transport.mode)" "the transport block is still written alongside"
  assert_contains "$DEPLOY_LOG" "User access:  domain_restricted" "the resolved mode is shown in the pre-deploy summary"
}

test_deploy_create_tier_off_ignores_user_access_config() {
  fresh_gcloud_state
  run_deploy_create_to_proxy_settings_yaml \
    "$(base_config_json "$HUB" ", \"user_access_mode\": \"open\"" "registry" "us-docker.pkg.dev/demo-project/scion")"
  assert_eq "true" "$DEPLOY_REACHED_SETTINGS_YAML" "a tier-off create with user access keys must still run"
  assert_contains "$DEPLOY_LOG" "applied only when the hybrid tier is on; ignoring them" \
    "tier off must say the user access keys are not applied"
  assert_not_contains "$(gcloud_log)" "user_access_mode" "tier off must not write user_access_mode"
}

test_deploy_create_tier_off_wrongly_typed_user_access_config_warns_only() {
  fresh_gcloud_state
  run_deploy_create_to_proxy_settings_yaml \
    "$(base_config_json "$HUB" "" "registry" "us-docker.pkg.dev/demo-project/scion")"
  assert_eq "true" "$DEPLOY_REACHED_SETTINGS_YAML" "a tier-off create with no user access keys must reach the proxy-mode write"
  local plain_log plain_dev plain_proxy typed_log
  plain_log="$(gcloud_log)"
  plain_dev="$(_settings_heredoc_nth "$plain_log" 1)"
  plain_proxy="$(_settings_heredoc_nth "$plain_log" 2)"
  fresh_gcloud_state
  run_deploy_create_to_proxy_settings_yaml \
    "$(base_config_json "$HUB" ", \"user_access_mode\": 5, \"authorized_domains\": \"example.com\"" "registry" "us-docker.pkg.dev/demo-project/scion")"
  assert_eq "true" "$DEPLOY_REACHED_SETTINGS_YAML" \
    "with the tier off, wrongly typed user access keys must not stop the deploy"
  assert_contains "$DEPLOY_LOG" "applied only when the hybrid tier is on; ignoring them" \
    "tier off must still warn that the user access keys are not applied"
  assert_not_contains "$DEPLOY_LOG" "Invalid user access settings" "tier off must not validate the key values"
  typed_log="$(gcloud_log)"
  assert_eq "$plain_dev" "$(_settings_heredoc_nth "$typed_log" 1)" \
    "the tier-off dev-mode settings.yaml must be byte-identical to a run without the keys"
  assert_eq "$plain_proxy" "$(_settings_heredoc_nth "$typed_log" 2)" \
    "the tier-off proxy-mode settings.yaml must be byte-identical to a run without the keys"
}

# =====================================================================
# Teardown of the agent transport service account, as deploy.sh
# reports it.
# =====================================================================

test_deploy_delete_lists_transport_sa_before_confirmation() {
  fresh_gcloud_state
  local email list_at first_delete_at
  email="$(hybrid_transport_sa_name "$HUB")@demo-project.iam.gserviceaccount.com"
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_contains "$DEPLOY_LOG" "  Service account:   ${email} (hybrid tier agent transport; if present and marked)" \
    "the will-be-deleted list must name the transport SA"
  list_at="$(echo "$DEPLOY_LOG" | grep -n -F "Service account:   ${email}" | head -1 | cut -d: -f1)"
  first_delete_at="$(echo "$DEPLOY_LOG" | grep -n "Non-interactive mode: proceeding with teardown\|Deleting " | head -1 | cut -d: -f1)"
  assert_true "$([[ -n "$list_at" && ( -z "$first_delete_at" || "$list_at" -lt "$first_delete_at" ) ]] && echo true || echo false)" \
    "the transport SA must be listed before teardown proceeds"
}

test_deploy_delete_proxy_delete_failure_still_removes_transport_sa_iap_binding() {
  fresh_gcloud_state
  local email binding_line
  email="$(hybrid_transport_sa_name "$HUB")@demo-project.iam.gserviceaccount.com"
  seed_service_account "$email" "$MARKER"
  set_run_service_delete_error "scion-hub-${HUB}-iap-proxy"
  run_deploy_delete "$(base_config_json "$HUB")"
  binding_line="$(gcloud_log | grep '^iap web remove-iam-policy-binding ' || true)"
  assert_contains "$binding_line" "--member=serviceAccount:${email}" \
    "with the Cloud Run delete failed, the transport SA's IAP binding on it must still be removed"
  assert_contains "$binding_line" "--service=scion-hub-${HUB}-iap-proxy" \
    "the binding removal must target the hub's own Cloud Run service"
  assert_contains "$(gcloud_log)" "iam service-accounts delete ${email} " \
    "the marked transport SA is still deleted once its binding is removed"
}


test_deploy_delete_transport_sa_deleted_and_reported() {
  fresh_gcloud_state
  local email
  email="$(hybrid_transport_sa_name "$HUB")@demo-project.iam.gserviceaccount.com"
  seed_service_account "$email" "$MARKER"
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_eq "0" "$DEPLOY_RC" "a clean teardown with a marked transport SA must exit 0"
  assert_contains "$(gcloud_log)" "iam service-accounts delete ${email} " "the marked transport SA must be deleted"
  assert_contains "$DEPLOY_LOG" "Deleted transport SA:       ${email}" "the summary must list the deleted transport SA"
  assert_contains "$DEPLOY_LOG" "No transport SA IAP access left on: scion-hub-${HUB}-iap-proxy" \
    "the summary must say no IAP access is left once the Cloud Run service is gone"
}

test_deploy_delete_transport_sa_absent_reported_not_found() {
  fresh_gcloud_state
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_eq "0" "$DEPLOY_RC" "an absent transport SA must not fail teardown"
  assert_contains "$DEPLOY_LOG" "Not found transport SA:     $(hybrid_transport_sa_name "$HUB")@demo-project.iam.gserviceaccount.com" \
    "the summary must list the transport SA as not found"
}

test_deploy_delete_transport_sa_delete_failure_exits_nonzero() {
  fresh_gcloud_state
  local email
  email="$(hybrid_transport_sa_name "$HUB")@demo-project.iam.gserviceaccount.com"
  seed_service_account "$email" "$MARKER"
  set_service_account_delete_will_fail "$email"
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_true "$([[ "$DEPLOY_RC" -ne 0 ]] && echo true || echo false)" \
    "a failed transport SA delete must make teardown exit non-zero"
  assert_contains "$DEPLOY_LOG" "Kept transport SA:          ${email} (delete failed)" \
    "the summary must list the transport SA as kept, with the reason"
}

test_deploy_delete_transport_sa_describe_error_exits_nonzero() {
  fresh_gcloud_state
  local email
  email="$(hybrid_transport_sa_name "$HUB")@demo-project.iam.gserviceaccount.com"
  set_service_account_describe_error "$email"
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_true "$([[ "$DEPLOY_RC" -ne 0 ]] && echo true || echo false)" \
    "a transport SA whose state cannot be read must make teardown exit non-zero"
  assert_contains "$DEPLOY_LOG" "Kept transport SA:          ${email} (state unknown: describe failed)" \
    "the summary must list the transport SA as kept because its state is unknown"
  assert_eq "0" "$(gcloud_log | grep -c "iam service-accounts delete ${email}" || true)" \
    "nothing must be deleted when the transport SA's state is unknown"
}

# =====================================================================
# Project-level IAM bindings
# =====================================================================
# The stub gcloud answers `projects add-iam-policy-binding` as for a
# project whose policy already contains conditional bindings: a call
# without --condition=None fails with gcloud's own error text.

PROJECT_BINDING_ERROR="ERROR: (gcloud.projects.add-iam-policy-binding) PERMISSION_DENIED: caller lacks resourcemanager.projects.setIamPolicy on project demo-project"

# _assert_project_bindings_carry_condition_none EXPECTED_COUNT LABEL —
# asserts the gcloud log holds exactly EXPECTED_COUNT project-level
# binding calls and that each one passes --condition=None as its own
# argument.
_assert_project_bindings_carry_condition_none() {
  local expected="$1" label="$2" lines total with_flag
  lines="$(gcloud_log | grep '^projects add-iam-policy-binding ' || true)"
  total="$(printf '%s' "$lines" | grep -c . || true)"
  with_flag="$(printf '%s' "$lines" | grep -cE '( |^)--condition=None( |$)' || true)"
  assert_eq "$expected" "$total" "${label}: expected ${expected} project-level binding calls"
  assert_eq "$total" "$with_flag" "${label}: every project-level binding call must pass --condition=None"
}

test_gcloud_stub_project_binding_without_condition_fails() {
  fresh_gcloud_state
  local out rc
  out="$(gcloud projects add-iam-policy-binding demo-project \
    --member=user:someone@example.com --role=roles/viewer --quiet 2>&1 >/dev/null)"
  rc=$?
  assert_true "$([[ "$rc" -ne 0 ]] && echo true || echo false)" \
    "a project-level binding without --condition must fail on a policy with conditions"
  assert_contains "$out" "Adding a binding without specifying a condition to a policy containing conditions is prohibited in non-interactive mode. Run the command again with \`--condition=None\`" \
    "the failure must carry gcloud's error text"
  out="$(gcloud projects add-iam-policy-binding demo-project \
    --member=user:someone@example.com --role=roles/viewer --condition=None --quiet 2>/dev/null)"
  assert_eq "0" "$?" "the same binding with --condition=None must succeed"
}

test_deploy_create_tier_off_project_bindings_pass_condition_none() {
  fresh_gcloud_state
  # The service-level IAP binding fails so the project-level fallback
  # runs too: all three binding sites are exercised.
  set_iap_web_binding_will_fail
  run_deploy_create_to_proxy_settings_yaml \
    "$(base_config_json "$HUB" "" "registry" "us-docker.pkg.dev/demo-project/scion")"
  assert_eq "true" "$DEPLOY_REACHED_SETTINGS_YAML" "a tier-off create must reach the Phase-5 proxy-mode settings.yaml write"
  # 6 service account roles, the deployer's tunnel role, the IAP fallback.
  _assert_project_bindings_carry_condition_none 8 "tier off"
  assert_eq "1" "$(gcloud_log | grep '^projects add-iam-policy-binding ' | grep -c -- '--role=roles/iap.httpsResourceAccessor' || true)" \
    "the project-level IAP fallback must run once"
  assert_not_contains "$DEPLOY_LOG" "Updated IAM policy for project" \
    "gcloud's stderr must not be printed when a binding succeeds"
  assert_contains "$DEPLOY_LOG" "IAP access granted to: stub-deployer@example.com (project-level fallback)" \
    "the fallback binding must succeed"
}

test_deploy_create_tier_on_project_bindings_pass_condition_none() {
  fresh_gcloud_state
  seed_cluster "mycluster" "default"
  run_deploy_create_to_proxy_settings_yaml \
    "$(base_config_json "$HUB" "$(hybrid_config_fragment)" "registry" "us-docker.pkg.dev/demo-project/scion")"
  assert_eq "true" "$DEPLOY_REACHED_SETTINGS_YAML" "a tier-on create must reach the Phase-5 proxy-mode settings.yaml write"
  # 6 service account roles and the deployer's tunnel role; the
  # service-level IAP binding succeeds, so there is no fallback.
  _assert_project_bindings_carry_condition_none 7 "tier on"
  assert_not_contains "$DEPLOY_LOG" "Updated IAM policy for project" \
    "gcloud's stderr must not be printed when a binding succeeds"
}

test_deploy_create_role_binding_failure_stops_with_gcloud_error() {
  fresh_gcloud_state
  set_project_binding_will_fail "roles/cloudtrace.agent" "$PROJECT_BINDING_ERROR"
  _run_deploy_create_expect_refusal \
    "$(base_config_json "$HUB" "" "registry" "us-docker.pkg.dev/demo-project/scion")"
  assert_true "$([[ "$DEPLOY_RC" -ne 0 && "$DEPLOY_RC" -ne 124 ]] && echo true || echo false)" \
    "a failed service account role binding must stop the deploy with a non-zero exit"
  assert_contains "$DEPLOY_LOG" "Failed to grant roles/cloudtrace.agent to scion-hub-${HUB}@demo-project.iam.gserviceaccount.com" \
    "the error must name the role and the service account"
  assert_contains "$DEPLOY_LOG" "  ${PROJECT_BINDING_ERROR}" \
    "the error must include gcloud's error text"
  assert_not_contains "$DEPLOY_LOG" "Roles bound:" "the deploy must not report the roles as bound"
  assert_eq "0" "$(gcloud_log | grep -c -- '--role=roles/artifactregistry.writer' || true)" \
    "no later role binding may run after a failure"
  assert_eq "0" "$(gcloud_log | grep -c '^compute instances create' || true)" \
    "no VM may be created after a failed role binding"
}

# =====================================================================
# Hub-minted service accounts: the hub VM's SA gets
# roles/iam.serviceAccountAdmin by default (hub_sa_minting), and
# iamcredentials.googleapis.com is a required API.
# =====================================================================

HUB_SA_ADMIN_BINDING_RE="^projects add-iam-policy-binding demo-project .*--member=serviceAccount:scion-hub-${HUB}@demo-project\.iam\.gserviceaccount\.com .*--role=roles/iam\.serviceAccountAdmin( |$)"

test_deploy_create_grants_hub_sa_service_account_admin_by_default() {
  fresh_gcloud_state
  run_deploy_create "$(base_config_json "$HUB")"
  assert_eq "1" "$(gcloud_log | grep -cE "$HUB_SA_ADMIN_BINDING_RE" || true)" \
    "a default deploy must grant roles/iam.serviceAccountAdmin to the hub VM's SA exactly once"
  assert_eq "1" "$(gcloud_log | grep -E "$HUB_SA_ADMIN_BINDING_RE" | grep -cE '( |^)--condition=None( |$)' || true)" \
    "the serviceAccountAdmin grant must pass --condition=None like the other role grants"
  assert_contains "$DEPLOY_LOG" "aiplatform.user, iam.serviceAccountAdmin" \
    "the roles-bound summary must list iam.serviceAccountAdmin"
}

test_deploy_create_hub_sa_minting_true_grants_service_account_admin() {
  fresh_gcloud_state
  run_deploy_create "$(base_config_json "$HUB" ', "hub_sa_minting": true')"
  assert_eq "1" "$(gcloud_log | grep -cE "$HUB_SA_ADMIN_BINDING_RE" || true)" \
    "hub_sa_minting: true must grant roles/iam.serviceAccountAdmin to the hub VM's SA"
}

test_deploy_create_hub_sa_minting_false_skips_service_account_admin() {
  fresh_gcloud_state
  run_deploy_create "$(base_config_json "$HUB" ', "hub_sa_minting": false')"
  assert_eq "0" "$(gcloud_log | grep -c -- '--role=roles/iam.serviceAccountAdmin' || true)" \
    "hub_sa_minting: false must skip the serviceAccountAdmin grant"
  assert_eq "1" "$(gcloud_log | grep '^projects add-iam-policy-binding ' | grep -c -- '--role=roles/aiplatform.user' || true)" \
    "hub_sa_minting: false must still grant the other hub SA roles"
  assert_contains "$DEPLOY_LOG" "Skipped iam.serviceAccountAdmin (hub_sa_minting is false)" \
    "the deploy must say the grant was skipped"
  assert_contains "$DEPLOY_LOG" "gcloud projects remove-iam-policy-binding demo-project --member=serviceAccount:scion-hub-${HUB}@demo-project.iam.gserviceaccount.com --role=roles/iam.serviceAccountAdmin --condition=None" \
    "the deploy must print how to remove a grant left by an earlier deploy"
}

test_deploy_create_hub_sa_minting_invalid_value_refused() {
  fresh_gcloud_state
  _run_deploy_create_expect_refusal "$(base_config_json "$HUB" ', "hub_sa_minting": "yes"')"
  assert_true "$([[ "$DEPLOY_RC" -ne 0 && "$DEPLOY_RC" -ne 124 ]] && echo true || echo false)" \
    "an invalid hub_sa_minting value must stop the deploy"
  assert_contains "$DEPLOY_LOG" "Invalid hub_sa_minting in config: 'yes' (expected: true or false)" \
    "the error must name the key and the accepted values"
  assert_eq "0" "$(gcloud_log | grep -c '^projects add-iam-policy-binding ' || true)" \
    "no role may be granted after an invalid hub_sa_minting value"
}

test_deploy_create_service_account_admin_binding_failure_stops_deploy() {
  fresh_gcloud_state
  set_project_binding_will_fail "roles/iam.serviceAccountAdmin" "$PROJECT_BINDING_ERROR"
  _run_deploy_create_expect_refusal \
    "$(base_config_json "$HUB" "" "registry" "us-docker.pkg.dev/demo-project/scion")"
  assert_true "$([[ "$DEPLOY_RC" -ne 0 && "$DEPLOY_RC" -ne 124 ]] && echo true || echo false)" \
    "a failed serviceAccountAdmin grant must stop the deploy like the other role grants"
  assert_contains "$DEPLOY_LOG" "Failed to grant roles/iam.serviceAccountAdmin to scion-hub-${HUB}@demo-project.iam.gserviceaccount.com" \
    "the error must name the role and the service account"
  assert_eq "0" "$(gcloud_log | grep -c '^compute instances create' || true)" \
    "no VM may be created after a failed serviceAccountAdmin grant"
}

test_deploy_create_api_check_enables_iamcredentials_when_missing() {
  fresh_gcloud_state
  seed_enabled_apis compute.googleapis.com run.googleapis.com iap.googleapis.com \
    cloudbuild.googleapis.com artifactregistry.googleapis.com aiplatform.googleapis.com \
    iam.googleapis.com
  run_deploy_create "$(base_config_json "$HUB")"
  local enable_line
  enable_line="$(gcloud_log | grep 'services enable' | head -1)"
  assert_eq "1" "$(gcloud_log | grep -c 'services enable' || true)" "exactly one enable call, for iamcredentials only"
  assert_contains "$enable_line" "iamcredentials.googleapis.com" \
    "the hub mints tokens for hub-minted service accounts through iamcredentials.googleapis.com"
  assert_not_contains "$enable_line" "compute.googleapis.com" "an already-enabled API must not be re-enabled"
}

# deploy.sh never removes the hub SA's project role bindings one by one:
# --delete deletes the hub SA itself, so every binding it holds
# (serviceAccountAdmin included) no longer applies. Asserts that path, against the
# exact state a default create left behind.
test_deploy_delete_after_create_deletes_hub_sa_holding_service_account_admin() {
  fresh_gcloud_state
  run_deploy_create "$(base_config_json "$HUB")"
  assert_eq "1" "$(gcloud_log | grep -cE "$HUB_SA_ADMIN_BINDING_RE" || true)" \
    "precondition: the create must have granted serviceAccountAdmin to the hub SA"
  run_deploy_delete "$(base_config_json "$HUB")"
  assert_eq "0" "$DEPLOY_RC" "teardown after a default create must exit 0"
  assert_eq "1" "$(gcloud_log | grep -c -- "^iam service-accounts delete scion-hub-${HUB}@demo-project.iam.gserviceaccount.com " || true)" \
    "teardown must delete the hub SA, so its serviceAccountAdmin binding no longer applies"
  assert_contains "$DEPLOY_LOG" "Deleted: scion-hub-${HUB}@demo-project.iam.gserviceaccount.com" \
    "teardown must report the hub SA as deleted"
}

test_deploy_create_tunnel_binding_failure_warns_with_gcloud_error() {
  fresh_gcloud_state
  set_project_binding_will_fail "roles/iap.tunnelResourceAccessor" "$PROJECT_BINDING_ERROR"
  run_deploy_create "$(base_config_json "$HUB" "" "registry" "us-docker.pkg.dev/demo-project/scion")"
  assert_eq "0" "$?" "a failed tunnel grant must not stop the deploy before Phase 3"
  assert_contains "$DEPLOY_LOG" "Failed to grant roles/iap.tunnelResourceAccessor to stub-deployer@example.com" \
    "the warning must name the role and the deployer"
  assert_contains "$DEPLOY_LOG" "  ${PROJECT_BINDING_ERROR}" \
    "the warning must include gcloud's error text"
  assert_contains "$DEPLOY_LOG" "Roles bound:" "the service account role bindings must succeed"
}

test_deploy_create_iap_fallback_binding_failure_reports_gcloud_error() {
  fresh_gcloud_state
  set_iap_web_binding_will_fail
  set_project_binding_will_fail "roles/iap.httpsResourceAccessor" "$PROJECT_BINDING_ERROR"
  run_deploy_create_to_proxy_settings_yaml \
    "$(base_config_json "$HUB" "" "registry" "us-docker.pkg.dev/demo-project/scion")"
  assert_eq "true" "$DEPLOY_REACHED_SETTINGS_YAML" \
    "a failed IAP fallback must not stop the deploy before the proxy-mode settings.yaml write"
  assert_contains "$DEPLOY_LOG" "Failed to grant IAP access to stub-deployer@example.com at both service and project levels." \
    "the error must say both IAP bindings failed"
  assert_contains "$DEPLOY_LOG" "  ${PROJECT_BINDING_ERROR}" \
    "the error must include gcloud's error text"
}

# =====================================================================
# Resource-level IAM bindings
# =====================================================================
# The stub gcloud answers `iap web add-iam-policy-binding` and `iam
# service-accounts add-iam-policy-binding` as for a policy that already
# contains conditional bindings: a call without --condition=None fails.

# _assert_resource_bindings_carry_condition_none IAP_COUNT SA_COUNT LABEL —
# asserts the gcloud log holds exactly IAP_COUNT `iap web
# add-iam-policy-binding` calls and SA_COUNT `iam service-accounts
# add-iam-policy-binding` calls, each passing --condition=None as its own
# argument.
_assert_resource_bindings_carry_condition_none() {
  local iap_expected="$1" sa_expected="$2" label="$3" lines total with_flag
  lines="$(gcloud_log | grep '^iap web add-iam-policy-binding ' || true)"
  total="$(printf '%s' "$lines" | grep -c . || true)"
  with_flag="$(printf '%s' "$lines" | grep -cE '( |^)--condition=None( |$)' || true)"
  assert_eq "$iap_expected" "$total" "${label}: expected ${iap_expected} IAP web binding calls"
  assert_eq "$total" "$with_flag" "${label}: every IAP web binding call must pass --condition=None"
  lines="$(gcloud_log | grep '^iam service-accounts add-iam-policy-binding ' || true)"
  total="$(printf '%s' "$lines" | grep -c . || true)"
  with_flag="$(printf '%s' "$lines" | grep -cE '( |^)--condition=None( |$)' || true)"
  assert_eq "$sa_expected" "$total" "${label}: expected ${sa_expected} service account binding calls"
  assert_eq "$total" "$with_flag" "${label}: every service account binding call must pass --condition=None"
}

test_deploy_create_tier_off_resource_bindings_pass_condition_none() {
  fresh_gcloud_state
  run_deploy_create_to_proxy_settings_yaml \
    "$(base_config_json "$HUB" "" "registry" "us-docker.pkg.dev/demo-project/scion")"
  assert_eq "true" "$DEPLOY_REACHED_SETTINGS_YAML" "a tier-off create must reach the Phase-5 proxy-mode settings.yaml write"
  # The operator's service-level IAP binding only.
  _assert_resource_bindings_carry_condition_none 1 0 "tier off"
  assert_contains "$DEPLOY_LOG" "IAP access granted to: stub-deployer@example.com (service-level binding)" \
    "the operator's service-level IAP binding must succeed"
  assert_not_contains "$DEPLOY_LOG" "Service-level IAP binding failed" \
    "a successful service-level binding must not take the fallback"
  assert_not_contains "$DEPLOY_LOG" "Updated IAM policy for" \
    "gcloud's stderr must not be printed when a binding succeeds"
}

test_deploy_create_tier_on_resource_bindings_pass_condition_none() {
  fresh_gcloud_state
  seed_cluster "mycluster" "default"
  run_deploy_create_to_proxy_settings_yaml \
    "$(base_config_json "$HUB" "$(hybrid_config_fragment)" "registry" "us-docker.pkg.dev/demo-project/scion")"
  assert_eq "true" "$DEPLOY_REACHED_SETTINGS_YAML" "a tier-on create must reach the Phase-5 proxy-mode settings.yaml write"
  # The operator's and the transport SA's IAP bindings, and the token
  # creator binding on the transport SA.
  _assert_resource_bindings_carry_condition_none 2 1 "tier on"
  assert_not_contains "$DEPLOY_LOG" "Updated IAM policy for" \
    "gcloud's stderr must not be printed when a binding succeeds"
}

test_deploy_create_service_iap_binding_failure_warns_with_gcloud_error_and_falls_back() {
  fresh_gcloud_state
  local msg="ERROR: (gcloud.iap.web.add-iam-policy-binding) PERMISSION_DENIED: caller lacks iap.webServices.setIamPolicy"
  set_iap_web_binding_will_fail "user:stub-deployer@example.com" "$msg"
  run_deploy_create_to_proxy_settings_yaml \
    "$(base_config_json "$HUB" "" "registry" "us-docker.pkg.dev/demo-project/scion")"
  assert_eq "true" "$DEPLOY_REACHED_SETTINGS_YAML" \
    "a failed service-level IAP binding must not stop the deploy before the proxy-mode settings.yaml write"
  assert_contains "$DEPLOY_LOG" "Service-level IAP binding failed; falling back to project-level binding." \
    "the warning must be printed"
  assert_contains "$DEPLOY_LOG" "  ${msg}" "the warning must include gcloud's error text"
  local warn_at err_at fallback_at
  warn_at="$(line_number "Service-level IAP binding failed" "$DEPLOY_LOG")"
  err_at="$(line_number "  ${msg}" "$DEPLOY_LOG")"
  fallback_at="$(line_number "IAP access granted to: stub-deployer@example.com (project-level fallback)" "$DEPLOY_LOG")"
  assert_true "$([[ -n "$warn_at" && -n "$err_at" && -n "$fallback_at" && "$warn_at" -lt "$err_at" && "$err_at" -lt "$fallback_at" ]] && echo true || echo false)" \
    "gcloud's error text must follow the warning and precede the fallback's result"
  assert_eq "1" "$(gcloud_log | grep '^projects add-iam-policy-binding ' | grep -c -- '--role=roles/iap.httpsResourceAccessor' || true)" \
    "the project-level fallback must run once"
}
