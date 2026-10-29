# scripts/single-node-vm/tests/test_rollback_guard.sh -- covers the
# boot-failure guard (design D1, Phase 3, ptone/scion#6009):
#   - scion-hub-rollback.sh run against a temporary install root with a
#     stubbed systemctl: the start-limit-hit gate, the inside-releases/
#     check on `previous` (relative "..", absolute and chained symlinks),
#     the current<->previous swap, the rollback.marker contents, a second
#     run, and that a rollback is never swapped back;
#   - its root path, with stubbed id/runuser/systemctl: root runs only
#     systemctl and hands every read and write under the install root to
#     the owner through runuser;
#   - scion-layout.sh install-guard: the root-owned script install.
#
# Sourced by run.sh, never executed.
# shellcheck shell=bash

ROLLBACK_SH="${TIER_DIR}/scion-hub-rollback.sh"
RB_LAYOUT_SH="${TIER_DIR}/scion-layout.sh"

# =====================================================================
# Helpers
# =====================================================================

# _rb_new_host -- a fake host: RB_ROOT with releases/nightly-20261020 and
# releases/nightly-20261023 (each with a scion binary), current -> the
# newer one, previous -> the older one; RB_SHIM, a PATH dir with a
# systemctl stub (see _rb_systemctl); RB_CALLS, the stubs' call log.
# Defaults: Result=start-limit-hit after a crash (exit status 1).
_rb_new_host() {
  RB_DIR="$(mktemp -d)"
  RB_ROOT="${RB_DIR}/opt/scion"
  RB_SHIM="${RB_DIR}/shim"
  RB_CALLS="${RB_DIR}/calls.log"
  mkdir -p "${RB_ROOT}/releases/nightly-20261020" "${RB_ROOT}/releases/nightly-20261023" "${RB_ROOT}/snapshots" "$RB_SHIM"
  printf '#!/bin/sh\nexit 0\n' > "${RB_ROOT}/releases/nightly-20261020/scion"
  printf '#!/bin/sh\nexit 1\n' > "${RB_ROOT}/releases/nightly-20261023/scion"
  chmod 0755 "${RB_ROOT}/releases/"*/scion
  ln -s releases/nightly-20261023 "${RB_ROOT}/current"
  ln -s releases/nightly-20261020 "${RB_ROOT}/previous"
  : > "$RB_CALLS"
  _rb_props start-limit-hit 1 1
  cat > "${RB_SHIM}/systemctl" <<EOF
#!/usr/bin/env bash
echo "systemctl \$*" >> "${RB_CALLS}"
if [[ "\$1" == show ]]; then
  # systemctl show -p PROP --value UNIT
  grep "^\$3=" "${RB_DIR}/props" | head -1 | cut -d= -f2-
fi
exit 0
EOF
  chmod 0755 "${RB_SHIM}/systemctl"
}

# _rb_props RESULT EXEC_MAIN_CODE EXEC_MAIN_STATUS -- what the systemctl
# stub reports for scion-hub.service.
_rb_props() {
  printf 'Result=%s\nExecMainCode=%s\nExecMainStatus=%s\n' "$1" "$2" "$3" > "${RB_DIR}/props"
}

# _rb_run -- runs scion-hub-rollback.sh unprivileged against the fake host
# (no owner: the owner step runs in-process). Sets RB_RC and RB_OUT.
_rb_run() {
  RB_OUT="$(PATH="${RB_SHIM}:${PATH}" SCION_INSTALL_ROOT="$RB_ROOT" SCION_INSTALL_OWNER="" bash "$ROLLBACK_SH" 2>&1)"
  RB_RC=$?
}

# _rb_state -- canonical listing of the install root (links, files and
# their contents), so two states can be compared.
_rb_state() {
  (
    cd "$RB_ROOT" || exit 1
    find . | LC_ALL=C sort | while IFS= read -r p; do
      if [[ -L "$p" ]]; then
        echo "$p link $(readlink "$p")"
      elif [[ -f "$p" ]]; then
        echo "$p file $(stat -c %a "$p") $(sha256sum < "$p" | cut -c1-16)"
      else
        echo "$p dir"
      fi
    done
  )
}

# _rb_assert_refused WHAT -- the last run refused: non-zero exit, the
# install root unchanged from RB_BEFORE, no marker, hub not started.
_rb_assert_refused() {
  assert_true "$([[ "$RB_RC" -ne 0 ]] && echo true || echo false)" "$1: the rollback must be refused (output: ${RB_OUT})"
  assert_contains "$RB_OUT" "refusing to roll back" "$1: the refusal must say why"
  assert_eq "$RB_BEFORE" "$(_rb_state)" "$1: nothing under the install root may change"
  assert_false "$([[ -e "${RB_ROOT}/rollback.marker" ]] && echo true)" "$1: no marker may be written"
  assert_not_contains "$(cat "$RB_CALLS")" "systemctl start" "$1: the hub must not be started"
  assert_not_contains "$(cat "$RB_CALLS")" "systemctl reset-failed" "$1: the failed state must be kept"
}

# =====================================================================
# The start-limit-hit gate
# =====================================================================

