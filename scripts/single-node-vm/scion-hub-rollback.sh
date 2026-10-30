#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# scripts/single-node-vm/scion-hub-rollback.sh -- the boot-failure guard
# (design D1, ptone/scion#6009). deploy.sh installs it root-owned (0755) at
# /usr/local/libexec/scion/scion-hub-rollback.sh, outside the hub-owned
# install root, so the hub user can neither edit nor replace it. The root
# oneshot scion-hub-rollback.service runs it, and scion-hub.service names
# that unit in OnFailure=.
#
# When scion-hub.service has hit its start limit (StartLimitBurst=5 within
# StartLimitIntervalSec=300) after crashing, the script:
#   1. refuses unless <root>/previous resolves (realpath) to a release
#      directly inside <root>/releases/ that holds a scion binary;
#   2. acts only on a failed update that is safe to undo, decided from
#      <root>/update-state.json (see "Update state" below) or, when that
#      file is missing or unreadable, from how recently <root>/current was
#      switched. A release that ran fine and then crash-loops (a settings
#      typo, a full disk, a port clash) is not rolled back, and neither is
#      one whose database migrations the previous release cannot run
#      with: the hub stays stopped for an operator;
#   3. writes <root>/rollback.marker, one line of JSON:
#        {"at":"<RFC3339 UTC>","from":"<failed tag>","to":"<restored tag>"}
#      before changing any link, so a marker that cannot be written means
#      no swap;
#   4. swaps current <-> previous: previous first, then current, each with
#      an atomic symlink rename. The pair is not atomic. Dying between the
#      two leaves both pointing at the failed release, which a later run
#      refuses ("current and previous are both ..."): it fails closed;
#   5. runs `systemctl reset-failed scion-hub && systemctl start scion-hub`.
#
# It swaps binaries only. The database is not restored: after a rollback
# across a schema migration, follow "Restoring after a failed update" in
# docs/deploy/single-node-vm.md.
#
# It never swaps back. If the restored release crash-loops too, the start
# limit stops the hub again and this script runs again: it finds the
# marker already records exactly this rollback (from = previous,
# to = current) and does nothing, leaving the hub stopped for an operator.
#
# Update state. The hub writes <root>/update-state.json (owner-written,
# atomic, 0644), one JSON object:
#   {"target":"<tag>","from":"<tag>","runId":"...","verified":false,
#    "migrated":false,"previous_safe":true}
# The hub being replaced writes it before its update restart, and
# scion-layout.sh install (deploy.sh) writes it with runId "deploy.sh"
# when it switches current; the new binary sets migrated=true before its first migration statement and
# previous_safe (whether `from` is at or above its DowngradeFloor,
# computed in Go), and verified=true once the update is confirmed. This
# script compares no versions; it decides on the booleans only:
#   - target is the release current points at and from the one previous
#     points at:
#       - verified is true: no swap (the update succeeded; this crash
#         loop has another cause);
#       - migrated is true and previous_safe is not true (false, missing
#         or not a boolean): no swap (the previous release may not run on
#         the migrated database; restore the snapshot);
#       - current was switched more than RECENT_SWITCH_WINDOW_SEC ago: no
#         swap. A release that never marks its update verified (any
#         release older than the update-state contract, e.g. after a
#         downgrade) would otherwise leave verified=false forever, and a
#         crash loop days later would roll it "back" to the newer release;
#       - otherwise: swap;
#   - target or from does not match: no swap. The links were changed by
#     something other than the update the file describes;
#   - file missing or unreadable (not a regular file, a symlink, over
#     4 KiB, not a JSON object with string target and from and boolean
#     verified and migrated, or no python3): fall back to the mtime gate --
#     swap only if current was switched within RECENT_SWITCH_WINDOW_SEC
#     (the symlink's own mtime; every switch creates a new link).
#
# It acts only when the hub crash-looped:
#   - scion-hub must be failed for good: ActiveState=failed AND
#     SubState=failed (systemd will not restart it), in each of
#     SETTLE_READS reads SETTLE_INTERVAL_SEC apart. systemd >= 254 fires
#     OnFailure= on every failed start while a restart is still pending: on
#     255.4 (Ubuntu 24.04) those runs see activating/auto-restart; the
#     source also has a transient failed/failed-before-auto-restart state,
#     which was not observed there. Requiring SubState=failed in every read
#     keeps all of those runs a no-op.
#   - The unit's Result is NOT a usable signal: on systemd 249 (Ubuntu
#     22.04) a crash loop that hits the start limit logs "Start request
#     repeated too quickly" but keeps Result=exit-code (or signal,
#     core-dump, ...); Result=start-limit-hit appears only when the
#     previous result was success, i.e. after clean restarts.
#   - failed/failed is not specific to the start limit: a failed operator
#     stop ends there too and fires OnFailure= -- `systemctl stop` that
#     hits TimeoutStopSec (the main process, or a child left in the cgroup,
#     is SIGKILLed: Result=timeout, with any main exit), or a main process
#     that exits non-zero on SIGTERM (Result=exit-code). systemd's NRestarts
#     cannot tell these apart (it counts every automatic restart since the
#     last manual start, and every update or Restart Hub self-exit is one),
#     so it is not used.
#   - The discriminator is <root>/start-attempts, kept by
#     scion-hub-start-attempt.sh (ExecStartPre=- of scion-hub.service):
#     {"target":"<release>","count":N}, the number of starts since current
#     was switched to that release. A crash is rolled back only if the file
#     is readable (a regular file, not a symlink, exactly that format),
#     target is the release current points at, and count >=
#     MIN_START_ATTEMPTS (2): this release failed at least one restart
#     since its switch. Not StartLimitBurst: systemd's start limit counts
#     every start in its window, the previous release's included, while the
#     counter resets on a switch, so after a Restart Hub or another update
#     shortly before, a real crash loop can end well below the burst. A
#     missing, unreadable or mismatched file means no swap. The check runs
#     in the owner step (root never reads the install root), before the
#     update-state decision table, so it applies to the switch-time
#     fallback too.
#   - The count does not tell a crash loop from a failed operator stop (a
#     release restarted once from the UI has count 2). The protection
#     against swapping a hub the operator stopped is: Result=timeout is
#     excluded, the update must be unverified (verified is set as soon as
#     the hub's listener binds, so a release that ran is verified), and the
#     switch must be recent (RECENT_SWITCH_WINDOW_SEC). Residuals, each
#     fail closed or accepted:
#     (a) if the previous release used 4+ of the window's starts, a
#         crash-looping new release can end at count 1 and the hub stays
#         stopped (restore by hand);
#     (b) a release that predates the update-state contract never sets
#         verified, so within the window, after 2+ starts, a failed
#         non-zero stop of it is rolled back;
#     (c) on the switch-time fallback (update-state.json missing or
#         unreadable, including no python3) verified is not consulted, so
#         within the window, after 2+ starts, a failed non-zero stop of a
#         release that ran fine is rolled back too;
#     (d) a failed non-zero stop that lands before the hub's listener has
#         bound (during startup, e.g. migrations; a hub that exits non-zero
#         when a requested shutdown interrupts its startup) of a release
#         that has started at least twice since its switch, within the
#         window, with previous_safe true, is rolled back: verified is not
#         set yet. The first boot after a switch (count 1) is safe. A hub
#         that exits 0 when a requested shutdown interrupts startup ends
#         inactive, not failed, so OnFailure never fires (the Go change on
#         scion/hu-p3-n1-go does this for P3+ releases).
#   - Result=timeout (a stop that timed out) is never acted on.
#   - A clean last exit (an update or Restart Hub exits on SIGTERM and
#     systemd starts it again; or manual restarts) only restarts the hub,
#     without swapping, and only with Result=start-limit-hit: five clean
#     restarts within five minutes stop the unit with exactly that. A
#     failed stop with a clean main exit (a child SIGKILLed at
#     TimeoutStopSec) has Result=timeout and is left alone.
#
# Privileges: root runs only systemctl. The install root and everything
# under it belong to the install owner (scion), who can change any entry
# there at any time, so -- as in scion-layout.sh -- root never writes,
# renames or follows links inside it: the checks, the swap and the marker
# run as the owner (runuser). The marker is therefore owned by the owner.
# Nothing outside the install root is changed.
#
# Safe to run more than once: after a successful rollback the hub is
# running again (not failed), and a rollback already recorded in the
# marker is never repeated.
#
# Environment (defaults are what the VM uses; tests override them):
#   SCION_ROLLBACK_SETTLE_SEC  seconds between the settle reads (default 1)
#   SCION_INSTALL_ROOT    install root            (default /opt/scion)
#   SCION_INSTALL_OWNER   user:group owning the root (default scion:scion);
#                         empty runs the owner step in-process (tests)

