#!/usr/bin/env bash
# Flags direct (*os.Root).Open, (*os.Root).OpenFile, (*os.Root).Create, and
# os.OpenInRoot uses outside an explicit allowlist.
#
# Use the shared helper in pkg/util/rootopen (OpenAt, OpenRegularForRead,
# OpenFileForWrite, OpenDir, MkdirAll, OpenFileForWriteInDir, UnlinkInDir) so
# relative path opens under an *os.Root enforce no-follow on every component
# and non-blocking leaf opens in the kernel.
#
# The analysis is go/ast based (hack/checkrootopen/main.go) and runs across
# all non-test .go files under pkg/ and cmd/, including indirect *os.Root
# producers (such as projectfs.Resolved.Open), import aliases of "os",
# method values, method expressions, and chained calls.
#
# Every justified exception is listed in hack/root-open-allowlist.txt as:
#   <file> | <function> | <count> | <reason>
#
# Severity: SECURITY-GRADE (root open confinement)
#   Missing go / build / parse error: exit 3 (nothing analysed)
#   Scan root or allowlist missing:   exit 4
#   No call found:                    exit 4 (the helper itself always opens ".")
#   Violations / stale allowlist:     exit 1
#   Clean:                            exit 0
#
# Usage: hack/check-root-open.sh [--self-test]
#
# See hack/LINT-CONVENTIONS.md for the conventions this script follows.
set -euo pipefail

cd "$(dirname "$0")/.." || exit 3

# shellcheck source=SCRIPTDIR/lib/require-tool.sh
source hack/lib/require-tool.sh
require_tool go "check-root-open" "Go toolchain (go)"

if [[ "${1:-}" == "--self-test" ]]; then
  exec go test -buildvcs=false -count=1 ./hack/checkrootopen/
fi

sha="$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")"
if [[ "$sha" != "unknown" ]] && [[ -n "$(git status --porcelain 2>/dev/null)" ]]; then
  sha="${sha}-dirty"
fi

tmpbin="$(mktemp)"
trap 'rm -f "$tmpbin"' EXIT

if ! go build -buildvcs=false -o "$tmpbin" ./hack/checkrootopen 2>&1; then
  echo "check-root-open: could not compile the checker; nothing was analysed" >&2
  exit 3
fi

exec "$tmpbin" -root . -allowlist hack/root-open-allowlist.txt -label "$sha" "$@"
