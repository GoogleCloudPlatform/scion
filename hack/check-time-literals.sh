#!/usr/bin/env bash
# check-time-literals: regression gate for server-side time handling
# (`make time-literals`).
#
# WHAT IT CHECKS
# Over pkg/hub (including githubapp), pkg/store, pkg/runtimebroker,
# pkg/hubsync, pkg/sciontool/hub and pkg/runtime/cloudrun (non-test,
# non-generated Go files):
#   format-utc          no time is formatted to a string without .UTC() first;
#   ent-bind-formatted  no raw SQL on an ent table, and no ent sql predicate,
#                       binds a formatted time string (ent columns hold
#                       time.Time values; text sorts differently);
#   webchat-bind-time   no raw SQL on a webchat_* table binds a time.Time in
#                       the SQLite store (those columns are RFC3339Nano TEXT).
# The analysis is go/ast based; the rules, heuristics and known blind spots
# are documented in hack/checktimeliterals/main.go. cmd/ display code is out
# of scope: it formats in the local zone by design.
#
# ALLOWLIST
# hack/time-literals-allowlist.txt, one entry per exception, anchored on file,
# function and finding text, each with a one-line justification. A stale entry
# fails the gate.
#
# SELF-TEST
#   ./hack/check-time-literals.sh --self-test
# runs the fixture tests in hack/checktimeliterals (positive and negative
# cases under hack/checktimeliterals/testdata).
#
# SEVERITY: correctness-grade. A run that analysed nothing fails the build.
#
# EXIT CODES
#   0  analysed, no violations
#   1  violations, or a malformed or stale allowlist entry (list on stderr)
#   3  could not analyse (go missing, build failure, parse error)
#   4  no candidate files (wrong working directory)
set -euo pipefail

cd "$(dirname "$0")/.." || exit 3

if ! command -v go >/dev/null 2>&1; then
  echo "check-time-literals: go not found; nothing was analysed" >&2
  exit 3
fi

if [[ "${1:-}" == "--self-test" ]]; then
  exec go test -count=1 ./hack/checktimeliterals/
fi

sha="$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")"
if [[ "$sha" != "unknown" && -n "$(git status --porcelain 2>/dev/null)" ]]; then
  sha="${sha}-dirty"
fi
echo "check-time-literals: analysing ${sha}" >&2

tmpbin="$(mktemp)"
trap 'rm -f "$tmpbin"' EXIT

if ! go build -buildvcs=false -o "$tmpbin" ./hack/checktimeliterals 2>&1; then
  echo "check-time-literals: could not compile the checker; nothing was analysed" >&2
  exit 3
fi

exec "$tmpbin" "$@"
