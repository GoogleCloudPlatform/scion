#!/usr/bin/env bash
#
# bash32-regex-probe.sh — does `[[ =~ ]]` + BASH_REMATCH behave the same on
# bash 3.2 as on bash 5?
#
# WHY THIS FILE EXISTS AS A FILE. The bash 3.2 answer below was obtained by a
# human running this block on his own Mac (bash 3.2.57, arm64 Darwin), before
# this repo had any macOS CI. It is recorded rather than remembered because a
# result nobody can reproduce on demand degrades into "someone said it once".
# If you are tempted to re-litigate "does 3.2 handle =~ differently", read
# EXPECTED_BASH32 first.
#
# It is kept now that .github/workflows/macos-bash32.yml exists, because the
# workflow does not replace it -- the workflow RUNS it, so every macOS job
# checks a second real 3.2 against this record on different hardware. One
# laptop is an anecdote; a laptop plus a runner that keeps agreeing is a pin.
#
# THE QUESTION IS NOT ACADEMIC. bash 3.2 changed the meaning of a QUOTED
# right-hand side: from 3.2 on, quoting the pattern makes it match LITERALLY
# instead of as a regex, so `[[ $h =~ "^(.*):[0-9]+$" ]]` silently stops
# matching anything real. deploy.sh's port-stripping (the block probed here)
# and its host-shape assertion both feed a security decision, so "the regex
# quietly matches nothing" is fail-open. Every =~ site in deploy.sh is
# unquoted, which is why the trap does not apply as written — case 5 below
# pins that the trap is REAL, so nobody "tidies" a pattern into quotes.
#
# Usage:  scripts/dev/bash32-regex-probe.sh          # run under $SCION_TEST_BASH or bash
#         scripts/dev/bash32-regex-probe.sh --check  # also diff against the 3.2 record
#
# --check exits non-zero if this interpreter disagrees with bash 3.2.57.

set -euo pipefail

# Verbatim output from bash 3.2.57 on arm64 Darwin (macOS), captured 2026-08-28.
EXPECTED_BASH32='example.com:443	port stripped	host=example.com
example.com	no port	host=example.com
[::1]	no port	host=[::1]
[::1]:8080	port stripped	host=[::1]
quoted RHS	no match	(literal, not regex)'

# SC2317: this body is not dead. It is shipped to the interpreter under test
# with `declare -f` and invoked there, so shellcheck cannot see the call site.
# Running it in THIS shell would defeat the point — the whole question is how
# ANOTHER bash behaves.
# shellcheck disable=SC2317
probe() {
  local host
  for host in 'example.com:443' 'example.com' '[::1]' '[::1]:8080'; do
    # Exactly the construct in scripts/single-node/deploy.sh — unquoted RHS.
    if [[ "$host" =~ ^(.*):[0-9]+$ ]]; then
      printf '%s\tport stripped\thost=%s\n' "$host" "${BASH_REMATCH[1]}"
    else
      printf '%s\tno port\thost=%s\n' "$host" "$host"
    fi
  done

  # Case 5: the trap itself. A QUOTED pattern must NOT match, on every
  # supported interpreter. If this ever prints "MATCHED", the quoting rule has
  # changed and the unquoted-RHS requirement above needs revisiting.
  local h='example.com:443'
  # shellcheck disable=SC2076
  if [[ "$h" =~ "^(.*):[0-9]+$" ]]; then
    printf 'quoted RHS\tMATCHED\t(regex — differs from 3.2)\n'
  else
    printf 'quoted RHS\tno match\t(literal, not regex)\n'
  fi
}

actual="$("${SCION_TEST_BASH:-bash}" -c "$(declare -f probe); probe")"

if [ "${1:-}" != "--check" ]; then
  printf '%s\n' "$actual"
  exit 0
fi

# SC2016: $BASH_VERSION must be expanded by the shell UNDER TEST, not this one.
# shellcheck disable=SC2016
version="$("${SCION_TEST_BASH:-bash}" -c 'echo "$BASH_VERSION"')"
printf 'interpreter: %s (%s)\n' "${SCION_TEST_BASH:-bash}" "$version"

if [ "$actual" = "$EXPECTED_BASH32" ]; then
  echo "IDENTICAL to bash 3.2.57 on all 5 cases."
  exit 0
fi

echo "DIVERGENT from bash 3.2.57:" >&2

# Named files, not `diff --label`: --label is GNU diffutils and macOS ships BSD
# diff. A probe for macOS portability must not itself depend on a GNU-only
# flag. `|| true` keeps the exit status ours -- diff returns 1 for "differs"
# and 2 for "trouble", and under `set -e` either would become this script's
# status and read as a crash rather than a verdict.
d="$(mktemp -d)"
trap 'rm -rf "$d"' EXIT
printf '%s\n' "$EXPECTED_BASH32" > "$d/bash-3.2.57"
printf '%s\n' "$actual"          > "$d/this-shell"
diff -u "$d/bash-3.2.57" "$d/this-shell" >&2 || true
exit 1