set -euo pipefail

INSTALL_ROOT="${SCION_INSTALL_ROOT:-/opt/scion}"
OWNER="${SCION_INSTALL_OWNER-scion:scion}"
readonly HUB_UNIT="scion-hub.service"

# Exit status of the owner step when the marker already records this
# rollback: nothing changed and the hub must not be started.
readonly ALREADY_ROLLED_BACK=3
# Exit status of the owner step when the crash loop is not a failed
# update that is safe to undo: nothing changed and the hub must stay
# stopped.
readonly LEAVE_STOPPED=4

# The largest update-state.json the owner step reads (bytes).
readonly UPDATE_STATE_MAX_BYTES=4096

# How recently current must have been switched for a crash loop to count
# as a failed update (seconds). Covers the update itself, the restart and
# the five failed starts (about 30 s with RestartSec=5) with a wide margin
# for a release that fails a little later, without reaching back to a
# release that has been running normally.
readonly RECENT_SWITCH_WINDOW_SEC=1800

# The failed state must hold over SETTLE_READS reads of the unit,
# SETTLE_INTERVAL_SEC apart (about 2 s in total), so a transient failed
# between automatic restart attempts is never taken for the final one.
readonly SETTLE_READS=3
SETTLE_INTERVAL_SEC="${SCION_ROLLBACK_SETTLE_SEC:-1}"
[[ "$SETTLE_INTERVAL_SEC" =~ ^[0-9]+$ ]] || SETTLE_INTERVAL_SEC=1
readonly SETTLE_INTERVAL_SEC

