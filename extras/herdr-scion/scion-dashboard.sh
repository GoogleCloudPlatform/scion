#!/usr/bin/env bash
# scion-dashboard.sh — Create a tiled herdr layout with all running Scion agents.
#
# Sends layout.apply requests straight to herdr's socket API ($HERDR_SOCKET_PATH)
# to build balanced split layouts where each pane runs scion-attach-wrapper.sh
# for one agent — herdr's CLI has no subcommand for layout.apply. The layout
# adapts to the number of agents: 1 agent = single pane, 2 = side-by-side,
# 3+ = grid. herdr refuses a layout with more than 24 panes, so the agents are
# paged: at most SCION_DASHBOARD_PAGE_SIZE (default 16, 1..24) panes per tab,
# one tab per page.
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

# Panes per dashboard tab. herdr 0.9.1 refuses a layout.apply with more than
# 24 panes ("layout has more than 24 panes"); 16 keeps each cell readable.
DASHBOARD_PAGE_DEFAULT=16
DASHBOARD_PAGE_MAX=24

# Print the page size from SCION_DASHBOARD_PAGE_SIZE: a plain decimal integer
# clamped to 1..DASHBOARD_PAGE_MAX; unset means the default without a log
# line; anything else (empty, signs, spaces, other text) means the default.
# A clamp or a fallback writes one fixed log line with the size used, and
# never the variable's name or value. Call once per run.
dashboard_page_size() {
  local raw n changed=0
  if [[ -z "${SCION_DASHBOARD_PAGE_SIZE+x}" ]]; then
    echo "$DASHBOARD_PAGE_DEFAULT"
    return 0
  fi
  raw="$SCION_DASHBOARD_PAGE_SIZE"
  if [[ "$raw" =~ ^[0-9]+$ ]]; then
    # Strip leading zeros so bash does not read the number as octal; more
    # than 4 digits is above the maximum whatever they are (no overflow).
    n="${raw#"${raw%%[!0]*}"}"
    if [[ -z "$n" ]]; then
      n=1 changed=1
    elif [[ "${#n}" -gt 4 || "$n" -gt "$DASHBOARD_PAGE_MAX" ]]; then
      n="$DASHBOARD_PAGE_MAX" changed=1
    fi
  else
    n="$DASHBOARD_PAGE_DEFAULT" changed=1
  fi
  if [[ "$changed" == "1" ]]; then
    log "dashboard page size setting out of range or not an integer; using ${n}"
  fi
  echo "$n"
}

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

# Print the ids of the tabs of this scope's dashboard set in workspace <ws>
# (every workspace when <ws> is empty): the dashboards this one replaces. A
# tab is in the set when its label is exactly <base_label> ("scion: <scope>",
# a one-page dashboard) or <base_label> followed by " [k/P]" (a page of a
# paged one). The prefix is compared as a plain string, so nothing in the
# scope is read as a pattern; only the suffix is a regex, anchored with \z
# so a label with a trailing newline is not in the set. "scion: all
# projects" and other projects' dashboards are not listed. Dashboards made
# before the scion-dash: labels have the same tab label (with panes labelled
# "scion:"), so they are replaced too.
# Returns 1 when herdr's tab list cannot be read.
dashboard_tab_ids() {
  local base_label="$1" ws="$2" tabs_json
  local -a list_args=(tab list)
  [[ -n "$ws" ]] && list_args+=(--workspace "$ws")
  if ! tabs_json="$("$HERDR_BIN" "${list_args[@]}")"; then
    log "herdr tab list failed."
    return 1
  fi
  if ! jq -r --arg base "$base_label" --arg ws "$ws" '
      .result.tabs[]
      | select(($ws == "" or .workspace_id == $ws)
          and (.label | type) == "string"
          and (.label == $base
            or ((.label | startswith($base))
              and (.label[($base | length):] | test("^ \\[[0-9]+/[0-9]+\\]\\z")))))
      | .tab_id' <<<"$tabs_json"; then
    log "Could not read herdr's tab list."
    return 1
  fi
}

# Close the tabs listed in <ids> (one per line); <what> names them in the
# log ("previous", "new"). Closing a tab only ends its attach wrappers; the
# agents and their own "scion:" panes are untouched.
# Returns 1 when a tab could not be closed (the others are still closed).
close_dashboard_tabs() {
  local ids="$1" what="${2:-previous}" tab_id rc=0
  while IFS= read -r tab_id; do
    [[ -z "$tab_id" ]] && continue
    if "$HERDR_BIN" tab close "$tab_id" >/dev/null; then
      log "Closed the ${what} dashboard tab ${tab_id}."
    else
      log "herdr tab close ${tab_id} failed."
      rc=1
    fi
  done <<<"$ids"
  return "$rc"
}

