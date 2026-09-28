# scripts/single-node-vm/tests/test_release_checksum.sh — covers the
# SHA256SUMS checksum preflight that runs at the end of deploy.sh's
# Phase 1 (before any GCP resource is created), and the
# download+verify-then-extract logic in the scion-binary and chat-plugin
# install commands in Phase 3. See tests/lib/curl for the stub these tests
# configure (CURL_STUB_SHA256SUMS_MISSING, CURL_STUB_SHA256SUMS_CONN_ERROR,
# CURL_STUB_FIXTURE_DIR).
#
# This file has no shebang -- it is always `source`d, never executed --
# so shellcheck needs an explicit shell directive to know its dialect.
# shellcheck shell=bash

DEPLOY_SH="${TIER_DIR}/deploy.sh"
HUB="checksumhub"

# release_checksum_config_json HUB [CHAT_PLUGINS_JSON_ARRAY] — a minimal,
# valid deploy.sh config (image source "build", so create mode never
# needs a registry path).
release_checksum_config_json() {
  local hub="$1" plugins="${2:-[]}"
  cat <<EOF
{
  "hub_name": "${hub}",
  "project_id": "demo-project",
  "region": "us-central1",
  "machine_size": "small",
  "disk_size_gb": 200,
  "chat_plugins": ${plugins},
  "container_images": {"source": "build", "registry": "", "force_rebuild": false},
  "admin_email": "admin@example.com",
  "update_policy": "auto",
  "release_channel": "nightly"
}
EOF
}

# _start_deploy_bg / _stop_deploy_bg / run_deploy_create_through_phase3:
# same pattern as test_deploy_base.sh's run_deploy_create and
# test_proxy_hardening.sh's run_deploy_create_through_phase4 (each test
# file defines its own copy -- see README.md "How it works"). Stops at
# the existing "settings-yaml-dev-mode-written" sentinel (touched by the
# gcloud stub's `compute ssh` case on the settings.yaml dev-mode write),
# which happens strictly after both the scion-binary and every
# chat-plugin install command in Phase 3, so gcloud_log is guaranteed to
# already contain their full --command text by the time this returns.
# Requires GCLOUD_STUB_SSH_SUCCEEDS=true (set here, unset on every exit
# path) so deploy.sh's SSH-readiness retry loop succeeds immediately
# instead of stopping there, the way run_deploy_create's callers do.
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

run_deploy_create_through_phase3() {
  local config_json="$1" config_file
  config_file="$(mktemp)"
  printf '%s' "$config_json" > "$config_file"
  local sentinel="${GCLOUD_STUB_STATE_DIR}/settings-yaml-dev-mode-written"
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
    # shellcheck disable=SC2034 # kept for parity with the other test
    # files' run_deploy_create_through_phaseN contract; this file's own
    # tests read DEPLOY_LOG, not DEPLOY_RC, on the timeout path.
    DEPLOY_RC=1
    DEPLOY_LOG="$(cat "$log_file")
FATAL: run_deploy_create_through_phase3: sentinel '${sentinel}' was never reached within 30000ms -- deploy.sh may be stuck, or this test's expectations no longer match its actual call sequence"
    rm -f "$config_file" "$log_file"
    unset GCLOUD_STUB_SSH_SUCCEEDS
    return 1
  fi
  _stop_deploy_bg "$pid"
  DEPLOY_LOG="$(cat "$log_file")"
  rm -f "$config_file" "$log_file"
  unset GCLOUD_STUB_SSH_SUCCEEDS
}

# =====================================================================
# Preflight (Phase 1, before any GCP resource is created)
# =====================================================================

