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
# is not exactly one of those forms. In the ANDed form the job's own condition
# must be one balanced operand, so the closing ") }}" closes the "(" after
# "&&": "(<GATE>) && (a) || (b)" is rejected (it runs whenever b holds). The
# if: must be on one line (a block scalar is rejected); a trailing comment is
# ignored. It fails closed ("unrecognised layout", exit 1) in every workflow
# except build-release.yml on what it does not parse: an on: value that is
# not a bare event or a one-line list of bare events (a flow map, a list over
# several lines), any line under a block on: that is not a 2-space "- event"
# entry, a 2-space "event:" key or 4+-space settings of such a key (so
# column-0 entries, a list or scalar on the next line, and 4-space entries
# fail), an on: with no entry (block or "[]"), no top-level on: or jobs: key
# (e.g. quoted "jobs":), an inline jobs: value, a job key that is not an
# unquoted 2-space key, and a jobs: with no job.
# Push triggers that only filter tags (tags/tags-ignore without
# branches/branches-ignore) are skipped. build-release.yml is skipped by name,
# layout rules included: its tag pushes are not gated yet (its jobs carry the
# upstream-only guard instead).
#
# Usage: hack/check-fork-ci-gate.sh [WORKFLOW_DIR]   (default .github/workflows)
#        hack/check-fork-ci-gate.sh --selftest       (runs the fixtures in
#                                                     hack/testdata/fork-ci-gate)
# Exit: 0 every gated job carries the gate; 1 gate or layout findings;
#       2 no workflow files, or a workflow could not be checked (not a
#       readable regular file, or its check failed or was killed).
# Needs bash, grep and awk only.
set -euo pipefail

GATE="(github.event_name != 'push' && github.event_name != 'schedule') || github.repository == 'GoogleCloudPlatform/scion'"
REPO_GUARD="github.repository == 'GoogleCloudPlatform/scion'"
SKIP_FILES="build-release.yml"

# One private temp dir for gated()'s probe files, created here in the main
# shell (an EXIT trap is not inherited by $(...) subshells, so a dir made
# lazily inside one could leak) and removed on any exit: normal, early under
# set -e, or on INT/TERM. On a signal the handler cleans up, restores the
# default action and re-sends the signal to $$, which is the main script
# even inside a $(...) subshell, so the script dies of that signal. A plain
# "exit 130" run inside a subshell would end only the subshell, and the
# main shell would carry on.
PROBE_DIR=$(mktemp -d "${TMPDIR:-/tmp}/check-fork-ci-gate.XXXXXX")
cleanup() { rm -rf -- "$PROBE_DIR"; }
on_signal() {
  trap - "$1"
  cleanup
  kill -"$1" "$$"
}
trap cleanup EXIT
trap 'on_signal INT' INT
trap 'on_signal TERM' TERM

