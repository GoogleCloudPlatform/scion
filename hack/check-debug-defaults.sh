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
# Excluded from scope (test-only, never deployed). Only the top-level
# directory of each chart is excluded; a directory with the same name deeper
# in a chart (for example templates/ci/) is still scanned:
#   deploy/helm/<chart>/ci/      helm CI values fixtures (exercise log_level: debug)
#   deploy/helm/<chart>/golden/  expected render output of those CI fixtures
#   deploy/helm/<chart>/tests/   chart test scripts
#   deploy/helm/<chart>/hack/    chart verification scripts
#
# A line is flagged when it is not a comment (first non-blank character is
# not '#') and matches one of (case-insensitive):
#   - `server start ... --debug` or `runtime-broker start ... --debug`
#   - `--debug ... server start` (the flag given before the subcommand)
#   - a command continuation line that starts with `--debug` (followed by
#     nothing, a trailing `\` or further flags), or a YAML list item
#     `- --debug`; prose that merely names the flag is not flagged
#   - `--debug` as an element of an inline list, quoted or not
#     (`args: ["--debug"]`, `args = ["server", "start", "--debug"]`,
#     `command: [scion, server, start, --debug]`)
#   - SCION_LOG_LEVEL set to debug (`=debug`, `: debug`, quoted or not)
#   - SCION_DEBUG set to a non-empty value
#   - log_level / logLevel set to debug (`log_level: debug`,
#     `logLevel: debug`, `log_level = "debug"`)
# In the flag forms, `--debug=true` (also `=t` and `=1`) counts as `--debug`;
# `--debug=false` is not flagged.
#
# Intentional mentions (for example, docs that explain how to turn debug on
# temporarily) go in the ALLOWLIST array below, anchored on file path plus a
# pattern for the line text, never on line numbers. An ALLOWLIST entry that
# matches nothing is reported as a violation, so stale entries are removed and
# every run proves the scan reached the allowlisted files.
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
#   4  could not analyse: a scan root is missing, no files were found, or
#      find/grep failed
#
# Uses only bash 3.2-compatible syntax, POSIX ERE patterns, and find/grep
# flags supported by both GNU and BSD tools, so it runs with the system bash
# and BSD tools on macOS.
set -euo pipefail

cd "$(dirname "$0")/.."

NAME="check-debug-defaults"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

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
  # Examples of storing an arbitrary agent environment variable with
  # `scion hub env set`; LOG_LEVEL here is not a Hub setting.
  "docs-site/src/content/docs/hosted/user/secrets.md::^scion hub env set .*[[:space:]]LOG_LEVEL=debug$"
)

# --- Patterns (POSIX ERE, matched case-insensitively) ---
# Do not use \b, \| or other GNU extensions: BSD grep would silently match
# nothing and the check would report a false clean.
q="[\"']"                    # optional quote around a key or value
dbg="--debug(=(true|t|1))?"  # the flag, bare or with an explicit true value
PATTERNS=(
  "(server|runtime-broker) start.*${dbg}([^=A-Za-z0-9_-]|$)"
  "${dbg}[[:space:]].*(server|runtime-broker) start"
  "^[[:space:]]*(-[[:space:]]+)?${q}?${dbg}${q}?([[:space:]]*(\\\\|--.*))?$"
  "[[,][[:space:]]*${q}?${dbg}${q}?[[:space:]]*[],]"
  "SCION_LOG_LEVEL${q}?[[:space:]]*[=:][[:space:]]*${q}?debug"
  "SCION_DEBUG${q}?[[:space:]]*[=:][[:space:]]*${q}?[^\"'[:space:]]"
  "(^|[^A-Za-z_])log_?level${q}?[[:space:]]*[:=][[:space:]]*${q}?debug"
)

