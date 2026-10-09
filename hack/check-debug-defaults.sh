#!/usr/bin/env bash
# Checks that setup scripts, deployment templates and production setup docs
# do not turn on debug logging by default. Debug is opt-in: an operator turns
# it on for a troubleshooting session and turns it off again afterwards.
#
# Scope (SCAN_ROOTS below):
#   scripts/starter-hub/, scripts/single-node/, scripts/single-node-vm/,
#   scripts/cloudrun/   -- setup scripts, unit files and config templates
#   deploy/             -- deployment templates and default values
#   docs-site/src/content/docs/hosted/ (*.md, *.mdx) -- hosted setup docs
#
# Excluded from scope (test-only, never deployed):
#   deploy/helm/*/ci/      helm CI values fixtures (exercise log_level: debug)
#   deploy/helm/*/golden/  expected render output of those CI fixtures
#   deploy/helm/*/tests/   chart test scripts
#   deploy/helm/*/hack/    chart verification scripts
#
# A line is flagged when it is not a comment (first non-blank character is
# not '#') and matches one of:
#   - `server start ... --debug` or `runtime-broker start ... --debug`
#   - a command continuation line that starts with `--debug` (followed by
#     nothing, a trailing `\` or further flags), or a YAML list item
#     `- --debug`; prose that merely names the flag is not flagged
#   - SCION_LOG_LEVEL set to debug (`=debug`, `: debug`, quoted or not)
#   - SCION_DEBUG set to a non-empty value
#   - `log_level: debug`
#
# Intentional mentions (for example, docs that explain how to turn debug on
# temporarily) go in the ALLOWLIST array below, anchored on file path plus a
# pattern for the line text, never on line numbers.
#
# LIMITATIONS
# The check is textual and line-oriented. A value split across lines (for
# example a Kubernetes env entry with `name: SCION_DEBUG` and `value: "1"` on
# separate lines) or supplied through a variable is not detected.
#
# Usage:
#   hack/check-debug-defaults.sh              scan the repository
#   hack/check-debug-defaults.sh --self-test  run the built-in fixture test
#
# Severity: FORMATTING-GRADE (see hack/LINT-CONVENTIONS.md)
#   0  analysed, no violations
#   1  analysed, violations found (listed on stderr)
#   4  could not analyse: a scan root is missing
#
# Uses only bash 3.2-compatible syntax and POSIX find/grep, so it runs with
# the system bash and BSD tools on macOS.
set -euo pipefail

cd "$(dirname "$0")/.."

NAME="check-debug-defaults"

SCAN_ROOTS=(
  scripts/starter-hub
  scripts/single-node
  scripts/single-node-vm
  scripts/cloudrun
  deploy
  docs-site/src/content/docs/hosted
)

# Allowlist: "path::ERE matched against the line text". Every entry needs a
# comment saying why the mention is intentional.
ALLOWLIST=(
  # Explains how to turn debug on temporarily for troubleshooting.
  "docs-site/src/content/docs/hosted/single-node/hub-server.md::turn it on temporarily"
  # Log-level reference: explains how to get debug-level Hub logs.
  "docs-site/src/content/docs/hosted/single-node/observability.md::To get DEBUG-level Hub logs"
)

# --- Patterns (POSIX ERE, matched case-insensitively) ---
# Do not use \b, \| or other GNU extensions: BSD grep would silently match
# nothing and the check would report a false clean.
q="[\"']"
PATTERNS=(
  "(server|runtime-broker) start.*--debug"
  "^[[:space:]]*(-[[:space:]]+)?${q}?--debug${q}?([[:space:]]*(\\\\|--.*))?$"
  "SCION_LOG_LEVEL${q}?[[:space:]]*[=:][[:space:]]*${q}?debug"
  "SCION_DEBUG${q}?[[:space:]]*[=:][[:space:]]*${q}?[^\"'[:space:]]"
  "(^|[^A-Za-z_])log_level${q}?[[:space:]]*:[[:space:]]*${q}?debug"
)

