# scripts/single-node-vm/tests/test_delete_sa_bindings.sh — `deploy.sh
# --delete` removes the project IAM role bindings deploy.sh granted the
# hub VM's service account, before deleting the SA, plus any
# deleted:serviceAccount:...?uid= tombstones an earlier SA delete left
# behind (ptone/scion#6161). Nothing else in the policy is touched.
#
# This file has no shebang -- it is always `source`d, never executed --
# so shellcheck needs an explicit shell directive to know its dialect.
# shellcheck shell=bash

DEPLOY_SH="${TIER_DIR}/deploy.sh"
HUB="bindhub"
SA_EMAIL="scion-hub-${HUB}@demo-project.iam.gserviceaccount.com"
SA_MEMBER="serviceAccount:${SA_EMAIL}"
TOMBSTONE_MEMBER="deleted:serviceAccount:${SA_EMAIL}?uid=123456789012345678901"
# Every role deploy.sh may grant the hub SA (Phase 2), minting role included.
HUB_ROLES=(roles/logging.logWriter roles/monitoring.metricWriter roles/cloudtrace.agent \
  roles/artifactregistry.writer roles/aiplatform.user roles/iam.serviceAccountAdmin)

# sa_bindings_config_json HUB [HUB_SA_MINTING] — a minimal, valid
# deploy.sh config, same fields as deploy-config.example.json.
sa_bindings_config_json() {
  local hub="$1" minting="${2:-true}"
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
  "release_channel": "nightly",
  "hub_sa_minting": ${minting}
}
EOF
}

# run_sa_bindings_create CONFIG_JSON — runs deploy.sh (create mode) in
# the background, in its own process group, until the stub's
# `phase2-complete` sentinel shows Phase 2 (SA and role grants included)
# has run, then stops it. Sets DEPLOY_RC and DEPLOY_LOG.
run_sa_bindings_create() {
  local config_file log_file sentinel pid waited_ms=0 monitor_was_on=false
  config_file="$(mktemp)"
  log_file="$(mktemp)"
  printf '%s' "$1" > "$config_file"
  sentinel="${GCLOUD_STUB_STATE_DIR}/phase2-complete"
  rm -f "$sentinel"
  # Job control only for this one launch (its own process group, so the
  # whole group can be stopped below); restore the caller's setting.
  [[ "$-" == *m* ]] && monitor_was_on=true
  set -m
  bash "$DEPLOY_SH" --config "$config_file" --version v1.0.0-test \
    < /dev/null > "$log_file" 2>&1 &
  pid=$!
  if [[ "$monitor_was_on" == "false" ]]; then set +m; fi
  while [[ ! -f "$sentinel" && "$waited_ms" -lt 30000 ]]; do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1
    waited_ms=$((waited_ms + 100))
  done
  kill -TERM -- "-${pid}" 2>/dev/null || true
  wait "$pid" 2>/dev/null
  DEPLOY_RC=$?
  waited_ms=0
  while kill -0 -- "-${pid}" 2>/dev/null && [[ "$waited_ms" -lt 5000 ]]; do
    sleep 0.05
    waited_ms=$((waited_ms + 50))
  done
  DEPLOY_LOG="$(cat "$log_file")"
  if [[ ! -f "$sentinel" ]]; then
    DEPLOY_RC=1
    DEPLOY_LOG="${DEPLOY_LOG}
FATAL: run_sa_bindings_create: sentinel '${sentinel}' was never reached"
  fi
  rm -f "$config_file" "$log_file"
}

# run_sa_bindings_delete CONFIG_JSON — runs `deploy.sh --delete` to
# completion. Sets DEPLOY_RC and DEPLOY_LOG.
run_sa_bindings_delete() {
  local config_file
  config_file="$(mktemp)"
  printf '%s' "$1" > "$config_file"
  DEPLOY_LOG="$(bash "$DEPLOY_SH" --delete --config "$config_file" < /dev/null 2>&1)"
  DEPLOY_RC=$?
  rm -f "$config_file"
}

# remove_calls — every `projects remove-iam-policy-binding` call logged.
remove_calls() {
  gcloud_log | grep '^projects remove-iam-policy-binding ' || true
}

