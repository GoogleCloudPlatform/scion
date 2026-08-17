#!/usr/bin/env bash
#
# verify-failopen.sh -- the independent-route check on run-all.sh's own
# fail-closed behaviour. AUTHORED BY gd-p0-rev-2; moved into the repository from
# a scratch volume, with the adoption changes recorded at the bottom of this
# header.
#
# Usage: verify-failopen.sh <sha>
#
# Verifies by the lead's route, NOT by reading the diff: git archive into a clean tree
# outside the author's container, then exercise the harness with and without helm.
#
# THIS VERIFIER MUST FAIL AT 60b2912. A verifier that passes on the broken commit is
# worthless; that is the whole finding it exists to check, one level up.
#
# WHY IT IS IN THE REPOSITORY AND NOT IN A SCRATCH DIRECTORY. Every reason is a
# thing that already happened on this chart: a check that lives outside the tree
# dies with its author's container; nobody in a later phase can run what they do
# not know exists; and it is unwired by construction, which is the defect we
# escalated one directory over and would then have committed knowingly. The
# entire reason the fail-open was found is that the harness had been ruled
# in-repo rather than in /tmp, so an independent party could git-archive it out.
#
# NOT RUN BY run-all.sh, DELIBERATELY, and named in that file's NOT_RUN_HERE
# list rather than left to fall out of a pattern: this script INVOKES
# run-all.sh, so running it from there would recurse, and its steps are
# assertions about the runner rather than about the chart - they must not be
# summed into the chart assertion total.
#
# exit 0 -- all steps passed
# exit 1 -- a step failed
# exit 2 -- fewer than EXPECTED_STEPS steps ran, or a required tool is absent
#
# ADOPTION CHANGES, so the authorship line above stays exact:
#   1. Tool discovery extended past helm to git, tar, mktemp and python3. Step 4
#      shells out to python3 and would otherwise have failed as a step rather
#      than reported as a harness error - a verifier misreporting its own
#      missing toolchain as a finding is precisely what it exists to catch.
#   2. STEP 5 ADDED, and rev-2 is the one who identified the gap: step 4 induces
#      a red assertion AND a shortfall together and asserts a != b, so it cannot
#      catch rev-3's counter-example, which is a red assertion with NO shortfall
#      (102/106 arising purely from a failing script contributing 0 instead of
#      n). The verifier was incomplete in exactly the way the patch it verifies
#      was incomplete. EXPECTED_STEPS moved 4 -> 5 with it.
# No existing step was altered or repointed.

set -u -o pipefail

SHA="${1:?usage: verify-failopen.sh <sha>}"
REPO="${REPO:-$(git rev-parse --show-toplevel 2>/dev/null || echo /workspace)}"

# The toolchain is DISCOVERED, never assumed at a path. An earlier draft of this
# file hardcoded /tmp/linux-amd64 -- the author's container -- which is the same
# environment-as-apparatus mistake this script exists to catch, committed inside
# the catcher. Steps 2, 4 and 5 need a real helm; say so and stop, do not proceed
# and report whatever a missing binary happens to produce.
HELM_BIN="$(command -v helm 2>/dev/null || true)"
if [ -z "$HELM_BIN" ]; then
  echo "HARNESS ERROR: helm not on PATH. Steps 2, 4 and 5 require it and NOTHING WAS VERIFIED."
  echo "  (This script's whole subject is what happens without helm. It cannot"
  echo "   establish the with-helm baseline it needs to compare against.)"
  exit 2
fi
HELM_PATH="$(dirname "$HELM_BIN"):$PATH"

# The other four. Same rule, same exit code: a tool this script needs in order to
# MAKE a measurement is a harness error, never a finding about the subject.
_vmissing=""
for _t in git tar mktemp python3; do
  command -v "$_t" >/dev/null 2>&1 || _vmissing="${_vmissing} ${_t}"
done
if [ -n "$_vmissing" ]; then
  echo "HARNESS ERROR: required tool(s) not on PATH:${_vmissing}"
  echo "NOTHING WAS VERIFIED. This is not a passing run and it is not a finding."
  exit 2
fi

EXPECTED_STEPS=5
executed=0
failed=0
step() { executed=$((executed+1)); echo "ok    STEP $1"; }
bad()  { executed=$((executed+1)); failed=$((failed+1)); echo "FAIL  STEP $1"; }

# --- 1. clean tree from git archive, outside the author's working copy -------
TREE="$(mktemp -d)"
trap 'rm -rf "$TREE"' EXIT
if git -C "$REPO" archive "$SHA" | tar -x -C "$TREE" 2>/dev/null && [ -f "$TREE/deploy/helm/scion-hub/tests/run-all.sh" ]; then
  step "1 git archive $SHA -> clean tree ($TREE)"