test_rollback_noop_unless_start_limit_hit() {
  local result
  for result in exit-code success signal core-dump timeout ""; do
    _rb_new_host
    _rb_props "$result" 1 1
    RB_BEFORE="$(_rb_state)"
    _rb_run
    assert_eq "0" "$RB_RC" "Result='${result}': not a start-limit failure is a successful no-op (output: ${RB_OUT})"
    assert_contains "$RB_OUT" "not start-limit-hit; nothing to do" "Result='${result}': the no-op must say why"
    assert_eq "$RB_BEFORE" "$(_rb_state)" "Result='${result}': nothing may change"
    assert_eq "systemctl show -p Result --value scion-hub.service" "$(cat "$RB_CALLS")" \
      "Result='${result}': only the Result may be read; no reset-failed, no start"
    rm -rf "$RB_DIR"
  done
}

# The start limit also counts clean restarts (self-SIGTERM on update or
# Restart Hub): five intentional restarts within 300 s hit it with a clean
# last exit, and the hub is restarted, not rolled back.
test_rollback_clean_exits_restart_without_swap() {
  local exit_spec
  for exit_spec in "1 0" "2 15" "2 2" "2 1" "2 13"; do
    _rb_new_host
    # shellcheck disable=SC2086 # two words: code and status
    _rb_props start-limit-hit $exit_spec
    RB_BEFORE="$(_rb_state)"
    _rb_run
    assert_eq "0" "$RB_RC" "clean exit (${exit_spec}): must succeed (output: ${RB_OUT})"
    assert_eq "$RB_BEFORE" "$(_rb_state)" "clean exit (${exit_spec}): no swap and no marker"
    assert_contains "$RB_OUT" "not a crash loop" "clean exit (${exit_spec}): must say why it does not roll back"
    assert_contains "$(cat "$RB_CALLS")" $'systemctl reset-failed scion-hub.service\nsystemctl start scion-hub.service' \
      "clean exit (${exit_spec}): the hub stopped by the start limit must be started again"
    rm -rf "$RB_DIR"
  done
}

# =====================================================================
# Swap and marker
# =====================================================================

test_rollback_swaps_current_and_previous() {
  local exit_spec
  # exit 1, exec failure (203), killed by KILL/SEGV/ABRT, ABRT with core.
  for exit_spec in "1 1" "1 203" "2 9" "2 11" "2 6" "3 6"; do
    _rb_new_host
    # shellcheck disable=SC2086 # two words: code and status
    _rb_props start-limit-hit $exit_spec
    _rb_run
    assert_eq "0" "$RB_RC" "crash (${exit_spec}): the rollback must succeed (output: ${RB_OUT})"
    assert_eq "releases/nightly-20261020" "$(readlink "${RB_ROOT}/current")" "crash (${exit_spec}): current must point at the old previous"
    assert_eq "releases/nightly-20261023" "$(readlink "${RB_ROOT}/previous")" "crash (${exit_spec}): previous must point at the failed release"
    assert_false "$([[ -e "${RB_ROOT}/current.new" || -e "${RB_ROOT}/previous.new" || -L "${RB_ROOT}/current.new" ]] && echo true)" \
      "crash (${exit_spec}): no temporary links may be left"
    assert_eq $'systemctl show -p Result --value scion-hub.service\nsystemctl show -p ExecMainCode --value scion-hub.service\nsystemctl show -p ExecMainStatus --value scion-hub.service\nsystemctl reset-failed scion-hub.service\nsystemctl start scion-hub.service' \
      "$(cat "$RB_CALLS")" "crash (${exit_spec}): reset-failed then start, after the swap"
    rm -rf "$RB_DIR"
  done
}

test_rollback_writes_marker() {
  _rb_new_host
  _rb_run
  assert_eq "0" "$RB_RC" "the rollback must succeed (output: ${RB_OUT})"
  local marker="${RB_ROOT}/rollback.marker"
  assert_true "$([[ -f "$marker" && ! -L "$marker" ]] && echo true || echo false)" "the marker must be a regular file"
  assert_eq "644" "$(stat -c %a "$marker")" "the marker must be mode 0644"
  assert_eq "1" "$(wc -l < "$marker" | tr -d ' ')" "the marker must be a single line ending in a newline"
  assert_true "$(grep -Eqx '\{"at":"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z","from":"nightly-20261023","to":"nightly-20261020"\}' "$marker" && echo true || echo false)" \
    "the marker must be {\"at\":<RFC3339 UTC>,\"from\":<failed tag>,\"to\":<restored tag>} (got: $(cat "$marker"))"
  assert_eq "valid" "$("$PYTHON" -c 'import json,sys,datetime; d=json.load(open(sys.argv[1])); datetime.datetime.strptime(d["at"], "%Y-%m-%dT%H:%M:%SZ"); print("valid" if sorted(d)==["at","from","to"] else "keys")' "$marker" 2>&1)" \
    "the marker must parse as JSON with exactly at/from/to"
  assert_eq "" "$(find "$RB_ROOT" -maxdepth 1 -name 'rollback.marker.*')" "no temporary marker may be left"
  rm -rf "$RB_DIR"
}

# =====================================================================
# Refusals: previous must resolve inside releases/
# =====================================================================