# The fewest starts of the current release (start-attempts) that count as
# a crash loop: the first start plus at least one failed restart.
readonly MIN_START_ATTEMPTS=2

log() { echo "scion-hub-rollback: $*"; }
die() { echo "scion-hub-rollback: ERROR: $*" >&2; exit 1; }

# canonical_abs PATH -- an absolute path without empty, "." or ".."
# components and without a trailing / (same rule as scion-layout.sh).
canonical_abs() {
  local p="$1"
  [[ "$p" == /?* && "$p" != */ && "$p" != *//* ]] || return 1
  [[ "/$p/" != */./* && "/$p/" != */../* ]]
}

canonical_abs "$INSTALL_ROOT" || die "SCION_INSTALL_ROOT must be an absolute path without '.', '..', '//' or a trailing '/' (got '$INSTALL_ROOT')"

# valid_tag TAG -- a release directory name (same rule as scion-layout.sh).
valid_tag() {
  [[ "$1" =~ ^[0-9A-Za-z][0-9A-Za-z._-]*$ ]] && [[ "$1" != *.partial ]] && [[ "$1" != *.old ]]
}

# ---------------------------------------------------------------------------
# Owner side (__swap): everything that reads or changes the install root.
# ---------------------------------------------------------------------------

# release_tag NAME -- the release <root>/NAME resolves to: prints its tag
# when the fully resolved path is a directory directly inside the resolved
# <root>/releases, named like a release. Fails otherwise -- a link that
# leaves releases/ through "..", an absolute target or a chain of links is
# refused the same way.
release_tag() {
  local name="$1" releases resolved tag
  [[ -L "$INSTALL_ROOT/$name" ]] || { echo "$INSTALL_ROOT/$name is not a symlink" >&2; return 1; }
  releases="$(realpath -e -- "$INSTALL_ROOT/releases")" || { echo "$INSTALL_ROOT/releases does not exist" >&2; return 1; }
  resolved="$(realpath -e -- "$INSTALL_ROOT/$name")" || { echo "$INSTALL_ROOT/$name does not resolve" >&2; return 1; }
  tag="${resolved##*/}"
  if [[ "${resolved%/*}" != "$releases" ]] || ! valid_tag "$tag" || [[ ! -d "$resolved" ]]; then
    echo "$INSTALL_ROOT/$name resolves to $resolved, which is not a release directory inside $releases" >&2
    return 1
  fi
  printf '%s' "$tag"
}

# marker_field KEY -- KEY's string value in <root>/rollback.marker (empty
# when the file or the key is missing).
marker_field() {
  local m="$INSTALL_ROOT/rollback.marker"
  [[ -f "$m" && ! -L "$m" ]] || return 0
  grep -o "\"$1\":\"[^\"]*\"" "$m" | head -1 | sed 's/^"[^"]*":"\(.*\)"$/\1/' || true
}

# swap_link NAME TARGET -- point <root>/NAME at TARGET (relative) with an
# atomic rename.
swap_link() {
  local tmp="$INSTALL_ROOT/$1.new"
  rm -f "$tmp"
  ln -s "$2" "$tmp"
  mv -T "$tmp" "$INSTALL_ROOT/$1"
}

# write_marker FROM TO -- write <root>/rollback.marker atomically: a fresh,
# randomly named file (never an existing name that could be a symlink),
# renamed over the marker. Fails (removing its temp file) without changing
# the marker if any step fails.
write_marker() {
  local tmp
  tmp="$(mktemp "$INSTALL_ROOT/rollback.marker.XXXXXX")" || return 1
  if printf '{"at":"%s","from":"%s","to":"%s"}\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" "$2" > "$tmp" \
     && chmod 0644 "$tmp" && mv -fT "$tmp" "$INSTALL_ROOT/rollback.marker"; then
    return 0
  fi
  rm -f "$tmp"
  return 1
}

# UPDATE_STATE_PY -- reads update-state.json without following a symlink,
# at most UPDATE_STATE_MAX_BYTES, and prints one word: missing,
# unreadable, mismatch, verified, unsafe or swap (see "Update state"),
# then, for a readable file, its from and runId for the log.
# shellcheck disable=SC2016 # Python source, not shell
UPDATE_STATE_PY='
import json, os, stat, sys
path, current, previous, cap = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
def clean(v):
    # One printable word for the log line.
    return "".join(c if c.isprintable() and not c.isspace() else "_" for c in str(v))[:80] or "?"
def out(word, d=None):
    d = d if isinstance(d, dict) else {}
    print(word, clean(d.get("from", "?")), clean(d.get("runId", "?")))
    sys.exit(0)
try:
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
except FileNotFoundError:
    out("missing")
except OSError:
    out("unreadable")
st = os.fstat(fd)
if not stat.S_ISREG(st.st_mode) or st.st_size > cap:
    out("unreadable")
data = os.read(fd, cap + 1)
if len(data) > cap:
    out("unreadable")
try:
    d = json.loads(data.decode("utf-8"))
except ValueError:
    out("unreadable")
if not isinstance(d, dict) or not all(isinstance(d.get(k), str) for k in ("target", "from")) \
        or not all(isinstance(d.get(k), bool) for k in ("verified", "migrated")):
    out("unreadable")
if d["target"] != current or d["from"] != previous:
    out("mismatch", d)
if d["verified"]:
    out("verified", d)
if d["migrated"] and d.get("previous_safe") is not True:
    out("unsafe", d)
out("swap", d)
'

# update_state_decision CURRENT_TAG PREVIOUS_TAG -- the decision word (plus
# from and runId); "unreadable" when python3 is missing or fails.
update_state_decision() {
  python3 -I -c "$UPDATE_STATE_PY" "$INSTALL_ROOT/update-state.json" "$1" "$2" "$UPDATE_STATE_MAX_BYTES" </dev/null 2>/dev/null \
    || echo "unreadable ? ?"
}

# start_attempts -- "<target> <count>" from <root>/start-attempts, or
# "- -" when it is missing, a symlink, not a regular file, too large, or not
# exactly the format scion-hub-start-attempt.sh writes.
start_attempts() {
  local f="$INSTALL_ROOT/start-attempts" line size
  if [[ -f "$f" && ! -L "$f" ]] && size="$(stat -c %s -- "$f" 2>/dev/null)" && (( size <= UPDATE_STATE_MAX_BYTES )) \
     && line="$(head -c "$UPDATE_STATE_MAX_BYTES" -- "$f" 2>/dev/null </dev/null)" \
     && [[ "$line" =~ ^\{\"target\":\"([0-9A-Za-z._-]+)\",\"count\":([0-9]{1,9})\}$ ]]; then
    echo "${BASH_REMATCH[1]} $((10#${BASH_REMATCH[2]}))"
  else
    echo "- -"
  fi
}

# switched_seconds_ago -- seconds since <root>/current was last switched:
# the mtime of the symlink itself (stat without -L), which each switch
# sets by creating a new link.
switched_seconds_ago() {
  local mtime
  mtime="$(stat -c %Y -- "$INSTALL_ROOT/current")" || return 1
  echo $(( $(date +%s) - mtime ))
}

cmd_swap() {
  [[ "$(id -u)" != "0" || -z "$OWNER" ]] || die "__swap is an internal step and must not run as root with an owner configured"
  local cur prev
  prev="$(release_tag previous)" || die "refusing to roll back: previous is not a release inside $INSTALL_ROOT/releases/; nothing changed"
  [[ -f "$INSTALL_ROOT/releases/$prev/scion" ]] || die "refusing to roll back: $INSTALL_ROOT/releases/$prev/scion is missing; nothing changed"
  cur="$(release_tag current)" || die "refusing to roll back: current is not a release inside $INSTALL_ROOT/releases/; nothing changed"
  [[ "$cur" != "$prev" ]] || die "refusing to roll back: current and previous are both $cur; nothing changed"
  if [[ "$(marker_field from)" == "$prev" && "$(marker_field to)" == "$cur" ]]; then
    log "rollback.marker records the rollback from $prev to $cur; not swapping back. Nothing changed; the hub stays stopped. Check journalctl -u scion-hub, fix the cause (docs: Restoring after a failed update), then run: systemctl reset-failed scion-hub && systemctl start scion-hub"
    exit "$ALREADY_ROLLED_BACK"
  fi
  # The start attempts since current was switched (scion-hub-start-attempt.sh)
  # must show at least one failed restart of this release. Applies to the
  # switch-time fallback too.
  local att_target att_count
  read -r att_target att_count <<< "$(start_attempts)"
  if [[ "$att_target" != "$cur" || ! "$att_count" =~ ^[0-9]+$ ]] || (( att_count < MIN_START_ATTEMPTS )); then
    log "start-attempts shows ${att_count:-?} start(s) of '${att_target:-?}', not >= $MIN_START_ATTEMPTS of releases/$cur: no failed restart of the current release on record: not rolling back. Nothing changed; the hub stays stopped. If the previous release used most of the start limit just before, follow docs/deploy/single-node-vm.md, Restoring after a failed update."
    exit "$LEAVE_STOPPED"
  fi
  local decision state_from run_id age
  local stay="Nothing changed; the hub stays stopped. Check journalctl -u scion-hub, fix the cause, then run: systemctl reset-failed scion-hub && systemctl start scion-hub"
  read -r decision state_from run_id <<< "$(update_state_decision "$cur" "$prev")"
  case "$decision" in
    swap)
      age="$(switched_seconds_ago)" || die "cannot read when $INSTALL_ROOT/current was switched; nothing changed"
      if (( age > RECENT_SWITCH_WINDOW_SEC )); then
        log "update-state.json: the update $state_from -> $cur (run $run_id) is not verified, but current was switched ${age}s ago, more than ${RECENT_SWITCH_WINDOW_SEC}s: update not verified but too old to roll back automatically. Nothing changed; the hub stays stopped. See docs/deploy/single-node-vm.md, Restoring after a failed update."
        exit "$LEAVE_STOPPED"
      fi
      log "update-state.json: failed update $state_from -> $cur (run $run_id), switched ${age}s ago, safe to undo" ;;
    mismatch)
      log "update-state.json describes another update (run $run_id, from $state_from) than current releases/$cur with previous releases/$prev; the links were changed by hand or by another tool: not rolling back. Nothing changed; the hub stays stopped. See docs/deploy/single-node-vm.md, Restoring after a failed update."
      exit "$LEAVE_STOPPED" ;;
    verified)
      log "update-state.json: the update to $cur (run $run_id) was verified, so this crash loop is not a failed update: not rolling back. $stay"
      exit "$LEAVE_STOPPED" ;;
    unsafe)
      log "update-state.json: $cur (run $run_id) migrated the database, and $prev is below its downgrade floor: not rolling back. Nothing changed; the hub stays stopped. Restore the database snapshot with the release it was taken from (docs/deploy/single-node-vm.md, Restoring after a failed update)."
      exit "$LEAVE_STOPPED" ;;
    *)
      age="$(switched_seconds_ago)" || die "cannot read when $INSTALL_ROOT/current was switched; nothing changed"
      if (( age > RECENT_SWITCH_WINDOW_SEC )); then
        log "update-state.json is $decision, and current (releases/$cur) was switched ${age}s ago, more than ${RECENT_SWITCH_WINDOW_SEC}s: this crash loop is not a failed update, so it is not rolled back. $stay"
        exit "$LEAVE_STOPPED"
      fi
      log "update-state.json is $decision; current was switched ${age}s ago (within ${RECENT_SWITCH_WINDOW_SEC}s): treating this as a failed update" ;;
  esac
  write_marker "$cur" "$prev" || die "could not write $INSTALL_ROOT/rollback.marker (disk full?); refusing to roll back without it. Nothing changed"
  swap_link previous "releases/$cur"
  swap_link current "releases/$prev"
  log "current -> releases/$prev (previous -> releases/$cur); wrote rollback.marker"
}

# ---------------------------------------------------------------------------
# Root side.
# ---------------------------------------------------------------------------

# run_as_owner -- run the __swap step as the owner: re-run this script from
# stdin through runuser, or in-process when no owner is configured (tests).
run_as_owner() {
  if [[ "$(id -u)" == "0" && -n "$OWNER" ]]; then
    runuser -u "${OWNER%%:*}" -- env SCION_INSTALL_ROOT="$INSTALL_ROOT" SCION_INSTALL_OWNER="" \
      bash -s -- __swap < "${BASH_SOURCE[0]}"
  else
    (cmd_swap)
  fi
}

# read_hub_state -- one `systemctl show` of scion-hub.service (a consistent
# snapshot), into HUB_ACTIVE, HUB_SUB, HUB_RESULT, HUB_EXEC_CODE,
# HUB_EXEC_STATUS and HUB_BURST (StartLimitBurst).
read_hub_state() {
  local line key value
  HUB_ACTIVE="" HUB_SUB="" HUB_RESULT="" HUB_EXEC_CODE="" HUB_EXEC_STATUS="" HUB_BURST=""
  while IFS= read -r line; do
    key="${line%%=*}"
    value="${line#*=}"
    case "$key" in
      ActiveState) HUB_ACTIVE="$value" ;;
      SubState) HUB_SUB="$value" ;;
      Result) HUB_RESULT="$value" ;;
      ExecMainCode) HUB_EXEC_CODE="$value" ;;
      ExecMainStatus) HUB_EXEC_STATUS="$value" ;;
      StartLimitBurst) HUB_BURST="$value" ;;
    esac
  done < <(systemctl show -p ActiveState,SubState,Result,ExecMainCode,ExecMainStatus,StartLimitBurst "$HUB_UNIT")
}

