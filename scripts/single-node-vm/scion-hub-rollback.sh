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
#   2. swaps current <-> previous, each with an atomic symlink rename;
#   3. writes <root>/rollback.marker, one line of JSON:
#        {"at":"<RFC3339 UTC>","from":"<failed tag>","to":"<restored tag>"}
#   4. runs `systemctl reset-failed scion-hub && systemctl start scion-hub`.
#
# It never swaps back. If the restored release crash-loops too, the start
# limit stops the hub again and this script runs again: it finds the
# marker already records exactly this rollback (from = previous,
# to = current) and does nothing, leaving the hub stopped for an operator.
#
# It acts only when the hub crash-looped:
#   - scion-hub's Result must be start-limit-hit. systemd >= 254 also
#     triggers OnFailure= on every single crash before an automatic
#     restart; those runs see Result=exit-code (or similar) and do nothing.
#   - The hub's last exit must be a failure. The start limit also counts
#     clean restarts (an update or Restart Hub exits on SIGTERM and systemd
#     starts it again), so five of those within five minutes trip it too.
#     Then the script only restarts the hub, without swapping.
#
# Privileges: root runs only systemctl. The install root and everything
# under it belong to the install owner (scion), who can change any entry
# there at any time, so -- as in scion-layout.sh -- root never writes,
# renames or follows links inside it: the checks, the swap and the marker
# run as the owner (runuser). The marker is therefore owned by the owner.
# Nothing outside the install root is changed.
#
# Safe to run more than once: after a successful rollback the hub's Result
# is no longer start-limit-hit, and a rollback already recorded in the
# marker is never repeated.
#
# Environment (defaults are what the VM uses; tests override them):
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
# renamed over the marker.
write_marker() {
  local tmp
  tmp="$(mktemp "$INSTALL_ROOT/rollback.marker.XXXXXX")"
  printf '{"at":"%s","from":"%s","to":"%s"}\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" "$2" > "$tmp"
  chmod 0644 "$tmp"
  mv -fT "$tmp" "$INSTALL_ROOT/rollback.marker"
}

cmd_swap() {
  [[ "$(id -u)" != "0" || -z "$OWNER" ]] || die "__swap is an internal step and must not run as root with an owner configured"
  local cur prev
  prev="$(release_tag previous)" || die "refusing to roll back: previous is not a release inside $INSTALL_ROOT/releases/; nothing changed"
  [[ -f "$INSTALL_ROOT/releases/$prev/scion" ]] || die "refusing to roll back: $INSTALL_ROOT/releases/$prev/scion is missing; nothing changed"
  cur="$(release_tag current)" || die "refusing to roll back: current is not a release inside $INSTALL_ROOT/releases/; nothing changed"
  [[ "$cur" != "$prev" ]] || die "refusing to roll back: current and previous are both $cur; nothing changed"
  if [[ "$(marker_field from)" == "$prev" && "$(marker_field to)" == "$cur" ]]; then
    log "already rolled back from $prev to $cur (rollback.marker); not swapping back. The restored release failed too: fix it by hand (docs: Restore after a failed update)."
    exit "$ALREADY_ROLLED_BACK"
  fi
  swap_link current "releases/$prev"
  swap_link previous "releases/$cur"
  write_marker "$cur" "$prev"
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

# hub_show PROPERTY -- one property of scion-hub.service.
hub_show() {
  systemctl show -p "$1" --value "$HUB_UNIT"
}

# last_exit_was_clean -- the hub's last main process ended the way systemd
# counts as clean: exit status 0, or killed by SIGHUP, SIGINT, SIGTERM or
# SIGPIPE. ExecMainCode is the waitid si_code: 1 exited, 2 killed, 3 dumped.
last_exit_was_clean() {
  local code status
  code="$(hub_show ExecMainCode)"
  status="$(hub_show ExecMainStatus)"
  case "$code:$status" in
    1:0|2:1|2:2|2:13|2:15) return 0 ;;
    *) return 1 ;;
  esac
}

restart_hub() {
  systemctl reset-failed "$HUB_UNIT"
  systemctl start "$HUB_UNIT"
}

cmd_rollback() {
  local result rc=0
  result="$(hub_show Result)"
  if [[ "$result" != "start-limit-hit" ]]; then
    log "$HUB_UNIT result is '$result', not start-limit-hit; nothing to do"
    return 0
  fi
  if last_exit_was_clean; then
    log "$HUB_UNIT hit its start limit after clean exits (repeated restarts, not a crash loop); restarting it without a rollback"
    restart_hub
    return 0
  fi
  run_as_owner || rc=$?
  case "$rc" in
    0) ;;
    "$ALREADY_ROLLED_BACK") return 0 ;;
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