# The chosen release has no SHA256SUMS and ALLOW_UNVERIFIED_RELEASE is
# unset: deploy.sh must fail before touching GCP at all, with a clear
# message naming both the cause and the override.
test_release_checksum_preflight_blocks_before_any_resource() {
  fresh_gcloud_state
  local config_file
  config_file="$(mktemp)"
  printf '%s' "$(release_checksum_config_json "$HUB")" > "$config_file"

  CURL_STUB_SHA256SUMS_MISSING=true run_expect_fail \
    bash "$DEPLOY_SH" --config "$config_file" --version v1.0.0-test
  rm -f "$config_file"

  assert_true "$([[ "$RUN_EXIT_CODE" -ne 0 ]] && echo true || echo false)" \
    "deploy.sh must exit non-zero when the chosen release has no SHA256SUMS and no override is set"
  assert_contains "$RUN_OUTPUT" "does not publish a SHA256SUMS checksums asset" \
    "the preflight failure must clearly name the cause"
  assert_contains "$RUN_OUTPUT" "ALLOW_UNVERIFIED_RELEASE=true" \
    "the preflight failure must clearly name the override"

  # The preflight runs after Phase 1's read-only zone/auth discovery
  # (`compute zones list`, `auth list`) but strictly before Phase 2's
  # first mutating call (`services enable`) -- and therefore before any
  # VM, network, firewall rule, or service account is created. Assert on
  # that mutation boundary, not on the log being empty: a zone/auth
  # lookup happening before the preflight is fine, creating a resource is
  # not.
  local log
  log="$(gcloud_log)"
  assert_not_contains "$log" "services enable" \
    "the checksum preflight must run before Phase 2 enables any API"
  assert_not_contains "$log" "compute instances create" \
    "the checksum preflight must run before the VM is created"
  assert_not_contains "$log" "iam service-accounts create" \
    "the checksum preflight must run before any service account is created"
  assert_not_contains "$log" "compute routers create" \
    "the checksum preflight must run before the Cloud Router is created"
  assert_not_contains "$log" "compute firewall-rules create" \
    "the checksum preflight must run before any firewall rule is created"
}

# ALLOW_UNVERIFIED_RELEASE=true must let the same release proceed, loudly.
test_release_checksum_preflight_override_proceeds_with_warning() {
  fresh_gcloud_state
  export CURL_STUB_SHA256SUMS_MISSING=true
  export ALLOW_UNVERIFIED_RELEASE=true
  run_deploy_create_through_phase3 "$(release_checksum_config_json "$HUB")"
  unset CURL_STUB_SHA256SUMS_MISSING ALLOW_UNVERIFIED_RELEASE

  assert_contains "$DEPLOY_LOG" "does not publish a SHA256SUMS checksums asset" \
    "the override path must still say why (loud warning, not a silent skip)"
  assert_contains "$DEPLOY_LOG" "Proceeding WITHOUT checksum verification because ALLOW_UNVERIFIED_RELEASE=true" \
    "the override path must warn explicitly, not fail silently"

  local log
  log="$(gcloud_log)"
  assert_true "$([[ -n "$(echo "$log" | grep "^compute instances create scion-hub-${HUB} " || true)" ]] && echo true || echo false)" \
    "the override must actually let deploy.sh proceed to create the VM, not just print a warning and stop"
}

# Default behavior (the ambient state of every other test in this suite,
# which knows nothing about SHA256SUMS): the release publishes checksums,
# so the preflight passes quietly and deploy.sh proceeds normally.
test_release_checksum_preflight_passes_when_checksums_exist() {
  fresh_gcloud_state
  run_deploy_create_through_phase3 "$(release_checksum_config_json "$HUB")"

  assert_contains "$DEPLOY_LOG" "SHA256SUMS found for v1.0.0-test" \
    "the preflight must report success when the release publishes checksums"
  assert_not_contains "$DEPLOY_LOG" "does not publish a SHA256SUMS" \
    "a release that does publish checksums must not trigger the missing-checksums message"
  assert_not_contains "$DEPLOY_LOG" "WARNING: could not download SHA256SUMS" \
    "a release that does publish checksums must not trigger the unverified-install warning"
}

