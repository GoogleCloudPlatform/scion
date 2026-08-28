#!/usr/bin/env bash
#
# shell-differential.sh — prove a shell change is BEHAVIOUR-PRESERVING, byte for
# byte, across a corpus of hostile inputs.
#
# WHAT THIS IS FOR, AND WHAT IT IS NOT FOR
#
# It is not a test runner and it does not prove a test passes. It answers one
# question: "does the candidate produce the SAME verdict and the SAME diagnostic
# bytes as the baseline, for every input in the corpus?" That is the question a
# portability change raises, and it is not the question a test suite answers.
#
# It exists because of a near miss. Rewriting deploy.sh's `${v,,}` (bash 4.0+,
# fatal on macOS's bash 3.2) into the obvious `$(… | tr …)` looked like a pure
# portability edit and was a SECURITY REGRESSION: command substitution strips
# trailing newlines, and the lowercasing ran BEFORE the host-shape assertion, so
# the plain form deleted a trailing newline before the check that exists to
# reject it. Three inputs flipped REJECT -> ALLOW, including
# `https://oauth2.googleapis.com\n`, which became a permitted host.
#
# THE 44-ROW GO TABLE WENT FULLY GREEN ON THAT REGRESSION. Every row it had was
# a row someone had thought of, and nobody had thought of trailing whitespace.
# The suite could not see it; this instrument could. That is the whole argument
# for keeping it: a portability change is a semantics change until proven
# otherwise, and "the tests still pass" is not that proof.
#
# USAGE
#
#   scripts/dev/shell-differential.sh BASELINE CANDIDATE FUNCTION CORPUS
#
#   BASELINE   shell script to source (e.g. `git show REF:path > /tmp/base.sh`)
#   CANDIDATE  shell script to source — the change under test
#   FUNCTION   function to call; invoked as FUNCTION <label> <value>
#   CORPUS     input file, format below
#
# Exit 0 when every input agrees. Exit 1 on any divergence, printing a unified
# diff of `name / exit code / octal dump of stderr`.
#
# CORPUS FORMAT
#
#   name<TAB>value        one per line; blank lines and #-comments ignored
#
# `value` is expanded with `printf %b`, so \n \r \t \\ and \0NNN work. Escapes
# rather than literal control characters are deliberate: the inputs that matter
# most here END in whitespace, and a literal trailing newline in a corpus file
# is invisible in review and eaten by most editors.
#
# THE INSTRUMENT IS SUBJECT TO ITS OWN FOUNDING BUG, TWICE. Both defences are
# marked below and neither is decoration:
#
#   INPUT  — corpus values are expanded through a command substitution, so the
#            `; printf x` / `%x` sentinel is what stops a trailing newline being
#            eaten before the value ever reaches the function under test. An
#            input corpus that cannot carry a trailing newline cannot test for
#            one.
#   OUTPUT — stderr is captured via a FILE rather than a substitution. The
#            sentinel does not work there: it would mask the exit status of the
#            thing being measured. See the comment at the capture site.

set -euo pipefail

