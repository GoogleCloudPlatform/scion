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

# scripts/single-node-vm/scion-hub-start-attempt.sh -- the start-attempts
# counter for the boot-failure guard (design D1, ptone/scion#6009).
#
# scion-hub.service runs it as `ExecStartPre=-` before every start of the
# hub, as the service user (the install owner); the "-" means a failure
# never blocks the start. deploy.sh installs it root-owned (0755) next to
# scion-hub-rollback.sh in /usr/local/libexec/scion/.
#
# It maintains <root>/start-attempts, one line of JSON:
#   {"target":"<release current points at>","count":N}
# On each start: if target is the release current points at, count + 1;
# otherwise (a switch, a missing, unreadable or garbled file) the file
# restarts at {"target":"<current>","count":1}. Written atomically (a fresh
# temp file renamed over it, so a planted symlink is replaced, never written
# through), mode 0644. An existing symlink is never read.
#
# The guard rolls back a crash only if count >= 2 for the release current
# points at (at least one failed restart since its switch). The count does
# not separate a crash loop from a failed operator stop; the guard's other
# conditions do (an unverified, recently switched release, Result not
# timeout). See scion-hub-rollback.sh.
#
# Environment (defaults are what the VM uses; tests override them):
#   SCION_INSTALL_ROOT    install root            (default /opt/scion)

set -uo pipefail

# The temp file of the current write, removed on any exit.
TMP_FILE=""
trap 'rm -f "${TMP_FILE:-}"' EXIT

INSTALL_ROOT="${SCION_INSTALL_ROOT:-/opt/scion}"
readonly ATTEMPTS_MAX_BYTES=4096

log() { echo "scion-hub-start-attempt: $*" >&2; }

# valid_tag TAG -- a release directory name (same rule as scion-layout.sh).
valid_tag() {
  [[ "$1" =~ ^[0-9A-Za-z][0-9A-Za-z._-]*$ ]] && [[ "$1" != *.partial ]] && [[ "$1" != *.old ]]
}

# current_tag -- the release current points at (its last path component),
# or nothing when current is not a symlink to a valid release name.
current_tag() {
  local target tag
  [[ -L "$INSTALL_ROOT/current" ]] || return 1
  target="$(readlink -- "$INSTALL_ROOT/current")" || return 1
  tag="${target%/}"
  tag="${tag##*/}"
  valid_tag "$tag" || return 1
  printf '%s' "$tag"
}

# previous_count TAG -- the count recorded for TAG, or 0 when the file is
# missing, a symlink, not a regular file, too large, or not exactly the
# format this script writes.
previous_count() {
  local f="$INSTALL_ROOT/start-attempts" line size
  [[ -f "$f" && ! -L "$f" ]] || { echo 0; return; }
  size="$(stat -c %s -- "$f" 2>/dev/null)" || { echo 0; return; }
  (( size <= ATTEMPTS_MAX_BYTES )) || { echo 0; return; }
  line="$(head -c "$ATTEMPTS_MAX_BYTES" -- "$f" 2>/dev/null)" || { echo 0; return; }
  if [[ "$line" =~ ^\{\"target\":\"([0-9A-Za-z._-]+)\",\"count\":([0-9]{1,9})\}$ ]] \
     && [[ "${BASH_REMATCH[1]}" == "$1" ]]; then
    echo "$((10#${BASH_REMATCH[2]}))"
  else
    echo 0
  fi
}

main() {
  local tag count
  if ! tag="$(current_tag)"; then
    log "$INSTALL_ROOT/current is not a symlink to a release; not counting this start"
    return 0
  fi
  count=$(( $(previous_count "$tag") + 1 ))
  if ! TMP_FILE="$(mktemp "$INSTALL_ROOT/start-attempts.XXXXXX" 2>/dev/null)"; then
    TMP_FILE=""
    log "cannot write in $INSTALL_ROOT; not counting this start"
    return 0
  fi
  if printf '{"target":"%s","count":%d}\n' "$tag" "$count" > "$TMP_FILE" \
     && chmod 0644 "$TMP_FILE" && mv -fT "$TMP_FILE" "$INSTALL_ROOT/start-attempts"; then
    TMP_FILE=""
    return 0
  fi
  log "could not update $INSTALL_ROOT/start-attempts; not counting this start"
  return 0
}

main "$@"
