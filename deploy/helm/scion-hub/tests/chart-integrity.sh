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
# Provenance: written by gd-p0-rev-2, adopted here whole. The 25 assertions and
# their messages are rev-2's; the changes made on adoption are the tool-presence
# arm, the ASSERTIONS_EXECUTED line for run-all.sh, and the inequality below.
#
# CONTRACT (shared with reserved-flags.sh and update-strategy.sh):
#   exit 0 -- all EXPECTED_TOTAL assertions ran and passed
#   exit 1 -- an assertion ran and failed
#   exit 2 -- the number of assertions executed was not EXACTLY EXPECTED_TOTAL,
#             or a required tool was absent so NOTHING WAS ANALYSED. Short and
#             long are both failures: a long run means assertions were added
#             without the number being committed in the same diff.
# Rule 9: assert the presence of N successes, never the absence of a failure.

set -u -o pipefail

HELM="${HELM:-helm}"
CHART="${CHART:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"

# Minimum values that make the chart render. Both are `required` with no default by design.
BASE=(--set image.repository=example.invalid/scion-hub --set hub.hubId=h)

EXPECTED_TOTAL=26   # 25 as adopted from rev-2, +1 for the base-url tripwire in D.
# TOOL-PRESENCE ARM. A MISSING TOOLCHAIN MUST NOT BE REPORTED AS A BROKEN CHART.
# Without this every helm invocation fails, every assertion fails, and the output
# accuses the chart of dropping templates when the truth is that helm is not
# installed. Found by the first person to run this suite who was not its author,
# in a container without helm, in four minutes. A mutation suite inherits its
# author's environment, so the environment is the one variable it cannot mutate
# from the inside - the same shape as axis (d), answerable only from outside.
# "Nothing was analysed" is a THIRD outcome, distinct from clean and from failing,
# and it exits 2 with the other harness errors rather than 1.
_missing=""
for _t in "$HELM" tar mktemp; do command -v "$_t" >/dev/null 2>&1 || _missing="${_missing} ${_t}"; done
if [ -n "$_missing" ]; then
  echo "HARNESS ERROR: required tool(s) not on PATH:${_missing}"
  echo "NOTHING WAS ANALYSED. This is not a passing run, and it is NOT a chart failure."
  echo "ASSERTIONS_EXECUTED=0"
  exit 2
fi

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
# C. The packaged chart carries what it must.  (16 assertions; D adds 1 more)
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
# 🔴 THE EMPTY GUARD COMES FIRST AND IS THE REASON THIS ASSERTION MEANS ANYTHING.
# Without it this is a bare negative satisfied by an empty listing: helm absent
# or helm package failing leaves listing="", nothing matches the pattern, and it
# prints "ok package excludes ci/ and tests/" - the single assertion in this
# script that went green on a machine with no helm, while the twenty-five around
# it went red. Found by rev-2 in its own script, and it is the same shape as the
# bare negative it had filed against me an hour earlier.
#
# It also becomes strictly more load-bearing as the exclusion list grows: a later
# phase adding golden/ and hack/ to .helmignore doubles the number of things this
# one line certifies, and an empty listing would certify all four.
if [ -z "$listing" ]; then
  fail "package exclusion check: the listing is EMPTY, so nothing was examined -- helm package failed or produced no tarball"
elif printf '%s\n' "$listing" | grep -q '^scion-hub/\(ci\|tests\)/'; then
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
# D. The base-url channel tripwire.  (1 assertion)
#
# 🔴 THIS ASSERTION EXISTS TO GO RED AT THE NEXT PHASE BOUNDARY. That is its
# purpose, not a side effect, and a reviewer who "fixes" it by bumping the
# constant without editing the prose below has defeated it.
#
# WHY IT IS HERE AND NOT IN PHASE 1. _helpers.tpl:1089 tells an operator that
# hub.args may not carry -base-url because "a later phase delivers this setting
# through another channel ... This chart delivers none of them yet: today the
# flag would simply take effect." :835 says the same of the whole $ownedByConfig
# list: "none of them lands anywhere. They are not rendered". Both are TRUE at
# this commit - measured, both channels empty, see the positive control below -
# and both become FALSE the moment the settings ConfigMap and
# SCION_SERVER_BASE_URL land. Eleven claims of exactly this shape have gone
# stale in this subtree and all eleven were caught by a person reading prose.
# None was caught by a check, because the only commit at which a transition
# tripwire can be installed is one BEFORE the transition. Landed after the
# boundary it watches, this is theatre: it would be written already knowing the
# answer, and it would never have been red.
#
# WHY IT IS ONE ASSERTION AND NOT TWO. The two halves rev-2 specified - measure
# the render against the committed number, and check the prose agrees with the
# committed number - are not independently useful. A render that gained a
# channel while the prose still denies it, and prose updated ahead of a render
# that has not moved, are the same defect seen from two sides: THE FILE AND THE
# CHART DISAGREE ABOUT WHAT THE CHART DOES. Reporting that once, with both
# measurements printed, is the honest count. gd-em's ruling fixed 25 -> 26 and
# 106 -> 107; this is the shape that fits that number, and the split is raised
# rather than silently resolved.
# ---------------------------------------------------------------------------

