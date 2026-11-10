#!/usr/bin/env bash
# scion-attach-wrapper.sh — Wrap `scion attach` with reconnection and
# idempotent start-or-attach logic, bound to one scion project.
#
# Usage:
#   scion-attach-wrapper.sh <name> --mode pin|context --project <project>
#                           --label <slug> [--hub <endpoint>] [--no-verify]
#                           [--start <template>]
#
# The scope arguments come from scion-common.sh (scope_wrapper_args):
#   --mode pin      every scion call gets `-g <project>` (a path, or "global")
#   --mode context  scion calls get no -g; scion applies its `scion use`
#                   context, and <project> is the hub project UUID that
#                   context must still name
#   --hub           the hub endpoint scion reported when the pane was opened
#   --label         the project slug (pane label, herdr agent name)
#   --no-verify     skip the context check ("all" mode)
# Before every scion call the wrapper checks that `scion use` still
# resolves to that project and hub (scope_check). If it doesn't (the user
# ran `scion use` for something else), it calls nothing, says so, and waits
# for Enter to check again — a pane never attaches to a different project.
# Every scion call runs from an empty temp dir, so the pane's CWD can't
# change the target either.
#
# Handles:
#   - Reconnection after inner tmux detach (Ctrl-b d inside scion session),
#     automatically after 10 seconds or on Enter.
#   - A reconnect cap: after 3 attach sessions in a row that each ended in
#     under 60 seconds, stop reconnecting by itself and wait for Enter
#     (scion attach already retries on its own; this stops an endless loop
#     once that has given up).
#   - Graceful exit when the agent stops running.
#   - Idempotent start: if the agent is already running, attaches instead of
#     starting a duplicate (works around the Hub-mode idempotency gap where
#     `scion start -a` may not handle already-running agents).
#
# Requires: scion, jq on PATH.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=scion-common.sh
source "${SCRIPT_DIR}/scion-common.sh"

USAGE="Usage: scion-attach-wrapper.sh <name> --mode pin|context --project <project> --label <slug> [--hub <endpoint>] [--no-verify] [--start <template>]"
NAME="${1:?$USAGE}"
shift

MODE=""
PROJECT=""
HUB=""
LABEL=""
VERIFY=1
TEMPLATE=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --mode)      MODE="${2:?--mode requires a value}"; shift 2 ;;
    --project)   PROJECT="${2:?--project requires a value}"; shift 2 ;;
    --hub)       HUB="${2:?--hub requires a value}"; shift 2 ;;
    --label)     LABEL="${2:?--label requires a value}"; shift 2 ;;
    --no-verify) VERIFY=0; shift ;;
    --start)     TEMPLATE="${2:?--start requires a template name}"; shift 2 ;;
    *) echo "Unknown argument: $1" >&2; echo "$USAGE" >&2; exit 1 ;;
  esac
done

if [[ "$MODE" != "pin" && "$MODE" != "context" ]] || [[ -z "$PROJECT" ]]; then
  echo "$USAGE" >&2
  exit 1
fi
[[ -n "$LABEL" ]] || LABEL="$(basename "$PROJECT")"

# Reconnect cap: this many attach sessions in a row, each shorter than
# SHORT_SESSION_SECS, stop the automatic reconnect.
MAX_SHORT_SESSIONS=3
SHORT_SESSION_SECS=60

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

log() { echo "[scion-attach] $*" >&2; }

