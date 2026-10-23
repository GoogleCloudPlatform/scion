#!/usr/bin/env bash
# check-runner-routing.sh: keep the cr-ci runner routing in .github/workflows
# consistent (cr-ci/docs/architecture.md, "Routing").
#
# A job is routed to the cr-ci runner (Cloud Run) when its runs-on reads the
# repository variable SCION_LARGE_RUNNER. The variable is unset upstream, so
# there every job runs on ubuntu-latest; a fork sets it to its runner label.
# This check fails when:
#   - a runs-on that mentions vars.SCION_LARGE_RUNNER is not exactly the
#     canonical expression below (the fork-PR guard keeps forks,
#     pull_request_target, schedule and workflow_dispatch on ubuntu-latest);
#   - a routed job cannot or must not run there: it has services: or
#     container: (no Docker on Cloud Run), its permissions grant write, or
#     its workflow is triggered by pull_request_target;
#   - a routed job has no timeout-minutes, or one above 35 (the runner's task
#     timeout is 45 min, minus its 10 min idle timeout).
#
# Usage: hack/check-runner-routing.sh [WORKFLOW_DIR]   (default .github/workflows)
#        hack/check-runner-routing.sh --selftest       (runs the fixtures in
#                                                       hack/testdata/runner-routing)
# Needs bash, grep and awk only.
set -euo pipefail

CANONICAL="runs-on: \${{ (github.event_name == 'push' || (github.event_name == 'pull_request' && github.event.pull_request.head.repo.full_name == github.repository)) && vars.SCION_LARGE_RUNNER || 'ubuntu-latest' }}"
MAX_TIMEOUT=35

# check_file FILE: prints one error per line; job-level keys are at 4 spaces
# under the top-level jobs: map (2-space indentation, as every workflow here).
check_file() {
  local f=$1 prt=0
  if grep -qE '^on:.*pull_request_target|^  pull_request_target:|^  - pull_request_target' "$f"; then
    prt=1
  fi
  awk -v file="$f" -v canon="$CANONICAL" -v maxt="$MAX_TIMEOUT" -v prt="$prt" '
    function flush() {
      if (job == "" || !routed) return
      if (svc != "") printf "%s: job %s is routed to the cr-ci runner but has %s (no Docker on Cloud Run); keep it on ubuntu-latest\n", file, job, svc
      if (wperm) printf "%s: job %s is routed to the cr-ci runner but its permissions grant write; keep it on a GitHub-hosted runner\n", file, job
      if (prt) printf "%s: job %s is routed to the cr-ci runner but its workflow runs on pull_request_target; keep it on a GitHub-hosted runner\n", file, job
      if (tmo == "") printf "%s: job %s is routed to the cr-ci runner but has no timeout-minutes; set one of at most %d\n", file, job, maxt
      else if (tmo !~ /^[0-9]+$/ || tmo + 0 > maxt) printf "%s: job %s is routed to the cr-ci runner with timeout-minutes %s; it must be a number of at most %d\n", file, job, tmo, maxt
    }
    /^jobs:[[:space:]]*$/ { injobs = 1; next }
    injobs && /^[^[:space:]#]/ { flush(); injobs = 0; job = "" }
    !injobs {
      if ($0 ~ /vars\.SCION_LARGE_RUNNER/ && $0 !~ /^[[:space:]]*#/)
        printf "%s:%d: vars.SCION_LARGE_RUNNER outside a job runs-on; only a job-level %s may use it\n", file, NR, canon
      next
    }
    /^  [A-Za-z0-9_-]+:[[:space:]]*$/ {
      flush()
      job = $0; sub(/^  /, "", job); sub(/:.*/, "", job)
      routed = 0; svc = ""; wperm = 0; tmo = ""; inperm = 0
      next
    }
    /^[[:space:]]*#/ { next }
    /vars\.SCION_LARGE_RUNNER/ {
      line = $0; sub(/[[:space:]]+$/, "", line)
      if (line == "    " canon) routed = 1
      else printf "%s:%d: job %s: a runs-on that reads vars.SCION_LARGE_RUNNER must be exactly:\n    %s\n  got:\n    %s\n", file, NR, job, canon, line
    }
    /^    [A-Za-z_-]+:/ { inperm = 0 }
    /^    (services|container):/ { k = $0; sub(/^    /, "", k); sub(/:.*/, "", k); svc = (svc == "" ? k ":" : svc " and " k ":") }
    /^    timeout-minutes:/ { t = $0; sub(/^    timeout-minutes:[[:space:]]*/, "", t); sub(/[[:space:]]+(#.*)?$/, "", t); tmo = t }
    /^    permissions:/ {
      if ($0 ~ /write/) wperm = 1
      inperm = 1; next
    }
    inperm && /^      [A-Za-z_-]+:[[:space:]]*write/ { wperm = 1 }
    END { flush() }
  ' "$f"
}

# check_dir DIR: prints every error for DIR's workflows; exit 1 if any.
check_dir() {
  local dir=$1 out="" f
  local -a files=()
  for f in "$dir"/*.yml "$dir"/*.yaml; do
    [ -e "$f" ] && files+=("$f")
  done
  if [ "${#files[@]}" -eq 0 ]; then
    echo "check-runner-routing: no workflow files in $dir" >&2
    return 2
  fi
  local o
  for f in "${files[@]}"; do
    o=$(check_file "$f")
    [ -z "$o" ] || out+="$o"$'\n'
  done
  if [ -n "$out" ]; then
    printf '%s' "$out"
    return 1
  fi
  return 0
}

selftest() {
  local here td fail=0 out rc want
  here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
  td="$here/testdata/runner-routing"
  if out=$(check_dir "$td/good" 2>&1); then
    echo "selftest ok: good fixtures pass"
  else
    echo "selftest FAILED: good fixtures reported:"; printf '%s\n' "$out"; fail=1
  fi
  for d in "$td"/bad-*/; do
    d=${d%/}
    want=$(<"$d/want")
    rc=0; out=$(check_dir "$d" 2>&1) || rc=$?
    if [ "$rc" -eq 1 ] && grep -qF -- "$want" <<<"$out"; then
      echo "selftest ok: ${d##*/} fails with: $want"
    else
      echo "selftest FAILED: ${d##*/} (exit $rc) did not report: $want"; printf '%s\n' "$out"; fail=1
    fi
  done
  return "$fail"
}

if [ "${1:-}" = "--selftest" ]; then
  selftest
  exit
fi
dir=${1:-.github/workflows}
if check_dir "$dir"; then
  echo "check-runner-routing: ok ($(grep -lF 'vars.SCION_LARGE_RUNNER' "$dir"/*.yml 2>/dev/null | wc -l) workflow file(s) route jobs)"
else
  rc=$?
  echo "check-runner-routing: FAILED; the canonical routing is:" >&2
  echo "    $CANONICAL" >&2
  exit "$rc"
fi
