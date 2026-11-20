#!/usr/bin/env bash
# check-raw-sql-predicates: security-grade gate for raw SQL predicates in Ent
# (`make check-raw-sql-predicates`).
#
# WHAT IT CHECKS
# Over pkg, cmd, and internal (non-test, non-generated Go files):
#   top-level-or   no entsql.P(...) or entsql.ExprP(...) predicate builder
#                  emits a top-level OR without enclosing outer parentheses.
#
# Ent's Selector.Where joins predicates with AND without wrapping raw
# predicates in parentheses. An unparenthesized top-level OR overrides
# surrounding WHERE predicates (such as access-scoping predicates; see
# ptone/scion#8531).
#
# ALLOWLIST
# hack/raw-sql-predicates-allowlist.txt, anchored on file | func | rule with a
# one-line justification. A malformed or stale entry fails the gate.
#
# LIMITATIONS
# Syntactic (go/ast) analysis. Unmodeled shapes (silent negatives): local
# variables, closures inside a builder body, zero-arg entsql.P().Append(...),
# b.Join(entsql.Expr(...)), defer/go/return/labeled/select/type-switch
# statements in a builder closure, method helpers taking *entsql.Builder, and
# cross-package helpers.
# Postgres E'...\'' escape strings and $$...$$ dollar-quoted strings are not
# lexed and may produce false positives (fail-closed; cover with the allowlist).
#
# SELF-TEST
#   ./hack/check-raw-sql-predicates.sh --self-test
# runs the unit tests in hack/checkrawsqlpredicates (positive, negative,
# allowlist, and empty-scan cases).
#
# SEVERITY: security-grade. A run that analysed nothing fails the build.
#
# EXIT CODES
#   0  analysed, no violations
#   1  violations, or a malformed or stale allowlist entry (list on stderr)
#   3  could not analyse (go missing, build failure, parse error)
#   4  no candidate files (wrong working directory)
#   5  usage error (unknown flag)
set -euo pipefail

cd "$(dirname "$0")/.." || exit 3

# shellcheck source=SCRIPTDIR/lib/require-tool.sh
source hack/lib/require-tool.sh
require_tool go check-raw-sql-predicates

if [[ "${1:-}" == "--self-test" ]]; then
  exec go test -buildvcs=false -count=1 ./hack/checkrawsqlpredicates/
fi

sha="$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")"
if [[ "$sha" != "unknown" && -n "$(git status --porcelain 2>/dev/null)" ]]; then
  sha="${sha}-dirty"
fi
echo "check-raw-sql-predicates: analysing ${sha}" >&2

tmpbin="$(mktemp)"
trap 'rm -f "$tmpbin"' EXIT

if ! go build -buildvcs=false -o "$tmpbin" ./hack/checkrawsqlpredicates 2>&1; then
  echo "check-raw-sql-predicates: could not compile the checker; nothing was analysed" >&2
  exit 3
fi

exec "$tmpbin" "$@"