else
  bad  "1 git archive $SHA failed or tests/run-all.sh absent"
  echo "HARNESS ERROR: cannot continue without a tree."; exit 2
fi
T="$TREE/deploy/helm/scion-hub/tests"

# --- 2. helm PRESENT: full count, exit 0 -------------------------------------
out_ok="$(PATH="$HELM_PATH" bash "$T/run-all.sh" 2>&1)"; rc_ok=$?
n_ok="$(printf '%s' "$out_ok" | sed -n 's|^PASS \([0-9]*\)/\([0-9]*\).*|\1/\2|p' | tail -1)"
if [ "$rc_ok" -eq 0 ] && [ -n "$n_ok" ] && [ "${n_ok%/*}" = "${n_ok#*/}" ]; then
  step "2 helm present -> exit 0, PASS $n_ok"
else
  bad  "2 helm present -> exit $rc_ok, count '$n_ok' (expected exit 0 and n==m)"
fi

# --- 3. helm ABSENT: exit 2, meta>0, 'nothing analysed', NO chart accusation --
out_no="$(env HELM=absent-helm-command bash "$T/run-all.sh" 2>&1)"; rc_no=$?
meta="$(printf '%s' "$out_no" | sed -n 's|.*meta-failures: \([0-9]*\).*|\1|p' | tail -1)"
accuse="$(printf '%s' "$out_no" | grep -cE 'MISSING|\.helmignore' || true)"
says_nothing="$(printf '%s' "$out_no" | grep -ciE 'nothing was (asserted|analysed|analyzed)|not found|NOTHING WAS' || true)"
ok3=1
[ "$rc_no" -eq 2 ]        || { echo "      - exit is $rc_no, expected 2"; ok3=0; }
[ "${meta:-0}" -gt 0 ]    || { echo "      - meta-failures is ${meta:-unset}, expected > 0"; ok3=0; }
[ "$accuse" -eq 0 ]       || { echo "      - $accuse line(s) ACCUSE THE CHART (matched MISSING or .helmignore):"; printf '%s\n' "$out_no" | grep -E 'MISSING|\.helmignore' | sed 's/^/          /' | head -5; ok3=0; }
[ "$says_nothing" -gt 0 ] || { echo "      - no wording saying nothing was analysed"; ok3=0; }
if [ "$ok3" -eq 1 ]; then
  step "3 helm absent -> exit 2, meta-failures=$meta, no chart accusation"
else
  bad  "3 helm absent -> fail-open not fixed"
fi

# --- 4. a REAL assertion failure AND a short run, together -------------------
# Both must be reported. The count inequality must fire ALONGSIDE the failure,
# not be short-circuited by it. That short-circuit is the 60b2912 defect.
M="$(mktemp -d)"; trap 'rm -rf "$TREE" "$M"' EXIT
cp -r "$TREE/deploy/helm/scion-hub" "$M/chart"
# (i) real assertion failure: break the chart so a render guard genuinely fails
rm -f "$M/chart/templates/service.yaml"
# (ii) short run: delete one assertion from update-strategy.sh AND lower its own total
python3 - "$M/chart/tests/update-strategy.sh" <<'PY'
import sys
p=sys.argv[1]; s=open(p).read()
s=s.replace('EXPECTED_TOTAL=4','EXPECTED_TOTAL=3')
L=s.split('\n'); idx=[i for i,l in enumerate(L) if l.strip().startswith('strategy_is')]
del L[idx[-1]]
open(p,'w').write('\n'.join(L))
PY
out_b="$(PATH="$HELM_PATH" bash "$M/chart/tests/run-all.sh" 2>&1)"; rc_b=$?
has_fail="$(printf '%s' "$out_b" | grep -cE 'ASSERTION FAILURE' || true)"
short="$(printf '%s' "$out_b" | sed -n 's|.*assertions: \([0-9]*\)/\([0-9]*\).*|\1 \2|p' | tail -1)"
a="${short% *}"; b="${short#* }"
meta_b="$(printf '%s' "$out_b" | sed -n 's|.*meta-failures: \([0-9]*\).*|\1|p' | tail -1)"
ok4=1
[ "$has_fail" -gt 0 ]      || { echo "      - no ASSERTION FAILURE reported, but one was induced"; ok4=0; }
[ "${a:-0}" -ne "${b:-0}" ] || { echo "      - assertion total $a/$b shows no shortfall, but one was induced"; ok4=0; }
[ "${meta_b:-0}" -gt 0 ]   || { echo "      - meta-failures is ${meta_b:-unset}: THE COUNT CHECK WAS SHORT-CIRCUITED BY THE ASSERTION FAILURE"; ok4=0; }
[ "$rc_b" -ne 0 ]          || { echo "      - exit 0 with both a failure and a short run"; ok4=0; }
if [ "$ok4" -eq 1 ]; then
  step "4 both at once -> ASSERTION FAILURE reported AND $a/$b shortfall as meta-failure ($meta_b), exit $rc_b"