test_rollback_refuses_previous_outside_releases() {
  local case_name
  for case_name in dotdot-relative dotdot-through-releases absolute chained releases-itself root-itself missing dangling not-a-link no-binary; do
    _rb_new_host
    mkdir -p "${RB_DIR}/evil"
    printf '#!/bin/sh\nexit 0\n' > "${RB_DIR}/evil/scion"
    chmod 0755 "${RB_DIR}/evil/scion"
    rm -f "${RB_ROOT}/previous"
    case "$case_name" in
      dotdot-relative) ln -s ../../evil "${RB_ROOT}/previous" ;;
      dotdot-through-releases) ln -s releases/../../../evil "${RB_ROOT}/previous" ;;
      absolute) ln -s "${RB_DIR}/evil" "${RB_ROOT}/previous" ;;
      chained)
        # releases/<tag> itself is a link out of releases/.
        ln -s "${RB_DIR}/evil" "${RB_ROOT}/releases/nightly-20261001"
        ln -s releases/nightly-20261001 "${RB_ROOT}/previous" ;;
      releases-itself) ln -s releases "${RB_ROOT}/previous" ;;
      root-itself) ln -s . "${RB_ROOT}/previous" ;;
      missing) ;;
      dangling) ln -s releases/nightly-20261001 "${RB_ROOT}/previous" ;;
      not-a-link) mkdir "${RB_ROOT}/previous" ;;
      no-binary)
        mkdir "${RB_ROOT}/releases/nightly-20261001"
        ln -s releases/nightly-20261001 "${RB_ROOT}/previous" ;;
    esac
    RB_BEFORE="$(_rb_state)"
    _rb_run
    _rb_assert_refused "previous ${case_name}"
    assert_eq "releases/nightly-20261023" "$(readlink "${RB_ROOT}/current")" "previous ${case_name}: current must be unchanged"
    rm -rf "$RB_DIR"
  done
}

test_rollback_refuses_current_outside_releases_or_equal() {
  local case_name
  for case_name in absolute dotdot-relative dotdot-through-releases chained same; do
    _rb_new_host
    mkdir -p "${RB_DIR}/evil"
    rm -f "${RB_ROOT}/current"
    case "$case_name" in
      absolute) ln -s "${RB_DIR}/evil" "${RB_ROOT}/current" ;;
      dotdot-relative) ln -s ../../evil "${RB_ROOT}/current" ;;
      dotdot-through-releases) ln -s releases/../../../evil "${RB_ROOT}/current" ;;
      chained)
        ln -s "${RB_DIR}/evil" "${RB_ROOT}/releases/nightly-20261030"
        ln -s releases/nightly-20261030 "${RB_ROOT}/current" ;;
      same) ln -s releases/nightly-20261020 "${RB_ROOT}/current" ;;
    esac
    RB_BEFORE="$(_rb_state)"
    _rb_run
    _rb_assert_refused "current ${case_name}"
    rm -rf "$RB_DIR"
  done
}

test_rollback_refuses_unsafe_install_root() {
  _rb_new_host
  RB_OUT="$(PATH="${RB_SHIM}:${PATH}" SCION_INSTALL_ROOT="${RB_ROOT}/../scion" SCION_INSTALL_OWNER="" bash "$ROLLBACK_SH" 2>&1)"
  RB_RC=$?
  assert_true "$([[ "$RB_RC" -ne 0 ]] && echo true || echo false)" "a non-canonical install root must be refused"
  assert_eq "" "$(cat "$RB_CALLS")" "nothing may run before the install root is validated"
  rm -rf "$RB_DIR"
}

# =====================================================================
# Second runs: idempotent, never swaps back
# =====================================================================

# A second run with the hub healthy again (Result no longer
# start-limit-hit, as after reset-failed + start): a no-op.
test_rollback_second_run_after_recovery_is_noop() {
  _rb_new_host
  _rb_run
  assert_eq "0" "$RB_RC" "the first run must succeed (output: ${RB_OUT})"
  local after_first
  after_first="$(_rb_state)"
  _rb_props success 1 0
  : > "$RB_CALLS"
  _rb_run
  assert_eq "0" "$RB_RC" "the second run must succeed (output: ${RB_OUT})"
  assert_eq "$after_first" "$(_rb_state)" "the second run must change nothing (marker included)"
  assert_not_contains "$(cat "$RB_CALLS")" "systemctl start" "the second run must not start the hub"
  rm -rf "$RB_DIR"
}

# The restored release crash-loops too: the start limit stops it again and
# the guard runs again. It must not swap back to the failed release, must
# not rewrite the marker and must not start the hub.
test_rollback_never_swaps_back() {
  _rb_new_host
  _rb_run
  assert_eq "0" "$RB_RC" "the first run must succeed (output: ${RB_OUT})"
  local after_first
  after_first="$(_rb_state)"
  : > "$RB_CALLS"
  _rb_run
  assert_eq "0" "$RB_RC" "a repeat of the same rollback is a no-op, not an error (output: ${RB_OUT})"
  assert_contains "$RB_OUT" "not swapping back" "the repeat must say why"
  assert_eq "releases/nightly-20261020" "$(readlink "${RB_ROOT}/current")" "current must stay on the restored release"
  assert_eq "$after_first" "$(_rb_state)" "nothing may change, the marker included"
  assert_not_contains "$(cat "$RB_CALLS")" "systemctl start" "the hub must stay stopped for an operator"
  assert_not_contains "$(cat "$RB_CALLS")" "systemctl reset-failed" "the failed state must stay visible"
  # And a third time: still nothing.
  _rb_run
  assert_eq "$after_first" "$(_rb_state)" "a third run must change nothing either"
  rm -rf "$RB_DIR"
}

