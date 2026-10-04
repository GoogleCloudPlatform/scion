#!/usr/bin/env bash
# Shared dependency check for hack/check-*.sh scripts. Source it, then call
# require_tool once per external tool the check measures with:
#
#   # shellcheck source=SCRIPTDIR/lib/require-tool.sh
#   source "$(dirname "$0")/lib/require-tool.sh"
#   require_tool rg check-foo
#
# A missing tool exits 3 at every severity level (see
# hack/LINT-CONVENTIONS.md): a run that analysed nothing must never read as
# a clean pass (ptone/scion#1114). The message goes to both streams so a
# consumer reading either one sees why the check failed.

# require_tool <tool> <check-name> [<description>]
require_tool() {
  local tool="$1" check="$2" desc="${3:-$1}"
  if ! command -v "$tool" >/dev/null 2>&1; then
    local msg="${check}: ${desc} not found — NOTHING WAS ANALYSED (skipped, not clean)"
    echo "$msg"
    echo "$msg" >&2
    exit 3
  fi
}
