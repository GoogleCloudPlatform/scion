#!/usr/bin/env bash
#
# Fires the shared-workspace guard in hack/wsguard/bin/git.
#
# A control that has never been fired is not a control. This script builds
# three throwaway repositories under a temp directory and runs the guard
# against real destructive commands, so that both directions are demonstrated
# rather than asserted:
#
#   the NEGATIVE — the guard refuses a destructive command, and the thing the
#   command would have destroyed is still there afterwards;
#   the POSITIVE — the guard permits the legitimate neighbours of each refused
#   command, and permits the destructive commands themselves in a repository
#   that is not the shared one. A guard that refuses everything is not an
#   instrument.
#
# POSITIVE CONTROLS ON THE HARNESS
#
# "The file was still modified after the guard refused" proves nothing unless
# `git checkout --` would have removed the modification in the first place. So
# before any arm runs, each hazard is reproduced with the REAL git and the
# harness asserts that the damage actually happens. If a control does not
# reproduce, the harness cannot exercise the hazard, and it exits 2 — it does
# not report a pass it did not earn.
#
# INSTRUMENT DISCLOSURE
#
# Every assertion in this file is a bash `[[ "$x" == *literal* ]]` glob
# containment test or an integer comparison. No grep, rg, awk or sed is used to
# decide any verdict, so no result here depends on which binary `grep` resolves
# to or on which regex dialect it defaults to. The one thing that must be
# resolved from the environment — the real git — is resolved explicitly and
# printed with its version in the provenance block.
#
# EXIT CODES
#
#   0  every arm behaved as specified
#   1  an arm misbehaved — the guard is not doing what it claims. Details on
#      stderr, naming the arm.
#   2  NOTHING WAS TESTED. The environment could not host the test: no real
#      git, no shim, a temp directory that could not be created, or a positive
#      control that did not reproduce its hazard.
#
# 2 is separate from 1 because they mean opposite things to whoever reads the
# log. 1 accuses the guard. 2 accuses the harness and says nothing whatever
# about the guard.
#
set -uo pipefail

SELF_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
SHIM_DIR="$SELF_DIR/bin"
SHIM="$SHIM_DIR/git"

die_cannot_evaluate() {
  echo "wsguard-selftest: $* — NOTHING WAS TESTED (broken harness, not a verdict on the guard)" >&2
  exit 2
}

[[ -f "$SHIM" ]] || die_cannot_evaluate "no shim at $SHIM"
[[ -x "$SHIM" ]] || die_cannot_evaluate "shim at $SHIM is not executable"

# ---------------------------------------------------------------------------
# --prove-it: a negative control on this harness.
#
# A green selftest is worth exactly as much as the harness's ability to go red.
# Before trusting a pass, run the whole suite once with one expectation
# deliberately falsified and require it to FAIL. If the falsified run passes,
# the harness is not measuring anything and the honest answer is 2, not 0.
#
# This is the question that has paid four times on this project today: what
# would this check have done if the thing it is checking for were real?
# ---------------------------------------------------------------------------
if [[ "${1:-}" == "--prove-it" ]]; then
  mutated_log="$(mktemp)" || die_cannot_evaluate "mktemp failed"
  mutated_status=0
  WSGUARD_SELFTEST_INJECT_FAILURE=1 "$SELF_DIR/selftest.sh" >"$mutated_log" 2>&1 || mutated_status=$?
  case "$mutated_status" in
    1)
      echo "wsguard-selftest: negative control PASSED — with one expectation falsified the harness exited 1."
      echo "wsguard-selftest: the suite below is therefore capable of failing. Running it for real."
      echo
      rm -f "$mutated_log"
      ;;
    0)
      echo "wsguard-selftest: negative control FAILED — a run with a falsified expectation still exited 0." >&2
      echo "wsguard-selftest: this harness cannot go red, so its green means nothing." >&2
      sed 's/^/  | /' "$mutated_log" >&2
      rm -f "$mutated_log"
      exit 2
      ;;
    *)
      echo "wsguard-selftest: negative control exited $mutated_status, expected 1 — the harness is broken, not the guard." >&2
      sed 's/^/  | /' "$mutated_log" >&2
      rm -f "$mutated_log"
      exit 2
      ;;
  esac
  shift
fi