# count_lines TEXT — the number of non-empty lines in TEXT.
count_lines() {
  if [[ -z "$1" ]]; then
    echo 0
  else
    printf '%s\n' "$1" | grep -c .
  fi
}

# bindings_of MEMBER_PREFIX — policy lines whose member starts with it.
bindings_of() {
  project_iam_bindings | awk -F'\t' -v p="$1" 'index($2, p) == 1' || true
}

test_delete_sa_bindings_after_create_removes_each_role_before_sa_delete() {
  fresh_gcloud_state
  run_sa_bindings_create "$(sa_bindings_config_json "$HUB")"
  assert_eq "6" "$(count_lines "$(bindings_of "$SA_MEMBER")")" \
    "precondition: the create must have bound six roles to the hub SA"
  run_sa_bindings_delete "$(sa_bindings_config_json "$HUB")"
  assert_eq "0" "$DEPLOY_RC" "teardown after a create must exit 0"

  local log removes sa_delete_line role line
  log="$(gcloud_log)"
  removes="$(remove_calls)"
  assert_eq "6" "$(count_lines "$removes")" \
    "teardown must make one remove-iam-policy-binding call per granted role"
  sa_delete_line="$(line_number "iam service-accounts delete ${SA_EMAIL} " "$log")"
  assert_true "$([[ -n "$sa_delete_line" ]] && echo true)" "teardown must delete the hub SA"
  for role in "${HUB_ROLES[@]}"; do
    assert_eq "1" "$(printf '%s\n' "$removes" | grep -c -- "--member=${SA_MEMBER} --role=${role} --condition=None" || true)" \
      "teardown must remove ${role} from the hub SA exactly once, with --condition=None"
    line="$(line_number "projects remove-iam-policy-binding demo-project --member=${SA_MEMBER} --role=${role} " "$log")"
    assert_true "$([[ -n "$line" && -n "$sa_delete_line" && "$line" -lt "$sa_delete_line" ]] && echo true)" \
      "${role} must be removed before the SA delete (remove at line '${line}', SA delete at '${sa_delete_line}')"
    assert_contains "$DEPLOY_LOG" "Removed: ${role} from ${SA_MEMBER}" "teardown must log the removal of ${role}"
  done
  assert_eq "" "$(bindings_of "$SA_MEMBER")" "no live binding for the hub SA may remain"
  assert_eq "" "$(bindings_of "deleted:${SA_MEMBER}")" "no tombstone for the hub SA may remain"
  assert_eq "1" "$(bindings_of "user:stub-deployer@example.com" | grep -c 'roles/iap.tunnelResourceAccessor' || true)" \
    "the deployer's IAP tunnel binding must be kept"
  assert_eq "0" "$(printf '%s\n' "$log" | grep -c 'set-iam-policy' || true)" "teardown must never call set-iam-policy"
  assert_contains "$DEPLOY_LOG" "Removed project bindings:   6 for ${SA_EMAIL}" \
    "the summary must report the removed bindings"
}