else
  bad  "4 the count inequality does not fire alongside an assertion failure"
fi

# --- 5. a REAL assertion failure with NO shortfall ----------------------------
# gd-p0-rev-3's counter-example, and the gap in step 4. Step 4 induces a failure
# AND a shortfall together, then asserts a != b - so it cannot see the case where
# a script goes red and its count is simply never summed. At 60b2912 that printed
# "assertions: 102/106  meta-failures: 0" WITH HELM PRESENT: a failing script
# contributed 0 instead of 4, so the size of the shortfall bore no relation to
# the size of the breach, and the count check was suppressed by the very failure
# that shortened it.
#
# The required outcome is the one the two exit codes exist for: a red chart and
# an INTACT check set, correctly distinguished. So exit 1, and the total is
# either complete or flagged - a short total sitting beside meta 0 is the defect.
M5="$(mktemp -d)"; trap 'rm -rf "$TREE" "$M" "$M5"' EXIT
cp -r "$TREE/deploy/helm/scion-hub" "$M5/chart"
# One assertion made genuinely red, by flipping the value it expects. Nothing
# else touched: no count changed, no assertion removed, the chart is untouched.
python3 - "$M5/chart/tests/update-strategy.sh" <<'PY'
import sys
p=sys.argv[1]; L=open(p).read().split('\n')
for i,l in enumerate(L):
    if l.startswith('strategy_is "default at replicaCount=1" Recreate'):
        L[i]=l.replace('Recreate','RollingUpdate',1); break
else:
    sys.exit("MUTATION TARGET NOT FOUND")
open(p,'w').write('\n'.join(L))
PY
if [ $? -ne 0 ]; then
  echo "HARNESS ERROR: step 5 could not induce its mutation; the target line has moved."
  echo "NOTHING WAS VERIFIED for step 5. Fix the mutation, do not drop the step."
  exit 2
fi
out_c="$(PATH="$HELM_PATH" bash "$M5/chart/tests/run-all.sh" 2>&1)"; rc_c=$?
hasfail_c="$(printf '%s' "$out_c" | grep -cE 'ASSERTION FAILURE' || true)"
short_c="$(printf '%s' "$out_c" | sed -n 's|.*assertions: \([0-9]*\)/\([0-9]*\).*|\1 \2|p' | tail -1)"
ac="${short_c% *}"; bc="${short_c#* }"
meta_c="$(printf '%s' "$out_c" | sed -n 's|.*meta-failures: \([0-9]*\).*|\1|p' | tail -1)"
ok5=1
[ -n "$short_c" ]        || { echo "      - no 'assertions: n/m' summary line at all"; ok5=0; }
[ "$hasfail_c" -gt 0 ]   || { echo "      - no ASSERTION FAILURE reported, but one was induced"; ok5=0; }
[ "$rc_c" -eq 1 ]        || { echo "      - exit is $rc_c, expected 1 (a red chart, an intact check set)"; ok5=0; }
if [ "${ac:-0}" -ne "${bc:-0}" ] && [ "${meta_c:-0}" -eq 0 ]; then
  echo "      - assertions ${ac}/${bc} is SHORT and meta-failures is 0: the count check was"
  echo "        suppressed by the assertion failure, or a red script contributed 0 instead of n"
  ok5=0
fi
if [ "$ok5" -eq 1 ]; then
  step "5 red assertion, no shortfall -> exit 1, assertions ${ac}/${bc}, meta ${meta_c}"
else
  bad  "5 a red assertion still distorts or suppresses the completeness count"
fi

# --- fail closed --------------------------------------------------------------
if [ "$executed" -ne "$EXPECTED_STEPS" ]; then
  echo "HARNESS ERROR: executed ${executed} steps, expected exactly ${EXPECTED_STEPS}."
  exit 2
fi
[ "$failed" -eq 0 ] || { echo "FAILED ${failed}/${executed}"; exit 1; }
echo "PASS ${executed}/${EXPECTED_STEPS} (helm $(helm version --short 2>/dev/null) present for steps 2 and 4)"
