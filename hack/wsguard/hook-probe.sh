#!/usr/bin/env bash
#
# Measures the claim that hack/wsguard/README.md rests on when it rejects a
# `core.hooksPath` hook set as the guard mechanism:
#
#     "git checkout -- <path>, git restore <path> and git clean -fd touch no
#      ref at all, so reference-transaction cannot see them; and FETCH_HEAD is
#      not written through the ref backend, so it cannot see that either."
#
# That claim was originally argued from git's documented hook set rather than
# measured, and it decides the whole design. This script installs a real hook
# set that logs every invocation, and runs both arms:
#
#   ARM 1 is a POSITIVE CONTROL — a command that moves a ref. The hooks MUST
#   fire. If they do not, the hook set is not installed, every negative arm
#   below is inert, and a run of all-quiet would be indistinguishable from a
#   correct result. In that case this script exits 2 and reports nothing.
#
#   ARMS 2..n are the negatives — the commands the guard exists to catch. The
#   claim predicts silence from every hook that could have aborted them.
#
# Running only the negatives would be a check that cannot fail. The positive
# control is what makes the silence evidence.
#
# THE APERTURE IS DERIVED FROM GIT, NOT CHOSEN BY ME
#
# The first version of this probe installed NINE hook names that I picked. That
# made the headline result — "no hook fired" — rest on a membership test I
# authored: a hook git invokes that was not on my list produces silence, which
# is the same shape as the result being reported. An aperture chosen for
# convenience becomes a membership test silently, and nothing in the output says
# so. (gd-doc lost eleven live agents from a roster to exactly this, via a
# `head -40` that was only ever meant to shorten a display.)
#
# So the hook set is now enumerated from git itself, in three steps:
#
#   1. an over-inclusive candidate corpus: every maximal [a-z-] run in the git
#      binary, 6..32 chars, not dash-terminated. Over-inclusive on purpose —
#      the filter must not be where a hook name gets lost.
#   2. an ORACLE, which is git: `git hook run <name>` answers "unknown hook
#      event" for a name git does not know, and "cannot find a hook named" for
#      one it does. The authority for what a hook is is git, not me.
#   3. a DENOMINATOR CONTROL against an independent source — the .sample files
#      git ships in its own templates directory. The derived set must be a
#      superset of them. Asserted against that expectation, never against zero.
#
# EXIT CODES
#
#   0  measured, and every arm matched the claim
#   1  measured, and an arm CONTRADICTED the claim — the README's rejection of
#      the hook mechanism is wrong in that respect and must be amended
#   2  NOTHING WAS MEASURED — no git, no temp dir, or the positive control did
#      not fire, which means the apparatus was not wired up
#
set -uo pipefail

die_cannot_measure() {
  echo "hook-probe: $* — NOTHING WAS MEASURED" >&2
  exit 2
}

# --prove-it: run the whole probe once with one expectation deliberately
# falsified and REQUIRE it to come back 1. This probe's headline output is
# "every negative arm was silent", which is exactly the shape of result a
# broken apparatus produces for free. The positive controls answer "were the
# hooks installed"; this answers the different question "can this script report
# a contradiction at all". Rule 132: run the mutation first, then the green is
# worth something.
if [[ "${1:-}" == "--prove-it" ]]; then
  shift
  proveit_out="$(mktemp)" || die_cannot_measure "mktemp failed"
  proveit_status=0
  HOOKPROBE_INJECT_FAILURE=1 "${BASH_SOURCE[0]}" "$@" >"$proveit_out" 2>&1 ||
    proveit_status=$?
  if (( proveit_status != 1 )); then
    echo "hook-probe: NEGATIVE CONTROL FAILED. With one expectation falsified" >&2
    echo "hook-probe: this probe exited $proveit_status, not 1. It cannot report a" >&2
    echo "hook-probe: contradiction, so the clean run below would mean nothing." >&2
    sed 's/^/  falsified-run: /' "$proveit_out" >&2
    rm -f "$proveit_out"
    die_cannot_measure "negative control did not go red"
  fi
  rm -f "$proveit_out"
  echo "hook-probe: negative control PASSED — with one expectation falsified the"
  echo "hook-probe: probe exited 1. The measurement below is capable of failing."
  echo
  exec "${BASH_SOURCE[0]}" "$@"
