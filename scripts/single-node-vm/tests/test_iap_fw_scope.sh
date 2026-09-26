# scripts/single-node-vm/tests/test_iap_fw_scope.sh — regression coverage
# for the IAP SSH firewall rule scoping fix (ptone/scion#1807): deploy.sh
# must tag the hub VM and create/update the IAP SSH firewall rule with
# --target-tags, so it no longer allows IAP-range SSH to every VM on
# network default, and must never narrow an existing unscoped rule until
# the hub VM's tag is actually confirmed this run.
#
# Runs upstream deploy.sh itself as a real subprocess against the stub
# `gcloud` on PATH, the same way test_deploy_base.sh does. Per-file
# isolation (see README.md "How it works") means this file cannot reuse
# test_deploy_base.sh's run_deploy_create/_start_deploy_bg/_stop_deploy_bg
# — they are defined here too, not shared, on purpose.
#
# This file has no shebang -- it is always `source`d, never executed --
# so shellcheck needs an explicit shell directive to know its dialect.
# shellcheck shell=bash

DEPLOY_SH="${TIER_DIR}/deploy.sh"
HUB="iapfw"
INSTANCE_NAME="scion-hub-${HUB}"
FW_RULE_NAME="scion-hub-${HUB}-allow-iap-ssh"
HUB_TAG="scion-hub-${HUB}"
# The stub's "compute zones list" case always answers "us-central1-b"
# regardless of --filter, so this is the zone deploy.sh will land on for
# any region in this file's config.
ZONE="us-central1-b"

# iap_fw_config_json HUB — a minimal, valid deploy.sh config: fields match
# deploy-config.example.json, with image source "build" (the default,
# needs no registry path) so a create-mode run never needs one.
iap_fw_config_json() {
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
# host. Sets DEPLOY_RC and DEPLOY_LOG. Appends to (rather than resetting)
# $GCLOUD_STUB_LOG across repeated calls, so a test can call this twice to
# exercise a re-run against the state the first call left behind.
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
    # shellcheck disable=SC2034 # set for interface parity with the canonical
    # copy of this helper in test_deploy_base.sh; this file's own tests only
    # read DEPLOY_LOG, not DEPLOY_RC, on the timeout path.
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

# =====================================================================
# Scenario 1: fresh deploy — no VM, no firewall rule yet.
# =====================================================================
# Expect: the rule is created already scoped with --target-tags, the VM is
# created already tagged, and there's nothing pre-existing to fix up (no
# add-tags, no update).

test_iap_fw_scope_fresh_create_tags_vm_and_scopes_rule() {
  fresh_gcloud_state
  run_deploy_create "$(iap_fw_config_json "$HUB")"
  local log
  log="$(gcloud_log)"

  assert_eq "0" "$(echo "$log" | grep -c '^compute instances add-tags' || true)" \
    "a fresh create has no pre-existing VM to add-tags to"
  assert_eq "0" "$(echo "$log" | grep -c '^compute firewall-rules update' || true)" \
    "a fresh create has no pre-existing rule to narrow"

  local fw_create_line vm_create_line
  fw_create_line="$(echo "$log" | grep "^compute firewall-rules create ${FW_RULE_NAME} " | head -1)"
  assert_contains "$fw_create_line" "--target-tags=${HUB_TAG}" \
    "the firewall rule must be created already scoped to the hub tag"
  vm_create_line="$(echo "$log" | grep "^compute instances create ${INSTANCE_NAME} " | head -1)"
  assert_contains "$vm_create_line" "--tags=${HUB_TAG}" \
    "the VM must be created with the hub tag"
}

# =====================================================================
# Scenario 2: re-run against a pre-fix deploy — VM and firewall rule
# already exist, but the rule has no target tags (the bug) and the VM has
# no tag.
# =====================================================================
# Expect: the VM gets tagged, THEN the rule is narrowed in place. Never
# the other order -- a re-run must not leave a window where the rule
# targets a tag the VM lacks. Hand-seeded (not left behind by a real
# create) since the fixed deploy.sh never produces an unscoped rule
# itself -- this state can only come from a deploy that predates the fix.

test_iap_fw_scope_rerun_against_untagged_deploy_tags_then_narrows() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "$ZONE"
  seed_firewall_rule_desc_only "$FW_RULE_NAME" "Allow SSH via IAP tunneling for Scion Hub"
  run_deploy_create "$(iap_fw_config_json "$HUB")"
  local log
  log="$(gcloud_log)"

  local add_tags_line update_line
  add_tags_line="$(echo "$log" | grep "^compute instances add-tags ${INSTANCE_NAME} " | head -1)"
  assert_contains "$add_tags_line" "--tags=${HUB_TAG}" \
    "the pre-existing VM must get the hub tag"
  update_line="$(echo "$log" | grep "^compute firewall-rules update ${FW_RULE_NAME} " | head -1)"
  assert_contains "$update_line" "--target-tags=${HUB_TAG}" \
    "the pre-existing unscoped rule must be narrowed to the hub tag"
  assert_eq "0" "$(echo "$log" | grep -c '^compute firewall-rules create' || true)" \
    "an existing rule must not also be created"
  assert_eq "0" "$(echo "$log" | grep -c '^compute instances create' || true)" \
    "an existing VM must not also be created"

  local add_tags_ln update_ln
  add_tags_ln="$(line_number "compute instances add-tags ${INSTANCE_NAME} " "$log")"
  update_ln="$(line_number "compute firewall-rules update ${FW_RULE_NAME} " "$log")"
  assert_true "$([[ -n "$add_tags_ln" && -n "$update_ln" && "$add_tags_ln" -lt "$update_ln" ]] && echo true || echo false)" \
    "the VM must be tagged before the rule is narrowed"
}

