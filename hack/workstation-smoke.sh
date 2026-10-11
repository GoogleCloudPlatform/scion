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

# workstation-smoke.sh - end-to-end check of the workstation hub path.
#
# In a temporary HOME and a temporary git repo, with no hub configured:
#   1. scion hub link              fails with the one-line "No hub configured"
#                                  error and starts no server
#   2. scion server start          starts the local workstation server
#   3. scion hub link              links to the local hub without a prompt
#   4. scion hub status            reports the project as linked
#   5. scion list                  returns an empty list through the hub
#   6. scion server stop           stops the server
# Before step 1, 'scion init' creates the project where a container runtime
# is installed; without one, the global project is used (--global). No
# container runtime is needed otherwise.
#
# Usage: hack/workstation-smoke.sh [path-to-scion-binary]
# Without an argument it builds ./cmd/scion with the default build tags
# (SQLite on). The server uses its default ports (web 8080, broker 9800), so
# they must be free.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/scion-ws-smoke.XXXXXX")"
SCION="${1:-}"

log() { printf '==> %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# Set once HOME points at the temporary home, so cleanup never runs
# 'server stop' against the caller's real HOME.
ISOLATED=0

cleanup() {
  local rc=$?
  if [ "$ISOLATED" = 1 ] && [ -n "${SCION:-}" ] && [ -x "$SCION" ]; then
    "$SCION" server stop >/dev/null 2>&1 || true
  fi
  if [ $rc -ne 0 ] && [ -f "$WORK/home/.scion/server.log" ]; then
    echo "--- server log (last 50 lines) ---" >&2
    tail -n 50 "$WORK/home/.scion/server.log" >&2 || true
  fi
  rm -rf "$WORK"
  exit $rc
}
trap cleanup EXIT

if [ -z "$SCION" ]; then
  log "building scion (default tags)"
  (cd "$REPO_ROOT" && go build -buildvcs=false -o "$WORK/scion" ./cmd/scion)
  SCION="$WORK/scion"
fi
SCION="$(cd "$(dirname "$SCION")" && pwd)/$(basename "$SCION")"
[ -x "$SCION" ] || fail "scion binary not found or not executable: $SCION"

# Isolate from the caller: fresh HOME, and no inherited SCION_* settings
# (inside an agent container these point at another hub).
while IFS='=' read -r name _; do
  case "$name" in SCION_*) unset "$name" ;; esac
done < <(env)
export HOME="$WORK/home"
ISOLATED=1
mkdir -p "$HOME"
export SCION_NO_BROWSER=1
# The workstation broker refuses to start without an image registry. No
# image is pulled here, so a placeholder is enough.
export SCION_IMAGE_REGISTRY=registry.invalid/scion-smoke
export GIT_CONFIG_NOSYSTEM=1
git config --global user.name "Scion Smoke"
git config --global user.email "smoke@example.com"
git config --global init.defaultBranch main

REPO="$WORK/repo"
mkdir -p "$REPO"
cd "$REPO"
git init -q
git commit -q --allow-empty -m "initial commit"

# 'scion init' needs a container runtime (it probes podman/docker). Where
# one is available, link the repo's own project; otherwise skip init and
# target the global project with --global (commands run outside a project
# require it). --format json skips the interactive image-registry question.
TARGET=()
if command -v docker >/dev/null 2>&1 || command -v podman >/dev/null 2>&1; then
  log "scion init --machine && scion init"
  "$SCION" init --machine --format json >"$WORK/init-machine.out" 2>&1 || {
    cat "$WORK/init-machine.out" >&2; fail "scion init --machine"; }
  "$SCION" init --format json >"$WORK/init.out" 2>&1 || { cat "$WORK/init.out" >&2; fail "scion init"; }
else
  log "no container runtime found: skipping scion init, using the global project"
  TARGET=(--global)
fi

log "1. with no hub endpoint, scion hub link fails and starts nothing"
if "$SCION" ${TARGET[@]+"${TARGET[@]}"} -y hub link >"$WORK/noep.out" 2>&1; then
  cat "$WORK/noep.out" >&2
  fail "hub link succeeded with no hub endpoint"