# Resolve the real git the same way the shim does, and by the same rule:
# the first git on PATH that is not the shim.
find_real_git() {
  local dir resolved candidate
  local IFS=:
  for dir in $PATH; do
    [[ -z "$dir" ]] && dir="."
    resolved="$(cd -- "$dir" 2>/dev/null && pwd -P)" || continue
    [[ "$resolved" == "$SHIM_DIR" ]] && continue
    candidate="$resolved/git"
    if [[ -f "$candidate" && -x "$candidate" ]]; then
      printf '%s' "$candidate"
      return 0
    fi
  done
  return 1
}
REAL_GIT="$(find_real_git)" || die_cannot_evaluate "no real git on PATH"

WORK="$(mktemp -d)" || die_cannot_evaluate "mktemp -d failed"
trap 'rm -rf "$WORK"' EXIT

SHARED="$WORK/shared"     # stands in for the shared /workspace mount
PRIVATE="$WORK/private"   # a clone the operator owns; the guard must not apply
DONOR="$WORK/donor"       # a fetch source
CONTROL="$WORK/control"   # where the hazards are reproduced with the real git
OUTSIDE="$WORK/outside"   # not a repository at all
AUDIT="$WORK/audit.log"

mkrepo() {
  local dir="$1"
  mkdir -p "$dir" || return 1
  "$REAL_GIT" -C "$dir" init -q -b main || return 1
  "$REAL_GIT" -C "$dir" config user.email wsguard@selftest.invalid || return 1
  "$REAL_GIT" -C "$dir" config user.name "wsguard selftest" || return 1
  printf 'committed content\n' >"$dir/tracked.txt" || return 1
  "$REAL_GIT" -C "$dir" add tracked.txt || return 1
  "$REAL_GIT" -C "$dir" commit -q -m "seed" || return 1
}

for d in "$SHARED" "$PRIVATE" "$DONOR" "$CONTROL"; do
  mkrepo "$d" || die_cannot_evaluate "could not build fixture repo at $d"
done
mkdir -p "$OUTSIDE" || die_cannot_evaluate "could not create $OUTSIDE"

DONOR_SHA="$("$REAL_GIT" -C "$DONOR" rev-parse HEAD)" ||
  die_cannot_evaluate "could not read donor HEAD"

# The marker that must survive a refusal, and the untracked file that must
# survive a refused clean.
dirty() {
  local dir="$1"
  printf 'UNCOMMITTED-WORK-OF-ANOTHER-AGENT\n' >"$dir/tracked.txt"
  printf 'scratch notes\n' >"$dir/untracked.txt"
}

# ---------------------------------------------------------------------------
# Positive controls: does the real git actually do the damage we claim?
# ---------------------------------------------------------------------------
controls_run=0
control_lines=()
control() {
  controls_run=$(( controls_run + 1 ))
  control_lines+=("$(printf '  control %-24s %s' "$1" "$2")")
}

dirty "$CONTROL"
control_status=0
"$REAL_GIT" -C "$CONTROL" checkout -- tracked.txt || control_status=$?
control_content="$(cat "$CONTROL/tracked.txt")"
if (( control_status != 0 )) || [[ "$control_content" == *UNCOMMITTED-WORK* ]]; then
  die_cannot_evaluate "control 'checkout -- discards a modification' did not reproduce (status=$control_status, content='$control_content')"
fi
control "checkout-discards" "reproduced: real git erased the uncommitted modification"

dirty "$CONTROL"
control_status=0
"$REAL_GIT" -C "$CONTROL" clean -fd >/dev/null || control_status=$?
if (( control_status != 0 )) || [[ -e "$CONTROL/untracked.txt" ]]; then
  die_cannot_evaluate "control 'clean -fd deletes an untracked file' did not reproduce (status=$control_status)"
fi
control "clean-deletes" "reproduced: real git deleted the untracked file"

control_status=0
"$REAL_GIT" -C "$CONTROL" fetch "$DONOR" main >/dev/null 2>&1 || control_status=$?
if (( control_status != 0 )) || [[ ! -f "$CONTROL/.git/FETCH_HEAD" ]]; then
  die_cannot_evaluate "control 'fetch writes FETCH_HEAD' did not reproduce (status=$control_status)"
fi
control_fetch_head="$(cat "$CONTROL/.git/FETCH_HEAD")"
if [[ "$control_fetch_head" != *"$DONOR_SHA"* ]]; then
  die_cannot_evaluate "control 'fetch writes FETCH_HEAD' wrote something unexpected"
fi
control "fetch-head-is-a-slot" "reproduced: fetch left $DONOR_SHA in the single .git/FETCH_HEAD slot"