DELIVERS_BASE_URL_CHANNEL=0   # Phase 1 sets this to 1 and edits :835 and :1089.

# THE POSITIVE CONTROL COMES FIRST. "Zero channels deliver base-url" is a
# negative assertion and an empty render satisfies it perfectly. Section B
# already rendered successfully, but relying on that is relying on a fact
# established forty lines away by code that could later be reordered, so the
# subject is re-read here at the point of use.
if [ -z "$RENDER" ]; then
  fail "base-url channel tripwire: the render is EMPTY, so no channel could have been observed"
else
  _chan=0
  _via=""
  # Channel 1: argv. Channel 2: the environment variable named in the reservation.
  if printf '%s\n' "$RENDER" | grep -q -- '--base-url'; then
    _chan=$((_chan + 1)); _via="${_via} argv(--base-url)"
  fi
  if printf '%s\n' "$RENDER" | grep -q 'SCION_SERVER_BASE_URL'; then
    _chan=$((_chan + 1)); _via="${_via} env(SCION_SERVER_BASE_URL)"
  fi
  # Does the prose still claim zero? Matched on the two sentences that make the
  # claim, not on the whole paragraph, so rewording around them does not count
  # as retracting them.
  _h="${CHART}/templates/_helpers.tpl"
  _claims_zero=0
  if grep -q 'delivers none of them yet' "$_h" 2>/dev/null \
     && grep -q 'none of them lands anywhere' "$_h" 2>/dev/null; then
    _claims_zero=1
  fi
  _want_claim=0; [ "$DELIVERS_BASE_URL_CHANNEL" -eq 0 ] && _want_claim=1

  if [ "$_chan" -eq "$DELIVERS_BASE_URL_CHANNEL" ] && [ "$_claims_zero" -eq "$_want_claim" ]; then
    pass "base-url channel tripwire: ${_chan} channel(s), prose agrees (committed ${DELIVERS_BASE_URL_CHANNEL})"
  else
    fail "base-url channel tripwire: THE CHART AND ITS OWN PROSE DISAGREE, or the phase boundary was crossed without updating both."
    echo "        channels measured in the render: ${_chan}${_via:+ --}${_via}"
    echo "        DELIVERS_BASE_URL_CHANNEL committed as: ${DELIVERS_BASE_URL_CHANNEL}"
    echo "        _helpers.tpl still claims zero channels: ${_claims_zero} (expected ${_want_claim})"
    echo "        IF YOU JUST LANDED THE SETTINGS ConfigMap OR SCION_SERVER_BASE_URL:"
    echo "          this is the intended red. Set DELIVERS_BASE_URL_CHANNEL=${_chan}, bump"
    echo "          EXPECTED_TOTAL nowhere (the count is unchanged), and edit BOTH prose"
    echo "          sites - _helpers.tpl:835 and :1089 - IN THE SAME DIFF. Bumping the"
    echo "          constant alone leaves the chart lying to operators in its own error text."
  fi
fi

# ---------------------------------------------------------------------------
# Fail closed.
# ---------------------------------------------------------------------------

# Emitted unconditionally, on every exit path, so run-all.sh can sum what
# actually ran even when this script is reporting a failure. The count check must
# not be silenced by the outcome it is meant to qualify.
echo "ASSERTIONS_EXECUTED=${executed}"

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