# A marker from an earlier, different rollback does not block a new one
# (e.g. a later downgrade to the release an old rollback restored).
test_rollback_old_marker_for_other_pair_does_not_block() {
  _rb_new_host
  printf '{"at":"2026-10-01T00:00:00Z","from":"nightly-20261005","to":"nightly-20261023"}\n' > "${RB_ROOT}/rollback.marker"
  _rb_run
  assert_eq "0" "$RB_RC" "a marker for another pair must not block the rollback (output: ${RB_OUT})"
  assert_eq "releases/nightly-20261020" "$(readlink "${RB_ROOT}/current")" "the rollback must happen"
  assert_contains "$(cat "${RB_ROOT}/rollback.marker")" '"from":"nightly-20261023","to":"nightly-20261020"' "the marker must be rewritten for this rollback"
  rm -rf "$RB_DIR"
}

# The marker is written before any link changes: if it cannot be written
# (here a directory is in its place; on a VM, a full disk), nothing is
# swapped and the hub is not started.
test_rollback_marker_write_failure_means_no_swap() {
  _rb_new_host
  mkdir "${RB_ROOT}/rollback.marker"
  RB_BEFORE="$(_rb_state)"
  _rb_run
  assert_true "$([[ "$RB_RC" -ne 0 ]] && echo true || echo false)" "a failed marker write must fail the run (output: ${RB_OUT})"
  assert_contains "$RB_OUT" "could not write" "the failure must say the marker could not be written"
  assert_eq "$RB_BEFORE" "$(_rb_state)" "no link may change and no temporary marker may be left"
  assert_not_contains "$(cat "$RB_CALLS")" "systemctl start" "the hub must not be started"
  rm -rf "$RB_DIR"
}

# A planted rollback.marker symlink is replaced, never written through.
test_rollback_marker_symlink_not_followed() {
  _rb_new_host
  printf 'keep\n' > "${RB_DIR}/victim"
  ln -s "${RB_DIR}/victim" "${RB_ROOT}/rollback.marker"
  _rb_run
  assert_eq "0" "$RB_RC" "the rollback must succeed (output: ${RB_OUT})"
  assert_eq "keep" "$(cat "${RB_DIR}/victim")" "the symlink target must not be written"
  assert_false "$([[ -L "${RB_ROOT}/rollback.marker" ]] && echo true)" "the marker must replace the symlink"
  rm -rf "$RB_DIR"
}

# =====================================================================
# Root path: root runs only systemctl; the install root is the owner's
# =====================================================================

