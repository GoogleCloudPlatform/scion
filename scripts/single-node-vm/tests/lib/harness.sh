# scripts/single-node-vm/tests/lib/harness.sh — shared helpers for the
# deploy.sh gcloud-stub test harness. Sourced by run.sh, never executed
# directly.
#
# Provides: stub info/warn/err/config_get/config_prompt (stand-ins for
# any future function-level test that sources a deploy.sh helper
# directly, the way this file's own caller expects them to already be
# defined), a fresh-per-test gcloud stub state dir on PATH, and small
# assertion helpers. No test here ever contacts GCP; the only `gcloud` on
# PATH is tests/lib/gcloud.
#
# This file has no shebang -- it is always `source`d, never executed --
# so shellcheck needs an explicit shell directive to know its dialect.
# shellcheck shell=bash

HARNESS_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PYTHON="${PYTHON:-python3}"

# A handful of tests may deliberately replace (not prepend to) $PATH for
# the duration of a single run_expect_fail call, to simulate a missing
# tool. run_expect_fail's own TMPDIR bookkeeping (find/sort/comm, below)
# must keep working even then, so it uses this snapshot -- taken once,
# here, before any test can touch $PATH -- rather than the ambient $PATH.
HARNESS_SAFE_PATH="$PATH"

PASS_COUNT=0
FAIL_COUNT=0

# --- Stub versions of deploy.sh's own helpers, for any future
# function-level test that sources a deploy.sh helper directly instead
# of driving deploy.sh as a subprocess. ---
info() { :; }
warn() { echo "WARN: $*" >&2; }
err()  { echo "ERR: $*" >&2; }

# --- Config stub -------------------------------------------------------
# A function-level test populates the CONFIG associative array (declare
# -A CONFIG=(...)) before calling a helper function; config_get reads
# from it instead of parsing any real JSON file.
declare -A CONFIG
config_get() {
  local key="$1" default="${2:-}"
  if [[ -n "${CONFIG[$key]+set}" ]]; then
    printf '%s' "${CONFIG[$key]}"
  else
    printf '%s' "$default"
  fi
}
config_prompt() {
  # Not expected to be called by a subprocess-driven test (deploy.sh's
  # own --config path never prompts); a function-level test's CONFIG
  # fixture is missing a key it should have set if this fires.
  err "config_prompt called unexpectedly with: $2"
  exit 1
}
# shellcheck disable=SC2034 # read by a future function-level test's config reads
CONFIG_FILE="/dev/null/harness-tests-placeholder"

# --- gcloud stub setup ---------------------------------------------------
# fresh_gcloud_state resets CONFIG, the invocation log, and the stub's
# on-disk fixture state dir for the next test.
fresh_gcloud_state() {
  # Removing the previous call's entries before making new ones keeps the
  # suite's own footprint bounded to "the current test's state" at any
  # point in time, not "every test's state so far". This still leaves the
  # very last test's entries in place when the run ends, which run.sh's
  # own per-run TMPDIR + EXIT trap is what actually cleans up.
  rm -rf "${GCLOUD_STUB_STATE_DIR:-}"
  rm -f "${GCLOUD_STUB_LOG:-}"
  CONFIG=()
  GCLOUD_STUB_STATE_DIR="$(mktemp -d)"
  GCLOUD_STUB_LOG="$(mktemp)"
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/firewall-rules" "${GCLOUD_STUB_STATE_DIR}/instances" \
    "${GCLOUD_STUB_STATE_DIR}/run-services" "${GCLOUD_STUB_STATE_DIR}/nats"
  export GCLOUD_STUB_STATE_DIR GCLOUD_STUB_LOG
}

# seed_instance NAME ZONE — simulates a pre-existing GCE VM.
seed_instance() {
  printf '%s' "$2" > "${GCLOUD_STUB_STATE_DIR}/instances/$1"
}

# set_instance_delete_will_fail NAME — the next `instances delete` call
# for this instance fails instead of succeeding (the instance stays
# "present" in the stub's state, matching a real failed delete).
set_instance_delete_will_fail() {
  touch "${GCLOUD_STUB_STATE_DIR}/instances/$1.delete-fail"
}