# ---------------------------------------------------------------------------
# Arm runner. The status is captured into a variable and branched on by value.
# It is never used as the condition of an `if`: an `if` sees a boolean, and a
# gate whose refusal path is an `if` cannot report cannot-evaluate at all.
# ---------------------------------------------------------------------------
arms_run=0
arms_refuse=0
arms_cannot=0
arms_permit=0
arms_mismatched=0
checks_run=0
checks_failed=0
failures=()
OVERRIDE_REASON=""

run_guarded() {
  local dir="$1"
  shift
  LAST_STATUS=0
  LAST_OUT="$(
    cd "$dir" || exit 90
    export PATH="$SHIM_DIR:$PATH"
    export SCION_WORKSPACE_MODE=shared-plain
    export SCION_WSGUARD_ROOT="$SHARED"
    export SCION_WSGUARD_AUDIT="$AUDIT"
    export SCION_AGENT_NAME=wsguard-selftest
    export SCION_AGENT_SLUG=wsguard-selftest
    if [[ -n "$OVERRIDE_REASON" ]]; then
      export SCION_WSGUARD_OVERRIDE="$OVERRIDE_REASON"
    fi
    git "$@" 2>&1
  )" || LAST_STATUS=$?
}

# arm <name> <expect-status> <dir> -- <git argv...>
arm() {
  local name="$1" expect="$2" dir="$3"
  shift 4  # name, expect, dir, --
  arms_run=$(( arms_run + 1 ))
  case "$expect" in
    77) arms_refuse=$(( arms_refuse + 1 )) ;;
    78) arms_cannot=$(( arms_cannot + 1 )) ;;
    0)  arms_permit=$(( arms_permit + 1 )) ;;
  esac
  # The falsified expectation used by --prove-it. It sits on the first arm so
  # that the negative control exercises the same path as a real regression.
  if [[ "${WSGUARD_SELFTEST_INJECT_FAILURE:-}" == "1" && "$name" == "N1-checkout-pathspec" ]]; then
    expect=0
  fi
  run_guarded "$dir" "$@"
  local verdict="ok"
  if (( LAST_STATUS != expect )); then
    verdict="FAIL"
    arms_mismatched=$(( arms_mismatched + 1 ))
    failures+=("$name: expected status $expect, got $LAST_STATUS")
  fi
  printf '\n--- arm %s [%s]\n' "$name" "$verdict"
  printf '    $ git %s\n' "$*"
  printf '    (cwd %s)\n' "${dir/#$WORK/\$WORK}"
  printf '    exit %s (expected %s)\n' "$LAST_STATUS" "$expect"
  if [[ -n "$LAST_OUT" ]]; then
    printf '%s\n' "$LAST_OUT" | sed 's/^/    | /'
  fi
  OVERRIDE_REASON=""
}

# assert <arm-name> <description> <condition-result 0|1>
assert() {
  local name="$1" desc="$2" ok="$3"
  checks_run=$(( checks_run + 1 ))
  if (( ok == 0 )); then
    printf '    + %s\n' "$desc"
    return
  fi
  checks_failed=$(( checks_failed + 1 ))
  failures+=("$name: $desc")
  printf '    ! %s  <-- FAILED\n' "$desc"
}

still_dirty() {
  local content
  content="$(cat "$SHARED/tracked.txt")"
  [[ "$content" == *UNCOMMITTED-WORK* ]]
}

echo "==========================================================================="
echo "wsguard selftest"
echo "==========================================================================="
shim_digest="$(sha256sum "$SHIM")" || shim_digest="unavailable (sha256sum failed)"
shim_digest="${shim_digest%% *}"
echo "  shim                 : $SHIM"
echo "  shim sha256          : $shim_digest"
echo "  real git             : $REAL_GIT"
echo "  real git version     : $("$REAL_GIT" --version)"
echo "  assertion instrument : bash [[ == ]] glob containment; no grep/rg/awk/sed decides any verdict"
echo "  fixture root         : \$WORK ($WORK)"
echo "  guarded root         : \$WORK/shared     (SCION_WSGUARD_ROOT)"
echo "  ungoverned repo      : \$WORK/private    (must behave like an ordinary clone)"
echo "  workspace mode       : shared-plain"
echo
echo "positive controls on the harness (the hazards, reproduced with the real git):"
printf '%s\n' "${control_lines[@]}"

echo
echo "==========================================================================="
echo "NEGATIVE — the guard must refuse, and the work must survive"
echo "==========================================================================="