# A DNS/network failure probing SHA256SUMS is not evidence the release
# lacks checksums, and must not be reported (or overridable) as if it
# were. Deliberately does not set ALLOW_UNVERIFIED_RELEASE at all: even
# the override shouldn't be mentioned for a connectivity problem.
test_release_checksum_preflight_connectivity_error_is_not_missing_checksums() {
  fresh_gcloud_state
  local config_file
  config_file="$(mktemp)"
  printf '%s' "$(release_checksum_config_json "$HUB")" > "$config_file"

  CURL_STUB_SHA256SUMS_CONN_ERROR=true run_expect_fail \
    bash "$DEPLOY_SH" --config "$config_file" --version v1.0.0-test
  rm -f "$config_file"

  assert_true "$([[ "$RUN_EXIT_CODE" -ne 0 ]] && echo true || echo false)" \
    "deploy.sh must exit non-zero when it cannot even reach the checksums endpoint"
  assert_contains "$RUN_OUTPUT" "Could not reach" \
    "a connectivity failure must be reported as a connectivity failure"
  assert_not_contains "$RUN_OUTPUT" "does not publish a SHA256SUMS checksums asset" \
    "a connectivity failure must not be reported as the release lacking checksums"
  assert_not_contains "$RUN_OUTPUT" "ALLOW_UNVERIFIED_RELEASE" \
    "the override must not be suggested for a connectivity problem it cannot fix"

  local log
  log="$(gcloud_log)"
  assert_not_contains "$log" "services enable" \
    "a connectivity failure must also be caught before any GCP resource is created"
}

# ALLOW_UNVERIFIED_RELEASE must be normalized to exactly "true" or "false"
# before it reaches the generated remote command text: any other value
# (including one containing a single quote, which would otherwise alter
# the remote command's own quoting) must collapse to "false".
test_release_checksum_allow_unverified_release_is_normalized() {
  fresh_gcloud_state
  export ALLOW_UNVERIFIED_RELEASE="yes'x"
  run_deploy_create_through_phase3 "$(release_checksum_config_json "$HUB")"
  unset ALLOW_UNVERIFIED_RELEASE
  local snippet
  snippet="$(_extract_binary_install_snippet "$(gcloud_log)")"

  assert_contains "$snippet" "'false' = 'true'" \
    "an unrecognized ALLOW_UNVERIFIED_RELEASE value must normalize to 'false' in the generated command"
  assert_not_contains "$snippet" "yes" \
    "the raw, unnormalized value must never reach the generated command text"
}

# =====================================================================
# Phase 3 install commands: download, verify, then (and only then) extract
# =====================================================================

# Both the scion-binary and the chat-plugin install commands must
# download SHA256SUMS and run sha256sum -c strictly before tar -xzf.
test_release_checksum_install_commands_verify_before_extract() {
  fresh_gcloud_state
  run_deploy_create_through_phase3 "$(release_checksum_config_json "$HUB" '["telegram"]')"
  local log
  log="$(gcloud_log)"

  # Default stub arch (x86_64) maps to amd64 -- see deploy.sh's ARCH_SUFFIX case.
  assert_contains "$log" "scion-linux-amd64.tar.gz" \
    "the binary install must download the arch-appropriate tarball"
  assert_contains "$log" "scion-plugin-telegram-linux-amd64.tar.gz" \
    "the plugin install must download the arch-appropriate plugin tarball"
  assert_contains "$log" "SHA256SUMS" \
    "the install commands must reference the SHA256SUMS asset"
  assert_contains "$log" "sha256sum -c -" \
    "the install commands must verify with sha256sum -c"

  local binary_verify_line binary_extract_line
  binary_verify_line="$(line_number "grep -E '^[0-9a-f]{64}  scion-linux-amd64\.tar\.gz\$' SHA256SUMS | sha256sum -c -" "$log")"
  binary_extract_line="$(line_number "tar -xzf /tmp/scion-linux-amd64.tar.gz -C /tmp" "$log")"
  assert_true "$([[ -n "$binary_verify_line" && -n "$binary_extract_line" && "$binary_verify_line" -lt "$binary_extract_line" ]] && echo true || echo false)" \
    "the scion binary must be checksum-verified before it is extracted"

  local plugin_verify_line plugin_extract_line
  plugin_verify_line="$(line_number "grep -E '^[0-9a-f]{64}  scion-plugin-telegram-linux-amd64\.tar\.gz\$' SHA256SUMS | sha256sum -c -" "$log")"
  plugin_extract_line="$(line_number "tar -xzf /tmp/scion-plugin-telegram-linux-amd64.tar.gz -C /tmp" "$log")"
  assert_true "$([[ -n "$plugin_verify_line" && -n "$plugin_extract_line" && "$plugin_verify_line" -lt "$plugin_extract_line" ]] && echo true || echo false)" \
    "the telegram plugin must be checksum-verified before it is extracted"

  assert_contains "$log" "no checksum entry for scion-linux-amd64.tar.gz in SHA256SUMS -- refusing to install" \
    "the binary install must refuse to install (not just warn) when SHA256SUMS has no matching entry, by default"
}

