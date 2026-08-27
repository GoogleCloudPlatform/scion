#!/usr/bin/env bash
#
# TRIGGER REGISTRY, ENTRY C - the Phase 2 IAM verification moving off UNRUN.
#
# ==============================================================================
# 🔴 READ THIS BEFORE ASSUMING CI IS WATCHING THIS FOR YOU. IT IS NOT.
# ==============================================================================
#
# This is the ONE registry entry that cannot be automated in chart CI, and the
# reason is structural rather than a gap somebody will close later:
#
#   ENTRY C'S PREDICATE IS NOT A PROPERTY OF ANY TREE. It is the state of a
#   Google Cloud project - specifically, whether the access that returned 403
#   on 2026-08-17 has been granted. No amount of reading this repository can
#   observe it. A checkout is not a credential.
#
# So the entry is split, and the split is the whole design:
#
#   predicate   external, NOT runnable in chart CI. Needs live gcloud
#               credentials. Run by a periodic probe or by a human.
#   obligation  internal, runnable anywhere, and wired into tests/run-all.sh.
#               It reads VALIDATION.md and nothing else.
#
# WHAT THE ENTRY SAYS, in the registry's own terms:
#
#   Predicate    the GCP grant lands - the 403s in VALIDATION.md section 7.2
#                stop being returned for the project's Cloud SQL and GKE APIs.
#   Obligation   the IAM verification is performed, and VALIDATION.md section
#                7.2 is updated from UNRUN to a RESULT. EITHER OUTCOME
#                DISCHARGES IT. "We tried and IAM DB auth is unavailable on
#                this project" is a result and closes the obligation; only
#                silence leaves it open.
#   Owner        whoever holds the chart branch when the grant arrives. Not a
#                named agent: the grant may outlive any of us, and an owner
#                who has ceased to exist is an obligation with no owner.
#
# THREE STATES, NOT TWO, IN BOTH EVALUATORS. A predicate that can only say
# true or false cannot say "I could not look", and "I could not look" reported
# as false is the failure this registry exists to prevent - a check that was
# never able to run, reporting the reassuring answer.
#
#   exit 0   TRUE / DISCHARGED       the thing is so, and was observed
#   exit 1   FALSE, AND THE CORPUS WAS READ
#   exit 2   CANNOT EVALUATE         no verdict. NOT a pass. NOT a failure.
#
# 🔴 THE ANTI-VACUITY CLAUSE, AND IT IS LOAD-BEARING. If VALIDATION.md is
# ABSENT, this script EXITS NON-ZERO (2) rather than passing. An obligation
# assertion whose subject has been deleted must not report success: deleting
# the file is the single cheapest way to make "the file no longer says UNRUN"
# true, and a naive check reads that as the obligation having been discharged.
# The clause is tested by the self-test below against an absent path, and that
# arm is not optional.
#
# A TRIGGER PREDICATE CANNOT HAVE A POSITIVE CONTROL IN PRODUCTION - the only
# world that exercises its true branch is the world it exists to catch, and by
# the time that world arrives it is too late to discover the branch was broken.
# So the control is synthetic: `selftest` drives the obligation evaluator over
# a committed true/false fixture pair plus an absent path, and requires three
# DIFFERENT exit codes. Two fixtures that agree prove nothing; an instrument
# returning the same answer to both arms is an instrument that is not reading.
#
# ATTACKS RUN AGAINST THIS SCRIPT on 2026-08-17, before it was trusted. A
# mutation is not evidence until something independent goes red on it, so each
# row names what went red and how many arms:
#
#   delete the live VALIDATION.md          -> FAILED 1 of 5. The anti-vacuity
#                                             clause on the live subject. Exit
#                                             2, not 0, which is the whole
#                                             clause.
#   blind obligation() to `return 0`       -> FAILED 4 of 5.
#   strike "UNRUN" from 7.2 by hand,
#     running nothing                      -> FAILED 1 of 5. Discharging the
#                                             obligation by editing the word is
#                                             visible as a discharge, which is
#                                             correct: it then demands a human
#                                             confirm a result replaced it.
#
# 🔴 THE ONE ARM A BLINDED EVALUATOR STILL PASSES IS THE TRUE ARM. Attack 2
# left "the DISCHARGED fixture reports DISCHARGED" green, because an evaluator
# that always says 0 agrees with the fixture whose answer is 0. THAT IS WHY THE
# PAIR IS REQUIRED AND WHY NEITHER HALF IS SUFFICIENT ALONE - a single-fixture
# control chosen to match the expected answer is indistinguishable from a dead
# instrument. Do not "simplify" this by dropping either fixture.
#
# PREDICATE, run 2026-08-17T10:24Z and again while writing this, both times
# exit 1 (FALSE-and-ran): principal
# scion-my-grove@deploy-demo-test.iam.gserviceaccount.com, 403 from
# `gcloud container clusters list`, "not authorized" from
# `gcloud sql instances list`. The full text is in VALIDATION.md section 7.2.
#
# Usage:
#   trigger-entry-c.sh predicate    [needs gcloud credentials; not for CI]
#   trigger-entry-c.sh obligation   [reads VALIDATION.md]
#   trigger-entry-c.sh selftest     [synthetic control; this is what CI runs]

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(dirname "$HERE")"

