#!/usr/bin/env bash
# scion-discover.sh — Discover running Scion agents and create herdr panes.
#
# Called as a startup hook and as the "Refresh Scion Agents" action.
# Lists running agents in the effective scion project (see resolve_scope in
# scion-common.sh; every project with SCION_HERDR_SCOPE=all), and for each
# one not already represented by a herdr pane, creates a new pane running
# scion-attach-wrapper.sh.
#
# Requires: scion, herdr, jq on PATH.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=scion-common.sh
source "${SCRIPT_DIR}/scion-common.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

log() { echo "[scion-discover] $*" >&2; }

# Milliseconds since the epoch, for the timing line: bash 5's EPOCHREALTIME
# when set, else GNU date's %3N, else whole seconds (BSD date has
# no %N; bash 3.2 has no EPOCHREALTIME).
now_ms() {
  local t
  if [[ -n "${EPOCHREALTIME:-}" ]]; then
    t="${EPOCHREALTIME/[.,]/}"
    if [[ "$t" =~ ^[0-9]+$ ]]; then
      echo $((t / 1000))
      return 0
    fi
  fi
  t="$(date +%s%3N 2>/dev/null)"
  if [[ "$t" =~ ^[0-9]+$ ]]; then
    echo "$t"
  else
    echo $(( $(date +%s) * 1000 ))
  fi
}