# =====================================================================
# Dry run: execute the *actual* generated download+verify+extract text
# (up to, not including, the sudo install step) against local fixtures.
# This executes deploy.sh's generated command text as a regression test.
# =====================================================================

# _extract_binary_install_snippet LOG — prints the scion-binary install
# command's own body, from its "Downloading scion binary..." echo through
# its "tar -xzf ... -C /tmp" line (inclusive), stripping deploy.sh's own
# leading indentation. Excludes everything from "sudo mv" onward (the
# `sudo mv`/`sudo chmod +x` install step needs real root, which this
# suite deliberately never provides -- see tests/lib/gcloud's
# "compute ssh" case): the download+verify logic under test here is
# entirely contained in the part before that.
_extract_binary_install_snippet() {
  local log="$1"
  printf '%s\n' "$log" | awk '
    /Downloading scion binary\.\.\./ { on = 1 }
    on { print }
    /tar -xzf \/tmp\/scion-linux-amd64\.tar\.gz -C \/tmp/ { if (on) exit }
  ' | sed -E 's/^[[:space:]]+//'
}

# _run_extracted_snippet WORKDIR SNIPPET — runs SNIPPET (deploy.sh's own
# generated text) as a real subprocess under `set -euo pipefail`, after
# rewriting every "/tmp" path reference in it to WORKDIR. deploy.sh's real
# remote command always operates on the *target VM's* /tmp; on the machine
# actually running this test suite, that would mean writing to (and
# `rm -f`-ing) fixed paths like /tmp/scion-linux-amd64.tar.gz on the real
# host -- clobbering a developer's own /tmp/scion, and racing a second
# concurrent run of this suite. WORKDIR is a fresh, per-call `mktemp -d`,
# so neither can happen. Its `curl` calls still resolve to tests/lib/curl
# (first on PATH), so CURL_STUB_* from the caller's environment governs
# what they see.
_run_extracted_snippet() {
  local workdir="$1" snippet="$2" rewritten
  # A single substitution pass over the *original* snippet: matching the
  # literal 4 characters "/tmp" (not "/tmp/") covers both "/tmp/NAME" and
  # the bare "/tmp" in "-C /tmp"/"cd /tmp", leaving the following "/" (or
  # nothing) exactly where it was. This has to be one pass, not two:
  # WORKDIR is itself a path under this suite's own TMPDIR, which is
  # itself under /tmp (see run.sh), so a second pass over the
  # already-rewritten text would find and mangle /tmp again inside
  # WORKDIR's own value.
  rewritten="${snippet//\/tmp/${workdir}}"
  bash -c "set -euo pipefail
$rewritten" 2>&1
}

# _make_tarball DIR NAME CONTENT — writes DIR/NAME as a gzip tarball
# whose sole member, "scion", contains CONTENT.
_make_tarball() {
  local dir="$1" name="$2" content="$3" work
  work="$(mktemp -d)"
  printf '%s' "$content" > "${work}/scion"
  tar -czf "${dir}/${name}" -C "$work" scion
  rm -rf "$work"
}

test_release_checksum_dry_run_good_tarball_extracts() {
  fresh_gcloud_state
  run_deploy_create_through_phase3 "$(release_checksum_config_json "$HUB")"
  local snippet
  snippet="$(_extract_binary_install_snippet "$(gcloud_log)")"
  assert_contains "$snippet" "sha256sum -c -" \
    "sanity: the extracted snippet must actually contain the verification step"

  local fixdir work
  fixdir="$(mktemp -d)"
  work="$(mktemp -d)"
  _make_tarball "$fixdir" "scion-linux-amd64.tar.gz" "real-scion-binary-payload"
  (cd "$fixdir" && sha256sum scion-linux-amd64.tar.gz > SHA256SUMS)

  local out rc
  out="$(CURL_STUB_FIXTURE_DIR="$fixdir" _run_extracted_snippet "$work" "$snippet")"
  rc=$?
  rm -rf "$fixdir"

  assert_eq "0" "$rc" "a matching tarball and SHA256SUMS entry must verify and extract cleanly"
  assert_contains "$out" "OK" "sha256sum -c must report OK for a matching tarball"
  assert_eq "real-scion-binary-payload" "$(cat "${work}/scion" 2>/dev/null || echo '<missing>')" \
    "tar -xzf must actually have extracted the verified tarball's payload"
  rm -rf "$work"
}