# _rb_root_shims -- stubs that make the script take its root path, run
# unprivileged: `id -u` prints 0; `runuser -u USER -- CMD...` logs its
# arguments, then (and only then) makes RB_ROOT accessible again and runs
# CMD as the current user. RB_ROOT is mode 000 until that point, so any
# root-side access to it before runuser fails (for a non-root test user).
_rb_root_shims() {
  local real_id
  real_id="$(command -v id)"
  cat > "${RB_SHIM}/id" <<EOF
#!/usr/bin/env bash
if [[ "\$1" == "-u" && \$# -eq 1 ]]; then echo 0; exit 0; fi
exec "${real_id}" "\$@"
EOF
  cat > "${RB_SHIM}/runuser" <<EOF
#!/usr/bin/env bash
echo "runuser \$*" >> "${RB_CALLS}"
[[ "\$1" == "-u" && "\$3" == "--" ]] || { echo "runuser stub: unexpected arguments: \$*" >&2; exit 97; }
shift 3
chmod 0755 "${RB_ROOT}"
exec "\$@"
EOF
  chmod 0755 "${RB_SHIM}/id" "${RB_SHIM}/runuser"
}

# _rb_run_as_root -- runs the script on its root path with the default
# owner (scion:scion). Sets RB_RC and RB_OUT.
_rb_run_as_root() {
  chmod 000 "$RB_ROOT"
  RB_OUT="$(PATH="${RB_SHIM}:${PATH}" SCION_INSTALL_ROOT="$RB_ROOT" bash "$ROLLBACK_SH" 2>&1)"
  RB_RC=$?
  chmod 0755 "$RB_ROOT"
}

test_rollback_root_path_hands_install_root_to_owner() {
  _rb_new_host
  _rb_root_shims
  _rb_run_as_root
  assert_eq "0" "$RB_RC" "the root path must succeed (output: ${RB_OUT})"
  assert_eq $'systemctl show -p Result --value scion-hub.service\nsystemctl show -p ExecMainCode --value scion-hub.service\nsystemctl show -p ExecMainStatus --value scion-hub.service\nrunuser -u scion -- env SCION_INSTALL_ROOT='"${RB_ROOT}"$' SCION_INSTALL_OWNER= bash -s -- __swap\nsystemctl reset-failed scion-hub.service\nsystemctl start scion-hub.service' \
    "$(cat "$RB_CALLS")" \
    "root must only query systemctl, hand the checks, swap and marker to the owner via runuser, then reset-failed and start"
  assert_eq "releases/nightly-20261020" "$(readlink "${RB_ROOT}/current")" "the owner step must have swapped current"
  assert_true "$([[ -f "${RB_ROOT}/rollback.marker" ]] && echo true || echo false)" "the owner step must have written the marker"
  if [[ "$(id -u)" == "0" ]]; then
    echo "NOTE [${CURRENT_TEST}]: running as root, so the mode-000 install root does not prove the absence of root-side access; the call sequence above still does"
  fi
  rm -rf "$RB_DIR"
}

# The script source must not touch the install root on the root side:
# every reference to INSTALL_ROOT outside its validation lives in the
# owner-side functions.
test_rollback_root_side_never_references_install_root() {
  local root_side
  root_side="$(sed -n '/^# Root side\./,$p' "$ROLLBACK_SH" | grep -v '^ *#')"
  assert_contains "$root_side" "run_as_owner()" "the root-side section must be found"
  assert_eq "1" "$(printf '%s\n' "$root_side" | grep -c 'INSTALL_ROOT')" \
    "the root side may name INSTALL_ROOT only to pass it to the owner step (runuser env)"
  # shellcheck disable=SC2016 # a literal from the script source
  assert_contains "$root_side" 'env SCION_INSTALL_ROOT="$INSTALL_ROOT" SCION_INSTALL_OWNER=""' \
    "that one reference must be the runuser hand-off"
}

test_rollback_root_path_refusal_does_not_start() {
  _rb_new_host
  _rb_root_shims
  rm -f "${RB_ROOT}/previous"
  ln -s ../../evil "${RB_ROOT}/previous"
  _rb_run_as_root
  assert_true "$([[ "$RB_RC" -ne 0 ]] && echo true || echo false)" "an owner-side refusal must fail the unit (output: ${RB_OUT})"
  assert_not_contains "$(cat "$RB_CALLS")" "systemctl start" "the hub must not be started after a refusal"
  assert_not_contains "$(cat "$RB_CALLS")" "systemctl reset-failed" "the failed state must be kept"
  assert_eq "releases/nightly-20261023" "$(readlink "${RB_ROOT}/current")" "current must be unchanged"
  rm -rf "$RB_DIR"
}

test_rollback_root_path_noop_never_calls_runuser() {
  _rb_new_host
  _rb_root_shims
  _rb_props exit-code 1 1
  _rb_run_as_root
  assert_eq "0" "$RB_RC" "the no-op must succeed (output: ${RB_OUT})"
  assert_not_contains "$(cat "$RB_CALLS")" "runuser" "a no-op must not run the owner step"
  rm -rf "$RB_DIR"
}

test_rollback_owner_step_refuses_root_with_owner() {
  _rb_new_host
  _rb_root_shims
  RB_OUT="$(PATH="${RB_SHIM}:${PATH}" SCION_INSTALL_ROOT="$RB_ROOT" bash "$ROLLBACK_SH" __swap 2>&1)"
  RB_RC=$?
  assert_true "$([[ "$RB_RC" -ne 0 ]] && echo true || echo false)" "__swap must refuse to run as root with an owner configured"
  assert_eq "releases/nightly-20261023" "$(readlink "${RB_ROOT}/current")" "nothing may change"
  rm -rf "$RB_DIR"
}

# =====================================================================
# scion-layout.sh install-guard
# =====================================================================

# _rb_guard CMD... -- runs scion-layout.sh against RB_DIR, with the guard
# path under RB_DIR. Sets RB_RC and RB_OUT.
_rb_guard() {
  RB_OUT="$(SCION_INSTALL_ROOT="$RB_ROOT" SCION_BIN_LINK="${RB_DIR}/usr/local/bin/scion" SCION_INSTALL_OWNER="" \
    SCION_ROLLBACK_PATH="${RB_GUARD}" bash "$RB_LAYOUT_SH" "$@" 2>&1)"
  RB_RC=$?
}

test_install_guard_creates_root_owned_script() {
  _rb_new_host
  RB_GUARD="${RB_DIR}/usr/local/libexec/scion/scion-hub-rollback.sh"
  mkdir -p "${RB_DIR}/usr/local"
  _rb_guard install-guard "$ROLLBACK_SH"
  assert_eq "0" "$RB_RC" "install-guard must succeed (output: ${RB_OUT})"
  assert_true "$(cmp -s "$ROLLBACK_SH" "$RB_GUARD" && echo true || echo false)" "the installed script must be byte-identical"
  assert_eq "755|$(id -un)" "$(stat -c '%a|%U' "$RB_GUARD")" "the script must be 0755, owned by the installing user (root on the VM)"
  assert_eq "directory|755|$(id -un)" "$(stat -c '%F|%a|%U' "${RB_DIR}/usr/local/libexec")" "missing dirs must be created 0755"
  assert_eq "directory|755|$(id -un)" "$(stat -c '%F|%a|%U' "${RB_DIR}/usr/local/libexec/scion")" "the script dir must be 0755"
  local first
  first="$(find "${RB_DIR}/usr" -printf '%p %y %m\n' | LC_ALL=C sort; sha256sum < "$RB_GUARD")"
  _rb_guard install-guard "$ROLLBACK_SH"
  assert_eq "0" "$RB_RC" "a second install-guard must succeed (output: ${RB_OUT})"
  assert_contains "$RB_OUT" "already installed" "a second run must be a no-op"
  assert_eq "$first" "$(find "${RB_DIR}/usr" -printf '%p %y %m\n' | LC_ALL=C sort; sha256sum < "$RB_GUARD")" \
    "a second run must leave the same state"
  rm -rf "$RB_DIR"
}

test_install_guard_replaces_changed_script() {
  _rb_new_host
  RB_GUARD="${RB_DIR}/usr/local/libexec/scion/scion-hub-rollback.sh"
  mkdir -p "${RB_DIR}/usr/local/libexec/scion"
  printf 'old\n' > "$RB_GUARD"
  chmod 0700 "$RB_GUARD"
  _rb_guard install-guard "$ROLLBACK_SH"
  assert_eq "0" "$RB_RC" "install-guard must succeed (output: ${RB_OUT})"
  assert_true "$(cmp -s "$ROLLBACK_SH" "$RB_GUARD" && echo true || echo false)" "a changed script must be replaced"
  assert_eq "755" "$(stat -c %a "$RB_GUARD")" "the mode must be fixed to 0755"
  assert_eq "" "$(find "${RB_DIR}/usr/local/libexec/scion" -name 'scion-hub-rollback.sh.*')" "no temporary file may be left"
  rm -rf "$RB_DIR"
}

test_install_guard_refuses_unsafe_dirs() {
  local case_name
  for case_name in group-writable symlinked-dir symlinked-script; do
    _rb_new_host
    RB_GUARD="${RB_DIR}/usr/local/libexec/scion/scion-hub-rollback.sh"
    mkdir -p "${RB_DIR}/usr/local/libexec" "${RB_DIR}/elsewhere"
    case "$case_name" in
      group-writable) mkdir -m 0775 "${RB_DIR}/usr/local/libexec/scion"; chmod 0775 "${RB_DIR}/usr/local/libexec/scion" ;;
      symlinked-dir) ln -s "${RB_DIR}/elsewhere" "${RB_DIR}/usr/local/libexec/scion" ;;
      symlinked-script)
        mkdir -m 0755 "${RB_DIR}/usr/local/libexec/scion"
        ln -s "${RB_DIR}/elsewhere/target" "$RB_GUARD" ;;
    esac
    _rb_guard install-guard "$ROLLBACK_SH"
    assert_true "$([[ "$RB_RC" -ne 0 ]] && echo true || echo false)" "${case_name}: install-guard must refuse (output: ${RB_OUT})"
    assert_eq "" "$(ls -A "${RB_DIR}/elsewhere")" "${case_name}: nothing may be written through the link"
    rm -rf "$RB_DIR"
  done
}