# ---------------------------------------------------------------------------
# SELF-TEST
#
# `shell-differential.sh --self-test` checks the instrument against known
# answers ON THE INTERPRETER IT IS ABOUT TO RUN UNDER, and it is not optional
# ceremony: the first working version of this tool reported
#
#     IDENTICAL: 22 input(s), same exit code and same stderr bytes.
#
# for a candidate in which EVERY verdict had changed. The exit-code column was
# a constant 0 (see the capture site), so half the comparison was dead and the
# message asserting otherwise was false. A differential tool that cannot fail
# is worse than no tool, because it is quoted as evidence.
#
# Each case below pins one load-bearing property, and each was OBSERVED
# POSITIVE by reintroducing the corresponding defect:
#
#   1 identical      baseline vs itself must be IDENTICAL — without this the
#                    other three pass trivially on a tool that always diverges.
#   2 verdict only   same stderr, different exit status. Red iff exit codes are
#                    really compared. This is the bug above.
#   3 stderr only    same exit status, stderr differing ONLY in a trailing
#                    newline. Red iff output capture is byte-exact.
#   4 input fidelity verdict depends on the last byte of the INPUT. Red iff the
#                    corpus sentinel survives; if it is removed both sides see
#                    the same stripped value and agree.
# ---------------------------------------------------------------------------
if [ "${1:-}" = "--self-test" ]; then
  # 'done' is quoted so shellcheck does not read it as the loop keyword (SC1010).
  export SHELL_DIFFERENTIAL_SELFTEST='done'
  d="$(mktemp -d)"
  trap 'rm -rf "$d"' EXIT

  printf 'f() { printf "e\\n" >&2; return 1; }\n'    > "$d/a.sh" # baseline
  printf 'f() { printf "e\\n" >&2; return 3; }\n'    > "$d/rc.sh"
  printf 'f() { printf "e\\n\\n" >&2; return 1; }\n' > "$d/err.sh"
  # Keyed on the final byte of $2 — the value under test. It moves BOTH columns
  # on purpose: keyed on the exit status alone, this case would also go red
  # whenever the exit column was dead, and could not distinguish an input-path
  # defect from case 2's.
  # SC2016: `$2` belongs to the GENERATED script, not this one. Expanding it
  # here would bake in a value and the fixture would stop testing anything.
  # shellcheck disable=SC2016
  printf 'f() { case "$2" in *"\n") printf "t\\n" >&2; return 1 ;; esac; return 0; }\n' > "$d/in_a.sh"
  printf 'f() { return 0; }\n'                                                         > "$d/in_b.sh"

  printf 'plain\tv\ntrailing newline\tv\\n\n' > "$d/corpus.tsv"

  fails=0
  check() { # check DESCRIPTION EXPECTED_EXIT BASELINE CANDIDATE
    local desc="$1" want="$2" got=0
    "$0" "$3" "$4" f "$d/corpus.tsv" > /dev/null 2>&1 || got=$?
    if [ "$got" = "$want" ]; then
      echo "  ok    $desc"
    else
      echo "  FAIL  $desc (want exit $want, got $got)" >&2
      fails=$((fails + 1))
    fi
  }

  echo "self-test: ${SCION_TEST_BASH:-bash}"
  check "identical baseline and candidate agree" 0 "$d/a.sh" "$d/a.sh"
  check "verdict-only change is detected"        1 "$d/a.sh" "$d/rc.sh"
  check "trailing-newline-only stderr change is detected" 1 "$d/a.sh" "$d/err.sh"
  check "input trailing newline reaches the function" 1 "$d/in_a.sh" "$d/in_b.sh"

  if [ "$fails" -ne 0 ]; then
    echo "SELF-TEST FAILED: ${fails} case(s). Do not trust this tool's output." >&2
    exit 1
  fi
  echo "self-test passed."
  exit 0
fi

if [ "$#" -ne 4 ]; then
  echo "usage: $0 BASELINE CANDIDATE FUNCTION CORPUS" >&2
  echo "       $0 --self-test" >&2
  exit 2
fi

# THE SELF-TEST RUNS ON EVERY COMPARISON, NOT ONLY WHEN ASKED. A check behind a
# flag is a check nobody runs, and the property it protects is interpreter-
# dependent: SCION_TEST_BASH points this tool at other shells on purpose, so
# "it worked on the bash I developed on" is not evidence about the bash it is
# running on now. It costs a few subshells. Silent on success, and a failure
# refuses to produce a verdict rather than producing a doubtful one.
if [ "${SHELL_DIFFERENTIAL_SELFTEST:-}" != "done" ]; then
  # 'done' is quoted so shellcheck does not read it as the loop keyword (SC1010).
  if ! st="$(SHELL_DIFFERENTIAL_SELFTEST='done' "$0" --self-test 2>&1)"; then
    echo "$st" >&2
    echo "refusing to compare: the instrument failed its own checks." >&2
    exit 3
  fi