test_delete_sa_bindings_touch_no_other_member_or_role() {
  fresh_gcloud_state
  seed_service_account "$SA_EMAIL" "Scion Hub"
  local role
  for role in "${HUB_ROLES[@]}"; do
    seed_project_iam_binding "$role" "$SA_MEMBER"
  done
  # Same roles, other members; same member, a role deploy.sh never grants.
  seed_project_iam_binding roles/logging.logWriter "serviceAccount:other@demo-project.iam.gserviceaccount.com"
  seed_project_iam_binding roles/aiplatform.user "user:someone@example.com"
  seed_project_iam_binding roles/aiplatform.user "deleted:serviceAccount:other@demo-project.iam.gserviceaccount.com?uid=42"
  seed_project_iam_binding roles/aiplatform.user "serviceAccount:${SA_EMAIL}.evil@demo-project.iam.gserviceaccount.com"
  seed_project_iam_binding roles/viewer "$SA_MEMBER"
  run_sa_bindings_delete "$(sa_bindings_config_json "$HUB")"
  assert_eq "0" "$DEPLOY_RC" "teardown must exit 0"
  assert_eq "6" "$(count_lines "$(remove_calls)")" "only the six hub SA bindings may be removed"
  assert_not_contains "$(remove_calls)" "roles/viewer" "a role deploy.sh never grants must not be removed"
  assert_not_contains "$(remove_calls)" "other@demo-project" "another SA's binding must not be removed"
  assert_not_contains "$(remove_calls)" "someone@example.com" "a user's binding must not be removed"
  assert_not_contains "$(remove_calls)" ".evil@" "a member that merely starts with the SA email must not be removed"
  local expected
  expected="$(printf '%s\n' \
    "roles/logging.logWriter	serviceAccount:other@demo-project.iam.gserviceaccount.com" \
    "roles/aiplatform.user	user:someone@example.com" \
    "roles/aiplatform.user	deleted:serviceAccount:other@demo-project.iam.gserviceaccount.com?uid=42" \
    "roles/aiplatform.user	serviceAccount:${SA_EMAIL}.evil@demo-project.iam.gserviceaccount.com" \
    "roles/viewer	deleted:serviceAccount:${SA_EMAIL}?uid=100000000000000000001")"
  assert_eq "$expected" "$(project_iam_bindings)" \
    "every other binding must be left as it was (the unowned role becomes a tombstone once the SA is deleted)"
}

test_delete_sa_bindings_idempotent_when_already_gone() {
  fresh_gcloud_state
  run_sa_bindings_create "$(sa_bindings_config_json "$HUB")"
  run_sa_bindings_delete "$(sa_bindings_config_json "$HUB")"
  assert_eq "0" "$DEPLOY_RC" "first teardown must exit 0"
  : > "$GCLOUD_STUB_LOG"
  run_sa_bindings_delete "$(sa_bindings_config_json "$HUB")"
  assert_eq "0" "$DEPLOY_RC" "a second teardown, with bindings and SA already gone, must exit 0"
  assert_eq "0" "$(count_lines "$(remove_calls)")" "nothing is bound, so nothing may be removed"
  assert_contains "$DEPLOY_LOG" "No project role bindings left for ${SA_EMAIL}." \
    "teardown must say no bindings were left"
  assert_not_contains "$DEPLOY_LOG" "Could not remove" "an absent binding is not an error"
}

test_delete_sa_bindings_removes_tombstones_when_sa_already_gone() {
  fresh_gcloud_state
  local role
  for role in roles/logging.logWriter roles/monitoring.metricWriter roles/cloudtrace.agent \
      roles/artifactregistry.writer roles/aiplatform.user; do
    seed_project_iam_binding "$role" "$TOMBSTONE_MEMBER"
  done
  seed_project_iam_binding roles/viewer "$TOMBSTONE_MEMBER"
  seed_project_iam_binding roles/logging.logWriter "deleted:serviceAccount:other@demo-project.iam.gserviceaccount.com?uid=7"
  run_sa_bindings_delete "$(sa_bindings_config_json "$HUB")"
  assert_eq "0" "$DEPLOY_RC" "teardown with the SA already gone must exit 0"
  assert_contains "$DEPLOY_LOG" "Service account ${SA_EMAIL} not found or already deleted." \
    "precondition: the SA was already gone"
  local removes
  removes="$(remove_calls)"
  assert_eq "5" "$(count_lines "$removes")" "one removal per tombstoned role deploy.sh grants"
  for role in roles/logging.logWriter roles/monitoring.metricWriter roles/cloudtrace.agent \
      roles/artifactregistry.writer roles/aiplatform.user; do
    assert_eq "1" "$(printf '%s\n' "$removes" | grep -cF -- "--member=${TOMBSTONE_MEMBER} --role=${role} --condition=None" || true)" \
      "the ${role} tombstone must be removed exactly once, with --condition=None"
  done
  local expected
  expected="$(printf '%s\n' \
    "roles/viewer	${TOMBSTONE_MEMBER}" \
    "roles/logging.logWriter	deleted:serviceAccount:other@demo-project.iam.gserviceaccount.com?uid=7")"
  assert_eq "$expected" "$(project_iam_bindings)" \
    "a tombstone on a role deploy.sh never grants, and another SA's tombstone, must be kept"
}