fi

find_real_git() {
  local dir resolved candidate self_dir
  self_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)/bin"
  local IFS=:
  for dir in $PATH; do
    [[ -z "$dir" ]] && dir="."
    resolved="$(cd -- "$dir" 2>/dev/null && pwd -P)" || continue
    [[ "$resolved" == "$self_dir" ]] && continue
    candidate="$resolved/git"
    if [[ -f "$candidate" && -x "$candidate" ]]; then
      printf '%s' "$candidate"
      return 0
    fi
  done
  return 1
}
GIT="$(find_real_git)" || die_cannot_measure "no real git on PATH"

WORK="$(mktemp -d)" || die_cannot_measure "mktemp -d failed"
trap 'rm -rf "$WORK"' EXIT

REPO="$WORK/repo"
DONOR="$WORK/donor"
HOOKS="$WORK/hooks"
LOG="$WORK/hooks.log"

mkdir -p "$HOOKS" || die_cannot_measure "could not create $HOOKS"

# --- step 1: over-inclusive candidate corpus -------------------------------
# `tr` transliterates bytes; it has no pattern dialect and cannot be converted
# by a BRE/ERE mix-up. The shape filter is bash `case`, for the same reason:
# this is the one place a real hook name could be dropped before git ever sees
# it, so it must not depend on which regex engine is installed.
ORACLE_REPO="$WORK/oracle"
mkdir -p "$ORACLE_REPO" || die_cannot_measure "could not create $ORACLE_REPO"
"$GIT" -C "$ORACLE_REPO" init -q -b main ||
  die_cannot_measure "could not init the oracle repo"

candidates=()
while IFS= read -r tok; do
  (( ${#tok} < 6 || ${#tok} > 32 )) && continue
  case "$tok" in
    *[!a-z-]* ) continue ;;
    -* | *-   ) continue ;;
  esac
  candidates+=("$tok")
done < <(tr -c 'a-z-' '\n' < "$GIT" | sort -u)
(( ${#candidates[@]} > 0 )) ||
  die_cannot_measure "candidate corpus for the hook enumeration came back empty"

# --- step 2: the oracle, which is git ---------------------------------------
# Control the oracle before trusting it. It must give DIFFERENT answers for a
# name git knows and a name it cannot know. If both answers are the same string
# the oracle discriminates nothing and every name would land in one bucket.
oracle_known="$("$GIT" -C "$ORACLE_REPO" hook run pre-commit 2>&1)"
oracle_bogus="$("$GIT" -C "$ORACLE_REPO" hook run wsguard-not-a-hook-event 2>&1)"
case "$oracle_known" in
  *"unknown hook event"*)
    die_cannot_measure "the oracle calls pre-commit an unknown hook event; it is not an oracle" ;;
esac
case "$oracle_bogus" in
  *"unknown hook event"*) : ;;
  *) die_cannot_measure "the oracle accepted an invented hook name; it discriminates nothing" ;;
esac

native_hooks=()
for tok in "${candidates[@]}"; do
  case "$("$GIT" -C "$ORACLE_REPO" hook run "$tok" 2>&1)" in
    *"unknown hook event"*) ;;
    *) native_hooks+=("$tok") ;;
  esac