# last_exit_was_clean -- the hub's last main process ended the way systemd
# counts as clean: exit status 0, or killed by SIGHUP, SIGINT, SIGTERM or
# SIGPIPE. ExecMainCode is the waitid si_code: 1 exited, 2 killed, 3 dumped.
last_exit_was_clean() {
  case "$HUB_EXEC_CODE:$HUB_EXEC_STATUS" in
    1:0|2:1|2:2|2:13|2:15) return 0 ;;
    *) return 1 ;;
  esac
}

restart_hub() {
  systemctl reset-failed "$HUB_UNIT"
  systemctl start "$HUB_UNIT"
}

# failed_for_good -- read the unit SETTLE_READS times, SETTLE_INTERVAL_SEC
# apart; true only if every read shows ActiveState=failed and
# SubState=failed. Leaves the last read in HUB_*.
failed_for_good() {
  local i
  for (( i = 1; i <= SETTLE_READS; i++ )); do
    (( i == 1 )) || sleep "$SETTLE_INTERVAL_SEC"
    read_hub_state
    if [[ "$HUB_ACTIVE" != "failed" || "$HUB_SUB" != "failed" ]]; then
      log "$HUB_UNIT is $HUB_ACTIVE/$HUB_SUB (result $HUB_RESULT) at read $i of $SETTLE_READS, not failed for good; nothing to do"
      return 1
    fi
  done
}

