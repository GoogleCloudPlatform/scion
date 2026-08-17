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
# --prove-it: negative controls on this harness. ONE PER COUNTER.
#
# A green selftest is worth exactly as much as the harness's ability to go red.
# Before trusting a pass, run the whole suite with an expectation deliberately
# falsified and require it to FAIL. If the falsified run passes, the harness is
# not measuring anything and the honest answer is 2, not 0.
#
# THE FIRST VERSION OF THIS PROVED HALF OF WHAT IT CLAIMED. It flipped one
# arm's expected EXIT STATUS, which exercises `arms_mismatched` and nothing
# else. The post-conditions — the checks that say the WORK SURVIVED, which are
# the whole reason this guard exists — run through a separate counter,
# `checks_failed`, that the negative control never touched. gd-wsg-rev-2
# measured the consequence by neutering `assert` so that every post-condition
# passed unconditionally:
#
#     assert() { ok=0; ... }   ./selftest.sh --prove-it
#       -> "negative control PASSED — the suite below is capable of failing."
#       -> "PASS — 33 arms, 37 post-conditions"                        rc=0
#
# The suite announced that it was capable of failing and then reported 37 dead
# post-conditions as green. It also controlled its own finding, which is why
# this is precise rather than a general accusation: mutating `still_dirty` to
# claim the work had been destroyed gave rc=1 and 8 named failures, so the
# assert path works — --prove-it simply never looked at it.
#
# A HEADLINE WITH TWO NUMBERS IN IT NEEDS TWO NEGATIVE CONTROLS. Anything less
# invites the reader to believe the control covers both, and it covered the
# first. So: two injection sites, one per counter, and each run must report the
# counter it was aimed at going red AND THE OTHER ONE STAYING GREEN. Requiring
# the other to stay green is what makes this a test of the counters rather than
# a test of the exit code, which is a single number they both feed.
# ---------------------------------------------------------------------------
prove_one() {                 # <inject-kind> <expected-counter-line>
  local kind="$1" want="$2" log status=0 body
  log="$(mktemp)" || die_cannot_evaluate "mktemp failed"
  WSGUARD_SELFTEST_INJECT="$kind" "$SELF_DIR/selftest.sh" >"$log" 2>&1 || status=$?
  body="$(cat "$log")"
  if (( status != 1 )); then
    echo "wsguard-selftest: negative control [$kind] exited $status, expected 1." >&2
    if (( status == 0 )); then
      echo "wsguard-selftest: a run with a falsified $kind expectation still PASSED, so this" >&2
      echo "wsguard-selftest: harness cannot go red on that axis and its green means nothing." >&2
    fi
    printf '%s\n' "$body" | sed 's/^/  | /' >&2
    rm -f "$log"
    exit 2
  fi
  # Bash glob containment, not grep — no external tool decides this verdict.
  if [[ "$body" != *"$want"* ]]; then
    echo "wsguard-selftest: negative control [$kind] exited 1, but for the WRONG REASON." >&2
    echo "wsguard-selftest: expected the counter line to contain: $want" >&2
    echo "wsguard-selftest: an injection that reddens a counter it was not aimed at proves" >&2
    echo "wsguard-selftest: nothing about the counter it was aimed at." >&2
    printf '%s\n' "$body" | sed 's/^/  | /' >&2
    rm -f "$log"
    exit 2
  fi
  rm -f "$log"
  echo "wsguard-selftest: negative control [$kind] PASSED — exit 1, and $want"
}

if [[ "${1:-}" == "--prove-it" ]]; then
  prove_one arm   "counters: arms_mismatched=1 checks_failed=0"
  prove_one check "counters: arms_mismatched=0 checks_failed=1"
  echo "wsguard-selftest: both counters are demonstrably capable of failing. Running for real."
  echo
  shift
fi

# Back-compatible with the single-axis variable the earlier round used, so that
# an external caller pinned to the old name still gets the arm injection rather
# than silently getting no injection at all — which would read as a pass.
INJECT="${WSGUARD_SELFTEST_INJECT:-}"
if [[ -z "$INJECT" && "${WSGUARD_SELFTEST_INJECT_FAILURE:-}" == "1" ]]; then
  INJECT=arm
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

# set_alias <repo> <name> <value> — write an alias fixture AND READ IT BACK.
#
# The round-4 record named three contaminated fixtures and stated the common
# shape as "the harness had no control on itself". The remedy went to the index
# and content fixtures — hard resets, and asserting the COMMITTED bytes rather
# than "not the dirty string" — and never reached the ALIAS fixtures, which were
# the third of the three. gd-wsg-rev-3 found that gap by pointing the alias
# writes at a config key that takes effect and means nothing:
#
#   suite: PASS — 49 arms, 61 post-conditions, 0 failures      identical headline
#
# N29 and N31 carry the entire C4 claim — an alias cannot demote a watched
# builtin — and both pass exactly as well when no alias was ever established,
# because a refusal arm cannot tell "refused despite the alias" from "refused
# with no alias present". Every one of the 20 alias writes in this suite now
# goes through here, so an alias that does not take is a hard 2 rather than a
# quieter green.
set_alias() { # <repo> <name> <value>
  local repo="$1" name="$2" value="$3" got
  "$REAL_GIT" -C "$repo" config "alias.$name" "$value" ||
    die_cannot_evaluate "could not write alias.$name in $repo"
  got="$("$REAL_GIT" -C "$repo" config --get "alias.$name" 2>/dev/null)"
  [[ "$got" == "$value" ]] ||
    die_cannot_evaluate "alias.$name did not take in $repo: wrote '$value', read back '$got' — the arms that depend on it would degrade to plain refusal arms and pass"
}

# require_alias_for <repo> <command-line> — assert that the FIRST WORD of the
# command about to be run has an alias defined.
#
# set_alias alone does not close R3, and finding out why cost a falsifier. It
# reads back the key it just wrote, deriving the key from the SAME VARIABLE as
# the write — so renaming both together (`alias.$sv` -> `alias.NOTSET$sv`)
# leaves it perfectly green: the alias did take, just not on the name the arm
# depends on. That is the self-reference defect from R1 one level down, in the
# control I had just added to fix a contaminated fixture.
#
# The independent anchor is the COMMAND THE ARM ACTUALLY RUNS. An alias-shadow
# arm is only meaningful if an alias exists for the verb being invoked, so the
# check reads its key off the command line rather than off the write. Now a
# mutation has to change the command too — at which point it is testing
# something else and saying so.
require_alias_for() { # <repo> <command-line>
  local repo="$1" cmdline="$2" verb got
  verb="${cmdline%% *}"
  [[ -n "$verb" ]] ||
    die_cannot_evaluate "require_alias_for got an empty command line"
  got="$("$REAL_GIT" -C "$repo" config --get "alias.$verb" 2>/dev/null)"
  [[ -n "$got" ]] ||
    die_cannot_evaluate "no alias is defined for '$verb', but the arm about to run '$cmdline' exists to prove an alias cannot demote it — with no alias present it is just another refusal arm and would pass"
}

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
set_alias "$CONTROL" "co" checkout
set_alias "$CONTROL" "sh" '!echo SHELL-ALIAS'
dirty "$CONTROL"
control_status=0
"$REAL_GIT" -C "$CONTROL" co -- tracked.txt || control_status=$?
control_content="$(cat "$CONTROL/tracked.txt")"
if (( control_status != 0 )) || [[ "$control_content" == *UNCOMMITTED-WORK* ]]; then
  die_cannot_evaluate "control 'an alias reaches checkout' did not reproduce (status=$control_status, content='$control_content')"