# analyse: scan SCAN_ROOTS (relative to the current directory), apply
# ALLOWLIST, and print one violation per line on stdout. Returns 0 when
# clean, 1 when violations were printed, 4 when nothing could be analysed.
analyse() {
  local root f line file text entry hit i rc args=() files=() used=" "

  for root in "${SCAN_ROOTS[@]}"; do
    if [[ ! -d "$root" ]]; then
      echo "$NAME: scan root '$root' is missing; NOTHING WAS ANALYSED" >&2
      return 4
    fi
  done

  # Prune each chart's top-level test directories only: in find -path, '*'
  # also matches '/', so the second -path keeps deeper same-named dirs.
  rc=0
  find "${SCAN_ROOTS[@]}" \
    \( \( -path 'deploy/helm/*/ci' ! -path 'deploy/helm/*/*/ci' \) \
       -o \( -path 'deploy/helm/*/golden' ! -path 'deploy/helm/*/*/golden' \) \
       -o \( -path 'deploy/helm/*/tests' ! -path 'deploy/helm/*/*/tests' \) \
       -o \( -path 'deploy/helm/*/hack' ! -path 'deploy/helm/*/*/hack' \) \) -prune \
    -o -type f \
    \( -path 'docs-site/*' \( -name '*.md' -o -name '*.mdx' \) -o ! -path 'docs-site/*' \) \
    -print >"$WORK/files" || rc=$?
  if [[ "$rc" -ne 0 ]]; then
    echo "$NAME: find failed (exit $rc); NOTHING WAS ANALYSED" >&2
    return 4
  fi
  while IFS= read -r f; do
    files+=("$f")
  done <"$WORK/files"
  if [[ "${#files[@]}" -eq 0 ]]; then
    echo "$NAME: no files found under the scan roots; NOTHING WAS ANALYSED" >&2
    return 4
  fi

  for f in "${PATTERNS[@]}"; do
    args+=(-e "$f")
  done
  # grep exits 0 on a match, 1 on no match, and >1 on an error.
  rc=0
  grep -HnIiE "${args[@]}" -- "${files[@]}" >"$WORK/hits" || rc=$?
  if [[ "$rc" -gt 1 ]]; then
    echo "$NAME: grep failed (exit $rc); NOTHING WAS ANALYSED" >&2
    return 4
  fi
  rc=0
  grep -Ev '^[^:]*:[0-9]+:[[:space:]]*#' "$WORK/hits" >"$WORK/uncommented" || rc=$?
  if [[ "$rc" -gt 1 ]]; then
    echo "$NAME: grep failed (exit $rc); NOTHING WAS ANALYSED" >&2
    return 4
  fi

  : >"$WORK/violations"
  # ${ALLOWLIST[@]+"${ALLOWLIST[@]}"} keeps an empty ALLOWLIST from aborting
  # under set -u on bash before 4.4 (macOS ships 3.2).
  while IFS= read -r line; do
    file="${line%%:*}"
    text="${line#*:}"
    text="${text#*:}"
    hit=""
    i=0
    for entry in ${ALLOWLIST[@]+"${ALLOWLIST[@]}"}; do
      if [[ "$file" == "${entry%%::*}" ]] && printf '%s\n' "$text" | grep -Eq -e "${entry#*::}"; then
        hit=1
        used="${used}${i} "
        break
      fi
      i=$((i + 1))
    done
    if [[ -z "$hit" ]]; then
      printf '%s\n' "$line" >>"$WORK/violations"
    fi
  done <"$WORK/uncommented"

  i=0
  for entry in ${ALLOWLIST[@]+"${ALLOWLIST[@]}"}; do
    case "$used" in
      *" $i "*) ;;
      *) printf 'stale-allowlist:%s (matched nothing; remove it or fix its pattern)\n' "$entry" >>"$WORK/violations" ;;
    esac
    i=$((i + 1))
  done

  if [[ -s "$WORK/violations" ]]; then
    cat "$WORK/violations"
    return 1
  fi
  return 0
}