test_release_checksum_dry_run_mismatched_tarball_fails_closed() {
  fresh_gcloud_state
  run_deploy_create_through_phase3 "$(release_checksum_config_json "$HUB")"
  local snippet
  snippet="$(_extract_binary_install_snippet "$(gcloud_log)")"

  local fixdir work
  fixdir="$(mktemp -d)"
  work="$(mktemp -d)"
  # SHA256SUMS matches the ORIGINAL payload; the tarball actually served
  # under that same filename has different bytes (corrupted or modified in
  # transit).
  _make_tarball "$fixdir" "scion-linux-amd64.tar.gz" "original-payload"
  (cd "$fixdir" && sha256sum scion-linux-amd64.tar.gz > SHA256SUMS)
  _make_tarball "$fixdir" "scion-linux-amd64.tar.gz" "modified-payload"

  local out rc
  out="$(CURL_STUB_FIXTURE_DIR="$fixdir" _run_extracted_snippet "$work" "$snippet")"
  rc=$?
  rm -rf "$fixdir"

  assert_true "$([[ "$rc" -ne 0 ]] && echo true || echo false)" \
    "a hash mismatch must fail the install command, not just warn"
  assert_contains "$out" "FAILED" "sha256sum -c must report FAILED for a mismatched tarball"
  assert_true "$([[ ! -e "${work}/scion" ]] && echo true || echo false)" \
    "tar -xzf must never have run against a tarball that failed verification"
  rm -rf "$work"
}

test_release_checksum_dry_run_missing_entry_fails_closed() {
  fresh_gcloud_state
  run_deploy_create_through_phase3 "$(release_checksum_config_json "$HUB")"
  local snippet
  snippet="$(_extract_binary_install_snippet "$(gcloud_log)")"

  local fixdir work
  fixdir="$(mktemp -d)"
  work="$(mktemp -d)"
  _make_tarball "$fixdir" "scion-linux-amd64.tar.gz" "real-scion-binary-payload"
  echo "0000000000000000000000000000000000000000000000000000000000000000  some-other-file.tar.gz" \
    > "${fixdir}/SHA256SUMS"

  local out rc
  out="$(CURL_STUB_FIXTURE_DIR="$fixdir" _run_extracted_snippet "$work" "$snippet")"
  rc=$?
  rm -rf "$fixdir"

  assert_true "$([[ "$rc" -ne 0 ]] && echo true || echo false)" \
    "a SHA256SUMS with no matching entry must fail the install command"
  assert_contains "$out" "no checksum entry for scion-linux-amd64.tar.gz" \
    "the failure must name the missing entry explicitly, not just fail sha256sum -c"
  assert_true "$([[ ! -e "${work}/scion" ]] && echo true || echo false)" \
    "tar -xzf must never have run when the checksum entry itself was missing"
  rm -rf "$work"
}

test_release_checksum_dry_run_missing_sums_asset_fails_closed() {
  fresh_gcloud_state
  run_deploy_create_through_phase3 "$(release_checksum_config_json "$HUB")"
  local snippet
  snippet="$(_extract_binary_install_snippet "$(gcloud_log)")"

  # A fixture dir that serves the tarball but has no SHA256SUMS file at
  # all simulates the checksums asset itself being unavailable (a 404),
  # as opposed to the "asset exists but has no matching entry" case
  # covered above. Deliberately not CURL_STUB_SHA256SUMS_MISSING here: if
  # this fixture dir were empty of everything, the *tarball* request
  # (which isn't a SHA256SUMS request) would fall through to a real,
  # unstubbed curl instead of being served -- see tests/lib/curl.
  local fixdir work
  fixdir="$(mktemp -d)"
  work="$(mktemp -d)"
  _make_tarball "$fixdir" "scion-linux-amd64.tar.gz" "real-scion-binary-payload"

  local out rc
  out="$(CURL_STUB_FIXTURE_DIR="$fixdir" _run_extracted_snippet "$work" "$snippet")"
  rc=$?
  rm -rf "$fixdir"

  assert_true "$([[ "$rc" -ne 0 ]] && echo true || echo false)" \
    "a missing SHA256SUMS asset at install time must fail the install command"
  assert_contains "$out" "could not download SHA256SUMS" \
    "the failure must say the checksums asset itself could not be fetched"
  assert_true "$([[ ! -e "${work}/scion" ]] && echo true || echo false)" \
    "tar -xzf must never have run when SHA256SUMS itself could not be downloaded"
  rm -rf "$work"
}