test_delete_sa_bindings_minting_role_removed_even_when_minting_now_off() {
  fresh_gcloud_state
  run_sa_bindings_create "$(sa_bindings_config_json "$HUB" true)"
  run_sa_bindings_delete "$(sa_bindings_config_json "$HUB" false)"
  assert_eq "0" "$DEPLOY_RC" "teardown must exit 0"
  assert_eq "1" "$(remove_calls | grep -c -- "--member=${SA_MEMBER} --role=roles/iam.serviceAccountAdmin " || true)" \
    "a serviceAccountAdmin binding from an earlier deploy must be removed even with hub_sa_minting now false"
  assert_eq "" "$(bindings_of "deleted:${SA_MEMBER}")" "no tombstone for the hub SA may remain"
}

test_delete_sa_bindings_policy_read_failure_falls_back_to_per_role_removal() {
  fresh_gcloud_state
  seed_service_account "$SA_EMAIL" "Scion Hub"
  seed_project_iam_binding roles/logging.logWriter "$SA_MEMBER"
  seed_project_iam_binding roles/aiplatform.user "$SA_MEMBER"
  set_project_get_iam_policy_will_fail
  run_sa_bindings_delete "$(sa_bindings_config_json "$HUB")"
  assert_eq "0" "$DEPLOY_RC" "an unreadable policy must not fail the teardown"
  assert_contains "$DEPLOY_LOG" "Could not read the IAM policy of project demo-project" \
    "teardown must warn that the policy could not be read"
  assert_eq "6" "$(count_lines "$(remove_calls)")" "the fallback must try each granted role once"
  assert_contains "$DEPLOY_LOG" "Removed: roles/logging.logWriter from ${SA_MEMBER}" "a bound role must be removed"
  assert_contains "$DEPLOY_LOG" "Not bound: roles/cloudtrace.agent for ${SA_MEMBER}" \
    "an absent binding must be reported, not treated as an error"
  assert_not_contains "$DEPLOY_LOG" "Could not remove" "an absent binding is not an error"
  assert_eq "" "$(project_iam_bindings)" "both seeded bindings must be gone"
}

test_delete_sa_bindings_removal_failure_warns_and_continues() {
  fresh_gcloud_state
  seed_service_account "$SA_EMAIL" "Scion Hub"
  local role
  for role in "${HUB_ROLES[@]}"; do
    seed_project_iam_binding "$role" "$SA_MEMBER"
  done
  set_project_binding_remove_will_fail roles/aiplatform.user
  run_sa_bindings_delete "$(sa_bindings_config_json "$HUB")"
  assert_eq "0" "$DEPLOY_RC" "a failed binding removal warns like other base-resource deletes"
  assert_contains "$DEPLOY_LOG" "Could not remove roles/aiplatform.user from ${SA_MEMBER}:" \
    "teardown must name the binding it could not remove"
  assert_contains "$DEPLOY_LOG" "PERMISSION_DENIED" "teardown must show gcloud's error"
  assert_contains "$DEPLOY_LOG" "Deleted: ${SA_EMAIL}" "the SA delete must still run"
  assert_contains "$DEPLOY_LOG" "Kept project bindings:      1 for ${SA_EMAIL}" \
    "the summary must report the binding that was kept"
  assert_contains "$DEPLOY_LOG" "Removed project bindings:   5 for ${SA_EMAIL}" \
    "the summary must report the bindings that were removed"
}