fi
grep -qF "No hub configured. Start the local hub with 'scion server start' (first run opens setup), or set a remote one with 'scion config set hub.endpoint <url>'." "$WORK/noep.out" || {
  cat "$WORK/noep.out" >&2; fail "missing the not-configured error"; }
if [ -e "$HOME/.scion/server.pid" ] || grep -q "Starting server" "$WORK/noep.out"; then
  cat "$WORK/noep.out" >&2; fail "hub link started a server"
fi

log "2. scion server start"
"$SCION" server start >"$WORK/start.out" 2>&1 || { cat "$WORK/start.out" >&2; fail "scion server start"; }
grep -q "Configured hub endpoint: http://127.0.0.1:" "$WORK/start.out" || {
  cat "$WORK/start.out" >&2; fail "server start did not configure the hub endpoint"; }
ENDPOINT="$(sed -n 's/^Configured hub endpoint: \(http:[^ ]*\).*/\1/p' "$WORK/start.out" | head -n 1)"
ready=0
for ((i = 1; i <= 60; i++)); do
  if curl -fsS "$ENDPOINT/healthz" >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
[ "$ready" = 1 ] || { cat "$WORK/start.out" >&2; fail "the local server did not answer /healthz"; }

log "3. scion hub link links to the local hub without a prompt"
# No terminal, and no -y for a project: a confirmation prompt would decline.
# The hub seeds a project named "Global", so linking the global project
# meets the separate "matching projects" choice, which this phase leaves
# as is; -y answers only that choice there (the check below still fails
# if the link confirmation appears).
LINK_FLAGS=()
if [ ${#TARGET[@]} -gt 0 ]; then LINK_FLAGS=(-y); fi
"$SCION" ${TARGET[@]+"${TARGET[@]}"} ${LINK_FLAGS[@]+"${LINK_FLAGS[@]}"} hub link </dev/null >"$WORK/link.out" 2>&1 || {
  cat "$WORK/link.out" >&2; fail "scion hub link"; }
cat "$WORK/link.out"
grep -q "Linking project '.*' to the local hub at http://127.0.0.1:" "$WORK/link.out" || fail "hub link did not report the local link"
if grep -q "Continue with linking?" "$WORK/link.out"; then fail "hub link asked for confirmation on the local hub"; fi
grep -q "is now linked to the Hub" "$WORK/link.out" || fail "hub link did not link the project"

log "4. scion hub status shows the project linked"
"$SCION" ${TARGET[@]+"${TARGET[@]}"} hub status --format json >"$WORK/status.json" 2>"$WORK/status.err" || {
  cat "$WORK/status.json" "$WORK/status.err" >&2; fail "scion hub status"; }
grep -Eq '"linked"[[:space:]]*:[[:space:]]*true' "$WORK/status.json" || {
  cat "$WORK/status.json" >&2; fail "hub status does not report the project as linked"; }

log "5. scion list returns an empty list through the hub"
"$SCION" ${TARGET[@]+"${TARGET[@]}"} list >"$WORK/list.out" 2>"$WORK/list.err" || {
  cat "$WORK/list.out" "$WORK/list.err" >&2; fail "scion list"; }
grep -q "Using hub: http://127.0.0.1:" "$WORK/list.err" || {
  cat "$WORK/list.out" "$WORK/list.err" >&2; fail "scion list did not go through the local hub"; }
"$SCION" ${TARGET[@]+"${TARGET[@]}"} list --format json >"$WORK/list.json" 2>"$WORK/list.err" || {
  cat "$WORK/list.json" "$WORK/list.err" >&2; fail "scion list --format json"; }
tr -d '[:space:]' <"$WORK/list.json" | grep -Eq '^(\[\]|null)$' || {
  cat "$WORK/list.json" "$WORK/list.err" >&2; fail "scion list did not return an empty list"; }

log "6. scion server stop"
"$SCION" server stop >"$WORK/stop.out" 2>&1 || { cat "$WORK/stop.out" >&2; fail "scion server stop"; }

log "workstation smoke test passed"
