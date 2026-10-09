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
# Contract: on every uncommented line in scope, the check flags a standalone
# `--debug` token (bare or =true, =t, =1, quoted or not) wherever it appears,
# and any assignment of `debug` to SCION_LOG_LEVEL, SCION_SERVER_LOG_LEVEL,
# SCION_SERVER_LOGLEVEL, log_level or logLevel, or of a non-empty value to
# SCION_DEBUG (including shell defaults and the Dockerfile `ENV KEY value`
# form). It is deliberately fail-closed: prose that names the
# flag is flagged too. Intentional mentions go in the commented ALLOWLIST below.
#
# Details. Matching is case-insensitive. A line is a comment when its first
# non-blank character is '#'. `--debug` must be preceded by start of line or
# any character other than a letter, digit, `_`, `-` or backtick, and
# followed by end of line or any character other than a letter, digit, `_`,
# `=` or `-`. So shell forms such as `--debug;`, `$(... --debug)`, `--debug&`,
# `--debug|tee` and `FLAGS=--debug` are flagged, and so is a backticked
# command such as `scion server start --debug` or `<code>--debug</code>`;
# `--debug=false`, `--debug-port` and `--debugx` pass, and so does a flag
# alone in markdown backticks (`` `--debug` ``). A value of `debug` must be followed
# by end of line or a character that is not a letter, digit, `_` or `-`, so
# `debug_off` and `debugx` pass. Shell defaults covered:
# ${SCION_LOG_LEVEL:-debug}, ${SCION_LOG_LEVEL:=debug}, and ${SCION_DEBUG:-1}
# or ${SCION_DEBUG:=1} with any non-empty default.
#
# Intentional mentions (for example, docs that explain how to turn debug on
# temporarily) go in the ALLOWLIST array below, anchored on file path plus a
# pattern for the line text, never on line numbers. An ALLOWLIST entry that
# matches nothing is reported as a violation, so stale entries are removed and
# every run proves the scan reached the allowlisted files.
#
# LIMITATIONS
# The check is textual and line-oriented. It does not detect:
#   - a key and value held in separate fields, on one line or split across
#     lines, such as a Kubernetes env entry (`name: SCION_DEBUG` /
#     `value: "1"`, or `{name: SCION_LOG_LEVEL, value: debug}`), a JSON
#     name/value pair, or an HCL env block (`name = "SCION_LOG_LEVEL"` /
#     `value = "debug"`)
#   - a value supplied through a variable (`SCION_DEBUG=$X`, `${X}`)
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
  # Chart template comments ({{/* ... */}}) that describe the server flag set;
  # they are not rendered and do not set the flag.
  "deploy/helm/scion-hub/templates/_helpers.tpl::--debug is on, no log line either"
  "deploy/helm/scion-hub/templates/_helpers.tpl::^  --debug \\(cmd/server\\.go, init\\)\\. Logging verbosity"
  "deploy/helm/scion-hub/templates/_helpers.tpl::^    --debug registered in cmd/root\\.go"
)