# =====================================================================
# Scenario 3: idempotent re-run — a fresh create followed by a second run
# against the state it left behind.
# =====================================================================
# Expect: add-tags runs both times (idempotent, harmless), but the rule
# -- already scoped by the first run's own create -- is never updated
# again, and neither resource is created a second time. Reuses the state
# a real first run left behind rather than hand-seeding an
# already-scoped rule, so this also proves the create path's own
# --target-tags is what a later describe reads back.

test_iap_fw_scope_idempotent_rerun_leaves_scoped_rule_alone() {
  fresh_gcloud_state
  run_deploy_create "$(iap_fw_config_json "$HUB")"
  run_deploy_create "$(iap_fw_config_json "$HUB")"
  local log
  log="$(gcloud_log)"

  assert_eq "1" "$(echo "$log" | grep -c '^compute instances add-tags' || true)" \
    "the tag comes from --tags at create time on the first run; add-tags only fires once the VM already exists, on the second"
  assert_eq "1" "$(echo "$log" | grep -c '^compute firewall-rules create' || true)" \
    "only the first run creates the rule"
  assert_eq "0" "$(echo "$log" | grep -c '^compute firewall-rules update' || true)" \
    "a rule already scoped to the hub tag must not be updated again"
  assert_eq "1" "$(echo "$log" | grep -c '^compute instances create' || true)" \
    "only the first run creates the VM"
  assert_contains "$DEPLOY_LOG" "Firewall rule already exists: ${FW_RULE_NAME}" \
    "the second run should report the rule as already existing, not narrow it again"
}

# =====================================================================
# Scenario 4: `instances describe` reports no VM, while an unscoped
# firewall rule already exists.
# =====================================================================
# A zone mismatch or a transient describe error looks identical to "no
# VM" to deploy.sh; simulated here simply by not seeding the instance
# fixture, same as a genuinely fresh deploy. Expect: must NOT narrow --
# doing so could lock out a VM that actually exists but wasn't found, and
# still lacks the tag. Must also not add-tags (nothing was found to tag)
# or create a second rule (one already exists).

