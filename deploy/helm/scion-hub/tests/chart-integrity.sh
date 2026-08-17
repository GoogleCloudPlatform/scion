#!/usr/bin/env bash
#
# chart-integrity.sh -- the positive twin for .helmignore breadth.
#
# WHY THIS EXISTS.
#
# `.helmignore` is applied when Helm loads a chart DIRECTORY, not only when it packages one.
# An over-broad pattern therefore silently removes files from every `helm template`, `helm lint`
# and `helm package` invocation at once. Measured at 721fc77:
#
#   ignore templates/service.yaml -> `helm template` catches it (5 kinds -> 4)
#                                    `helm lint --strict` is BLIND (0 chart(s) failed)
#   ignore values.schema.json     -> `helm template` is BLIND (still 5 kinds, byte-identical)
#                                    `helm lint --strict` is BLIND
#
# The second row is why this file exists. Deleting the values contract makes the chart accept
# MORE and render IDENTICALLY, so the guard's removal is invisible to the guard's own success
# criterion. Every positive signal stays green while the protection is gone.
#
# The measurement this replaces was "helm package emits 0 files under tests/" -- a bare negative.
# It says what is absent and nothing about what survived. This script asserts what survived.
#
# CONTRACT (shared with reserved-flags.sh and update-strategy.sh):
#   exit 0 -- all EXPECTED_TOTAL assertions ran and passed
#   exit 1 -- an assertion ran and failed
#   exit 2 -- fewer than EXPECTED_TOTAL assertions ran. A SHORT RUN IS A FAILED RUN.
# Rule 9: assert the presence of N successes, never the absence of a failure.

set -u -o pipefail

HELM="${HELM:-helm}"
CHART="${CHART:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"

# Minimum values that make the chart render. Both are `required` with no default by design.
BASE=(--set image.repository=example.invalid/scion-hub --set hub.hubId=h)

EXPECTED_TOTAL=25
executed=0
failed=0

pass() { executed=$((executed+1)); echo "ok    $1"; }
fail() { executed=$((executed+1)); failed=$((failed+1)); echo "FAIL  $1"; }

# ---------------------------------------------------------------------------
# A. The values contract is present AND enforcing.  (3 assertions)
#
# Asserting the *error text* rather than merely a non-zero exit is deliberate: a `fail` in
# _helpers.tpl also exits non-zero, so "it was rejected" alone does not prove the schema is
# what rejected it. This is the difference between testing the guard and testing any guard.
# ---------------------------------------------------------------------------

schema_rejects() { # $1 = --set expr, $2 = expected path in the schema error
  local out
  out="$("$HELM" template t "$CHART" "${BASE[@]}" --set "$1" 2>&1)"
  if printf '%s' "$out" | grep -q "Additional property .* is not allowed" \
     && printf '%s' "$out" | grep -q "^- $2: Additional property"; then
    pass "schema rejects unknown key ($1) at '$2'"
  else
    fail "schema did NOT reject unknown key ($1) at '$2' -- values.schema.json missing or not enforcing"
  fi
}

schema_rejects "bogusKeyNotInSchema=1" "(root)"
schema_rejects "hub.bogusNested=1"     "hub"

# POSITIVE TWIN. Without this, a schema that rejects EVERYTHING passes both cases above.
if "$HELM" template t "$CHART" "${BASE[@]}" >/dev/null 2>&1; then
  pass "positive twin: valid values still render"
else
  fail "positive twin: valid values were REJECTED -- schema is over-strict, not merely present"
fi

# ---------------------------------------------------------------------------
# B. The rendered manifest set is intact.  (6 assertions)
# Catches an over-broad pattern that reaches templates/.
# ---------------------------------------------------------------------------

RENDER="$("$HELM" template t "$CHART" "${BASE[@]}" 2>/dev/null)" || RENDER=""

for k in Deployment Role RoleBinding Service ServiceAccount; do
  if printf '%s\n' "$RENDER" | grep -qx "kind: $k"; then
    pass "render contains kind: $k"
  else
    fail "render is MISSING kind: $k -- template dropped (check .helmignore breadth)"
  fi
done

kinds="$(printf '%s\n' "$RENDER" | grep -c '^kind:')"
if [ "$kinds" -eq 5 ]; then
  pass "render emits exactly 5 manifests"
else
  fail "render emits ${kinds} manifests, expected 5"
fi

# ---------------------------------------------------------------------------
# C. The packaged chart carries what it must.  (16 assertions)
#
# Separate from B because `helm package` and `helm template` do not fail together:
# values.schema.json is absent from B's signal entirely, and VALIDATION.md is absent from
# both unless asserted here.
# ---------------------------------------------------------------------------

EXPECTED_FILES=(
  scion-hub/.helmignore
  scion-hub/Chart.yaml
  scion-hub/VALIDATION.md
  scion-hub/values.schema.json
  scion-hub/values.yaml
  scion-hub/templates/NOTES.txt
  scion-hub/templates/_helpers.tpl
  scion-hub/templates/deployment.yaml
  scion-hub/templates/rbac-clusterrole.yaml
  scion-hub/templates/rbac-clusterrolebinding.yaml
  scion-hub/templates/rbac-role.yaml
  scion-hub/templates/rbac-rolebinding.yaml
  scion-hub/templates/service.yaml
  scion-hub/templates/serviceaccount.yaml
)

pkgdir="$(mktemp -d)"
trap 'rm -rf "$pkgdir"' EXIT

if "$HELM" package "$CHART" -d "$pkgdir" >/dev/null 2>&1; then
  listing="$(tar tzf "$pkgdir"/*.tgz | grep -v '/$' | sort)"
else
  listing=""
fi

for f in "${EXPECTED_FILES[@]}"; do
  if printf '%s\n' "$listing" | grep -qx "$f"; then
    pass "package contains $f"
  else
    fail "package is MISSING $f -- .helmignore is too broad, or packaging failed"
  fi
done

# ci/ is ignored by design (fixture values must never be mistaken for defaults inside a
# packaged chart) and tests/ is ignored because helm package does not preserve the
# executable bit. Both are negatives; each is only meaningful beside the 14 positives above.
if printf '%s\n' "$listing" | grep -q '^scion-hub/\(ci\|tests\)/'; then
  fail "package contains ci/ or tests/ -- these are ignored by design"
else
  pass "package excludes ci/ and tests/"
fi

count="$(printf '%s\n' "$listing" | grep -c '^scion-hub/')"
if [ "$count" -eq "${#EXPECTED_FILES[@]}" ]; then
  pass "package contains exactly ${#EXPECTED_FILES[@]} files"
else
  fail "package contains ${count} files, expected ${#EXPECTED_FILES[@]} -- update EXPECTED_FILES deliberately, do not just bump the number"
fi

# ---------------------------------------------------------------------------
# Fail closed.
# ---------------------------------------------------------------------------

if [ "$executed" -ne "$EXPECTED_TOTAL" ]; then
  # INEQUALITY, NOT A FLOOR. A short run is a failed run; a LONG run means
  # assertions were added without committing the number, which is the same
  # defect facing the other way. Where a check counts anything, the number is
  # committed and both directions fail.
  echo "HARNESS ERROR: executed ${executed} assertions, expected exactly ${EXPECTED_TOTAL}."
  exit 2
fi

if [ "$failed" -ne 0 ]; then
  echo "FAILED ${failed}/${executed}"
  exit 1
fi

echo "PASS ${executed}/${EXPECTED_TOTAL}"