# set_instance_delete_error_text NAME TEXT — used with
# set_instance_delete_will_fail above: replaces the stub's generic
# failure message with TEXT, so a test can distinguish a confirmed
# not-found error from an ambiguous one (permission, API outage, etc.).
set_instance_delete_error_text() {
  printf '%s' "$2" > "${GCLOUD_STUB_STATE_DIR}/instances/$1.delete-fail-text"
}

# set_firewall_list_will_fail — the next `firewall-rules list` call fails
# instead of returning a rule list.
set_firewall_list_will_fail() {
  touch "${GCLOUD_STUB_STATE_DIR}/firewall-rules-list-should-fail"
}

# set_instances_list_will_fail — every `instances list` call fails
# (simulating a transient API error), so a VM-gone check can't tell
# whether the VM is still there.
set_instances_list_will_fail() {
  touch "${GCLOUD_STUB_STATE_DIR}/instances-list-should-fail"
}

# set_instances_list_zone_unreachable — every `instances list` call
# simulates a real AggregatedList with one UNREACHABLE zone: exit 0 with
# the VM missing from stdout and a warning on stderr, unless the caller
# set CLOUDSDK_COMPUTE_ALLOW_PARTIAL_ERROR=false, in which case it's a
# hard error (exit 1) instead -- matching real gcloud's own behavior.
set_instances_list_zone_unreachable() {
  touch "${GCLOUD_STUB_STATE_DIR}/instances-list-zone-unreachable"
}

# set_router_exists / set_service_account_exists — simulate a
# pre-existing base router or service account, so its create-only marker
# can be asserted absent on a re-run/adopt path.
set_router_exists() {
  touch "${GCLOUD_STUB_STATE_DIR}/router-exists"
}
set_service_account_exists() {
  touch "${GCLOUD_STUB_STATE_DIR}/service-account-exists"
}

# set_nat_exists NAME — simulates a pre-existing Cloud NAT with this name
# (name-scoped, unlike set_router_exists above, since a project can have
# more than one NAT), so its create-only marker can be asserted absent on
# a re-run/adopt path. `compute routers nats create` for this name sets
# the same marker, so a re-run against a create's own leftover state sees
# it without a test needing to hand-seed it too.
set_nat_exists() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/nats"
  touch "${GCLOUD_STUB_STATE_DIR}/nats/$1.exists"
}

# seed_service_account EMAIL DESCRIPTION — a name-scoped service-account
# fixture: the next `describe` for exactly this email returns this
# description.
seed_service_account() {
  local email="$1" description="${2:-}"
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/service-accounts"
  "$PYTHON" -c "
import json, sys
email, desc, path = sys.argv[1:4]
json.dump({'email': email, 'description': desc}, open(path, 'w'))
" "$email" "$description" "${GCLOUD_STUB_STATE_DIR}/service-accounts/${email}.json"
}

# set_service_account_delete_will_fail EMAIL — the next `delete` for
# exactly this service account fails.
set_service_account_delete_will_fail() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/service-accounts"
  touch "${GCLOUD_STUB_STATE_DIR}/service-accounts/$1.json.delete-fail"
}

# set_service_account_delete_not_found EMAIL — `describe` still finds
# this service account, but the next `delete` reports NOT_FOUND (it was
# deleted in between).
set_service_account_delete_not_found() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/service-accounts"
  touch "${GCLOUD_STUB_STATE_DIR}/service-accounts/$1.json.delete-not-found"
}

# set_service_account_grant_will_fail EMAIL — the next
# `add-iam-policy-binding` on exactly this service account fails.
set_service_account_grant_will_fail() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/service-accounts"
  touch "${GCLOUD_STUB_STATE_DIR}/service-accounts/$1.json.grant-fail"
}

# set_service_account_describe_error EMAIL [MESSAGE] — `describe` for
# exactly this service account fails with MESSAGE, or by default a
# realistic PERMISSION_DENIED error that says nothing about existence.
set_service_account_describe_error() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/service-accounts"
  printf '%s' "${2:-}" > "${GCLOUD_STUB_STATE_DIR}/service-accounts/$1.json.describe-error"
}