test_install_guard_requires_script() {
  _rb_new_host
  RB_GUARD="${RB_DIR}/usr/local/libexec/scion/scion-hub-rollback.sh"
  _rb_guard install-guard "${RB_DIR}/missing.sh"
  assert_true "$([[ "$RB_RC" -ne 0 ]] && echo true || echo false)" "a missing source must be refused"
  assert_false "$([[ -e "${RB_DIR}/usr/local/libexec" ]] && echo true)" "nothing may be created"
  rm -rf "$RB_DIR"
}

# =====================================================================
# Unit templates
# =====================================================================

# _rb_unit_section FILE SECTION -- the lines of [SECTION] in a unit file.
_rb_unit_section() {
  awk -v s="[$2]" '/^\[/ { on = ($0 == s); next } on' "$1"
}

test_rollback_hub_unit_has_guard_in_unit_section() {
  local unit="${TIER_DIR}/config-templates/scion-hub.service" section
  section="$(_rb_unit_section "$unit" Unit)"
  assert_contains "$section" $'\nStartLimitIntervalSec=300\n' "StartLimitIntervalSec=300 must be in [Unit] (systemd >= 230)"
  assert_contains "$section" $'\nStartLimitBurst=5\n' "StartLimitBurst=5 must be in [Unit]"
  assert_contains "$section" $'\nOnFailure=scion-hub-rollback.service' "OnFailure must name the rollback unit"
  assert_not_contains "$(_rb_unit_section "$unit" Service)" "StartLimit" "StartLimit* must not be in [Service] (the pre-230 spelling)"
  assert_contains "$(_rb_unit_section "$unit" Service)" "Restart=always" "the hub must still restart on exit"
}

test_rollback_unit_runs_root_owned_script() {
  local unit="${TIER_DIR}/config-templates/scion-hub-rollback.service" service
  service="$(_rb_unit_section "$unit" Service)"
  assert_contains "$service" "Type=oneshot" "the rollback unit must be a oneshot"
  assert_contains "$service" "ExecStart=/usr/local/libexec/scion/scion-hub-rollback.sh" "it must run the root-owned guard script"
  assert_not_contains "$service" "User=" "it must run as root"
  assert_not_contains "$(cat "$unit")" "/opt/scion/rollback.sh" "the script must not live inside the hub-owned install root"
  # shellcheck disable=SC2016 # a literal from the script source
  assert_contains "$(cat "$RB_LAYOUT_SH")" 'ROLLBACK_PATH="${SCION_ROLLBACK_PATH:-/usr/local/libexec/scion/scion-hub-rollback.sh}"' \
    "scion-layout.sh install-guard must install it at the path the unit runs"
}

test_rollback_unit_has_start_timeout() {
  assert_contains "$(_rb_unit_section "${TIER_DIR}/config-templates/scion-hub-rollback.service" Service)" "TimeoutStartSec=120" \
    "a hung guard run must time out instead of blocking later OnFailure= starts"
}

