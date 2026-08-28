#!/usr/bin/env bash
#
# bash32-feature-probe.sh — measure which bash 4+ constructs bash 3.2.57 accepts
#
# WHY THIS EXISTS. .design/hosted/cloud-run-single-node.md carried a row headed
# "Measured" listing ten constructs absent from bash 3.2.57. The version string
# was measured on real Darwin hardware. The feature list beside it was written
# from version history and placed under a header that says it was measured. That
# is the exact false-prose class this branch has already spent three review
# rounds on. This script settles it on real hardware.
#
# THE TRAP. Several of these are PARSE errors in 3.2, not runtime errors. A
# parse error aborts the entire script before line one executes. If you probe
# them in one script you will not measure ten constructs — you will measure one
# parse failure and get a tidy table of nine "unsupported" results that were
# never run. That failure mode produces exactly the output you are expecting,
# which is why it will not look wrong.
#
# So each construct is probed in its own separate subprocess: one
# `bash -c '<construct>'` per construct, with exit status and stderr captured
# separately.
#
# Usage:  scripts/dev/bash32-feature-probe.sh
#         SCION_TEST_BASH=/bin/bash scripts/dev/bash32-feature-probe.sh

set -euo pipefail

BASH_UNDER_TEST="${SCION_TEST_BASH:-bash}"

# probe runs a single construct in its own subprocess and reports the result.
# Arguments: NAME SNIPPET
probe() {
  local name="$1" snippet="$2"
  local rc=0 stderr_file
  stderr_file="$(mktemp)"

  "$BASH_UNDER_TEST" -c "$snippet" >/dev/null 2>"$stderr_file" || rc=$?

  local stderr_first_line
  stderr_first_line="$(head -1 "$stderr_file" 2>/dev/null || true)"
  # Trim to a reasonable length for tabular display
  if [ ${#stderr_first_line} -gt 120 ]; then
    stderr_first_line="${stderr_first_line:0:117}..."
  fi

  local verdict
  if [ "$rc" -eq 0 ] && [ -z "$stderr_first_line" ]; then
    verdict="SUPPORTED"
  elif [ "$rc" -eq 0 ] && [ -n "$stderr_first_line" ]; then
    # Exit 0 but stderr non-empty: the command ran but the flag was silently
    # rejected. declare -A on bash 3.2 does this — declare succeeds but -A is
    # an invalid option, so the variable is indexed, not associative.
    verdict="EXIT 0 BUT REJECTED (unsupported)"
  elif [ -n "$stderr_first_line" ] && echo "$stderr_first_line" | grep -q 'syntax error\|unexpected\|bad substitution\|parse error'; then
    verdict="PARSE ERROR (unsupported)"
  else
    verdict="RUNTIME ERROR (unsupported)"
  fi

  printf '%s\t%d\t%s\t%s\n' "$name" "$rc" "$verdict" "$stderr_first_line"
  rm -f "$stderr_file"
}

# --- Version banner ---
# SC2016: $BASH_VERSION is the child's variable.
# shellcheck disable=SC2016
version="$("$BASH_UNDER_TEST" -c 'echo "$BASH_VERSION"')"
echo "interpreter: $BASH_UNDER_TEST ($version)"
echo ""

# --- Control: must succeed. If this fails the harness is broken. ---
probe "control (printf '%s' hi)" 'printf "%s" hi'
echo ""

# --- The ten constructs, one subprocess each ---
# Leading with printf -v because a correction to the design doc is waiting on it.
#
# PRECONDITION: every snippet must be STDERR-CLEAN on success. The verdict logic
# classifies exit 0 with stderr output as "EXIT 0 BUT REJECTED (unsupported)" —
# that is how it catches declare -A, which exits 0 but prints "invalid option".
# A snippet that legitimately writes to stderr while succeeding will be
# misclassified as unsupported: silently, with a green run, producing a wrong
# row in a matrix the design doc cites as measured. If you add a construct whose
# success path writes to stderr, you must either suppress that output in the
# snippet or extend the verdict logic to distinguish it.
#
# SC2016: every probe snippet is a literal string sent to the interpreter under
# test via bash -c. Expanding $ here would destroy the measurement — we would
# test THIS shell's variables, not the construct in the child.
# shellcheck disable=SC2016
probe 'printf -v'    'printf -v x "%s" hello; [ "$x" = hello ]'
# shellcheck disable=SC2016
probe '${v,,}'       'v=ABC; echo "${v,,}"'
# shellcheck disable=SC2016
probe '${v^^}'       'v=abc; echo "${v^^}"'
# shellcheck disable=SC2016
probe 'declare -A'   'declare -A m; m[k]=v; [ "${m[k]}" = v ]'
probe 'mapfile'      'echo hello | mapfile -t arr'
probe 'readarray'    'echo hello | readarray -t arr'
# shellcheck disable=SC2016
probe 'local -n'     'f() { local -n ref=$1; ref=42; }; x=0; f x; [ "$x" = 42 ]'
probe '[[ -v ]]'     'x=1; [[ -v x ]]'
probe 'wait -n'      'sleep 0 & wait -n'
# shellcheck disable=SC2016
probe 'coproc'       'coproc cat; echo hi >&${COPROC[1]}; exec {COPROC[1]}>&-; read -r line <&${COPROC[0]}; [ "$line" = hi ]'