# Sibling hubs whose SA ids share this hub's prefix ("bindhub-p1pm-vm",
# "bindhubx"): their live and tombstoned members must never match the
# hub SA's. A prefix match on the SA id or email would remove them.
test_delete_sa_bindings_leave_sibling_hub_sas_alone() {
  fresh_gcloud_state
  seed_service_account "$SA_EMAIL" "Scion Hub"
  seed_project_iam_binding roles/aiplatform.user "$SA_MEMBER"
  local sibling sibling_members=()
  for sibling in "scion-hub-${HUB}-p1pm-vm@demo-project.iam.gserviceaccount.com" \
      "scion-hub-${HUB}x@demo-project.iam.gserviceaccount.com"; do
    sibling_members+=("serviceAccount:${sibling}" "deleted:serviceAccount:${sibling}?uid=55")
  done
  local m
  for m in "${sibling_members[@]}"; do
    seed_project_iam_binding roles/aiplatform.user "$m"
  done
  run_sa_bindings_delete "$(sa_bindings_config_json "$HUB")"
  assert_eq "0" "$DEPLOY_RC" "teardown must exit 0"
  local removes
  removes="$(remove_calls)"
  assert_eq "1" "$(count_lines "$removes")" "only the hub SA's own binding may be removed"
  for m in "${sibling_members[@]}"; do
    assert_not_contains "$removes" "--member=${m} " "sibling member ${m} must get no remove call"
    assert_eq "1" "$(project_iam_bindings | grep -cxF "roles/aiplatform.user	${m}" || true)" \
      "sibling member ${m} must survive"
  done
}

# deploy.sh never creates a conditional binding, and --condition=None
# cannot remove one, so a conditional binding of the hub SA is skipped;
# and a role/member listed twice still gets only one call.
test_delete_sa_bindings_skip_conditional_and_dedupe() {
  fresh_gcloud_state
  seed_service_account "$SA_EMAIL" "Scion Hub"
  seed_project_iam_binding roles/aiplatform.user "$SA_MEMBER" "temporary access"
  seed_project_iam_binding roles/cloudtrace.agent "$TOMBSTONE_MEMBER" "temporary access"
  seed_project_iam_binding roles/logging.logWriter "$SA_MEMBER"
  seed_project_iam_binding roles/logging.logWriter "$SA_MEMBER"
  run_sa_bindings_delete "$(sa_bindings_config_json "$HUB")"
  assert_eq "0" "$DEPLOY_RC" "teardown must exit 0"
  local removes
  removes="$(remove_calls)"
  assert_eq "1" "$(count_lines "$removes")" "one call for the duplicated unconditional binding, none for conditional ones"
  assert_contains "$removes" "--member=${SA_MEMBER} --role=roles/logging.logWriter --condition=None" \
    "the unconditional binding must be removed"
  assert_not_contains "$removes" "roles/aiplatform.user" "a conditional binding must get no remove call"
  assert_not_contains "$removes" "roles/cloudtrace.agent" "a conditional tombstone must get no remove call"
  assert_not_contains "$DEPLOY_LOG" "Could not remove" "nothing may be reported as failed"
}

# _project_binding_absent, called directly: true only for gcloud's
# IamPolicyBindingNotFound texts (singular and plural, with and without
# the condition, "principal" and older SDKs' "member"), false for any
# other error, including a permission error that also says "not found".
test_delete_sa_bindings_project_binding_absent_table() {
  eval "$(sed -n '/^_project_binding_absent() {/,/^}/p' "$DEPLOY_SH")"
  local prefix="ERROR: (gcloud.projects.remove-iam-policy-binding) " text
  local -a absent=(
    "Policy binding with the specified principal, role, and condition not found!"
    "Policy binding with the specified principal and role not found!"
    "Policy bindings with the specified principal and role not found!"
    "Policy binding with the specified member, role, and condition not found!"
    "Policy binding with the specified member and role not found!"
    "Policy bindings with the specified member and role not found!"
  )
  local -a not_absent=(
    "PERMISSION_DENIED: Policy update access denied."
    "PERMISSION_DENIED: permission resourcemanager.projects.setIamPolicy not found on project"
    "Policy binding with the specified principal and role not found! (permission denied)"
    "NOT_FOUND: Requested entity was not found."
    "INVALID_ARGUMENT: Invalid service account email."
    "ABORTED: There were concurrent policy changes. Please retry the whole read-modify-write with exponential backoff."
    ""
  )
  for text in "${absent[@]}"; do
    assert_true "$(_project_binding_absent "${prefix}${text}" && echo true)" \
      "must count as an absent binding: ${text}"
  done
  for text in "${not_absent[@]}"; do
    assert_false "$(_project_binding_absent "${prefix}${text}" && echo true)" \
      "must not count as an absent binding: '${text}'"
  done
}
