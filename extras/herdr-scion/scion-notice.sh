#!/usr/bin/env bash
# scion-notice.sh — Overlay pane that shows one plugin message.
#
# Opened by open_notice (scion-common.sh) as the "notice" plugin pane. The
# message is the plugin's own fixed text, in the 0600 file named by
# SCION_NOTICE_FILE (herdr passes plugin panes no arguments). Closes on
# Enter, or by itself after SCION_NOTICE_TIMEOUT seconds (default 30); the
# pane closes when this script exits.
#
# SCION_NOTICE_FILE is only trusted when it is a file open_notice could have
# made: directly in the plugin state dir (HERDR_PLUGIN_STATE_DIR, or TMPDIR
# when that is unset), named notice.XXXXXX, a regular file (not a link),
# owned by this user, mode 600. Anything else is neither read nor removed;
# the pane then shows a fixed fallback line.

set -uo pipefail

FALLBACK="Scion: see 'herdr plugin log'."

timeout_s="${SCION_NOTICE_TIMEOUT:-30}"
[[ "$timeout_s" =~ ^[0-9]+$ ]] || timeout_s=30

# File mode as octal digits (GNU stat, then BSD stat).
file_mode() {
  stat -c '%a' -- "$1" 2>/dev/null || stat -f '%Lp' -- "$1" 2>/dev/null
}

# Returns 0 when <path> passes every check above.
notice_file_trusted() {
  local f="$1" dir="${HERDR_PLUGIN_STATE_DIR:-}"
  [[ -n "$dir" ]] || dir="${TMPDIR:-/tmp}"
  dir="${dir%/}"
  [[ -n "$f" && -n "$dir" ]] || return 1
  # Directly in the state dir, compared as strings (no link is resolved),
  # with no "." or ".." component anywhere.
  [[ "${f%/*}" == "$dir" ]] || return 1
  case "/${f}/" in
    */./*|*/../*) return 1 ;;
  esac
  [[ "${f##*/}" =~ ^notice\.[A-Za-z0-9]{6}$ ]] || return 1
  [[ -f "$f" && ! -L "$f" && -O "$f" ]] || return 1
  [[ "$(file_mode "$f")" == "600" ]] || return 1
}

f="${SCION_NOTICE_FILE:-}"
text=""
if notice_file_trusted "$f"; then
  # Printable text only: no terminal control sequences. C1 bytes (0x80-0x9f)
  # are not stripped; that is fine, the text is the plugin's fixed text and
  # the checks above hold.
  text="$(head -c 2000 -- "$f" | tr -d '\000-\010\013-\037\177')"
  rm -f -- "$f"
fi
[[ -n "$text" ]] || text="$FALLBACK"

printf '\n  Scion\n\n'
while IFS= read -r line; do
  printf '  %s\n' "$line"
done <<<"$text"
printf '\n  Press Enter to close (closes by itself in %ss).\n' "$timeout_s"

if read -r -t "$timeout_s" _; then :; fi
exit 0
