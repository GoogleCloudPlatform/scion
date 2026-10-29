#!/usr/bin/env bash
# check-fork-ci-gate.sh: keep the fork CI gate in .github/workflows complete
# (cr-ci/workflows/README.md, "Fork CI gate").
#
# Forks of GoogleCloudPlatform/scion (e.g. ptone/scion) are for staging pull
# requests, so push and schedule runs happen only upstream. Every job in a
# workflow triggered by a branch push or a schedule carries the gate as its
# job-level if:, alone or ANDed in front of the job's own condition:
#     if: ${{ <GATE> }}
#     if: ${{ (<GATE>) && (<the job's own condition>) }}
# A job whose if: is exactly the stricter upstream-only guard
#     if: github.repository == 'GoogleCloudPlatform/scion'   (or in ${{ }})
# also passes. pull_request, pull_request_target and workflow_dispatch runs are
# untouched by the gate, on the fork too.
# This check fails when a job in such a workflow has no if:, or an if: that
# does not start with the gate in one of those forms. Push triggers that only
# filter tags (tags/tags-ignore without branches/branches-ignore) are skipped,
# and build-release.yml is skipped by name: its tag pushes are not gated yet
# (its jobs carry the upstream-only guard instead).
#
# Usage: hack/check-fork-ci-gate.sh [WORKFLOW_DIR]   (default .github/workflows)
#        hack/check-fork-ci-gate.sh --selftest       (runs the fixtures in
#                                                     hack/testdata/fork-ci-gate)
# Needs bash, grep and awk only.
set -euo pipefail

GATE="(github.event_name != 'push' && github.event_name != 'schedule') || github.repository == 'GoogleCloudPlatform/scion'"
REPO_GUARD="github.repository == 'GoogleCloudPlatform/scion'"
SKIP_FILES="build-release.yml"