fi

baseline="$1"
candidate="$2"
func="$3"
corpus="$4"

for f in "$baseline" "$candidate" "$corpus"; do
  if [ ! -r "$f" ]; then
    echo "error: cannot read '$f'" >&2
    exit 2
  fi
done

# observe runs FUNCTION under one script for every corpus row and prints a
# stable, diffable record per row.
observe() {
  local script="$1"
  local name value out rc errfile
  errfile="$(mktemp)"

  while IFS=$'\t' read -r name value; do
    case "$name" in '' | '#'*) continue ;; esac

    # printf %b expands the escapes; the sentinel keeps trailing bytes.
    local expanded
    expanded="$(printf '%b' "$value"; printf x)"
    expanded="${expanded%x}"

    # Never let a failing candidate abort the sweep — a non-zero exit IS data.
    # STDERR GOES TO A FILE, NOT A COMMAND SUBSTITUTION, AND THE REASON IS THE
    # BUG THIS TOOL EXISTS TO CATCH. The obvious form,
    #     out="$( … 2>&1 >/dev/null; printf x )"; rc=$?
    # reports rc=0 for EVERY row, because the sentinel `printf x` is the last
    # command in the subshell and its status is what $? returns. The verdict
    # column silently becomes a constant and the tool compares diagnostics only.
    # Measured — it happened here on the first run. A file needs no sentinel:
    # `od` reads the bytes directly, so nothing is stripped and $? is the real
    # exit status of the function under test.
    set +e
    # SC2016: the single quotes are the point. $1/$2/$3 are the INNER shell's
    # positional parameters, passed as argv below. Expanding them out here would
    # interpolate hostile corpus values into the command text — the exact
    # data-becomes-code defect this corpus is built to catch.
    # shellcheck disable=SC2016
    "${SCION_TEST_BASH:-bash}" -c '
        set -euo pipefail
        # shellcheck disable=SC1090
        . "$1"
        "$2" DIFFERENTIAL_INPUT "$3"
      ' _ "$script" "$func" "$expanded" >/dev/null 2>"$errfile"
    rc=$?
    set -e

    # od, not the raw bytes: makes trailing whitespace and CR visible in a diff.
    out="$(od -An -c < "$errfile" | tr -s ' ' | tr -d '\n')"
    printf '%s\t%d\t%s\n' "$name" "$rc" "$out"
  done < "$corpus"
  rm -f "$errfile"
}

# Named files in a temp DIR rather than two bare mktemp files, so the diff
# header reads ".../baseline" and ".../candidate" without `diff --label`.
# --label is GNU diffutils; macOS ships BSD diff. Depending on a GNU-only flag
# in the tool whose job is proving macOS portability would be its own punchline.
outdir="$(mktemp -d)"
base_out="$outdir/baseline"
cand_out="$outdir/candidate"
trap 'rm -rf "$outdir"' EXIT

observe "$baseline"  > "$base_out"
observe "$candidate" > "$cand_out"

rows="$(grep -cve '^[[:space:]]*$' -e '^[[:space:]]*#' "$corpus" || true)"

if diff -u "$base_out" "$cand_out" > /dev/null; then
  echo "IDENTICAL: ${rows} input(s), same exit code and same stderr bytes."
  exit 0
fi

echo "DIVERGENT: the candidate does not preserve behaviour." >&2
echo "  baseline:  $baseline" >&2
echo "  candidate: $candidate" >&2
echo >&2
# `|| true` so the exit status below is OURS. diff exits 1 for "differs" and 2
# for "trouble", and under `set -e` either would abort here and become the
# script's status -- silently turning a clean DIVERGENT verdict into a 2 that
# callers read as a crash.
diff -u "$base_out" "$cand_out" >&2 || true
exit 1