fi
control "alias-reaches-checkout" "reproduced: \`git co\` erased the modification, so the alias is a real path to the hazard"

# F1. The original version of this control concluded the OPPOSITE, and the way
# it went wrong is the reason for the reset below.
#
# It reused a fixture whose index had already been rewritten by earlier arms, so
# the DIRTY content was also the STAGED content. `checkout -- f` then restored
# the file to bytes identical to the ones already on disk, the post-condition
# "the content did not change" held, and the arm reported that git had not
# expanded the alias. It had. THE POST-CONDITION COULD NOT DISTINGUISH "THE
# COMMAND NEVER RAN" FROM "THE COMMAND RAN AND WROTE THE SAME BYTES", and a
# post-condition that cannot separate those is not a measurement.
#
# Two changes make it one: hard-reset the index before the arm, and assert the
# content is now the COMMITTED string rather than merely "not the dirty string".
"$REAL_GIT" -C "$CONTROL" reset -q --hard >/dev/null 2>&1
set_alias "$CONTROL" "a" co
for _d in 1 2 3 4 5 6; do
  if (( _d == 1 )); then
    set_alias "$CONTROL" "d1" co
  else
    set_alias "$CONTROL" "d$_d" "d$(( _d - 1 ))"
  fi
done
dirty "$CONTROL"
control_status=0
"$REAL_GIT" -C "$CONTROL" d6 -- tracked.txt || control_status=$?
control_content="$(cat "$CONTROL/tracked.txt")"
if (( control_status != 0 )) || [[ "$control_content" != *"committed content"* ]]; then
  die_cannot_evaluate "control 'git chains aliases' did not reproduce (status=$control_status, content='$control_content') — if this says the file is unchanged, check the index is clean before blaming git"
fi
control "alias-chains-to-depth-6" "reproduced: a six-deep chain reached checkout and restored the COMMITTED bytes"

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
MODE_OVERRIDE=""

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
    if [[ -n "$MODE_OVERRIDE" ]]; then
      if [[ "$MODE_OVERRIDE" == "unset" ]]; then
        unset SCION_WORKSPACE_MODE
      else
        export SCION_WORKSPACE_MODE="$MODE_OVERRIDE"
      fi
    else
      export SCION_WORKSPACE_MODE=shared-plain
    fi
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
  # Injection site 1 of 2: the expected EXIT STATUS. Reddens arms_mismatched
  # and must leave checks_failed alone, which is why it flips the expectation
  # rather than the command.
  if [[ "$INJECT" == "arm" && "$name" == "N1-checkout-pathspec" ]]; then
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
  MODE_OVERRIDE=""
}