cmd_rollback() {
  local rc=0
  failed_for_good || return 0
  local facts="result $HUB_RESULT, last exit $HUB_EXEC_CODE:$HUB_EXEC_STATUS, StartLimitBurst '$HUB_BURST'"
  if last_exit_was_clean; then
    if [[ "$HUB_RESULT" != "start-limit-hit" ]]; then
      log "$HUB_UNIT failed ($facts) after a clean exit but not at the start limit (e.g. a stop that timed out); nothing to do"
      return 0
    fi
    log "$HUB_UNIT hit its start limit after clean exits ($facts; repeated restarts, not a crash loop); restarting it without a rollback"
    restart_hub
    return 0
  fi
  if [[ "$HUB_RESULT" == "timeout" ]]; then
    log "$HUB_UNIT failed ($facts): a timeout (e.g. a stop that timed out), not a crash loop; nothing to do"
    return 0
  fi
  log "$HUB_UNIT failed after a crash ($facts); checking the start attempts and update state"
  run_as_owner || rc=$?
  case "$rc" in
    0) ;;
    "$ALREADY_ROLLED_BACK"|"$LEAVE_STOPPED") return 0 ;;
    *) exit "$rc" ;;
  esac
  restart_hub
  log "restarted $HUB_UNIT"
}

main() {
  case "${1:-}" in
    "") cmd_rollback ;;
    __swap) cmd_swap ;;
    *) die "usage: scion-hub-rollback.sh (no arguments; run by scion-hub-rollback.service)" ;;
  esac
}

main "$@"