# set_iap_web_binding_will_fail — the next `iap web
# add-iam-policy-binding` call fails.
set_iap_web_binding_will_fail() {
  touch "${GCLOUD_STUB_STATE_DIR}/iap-web-add-binding-should-fail"
}

# seed_run_service_exists NAME — the next `run services describe` call
# for this service succeeds (simulating a redeploy of an existing
# service). Without this, the stub reports NOT_FOUND.
seed_run_service_exists() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/run-services"
  touch "${GCLOUD_STUB_STATE_DIR}/run-services/$1.exists"
}

# set_run_service_describe_error NAME — the next `run services describe`
# call for this service fails (a permissions problem, for example).
set_run_service_describe_error() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/run-services"
  touch "${GCLOUD_STUB_STATE_DIR}/run-services/$1.describe-error"
}

# set_run_service_delete_error NAME — `run services delete` for this
# service fails with an error that is not a not-found.
set_run_service_delete_error() {
  mkdir -p "${GCLOUD_STUB_STATE_DIR}/run-services"
  touch "${GCLOUD_STUB_STATE_DIR}/run-services/$1.delete-error"
}

# set_firewall_delete_will_fail NAME — the next `firewall-rules delete`
# call for this rule fails instead of succeeding (the rule's JSON stays
# present in stub state, matching a real failed delete rather than one
# that succeeded or found nothing).
set_firewall_delete_will_fail() {
  touch "${GCLOUD_STUB_STATE_DIR}/firewall-rules/$1.json.delete-fail"
}

# seed_firewall_rule_desc_only NAME DESCRIPTION — simulates a pre-existing
# rule that carries an arbitrary (possibly non-marker) description, with no
# other fields set.
seed_firewall_rule_desc_only() {
  "$PYTHON" "${HARNESS_LIB_DIR}/firewall-rule-json.py" \
    "$2" "default" "INGRESS" "ALLOW" "tcp" "22" "" "" "" "900" \
    > "${GCLOUD_STUB_STATE_DIR}/firewall-rules/$1.json"
}

# seed_firewall_rule_json NAME DESC NETWORK DIRECTION ACTION PROTO PORTS \
#   SOURCE_TAGS SOURCE_RANGES TARGET_TAGS PRIORITY \
#   [SOURCE_SAS [TARGET_SAS [DEST_RANGES [DISABLED [EXTRA_PROTO [EXTRA_PORTS]]]]]]
#
# Simulates a pre-existing rule with a fully specified spec. The trailing
# fields are optional and default to empty/false; see firewall-rule-json.py
# for what each one builds.
seed_firewall_rule_json() {
  local name="$1"
  shift
  "$PYTHON" "${HARNESS_LIB_DIR}/firewall-rule-json.py" "$@" \
    > "${GCLOUD_STUB_STATE_DIR}/firewall-rules/${name}.json"
}

# seed_routers_list JSON — writes JSON (a full JSON array, in the shape
# `gcloud compute routers list --format=json` returns) as the fixture the
# stub's `compute routers list` case serves. Without this, the stub
# defaults to an empty list ("[]").
seed_routers_list() {
  printf '%s' "$1" > "${GCLOUD_STUB_STATE_DIR}/routers-list.json"
}

# set_routers_list_will_fail — the next (and every subsequent) `compute
# routers list` call fails instead of returning a list.
set_routers_list_will_fail() {
  touch "${GCLOUD_STUB_STATE_DIR}/routers-list-should-fail"
}

gcloud_log() {
  [[ -f "$GCLOUD_STUB_LOG" ]] && cat "$GCLOUD_STUB_LOG"
}

gcloud_call_count() {
  gcloud_log | grep -c . || true
}

# --- Assertions ----------------------------------------------------------
# Each prints PASS/FAIL with the current test name and bumps the counters.
# CURRENT_TEST is set by run.sh before invoking each test_* function.

assert_eq() {
  local expected="$1" actual="$2" msg="${3:-}"
  if [[ "$expected" == "$actual" ]]; then
    PASS_COUNT=$((PASS_COUNT + 1))
  else
    FAIL_COUNT=$((FAIL_COUNT + 1))
    echo "FAIL [${CURRENT_TEST}]: ${msg} (expected '${expected}', got '${actual}')"
  fi
}