dirty "$SHARED"
arm "N1-checkout-pathspec" 77 "$SHARED" -- checkout -- tracked.txt
still_dirty && assert "N1-checkout-pathspec" "the uncommitted modification is still in the working tree" 0 ||
  assert "N1-checkout-pathspec" "the uncommitted modification is still in the working tree" 1
[[ "$LAST_OUT" == *"REFUSED [a/working-tree]"* ]] &&
  assert "N1-checkout-pathspec" "refusal names the rule it fired" 0 ||
  assert "N1-checkout-pathspec" "refusal names the rule it fired" 1

arm "N2-clean-force" 77 "$SHARED" -- clean -fd
[[ -e "$SHARED/untracked.txt" ]] &&
  assert "N2-clean-force" "the untracked file another agent was holding still exists" 0 ||
  assert "N2-clean-force" "the untracked file another agent was holding still exists" 1

arm "N3-branch-switch" 77 "$SHARED" -- checkout -b some-other-branch
branch_now="$("$REAL_GIT" -C "$SHARED" rev-parse --abbrev-ref HEAD)"
[[ "$branch_now" == "main" ]] &&
  assert "N3-branch-switch" "HEAD is still on main for every other agent in the tree" 0 ||
  assert "N3-branch-switch" "HEAD is still on main for every other agent in the tree" 1

arm "N4-reset-hard" 77 "$SHARED" -- reset --hard HEAD
still_dirty && assert "N4-reset-hard" "the uncommitted modification survived the refused reset" 0 ||
  assert "N4-reset-hard" "the uncommitted modification survived the refused reset" 1

arm "N5-stash" 77 "$SHARED" -- stash push -m "mine"
still_dirty && assert "N5-stash" "the modification was not swept into the shared stash" 0 ||
  assert "N5-stash" "the modification was not swept into the shared stash" 1

arm "N6-restore" 77 "$SHARED" -- restore tracked.txt
still_dirty && assert "N6-restore" "the uncommitted modification survived the refused restore" 0 ||
  assert "N6-restore" "the uncommitted modification survived the refused restore" 1

arm "N7-branch-delete" 77 "$SHARED" -- branch -D main

# Hazard (b), the invisible one: the write side and the read side.
arm "N8-fetch-into-fetch-head" 77 "$SHARED" -- fetch "$DONOR" main
[[ ! -f "$SHARED/.git/FETCH_HEAD" ]] &&
  assert "N8-fetch-into-fetch-head" "the shared FETCH_HEAD slot was not written" 0 ||
  assert "N8-fetch-into-fetch-head" "the shared FETCH_HEAD slot was not written" 1

arm "N9-read-fetch-head" 77 "$SHARED" -- log -1 --format=%H FETCH_HEAD
[[ "$LAST_OUT" == *"REFUSED [b/fetch-head-read]"* ]] &&
  assert "N9-read-fetch-head" "refusal names the FETCH_HEAD rule, not the working-tree rule" 0 ||
  assert "N9-read-fetch-head" "refusal names the FETCH_HEAD rule, not the working-tree rule" 1

OVERRIDE_REASON="1"
arm "N10-override-without-a-reason" 77 "$SHARED" -- checkout -- tracked.txt
still_dirty && assert "N10-override-without-a-reason" "a one-character override did not buy passage" 0 ||
  assert "N10-override-without-a-reason" "a one-character override did not buy passage" 1

echo
echo "==========================================================================="
echo "CANNOT EVALUATE — armed, watched, but the target could not be identified"
echo "==========================================================================="

arm "U1-not-a-repository" 78 "$OUTSIDE" -- checkout -- anything.txt
[[ "$LAST_OUT" == *"CANNOT EVALUATE"* ]] &&
  assert "U1-not-a-repository" "the diagnostic says no reading was taken, and does not read as a refusal" 0 ||
  assert "U1-not-a-repository" "the diagnostic says no reading was taken, and does not read as a refusal" 1
[[ "$LAST_OUT" == *"rev-parse --show-toplevel exited"* ]] &&
  assert "U1-not-a-repository" "the underlying git stderr is reprinted, not swallowed" 0 ||
  assert "U1-not-a-repository" "the underlying git stderr is reprinted, not swallowed" 1

echo
echo "==========================================================================="
echo "POSITIVE — the guard must permit; a gate that refuses everything is not one"
echo "==========================================================================="