# Print the ids of the tabs labelled exactly <tab_label> in workspace <ws>
# (every workspace when <ws> is empty). Returns 1 when herdr's tab list
# cannot be read.
tab_ids_labelled() {
  local tab_label="$1" ws="$2" tabs_json
  local -a list_args=(tab list)
  [[ -n "$ws" ]] && list_args+=(--workspace "$ws")
  tabs_json="$("$HERDR_BIN" "${list_args[@]}")" || return 1
  jq -r --arg want "$tab_label" --arg ws "$ws" \
    '.result.tabs[] | select(.label == $want and ($ws == "" or .workspace_id == $ws)) | .tab_id' \
    <<<"$tabs_json"
}

# Undo a partly applied dashboard set: close the new pages in <new> (ids,
# one per line), then any tab labelled <failed_label> in <ws> that is in
# neither <old> nor <new>: the failed page, made by herdr although its reply
# was lost. Only that page's label is swept, so another dashboard run's
# pages are left alone, and the previous set (<old>) is never closed here.
# Best effort: every close is logged, and a failure is only logged.
rollback_new_pages() {
  local failed_label="$1" ws="$2" old="$3" new="$4" now tab_id stray=""
  close_dashboard_tabs "$new" new || :
  if ! now="$(tab_ids_labelled "$failed_label" "$ws")"; then
    log "herdr tab list failed; a dashboard page whose reply was lost may remain."
  else
    while IFS= read -r tab_id; do
      [[ -z "$tab_id" ]] && continue
      grep -qxF -- "$tab_id" <<<"$old" && continue
      grep -qxF -- "$tab_id" <<<"$new" && continue
      stray+="${tab_id}"$'\n'
    done <<<"$now"
    close_dashboard_tabs "$stray" new || :
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
    agents="$(jq -r '[.[] | select(.phase == "running")]
              | sort_by([(.project // "global"), (.slug // .name)]) | .[]
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
    agents="$(jq -r '[.[] | select(.phase == "running")] | sort_by(.slug // .name) | .[] | (.slug // .name)' <<<"$list_json")"
  fi

  if [[ -z "$agents" ]]; then
    notify_user "No running Scion agents found."
    open_notice "$SCION_NOTICE_NO_AGENTS" "$SCION_NOTICE_NO_AGENTS_HINT"
    exit 0
  fi

  # Panes per page: read once.
  local page_size
  page_size="$(dashboard_page_size)"

  # One pane row per agent, in the sorted order above; pages are cut from
  # these rows.
  local -a rows=()
  local row count=0 name project project_path project_id
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
    rows+=("$row")
    count=$((count + 1))
  done <<< "$agents"

  if [[ "$count" -eq 0 ]]; then
    log "No agent pane could be built."
    notify_user "Scion dashboard failed; see 'herdr plugin log'"
    exit 1
  fi
  local pages=$(( (count + page_size - 1) / page_size ))
  log "Found $count running agent(s): ${pages} page(s) of up to ${page_size} pane(s)."

  local base_label="scion: ${SCOPE_LABEL}"
  [[ "$all_mode" == "1" ]] && base_label="scion: all projects"

  # One dashboard set per scope (in the invoking workspace): replace, never
  # stack. Collect the previous set's tabs now, apply every page of the new
  # set, and close the old tabs only once all pages exist, so there is never
  # a moment without a dashboard and a failed apply leaves the old set in
  # place. If the tabs cannot be listed, refuse rather than risk a second
  # dashboard.
  local ws old_tabs
  ws="$(invocation_workspace_id)"
  [[ -n "$ws" ]] || log "herdr gave no workspace id; replacing '${base_label}' dashboard tabs in every workspace."
  if ! old_tabs="$(dashboard_tab_ids "$base_label" "$ws")"; then
    notify_user "Could not read herdr's tab list; the Scion dashboard was not opened."
    open_notice "Could not read herdr's tab list; the Scion dashboard was not opened."
    exit 1
  fi

  local request_stamp new_tabs="" first_tab="" k=1 start tab_label page_rows i
  local layout_tree request_id api_request api_response tab_id reply_log
  request_stamp="scion-dashboard-$(date +%s)"
  while [[ "$k" -le "$pages" ]]; do
    tab_label="$base_label"
    [[ "$pages" -gt 1 ]] && tab_label="${base_label} [${k}/${pages}]"
    start=$(( (k - 1) * page_size ))
    page_rows=""
    i="$start"
    while [[ "$i" -lt "$count" && "$i" -lt $((start + page_size)) ]]; do
      page_rows+="${rows[$i]}"$'\n'
      i=$((i + 1))
    done

    if ! layout_tree="$(build_layout_tree <<<"$page_rows")"; then
      log "Could not build the dashboard layout for page ${k} of ${pages}."
      rollback_new_pages "$tab_label" "$ws" "$old_tabs" "$new_tabs"
      notify_user "Scion dashboard failed; see 'herdr plugin log'"
      exit 1
    fi

    # -c (compact) is required: the socket protocol frames one JSON request
    # per line, so the request must not contain embedded newlines.
    request_id="${request_stamp}-${k}"
    if ! api_request="$(jq -nc --arg id "$request_id" --arg tab "$tab_label" --arg ws "$ws" \
        --argjson root "$layout_tree" '{
        id: $id,
        method: "layout.apply",
        params: ({ root: $root, tab_label: $tab }
          + (if $ws == "" then {} else { workspace_id: $ws } end))
      }')"; then
      log "Could not build the layout.apply request for page ${k} of ${pages}."
      rollback_new_pages "$tab_label" "$ws" "$old_tabs" "$new_tabs"
      notify_user "Scion dashboard failed; see 'herdr plugin log'"
      exit 1
    fi

    # herdr has no CLI command for layout.apply — send the request straight
    # to its socket API (newline-terminated JSON), same mechanism herdr's own
    # bundled integrations use.
    log "Applying layout page ${k} of ${pages}..."
    api_response=""
    if ! api_response="$(herdr_socket_request "$api_request")"; then
      log "Failed to apply layout page ${k} of ${pages} via herdr socket API."
      rollback_new_pages "$tab_label" "$ws" "$old_tabs" "$new_tabs"
      notify_user "Scion dashboard failed; see 'herdr plugin log'"
      exit 1
    fi
    # Success is tested positively: herdr answers a request with one JSON
    # object that echoes the request id and carries .result (no .error),
    # with the new tab's id in .result.layout.tab_id. An empty, unparsable or
    # error response is a failure: the pages made so far are closed again
    # and the previous dashboard set is kept.
    if ! tab_id="$(jq -er --arg rid "$request_id" \
        'select(type == "object" and .id == $rid and (.result | type) == "object" and .error == null)
         | .result.layout.tab_id | select(type == "string" and length > 0)' \
        <<<"$api_response" 2>/dev/null)"; then
      # Log the reply defanged: its first 500 bytes, with C0, DEL and C1
      # control bytes removed (C locale, so the cut and tr work on bytes).
      reply_log="$(export LC_ALL=C; printf '%s' "${api_response:0:500}" | tr -d '\000-\037\177-\237')"
      log "herdr layout.apply did not report success for page ${k} of ${pages} (reply, first 500 bytes, control bytes removed: ${reply_log:-none}); the previous dashboard is kept."
      rollback_new_pages "$tab_label" "$ws" "$old_tabs" "$new_tabs"
      notify_user "Scion dashboard failed; see 'herdr plugin log'"
      exit 1
    fi
    new_tabs+="${tab_id}"$'\n'
    [[ -n "$first_tab" ]] || first_tab="$tab_id"
    k=$((k + 1))
  done

  log "Dashboard created with $count agent pane(s) in ${pages} tab(s)."

  if ! close_dashboard_tabs "$old_tabs"; then
    notify_user "An older Scion dashboard tab could not be closed; close it by hand"
  fi

  # Show the first page. layout.apply's own focus flag moves herdr's focus
  # but not an attached client's view; tab focus does both. Not being able
  # to focus is not a failure: the dashboard exists.
  if ! "$HERDR_BIN" tab focus "$first_tab" >/dev/null 2>&1; then
    log "dashboard opened but could not be focused"
  fi

  if [[ "$pages" -gt 1 ]]; then
    open_notice "Scion dashboard: ${count} agents in ${pages} tabs"
  fi

  # Start the state bridge in the background. Redirect its stdio away from
  # ours: herdr's plugin runtime waits for this script's stdout/stderr pipes
  # to close before marking the action finished, and a background child that
  # inherits them holds them open for as long as it runs (forever, here).
  bash "${SCRIPT_DIR}/scion-state-bridge.sh" </dev/null >/dev/null 2>&1 &
  disown
}

main "$@"