# check_file FILE: prints one error per line. Triggers are read from the
# top-level on: (block map, block list or inline); job-level keys are at 4
# spaces under the top-level jobs: map (2-space indentation, as every workflow
# here). Prints nothing for a workflow out of scope.
check_file() {
  local f=$1 base
  base=${f##*/}
  case " $SKIP_FILES " in *" $base "*) return 0 ;; esac
  awk -v file="$f" -v gate="$GATE" -v repo="$REPO_GUARD" '
    # wrapped MID: 1 if MID, the text between "${{ (<gate>) && (" and ") }}",
    # is one balanced operand, so the closing ")" of the suffix is the one the
    # prefix opened (rejects "(<gate>) && (a) || (b)"). Parens inside
    # single-quoted string literals do not count; a doubled quote inside
    # a literal toggles twice, so it needs no special case.
    function wrapped(m,   i, c, d, q) {
      d = 1; q = 0
      for (i = 1; i <= length(m); i++) {
        c = substr(m, i, 1)
        if (c == "\047") { q = !q; continue }
        if (q) continue
        if (c == "(") d++
        else if (c == ")") { d--; if (d < 1) return 0 }
      }
      return d == 1 && !q
    }
    function layout(n, what) {
      lerrs[++nlerr] = sprintf("%s:%d: unrecognised layout: %s; this check reads only 2-space block YAML (unquoted keys), so it cannot verify the fork CI gate here", file, n, what)
    }
    function flush(   v, ok, pre, suf, m) {
      if (job == "") return
      if (!hasif) {
        errs[++nerr] = sprintf("%s:%d: job %s has no if:; on push and schedule it must carry the fork CI gate", file, jobline, job)
      } else if (ifval ~ /^[>|]/) {
        errs[++nerr] = sprintf("%s:%d: job %s: if: is a block scalar; write the fork CI gate on the if: line itself", file, ifline, job)
      } else {
        v = ifval
        pre = "${{ (" gate ") && ("
        suf = ") }}"
        ok = (v == "${{ " gate " }}") || (v == repo) || (v == "${{ " repo " }}")
        if (!ok && index(v, pre) == 1 && length(v) > length(pre) + length(suf) && substr(v, length(v) - length(suf) + 1) == suf) {
          m = substr(v, length(pre) + 1, length(v) - length(pre) - length(suf))
          ok = wrapped(m)
        }
        if (!ok) errs[++nerr] = sprintf("%s:%d: job %s: if: does not apply the fork CI gate:\n  got:\n    if: %s", file, ifline, job, v)
      }
      job = ""
    }
    # Top-level on: (also "on" quoted). Every line of it must be consumed by
    # a rule below; anything else is an unrecognised layout (fails closed).
    # Inline value: a bare event or a one-line flow list of bare events.
    /^["\047]?on["\047]?:/ {
      onseen = 1
      v = $0; sub(/^["\047]?on["\047]?:[[:space:]]*/, "", v); sub(/[[:space:]]*#.*$/, "", v)
      if (v == "") { inon = 1; onentries = 0; trig = ""; next }
      if (v ~ /^\{/) { layout(NR, "flow-map on:"); next }
      if (v ~ /^\[/ && v !~ /\]$/) { layout(NR, "on: flow list not closed on its line"); next }
      gsub(/[][ ]/, "", v); n = split(v, ev, ",")
      if (n == 0) { layout(NR, "on: with no entry read"); next }
      for (i = 1; i <= n; i++) {
        if (ev[i] !~ /^[a-z_]+$/) { layout(NR, "on: value not a bare event or a list of bare events"); next }
        if (ev[i] == "push") { push = 1; pbranch = 1 }
        if (ev[i] == "schedule") sched = 1
      }
      next
    }
    inon && /^[[:space:]]*(#.*)?$/ { next }
    inon && /^-/ { layout(NR, "on: entries at column 0"); inon = 0; next }
    inon && /^[^ ]/ {
      if (!onentries) layout(NR - 1, "on: with no entry read")
      inon = 0; trig = ""
    }
    inon && /^  - *[a-z_]+[[:space:]]*(#.*)?$/ {
      v = $0; sub(/^  - */, "", v); sub(/[[:space:]#].*$/, "", v)
      if (v == "push") { push = 1; pbranch = 1 }
      if (v == "schedule") sched = 1
      onentries++; trig = ""
      next
    }
    inon && /^  [a-z_]+:/ {
      trig = $0; sub(/^  /, "", trig); sub(/:.*/, "", trig)
      if (trig == "push") push = 1
      if (trig == "schedule") sched = 1
      onentries++
      next
    }
    # The settings of a trigger (4+ spaces) are consumed; for push, the branch
    # and tag filters are read at 4 spaces. A filter at another depth counts as
    # a branch push (a false positive at worst, never a missed gate).
    inon && trig == "push" && /^    (branches|branches-ignore):/ { pbranch = 1 }
    inon && trig == "push" && /^    (tags|tags-ignore):/ { ptags = 1 }
    inon && trig != "" && /^    / { next }
    inon { layout(NR, "unparsed line under on:"); inon = 0; next }

    /^jobs:/ {
      v = $0; sub(/^jobs:[[:space:]]*/, "", v); sub(/[[:space:]]*#.*$/, "", v)
      if (v != "") { layout(NR, "inline jobs: value"); next }
      injobs = 1; jobsline = NR; next
    }
    injobs && /^[^ #]/ { flush(); injobs = 0 }
    injobs && /^  [A-Za-z0-9_-]+:[[:space:]]*(#.*)?$/ {
      flush()
      job = $0; sub(/^  /, "", job); sub(/:.*/, "", job)
      jobline = NR; hasif = 0; ifval = ""; njobs++
      next
    }
    # Anything else that is not deeper job content: a quoted, 4-space or
    # otherwise unparsed job key. Job content (4+ spaces) is only valid once a
    # job key has been read.
    injobs && /^[[:space:]]+[^[:space:]#]/ && ($0 !~ /^    / || job == "") {
      layout(NR, "job key not an unquoted 2-space key"); injobs = 0; job = ""; next
    }
    injobs && job != "" && !hasif && /^    if:/ {
      hasif = 1; ifline = NR
      ifval = $0; sub(/^    if:[[:space:]]*/, "", ifval); sub(/[[:space:]]+$/, "", ifval)
      # A trailing comment (no quotes in it) is not part of the expression.
      sub(/[[:space:]]+#[^\047"]*$/, "", ifval)
    }
    END {
      flush()
      if (inon && !onentries) layout(NR, "on: with no entry read")
      # Every workflow has both keys; a missing one means it was not recognised
      # (e.g. "jobs": quoted, or jobs : with a space).
      if (!onseen) layout(1, "no top-level on: key recognised")
      if (!jobsline) layout(1, "no top-level jobs: key recognised")
      if (jobsline && !njobs && !nlerr) layout(jobsline, "jobs: with no job parsed")
      # Layout errors fail closed whatever the triggers are: the triggers may
      # be what could not be read.
      for (i = 1; i <= nlerr; i++) print lerrs[i]
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
  # injected job without an if: is reported. Grep for that one error, as
  # layout errors are printed whatever the triggers are.
  # The probe lives in PROBE_DIR, which the EXIT/INT/TERM trap removes.
  # A failure here exits 2 (gated runs in the main shell): treating it as
  # "out of scope" would be a silent pass.
  local tmp="$PROBE_DIR/probe.yml"
  local o
  { awk '/^jobs:/ { exit } { print }' "$f" >"$tmp" &&
    printf 'jobs:\n  probe:\n    runs-on: x\n' >>"$tmp" &&
    o=$(check_file "$tmp"); } || {
    echo "check-fork-ci-gate: cannot check $f (scope probe failed)" >&2
    exit 2
  }
  grep -qF 'job probe has no if:' <<<"$o"
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
  # A file that cannot be checked (not a regular readable file, or check_file
  # failed: awk could not read it, or its subshell was killed) is exit 2, never
  # a pass: set -e is suspended under "if check_dir", so the status of each
  # check_file must be tested here.
  local o rc
  for f in "${files[@]}"; do
    rc=0
    if [ -f "$f" ] && [ -r "$f" ]; then
      o=$(check_file "$f") || rc=$?
    else
      rc=1
    fi
    if [ "$rc" -ne 0 ]; then
      [ -z "$out" ] || printf '%s' "$out"
      echo "check-fork-ci-gate: cannot check $f (not a readable regular file, or the check failed with rc=$rc)" >&2
      return 2
    fi
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
  # A workflow that cannot be checked must fail with rc 2 and no "ok" line.
  # Built at run time in PROBE_DIR (git cannot store a mode-000 file): an
  # unreadable copy of a good fixture, and a directory named like a workflow.
  # The whole script is run, so the final summary is covered too.
  local cd_unr="$PROBE_DIR/st-unreadable" cd_dir="$PROBE_DIR/st-not-a-file" case_dir
  mkdir -p "$cd_unr" "$cd_dir/ci.yml"
  cp "$td/good/push.yml" "$cd_unr/ci.yml"
  chmod 000 "$cd_unr/ci.yml"
  for case_dir in "$cd_unr" "$cd_dir"; do
    if [ "$case_dir" = "$cd_unr" ] && [ -r "$cd_unr/ci.yml" ]; then
      echo "selftest skipped: unreadable-file case (mode 000 is still readable here, e.g. as root)"
      continue
    fi
    rc=0; out=$(bash "${BASH_SOURCE[0]}" "$case_dir" 2>&1) || rc=$?
    if [ "$rc" -eq 2 ] && grep -qF 'cannot check' <<<"$out" && ! grep -qF ': ok (' <<<"$out"; then
      echo "selftest ok: ${case_dir##*/} fails with rc 2: cannot check"
    else
      echo "selftest FAILED: ${case_dir##*/} (exit $rc) did not fail with rc 2 and 'cannot check':"; printf '%s\n' "$out"; fail=1
    fi
  done
  chmod 600 "$cd_unr/ci.yml"
  # A readable workflow whose check dies (awk killed by SIGKILL, through an
  # awk shim first on PATH) must also be rc 2, not a silent pass. SHIM_MODE
  # picks the awk call that dies: "check" kills check_file's read of the
  # workflow (the call with "file=" in its arguments), pinning check_dir's
  # status test; "probe" kills the copy gated() makes of it, pinning the
  # scope probe's exit 2.
  local shim="$PROBE_DIR/st-shim" cd_kill="$PROBE_DIR/st-killed-check" real_awk mode
  real_awk=$(command -v awk)
  mkdir -p "$shim" "$cd_kill"
  cp "$td/good/push.yml" "$cd_kill/victim.yml"
  # The shim source is written literally: its $ must not expand here.
  # shellcheck disable=SC2016
  {
    echo '#!/usr/bin/env bash'
    echo 'v=0; c=0'
    echo 'for a in "$@"; do case $a in *victim.yml) v=1 ;; esac; case $a in file=*) c=1 ;; esac; done'
    echo 'k=0'
    echo '[ "$v$SHIM_MODE$c" = 1check1 ] && k=1'
    echo '[ "$v$SHIM_MODE$c" = 1probe0 ] && k=1'
    echo '[ "$k" = 0 ] || kill -KILL $$'
    printf 'exec %q "$@"\n' "$real_awk"
  } >"$shim/awk"
  chmod 700 "$shim/awk"
  for mode in check probe; do
    rc=0; out=$(SHIM_MODE=$mode PATH="$shim:$PATH" bash "${BASH_SOURCE[0]}" "$cd_kill" 2>&1) || rc=$?
    if [ "$rc" -eq 2 ] && grep -qF 'cannot check' <<<"$out" && ! grep -qF ': ok (' <<<"$out"; then
      echo "selftest ok: ${cd_kill##*/} ($mode killed) fails with rc 2: cannot check"
    else
      echo "selftest FAILED: ${cd_kill##*/} ($mode killed, exit $rc) did not fail with rc 2 and 'cannot check':"; printf '%s\n' "$out"; fail=1
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
  if [ "$rc" -ne 1 ]; then
    echo "check-fork-ci-gate: FAILED (rc=$rc): no gate result; see the error above" >&2
    exit "$rc"
  fi
  echo "check-fork-ci-gate: FAILED; every job in a push- or schedule-triggered workflow needs:" >&2
  echo "    if: \${{ $GATE }}" >&2
  echo "  or, with the job's own condition:" >&2
  echo "    if: \${{ ($GATE) && (<condition>) }}" >&2
  echo "  on the if: line itself, with <condition> one balanced operand (no || outside it)." >&2
  exit "$rc"
fi