# --- Patterns (POSIX ERE, matched case-insensitively) ---
# Do not use \b, \| or other GNU extensions: BSD grep would silently match
# nothing and the check would report a false clean. A literal `]` is written
# outside any bracket expression (where it is ordinary in ERE), and `$` and `{`
# as the bracket expressions [$] and [{].
# The key rules do not start inside ${...}: a `{` before the key hands those
# forms to the shell-default rules, so ${SCION_DEBUG:-} (empty) passes.
q="[\"']"                                 # optional quote
dflt="([$][{][A-Za-z0-9_]+:?[-=])?"        # optional shell default ${VAR:-...
lvl="${q}?${dflt}debug(\$|[^A-Za-z0-9_-])" # the value debug, then a boundary
set1="([^\"'[:space:]$}]|[$][{][A-Za-z0-9_]+:?[-=][^\"'[:space:]}])" # non-empty
PATTERNS=(
  # The --debug token.
  # Delimiters are complement classes: left is start of line or any character
  # other than a letter, digit, _, - or backtick; right is end of line or any
  # character other than a letter, digit, _, = or -. (\` is a literal backtick.)
  "(^|[^A-Za-z0-9_\`-])--debug(=${q}?(true|t|1)${q}?)?(\$|[^A-Za-z0-9_=-])"
  # SCION_LOG_LEVEL / SCION_SERVER_LOG_LEVEL / SCION_SERVER_LOGLEVEL /
  # log_level / logLevel = or : debug.
  "(^|[^A-Za-z0-9_{])(SCION_LOG_LEVEL|SCION_SERVER_LOG_?LEVEL|log_?level)${q}?[[:space:]]*[=:][[:space:]]*${lvl}"
  # Shell default for the level: ${SCION_LOG_LEVEL:-debug}, ${SCION_LOG_LEVEL:=debug}.
  "[$][{]SCION_LOG_LEVEL:?[-=]${lvl}"
  # SCION_DEBUG = or : a non-empty value (also via a non-empty shell default).
  "(^|[^A-Za-z0-9_{])SCION_DEBUG${q}?[[:space:]]*[=:][[:space:]]*${q}?${set1}"
  # Shell default for SCION_DEBUG: ${SCION_DEBUG:-1}, ${SCION_DEBUG:=1}.
  "[$][{]SCION_DEBUG:?[-=][^\"'[:space:]}]"
  # Dockerfile ENV with a space separator: ENV SCION_LOG_LEVEL debug.
  "^[[:space:]]*ENV[[:space:]]+(SCION_LOG_LEVEL[[:space:]]+${lvl}|SCION_DEBUG[[:space:]]+${q}?${set1})"
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
  local fx="$WORK/fx" out rc expected got bad_lines i
  mkdir -p "$fx/scripts/starter-hub" "$fx/deploy/helm/chart/ci" \
    "$fx/deploy/helm/chart/templates/ci" "$fx/docs-site/src/content/docs/hosted"

  # Every line of bad.sh must be flagged; no line of good.sh may be.
  cat >"$fx/scripts/starter-hub/bad.sh" <<'EOF'
ExecStart=/usr/local/bin/scion server start --foreground --debug --enable-hub
ExecStart=/usr/local/bin/scion --global --debug server start --foreground
  --debug \
  --debug=true \
  - --debug
scion server start --debug=true --enable-hub
args: ["--debug"]
args = ["server", "start", "--debug"]
command: [scion, server, start, --debug]
args: ['--debug=true', '--enable-hub']
  --foreground --production --debug \
  --foreground --debug
  "--debug",
scion server  start --debug
The --debug flag turns on debug logging.
SCION_LOG_LEVEL=debug
export SCION_LOG_LEVEL="debug"
SCION_DEBUG=1
    log_level: debug
logLevel: debug
log_level = "debug"
SCION_LOG_LEVEL=${SCION_LOG_LEVEL:-debug}
: "${SCION_LOG_LEVEL:=debug}"
SCION_DEBUG=${SCION_DEBUG:-1}
: "${SCION_DEBUG:=1}"
ENV SCION_LOG_LEVEL debug
ENV SCION_DEBUG 1
scion server start --debug;
(scion server start --debug)
out=$(scion server start --debug)
scion server start --debug&
scion server start --debug|tee log
DEBUG_FLAG=--debug
Environment=SCION_FLAGS=--debug
Run `scion server start --debug` to troubleshoot.
Pass <code>--debug</code> to the server.
scion server start --debug="true"
scion server start --debug='1'
SCION_SERVER_LOGLEVEL=debug
SCION_SERVER_LOG_LEVEL=debug
EOF
  # A tab between the words (heredoc tabs are easy to lose in an edit).
  printf 'scion server\tstart --debug\n' >>"$fx/scripts/starter-hub/bad.sh"
  cat >"$fx/scripts/starter-hub/good.sh" <<'EOF'
# SCION_LOG_LEVEL=debug
  # scion server start --debug
ExecStart=/usr/local/bin/scion server start --foreground --enable-hub
scion server start --debug=false --enable-hub
scion --debug=false server start
  --debug=false \
  --foreground --debug=false \
scion server start --debug-port 9000
scion server start --debugx
args: ["--debug=false"]
args: ["--debug-port", "9000"]
args: []
SCION_LOG_LEVEL=info
SCION_LOG_LEVEL=${SCION_LOG_LEVEL:-info}
SCION_LOG_LEVEL: debugx
log_level: debug_off
log_level: info
logLevel: "info"
SCION_DEBUG=
SCION_DEBUG=""
SCION_DEBUG=${SCION_DEBUG:-}
SCION_DEBUG_EXTRA=1
ENV SCION_LOG_LEVEL info
scion server start --debug="false"
SCION_SERVER_LOGLEVEL=info
EOF
  # Pruned: top-level ci/ of a chart.
  printf 'log_level: debug\n' >"$fx/deploy/helm/chart/ci/values.yaml"
  # Not pruned: a deeper directory that happens to be named ci/.
  printf 'SCION_LOG_LEVEL=debug\n' >"$fx/deploy/helm/chart/templates/ci/env.yaml"
  # Ignored: docs are scanned only for *.md and *.mdx.
  printf 'SCION_LOG_LEVEL=debug\n' >"$fx/docs-site/src/content/docs/hosted/notes.txt"
  # Line 1 is allowlisted; line 2 is a near miss in the same file; line 3
  # names the flag in backticks only, which is not a token match.
  # shellcheck disable=SC2016  # backticks are literal markdown fixture text
  printf '%s\n' 'To troubleshoot, set `SCION_LOG_LEVEL=debug` temporarily.' \
    'SCION_LOG_LEVEL=debug' 'Use `--debug` only while troubleshooting.' \
    >"$fx/docs-site/src/content/docs/hosted/page.md"
  bad_lines="$(wc -l <"$fx/scripts/starter-hub/bad.sh" | tr -d ' ')"

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
stale-allowlist:docs-site/src/content/docs/hosted/page.md"
  i=1
  while [[ "$i" -le "$bad_lines" ]]; do
    expected="${expected}
scripts/starter-hub/bad.sh:${i}"
    i=$((i + 1))
  done
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
  if [[ "$rc" -ne 1 ]] || [[ "$got" -ne $((bad_lines + 2)) ]] || printf '%s\n' "$out" | grep -q '^stale-allowlist:'; then
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

  echo "$NAME --self-test: ok ($((bad_lines + 2)) violations and 1 stale allowlist entry flagged; clean, commented, pruned and non-doc lines ignored; empty allowlist works; missing root exits 4)"
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