# check_file FILE: prints one error per line. Triggers are read from the
# top-level on: (block map, block list or inline); job-level keys are at 4
# spaces under the top-level jobs: map (2-space indentation, as every workflow
# here). Prints nothing for a workflow out of scope.
check_file() {
  local f=$1 base
  base=${f##*/}
  case " $SKIP_FILES " in *" $base "*) return 0 ;; esac
  awk -v file="$f" -v gate="$GATE" -v repo="$REPO_GUARD" '
    function flush(   v, ok, pre, suf) {
      if (job == "") return
      if (!hasif) {
        errs[++nerr] = sprintf("%s:%d: job %s has no if:; on push and schedule it must carry the fork CI gate", file, jobline, job)
      } else {
        v = ifval
        pre = "${{ (" gate ") && ("
        suf = ") }}"
        ok = (v == "${{ " gate " }}") || (v == repo) || (v == "${{ " repo " }}")
        if (!ok && index(v, pre) == 1 && length(v) > length(pre) + length(suf) && substr(v, length(v) - length(suf) + 1) == suf) ok = 1
        if (!ok) errs[++nerr] = sprintf("%s:%d: job %s: if: does not apply the fork CI gate:\n  got:\n    if: %s", file, ifline, job, v)
      }
      job = ""
    }
    # Top-level on: (also "on" quoted). Inline value: a bare event or a flow list.
    /^["\047]?on["\047]?:/ {
      v = $0; sub(/^["\047]?on["\047]?:[[:space:]]*/, "", v); sub(/[[:space:]]*#.*$/, "", v)
      if (v == "") { inon = 1; next }
      gsub(/[][ ]/, "", v); n = split(v, ev, ",")
      for (i = 1; i <= n; i++) {
        if (ev[i] == "push") { push = 1; pbranch = 1 }
        if (ev[i] == "schedule") sched = 1
      }
      next
    }
    inon && /^[^ #]/ { inon = 0; trig = "" }
    inon && /^  - *[a-z_]+/ {
      v = $0; sub(/^  - */, "", v); sub(/[[:space:]#].*$/, "", v)
      if (v == "push") { push = 1; pbranch = 1 }
      if (v == "schedule") sched = 1
      next
    }
    inon && /^  [a-z_]+:/ {
      trig = $0; sub(/^  /, "", trig); sub(/:.*/, "", trig)
      if (trig == "push") push = 1
      if (trig == "schedule") sched = 1
      next
    }
    inon && trig == "push" && /^    (branches|branches-ignore):/ { pbranch = 1 }
    inon && trig == "push" && /^    (tags|tags-ignore):/ { ptags = 1 }

    /^jobs:/ { injobs = 1; next }
    injobs && /^[^ #]/ { flush(); injobs = 0 }
    injobs && /^  [A-Za-z0-9_-]+:[[:space:]]*(#.*)?$/ {
      flush()
      job = $0; sub(/^  /, "", job); sub(/:.*/, "", job)
      jobline = NR; hasif = 0; ifval = ""
      next
    }
    injobs && job != "" && !hasif && /^    if:/ {
      hasif = 1; ifline = NR
      ifval = $0; sub(/^    if:[[:space:]]*/, "", ifval); sub(/[[:space:]]+$/, "", ifval)
    }
    END {
      flush()
      # A push that filters only tags never runs for a branch push.
      if (push && ptags && !pbranch) push = 0
      if (!push && !sched) exit
      for (i = 1; i <= nerr; i++) print errs[i]
    }
  ' "$f"
}

# gated FILE: exit 0 if FILE is in scope (branch push or schedule, not skipped).
gated() {
  local f=$1
  case " $SKIP_FILES " in *" ${f##*/} "*) return 1 ;; esac
  # Reuse check_file on a copy with every job removed: in scope iff an
  # injected job without an if: is reported.
  local tmp
  tmp=$(mktemp)
  awk '/^jobs:/ { exit } { print }' "$f" >"$tmp"
  printf 'jobs:\n  probe:\n    runs-on: x\n' >>"$tmp"
  local o
  o=$(check_file "$tmp")
  rm -f "$tmp"
  [ -n "$o" ]
}

# check_dir DIR: prints every error for DIR's workflows; exit 1 if any.
check_dir() {
  local dir=$1 out="" f
  local -a files=()
  for f in "$dir"/*.yml "$dir"/*.yaml; do
    [ -e "$f" ] && files+=("$f")
  done
  if [ "${#files[@]}" -eq 0 ]; then
    echo "check-fork-ci-gate: no workflow files in $dir" >&2
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
  td="$here/testdata/fork-ci-gate"
  if out=$(check_dir "$td/good" 2>&1); then
    echo "selftest ok: good fixtures pass"
  else
    echo "selftest FAILED: good fixtures reported:"; printf '%s\n' "$out"; fail=1
  fi
  # The good fixtures must exercise both sides of the scope rule: these are
  # in scope, the rest (tag-only push, pull_request only, build-release.yml)
  # are not.
  local f
  for f in push.yml schedule.yml push-all.yml; do
    if [ -r "$td/good/$f" ] && gated "$td/good/$f"; then
      echo "selftest ok: good/$f is in scope"
    else
      echo "selftest FAILED: good/$f is missing or not in scope"; fail=1
    fi
  done
  for f in tags-only.yml pr-only.yml build-release.yml; do
    if [ -r "$td/good/$f" ] && ! gated "$td/good/$f"; then
      echo "selftest ok: good/$f is out of scope"
    else
      echo "selftest FAILED: good/$f is missing or in scope"; fail=1
    fi
  done
  # Fixtures are part of the test: a missing good dir fails above (check_dir finds no
  # workflow), and no bad-* dir, or one without its want file, fails here, so a
  # broken fixture set cannot pass by skipping (an unmatched glob is left literal).
  local nbad=0 d
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
  n=0
  for f in "$dir"/*.yml "$dir"/*.yaml; do
    [ -e "$f" ] && gated "$f" && n=$((n + 1))
  done
  echo "check-fork-ci-gate: ok ($n push- or schedule-triggered workflow file(s) gated)"
else
  rc=$?
  echo "check-fork-ci-gate: FAILED; every job in a push- or schedule-triggered workflow needs:" >&2
  echo "    if: \${{ $GATE }}" >&2
  echo "  or, with the job's own condition:" >&2
  echo "    if: \${{ ($GATE) && (<condition>) }}" >&2
  exit "$rc"
fi
