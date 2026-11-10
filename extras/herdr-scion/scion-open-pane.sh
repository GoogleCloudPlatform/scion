#!/usr/bin/env bash
# scion-open-pane.sh — Launcher action that opens an interactive plugin pane.
#
# scion-pick-agent.sh and scion-start-agent.sh are interactive (fzf, or
# `read -rp` for their fallback menus). Plugin actions run with piped
# stdout/stderr and no controlling terminal, so they can't run as actions
# directly. Instead, this tiny launcher (itself an action, so it does have
# to run non-interactively) opens the real script as an overlay pane, which
# does get a terminal and closes automatically when the script exits.
#
# Usage: scion-open-pane.sh <pick|start>
#
# Passes the action-invoking pane's ID through as SCION_TARGET_PANE, so the
# picker/start script can split the new agent pane off from the user's pane
# rather than off the overlay itself.
#
# Requires: herdr on PATH (or $HERDR_BIN_PATH).

set -euo pipefail

ENTRYPOINT="${1:?Usage: scion-open-pane.sh <pick|start>}"
HERDR_BIN="${HERDR_BIN_PATH:-herdr}"

"$HERDR_BIN" plugin pane open \
  --plugin scion.herdr-integration \
  --entrypoint "$ENTRYPOINT" \
  --placement overlay \
  --env "SCION_TARGET_PANE=${HERDR_PANE_ID:-}"