check_deps() {
  local missing=()
  for cmd in scion jq; do
    command -v "$cmd" >/dev/null 2>&1 || missing+=("$cmd")
  done
  if [[ ${#missing[@]} -gt 0 ]]; then
    log "Missing required commands: ${missing[*]}"
    exit 1
  fi
}

# Block until scion still resolves this pane's scope. On a mismatch, print
# why and wait for Enter, then check again.
ensure_scope() {
  while ! scope_check; do
    echo ""
    echo "$SCOPE_MISMATCH_MSG"
    echo "Press Enter to check again, or Ctrl-C to close this pane."
    read -r || exit 1
  done
}

# Check whether the agent is currently running. Scoped to this pane's
# project, so matching on name alone is unambiguous.
# The pane shows its own messages, so these list calls are quiet (no herdr
# notification; see scion_list).
is_running() {
  SCION_LIST_QUIET=1 scion_list "Checking the Scion agent" scion_scoped -r 2>/dev/null \
    | jq -e --arg n "$NAME" '.[] | select((.slug // .name) == $n and .phase == "running")' \
    >/dev/null 2>&1
}

# Get the agent's current lifecycle phase, or "unknown" with the reason when
# the list failed or was truncated.
agent_phase() {
  local list_json phase rc=0
  list_json="$(SCION_LIST_QUIET=1 scion_list "Checking the Scion agent" scion_scoped 2>/dev/null)" || rc=$?
  case "$rc" in
    0) ;;
    3) echo "unknown: the agent list was truncated"; return 0 ;;
    *) echo "unknown: scion list failed"; return 0 ;;
  esac
  phase="$(jq -r --arg n "$NAME" '.[] | select((.slug // .name) == $n) | .phase // "unknown"' \
    <<<"$list_json" 2>/dev/null | head -1)"
  echo "${phase:-unknown}"
}

# Register this pane with herdr for state reporting. HERDR_PANE_ID is set by
# herdr for processes in a pane. The state is unknown until the state bridge
# reports the agent's real state.
register_pane() {
  if [[ -n "${HERDR_PANE_ID:-}" ]]; then
    "$HERDR_BIN" pane report-agent "$HERDR_PANE_ID" \
      --source "scion:integration" \
      --agent "scion/${LABEL}/${NAME}" \
      --state unknown >/dev/null
  fi
}

# ---------------------------------------------------------------------------
# Connect — start if necessary
# ---------------------------------------------------------------------------

connect() {
  if is_running; then
    log "Agent $NAME is running — attaching."
    scion_scoped attach "$NAME"
    return $?
  fi

  if [[ -n "$TEMPLATE" ]]; then
    log "Agent $NAME is not running — starting from template '$TEMPLATE'."
    scion_scoped start "$NAME" -t "$TEMPLATE" -a
    return $?
  fi

  # No template and agent not running — try attach anyway so the user gets
  # scion's own error message.
  log "Agent $NAME is not running and no template provided — attempting attach."
  scion_scoped attach "$NAME"
  return $?
}

# ---------------------------------------------------------------------------
# Reconnection loop
# ---------------------------------------------------------------------------

reconnect_loop() {
  local short_sessions=0
  while true; do
    ensure_scope
    local started=$SECONDS
    connect
    local exit_code=$?
    local lasted=$(( SECONDS - started ))

    ensure_scope
    local phase
    phase="$(agent_phase)"

    if [[ "$phase" != "running" ]]; then
      echo ""
      case "$phase" in
        "unknown: "*)
          # The list failed (e.g. the hub is restarting) or was truncated:
          # don't claim the agent stopped.
          echo "Disconnected from $NAME, and its state could not be checked (${phase#unknown: })."
          echo "Run the refresh action to reopen it later."
          ;;
        *) echo "Agent $NAME is no longer running (phase: ${phase})." ;;
      esac
      echo "Press Enter to close this pane, or Ctrl-C."
      read -r || true
      return "$exit_code"
    fi

    if (( lasted < SHORT_SESSION_SECS )); then
      short_sessions=$(( short_sessions + 1 ))
    else
      short_sessions=0
    fi

    echo ""
    if (( short_sessions >= MAX_SHORT_SESSIONS )); then
      echo "Disconnected from $NAME: the last $short_sessions sessions each ended in under ${SHORT_SESSION_SECS}s, so automatic reconnect is off."
      echo "Press Enter to reconnect, or Ctrl-C to close."
      read -r || exit 1
      short_sessions=0
    else
      echo "Disconnected from $NAME. Press Enter to reconnect, or Ctrl-C to close."
      # Auto-reconnect after 10 seconds if the user doesn't press anything.
      read -r -t 10 || true
    fi
  done
}

# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

check_deps
scope_set "$MODE" "$PROJECT" "$HUB" "$LABEL" "$VERIFY" || exit 1
register_pane
reconnect_loop