test_install_guard_unsafe_dir_message_names_guard_path() {
  _rb_new_host
  RB_GUARD="${RB_DIR}/usr/local/libexec/scion/scion-hub-rollback.sh"
  mkdir -p "${RB_DIR}/usr/local"
  chmod 0775 "${RB_DIR}/usr/local"
  _rb_guard install-guard "$ROLLBACK_SH"
  assert_true "$([[ "$RB_RC" -ne 0 ]] && echo true || echo false)" "a group-writable parent must be refused (output: ${RB_OUT})"
  assert_contains "$RB_OUT" "${RB_GUARD}" "the refusal must name the guard path"
  assert_not_contains "$RB_OUT" "Refusing to set up ${RB_ROOT}" "the refusal must not name the install root"
  chmod 0755 "${RB_DIR}/usr/local"
  rm -rf "$RB_DIR"
}

# =====================================================================
# Update state: the decision table
# =====================================================================

# _rb_update_state JSON -- writes RB_ROOT/update-state.json.
_rb_update_state() {
  printf '%s\n' "$1" > "${RB_ROOT}/update-state.json"
}

# _rb_happy_state [OVERRIDES...] -- the matching state for the default host
# (current nightly-20261023, previous nightly-20261020), with key=JSON
# overrides, e.g. verified=true or previous_safe=  (empty drops the key).
_rb_happy_state() {
  "$PYTHON" -c '
import json, sys
d = {"target": "nightly-20261023", "from": "nightly-20261020", "runId": "run-1",
     "verified": False, "migrated": False, "previous_safe": True}
for kv in sys.argv[1:]:
    k, v = kv.split("=", 1)
    if v == "":
        d.pop(k, None)
    else:
        d[k] = json.loads(v)
print(json.dumps(d, separators=(",", ":")))
' "$@"
}

# _rb_stale -- current was switched two hours ago.
_rb_stale() {
  touch -h -d '2 hours ago' "${RB_ROOT}/current"
}

# _rb_expect OUTCOME WHAT [LOG_SNIPPET] -- after _rb_run: OUTCOME swap (the
# rollback happened, the hub started) or stay (exit 0, nothing changed, no
# marker, hub not started).
_rb_expect() {
  local outcome="$1" what="$2" snippet="${3:-}"
  if [[ "$outcome" == swap ]]; then
    assert_eq "0" "$RB_RC" "${what}: must roll back (output: ${RB_OUT})"
    assert_eq "releases/nightly-20261020" "$(readlink "${RB_ROOT}/current")" "${what}: current must be the restored release"
    assert_true "$([[ -f "${RB_ROOT}/rollback.marker" ]] && echo true || echo false)" "${what}: the marker must be written"
    assert_contains "$(cat "$RB_CALLS")" "systemctl start scion-hub.service" "${what}: the hub must be started"
  else
    assert_eq "0" "$RB_RC" "${what}: leaving the hub stopped is not an error (output: ${RB_OUT})"
    assert_eq "$RB_BEFORE" "$(_rb_state)" "${what}: nothing may change, no marker"
    assert_not_contains "$(cat "$RB_CALLS")" "systemctl start" "${what}: the hub must stay stopped"
    assert_not_contains "$(cat "$RB_CALLS")" "systemctl reset-failed" "${what}: the failed state must stay visible"
  fi
  if [[ -n "$snippet" ]]; then
    assert_contains "$RB_OUT" "$snippet" "${what}: the log must say why"
  fi
}

# _rb_case OUTCOME WHAT STALE SNIPPET SETUP... -- one row: a fresh host,
# SETUP evaluated (writes the state file), current made stale if STALE is
# "stale", then the guard run and checked.
_rb_case() {
  local outcome="$1" what="$2" stale="$3" snippet="$4"
  shift 4
  _rb_new_host
  eval "$*"
  [[ "$stale" == stale ]] && _rb_stale
  RB_BEFORE="$(_rb_state)"
  _rb_run
  _rb_expect "$outcome" "$what" "$snippet"
  rm -rf "$RB_DIR"
}