test_iap_fw_scope_describe_not_found_does_not_narrow_unscoped_rule() {
  fresh_gcloud_state
  seed_firewall_rule_desc_only "$FW_RULE_NAME" "Allow SSH via IAP tunneling for Scion Hub"
  run_deploy_create "$(iap_fw_config_json "$HUB")"
  local log
  log="$(gcloud_log)"

  assert_eq "0" "$(echo "$log" | grep -c '^compute instances add-tags' || true)" \
    "no VM was found to add-tags to"
  assert_eq "0" "$(echo "$log" | grep -c '^compute firewall-rules update' || true)" \
    "must not narrow the rule while the VM's tag could not be confirmed"
  assert_eq "0" "$(echo "$log" | grep -c '^compute firewall-rules create' || true)" \
    "an existing rule must not also be created"
  assert_contains "$DEPLOY_LOG" "could not be confirmed this run" \
    "deploy.sh should explain why it left the rule unscoped"
}

# =====================================================================
# Scenario 5: the rule already exists with target tags, but they don't
# include the hub tag (e.g. a hand-edited rule).
# =====================================================================
# Expect: warn instead of silently treating it as already fine, and never
# auto-modify a rule that already carries someone else's tags.

test_iap_fw_scope_foreign_target_tags_are_left_alone_with_a_warning() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "$ZONE"
  seed_firewall_rule_json "$FW_RULE_NAME" \
    "Allow SSH via IAP tunneling for Scion Hub" "default" "INGRESS" "ALLOW" "tcp" "22" \
    "" "35.235.240.0/20" "some-other-tag" "1000"
  run_deploy_create "$(iap_fw_config_json "$HUB")"
  local log
  log="$(gcloud_log)"

  assert_eq "1" "$(echo "$log" | grep -c "^compute instances add-tags ${INSTANCE_NAME} " || true)" \
    "the VM still gets the hub tag even though the rule has foreign tags"
  assert_eq "0" "$(echo "$log" | grep -c '^compute firewall-rules update' || true)" \
    "a rule with foreign target tags must not be auto-modified"
  assert_eq "0" "$(echo "$log" | grep -c '^compute firewall-rules create' || true)" \
    "an existing rule must not also be created"
  assert_contains "$DEPLOY_LOG" "do not include ${HUB_TAG}" \
    "deploy.sh should warn that the existing rule's tags don't cover the hub VM"
}

# =====================================================================
# Scenario 6: `instances add-tags` itself fails (e.g. a permissions gap),
# for a VM that `describe` did find.
# =====================================================================
# Expect: add-tags is still attempted, but its failure must leave
# HUB_TAG_CONFIRMED false -- the rule must not be narrowed, and deploy.sh
# must say why. A later run where add-tags can succeed must then tag the
# VM and narrow the rule, proving this converges rather than getting
# stuck unscoped forever.

test_iap_fw_scope_add_tags_failure_does_not_narrow_then_converges_next_run() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "$ZONE"
  seed_firewall_rule_desc_only "$FW_RULE_NAME" "Allow SSH via IAP tunneling for Scion Hub"
  set_instance_add_tags_will_fail "$INSTANCE_NAME"
  run_deploy_create "$(iap_fw_config_json "$HUB")"
  local log
  log="$(gcloud_log)"

  assert_eq "1" "$(echo "$log" | grep -c "^compute instances add-tags ${INSTANCE_NAME} " || true)" \
    "add-tags must still be attempted even though it's set up to fail"
  assert_eq "0" "$(echo "$log" | grep -c '^compute firewall-rules update' || true)" \
    "must not narrow the rule when add-tags failed -- the tag isn't confirmed"
  assert_contains "$DEPLOY_LOG" "Could not add network tag ${HUB_TAG} to VM ${INSTANCE_NAME}" \
    "deploy.sh should warn that it could not tag the VM"
  assert_contains "$DEPLOY_LOG" "could not be confirmed this run" \
    "deploy.sh should explain why it's leaving the rule unscoped"

  # Convergence: once add-tags can succeed, a later run tags the VM and
  # narrows the rule. Removes the fixture's failure flag directly (there
  # is no unset_* helper for it, matching how other tests in this suite
  # reach into $GCLOUD_STUB_STATE_DIR when a fixture has no dedicated
  # helper for reverting itself, e.g. test_deploy_base.sh's direct
  # `[[ -f "${GCLOUD_STUB_STATE_DIR}/router-exists" ]]` checks).
  rm -f "${GCLOUD_STUB_STATE_DIR}/instances/${INSTANCE_NAME}.add-tags-fail"
  run_deploy_create "$(iap_fw_config_json "$HUB")"
  log="$(gcloud_log)"

  assert_eq "2" "$(echo "$log" | grep -c "^compute instances add-tags ${INSTANCE_NAME} " || true)" \
    "add-tags is attempted again on the next run"
  assert_eq "1" "$(echo "$log" | grep -c "^compute firewall-rules update ${FW_RULE_NAME} " || true)" \
    "a later run where add-tags succeeds must narrow the rule"
  # DEPLOY_LOG was reassigned by the second run_deploy_create call above,
  # so this is specifically the converging run's own output.
  assert_contains "$DEPLOY_LOG" "Updated firewall rule ${FW_RULE_NAME} with --target-tags=${HUB_TAG}" \
    "the converging run should report the rule as narrowed"
}

