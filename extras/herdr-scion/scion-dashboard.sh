#!/usr/bin/env bash
# scion-dashboard.sh — Create a tiled herdr layout with all running Scion agents.
#
# Sends a layout.apply request straight to herdr's socket API ($HERDR_SOCKET_PATH)
# to build a balanced split layout where each pane runs scion-attach-wrapper.sh
# for one agent — herdr's CLI has no subcommand for layout.apply. The layout
# adapts to the number of agents: 1 agent = single pane, 2 = side-by-side,
# 3+ = grid.
#
# Requires: scion, herdr, jq on PATH.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=scion-common.sh
source "${SCRIPT_DIR}/scion-common.sh"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

log() { echo "[scion-dashboard] $*" >&2; }

# Field separator for pane rows (ASCII unit separator).
PANE_ROW_SEP=$'\x1f'

# Print one pane row for agent $1 in the current scope (SCOPE_*), and record
# the scope for the state bridge. A row is the pane label, the agent name,
# the project label and the wrapper command, joined by PANE_ROW_SEP;
# build_layout_tree turns all rows into the layout in one jq call.
#
# Dashboard panes are labelled "scion-dash:<project>/<name>", never with the
# agent pane label "scion:<project>/<name>": discover's existing-pane check
# and other label readers match '^scion:', and "scion-dash:" does not match
# it (the sixth character is "-"). The dashboard is a second view of an agent,
# not the agent's own pane.
#
# Returns 1 when a field contains a newline or the separator.
pane_row() {
  local name="$1" field row=""
  scope_record_write || log "Warning: could not write the scope record for ${SCOPE_LABEL}"
  scope_wrapper_argv
  local -a fields=("scion-dash:${SCOPE_LABEL}/${name}" "$name" "$SCOPE_LABEL"
    bash "${SCRIPT_DIR}/scion-attach-wrapper.sh" "$name" "${SCOPE_WRAPPER_ARGV[@]}")
  for field in "${fields[@]}"; do
    case "$field" in
      *$'\n'*|*"$PANE_ROW_SEP"*) return 1 ;;
    esac
    row+="${row:+$PANE_ROW_SEP}${field}"
  done
  printf '%s\n' "$row"
}

# Read pane rows (pane_row) on stdin and print the layout: a balanced binary
# split tree of pane nodes, alternating split direction by depth (even=right,
# odd=down) for a grid. One jq call for all agents.
#
# jq 1.6: no jq keyword as a $variable or shorthand key (`label: .[0]` is
# fine; a variable named $label is not).
build_layout_tree() {
  jq -Rn --arg sep "$PANE_ROW_SEP" '
    def tree($depth):
      if length == 0 then null
      elif length == 1 then .[0]
      else (length / 2 | floor) as $mid
        | { type: "split",
            direction: (if $depth % 2 == 0 then "right" else "down" end),
            ratio: 0.5,
            first: (.[:$mid] | tree($depth + 1)),
            second: (.[$mid:] | tree($depth + 1)) }
      end;
    [ inputs | select(length > 0) | split($sep)
      | { type: "pane",
          label: .[0],
          command: .[3:],
          env: { SCION_AGENT: .[1], SCION_PROJECT: .[2] } } ]
    | tree(0)'
}

# The herdr workspace this action was invoked from (herdr's
# PluginInvocationContext.workspace_id), or empty when herdr gave none.
invocation_workspace_id() {
  local ws=""
  if [[ -n "${HERDR_PLUGIN_CONTEXT_JSON:-}" ]]; then
    if ! ws="$(jq -r '.workspace_id // empty' <<<"$HERDR_PLUGIN_CONTEXT_JSON" 2>/dev/null)"; then
      ws=""
    fi
  fi
  echo "$ws"
}