# shellcheck disable=SC2016 # SETUP snippets are evaluated by _rb_case
test_rollback_update_state_decision_table() {
  # Readable and matching: the booleans decide, whatever the switch time.
  _rb_case swap "happy path (fresh)" fresh "safe to undo" '_rb_update_state "$(_rb_happy_state)"'
  # Valid, matching and unverified also needs a recent switch (a release
  # that never sets verified must not be rolled back days later).
  _rb_case stay "valid, unverified, stale current" stale "too old to roll back automatically" '_rb_update_state "$(_rb_happy_state)"'
  _rb_case stay "valid, unverified, migrated and safe, stale current" stale "too old to roll back automatically" '_rb_update_state "$(_rb_happy_state migrated=true)"'
  _rb_case stay "verified" fresh "was verified" '_rb_update_state "$(_rb_happy_state verified=true)"'
  _rb_case stay "migrated, previous not safe" fresh "below its downgrade floor" '_rb_update_state "$(_rb_happy_state migrated=true previous_safe=false)"'
  _rb_case stay "migrated, previous_safe missing" fresh "Restoring after a failed update" '_rb_update_state "$(_rb_happy_state migrated=true previous_safe=)"'
  _rb_case stay "migrated, previous_safe not a boolean" fresh "below its downgrade floor" '_rb_update_state "$(_rb_happy_state migrated=true previous_safe="\"true\"")"'
  _rb_case swap "migrated, previous safe" fresh "safe to undo" '_rb_update_state "$(_rb_happy_state migrated=true)"'
  _rb_case swap "not migrated, previous not safe" fresh "safe to undo" '_rb_update_state "$(_rb_happy_state previous_safe=false)"'
  # Readable but about other links: fail closed, never the timer.
  _rb_case stay "mismatched target" fresh "describes another update" '_rb_update_state "$(_rb_happy_state target="\"nightly-20261025\"")"'
  _rb_case stay "mismatched from" fresh "describes another update" '_rb_update_state "$(_rb_happy_state from="\"nightly-20261001\"")"'
  # Missing or unreadable: the switch-time fallback.
  _rb_case swap "missing, fresh current" fresh "is missing; current was switched" ':'
  _rb_case stay "missing, stale current" stale "is missing, and current" ':'
  _rb_case swap "invalid JSON, fresh" fresh "is unreadable; current was switched" '_rb_update_state "{not json"'
  _rb_case stay "invalid JSON, stale" stale "is unreadable, and current" '_rb_update_state "{not json"'
  _rb_case stay "wrong types (verified a string), stale" stale "is unreadable" '_rb_update_state "$(_rb_happy_state verified="\"false\"")"'
  _rb_case stay "from missing, stale" stale "is unreadable" '_rb_update_state "$(_rb_happy_state from=)"'
  _rb_case stay "JSON array, stale" stale "is unreadable" '_rb_update_state "[]"'
  _rb_case stay "a directory, stale" stale "is unreadable" 'mkdir "${RB_ROOT}/update-state.json"'
  _rb_case swap "oversized, fresh" fresh "is unreadable; current was switched" '_rb_update_state "$(_rb_happy_state runId="\"$(printf "x%.0s" {1..5000})\"")"'
  _rb_case stay "oversized happy state, stale (cap enforced, not parsed)" stale "is unreadable" '_rb_update_state "$(_rb_happy_state runId="\"$(printf "x%.0s" {1..5000})\"")"'
  # A symlinked file is unreadable (O_NOFOLLOW): a happy target behind it,
  # with a stale current, must not swap.
  _rb_case stay "symlink to a happy state, stale (not followed)" stale "is unreadable" \
    '_rb_happy_state > "${RB_DIR}/happy.json"; ln -s "${RB_DIR}/happy.json" "${RB_ROOT}/update-state.json"'
  _rb_case swap "symlink, fresh (fallback)" fresh "is unreadable; current was switched" \
    '_rb_happy_state > "${RB_DIR}/happy.json"; ln -s "${RB_DIR}/happy.json" "${RB_ROOT}/update-state.json"'
}

# The same window applies to a valid, matching, unverified state.
test_rollback_update_state_window() {
  _rb_new_host
  _rb_update_state "$(_rb_happy_state)"
  touch -h -d '1700 seconds ago' "${RB_ROOT}/current"
  _rb_run
  _rb_expect swap "valid, unverified, switched 1700 s ago" "switched 1700s ago, safe to undo"
  rm -rf "$RB_DIR"
  _rb_new_host
  _rb_update_state "$(_rb_happy_state)"
  touch -h -d '1900 seconds ago' "${RB_ROOT}/current"
  RB_BEFORE="$(_rb_state)"
  _rb_run
  _rb_expect stay "valid, unverified, switched 1900 s ago" "too old to roll back automatically"
  rm -rf "$RB_DIR"
}

# The window edge of the fallback: 1700 s ago swaps, 1900 s ago does not.
test_rollback_mtime_fallback_window() {
  _rb_new_host
  touch -h -d '1700 seconds ago' "${RB_ROOT}/current"
  _rb_run
  _rb_expect swap "switched 1700 s ago"
  rm -rf "$RB_DIR"
  _rb_new_host
  touch -h -d '1900 seconds ago' "${RB_ROOT}/current"
  RB_BEFORE="$(_rb_state)"
  _rb_run
  _rb_expect stay "switched 1900 s ago" "more than 1800s"
  rm -rf "$RB_DIR"
}

# Without python3 the file counts as unreadable: the fallback applies.
test_rollback_update_state_without_python_falls_back() {
  _rb_new_host
  _rb_update_state "$(_rb_happy_state)"
  _rb_stale
  printf '#!/bin/sh\nexit 127\n' > "${RB_SHIM}/python3"
  chmod 0755 "${RB_SHIM}/python3"
  RB_BEFORE="$(_rb_state)"
  _rb_run
  _rb_expect stay "no python3, stale current" "is unreadable, and current"
  rm -rf "$RB_DIR"
}

# The root path still never opens update-state.json: the decision runs in
# the owner step (the install root is mode 000 until runuser).
test_rollback_root_path_update_state_read_by_owner() {
  _rb_new_host
  _rb_root_shims
  _rb_update_state "$(_rb_happy_state verified=true)"
  RB_BEFORE="$(_rb_state)"
  _rb_run_as_root
  _rb_expect stay "root path, verified" "was verified"
  assert_contains "$(cat "$RB_CALLS")" "runuser -u scion" "the decision must be made in the owner step"
  rm -rf "$RB_DIR"
}
