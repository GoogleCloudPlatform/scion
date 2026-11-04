#!/usr/bin/env bash
# check-areadag: the pkg/hub package-DAG guard (hack/areadag).
#
# WHAT IT CHECKS
# hack/areadag/dag.txt declares the refactor's pkg/hub subpackages as leaf,
# area or testsupport. Against the import metadata of the whole module
# (go list -deps=false -json ./..., metadata only: nothing compiles), it fails
# when:
#   - a declared package imports, in non-test or test code, a pkg/hub
#     subpackage that is neither a declared leaf nor an area listed on its line;
#   - a declared package imports pkg/hub itself;
#   - a leaf lists an area that itself imports pkg/hub packages;
#   - any package of the module imports a testsupport package from non-test
#     code (testsupport packages may import each other); everything at or
#     under a dag.txt testsupport path is testsupport;
#   - any package main reaches a testsupport package through non-test imports;
#   - a pkg/hub zz_alias_*.go file imports an undeclared pkg/hub subpackage.
# It fails closed when an import of a module path names a package neither
# go list nor the walk saw (a symlinked, ., _, testdata or vendor directory).
# Files of every build tag set and platform count: go list IgnoredGoFiles are
# parsed, and the module root is walked for directories go list does not list
# (every file tagged). Undeclared pkg/hub subpackages are not checked
# themselves. The rules are documented in hack/areadag/main.go, the file
# format in hack/areadag/dag/dag.go.
#
# SELF-TEST
#   ./hack/check-areadag.sh --self-test
# runs the fixture tests in hack/areadag (planted violations that must fail,
# clean fixtures that must pass, and vacuous inputs that must not pass).
#
# SEVERITY: guard-grade. A zero input count (no packages, no declarations, no
# testsupport declaration, no alias file, no edge, no package main), a stale
# declaration, a testsupport package main, an unresolved module import or a
# symlinked .go file in a walked directory fails the run.
#
# EXIT CODES
#   0  analysed, no violations
#   1  violations (listed on stdout)
#   3  could not analyse (go missing, go list or build failure, bad input)
set -euo pipefail

cd "$(dirname "$0")/.." || exit 3

if ! command -v go >/dev/null 2>&1; then
  echo "check-areadag: go not found; nothing was analysed" >&2
  exit 3
fi

if [[ "${1:-}" == "--self-test" ]]; then
  exec go test -count=1 ./hack/areadag/...
fi
if [[ $# -ne 0 ]]; then
  echo "usage: $0 [--self-test]" >&2
  exit 3
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

if ! go list -deps=false -json ./... >"$tmp/list.json"; then
  echo "check-areadag: go list failed; nothing was analysed" >&2
  exit 3
fi
if ! go build -buildvcs=false -o "$tmp/areadag" ./hack/areadag; then
  echo "check-areadag: could not compile the checker; nothing was analysed" >&2
  exit 3
fi

set +e
"$tmp/areadag" -dag hack/areadag/dag.txt -list "$tmp/list.json" -root .
rc=$?
set -e
case "$rc" in
  0 | 1) exit "$rc" ;;
  *)
    echo "check-areadag: the checker could not analyse (exit $rc); nothing was analysed" >&2
    exit 3
    ;;
esac
