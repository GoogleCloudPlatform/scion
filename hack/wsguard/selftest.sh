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

# The three hazards below were all reported by gd-wsg-rev against a shipped
# version of the shim that permitted them. Each is reproduced with the REAL git
# before the corresponding arm runs, because "the guard refused it" is not a
# result unless the thing refused would otherwise have done damage.
"$REAL_GIT" -C "$CONTROL" config alias.co checkout
"$REAL_GIT" -C "$CONTROL" config alias.sh '!echo SHELL-ALIAS'
dirty "$CONTROL"
control_status=0
"$REAL_GIT" -C "$CONTROL" co -- tracked.txt || control_status=$?
control_content="$(cat "$CONTROL/tracked.txt")"
if (( control_status != 0 )) || [[ "$control_content" == *UNCOMMITTED-WORK* ]]; then
  die_cannot_evaluate "control 'an alias reaches checkout' did not reproduce (status=$control_status, content='$control_content')"
fi
control "alias-reaches-checkout" "reproduced: \`git co\` erased the modification, so the alias is a real path to the hazard"

dirty "$CONTROL"
control_status=0
"$REAL_GIT" -C "$CONTROL" rm -f -q tracked.txt >/dev/null 2>&1 || control_status=$?
if (( control_status != 0 )) || [[ -e "$CONTROL/tracked.txt" ]]; then
  die_cannot_evaluate "control 'git rm -f deletes from the working tree' did not reproduce (status=$control_status)"
fi
control "rm-deletes-worktree" "reproduced: git rm -f removed the file from the working tree, not just the index"
"$REAL_GIT" -C "$CONTROL" checkout -q -- tracked.txt 2>/dev/null || "$REAL_GIT" -C "$CONTROL" reset -q --hard >/dev/null

# A file named exactly `-h`. This is what turns O4 from a disagreement about an
# exit code into a bypass: after `--` git reads `-h` as a pathspec.
printf 'committed\n' >"$CONTROL/-h"
"$REAL_GIT" -C "$CONTROL" add -- ./-h >/dev/null 2>&1
"$REAL_GIT" -C "$CONTROL" -c user.email=w@g -c user.name=w commit -qm "a file named -h" >/dev/null 2>&1
printf 'UNCOMMITTED-WORK\n' >"$CONTROL/-h"
control_status=0
"$REAL_GIT" -C "$CONTROL" checkout -- -h >/dev/null 2>&1 || control_status=$?
control_dash_h="$(cat "$CONTROL/-h")"
if (( control_status != 0 )) || [[ "$control_dash_h" == *UNCOMMITTED-WORK* ]]; then
  die_cannot_evaluate "control 'checkout -- -h overwrites a file named -h' did not reproduce (status=$control_status, content='$control_dash_h')"
fi
control "dash-h-is-a-pathspec" "reproduced: after -- git treated -h as a PATH and overwrote it"

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
ROOT_OVERRIDE=""
PATH_OVERRIDE=""
HOPS_OVERRIDE=""