# --- ALLOW_UNVERIFIED_RELEASE inside the remote install commands ---
# Two of the four dry-run tests above (mismatch, missing SUMS) run with
# the default `deploy.sh`-generated text, i.e. ALLOW_UNVERIFIED_RELEASE=
# false baked in. The two tests below exercise the *other* value, generated by
# exporting ALLOW_UNVERIFIED_RELEASE=true before driving deploy.sh, to
# cover two properties: a hash mismatch is never overridable, and
# "missing SUMS + override" really does proceed (with a warning), not
# just "still fails but with different text."

test_release_checksum_dry_run_override_does_not_bypass_mismatch() {
  fresh_gcloud_state
  export ALLOW_UNVERIFIED_RELEASE=true
  run_deploy_create_through_phase3 "$(release_checksum_config_json "$HUB")"
  unset ALLOW_UNVERIFIED_RELEASE
  local snippet
  snippet="$(_extract_binary_install_snippet "$(gcloud_log)")"
  assert_contains "$snippet" "'true' = 'true'" \
    "sanity: the extracted snippet must actually carry ALLOW_UNVERIFIED_RELEASE=true"

  local fixdir work
  fixdir="$(mktemp -d)"
  work="$(mktemp -d)"
  _make_tarball "$fixdir" "scion-linux-amd64.tar.gz" "original-payload"
  (cd "$fixdir" && sha256sum scion-linux-amd64.tar.gz > SHA256SUMS)
  _make_tarball "$fixdir" "scion-linux-amd64.tar.gz" "modified-payload"

  local out rc
  out="$(CURL_STUB_FIXTURE_DIR="$fixdir" _run_extracted_snippet "$work" "$snippet")"
  rc=$?
  rm -rf "$fixdir"

  assert_true "$([[ "$rc" -ne 0 ]] && echo true || echo false)" \
    "ALLOW_UNVERIFIED_RELEASE=true must not let a hash mismatch through"
  assert_contains "$out" "FAILED" \
    "a mismatch under the override must still report FAILED from sha256sum -c"
  assert_true "$([[ ! -e "${work}/scion" ]] && echo true || echo false)" \
    "tar -xzf must never have run: a present-but-wrong checksum is never overridable"
  rm -rf "$work"
}

test_release_checksum_dry_run_override_allows_missing_sums() {
  fresh_gcloud_state
  export ALLOW_UNVERIFIED_RELEASE=true
  run_deploy_create_through_phase3 "$(release_checksum_config_json "$HUB")"
  unset ALLOW_UNVERIFIED_RELEASE
  local snippet
  snippet="$(_extract_binary_install_snippet "$(gcloud_log)")"

  # Same fixture shape as the no-override missing-SUMS test above: a
  # fixture dir with the tarball but no SHA256SUMS file, so the SUMS
  # request 404s.
  local fixdir work
  fixdir="$(mktemp -d)"
  work="$(mktemp -d)"
  _make_tarball "$fixdir" "scion-linux-amd64.tar.gz" "real-scion-binary-payload"

  local out rc
  out="$(CURL_STUB_FIXTURE_DIR="$fixdir" _run_extracted_snippet "$work" "$snippet")"
  rc=$?
  rm -rf "$fixdir"

  assert_eq "0" "$rc" \
    "ALLOW_UNVERIFIED_RELEASE=true must let a missing SHA256SUMS asset proceed to extraction"
  assert_contains "$out" "installing UNVERIFIED" \
    "the override path must print its own loud warning, not install silently"
  assert_eq "real-scion-binary-payload" "$(cat "${work}/scion" 2>/dev/null || echo '<missing>')" \
    "extraction must actually have happened under the override"
  rm -rf "$work"
}