# Running agents as TSV rows: name, then (all mode only) project,
# projectPath and projectId, "-" when empty (read with IFS=tab collapses
# empty fields). Uses .slug // .name because slug is omitempty in local/podman
# mode. Default mode lists the scoped project; all mode lists every project.
running_agents() {
  local list_json
  if [[ "$SCOPE_MODE_ALL" == "1" ]]; then
    list_json="$(scion_list "Scion agent discovery" scion_unscoped -a -r)" || return 1
    jq -r '.[] | select(.phase == "running")
             | [(.slug // .name), (.project // "global"), (.projectPath // "-"), (.projectId // "-")]
             | map(if . == "" then "-" else . end) | @tsv' <<<"$list_json"
  else
    list_json="$(scion_list "Scion agent discovery" scion_scoped -r)" || return 1
    jq -r '.[] | select(.phase == "running") | (.slug // .name)' <<<"$list_json"
  fi
}

# Collect agent identifiers from existing herdr panes that belong to this plugin.
# Panes are tracked by their .label, set via `herdr pane rename` right after
# split, with the format "scion:<identifier>".
existing_pane_agents() {
  local panes_json
  if ! panes_json="$("$HERDR_BIN" pane list)"; then
    log "Warning: herdr pane list failed — assuming no existing panes."
    return 0
  fi
  echo "$panes_json" \
    | jq -r '.result.panes[] | select(.label != null) | .label' \
    | grep '^scion:' \
    | sed 's|^scion:||' || true
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

check_deps() {
  local missing=()
  for cmd in scion jq; do
    command -v "$cmd" >/dev/null 2>&1 || missing+=("$cmd")
  done
  command -v "$HERDR_BIN" >/dev/null 2>&1 || missing+=("$HERDR_BIN")
  if [[ ${#missing[@]} -gt 0 ]]; then
    log "Missing required commands: ${missing[*]}"
    notify_user "Scion plugin: a required command is missing; see 'herdr plugin log'"
    exit 1
  fi
}

main() {
  local t_start t_list t_panes t_diff_end open_ms=0 open_t
  t_start="$(now_ms)"
  check_deps

  SCOPE_MODE_ALL=0
  if [[ "$(herdr_scope_mode)" == "all" ]]; then
    SCOPE_MODE_ALL=1
    scope_dir_init || exit 1
    log "Discovering running Scion agents in all projects (SCION_HERDR_SCOPE=all)..."
  else
    if ! resolve_scope; then
      log "Discovery aborted: no scion project resolved."
      if [[ "${SCOPE_FAIL_REASON:-}" == "no_project" ]]; then
        open_notice "$SCION_NOTICE_NO_PROJECT" "$SCION_NOTICE_NO_PROJECT_HINT"
      fi
      exit 1
    fi
    log "Discovering running Scion agents — $(scope_header) [mode ${SCOPE_MODE}: ${SCOPE_PROJECT}${SCOPE_HUB:+, hub ${SCOPE_HUB}}]..."
  fi

  local agents
  t_list="$(now_ms)"
  if ! agents="$(running_agents)"; then
    log "scion list failed or was truncated — see the error above."
    exit 1
  fi

  if [[ -z "$agents" ]]; then
    t_panes="$(now_ms)"
    log "timing ms: scion_list=$((t_panes - t_list)) pane_list=0 diff=0 opens=0 opens_total=0 total=$((t_panes - t_start))"
    log "No running Scion agents found."
    notify_user "No running Scion agents found."
    open_notice "$SCION_NOTICE_NO_AGENTS" "$SCION_NOTICE_NO_AGENTS_HINT"
    start_state_bridge
    return 0
  fi

  t_panes="$(now_ms)"
  local existing
  existing="$(existing_pane_agents)"
  t_diff_end="$(now_ms)"

  local created=0 failed=0
  local name project project_path project_id
  while IFS=$'\t' read -r name project project_path project_id; do
    [[ -z "$name" ]] && continue

    if [[ "$SCOPE_MODE_ALL" == "1" ]]; then
      # Each agent carries its own project; pin it as `scion list -a`
      # reported it: the path when known, else the project id (a UUID; a
      # name can be shadowed by a slug, be ambiguous, or contain "/").
      [[ "$project_path" == "-" ]] && project_path=""
      [[ "$project_id" == "-" ]] && project_id=""
      scope_set pin "${project_path:-${project_id:-$project}}" "" "$project" 0 all
    fi

    # Identifier is project/name: scion agent names are only unique within a
    # project, and panes from other projects (earlier refreshes under
    # another `scion use`, or all mode) share the workspace.
    local identifier="${SCOPE_LABEL}/${name}"

    if grep -qxF "$identifier" <<<"$existing"; then
      log "Pane already exists for $identifier — skipping."
      continue
    fi

    log "Creating pane for agent: $identifier"
    open_t="$(now_ms)"
    if ! open_agent_pane "$name" >/dev/null; then
      open_ms=$((open_ms + $(now_ms) - open_t))
      log "Could not open a pane for $identifier — skipping."
      failed=$((failed + 1))
      continue
    fi
    open_ms=$((open_ms + $(now_ms) - open_t))
    created=$((created + 1))
  done <<< "$agents"

  local t_end
  t_end="$(now_ms)"
  log "Discovery complete. Created $created new pane(s)."
  # Phase timings in ms, numbers only: scion list, herdr pane
  # list, the diff (the loop without its pane opens), the pane opens (count
  # and total; failed opens count too), and the whole run.
  log "timing ms: scion_list=$((t_panes - t_list)) pane_list=$((t_diff_end - t_panes)) diff=$((t_end - t_diff_end - open_ms)) opens=$((created + failed)) opens_total=${open_ms} total=$((t_end - t_start))"
  if [[ "$failed" -gt 0 ]]; then
    notify_user "Could not open a pane for ${failed} Scion agent(s); see 'herdr plugin log'"
  fi
  start_state_bridge
}

# Start the state bridge in the background (it exits at once if one is
# already running). Redirect its stdio away from ours: herdr's plugin
# runtime waits for this script's stdout/stderr pipes to close before
# marking the action/startup hook finished, and a background child that
# inherits them holds them open for as long as it runs (forever).
start_state_bridge() {
  bash "${SCRIPT_DIR}/scion-state-bridge.sh" </dev/null >/dev/null 2>&1 &
  disown
}

main "$@"