run_guarded() {
  local dir="$1"
  shift
  LAST_STATUS=0
  LAST_OUT="$(
    cd "$dir" || exit 90
    if [[ -n "$PATH_OVERRIDE" ]]; then
      export PATH="$PATH_OVERRIDE"
    else
      export PATH="$SHIM_DIR:$PATH"
    fi
    if [[ -n "$HOPS_OVERRIDE" ]]; then
      export SCION_WSGUARD_HOPS="$HOPS_OVERRIDE"
    fi
    export SCION_WORKSPACE_MODE=shared-plain
    if [[ -n "$ROOT_OVERRIDE" ]]; then
      export SCION_WSGUARD_ROOT="$ROOT_OVERRIDE"
    else
      export SCION_WSGUARD_ROOT="$SHARED"
    fi
    export SCION_WSGUARD_AUDIT="$AUDIT"
    export SCION_AGENT_NAME=wsguard-selftest
    export SCION_AGENT_SLUG=wsguard-selftest
    if [[ -n "$OVERRIDE_REASON" ]]; then
      export SCION_WSGUARD_OVERRIDE="$OVERRIDE_REASON"
    fi
    # Every arm runs under a timeout. Two shims that resolve to each other do
    # not fail, they SPIN — measured at rc=124 with empty stderr before the fix
    # below. A suite that can hang cannot report, so the hang is bounded and
    # scored as a distinct status rather than being waited on.
    if [[ -n "$TIMEOUT_BIN" ]]; then
      "$TIMEOUT_BIN" 20 git "$@" 2>&1
    else
      git "$@" 2>&1
    fi
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
  ROOT_OVERRIDE=""
  PATH_OVERRIDE=""
  HOPS_OVERRIDE=""
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

TIMEOUT_BIN="$(command -v timeout 2>/dev/null)" || TIMEOUT_BIN=""

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

# The scoping decision compares two paths. If they are not produced by the same
# normaliser the comparison silently answers "different", which for this guard
# means PASSTHROUGH — an armed guard that permits everything while still
# printing its arming banner. U2 and N11 are the two shapes of that bug.
dirty "$SHARED"
ROOT_OVERRIDE="$WORK/root-that-does-not-exist"
arm "U2-unresolvable-root" 78 "$SHARED" -- checkout -- tracked.txt
[[ "$LAST_OUT" == *"guarded root could not be resolved"* ]] &&
  assert "U2-unresolvable-root" "an unresolvable root is cannot-evaluate, NOT a silent passthrough" 0 ||
  assert "U2-unresolvable-root" "an unresolvable root is cannot-evaluate, NOT a silent passthrough" 1
[[ "$LAST_OUT" == *"cd exited"* ]] &&
  assert "U2-unresolvable-root" "the shell's own error is reprinted, not swallowed" 0 ||
  assert "U2-unresolvable-root" "the shell's own error is reprinted, not swallowed" 1
still_dirty && assert "U2-unresolvable-root" "the modification survived — nothing ran" 0 ||
  assert "U2-unresolvable-root" "the modification survived — nothing ran" 1

echo
echo "==========================================================================="
echo "REFUSAL, CONTINUED — the root must be normalised before it is compared"
echo "==========================================================================="

# Fixtures for the alias and dash-pathspec arms, on the SHARED repo.
"$REAL_GIT" -C "$SHARED" config alias.co checkout
"$REAL_GIT" -C "$SHARED" config alias.sh '!echo SHELL-ALIAS'
printf 'committed\n' >"$SHARED/-h"
"$REAL_GIT" -C "$SHARED" add -- ./-h >/dev/null 2>&1
"$REAL_GIT" -C "$SHARED" -c user.email=w@g -c user.name=w commit -qm "a file named -h" >/dev/null 2>&1

dirty "$SHARED"
ROOT_OVERRIDE="$SHARED/"
arm "N11-root-with-trailing-slash" 77 "$SHARED" -- checkout -- tracked.txt
still_dirty && assert "N11-root-with-trailing-slash" "a trailing slash on the root does not open the gate" 0 ||
  assert "N11-root-with-trailing-slash" "a trailing slash on the root does not open the gate" 1

echo
echo "==========================================================================="
echo "REFUSAL, CONTINUED — where the guard's parse must agree with git's parse"
echo "==========================================================================="

# R2. The guard classified the literal token; git expands the alias internally
# and never re-enters the shim. Shipped behaviour was rc=0 and silence.
dirty "$SHARED"
arm "N12-alias-reaches-checkout" 77 "$SHARED" -- co -- tracked.txt
still_dirty && assert "N12-alias-reaches-checkout" "an alias is judged as the command git will dispatch" 0 ||
  assert "N12-alias-reaches-checkout" "an alias is judged as the command git will dispatch" 1

# R3. git rm deletes from the WORKING TREE, and was simply missing.
dirty "$SHARED"
arm "N13-rm-deletes-worktree" 77 "$SHARED" -- rm -f tracked.txt
[[ -e "$SHARED/tracked.txt" ]] && assert "N13-rm-deletes-worktree" "the file is still on disk after the refusal" 0 ||
  assert "N13-rm-deletes-worktree" "the file is still on disk after the refusal" 1

# O4. After `--` every token is a pathspec, including one that looks like a flag.
dirty "$SHARED"
printf 'UNCOMMITTED-WORK\n' >"$SHARED/-h"
arm "N14-dash-h-after-terminator" 77 "$SHARED" -- checkout -- -h
[[ "$(cat "$SHARED/-h")" == *UNCOMMITTED-WORK* ]] &&
  assert "N14-dash-h-after-terminator" "the -h help scan stops at the terminator, so the FILE named -h survives" 0 ||
  assert "N14-dash-h-after-terminator" "the -h help scan stops at the terminator, so the FILE named -h survives" 1

echo
echo "==========================================================================="
echo "POSITIVE — the guard must permit; a gate that refuses everything is not one"
echo "==========================================================================="

# --- rule (c): the shared REMOTE namespace -------------------------------
# A bare repo stands in for origin. The refusal arms never reach it (the guard
# refuses before running), but the permit arms must actually push, or "permitted"
# would only mean "not refused" and would not show the command works.
BARE="$WORK/origin.git"
"$REAL_GIT" init -q --bare "$BARE"
"$REAL_GIT" -C "$SHARED" remote add origin "$BARE" 2>/dev/null || \
  "$REAL_GIT" -C "$SHARED" remote set-url origin "$BARE"
"$REAL_GIT" -C "$SHARED" push -q origin main 2>/dev/null ||
  die_cannot_evaluate "could not seed the stand-in remote; the push arms would be vacuous"
bare_before="$("$REAL_GIT" -C "$BARE" rev-parse refs/heads/main)" ||
  die_cannot_evaluate "stand-in remote has no main after seeding"

arm "N15-push-delete" 77 "$SHARED" -- push origin --delete main
[[ "$("$REAL_GIT" -C "$BARE" rev-parse refs/heads/main 2>/dev/null)" == "$bare_before" ]] &&
  assert "N15-push-delete" "the remote branch still exists after the refusal" 0 ||
  assert "N15-push-delete" "the remote branch still exists after the refusal" 1

arm "N16-push-force" 77 "$SHARED" -- push --force origin main
[[ "$LAST_OUT" == *"c/shared-remote"* ]] &&
  assert "N16-push-force" "the refusal cites the remote namespace, not the local ref store" 0 ||
  assert "N16-push-force" "the refusal cites the remote namespace, not the local ref store" 1

# The lease is the alternative the refusal offers, so it had better work.
arm "P12-push-force-with-lease" 0 "$SHARED" -- push --force-with-lease origin main
[[ "$LAST_STATUS" == 0 ]] &&
  assert "P12-push-force-with-lease" "--force-with-lease is permitted: the offered alternative is real" 0 ||
  assert "P12-push-force-with-lease" "--force-with-lease is permitted: the offered alternative is real" 1

# And the justification moved: branch -D is still refused, on the narrower
# ground, and must no longer claim the shared namespace.
arm "N17-branch-D-cites-local-ground" 77 "$SHARED" -- branch -D nonexistent-branch
[[ "$LAST_OUT" == *"a/local-refs"* && "$LAST_OUT" != *"every agent in this project shares"* ]] &&
  assert "N17-branch-D-cites-local-ground" "branch -D no longer claims a shared ref namespace it cannot reach" 0 ||
  assert "N17-branch-D-cites-local-ground" "branch -D no longer claims a shared ref namespace it cannot reach" 1

# --cached is the discriminating flag for rm, exactly as -n is for clean.
dirty "$SHARED"
arm "P10-rm-cached-is-permitted" 0 "$SHARED" -- rm --cached -q tracked.txt
[[ -e "$SHARED/tracked.txt" ]] && assert "P10-rm-cached-is-permitted" "rm --cached is permitted and leaves the file on disk" 0 ||
  assert "P10-rm-cached-is-permitted" "rm --cached is permitted and leaves the file on disk" 1
"$REAL_GIT" -C "$SHARED" reset -q >/dev/null 2>&1

# A `!`-alias needs no expansion here: any git it runs re-enters this shim on
# PATH and is judged on its own merits. Passing it through is the correct
# answer, not a gap.
arm "P11-shell-alias-passes-through" 0 "$SHARED" -- sh
[[ "$LAST_OUT" == *"SHELL-ALIAS"* ]] &&
  assert "P11-shell-alias-passes-through" "a !-alias runs; its nested git is covered by re-entry, not by expansion" 0 ||
  assert "P11-shell-alias-passes-through" "a !-alias runs; its nested git is covered by re-entry, not by expansion" 1

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
echo "TWO SHIMS ON ONE PATH — the failure mode that produces no output at all"
echo "==========================================================================="
echo
echo "A second copy of the shim on PATH used to be a fork bomb: copy A skips its"
echo "own directory, finds copy B, execs it; B skips its own, finds A, execs it."
echo "Measured before the fix: timeout killed it at rc=124 with EMPTY stderr. A"
echo "guard that hangs silently is worse than one that refuses with a reason, so"
echo "the shim now recognises its own kind by a marker in the file rather than by"
echo "path, and bounds the hops as a backstop."

TWOSHIM="$WORK/twoshim"
mkdir -p "$TWOSHIM/a" "$TWOSHIM/b"
cp "$SHIM" "$TWOSHIM/a/git"
cp "$SHIM" "$TWOSHIM/b/git"
chmod +x "$TWOSHIM/a/git" "$TWOSHIM/b/git"
real_git_dir="${REAL_GIT%/*}"

# A minimal bin holding ONLY bash, so PATH_OVERRIDE can be exhaustive without
# breaking the shim's own `#!/usr/bin/env bash`. Building it explicitly rather
# than reusing /usr/bin keeps the control honest: the C1 arm below asserts that
# NO real git is reachable, and that assertion is worthless if the directory
# supplying bash also happens to supply git.
MINBIN="$WORK/minbin"
mkdir -p "$MINBIN"
bash_path="$(command -v bash)" || bash_path=""
if [[ -z "$bash_path" ]]; then
  echo "wsguard-selftest: no bash on PATH; the two-shim arms cannot be built" >&2
  exit 2
fi
ln -s "$bash_path" "$MINBIN/bash"
if [[ -e "$MINBIN/git" ]]; then
  echo "wsguard-selftest: $MINBIN unexpectedly contains git; the C1 control would be vacuous" >&2
  exit 2
fi

# CONTROL FIRST. If PATH holds nothing but shim copies, the marker check must
# reject BOTH and the shim must report that it cannot find a real git. Without
# the content check this configuration is the fork bomb itself, so a clean 78
# here is direct evidence that the marker is doing the work — and P9 below would
# otherwise be passing for some other reason.
PATH_OVERRIDE="$TWOSHIM/a:$TWOSHIM/b:$MINBIN"
arm "C1-only-shims-on-path" 78 "$SHARED" -- checkout -- tracked.txt
[[ "$LAST_OUT" == *"could not find a real git"* ]] &&
  assert "C1-only-shims-on-path" "a shim copy is recognised as a shim, not mistaken for the real git" 0 ||
  assert "C1-only-shims-on-path" "a shim copy is recognised as a shim, not mistaken for the real git" 1
(( LAST_STATUS != 124 )) &&
  assert "C1-only-shims-on-path" "it answered instead of spinning (124 is the timeout kill)" 0 ||
  assert "C1-only-shims-on-path" "it answered instead of spinning (124 is the timeout kill)" 1

PATH_OVERRIDE="$TWOSHIM/a:$TWOSHIM/b:$MINBIN:$real_git_dir"
arm "P9-two-shims-then-real-git" 0 "$SHARED" -- --version
[[ "$LAST_OUT" == *"git version"* ]] &&
  assert "P9-two-shims-then-real-git" "two shims on PATH resolve THROUGH to the real git" 0 ||
  assert "P9-two-shims-then-real-git" "two shims on PATH resolve THROUGH to the real git" 1

# The backstop, tested directly rather than trusted. It only fires if the marker
# check has already failed, so it cannot be reached by any ordinary path.
HOPS_OVERRIDE=99
arm "U3-hop-cap" 78 "$SHARED" -- status --porcelain
[[ "$LAST_OUT" == *"entered 99 times"* ]] &&
  assert "U3-hop-cap" "the backstop names the loop instead of hanging" 0 ||
  assert "U3-hop-cap" "the backstop names the loop instead of hanging" 1

echo
echo "==========================================================================="
printf 'arms run              : %d  (%d refusal, %d cannot-evaluate, %d permit)\n' \
  "$arms_run" "$arms_refuse" "$arms_cannot" "$arms_permit"
printf 'exit-status mismatches: %d/%d\n' "$arms_mismatched" "$arms_run"
printf 'post-condition checks : %d  failed: %d\n' "$checks_run" "$checks_failed"
# Not printed as a ratio. Every control calls die_cannot_evaluate on failure, so
# the denominator can only ever equal the numerator and "6/6" would be a
# tautology dressed as a measurement. The load-bearing statement is the second
# line: a control that does not reproduce ends the run at 2.
printf 'harness controls      : %d reproduced with the real git\n' "$controls_run"
printf '                        (any that had not would have exited 2, not 0)\n'
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