assert_contains() {
  local haystack="$1" needle="$2" msg="${3:-}"
  if [[ "$haystack" == *"$needle"* ]]; then
    PASS_COUNT=$((PASS_COUNT + 1))
  else
    FAIL_COUNT=$((FAIL_COUNT + 1))
    echo "FAIL [${CURRENT_TEST}]: ${msg} (expected to find '${needle}')"
    echo "  --- haystack ---"
    local line
    while IFS= read -r line; do
      echo "  ${line}"
    done <<< "$haystack"
  fi
}

assert_not_contains() {
  local haystack="$1" needle="$2" msg="${3:-}"
  if [[ "$haystack" != *"$needle"* ]]; then
    PASS_COUNT=$((PASS_COUNT + 1))
  else
    FAIL_COUNT=$((FAIL_COUNT + 1))
    echo "FAIL [${CURRENT_TEST}]: ${msg} (did not expect to find '${needle}')"
  fi
}

assert_true() {
  local cond="$1" msg="${2:-}"
  if [[ "$cond" == "true" ]]; then
    PASS_COUNT=$((PASS_COUNT + 1))
  else
    FAIL_COUNT=$((FAIL_COUNT + 1))
    echo "FAIL [${CURRENT_TEST}]: ${msg} (expected true, got '${cond}')"
  fi
}

# assert_false COND MSG — COND must be "" or "false" (the two shapes
# `$([[ x ]] && echo true)` and `$([[ x ]] && echo true || echo false)`
# both produce when x is false).
assert_false() {
  local cond="$1" msg="${2:-}"
  if [[ -z "$cond" || "$cond" == "false" ]]; then
    PASS_COUNT=$((PASS_COUNT + 1))
  else
    FAIL_COUNT=$((FAIL_COUNT + 1))
    echo "FAIL [${CURRENT_TEST}]: ${msg} (expected false/empty, got '${cond}')"
  fi
}

# run_expect_fail CMD... — runs a command that is expected to call `exit`
# with a non-zero status. The command substitution below already runs it
# in a subshell, so its `exit` can never end the calling test (each test
# additionally runs in its own subshell at the run.sh level, so this is
# defense in depth, not the only thing preventing that). Restores
# errexit to whatever it was before the call, rather than unconditionally
# turning it on, so this never changes the caller's shell options as a
# side effect. Sets RUN_EXIT_CODE and RUN_OUTPUT, both read back by the
# caller.
run_expect_fail() {
  local out had_errexit=false before_tmp after_tmp
  case "$-" in *e*) had_errexit=true ;; esac
  set +e
  before_tmp="$(PATH="$HARNESS_SAFE_PATH" find "${TMPDIR:-/tmp}" -mindepth 1 2>/dev/null | PATH="$HARNESS_SAFE_PATH" sort)"
  out="$("$@" 2>&1)"
  # shellcheck disable=SC2034 # read by callers
  RUN_EXIT_CODE=$?
  [[ "$had_errexit" == "true" ]] && set -e
  # shellcheck disable=SC2034 # read by callers
  RUN_OUTPUT="$out"
  # "$@" above runs inside a command-substitution subshell, so a mktemp
  # file/dir it creates is invisible here once the subshell exits: there
  # is no variable left to rm -f, only the file on disk. Diffing TMPDIR
  # before/after and removing whatever appeared catches this generically,
  # for any function this wraps, not just the ones a test author
  # remembered to audit.
  after_tmp="$(PATH="$HARNESS_SAFE_PATH" find "${TMPDIR:-/tmp}" -mindepth 1 2>/dev/null | PATH="$HARNESS_SAFE_PATH" sort)"
  if [[ "$after_tmp" != "$before_tmp" ]]; then
    PATH="$HARNESS_SAFE_PATH" comm -13 <(printf '%s\n' "$before_tmp") <(printf '%s\n' "$after_tmp") | while IFS= read -r _new_tmp_entry; do
      [[ -n "$_new_tmp_entry" ]] && rm -rf "$_new_tmp_entry"
    done
  fi
}