# assert <arm-name> <description> <condition-result 0|1>
#
# Injection site 2 of 2. It inverts the FIRST post-condition of the run and
# then disarms itself, so exactly one check goes red and `checks_failed=1` is
# an exact expectation rather than a lower bound. Inverting the RESULT rather
# than stubbing the function is deliberate: stubbing would also disable every
# other check, and a control that disables the thing it is measuring cannot
# tell you the thing was working.
inject_check_fired=0
assert() {
  local name="$1" desc="$2" ok="$3"
  if [[ "$INJECT" == "check" ]] && (( inject_check_fired == 0 )); then
    inject_check_fired=1
    ok=$(( 1 - ok ))
    desc="$desc [FALSIFIED BY --prove-it]"
  fi
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

# guard_silent <arm-name> — the guard emitted NOTHING on a permitted command.
#
# gd-wsg-rev-3 asked (Q3) what the permit arms actually assert, and the honest
# answer was: rc=0, and for two of them not even that. Every one of the 14 was
# scored purely on "did not refuse". That is the weaker half of the property.
# A guard that prints a warning, a deprecation, or a stray diagnostic on every
# `git status` is one an agent learns to read past, and the day it prints
# REFUSED nobody sees it. The refusal arms are only worth what the permit arms
# cost, so silence is a post-condition, not an aesthetic.
#
# The marker is "wsguard:" because that is the prefix on EVERY line the shim
# writes — refusals, cannot-evaluates, override notices and the shell-alias
# note alike. Matching the prefix rather than any single message means a new
# diagnostic added later is caught by these arms without anyone remembering to
# extend a list.
#
# Two permit arms are deliberately NOT silent and do not call this: P8 is the
# audited override, whose entire purpose is to announce itself, and P11 prints
# the SHELL-ALIAS note. Both assert on that output directly. The remaining 12
# must say nothing at all.
guard_silent() {
  local name="$1"
  local desc="the guard is SILENT on a permitted command: no output to read past"
  if [[ "$LAST_OUT" == *"wsguard:"* ]]; then
    assert "$name" "$desc" 1
  else
    assert "$name" "$desc" 0
  fi
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
set_alias "$SHARED" "co" checkout
set_alias "$SHARED" "sh" '!echo SHELL-ALIAS'
# F1: git chains aliases to arbitrary depth. F1b: an expansion may LEAD with a
# global option, or quote the dispatched word.
set_alias "$SHARED" "a" co
set_alias "$SHARED" "d1" co
for _d in 2 3 4 5 6; do
  set_alias "$SHARED" "d$_d" "d$(( _d - 1 ))"
done
set_alias "$SHARED" "r1" rm
set_alias "$SHARED" "r2" r1
set_alias "$SHARED" "g" '-c core.quotepath=false checkout'
set_alias "$SHARED" "q" '"checkout"'
set_alias "$SHARED" "safe" status
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

# F1. Depth two, depth six, and a chain ending at rm. Each is preceded by a hard
# reset so that "the work survived" cannot be satisfied by a stale index.
"$REAL_GIT" -C "$SHARED" reset -q --hard >/dev/null 2>&1
dirty "$SHARED"
arm "N18-alias-chain-depth-2" 77 "$SHARED" -- a -- tracked.txt
still_dirty && assert "N18-alias-chain-depth-2" "a two-deep chain is resolved to checkout and refused" 0 ||
  assert "N18-alias-chain-depth-2" "a two-deep chain is resolved to checkout and refused" 1

"$REAL_GIT" -C "$SHARED" reset -q --hard >/dev/null 2>&1
dirty "$SHARED"
arm "N19-alias-chain-depth-6" 77 "$SHARED" -- d6 -- tracked.txt
still_dirty && assert "N19-alias-chain-depth-6" "depth is not bounded at one: a six-deep chain is refused" 0 ||
  assert "N19-alias-chain-depth-6" "depth is not bounded at one: a six-deep chain is refused" 1
[[ "$LAST_OUT" == *"resolves to"* && "$LAST_OUT" == *"checkout"* ]] &&
  assert "N19-alias-chain-depth-6" "the refusal names the resolution, so the operator can find a word they never typed" 0 ||
  assert "N19-alias-chain-depth-6" "the refusal names the resolution, so the operator can find a word they never typed" 1

"$REAL_GIT" -C "$SHARED" reset -q --hard >/dev/null 2>&1
dirty "$SHARED"
arm "N20-alias-chain-to-rm" 77 "$SHARED" -- r2 -f tracked.txt
[[ -e "$SHARED/tracked.txt" ]] && assert "N20-alias-chain-to-rm" "a chain ending at rm is refused and the file survives" 0 ||
  assert "N20-alias-chain-to-rm" "a chain ending at rm is refused and the file survives" 1

# F1b. The dispatched word is not always the first word.
"$REAL_GIT" -C "$SHARED" reset -q --hard >/dev/null 2>&1
dirty "$SHARED"
arm "N21-alias-leading-global-option" 77 "$SHARED" -- g -- tracked.txt
still_dirty && assert "N21-alias-leading-global-option" "an expansion leading with -c is resolved past the option" 0 ||
  assert "N21-alias-leading-global-option" "an expansion leading with -c is resolved past the option" 1

"$REAL_GIT" -C "$SHARED" reset -q --hard >/dev/null 2>&1
dirty "$SHARED"
arm "N22-alias-quoted-word" 77 "$SHARED" -- q -- tracked.txt
still_dirty && assert "N22-alias-quoted-word" "git splits aliases with shell quoting, so a quoted word still resolves" 0 ||
  assert "N22-alias-quoted-word" "git splits aliases with shell quoting, so a quoted word still resolves" 1

# The cap is fail-closed, and it is tested rather than trusted. git refuses a
# cyclic alias itself, but this guard cannot tell a cycle from a chain deeper
# than 10, and "it is probably the harmless one" is the reasoning the guard
# exists to refuse.
set_alias "$SHARED" "loop1" loop2
set_alias "$SHARED" "loop2" loop1
"$REAL_GIT" -C "$SHARED" reset -q --hard >/dev/null 2>&1
dirty "$SHARED"
arm "U4-alias-cap" 78 "$SHARED" -- loop1 -- tracked.txt
still_dirty && assert "U4-alias-cap" "an unresolvable alias is 78 and NOT RUN, not a passthrough" 0 ||
  assert "U4-alias-cap" "an unresolvable alias is 78 and NOT RUN, not a passthrough" 1

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

# ---------------------------------------------------------------------------
# C3. A global option that takes a SEPARATE value and that the guard's table
# does not know. `--attr-source` is real, takes a separate value in git 2.54.0,
# and is deliberately absent from opt_takes_value — see the comment there. The
# guard must refuse anyway, via the union of candidate readings.
# ---------------------------------------------------------------------------
"$REAL_GIT" -C "$SHARED" reset -q --hard >/dev/null 2>&1
dirty "$SHARED"
arm "N23-unknown-option-separated-value" 77 "$SHARED" -- --attr-source HEAD checkout -- tracked.txt
still_dirty && assert "N23-unknown-option-separated-value" "an option of unknown arity does not hide the verb behind it" 0 ||
  assert "N23-unknown-option-separated-value" "an option of unknown arity does not hide the verb behind it" 1
[[ "$LAST_OUT" == *"not an option this guard knows the shape of"* ]] &&
  assert "N23-unknown-option-separated-value" "the refusal says which reading it rested on, instead of looking like a misparse" 0 ||
  assert "N23-unknown-option-separated-value" "the refusal says which reading it rested on, instead of looking like a misparse" 1

dirty "$SHARED"
arm "N24-unknown-option-then-another" 77 "$SHARED" -- --attr-source HEAD --literal-pathspecs checkout -- tracked.txt
still_dirty && assert "N24-unknown-option-then-another" "the second reading resumes the scan rather than taking the next token blind" 0 ||
  assert "N24-unknown-option-then-another" "the second reading resumes the scan rather than taking the next token blind" 1

# GENERATED, NOT ENUMERATED. The reviewer's central criticism of this suite was
# that every arm was one-per-known-bug — N12 tests a depth-one alias because
# the bug was depth one — so no arm generalised its class and four more members
# of the class survived to round 4. These next two arms are the answer: they
# construct their inputs from the guard's OWN table plus a name the table does
# not contain, and assert a property that must hold across the class.
#
# The property: for every global option, the SEPARATED form must classify the
# same as the ATTACHED form. It was the divergence between those two that made
# C3 visible in the first place.
#
# THE ASSERTION MUST CARRY THE DISCRIMINATION. This arm first read "status is
# not 0", and that was wrong in the way R3 was wrong: a nonzero status can come
# from the guard refusing OR from real git rejecting the option, and "not 0"
# cannot tell those apart. Measured, with the union deliberately removed:
#
#   --attr-source                rc=128  tracked.txt GONE   guard silent
#   --wsguard-not-a-real-option  rc=129  tracked.txt DIRTY  guard silent
#
# The first line is a WORKING-TREE MUTATION — the file the guard exists to
# protect was destroyed — and the arm went green, because 128 is not 0. So the
# one arm whose job was to generalise the class was the one arm that could not
# see the class break. Two things fix it, and both are properties, not statuses:
# the guard must have SPOKEN, and the content must still be there.
optlist=(-C -c --git-dir --work-tree --namespace --config-env --attr-source --wsguard-not-a-real-option)
gen_class_ok=1
gen_class_detail=""
gen_class_spoke=0
for gopt in "${optlist[@]}"; do
  "$REAL_GIT" -C "$SHARED" reset -q --hard >/dev/null 2>&1
  dirty "$SHARED"
  run_guarded "$SHARED" "$gopt" "$SHARED" checkout -- tracked.txt
  sep_status="$LAST_STATUS"
  # (1) The guard, not git, must be what stopped this. 77 refused or 78
  #     cannot-evaluate, and the banner present so the code cannot be a
  #     coincidence of some other exit path.
  case "$sep_status:$LAST_OUT" in
    77:*"wsguard: REFUSED"*|78:*"wsguard: CANNOT EVALUATE"*)
      gen_class_spoke=$(( gen_class_spoke + 1 )) ;;
    *)
      gen_class_ok=0
      gen_class_detail="$gen_class_detail $gopt=rc$sep_status/guard-silent" ;;
  esac
  # (2) The safety property itself, independent of any status: the uncommitted
  #     content is still on disk.
  if ! still_dirty; then
    gen_class_ok=0
    gen_class_detail="$gen_class_detail $gopt=CONTENT-LOST"
  fi