# The project the 403s were measured against. Named here rather than passed in
# so that a re-run cannot silently probe a DIFFERENT project and report that
# the grant landed.
ENTRY_C_PROJECT="${ENTRY_C_PROJECT:-deploy-demo-test}"

# ------------------------------------------------------------------ predicate

predicate() {
  if ! command -v gcloud >/dev/null 2>&1; then
    echo "CANNOT-EVALUATE  gcloud is not on PATH. This is not evidence that the grant"
    echo "                 has or has not landed - the probe could not be issued."
    return 2
  fi

  local acct
  acct="$(gcloud config get-value account 2>/dev/null)"
  if [[ -z "$acct" || "$acct" == "(unset)" ]]; then
    echo "CANNOT-EVALUATE  gcloud has no active account, so a 403 here would mean"
    echo "                 'not authenticated' rather than 'not authorised'. Those"
    echo "                 are different findings and only one of them is Entry C's."
    return 2
  fi

  # BOTH APIS, because the obligation needs both. Cloud SQL alone is not enough
  # to run the verification - it also has to be installed on a cluster - and a
  # partial grant reported as the full one sends somebody to do work they still
  # cannot do.
  local sql_out gke_out sql_rc gke_rc
  sql_out="$(gcloud sql instances list --project="$ENTRY_C_PROJECT" 2>&1)"; sql_rc=$?
  gke_out="$(gcloud container clusters list --project="$ENTRY_C_PROJECT" 2>&1)"; gke_rc=$?

  echo "principal: $acct"
  echo "project:   $ENTRY_C_PROJECT"
  echo "--- gcloud sql instances list (rc=$sql_rc) ---"
  echo "$sql_out"
  echo "--- gcloud container clusters list (rc=$gke_rc) ---"
  echo "$gke_out"

  if [[ $sql_rc -eq 0 && $gke_rc -eq 0 ]]; then
    echo
    echo "TRUE  both APIs answered. The grant appears to have landed."
    echo "      ENTRY C IS NOW DUE: perform the IAM verification and update"
    echo "      VALIDATION.md section 7.2 from UNRUN to a result."
    return 0
  fi

  # A 403 IS A SUCCESSFUL MEASUREMENT. The probe ran, the project answered, and
  # the answer was no. That is FALSE-and-the-corpus-was-read, and it is a
  # different state from having been unable to ask.
  if echo "$sql_out$gke_out" | grep -qE '403|does not have permission|not authorized'; then
    echo
    echo "FALSE (and the probe ran)  the 403s recorded in VALIDATION.md section 7.2"
    echo "      are still being returned. Entry C stays registered and not yet due."
    return 1
  fi

  echo
  echo "CANNOT-EVALUATE  the probe failed for a reason that is neither success nor"
  echo "                 a 403 - a network error, a missing project, an API not"
  echo "                 enabled. Read the output above. Reporting this as FALSE"
  echo "                 would claim the grant has not landed on the strength of"
  echo "                 a question that was never delivered."
  return 2
}

# ----------------------------------------------------------------- obligation

# obligation [path-to-VALIDATION.md]
obligation() {
  local f="${1:-$CHART_DIR/VALIDATION.md}"

  # 🔴 THE ANTI-VACUITY CLAUSE.
  if [[ ! -f "$f" ]]; then
    echo "CANNOT-EVALUATE  $f does not exist."
    echo "                 THIS IS EXIT 2 AND NOT EXIT 0, DELIBERATELY. An absent"
    echo "                 subject does not discharge an obligation about it -"
    echo "                 deleting the file is the cheapest possible way to stop"
    echo "                 it saying UNRUN, and a check that rewarded that would"
    echo "                 be worse than no check."
    return 2
  fi
  if [[ ! -s "$f" ]]; then
    echo "CANNOT-EVALUATE  $f is empty. Same reasoning as an absent file."
    return 2
  fi

  # THE SECTION, EXTRACTED BY HEADING. If the heading is gone the corpus is not
  # what this evaluator thinks it is, and every verdict below would be about a
  # document it did not find.
  local section
  section="$(awk '/^#### 7\.2 /{f=1} f{ if (/^#### 7\.3 /) exit; print }' "$f")"
  if [[ -z "$section" ]]; then
    echo "CANNOT-EVALUATE  $f exists but has no '#### 7.2' section, or the section"
    echo "                 boundary changed. The obligation is about that section's"
    echo "                 contents; with the section unlocatable there is nothing"
    echo "                 to read. Renumbered? Update the awk above IN THE SAME"
    echo "                 COMMIT as the renumbering."
    return 2
  fi

  # THE DENOMINATOR. An extraction that returned a one-line fragment would make
  # every absence test below pass on almost nothing.
  local lines
  lines="$(printf '%s\n' "$section" | wc -l)"
  if [[ "$lines" -lt 10 ]]; then
    echo "CANNOT-EVALUATE  section 7.2 extracted as $lines line(s). That is too"
    echo "                 short to be the section; the extraction is reading the"
    echo "                 wrong thing and would report DISCHARGED on a fragment."
    return 2
  fi

  if printf '%s\n' "$section" | grep -qF 'UNRUN'; then
    echo "OPEN  VALIDATION.md section 7.2 still says UNRUN ($lines lines read)."
    echo "      Entry C's obligation is not discharged."
    return 1
  fi

  echo "DISCHARGED  section 7.2 no longer says UNRUN ($lines lines read)."
  echo "            Verify by hand that it was replaced with a RESULT and not"
  echo "            merely reworded - this evaluator can see the word leaving,"
  echo "            not what replaced it, and that limit is why a human signs"
  echo "            this one off."
  return 0
}