self_test() {
  local fx="$WORK/fx" out rc expected got
  mkdir -p "$fx/scripts/starter-hub" "$fx/deploy/helm/chart/ci" \
    "$fx/deploy/helm/chart/templates/ci" "$fx/docs-site/src/content/docs/hosted"

  cat >"$fx/scripts/starter-hub/bad.sh" <<'EOF'
ExecStart=/usr/local/bin/scion server start --foreground --debug --enable-hub
ExecStart=/usr/local/bin/scion --global --debug server start --foreground
exec scion server start \
  --debug \
  --debug=true \
  --enable-hub
SCION_LOG_LEVEL=debug
export SCION_LOG_LEVEL="debug"
SCION_DEBUG=1
    log_level: debug
args:
  - --debug
logLevel: debug
log_level = "debug"
scion server start --debug=true --enable-hub
args: ["--debug"]
args = ["server", "start", "--debug"]
command: [scion, server, start, --debug]
args: ['--debug=true', '--enable-hub']
EOF
  cat >"$fx/scripts/starter-hub/good.sh" <<'EOF'
# SCION_LOG_LEVEL=debug
  # scion server start --debug
ExecStart=/usr/local/bin/scion server start --foreground --enable-hub
scion server start --debug=false --enable-hub
scion --debug=false server start
  --debug=false \
scion server start --debug-port 9000
SCION_LOG_LEVEL=info
SCION_DEBUG=
SCION_DEBUG=""
SCION_DEBUG_EXTRA=1
log_level: info
logLevel: "info"
The --debug flag turns on debug logging.
  --debug is on, no log line either.
args: ["--debug=false"]
args: ["--debug-port", "9000"]
args: []
EOF
  # Pruned: top-level ci/ of a chart.
  printf 'log_level: debug\n' >"$fx/deploy/helm/chart/ci/values.yaml"
  # Not pruned: a deeper directory that happens to be named ci/.
  printf 'SCION_LOG_LEVEL=debug\n' >"$fx/deploy/helm/chart/templates/ci/env.yaml"
  # Ignored: docs are scanned only for *.md and *.mdx.
  printf 'SCION_LOG_LEVEL=debug\n' >"$fx/docs-site/src/content/docs/hosted/notes.txt"
  # Line 1 is allowlisted; line 2 is a near miss in the same file.
  printf '%s\n' 'To troubleshoot, set `SCION_LOG_LEVEL=debug` temporarily.' \
    'SCION_LOG_LEVEL=debug' >"$fx/docs-site/src/content/docs/hosted/page.md"

  rc=0
  out="$(
    cd "$fx"
    SCAN_ROOTS=(scripts deploy docs-site/src/content/docs/hosted)
    ALLOWLIST=(
      "docs-site/src/content/docs/hosted/page.md::temporarily"
      "docs-site/src/content/docs/hosted/page.md::no line matches this"
    )
    analyse
  )" || rc=$?

  expected="deploy/helm/chart/templates/ci/env.yaml:1
docs-site/src/content/docs/hosted/page.md:2
scripts/starter-hub/bad.sh:1
scripts/starter-hub/bad.sh:10
scripts/starter-hub/bad.sh:12
scripts/starter-hub/bad.sh:13
scripts/starter-hub/bad.sh:14
scripts/starter-hub/bad.sh:15
scripts/starter-hub/bad.sh:16
scripts/starter-hub/bad.sh:17
scripts/starter-hub/bad.sh:18
scripts/starter-hub/bad.sh:19
scripts/starter-hub/bad.sh:2
scripts/starter-hub/bad.sh:4
scripts/starter-hub/bad.sh:5
scripts/starter-hub/bad.sh:7
scripts/starter-hub/bad.sh:8
scripts/starter-hub/bad.sh:9
stale-allowlist:docs-site/src/content/docs/hosted/page.md"
  got="$(printf '%s\n' "$out" | cut -d: -f1,2 | LC_ALL=C sort)"
  expected="$(printf '%s\n' "$expected" | LC_ALL=C sort)"
  if [[ "$rc" -ne 1 || "$got" != "$expected" ]]; then
    echo "$NAME --self-test: FAIL (exit $rc). Expected:" >&2
    printf '%s\n' "$expected" >&2
    echo "Got:" >&2
    printf '%s\n' "$out" >&2
    exit 1
  fi

  # An empty ALLOWLIST must still analyse: the allowlisted docs line is then
  # reported too, and no stale entry exists.
  rc=0
  out="$(
    cd "$fx"
    SCAN_ROOTS=(scripts deploy docs-site/src/content/docs/hosted)
    ALLOWLIST=()
    analyse
  )" || rc=$?
  got="$(printf '%s\n' "$out" | cut -d: -f1,2 | grep -c -e '^docs-site/src/content/docs/hosted/page.md:[12]$' -e '^scripts/starter-hub/bad.sh:' || true)"
  if [[ "$rc" -ne 1 ]] || [[ "$got" -ne 18 ]] || printf '%s\n' "$out" | grep -q '^stale-allowlist:'; then
    echo "$NAME --self-test: FAIL, empty ALLOWLIST gave exit $rc with output:" >&2
    printf '%s\n' "$out" >&2
    exit 1
  fi

  # A missing scan root must report "nothing analysed", not a clean pass.
  rc=0
  (cd "$fx" && SCAN_ROOTS=(no-such-dir) && analyse) >/dev/null 2>&1 || rc=$?
  if [[ "$rc" -ne 4 ]]; then
    echo "$NAME --self-test: FAIL, missing scan root gave exit $rc, expected 4" >&2
    exit 1
  fi

  echo "$NAME --self-test: ok (18 violations and 1 stale allowlist entry flagged; clean, commented, pruned and non-doc lines ignored; empty allowlist works; missing root exits 4)"
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

rc=0
found="$(analyse)" || rc=$?

if [[ "$rc" -eq 1 ]]; then
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
if [[ "$rc" -ne 0 ]]; then
  exit "$rc"
fi

echo "$NAME: analysed ${sha}, no debug-by-default settings found"