done
# Denominator control: if the loop measured nothing, the two checks above are
# vacuously true and the arm would report success having tested no options.
if (( ${#optlist[@]} == 0 )); then
  die_cannot_evaluate "N25 option list is empty: the class assertion would pass without measuring anything"
fi
(( gen_class_ok == 1 )) &&
  assert "N25-global-option-class" "every option in the guard's table plus one it has never heard of: the guard itself stops the separated form and the content survives (${gen_class_spoke}/${#optlist[@]} options, guard spoke on each)" 0 ||
  assert "N25-global-option-class" "every option in the guard's table plus one it has never heard of: the guard itself stops the separated form and the content survives — LEAKED:$gen_class_detail" 1

# N25 covers the class of unknown options at COUNT ONE. gd-wsg-rev-3 found the
# other axis: the union was a single resumed scan, so it produced exactly two
# candidate readings while the number of readings grows with the number of
# unknown-arity options. At two, both candidates land on option VALUES and the
# verb is never classified — measured on the shipped head, work destroyed at
# rc=0 with the guard silent, and reproduced here before the fix went in.
#
# So the arm sweeps a RANGE of depths rather than adding "the n=2 case". n=1 is
# included deliberately: it is the reading that already worked, and if a future
# change breaks it this arm should say so rather than only testing the new part.
# The verbs vary too, because the bypass was never specific to checkout.
attr_ok=1
attr_detail=""
attr_n=0
for depth in 1 2 3 4; do
  for spec in "checkout|-- tracked.txt" "reset|--hard HEAD" "rm|-f tracked.txt"; do
    verb="${spec%%|*}"; verb_args="${spec#*|}"
    "$REAL_GIT" -C "$SHARED" reset -q --hard >/dev/null 2>&1
    dirty "$SHARED"
    gopts=()
    for (( d = 0; d < depth; d++ )); do gopts+=(--attr-source HEAD); done
    # shellcheck disable=SC2086
    run_guarded "$SHARED" "${gopts[@]}" "$verb" $verb_args
    attr_n=$(( attr_n + 1 ))
    if [[ "$LAST_STATUS" != 77 || "$LAST_OUT" != *"wsguard: REFUSED"* ]]; then
      attr_ok=0
      attr_detail="$attr_detail n=$depth/$verb=rc$LAST_STATUS"
    elif ! still_dirty; then
      attr_ok=0
      attr_detail="$attr_detail n=$depth/$verb=CONTENT-LOST"
    fi
  done
done
if (( attr_n == 0 )); then
  die_cannot_evaluate "N34 swept no depth/verb combinations; the assertion would be vacuous"
fi
(( attr_ok == 1 )) &&
  assert "N35-unknown-option-repeated" "repeating an unknown-arity option does not exhaust the union: the verb is still found and refused at every depth ($attr_n combinations, depths 1-4)" 0 ||
  assert "N35-unknown-option-repeated" "repeating an unknown-arity option does not exhaust the union — LEAKED:$attr_detail" 1

# GENERATED, second: an alias chain of depth N, built here rather than written
# out. gd-wsg-rev-2 was explicit that SIX WAS ITS FIXTURE DEPTH AND NOT A
# MEASURED LIMIT of git, and that a number lifted from a fixture and recorded
# as a property is how the depth-one cap came to exist. So the arm takes its
# depth from a variable, and the assertion is on the hop count the guard
# reports, not on any particular number being special.
GEN_DEPTH=9
set_alias "$SHARED" "gen1" checkout
for (( n = 2; n <= GEN_DEPTH; n++ )); do
  set_alias "$SHARED" "gen$n" "gen$(( n - 1 ))"
done
"$REAL_GIT" -C "$SHARED" reset -q --hard >/dev/null 2>&1
dirty "$SHARED"
arm "N26-alias-chain-generated-depth-$GEN_DEPTH" 77 "$SHARED" -- "gen$GEN_DEPTH" -- tracked.txt
still_dirty && assert "N26-alias-chain-generated-depth-$GEN_DEPTH" "a chain of generated depth $GEN_DEPTH is resolved to a fixed point and refused" 0 ||
  assert "N26-alias-chain-generated-depth-$GEN_DEPTH" "a chain of generated depth $GEN_DEPTH is resolved to a fixed point and refused" 1
[[ "$LAST_OUT" == *"after $GEN_DEPTH expansion(s)"* ]] &&
  assert "N26-alias-chain-generated-depth-$GEN_DEPTH" "the guard reports the depth it actually walked, so the arm cannot pass on a shorter walk" 0 ||
  assert "N26-alias-chain-generated-depth-$GEN_DEPTH" "the guard reports the depth it actually walked, so the arm cannot pass on a shorter walk" 1

# ---------------------------------------------------------------------------
# F3. Two plumbing commands that overwrite the working tree. Filed FYI; closed
# anyway, because the caller of plumbing is usually a script and a script
# destroys a shared tree exactly as thoroughly as a person does.
# ---------------------------------------------------------------------------
"$REAL_GIT" -C "$SHARED" reset -q --hard >/dev/null 2>&1
dirty "$SHARED"
arm "N27-checkout-index-force" 77 "$SHARED" -- checkout-index -f -a
still_dirty && assert "N27-checkout-index-force" "checkout-index -f overwrites the tree from the index, and is refused" 0 ||
  assert "N27-checkout-index-force" "checkout-index -f overwrites the tree from the index, and is refused" 1

dirty "$SHARED"
arm "N28-read-tree-u-reset" 77 "$SHARED" -- read-tree -u --reset HEAD
still_dirty && assert "N28-read-tree-u-reset" "read-tree -u updates the WORKING TREE, and is refused" 0 ||
  assert "N28-read-tree-u-reset" "read-tree -u updates the WORKING TREE, and is refused" 1

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
guard_silent "P12-push-force-with-lease"
[[ "$LAST_STATUS" == 0 ]] &&
  assert "P12-push-force-with-lease" "--force-with-lease is permitted: the offered alternative is real" 0 ||
  assert "P12-push-force-with-lease" "--force-with-lease is permitted: the offered alternative is real" 1

# C4. An alias whose NAME SHADOWS A BUILTIN. Git ignores it and runs the
# builtin; the guard used to honour it and reclassify a real `push` as `log`,
# then wave it through onto the shared remote. The post-condition is the REMOTE
# REF, not the working tree — a push arm asserting on tracked.txt would pass
# for a reason that has nothing to do with the thing being tested.
set_alias "$SHARED" "push" log
require_alias_for "$SHARED" "push --force origin main:main"
arm "N29-alias-shadowing-a-builtin" 77 "$SHARED" -- push --force origin main:main
[[ "$("$REAL_GIT" -C "$BARE" rev-parse refs/heads/main 2>/dev/null)" == "$bare_before" ]] &&
  assert "N29-alias-shadowing-a-builtin" "the remote ref is untouched: an alias cannot demote a watched builtin" 0 ||
  assert "N29-alias-shadowing-a-builtin" "the remote ref is untouched: an alias cannot demote a watched builtin" 1
[[ "$LAST_OUT" == *"c/shared-remote"* ]] &&
  assert "N29-alias-shadowing-a-builtin" "it is refused AS a push, not as whatever the alias named" 0 ||
  assert "N29-alias-shadowing-a-builtin" "it is refused AS a push, not as whatever the alias named" 1
"$REAL_GIT" -C "$SHARED" config --unset alias.push

# ...and N29 on its own is one-per-known-bug, which is the criticism that
# produced this round. C4 was FOUND via push, but it is a property of every
# watched verb: the guard must dispatch a name the way git dispatches it, and
# git never lets an alias shadow a builtin. Mutation-measured, one verb removed
# from is_watched_verb at a time, alias.<verb>=log defined, list intact vs
# dropped — every one of the ten bypasses, and eight of them destroy the
# uncommitted work at rc=0 with the guard silent:
#
#   checkout restore reset switch stash checkout-index read-tree  content
#     -> COMMITTED (uncommitted work gone)      rm -> GONE (file deleted)
#   clean -fd and branch -f main left the tree alone for reasons that have
#   nothing to do with the guard — it was bypassed in those two as well.
#
# So the list is load-bearing at every entry, and one arm covered one entry.
# This arm is generated from the guard's OWN list: it cannot fall behind a verb
# added later, which is how the single-entry version fell behind in the first
# place.
mapfile -t shadow_verbs < <(
  /usr/bin/sed -n '/^is_watched_verb()/,/^}/p' "$SHIM_DIR/git" |
  /usr/bin/grep -oE '^ *[a-z|-]+\)' |
  /usr/bin/tr -d ' )' | /usr/bin/tr '|' '\n' | /usr/bin/grep -v '^$'
)
# Denominator control: an empty or implausibly short derived list would make
# every check below vacuous, and the arm would report a clean sweep of nothing.
if (( ${#shadow_verbs[@]} < 5 )); then
  die_cannot_evaluate "N31 derived only ${#shadow_verbs[@]} verbs from is_watched_verb; the sweep would be vacuous"
fi
declare -A SHADOW_ARGV=(
  [checkout]="checkout -- tracked.txt"   [restore]="restore tracked.txt"
  [reset]="reset --hard HEAD"            [clean]="clean -fd"
  [rm]="rm -f tracked.txt"               [switch]="switch -f main"
  [stash]="stash"                        [branch]="branch -f other HEAD"
  [checkout-index]="checkout-index -f -a" [read-tree]="read-tree -u --reset HEAD"
  # `fetch origin` alone is CORRECTLY permitted — rule b only fires on a
  # second positional with no colon, the FETCH_HEAD-writing form. The first
  # version of this table used the permitted shape and N31 reported it as a
  # demotion; the arm was wrong, not the guard.
  [fetch]="fetch origin main"            [push]="push --force origin main"
)
shadow_ok=1
shadow_detail=""
shadow_n=0
for sv in "${shadow_verbs[@]}"; do
  # Coverage control: a verb in the guard's list with no argv here would be
  # silently skipped, and the sweep would shrink as the guard grows.
  if [[ -z "${SHADOW_ARGV[$sv]:-}" ]]; then
    die_cannot_evaluate "N31 has no argv for watched verb '$sv': the sweep would silently skip it"
  fi
  "$REAL_GIT" -C "$SHARED" reset -q --hard >/dev/null 2>&1
  dirty "$SHARED"
  set_alias "$SHARED" "$sv" log
  require_alias_for "$SHARED" "${SHADOW_ARGV[$sv]}"
  # shellcheck disable=SC2086
  run_guarded "$SHARED" ${SHADOW_ARGV[$sv]}
  "$REAL_GIT" -C "$SHARED" config --unset "alias.$sv"
  shadow_n=$(( shadow_n + 1 ))
  if [[ "$LAST_STATUS" != 77 || "$LAST_OUT" != *"wsguard: REFUSED"* ]]; then
    shadow_ok=0
    shadow_detail="$shadow_detail $sv=rc$LAST_STATUS"
  fi
done
(( shadow_ok == 1 )) &&
  assert "N31-alias-shadowing-every-watched-verb" "an alias named after a watched builtin cannot demote it, for every verb in the guard's own list ($shadow_n verbs, derived not enumerated)" 0 ||
  assert "N31-alias-shadowing-every-watched-verb" "an alias named after a watched builtin cannot demote it, for every verb in the guard's own list — DEMOTED:$shadow_detail" 1

# N31 GENERALISES OVER ADDITIONS AND IS BLIND TO DELETIONS. It derives its
# verbs from is_watched_verb, so deleting a verb from that list shrinks the
# arm's own input and it sweeps the smaller set reporting success — measured:
# with checkout-index and read-tree removed, the whole suite stayed green while
# both were live bypasses. An arm keyed to the thing under test cannot see the
# thing under test get smaller. So the denominator has to be pinned to a
# DIFFERENT source, and there is a natural one: armed_for holds the same verbs
# for a different purpose.
#
# The invariant, and the safety-critical direction of it: every verb armed_for
# can arm MUST appear in is_watched_verb. A verb that is armed but not watched
# is precisely C4 — resolve_alias walks past it, an alias demotes it, and the
# guard permits. The converse (watched but not armed) only stops the alias walk
# early and is harmless, so it is reported rather than failed.
derive_verbs() { # <function-name>
  /usr/bin/sed -n "/^$1()/,/^}/p" "$SHIM_DIR/git" |
  /usr/bin/grep -oE '^ *[a-z|-]+\)' |
  /usr/bin/tr -d ' )' | /usr/bin/tr '|' '\n' | /usr/bin/grep -v '^$' | LC_ALL=C sort -u
}
mapfile -t watched_set < <(derive_verbs is_watched_verb)
mapfile -t armed_set   < <(derive_verbs armed_for)
if (( ${#watched_set[@]} == 0 || ${#armed_set[@]} == 0 )); then
  die_cannot_evaluate "N32 derived ${#watched_set[@]} watched and ${#armed_set[@]} armed verbs; the containment check would be vacuous"
fi
unwatched=""
for av in "${armed_set[@]}"; do
  found=0
  for wv in "${watched_set[@]}"; do [[ "$av" == "$wv" ]] && { found=1; break; }; done
  (( found == 0 )) && unwatched="$unwatched $av"
done
[[ -z "$unwatched" ]] &&
  assert "N32-armed-implies-watched" "every verb armed_for can arm is also in is_watched_verb, so no rule can be reached by a name the alias walk would demote (${#armed_set[@]} armed, ${#watched_set[@]} watched)" 0 ||
  assert "N32-armed-implies-watched" "every verb armed_for can arm is also in is_watched_verb — ARMED BUT NOT WATCHED:$unwatched" 1

# N32 WAS THE SAME SELF-REFERENCE ONE LEVEL UP, and gd-wsg-rev-3 was right about
# why: not because armed_for is derived from is_watched_verb — it is a
# physically separate case block, which is the right structure and stays — but
# because of the DIRECTION of the assertion. N31, N32 and N33 are all
# containments whose left-hand side shrinks with is_watched_verb, and A ⊆ B is
# preserved when A and B shrink TOGETHER. Deleting a verb from both case blocks
# leaves all three green.
#
# Their Mutant B: delete `switch` from is_watched_verb and armed_for.
#
#   git switch -f main    rc=0, guard SILENT, uncommitted work DESTROYED
#   suite                 PASS — 49 arms, 61 post-conditions, byte-identical
#                         headline to baseline
#
# `switch` was the only watched verb with no hardcoded arm, which is exactly why
# their other mutant died — killed by N27/N28, which are INSTANCES. The class
# was being defended by hardcoded arms with one hole in the row.
#
# The fix needs a denominator that does NOT move when the guard moves, and one
# was already in this file three lines up: SHADOW_ARGV is hand-written here, not
# derived from the shim. It had a coverage control in the ADDITION direction
# (a watched verb with no argv is a hard error); this is the DELETION direction.
# The table is now load-bearing in both, so a verb cannot leave the guard's
# watched set without someone also deleting its entry here — two edits in two
# files, which is the point.
unarmed_but_tabled=""
for tv in "${!SHADOW_ARGV[@]}"; do
  found=0
  for wv in "${watched_set[@]}"; do [[ "$tv" == "$wv" ]] && { found=1; break; }; done
  (( found == 0 )) && unarmed_but_tabled="$unarmed_but_tabled $tv"
done
[[ -z "$unarmed_but_tabled" ]] &&
  assert "N34-watched-set-has-not-shrunk" "every verb in the hand-written argv table is still watched by the guard, so a verb cannot silently leave the watched set (${#SHADOW_ARGV[@]} table verbs, independent of the shim)" 0 ||
  assert "N34-watched-set-has-not-shrunk" "every verb in the hand-written argv table is still watched by the guard — IN THE ARGV TABLE BUT NO LONGER WATCHED:$unarmed_but_tabled" 1

# The guard grew two verbs this round and the sentence in AGENTS.md that tells
# every agent what is guarded did not grow with them. That sentence is the only
# description most agents will ever read, and an agent who reads an incomplete
# list will run `git rm` believing it is unguarded. Fixing the sentence fixes
# the instance; this arm is what stops it drifting again, and it is the same
# shape as N32 — derive the set, check containment against a second source.
AGENTS_MD="$SELF_DIR/../../AGENTS.md"
if [[ ! -r "$AGENTS_MD" ]]; then
  die_cannot_evaluate "N33 cannot read $AGENTS_MD; the documentation check would be vacuous"
fi
# The paragraph, not the whole file: `git checkout main` appears elsewhere in
# AGENTS.md as advice, and matching the whole file would let the check pass on
# a mention that has nothing to do with the guard.
wsguard_para="$(/usr/bin/grep -F 'puts a `git` shim in front of the real git' "$AGENTS_MD")"
if [[ -z "$wsguard_para" ]]; then
  die_cannot_evaluate "N33 could not locate the wsguard paragraph in AGENTS.md; the check would be vacuous"
fi
# Only the REFUSED half. The paragraph ends with a list of things that are
# explicitly permitted — `git rm --cached`, `push --force-with-lease` — and a
# plain substring test over the whole paragraph counts those as documentation
# of `rm` and `push`. Measured: deleting `rm` from the refused list left this
# arm green, because `git rm --cached` further down still matched. A check that
# passes on the permitted list is a check that cannot see the refused list
# shrink.
refused_half="${wsguard_para%%It refuses only inside the shared root*}"
if [[ "$refused_half" == "$wsguard_para" ]]; then
  die_cannot_evaluate "N33 could not split the AGENTS.md paragraph at the permitted-list boundary; the check would test the wrong half"
fi
# Exact match against BACKTICKED CODE TOKENS, not a substring of the prose.
# Measured: with `rm` deleted from the refused list this arm was still green,
# because the word "armed" three clauses later contains the substring "rm". A
# two-letter verb makes that failure obvious; a longer one would have hidden it.
# So the paragraph is parsed the way it is written — as code spans — and each
# span contributes its command word, with a leading `git ` stripped.
mapfile -t doc_tokens < <(
  printf '%s' "$refused_half" |
  /usr/bin/grep -oE '`[^`]+`' | /usr/bin/tr -d '`' |
  /usr/bin/sed -E 's/^git +//' |
  /usr/bin/awk '{print $1}' | LC_ALL=C sort -u
)
if (( ${#doc_tokens[@]} == 0 )); then
  die_cannot_evaluate "N33 extracted no code spans from the AGENTS.md paragraph; the check would be vacuous"
fi
undocumented=""
for wv in "${watched_set[@]}"; do
  found=0
  for dt in "${doc_tokens[@]}"; do [[ "$wv" == "$dt" ]] && { found=1; break; }; done
  (( found == 0 )) && undocumented="$undocumented $wv"
done
[[ -z "$undocumented" ]] &&
  assert "N33-watched-set-is-documented" "every verb the guard watches is named in the AGENTS.md paragraph agents actually read (${#watched_set[@]} verbs)" 0 ||
  assert "N33-watched-set-is-documented" "every verb the guard watches is named in the AGENTS.md paragraph agents actually read — UNDOCUMENTED:$undocumented" 1

# N36. THE DOCUMENTATION AS A DELETION TRIPWIRE — the other direction of N33.
#
# N34 raised the bar from one file and two sites to two files and three sites,
# and gd-wsg-rev-3 measured that the bar is still finite: deleting `switch` from
# is_watched_verb, armed_for AND SHADOW_ARGV is a live silent bypass — measured
# here before writing this, `git switch -f other` at rc=0 with the uncommitted
# work destroyed — while the suite prints `PASS — 50 arms, 78 post-conditions`
# and exits 0. The floors cannot see it, because N31 sweeps inside a single
# assertion and N31–N34 each contribute exactly one check regardless of how big
# their sets are. A count is not a denominator.
#
# SHADOW_ARGV was the right first move and it is not enough, for a reason worth
# stating plainly: IT LIVES IN THIS FILE. Two sources in one file are two sites
# in one edit. The remaining independent source is AGENTS.md — a different file,
# written for a different audience, and the one artefact that a person removing
# a verb from the shim has no reason to touch.
#
# So N33's containment gets its converse. N33 catches a verb ADDED to the guard
# and not documented; N36 catches a verb DELETED from the guard while the
# documentation still promises it. Together they are set equality, which is the
# only shape that is blind in neither direction.
#
# THE ALLOWLIST IS THE ESCAPE HATCH AND IS TREATED AS ONE. Set equality holds if
# a deleted verb is moved into the allowlist instead, so the allowlist is pinned
# at its exact size and every entry must actually occur in the paragraph. Hiding
# a deletion now costs a third file, a fifth site, and an edit to a constant
# whose comment says what raising it does. That is no longer an accident, and
# accidents are this guard's threat model.
#
# WHAT THIS STILL DOES NOT CLOSE, stated because the last four rounds were each
# a bound that someone had declared sufficient. Deleting the verb from AGENTS.md
# as well — three files, four sites — makes N33 and N36 agree on a smaller set
# and both go green. This arm RAISES A COST; it does not prove a negative. The
# reason to believe the cost is enough is not that the number is large, it is
# that AGENTS.md is prose maintained for other agents to read and nothing about
# removing a case arm from a shell script prompts anyone to edit it. If that
# stops being true — if the paragraph ever becomes generated from the shim —
# THIS ARM SILENTLY BECOMES N31 AGAIN, and whoever generates it owes this file a
# replacement source that is independent for a reason, not by coincidence.
DOC_NON_VERBS=(git .git FETCH_HEAD)
EXPECT_DOC_NON_VERBS=3
if (( ${#DOC_NON_VERBS[@]} != EXPECT_DOC_NON_VERBS )); then
  die_cannot_evaluate "N36's allowlist has ${#DOC_NON_VERBS[@]} entries, not $EXPECT_DOC_NON_VERBS — growing it is how a deleted verb gets hidden from this arm, so it is pinned, not bounded"
fi
for nv in "${DOC_NON_VERBS[@]}"; do
  nv_found=0
  for dt in "${doc_tokens[@]}"; do [[ "$nv" == "$dt" ]] && { nv_found=1; break; }; done
  (( nv_found == 0 )) &&
    die_cannot_evaluate "N36 allowlists '$nv' but the AGENTS.md paragraph no longer contains it; a dead allowlist entry is a slot waiting for a real verb"
done
undeleted=""
doc_verb_n=0
for dt in "${doc_tokens[@]}"; do
  skip=0
  for nv in "${DOC_NON_VERBS[@]}"; do [[ "$dt" == "$nv" ]] && { skip=1; break; }; done
  (( skip == 1 )) && continue
  doc_verb_n=$(( doc_verb_n + 1 ))
  found=0
  for wv in "${watched_set[@]}"; do [[ "$dt" == "$wv" ]] && { found=1; break; }; done
  (( found == 0 )) && undeleted="$undeleted $dt"
done
if (( doc_verb_n < 5 )); then
  die_cannot_evaluate "N36 found only $doc_verb_n documented verbs after the allowlist; the containment would be vacuous"
fi
[[ -z "$undeleted" ]] &&
  assert "N36-documented-verbs-are-still-watched" "every verb AGENTS.md promises is guarded is still in is_watched_verb, so a verb cannot leave the shim while the documentation still promises it ($doc_verb_n documented verbs, from a file the shim is not generated from)" 0 ||
  assert "N36-documented-verbs-are-still-watched" "every verb AGENTS.md promises is guarded is still in is_watched_verb — DOCUMENTED BUT NO LONGER WATCHED:$undeleted" 1

# Rule (c) is armed in EVERY workspace mode, because the remote is shared in
# every workspace mode. It used to ride on refs_shared, which left force-push
# unguarded in clone-per-agent and with the mode unset — inside the guarded
# root, where the stated gap did not reach.
for wsmode in clone-per-agent unset; do
  MODE_OVERRIDE="$wsmode"
  arm "N30-push-force-mode-$wsmode" 77 "$SHARED" -- push --force origin main
  [[ "$("$REAL_GIT" -C "$BARE" rev-parse refs/heads/main 2>/dev/null)" == "$bare_before" ]] &&
    assert "N30-push-force-mode-$wsmode" "SCION_WORKSPACE_MODE=$wsmode: a private clone does not make the remote private" 0 ||
    assert "N30-push-force-mode-$wsmode" "SCION_WORKSPACE_MODE=$wsmode: a private clone does not make the remote private" 1
done

# ...and the tree rules must still stand down in a mode where the tree is NOT
# shared, or the arm above would just be proving the guard refuses everything.
MODE_OVERRIDE="clone-per-agent"
dirty "$SHARED"
arm "P14-checkout-permitted-in-private-mode" 0 "$SHARED" -- checkout -- tracked.txt
guard_silent "P14-checkout-permitted-in-private-mode"
[[ "$(cat "$SHARED/tracked.txt")" != *UNCOMMITTED-WORK* ]] &&
  assert "P14-checkout-permitted-in-private-mode" "rule (a) stands down in clone-per-agent: arming rule (c) everywhere did not arm the others" 0 ||
  assert "P14-checkout-permitted-in-private-mode" "rule (a) stands down in clone-per-agent: arming rule (c) everywhere did not arm the others" 1

# And the justification moved: branch -D is still refused, on the narrower
# ground, and must no longer claim the shared namespace.
arm "N17-branch-D-cites-local-ground" 77 "$SHARED" -- branch -D nonexistent-branch
[[ "$LAST_OUT" == *"a/local-refs"* && "$LAST_OUT" != *"every agent in this project shares"* ]] &&
  assert "N17-branch-D-cites-local-ground" "branch -D no longer claims a shared ref namespace it cannot reach" 0 ||
  assert "N17-branch-D-cites-local-ground" "branch -D no longer claims a shared ref namespace it cannot reach" 1

# --cached is the discriminating flag for rm, exactly as -n is for clean.
dirty "$SHARED"
arm "P10-rm-cached-is-permitted" 0 "$SHARED" -- rm --cached -q tracked.txt
guard_silent "P10-rm-cached-is-permitted"
[[ -e "$SHARED/tracked.txt" ]] && assert "P10-rm-cached-is-permitted" "rm --cached is permitted and leaves the file on disk" 0 ||
  assert "P10-rm-cached-is-permitted" "rm --cached is permitted and leaves the file on disk" 1
"$REAL_GIT" -C "$SHARED" reset -q >/dev/null 2>&1

# A `!`-alias needs no expansion here: any git it runs re-enters this shim on
# PATH and is judged on its own merits. Passing it through is the correct
# answer, not a gap.
arm "P13-alias-to-safe-command" 0 "$SHARED" -- safe --porcelain
guard_silent "P13-alias-to-safe-command"
[[ "$LAST_OUT" == *"tracked.txt"* ]] &&
  assert "P13-alias-to-safe-command" "an alias resolving to status is permitted: the loop classifies, it does not blanket-refuse" 0 ||
  assert "P13-alias-to-safe-command" "an alias resolving to status is permitted: the loop classifies, it does not blanket-refuse" 1

arm "P11-shell-alias-passes-through" 0 "$SHARED" -- sh
[[ "$LAST_OUT" == *"SHELL-ALIAS"* ]] &&
  assert "P11-shell-alias-passes-through" "a !-alias runs; its nested git is covered by re-entry, not by expansion" 0 ||
  assert "P11-shell-alias-passes-through" "a !-alias runs; its nested git is covered by re-entry, not by expansion" 1

arm "P1-status" 0 "$SHARED" -- status --porcelain
guard_silent "P1-status"
[[ "$LAST_OUT" == *"tracked.txt"* ]] &&
  assert "P1-status" "real git output came back through the shim unaltered" 0 ||
  assert "P1-status" "real git output came back through the shim unaltered" 1

arm "P2-clean-dry-run" 0 "$SHARED" -- clean -n
guard_silent "P2-clean-dry-run"
[[ "$LAST_OUT" == *"untracked.txt"* ]] &&
  assert "P2-clean-dry-run" "the permitted neighbour of the refused command still answers the question" 0 ||
  assert "P2-clean-dry-run" "the permitted neighbour of the refused command still answers the question" 1
[[ -e "$SHARED/untracked.txt" ]] &&
  assert "P2-clean-dry-run" "and it deleted nothing" 0 ||
  assert "P2-clean-dry-run" "and it deleted nothing" 1

arm "P3-fetch-to-named-ref" 0 "$SHARED" -- fetch "$DONOR" "main:refs/wsguard/wsguard-selftest/donor-main"
guard_silent "P3-fetch-to-named-ref"
fetched_sha="$("$REAL_GIT" -C "$SHARED" rev-parse refs/wsguard/wsguard-selftest/donor-main 2>&1)"
[[ "$fetched_sha" == "$DONOR_SHA" ]] &&
  assert "P3-fetch-to-named-ref" "the fetch landed in a ref this agent owns: $DONOR_SHA" 0 ||
  assert "P3-fetch-to-named-ref" "the fetch landed in a ref this agent owns (got '$fetched_sha')" 1

arm "P4-log-named-ref" 0 "$SHARED" -- log -1 --format=%H refs/wsguard/wsguard-selftest/donor-main
guard_silent "P4-log-named-ref"
[[ "$LAST_OUT" == "$DONOR_SHA" ]] &&
  assert "P4-log-named-ref" "reading the owned ref is permitted and returns the right commit" 0 ||
  assert "P4-log-named-ref" "reading the owned ref is permitted and returns the right commit" 1

arm "P5-stash-list" 0 "$SHARED" -- stash list
guard_silent "P5-stash-list"
arm "P6-branch-list" 0 "$SHARED" -- branch --list
guard_silent "P6-branch-list"

# The scoping arm. Same command as N1, in a repository that is not shared.
printf 'MY OWN WORK\n' >"$PRIVATE/tracked.txt"
arm "P7-checkout-in-private-clone" 0 "$PRIVATE" -- checkout -- tracked.txt
guard_silent "P7-checkout-in-private-clone"
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

# P15. THE AUDIT LOG IS THE ONLY ACCOUNT OF AN OVERRIDE, AND ITS CONTENT COMES
# FROM THE PERSON BEING AUDITED. $SCION_WSGUARD_OVERRIDE is free text and the
# argv is whatever was typed, so before the sanitiser a reason containing a
# newline appended a second, entirely fabricated ROW — correct column count,
# plausible timestamp position, attributable to any agent name the writer
# chose. A log that can be forged by the subject of the entry is worse than no
# log, because it is a log the next reader will believe. Flagged by
# gd-wsg-rev-3 alongside Q3.
#
# The post-condition is the LINE COUNT, not a substring. "The escape appears in
# the file" would also pass if the injected row were written as well; only
# counting rows distinguishes "escaped" from "escaped, and also injected".
dirty "$SHARED"
audit_lines_before="$(wc -l <"$AUDIT")"
OVERRIDE_REASON="$(printf 'selftest injection: line one\n1999-01-01T00:00:00Z\tsomeone-else\ta/local-refs\tforged\tgit push --force')"
arm "P15-audit-log-cannot-be-forged" 0 "$SHARED" -- checkout -- tracked.txt
audit_lines_after="$(wc -l <"$AUDIT")"
(( audit_lines_after - audit_lines_before == 1 )) &&
  assert "P15-audit-log-cannot-be-forged" "one override wrote exactly one row: a newline in the reason cannot forge a second" 0 ||
  assert "P15-audit-log-cannot-be-forged" "one override wrote exactly one row (got $(( audit_lines_after - audit_lines_before )))" 1
audit_last="$(tail -1 "$AUDIT")"
[[ "$audit_last" == *'line one\n1999-01-01'* && "$audit_last" == *'Z\tsomeone-else'* ]] &&
  assert "P15-audit-log-cannot-be-forged" "the injected newline and tab survive as visible escapes: sanitised, not silently dropped" 0 ||
  assert "P15-audit-log-cannot-be-forged" "the injected newline and tab survive as visible escapes (got '$audit_last')" 1
# The row still has its own five fields. An escape that also ate a real
# separator would pass the count check above while destroying the format.
audit_fields="$(printf '%s' "$audit_last" | tr -cd '\t' | wc -c)"
(( audit_fields == 4 )) &&
  assert "P15-audit-log-cannot-be-forged" "the row still has exactly 5 fields: sanitising the payload did not corrupt the format" 0 ||
  assert "P15-audit-log-cannot-be-forged" "the row still has exactly 5 fields (got $(( audit_fields + 1 )))" 1

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
guard_silent "P9-two-shims-then-real-git"
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
# Machine-readable, and read by --prove-it with a bash containment test. The
# two counters are printed SEPARATELY and asserted SEPARATELY because the exit
# code is one number fed by both, and one number cannot say which of two things
# went wrong.
printf 'counters: arms_mismatched=%d checks_failed=%d\n' "$arms_mismatched" "$checks_failed"
# Not printed as a ratio. Every control calls die_cannot_evaluate on failure, so
# the denominator can only ever equal the numerator and "6/6" would be a
# tautology dressed as a measurement. The load-bearing statement is the second
# line: a control that does not reproduce ends the run at 2.
printf 'harness controls      : %d reproduced with the real git\n' "$controls_run"
printf '                        (any that had not would have exited 2, not 0)\n'
echo "==========================================================================="

# ---------------------------------------------------------------------------
# THE SUITE'S OWN SIZE IS A MEASUREMENT, AND IT WAS ASSERTED AGAINST NOTHING.
#
# Every DERIVED array in this file has a floor that ends the run at 2 — optlist,
# shadow_verbs, watched_set, armed_set, doc_tokens. The suite itself did not.
# gd-wsg-rev-3 deleted the N27/N28 block and got:
#
#   PASS — 47 arms, 59 post-conditions      rc=0
#   ...and --prove-it still reported BOTH negative controls green.
#
# That is the exact gap: --prove-it proves the two counters CAN redden. It says
# nothing about whether the arms that feed them still exist. A suite that gets
# smaller is a suite that measures less, and it should be as loud as a suite
# that measures wrong — louder, because it looks like success.
#
# This is 2 rather than 1 on purpose. "Fewer arms ran than this file claims to
# contain" is not a verdict about the guard; it is the harness reporting that it
# did not do what it says. Same reasoning as every other die_cannot_evaluate.
#
# These are FLOORS, not equalities. Adding an arm must not require editing a
# constant in two places — that is a chore, and chores get done by lowering the
# number. Removing one must.
EXPECT_ARMS=50
EXPECT_CHECKS=79
EXPECT_CONTROLS=7
if (( arms_run < EXPECT_ARMS || checks_run < EXPECT_CHECKS || controls_run < EXPECT_CONTROLS )); then
  {
    echo "wsguard-selftest: THE SUITE GOT SMALLER — NOTHING WAS TESTED."
    printf '  arms      : %d, floor %d\n' "$arms_run" "$EXPECT_ARMS"
    printf '  checks    : %d, floor %d\n' "$checks_run" "$EXPECT_CHECKS"
    printf '  controls  : %d, floor %d\n' "$controls_run" "$EXPECT_CONTROLS"
    echo "  Arms were deleted or skipped. Raise the floors deliberately if the"
    echo "  removal was intended; do not lower them to make this pass."
  } >&2
  exit 2
fi

total_failed=$(( arms_mismatched + checks_failed ))
if (( total_failed == 0 )); then
  echo "wsguard-selftest: PASS — $arms_run arms (floor $EXPECT_ARMS), $checks_run post-conditions (floor $EXPECT_CHECKS); refusals and permissions both demonstrated"
  exit 0
fi

echo >&2
echo "wsguard-selftest: FAIL — $total_failed of $(( arms_run + checks_run )) expectations did not hold:" >&2
for f in "${failures[@]}"; do
  echo "  - $f" >&2
done
exit 1
