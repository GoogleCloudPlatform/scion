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
#   - a routed job cannot or must not run there: it has services:, container:,
#     a uses: docker:// step or a docker command in a step (docker run, build,
#     buildx, compose, pull, push, exec, login, create, start, also after global
#     options such as -H host or --log-level debug, or docker-compose; no Docker
#     on Cloud Run; other uses of the word, such as "Dockerfile", do not match),
#     its permissions (its own, or the workflow's when it has none) grant write,
#     or its workflow is triggered by pull_request_target;
#   - a routed job has no timeout-minutes, or one outside 1-35 (the runner's
#     task timeout is 45 min, minus its 10 min idle timeout). A job whose
#     hosted runs need longer may use the guarded form
#       ${{ <the runs-on guard> && vars.SCION_LARGE_RUNNER && N || M }}
#     (the same guard as the runs-on, so fork pull requests and the other
#     unrouted events keep M on ubuntu-latest): N (the cr-ci value) must be
#     1-35. No other job line may read vars.SCION_LARGE_RUNNER;
#   - any job's runs-on names a self-hosted runner literally (self-hosted or a
#     scion-cr-* label, inline or as a block list) instead of through the
#     canonical expression.
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
    BEGIN {
      # The guard is the canonical runs-on between "runs-on: ${{ " and
      # " && vars.SCION_LARGE_RUNNER || 'ubuntu-latest' }}"; the timeout form
      # reuses it byte for byte.
      head = "runs-on: ${{ "
      tail = " && vars.SCION_LARGE_RUNNER || \047ubuntu-latest\047 }}"
      guard = substr(canon, length(head) + 1, length(canon) - length(head) - length(tail))
      tprefix = "${{ " guard " && vars.SCION_LARGE_RUNNER && "
      tform_doc = "${{ <the runs-on guard> && vars.SCION_LARGE_RUNNER && N || M }}"
    }
    # tform VALUE: N if VALUE is the guarded timeout form, "" otherwise.
    function tform(v,   r) {
      if (index(v, tprefix) != 1) return ""
      r = substr(v, length(tprefix) + 1)
      if (r !~ /^[0-9]+ \|\| [0-9]+ \}\}$/) return ""
      sub(/ \|\|.*$/, "", r)
      return r
    }
    function tvalue(line,   t) {
      t = line; sub(/^    timeout-minutes:[[:space:]]*/, "", t); sub(/[[:space:]]+(#.*)?$/, "", t)
      return t
    }
    function flush() {
      if (job == "" || !routed) return
      if (svc != "") printf "%s: job %s is routed to the cr-ci runner but has %s (no Docker on Cloud Run); keep it on ubuntu-latest\n", file, job, svc
      if (dock) printf "%s: job %s is routed to the cr-ci runner but has a uses: docker:// step (no Docker on Cloud Run); keep it on ubuntu-latest\n", file, job
      if (dcmd) printf "%s:%d: job %s is routed to the cr-ci runner but runs docker (no Docker on Cloud Run); keep it on ubuntu-latest\n", file, dcmd, job
      if (wperm) printf "%s: job %s is routed to the cr-ci runner but its permissions grant write; keep it on a GitHub-hosted runner\n", file, job
      else if (!hasperm && topw) printf "%s: job %s is routed to the cr-ci runner but inherits the workflow permissions, which grant write; keep it on a GitHub-hosted runner\n", file, job
      if (prt) printf "%s: job %s is routed to the cr-ci runner but its workflow runs on pull_request_target; keep it on a GitHub-hosted runner\n", file, job
      if (tmo == "") printf "%s: job %s is routed to the cr-ci runner but has no timeout-minutes; set one from 1 to %d\n", file, job, maxt
      else if (tmo !~ /^[0-9]+$/ || tmo + 0 < 1 || tmo + 0 > maxt) printf "%s: job %s is routed to the cr-ci runner with timeout-minutes %s; it must be a number from 1 to %d\n", file, job, tmo, maxt
    }
    /^jobs:[[:space:]]*$/ { injobs = 1; intopperm = 0; next }
    injobs && /^[^[:space:]#]/ { flush(); injobs = 0; job = "" }
    !injobs {
      if ($0 ~ /^[[:space:]]*#/) next
      if ($0 ~ /vars\.SCION_LARGE_RUNNER/)
        printf "%s:%d: vars.SCION_LARGE_RUNNER outside a job runs-on; only a job-level %s may use it\n", file, NR, canon
      if ($0 ~ /^[^[:space:]]/) intopperm = 0
      if ($0 ~ /^permissions:/) { if ($0 ~ /write/) topw = 1; intopperm = 1; next }
      if (intopperm && $0 ~ /^  [A-Za-z_-]+:[[:space:]]*write/) topw = 1
      next
    }
    /^  [A-Za-z0-9_-]+:[[:space:]]*$/ {
      flush()
      job = $0; sub(/^  /, "", job); sub(/:.*/, "", job)
      routed = 0; svc = ""; dock = 0; dcmd = 0; wperm = 0; hasperm = 0; tmo = ""; inperm = 0; inrunson = 0
      next
    }
    /^[[:space:]]*#/ { next }
    /^    [A-Za-z_-]+:/ { inrunson = 0 }
    /^    runs-on:[[:space:]]*$/ { inrunson = 1; next }
    (/^    runs-on:/ && !/vars\.SCION_LARGE_RUNNER/ || inrunson && /^      /) && /self-hosted|scion-cr-/ {
      printf "%s:%d: job %s names a self-hosted runner literally; route it only through the canonical expression:\n    %s\n", file, NR, job, canon
    }
    /uses:[[:space:]]*["\047]?docker:\/\// { dock = 1 }
    # A docker command anywhere in the job (comment lines were skipped above); the
    # first one is reported. Global options may come between docker and the
    # subcommand (docker -H host run, docker --log-level debug build,
    # docker --context x compose up, docker -D pull): each is a token starting with
    # "-", optionally followed by one value token that does not. A false match only
    # keeps a job on ubuntu-latest. Known gap: a quoted option value that contains
    # whitespace (docker --config "/a b" run x) is not matched, because the value
    # token ends at the space; such a job would fail on the runner (no Docker).
    !dcmd && /(^|[;&|(`[:space:]])docker([[:space:]]+-[^[:space:]]+([[:space:]]+[^-[:space:]][^[:space:]]*)?)*[[:space:]]+(run|build|buildx|compose|pull|push|exec|login|create|start)([[:space:]]|$)|(^|[;&|(`[:space:]])docker-compose([[:space:]]|$)/ { dcmd = NR }
    /^    runs-on:.*vars\.SCION_LARGE_RUNNER/ {
      line = $0; sub(/[[:space:]]+$/, "", line)
      if (line == "    " canon) routed = 1
      else printf "%s:%d: job %s: a runs-on that reads vars.SCION_LARGE_RUNNER must be exactly:\n    %s\n  got:\n%s\n", file, NR, job, canon, line
    }
    /vars\.SCION_LARGE_RUNNER/ && !/^    runs-on:/ && !(/^    timeout-minutes:/ && tform(tvalue($0)) != "") {
      printf "%s:%d: job %s reads vars.SCION_LARGE_RUNNER outside runs-on and the guarded timeout-minutes form; only the canonical runs-on and %s may use it\n", file, NR, job, tform_doc
    }
    /^    [A-Za-z_-]+:/ { inperm = 0 }
    /^    (services|container):/ { k = $0; sub(/^    /, "", k); sub(/:.*/, "", k); svc = (svc == "" ? k ":" : svc " and " k ":") }
    /^    timeout-minutes:/ {
      # The guarded form keeps a longer hosted timeout (M); N is what the cr-ci
      # runner gets, and what counts here.
      t = tvalue($0)
      n = tform(t)
      tmo = (n != "" ? n : t)
    }
    /^    permissions:/ {
      hasperm = 1
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
  # Fixtures are part of the test: a missing good dir fails above (check_dir finds no
  # workflow), and no bad-* dir, or one without its want file, fails here, so a
  # broken fixture set cannot pass by skipping (an unmatched glob is left literal).
  local nbad=0
  for d in "$td"/bad-*/; do
    [ -d "$d" ] || continue
    d=${d%/}
    nbad=$((nbad + 1))
    if [ ! -r "$d/want" ]; then
      echo "selftest FAILED: ${d##*/} has no want file"; fail=1
      continue
    fi
    want=$(<"$d/want")
    rc=0; out=$(check_dir "$d" 2>&1) || rc=$?
    if [ "$rc" -eq 1 ] && grep -qF -- "$want" <<<"$out"; then
      echo "selftest ok: ${d##*/} fails with: $want"
    else
      echo "selftest FAILED: ${d##*/} (exit $rc) did not report: $want"; printf '%s\n' "$out"; fail=1
    fi
  done
  if [ "$nbad" -eq 0 ]; then
    echo "selftest FAILED: no bad-* fixtures in $td"; fail=1
  fi
  return "$fail"
}

if [ "${1:-}" = "--selftest" ]; then
  selftest
  exit
fi
dir=${1:-.github/workflows}
if check_dir "$dir"; then
  routed_files=0
  for f in "$dir"/*.yml "$dir"/*.yaml; do
    [ -e "$f" ] && grep -qF 'vars.SCION_LARGE_RUNNER' "$f" && routed_files=$((routed_files + 1))
  done
  echo "check-runner-routing: ok ($routed_files workflow file(s) route jobs)"
else
  rc=$?
  echo "check-runner-routing: FAILED; the canonical routing is:" >&2
  echo "    $CANONICAL" >&2
  exit "$rc"
fi