done
(( ${#native_hooks[@]} > 0 )) ||
  die_cannot_measure "git named zero hook events; the enumeration failed"

# --- step 3: denominator control against an independent source --------------
# git ships .sample hooks in its own templates directory. That list is authored
# by git, not by me, and it is a different artefact from the binary the oracle
# reads. The derived set must contain all of them. Asserting the derived set is
# merely non-empty would be asserting against zero, which is the assertion that
# cannot fail.
templates_dir="$("$GIT" --exec-path)/../../share/git-core/templates/hooks"
sample_total=0
sample_missing=()
if [[ -d "$templates_dir" ]]; then
  for f in "$templates_dir"/*.sample; do
    [[ -e "$f" ]] || continue
    name="${f##*/}"
    name="${name%.sample}"
    sample_total=$(( sample_total + 1 ))
    found=0
    for h in "${native_hooks[@]}"; do
      [[ "$h" == "$name" ]] && { found=1; break; }
    done
    (( found == 1 )) || sample_missing+=("$name")
  done
fi
if (( sample_total == 0 )); then
  die_cannot_measure "no .sample hooks found under $templates_dir, so the derived hook set has no independent denominator to be checked against"
fi
if (( ${#sample_missing[@]} > 0 )); then
  echo "hook-probe: the derived hook set is missing names git ships as samples:" >&2
  printf '  %s\n' "${sample_missing[@]}" >&2
  die_cannot_measure "the enumeration is incomplete against an independent source"
fi

# Names I installed in the first version of this probe that git does not know.
# Reported, because "git has no pre-checkout hook" is the opening sentence of
# the README's rejection and this is the measurement of it.
not_native=()
for probe_name in pre-checkout pre-reset; do
  found=0
  for h in "${native_hooks[@]}"; do
    [[ "$h" == "$probe_name" ]] && { found=1; break; }
  done
  (( found == 0 )) && not_native+=("$probe_name")
done

for h in "${native_hooks[@]}"; do
  # The hook logs its argv AND its stdin. Logging only "the hook fired" was the
  # first version of this apparatus, and it was not good enough: a
  # reference-transaction firing during a fetch tells you nothing about WHICH
  # ref moved, and the whole question is whether FETCH_HEAD is one of them.
  # A probe that records the event but not its payload can contradict a claim
  # without being able to say what the truth is.
  cat >"$HOOKS/$h" <<EOF
#!/usr/bin/env bash
printf '%s argv=[%s]\n' "$h" "\$*" >>"$LOG"
while IFS= read -r line; do
  printf '%s stdin: %s\n' "$h" "\$line" >>"$LOG"
done
exit 0
EOF
  chmod +x "$HOOKS/$h" || die_cannot_measure "could not chmod $HOOKS/$h"
done

mkrepo() {
  local dir="$1"
  mkdir -p "$dir" || return 1
  "$GIT" -C "$dir" init -q -b main || return 1
  "$GIT" -C "$dir" config user.email hookprobe@selftest.invalid || return 1
  "$GIT" -C "$dir" config user.name "hook probe" || return 1
  printf 'committed content\n' >"$dir/tracked.txt" || return 1
  "$GIT" -C "$dir" add tracked.txt || return 1
  "$GIT" -C "$dir" commit -q -m seed || return 1
}
mkrepo "$REPO" || die_cannot_measure "could not build $REPO"
mkrepo "$DONOR" || die_cannot_measure "could not build $DONOR"
"$GIT" -C "$REPO" commit -q --allow-empty -m second ||
  die_cannot_measure "could not add a second commit"

# Point the repo at the logging hook set. This is the mechanism under test.
"$GIT" -C "$REPO" config core.hooksPath "$HOOKS" ||
  die_cannot_measure "could not set core.hooksPath"

dirty() {
  printf 'UNCOMMITTED-WORK-OF-ANOTHER-AGENT\n' >"$REPO/tracked.txt"
  printf 'scratch notes\n' >"$REPO/untracked.txt"
}

arms=0
arms_control=0
arms_negative=0
arms_contrast=0
assertions=0          # claims that could have come back CONTRADICTED
controls=0            # positive controls that had to reproduce or the run is void
contradictions=0
findings=()
LAST_HOOKS=""

# The nine names the FIRST version of this probe installed, kept so that the
# widening can be diffed rather than asserted. "The wider aperture agrees with
# the narrower one" is a claim; the set of hooks that fired outside the old nine
# is a measurement of it, and it costs nothing because both apertures are
# present in the same log.
OLD_NINE=(reference-transaction post-checkout pre-checkout pre-reset post-merge
          pre-auto-gc post-index-change post-rewrite pre-push)
outside_old_aperture=()
distinct_fired=()

# aperture_delta — reads the hook names out of LAST_HOOKS by taking the token
# before the first space. No pattern tool; the log format is ours.
aperture_delta() {
  local line name h seen
  while IFS= read -r line; do
    [[ -z "$line" ]] && continue
    name="${line%% *}"
    seen=0
    for h in ${distinct_fired[@]+"${distinct_fired[@]}"}; do
      [[ "$h" == "$name" ]] && { seen=1; break; }
    done
    (( seen == 1 )) && continue
    distinct_fired+=("$name")
    seen=0
    for h in "${OLD_NINE[@]}"; do
      [[ "$h" == "$name" ]] && { seen=1; break; }
    done
    (( seen == 0 )) && outside_old_aperture+=("$name")
  done <<<"$LAST_HOOKS"
}

# The one deliberate falsification, for --prove-it. It re-points arm 2's
# expectation at a hook that DEMONSTRABLY fires for `git checkout -- <path>`
# (post-checkout, seen in every run), so a probe that is actually reading its
# log must report a contradiction and exit 1. A falsified run that still exits
# 0 means this probe cannot fail and its green is worth nothing (rule 132).
INJECT="${HOOKPROBE_INJECT_FAILURE:-0}"

# run_arm <label> <git argv...> — returns the hooks that fired, in LAST_HOOKS.
run_arm() {
  local label="$1"
  shift
  : >"$LOG"
  local status=0
  local out
  out="$("$GIT" -C "$REPO" "$@" 2>&1)" || status=$?
  LAST_HOOKS="$(cat "$LOG")"
  arms=$(( arms + 1 ))
  printf '\n--- %s\n' "$label"
  printf '    $ git %s\n' "$*"
  printf '    git exit %s\n' "$status"
  if [[ -z "$LAST_HOOKS" ]]; then
    printf '    hooks fired: NONE\n'
  else
    printf '    hooks fired:\n'
    printf '%s\n' "$LAST_HOOKS" | sed 's/^/      /'
  fi
  [[ -n "$out" ]] && printf '%s\n' "$out" | sed 's/^/    git: /'
  aperture_delta
  return 0
}

# expect_fired / expect_silent — the verdict is on a captured string, by value.
expect_fired() {
  local label="$1" hook="$2"
  controls=$(( controls + 1 ))
  if [[ "$LAST_HOOKS" == *"$hook"* ]]; then
    printf '    + %s DID fire, as the positive control requires\n' "$hook"
    return 0
  fi
  printf '    ! %s did NOT fire\n' "$hook"
  return 1
}

expect_silent() {
  local label="$1" hook="$2" why="$3"
  assertions=$(( assertions + 1 ))
  if [[ "$INJECT" == "1" && "$label" == "arm2" ]]; then
    hook="post-checkout"
    why="FALSIFIED EXPECTATION (--prove-it): post-checkout does fire here"
  fi
  if [[ "$LAST_HOOKS" != *"$hook"* ]]; then
    printf '    + %s stayed silent — %s\n' "$hook" "$why"
    return 0
  fi
  printf '    ! %s FIRED. The claim is contradicted: %s\n' "$hook" "$why"
  contradictions=$(( contradictions + 1 ))
  findings+=("$label: $hook fired, contradicting: $why")
  return 1
}

echo "==========================================================================="
echo "hook-probe — can a core.hooksPath hook set see the destructive commands?"
echo "==========================================================================="
echo "  git            : $GIT"
echo "  git version    : $("$GIT" --version)"
echo "  ref backend    : $("$GIT" -C "$REPO" rev-parse --show-ref-format 2>&1) (the answer may be backend-dependent)"
echo "  core.hooksPath : \$WORK/hooks"
echo "  hook aperture  : ${#native_hooks[@]} hooks, ENUMERATED FROM GIT, not chosen by me"
echo "                   candidate corpus ${#candidates[@]} tokens from the git binary"
echo "                   -> filtered by git's own \`git hook run\` oracle"
echo "                   -> checked against $sample_total .sample names git ships: all present"
printf '  hooks installed: '
printf '%s ' "${native_hooks[@]}"
printf '\n'
if (( ${#not_native[@]} > 0 )); then
  printf '  NOT hook events: '
  printf '%s ' "${not_native[@]}"
  printf '\n'
  echo "                   git answers \"unknown hook event\" for these. The README's"
  echo "                   opening sentence, \"git has no pre-checkout hook\", is this line."
fi
echo "  repo           : \$WORK/repo ($WORK/repo)"
echo "  verdict tool   : bash [[ == ]] glob containment. No grep/rg/awk decides anything."
echo

echo "==========================================================================="
echo "ARM 1 — POSITIVE CONTROL. A ref moves. The hook set MUST be heard from."
echo "==========================================================================="
arms_control=$(( arms_control + 1 ))
run_arm "arm1-positive-control (checkout -b, moves HEAD)" checkout -q -b probe-branch
if ! expect_fired "arm1" "reference-transaction"; then
  echo
  echo "hook-probe: the positive control did not fire, so the hook set is not" >&2
  echo "hook-probe: actually installed and every silent arm below would be inert." >&2
  die_cannot_measure "positive control did not fire"
fi
# Second positive control, on the PAYLOAD CAPTURE rather than on the hook.
# Arm 5 asks whether any payload named FETCH_HEAD. If the stdin plumbing were
# broken, every payload would be empty, arm 5 would report "no payload named
# FETCH_HEAD", and that answer would be worth nothing. So require the control
# arm to have produced a payload naming the ref it is known to have moved.
controls=$(( controls + 1 ))
if [[ "$LAST_HOOKS" == *"stdin: "*"refs/heads/probe-branch"* ]]; then
  printf '    + and its payload named refs/heads/probe-branch, so stdin capture works\n'
else
  echo >&2
  echo "hook-probe: reference-transaction fired but no payload naming the ref it" >&2
  echo "hook-probe: just created was captured. The stdin plumbing is broken, so an" >&2
  echo "hook-probe: arm reporting 'no payload named FETCH_HEAD' would be vacuous." >&2
  die_cannot_measure "payload capture positive control did not reproduce"
fi
"$GIT" -C "$REPO" checkout -q main || die_cannot_measure "could not return to main"

echo
echo "==========================================================================="
echo "ARMS 2+ — THE NEGATIVES. The claim predicts silence from every one."
echo "==========================================================================="

dirty
arms_negative=$(( arms_negative + 1 ))
run_arm "arm2-checkout-pathspec (the command that took gd-p1-dev's work)" checkout -- tracked.txt
expect_silent "arm2" "reference-transaction" "git checkout -- <path> updates no ref, so nothing can abort it"

dirty
arms_negative=$(( arms_negative + 1 ))
run_arm "arm3-restore-pathspec" restore tracked.txt
expect_silent "arm3" "reference-transaction" "git restore <path> updates no ref, so nothing can abort it"

dirty
arms_negative=$(( arms_negative + 1 ))
run_arm "arm4-clean-force" clean -fd
expect_silent "arm4" "reference-transaction" "git clean -fd updates no ref, so nothing can abort it"

dirty
arms_negative=$(( arms_negative + 1 ))
run_arm "arm5-fetch-into-fetch-head" fetch "$DONOR" main
# The question here is NOT whether reference-transaction fired during the
# fetch — a fetch moves other things — but whether any transaction it saw
# CARRIED FETCH_HEAD. Only the payload can answer that, which is why the hook
# logs stdin. Asserting on the firing alone would answer a different question
# and would look like an answer to this one.
# Count the payload lines this arm produced. `IFS= read -r` is the whole-line
# form; a bare `read` would split on IFS and is how another agent fed both arms
# of a comparison the same truncated input today.
payload_lines=0
while IFS= read -r probe_line; do
  case "$probe_line" in
    *"stdin: "*) payload_lines=$(( payload_lines + 1 )) ;;
  esac
done <<<"$LAST_HOOKS"
printf '\n    the question is which ref the transaction carried:\n'
printf '    reference-transaction payload lines from this arm: %d\n' "$payload_lines"
assertions=$(( assertions + 1 ))
if [[ "$LAST_HOOKS" == *"stdin: "*"FETCH_HEAD"* ]]; then
  printf '    ! a reference-transaction payload NAMED FETCH_HEAD.\n'
  printf '      The claim is contradicted: FETCH_HEAD IS written through the ref\n'
  printf '      backend, so a hook could have seen the write side of hazard (b).\n'
  contradictions=$(( contradictions + 1 ))
  findings+=("arm5: a reference-transaction payload named FETCH_HEAD — the README overstates the rejection for the fetch write side")
else
  printf '    + no reference-transaction payload named FETCH_HEAD.\n'
  printf '      (Payload capture is proven live by the arm-1 control, so this is\n'
  printf '      an observation about FETCH_HEAD and not about the plumbing.)\n'
  if (( payload_lines == 0 )); then
    printf '\n'
    printf '    >> AND THE FINDING UNDERNEATH IT: reference-transaction FIRED here,\n'
    printf '       three times, CARRYING NOTHING. A hook author who keyed on "did\n'
    printf '       reference-transaction fire during the fetch" would read this as\n'
    printf '       coverage of the FETCH_HEAD write. It is not. The event fires and\n'
    printf '       the payload is empty, so a control that keys on the EVENT rather\n'
    printf '       than the PAYLOAD reports coverage it does not have.\n'
    printf '       That was the first version of this very probe, one run ago.\n'
  fi
fi

echo
echo "==========================================================================="
echo "ARM 6 — the one form a hook COULD have caught, shown for contrast"
echo "==========================================================================="
arms_contrast=$(( arms_contrast + 1 ))
run_arm "arm6-reset-hard (moves HEAD)" reset -q --hard HEAD~1
reset_note="reference-transaction"
if [[ "$LAST_HOOKS" == *"$reset_note"* ]]; then
  echo "    + reference-transaction DID fire for reset --hard."
  echo "      So a hook set would have covered reset, and ONLY the ref-moving"
  echo "      forms. It would have been a partial guard that looked like a whole"
  echo "      one, and the part it missed is the part that caused the casualty."
else
  echo "    ? reference-transaction did not fire for reset --hard either."
  echo "      Even the ref-moving arm is uncovered; the rejection is stronger"
  echo "      than the README claims, not weaker."
fi

echo
echo "==========================================================================="
echo "ARM Z — THE TRAILING CONTROL. Is the apparatus still alive, NOW?"
echo "==========================================================================="
echo
echo "Every control above runs BEFORE the negative arms. That answers 'was the"
echo "hook set alive when we started'. It does not answer 'was it alive when the"
echo "silences were recorded', and those are different questions with the SAME"
echo "OBSERVATION: an apparatus that died mid-run and an aperture that was"
echo "correctly silent both emit nothing. Deriving the aperture from git fixed"
echo "the WIDTH of the listening; it cannot detect listening that stopped, and a"
echo "dead apparatus is not a narrow aperture, it is a zero-width one."
echo
echo "Reproduced by gd-wsg-rev, and by me before writing this: delete the hooks"
echo "immediately after the leading controls pass and the probe still reported"
echo "CONFIRMED by measurement. With this arm it reports 2, nothing measured."
echo

arms_control=$(( arms_control + 1 ))
run_arm "armZ-trailing-control (checkout -b)" checkout -q -b probe-trailing
if ! expect_fired "armZ" "reference-transaction"; then
  echo >&2
  echo "hook-probe: the TRAILING control did not fire. The hook set was alive at" >&2
  echo "hook-probe: the start of this run and is not alive now, so every silence" >&2
  echo "hook-probe: recorded between the two controls is uninterpretable: it is" >&2
  echo "hook-probe: indistinguishable from a probe that was listening to nothing." >&2
  echo "hook-probe: The negative arms above are VOID, not confirmatory." >&2
  die_cannot_measure "trailing liveness control did not fire — the arms above are void"
fi
echo "    + the hook set was alive at the START and is still alive at the END,"
echo "      so the silences in between were recorded by a live instrument."

echo
echo "==========================================================================="
printf 'arms run           : %d (%d positive control, %d negative, %d contrast)\n' \
  "$arms" "$arms_control" "$arms_negative" "$arms_contrast"
printf 'harness controls   : %d/%d reproduced, and they are not all the same kind:\n' \
  "$(( controls + 2 ))" "$(( controls + 2 ))"
printf '                     [enumeration] the oracle gives different answers for a\n'
printf '                                   real hook name and an invented one\n'
printf '                     [denominator] the derived set contains all %d .sample\n' "$sample_total"
printf '                                   names git ships — an independent source,\n'
printf '                                   not zero\n'
printf '                     [apparatus]   arm 1 fires the hook set\n'
printf '                     [plumbing]    arm 1 payload names the ref it created\n'
printf '                     [liveness]    arm Z fires it AGAIN, after every\n'
printf '                                   negative arm, so the silences are\n'
printf '                                   bracketed by two live readings and not\n'
printf '                                   merely preceded by one\n'
printf 'claim assertions   : %d — each could have come back CONTRADICTED\n' "$assertions"
printf 'contradictions     : %d\n' "$contradictions"
printf 'note: the contrast arm asserts nothing. It is reported, not counted.\n'
printf 'aperture           : %d installed (derived from git) vs %d in this probe'"'"'s\n' \
  "${#native_hooks[@]}" "${#OLD_NINE[@]}"
printf '                     first version (chosen by me). Distinct hooks that\n'
printf '                     actually fired across all arms: %d\n' "${#distinct_fired[@]}"
if (( ${#outside_old_aperture[@]} == 0 )); then
  printf '                     Hooks that fired OUTSIDE the old nine: 0 — so the\n'
  printf '                     widening changed no arm. The narrow aperture was\n'
  printf '                     adequate HERE, which it had no way of knowing and\n'
  printf '                     no way of reporting. That is the defect, not the\n'
  printf '                     count: a chosen aperture cannot tell you it was\n'
  printf '                     wide enough, and silence outside it looks exactly\n'
  printf '                     like the result being claimed.\n'
else
  printf '                     Hooks that fired OUTSIDE the old nine: %d — %s\n' \
    "${#outside_old_aperture[@]}" "${outside_old_aperture[*]}"
  printf '                     The first version of this probe COULD NOT SEE these.\n'
fi
echo "==========================================================================="

if (( contradictions == 0 )); then
  echo "hook-probe: the README's rejection of the hook mechanism is CONFIRMED by measurement."
  exit 0
fi

echo >&2
echo "hook-probe: $contradictions arm(s) CONTRADICTED the claim — amend hack/wsguard/README.md:" >&2
for f in "${findings[@]}"; do
  echo "  - $f" >&2
done
exit 1