# ------------------------------------------------------------------- selftest

selftest() {
  local fixtures="$HERE/fixtures"
  local failures=0 executed=0

  check() {
    local label="$1" want="$2" path="$3"
    local out rc
    out="$(obligation "$path" 2>&1)"; rc=$?
    executed=$((executed + 1))
    if [[ "$rc" -eq "$want" ]]; then
      echo "ok    $label -> exit $rc"
    else
      echo "FAIL  $label -> exit $rc, expected $want"
      printf '%s\n' "$out" | sed 's/^/        /'
      failures=$((failures + 1))
    fi
  }

  # THE APPARATUS FIRST. A missing fixture would make the arm below "pass" by
  # taking the absent-file branch and returning 2 for the wrong reason - the
  # OPEN arm would go red, but the CANNOT-EVALUATE arm would go green while
  # measuring nothing.
  local fx
  for fx in entry-c-open.md entry-c-discharged.md; do
    if [[ ! -s "$fixtures/$fx" ]]; then
      echo "HARNESS ERROR: $fixtures/$fx is missing or empty. The synthetic control"
      echo "               cannot run and NOTHING WAS MEASURED."
      echo "ASSERTIONS_EXECUTED=${executed}"
      exit 2
    fi
  done

  # THREE ARMS, THREE DIFFERENT EXIT CODES REQUIRED. This is the part that
  # cannot be satisfied by an evaluator that always returns the same thing.
  check "the OPEN fixture (says UNRUN) reports OPEN"            1 "$fixtures/entry-c-open.md"
  check "the DISCHARGED fixture (carries a result) reports DISCHARGED" 0 "$fixtures/entry-c-discharged.md"
  check "ANTI-VACUITY: an absent VALIDATION.md exits 2, not 0"  2 "$fixtures/this-file-does-not-exist.md"
  check "a file with no 7.2 section exits 2, not 0"             2 "$fixtures/entry-c-no-section.md"

  # THE LIVE SUBJECT. Today this must be OPEN - the verification is unrun. When
  # somebody discharges it this arm goes red, and that redness is the signal to
  # retire Entry C from the registry, not a defect to be silenced.
  local live_out live_rc
  live_out="$(obligation "$CHART_DIR/VALIDATION.md" 2>&1)"; live_rc=$?
  executed=$((executed + 1))
  if [[ "$live_rc" -eq 1 ]]; then
    echo "ok    the live VALIDATION.md reports OPEN, which is correct today"
  elif [[ "$live_rc" -eq 0 ]]; then
    echo "FAIL  the live VALIDATION.md reports DISCHARGED. If the IAM verification"
    echo "      was genuinely run, THIS IS THE INTENDED FAILURE: retire Entry C"
    echo "      from the trigger registry and delete this arm, in one commit."
    failures=$((failures + 1))
  else
    echo "FAIL  the live VALIDATION.md could not be evaluated (exit $live_rc):"
    printf '%s\n' "$live_out" | sed 's/^/        /'
    failures=$((failures + 1))
  fi

  echo
  echo "ASSERTIONS_EXECUTED=${executed}"
  if [[ "$failures" -eq 0 ]]; then
    echo "PASS ${executed}/${executed}"
    return 0
  fi
  echo "FAILED ${failures} of ${executed}"
  return 1
}

case "${1:-}" in
  predicate)  predicate ;;
  obligation) obligation "${2:-}" ;;
  selftest)   selftest ;;
  *)
    echo "usage: $0 {predicate|obligation [file]|selftest}" >&2
    echo "  predicate is NOT runnable in chart CI - it needs live GCP credentials." >&2
    exit 2 ;;
esac