arm "P1-status" 0 "$SHARED" -- status --porcelain
[[ "$LAST_OUT" == *"tracked.txt"* ]] &&
  assert "P1-status" "real git output came back through the shim unaltered" 0 ||
  assert "P1-status" "real git output came back through the shim unaltered" 1

arm "P2-clean-dry-run" 0 "$SHARED" -- clean -n
[[ "$LAST_OUT" == *"untracked.txt"* ]] &&
  assert "P2-clean-dry-run" "the permitted neighbour of the refused command still answers the question" 0 ||
  assert "P2-clean-dry-run" "the permitted neighbour of the refused command still answers the question" 1
[[ -e "$SHARED/untracked.txt" ]] &&
  assert "P2-clean-dry-run" "and it deleted nothing" 0 ||
  assert "P2-clean-dry-run" "and it deleted nothing" 1

arm "P3-fetch-to-named-ref" 0 "$SHARED" -- fetch "$DONOR" "main:refs/wsguard/wsguard-selftest/donor-main"
fetched_sha="$("$REAL_GIT" -C "$SHARED" rev-parse refs/wsguard/wsguard-selftest/donor-main 2>&1)"
[[ "$fetched_sha" == "$DONOR_SHA" ]] &&
  assert "P3-fetch-to-named-ref" "the fetch landed in a ref this agent owns: $DONOR_SHA" 0 ||
  assert "P3-fetch-to-named-ref" "the fetch landed in a ref this agent owns (got '$fetched_sha')" 1

arm "P4-log-named-ref" 0 "$SHARED" -- log -1 --format=%H refs/wsguard/wsguard-selftest/donor-main
[[ "$LAST_OUT" == "$DONOR_SHA" ]] &&
  assert "P4-log-named-ref" "reading the owned ref is permitted and returns the right commit" 0 ||
  assert "P4-log-named-ref" "reading the owned ref is permitted and returns the right commit" 1

arm "P5-stash-list" 0 "$SHARED" -- stash list
arm "P6-branch-list" 0 "$SHARED" -- branch --list

# The scoping arm. Same command as N1, in a repository that is not shared.
printf 'MY OWN WORK\n' >"$PRIVATE/tracked.txt"
arm "P7-checkout-in-private-clone" 0 "$PRIVATE" -- checkout -- tracked.txt
private_content="$(cat "$PRIVATE/tracked.txt")"
[[ "$private_content" == "committed content" ]] &&
  assert "P7-checkout-in-private-clone" "the guard did not stand between the operator and their own clone" 0 ||
  assert "P7-checkout-in-private-clone" "the guard did not stand between the operator and their own clone (got '$private_content')" 1

# The audited override. Destructive, permitted, and recorded.
dirty "$SHARED"
OVERRIDE_REASON="selftest: exercising the audited override path"
arm "P8-override-with-a-reason" 0 "$SHARED" -- checkout -- tracked.txt
override_content="$(cat "$SHARED/tracked.txt")"
[[ "$override_content" == "committed content" ]] &&
  assert "P8-override-with-a-reason" "the override actually let the command through" 0 ||
  assert "P8-override-with-a-reason" "the override actually let the command through" 1
audit_body="$(cat "$AUDIT" 2>&1)"
[[ "$audit_body" == *"exercising the audited override path"* ]] &&
  assert "P8-override-with-a-reason" "and the reason is on the record in the audit log" 0 ||
  assert "P8-override-with-a-reason" "and the reason is on the record in the audit log (got '$audit_body')" 1

echo
echo "audit log after the run:"
sed 's/^/    | /' "$AUDIT"

echo
echo "==========================================================================="
printf 'arms run              : %d  (%d refusal, %d cannot-evaluate, %d permit)\n' \
  "$arms_run" "$arms_refuse" "$arms_cannot" "$arms_permit"
printf 'exit-status mismatches: %d/%d\n' "$arms_mismatched" "$arms_run"
printf 'post-condition checks : %d  failed: %d\n' "$checks_run" "$checks_failed"
printf 'harness controls      : %d/3 reproduced with the real git\n' "$controls_run"
echo "==========================================================================="

total_failed=$(( arms_mismatched + checks_failed ))
if (( total_failed == 0 )); then
  echo "wsguard-selftest: PASS — $arms_run arms, $checks_run post-conditions; refusals and permissions both demonstrated"
  exit 0
fi

echo >&2
echo "wsguard-selftest: FAIL — $total_failed of $(( arms_run + checks_run )) expectations did not hold:" >&2
for f in "${failures[@]}"; do
  echo "  - $f" >&2
done
exit 1