# =====================================================================
# Scenario 7: the rule's only target tag is a hyphenated extension of the
# hub tag (e.g. a hybrid-tier-style "${HUB_TAG}-nfs"), not the hub tag
# itself.
# =====================================================================
# `grep -w` treats "-" as a non-word character, so a naive word-boundary
# membership check would treat "${HUB_TAG}-nfs" as containing "${HUB_TAG}"
# and skip the N3 warning. Membership must be an exact match against one
# item of the tag list, not a substring/word match against the raw string.

test_iap_fw_scope_hyphenated_lookalike_tag_still_warns() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "$ZONE"
  seed_firewall_rule_json "$FW_RULE_NAME" \
    "Allow SSH via IAP tunneling for Scion Hub" "default" "INGRESS" "ALLOW" "tcp" "22" \
    "" "35.235.240.0/20" "${HUB_TAG}-nfs" "1000"
  run_deploy_create "$(iap_fw_config_json "$HUB")"
  local log
  log="$(gcloud_log)"

  assert_eq "0" "$(echo "$log" | grep -c '^compute firewall-rules update' || true)" \
    "a rule whose only tag is a look-alike extension of the hub tag must not be auto-modified"
  assert_contains "$DEPLOY_LOG" "do not include ${HUB_TAG}" \
    "a hyphenated look-alike tag is not the hub tag and must still trigger the N3 warning"
}

# =====================================================================
# Scenario 8: the rule exists (the plain `describe` succeeds) and already
# carries foreign target tags, but the second describe -- the one reading
# `--format=value(targetTags)` -- fails.
# =====================================================================
# A `|| true` around that second read would fold a transient failure into
# an empty EXISTING_TARGET_TAGS, indistinguishable from a genuinely
# unscoped rule. With the VM's tag confirmed this run, that would narrow
# the rule and silently wipe out the foreign tags it actually has. The
# read failing must instead be treated as "unknown": warn, and leave the
# rule alone.

test_iap_fw_scope_target_tags_describe_failure_does_not_narrow() {
  fresh_gcloud_state
  seed_instance "$INSTANCE_NAME" "$ZONE"
  seed_firewall_rule_json "$FW_RULE_NAME" \
    "Allow SSH via IAP tunneling for Scion Hub" "default" "INGRESS" "ALLOW" "tcp" "22" \
    "" "35.235.240.0/20" "some-other-tag" "1000"
  set_firewall_describe_target_tags_will_fail "$FW_RULE_NAME"
  run_deploy_create "$(iap_fw_config_json "$HUB")"
  local log
  log="$(gcloud_log)"

  assert_eq "1" "$(echo "$log" | grep -c "^compute instances add-tags ${INSTANCE_NAME} " || true)" \
    "the VM still gets tagged even though the rule's tags couldn't be read"
  assert_eq "0" "$(echo "$log" | grep -c '^compute firewall-rules update' || true)" \
    "must not narrow the rule while its target tags are unknown -- doing so would silently replace whatever foreign tags it actually has"
  assert_eq "0" "$(echo "$log" | grep -c '^compute firewall-rules create' || true)" \
    "an existing rule must not also be created"
  assert_contains "$DEPLOY_LOG" "the read failed" \
    "deploy.sh should explain that the rule's target tags could not be read"
}