# scan <root>... : print "path:line:text" for every uncommented match.
scan() {
  local args=() p
  for p in "${PATTERNS[@]}"; do
    args+=(-e "$p")
  done
  find "$@" \
    \( -path 'deploy/helm/*/ci' -o -path 'deploy/helm/*/golden' \
       -o -path 'deploy/helm/*/tests' -o -path 'deploy/helm/*/hack' \) -prune \
    -o -type f \
    \( -path 'docs-site/*' \( -name '*.md' -o -name '*.mdx' \) -o ! -path 'docs-site/*' \) \
    -exec grep -HnIiE "${args[@]}" {} + 2>/dev/null \
    | grep -Ev '^[^:]*:[0-9]+:[[:space:]]*#' || true
}

# allowed <match-line> : succeed when the match is covered by ALLOWLIST.
allowed() {
  local match="$1" file text entry
  file="${match%%:*}"
  text="${match#*:}"
  text="${text#*:}"
  for entry in "${ALLOWLIST[@]}"; do
    if [[ "$file" == "${entry%%::*}" ]] && printf '%s\n' "$text" | grep -Eq -e "${entry#*::}"; then
      return 0
    fi
  done
  return 1
}

# violations <root>... : scan and drop allowlisted matches.
violations() {
  local line
  scan "$@" | while IFS= read -r line; do
    if ! allowed "$line"; then
      printf '%s\n' "$line"
    fi
  done
}

self_test() {
  local dir out count
  dir="$(mktemp -d)"
  # shellcheck disable=SC2064  # expand $dir now
  trap "rm -rf '$dir'" EXIT
  mkdir -p "$dir/fx"
  cat >"$dir/fx/bad.sh" <<'EOF'
ExecStart=/usr/local/bin/scion server start --foreground --debug --enable-hub
exec scion server start \
  --debug \
  --enable-hub
SCION_LOG_LEVEL=debug
export SCION_LOG_LEVEL="debug"
SCION_DEBUG=1
    log_level: debug
args:
  - --debug
EOF
  cat >"$dir/fx/good.sh" <<'EOF'
# SCION_LOG_LEVEL=debug
  # scion server start --debug
ExecStart=/usr/local/bin/scion server start --foreground --enable-hub
SCION_LOG_LEVEL=info
SCION_DEBUG=
SCION_DEBUG=""
SCION_DEBUG_EXTRA=1
log_level: info
The --debug flag turns on debug logging.
  --debug is on, no log line either.
EOF
  out="$(violations "$dir/fx")"
  count=0
  if [[ -n "$out" ]]; then
    count="$(printf '%s\n' "$out" | wc -l | tr -d ' ')"
  fi
  if printf '%s\n' "$out" | grep -q 'good\.sh'; then
    echo "$NAME --self-test: FAIL, clean fixture lines were flagged:" >&2
    printf '%s\n' "$out" | grep 'good\.sh' >&2
    exit 1
  fi
  if [[ "$count" -ne 7 ]]; then
    echo "$NAME --self-test: FAIL, expected 7 violations, got ${count}:" >&2
    printf '%s\n' "$out" >&2
    exit 1
  fi
  echo "$NAME --self-test: ok (7 violations flagged, clean lines ignored)"
}

if [[ "${1:-}" == "--self-test" ]]; then
  self_test
  exit 0
fi

# --- Provenance ---
sha="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
if [[ "$sha" != "unknown" && -n "$(git status --porcelain 2>/dev/null)" ]]; then
  sha="${sha}-dirty"
fi

# --- Scan roots must exist, so a rename cannot silently shrink the scan ---
for root in "${SCAN_ROOTS[@]}"; do
  if [[ ! -d "$root" ]]; then
    echo "$NAME: scan root '$root' is missing; NOTHING WAS ANALYSED" >&2
    exit 4
  fi
done

found="$(violations "${SCAN_ROOTS[@]}")"

if [[ -n "$found" ]]; then
  echo "$NAME: analysed ${sha}, violations found" >&2
  echo "" >&2
  echo "Debug logging is enabled in a setup script, template or production example:" >&2
  printf '%s\n' "$found" >&2
  echo "" >&2
  echo "Debug must be off by default. Remove the setting, or, for an intentional" >&2
  echo "mention (such as docs on turning debug on temporarily), add an entry to" >&2
  echo "ALLOWLIST in hack/check-debug-defaults.sh with a comment." >&2
  exit 1
fi

echo "$NAME: analysed ${sha}, no debug-by-default settings found"