# Print the ids of the tabs labelled exactly <tab_label> in workspace <ws>
# (every workspace when <ws> is empty): the dashboards this one replaces. The
# match is exact: "scion: all projects" and other projects' dashboards are
# not listed. Dashboards made before the scion-dash: labels have the same tab
# label (with panes labelled "scion:"), so they are replaced too.
# Returns 1 when herdr's tab list cannot be read.
dashboard_tab_ids() {
  local tab_label="$1" ws="$2" tabs_json
  local -a list_args=(tab list)
  [[ -n "$ws" ]] && list_args+=(--workspace "$ws")
  if ! tabs_json="$("$HERDR_BIN" "${list_args[@]}")"; then
    log "herdr tab list failed."
    return 1
  fi
  if ! jq -r --arg tab_label "$tab_label" --arg ws "$ws" \
      '.result.tabs[] | select(.label == $tab_label and ($ws == "" or .workspace_id == $ws)) | .tab_id' \
      <<<"$tabs_json"; then
    log "Could not read herdr's tab list."
    return 1
  fi
}

# Close the tabs listed in <ids> (one per line). Closing a tab only ends its
# attach wrappers; the agents and their own "scion:" panes are untouched.
# Returns 1 when a tab could not be closed (the others are still closed).
close_dashboard_tabs() {
  local ids="$1" tab_id rc=0
  while IFS= read -r tab_id; do
    [[ -z "$tab_id" ]] && continue
    if "$HERDR_BIN" tab close "$tab_id" >/dev/null; then
      log "Closed the previous dashboard tab ${tab_id}."
    else
      log "herdr tab close ${tab_id} failed."
      rc=1
    fi
  done <<<"$ids"
  return "$rc"
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
  check_deps
  local all_mode=0 list_json agents
  if [[ "$(herdr_scope_mode)" == "all" ]]; then
    all_mode=1
    scope_dir_init || exit 1
    log "Building agent dashboard for all projects (SCION_HERDR_SCOPE=all)..."
    list_json="$(scion_list "Scion dashboard" scion_unscoped -a -r)" || {
      log "scion list failed or was truncated."
      exit 1
    }
    agents="$(jq -r '.[] | select(.phase == "running")
              | [(.slug // .name), (.project // "global"), (.projectPath // "-"), (.projectId // "-")]
              | map(if . == "" then "-" else . end) | @tsv' <<<"$list_json")"
  else
    if ! resolve_scope; then
      log "Dashboard aborted: no scion project resolved."
      if [[ "${SCOPE_FAIL_REASON:-}" == "no_project" ]]; then
        open_notice "$SCION_NOTICE_NO_PROJECT" "$SCION_NOTICE_NO_PROJECT_HINT"
      fi
      exit 1
    fi
    log "Building agent dashboard — $(scope_header)..."
    list_json="$(scion_list "Scion dashboard" scion_scoped -r)" || {
      log "scion list failed or was truncated."
      exit 1
    }
    agents="$(jq -r '.[] | select(.phase == "running") | (.slug // .name)' <<<"$list_json")"
  fi

  if [[ -z "$agents" ]]; then
    notify_user "No running Scion agents found."
    open_notice "$SCION_NOTICE_NO_AGENTS" "$SCION_NOTICE_NO_AGENTS_HINT"
    exit 0
  fi

  # One pane row per agent, then the whole layout in one jq call.
  local rows="" row count=0 name project project_path project_id
  while IFS=$'\t' read -r name project project_path project_id; do
    [[ -z "$name" ]] && continue
    if [[ "$all_mode" == "1" ]]; then
      # Path when known, else the project id (see scion-discover.sh).
      [[ "$project_path" == "-" ]] && project_path=""
      [[ "$project_id" == "-" ]] && project_id=""
      scope_set pin "${project_path:-${project_id:-$project}}" "" "$project" 0 all
    fi
    if ! row="$(pane_row "$name")"; then
      log "Skipping ${SCOPE_LABEL}/${name}: a pane field contains a newline or the unit separator."
      continue
    fi
    rows+="${row}"$'\n'
    count=$((count + 1))
  done <<< "$agents"

  if [[ "$count" -eq 0 ]]; then
    log "No agent pane could be built."
    notify_user "Scion dashboard failed; see 'herdr plugin log'"
    exit 1
  fi
  log "Found $count running agent(s). Building layout..."

  local layout_tree
  if ! layout_tree="$(build_layout_tree <<<"$rows")"; then
    log "Could not build the dashboard layout."
    notify_user "Scion dashboard failed; see 'herdr plugin log'"
    exit 1
  fi

  # Apply via herdr API.
  local request_id
  request_id="scion-dashboard-$(date +%s)"

  local tab_label="scion: ${SCOPE_LABEL}"
  [[ "$all_mode" == "1" ]] && tab_label="scion: all projects"

  # One dashboard tab per scope (in the invoking workspace): replace, never
  # stack. Collect the previous dashboard tabs now, apply the new layout, and
  # close them only once it exists, so there is never a moment without a
  # dashboard and a failed apply leaves the old one in place. If the tabs
  # cannot be listed, refuse rather than risk a second dashboard.
  local ws old_tabs
  ws="$(invocation_workspace_id)"
  [[ -n "$ws" ]] || log "herdr gave no workspace id; replacing '${tab_label}' tabs in every workspace."
  if ! old_tabs="$(dashboard_tab_ids "$tab_label" "$ws")"; then
    notify_user "Could not read herdr's tab list; the Scion dashboard was not opened."
    open_notice "Could not read herdr's tab list; the Scion dashboard was not opened."
    exit 1
  fi

  # -c (compact) is required: the socket protocol frames one JSON request
  # per line, so the request must not contain embedded newlines.
  local api_request
  api_request="$(jq -nc --arg id "$request_id" --arg tab "$tab_label" --arg ws "$ws" \
    --argjson root "$layout_tree" '{
    id: $id,
    method: "layout.apply",
    params: ({ root: $root, tab_label: $tab }
      + (if $ws == "" then {} else { workspace_id: $ws } end))
  }')"

  # herdr has no CLI command for layout.apply — send the request straight to
  # its socket API (newline-terminated JSON), same mechanism herdr's own
  # bundled integrations use.
  log "Applying layout..."
  local api_response
  if ! api_response="$(herdr_socket_request "$api_request")"; then
    log "Failed to apply layout via herdr socket API."
    notify_user "Scion dashboard failed; see 'herdr plugin log'"
    exit 1
  fi
  # Success is tested positively: herdr answers a request with one JSON
  # object that echoes the request id and carries .result (no .error). An
  # empty, unparsable or error response is a failure, and the previous
  # dashboard tabs are then kept.
  if ! jq -e --arg rid "$request_id" \
      'type == "object" and .id == $rid and (.result | type) == "object" and .error == null' \
      <<<"$api_response" >/dev/null 2>&1; then
    # Log the reply defanged: its first 500 bytes, with C0, DEL and C1
    # control bytes removed (C locale, so the cut and tr work on bytes).
    local reply_log
    reply_log="$(export LC_ALL=C; printf '%s' "${api_response:0:500}" | tr -d '\000-\037\177-\237')"
    log "herdr layout.apply did not report success (reply, first 500 bytes, control bytes removed: ${reply_log:-none}); the previous dashboard is kept."
    notify_user "Scion dashboard failed; see 'herdr plugin log'"
    exit 1
  fi

  log "Dashboard created with $count agent pane(s)."

  if ! close_dashboard_tabs "$old_tabs"; then
    notify_user "An older Scion dashboard tab could not be closed; close it by hand"
  fi

  # Start the state bridge in the background. Redirect its stdio away from
  # ours: herdr's plugin runtime waits for this script's stdout/stderr pipes
  # to close before marking the action finished, and a background child that
  # inherits them holds them open for as long as it runs (forever, here).
  bash "${SCRIPT_DIR}/scion-state-bridge.sh" </dev/null >/dev/null 2>&1 &
  disown
}

main "$@"
