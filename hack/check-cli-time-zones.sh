#!/usr/bin/env bash
# check-cli-time-zones: every absolute time layout in CLI code carries a zone
# (`make cli-time-zones`).
#
# WHAT IT CHECKS
# Over cmd/ (recursively) and pkg/agent/list.go (non-test Go files):
#   zoneless-layout  no string literal that is a Go time layout (it contains
#                    2006, 15:04, 03:04 or 3:04) lacks a zone token (MST,
#                    Z07:00, Z0700, Z07, -07:00, -0700, -07, ...);
#   zoneless-const   no reference to a zoneless time package layout constant
#                    (ANSIC, Kitchen, Stamp*, DateTime, DateOnly, TimeOnly).
# The layout argument of time.Parse / time.ParseInLocation is exempt (it
# describes input). CLI output formats times with pkg/clitime, which converts
# to the display zone (local, or --tz/--utc) and always prints the zone. The
# analysis is go/ast based; rules and blind spots are documented in
# hack/checkclitimezones/main.go.
#
# ALLOWLIST
# None. A zoneless display layout has no legitimate use in CLI output; parse
# layouts are exempt by construction.
#
# SELF-TEST
#   ./hack/check-cli-time-zones.sh --self-test
# runs the fixture tests in hack/checkclitimezones (positive and negative
# cases under hack/checkclitimezones/testdata).
#
# SEVERITY: correctness-grade. A run that analysed nothing fails the build.
#
# EXIT CODES
#   0  analysed, no violations
#   1  violations (list on stderr)
#   3  could not analyse (go missing, build failure, parse error)
#   4  no candidate files (wrong working directory)
set -euo pipefail

cd "$(dirname "$0")/.." || exit 3

if ! command -v go >/dev/null 2>&1; then
  echo "check-cli-time-zones: go not found; nothing was analysed" >&2
  exit 3
fi

if [[ "${1:-}" == "--self-test" ]]; then
  exec go test -count=1 ./hack/checkclitimezones/
fi

sha="$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")"
if [[ "$sha" != "unknown" && -n "$(git status --porcelain 2>/dev/null)" ]]; then
  sha="${sha}-dirty"
fi
echo "check-cli-time-zones: analysing ${sha}" >&2

tmpbin="$(mktemp)"
trap 'rm -f "$tmpbin"' EXIT

if ! go build -buildvcs=false -o "$tmpbin" ./hack/checkclitimezones 2>&1; then
  echo "check-cli-time-zones: could not compile the checker; nothing was analysed" >&2
  exit 3
fi

# Not exec: the EXIT trap must still remove the temporary binary.
set +e
"$tmpbin" "$@"
rc=$?
set -e
exit "$rc"
