#!/usr/bin/env bash
# scion-pick-agent.sh — Interactive agent picker for the "Attach to Scion Agent"
# herdr action.
#
# Lists running agents in the effective scion project (every project with
# SCION_HERDR_SCOPE=all), lets the user pick one (via fzf if available,
# falling back to a numbered menu), then creates a herdr pane attached to
# that agent. The header names the project and where scion got it from.
#
# Requires: scion, herdr, jq on PATH. fzf is optional.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=scion-common.sh
source "${SCRIPT_DIR}/scion-common.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

log() { echo "[scion-pick] $*" >&2; }

# Build a display row for each running agent: display, template, activity,
# project, name, projectPath, projectId ("-" when empty). Only the first three columns are shown (see
# --with-nth below); the rest travel through unseen so the caller can
# recover the agent's project after a selection. In all mode display is
# "project/name" (names are only unique within a project). Uses
# .slug // .name because slug is omitempty in local/podman mode.
agent_display_lines() {
  local list_json
  if [[ "$SCOPE_MODE_ALL" == "1" ]]; then
    list_json="$(scion_list "Listing Scion agents" scion_unscoped -a -r)" || return 1
    jq -r '.[] | select(.phase == "running")
             | [((.project // "global") + "/" + (.slug // .name)),
                (.template // "-"), (.activity // "idle"),
                (.project // "global"), (.slug // .name), (.projectPath // "-"), (.projectId // "-")]
             | map(if . == "" then "-" else . end) | @tsv' <<<"$list_json"
  else
    list_json="$(scion_list "Listing Scion agents" scion_scoped -r)" || return 1
    jq -r --arg p "$SCOPE_LABEL" '.[] | select(.phase == "running")
             | [(.slug // .name), (.template // "-"), (.activity // "idle"),
                $p, (.slug // .name), "-", "-"] | @tsv' <<<"$list_json"
  fi
}

# Pick using fzf if available, otherwise a basic numbered menu on
# stderr/stdin. Returns the full selected row (all 7 tab-separated fields),
# not just the display column — fzf's --with-nth only changes what's shown
# and matched against, not what's printed on selection.
pick_agent() {
  local lines="$1"

  if command -v fzf >/dev/null 2>&1; then
    echo "$lines" \
      | fzf --prompt="Select Scion agent> " \
            --delimiter=$'\t' \
            --with-nth=1,2,3 \
            --header="$(scope_header)"$'\n'"AGENT  TEMPLATE  ACTIVITY"
    return
  fi

  # Fallback: numbered menu. Keep each full row so project/name/projectPath
  # survive a numeric pick too.
  local -a rows=()
  local i=1
  echo "" >&2
  scope_header >&2
  echo "Running Scion agents:" >&2
  while IFS= read -r line; do
    local display tmpl activity
    IFS=$'\t' read -r display tmpl activity _ _ _ _ <<< "$line"
    printf "  %d) %-32s  %-16s  %s\n" "$i" "$display" "$tmpl" "$activity" >&2
    rows+=("$line")
    i=$((i + 1))
  done <<< "$lines"

  echo "" >&2
  read -rp "Select agent [1-${#rows[@]}]: " choice
  if [[ "$choice" =~ ^[0-9]+$ ]] && (( choice >= 1 && choice <= ${#rows[@]} )); then
    echo "${rows[$((choice - 1))]}"
  fi
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

# Keep the overlay open on an error long enough to read it.
fail() {
  echo "$*" >&2
  read -rp "Press Enter to close." _ || true
  exit 1
}

main() {
  check_deps

  SCOPE_MODE_ALL=0
  if [[ "$(herdr_scope_mode)" == "all" ]]; then
    SCOPE_MODE_ALL=1
    scope_set pin global "" all 0 all
  elif ! resolve_scope; then
    fail "Cannot list agents: no scion project resolved."
  fi

  local lines
  lines="$(agent_display_lines)" || fail "scion list failed or was truncated — see the error above."

  if [[ -z "$lines" ]]; then
    scope_header >&2
    fail "No running Scion agents found."
  fi

  local selected_row
  selected_row="$(pick_agent "$lines")"

  if [[ -z "$selected_row" ]]; then
    echo "No agent selected." >&2
    exit 0
  fi

  local project name project_path project_id
  IFS=$'\t' read -r _ _ _ project name project_path project_id <<< "$selected_row"
  if [[ "$SCOPE_MODE_ALL" == "1" ]]; then
    # Path when known, else the project id (a UUID; see scion-discover.sh).
    [[ "$project_path" == "-" ]] && project_path=""
    [[ "$project_id" == "-" ]] && project_id=""
    scope_set pin "${project_path:-${project_id:-$project}}" "" "$project" 0 all
  fi
  local identifier="${SCOPE_LABEL}/${name}"

  # Check if a pane already exists for this agent (tracked via pane .label).
  if pane_exists_for_agent "$identifier"; then
    log "Pane already exists for $identifier — focusing it."
    local pane_id
    pane_id="$(pane_id_for_agent "$identifier")"
    if [[ -n "$pane_id" ]]; then
      # `agent focus` takes a pane id when an agent is reported in it (the
      # wrapper reports one); 0.9.1's `pane focus` is directional only.
      "$HERDR_BIN" agent focus "$pane_id" >/dev/null
    fi
    exit 0
  fi

  log "Creating pane for agent: $identifier"
  open_agent_pane "$name" >/dev/null || fail "Could not open a pane for $identifier."
}

main "$@"
