# shellcheck shell=bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# lib.sh holds the helpers shard.sh and hubshard.sh share. Source it from
# bash; it defines functions only.
#
# HUB_SHARD_DEADLINE (optional) is a Unix time in seconds. When it is set,
# step runs every command under timeout(1) with the time left until 60
# seconds before the deadline, and fails at once if none is left. Unset
# (the default, and in CI) runs every command as is.

# step runs "$@", under timeout(1) when HUB_SHARD_DEADLINE is set.
step() {
	if [ -z "${HUB_SHARD_DEADLINE:-}" ]; then
		"$@"
		return
	fi
	case "$HUB_SHARD_DEADLINE" in
	*[!0-9]*)
		echo "ERROR: HUB_SHARD_DEADLINE must be a Unix time in seconds: $HUB_SHARD_DEADLINE" >&2
		return 2
		;;
	esac
	local left=$((HUB_SHARD_DEADLINE - $(date +%s) - 60))
	if [ "$left" -le 0 ]; then
		echo "ERROR: HUB_SHARD_DEADLINE leaves no time for: $*" >&2
		return 124
	fi
	timeout "${left}s" "$@"
}

# build_hubshard ROOT OUT GOGC builds ROOT/hack/hubshard into the file OUT
# (go run ./hack/hubshard would build it again on every call) with GOGC.
build_hubshard() {
	local root=$1 out=$2 gogc=$3
	(cd "$root" && step env GOGC="$gogc" go build -buildvcs=false -o "$out" ./hack/hubshard)
}

# run_skip_summary CMD... runs the test binary command CMD with -test.v
# appended and prints its stdout rebuilt as without -test.v, then a
# summary of the skipped top-level tests. It returns CMD's exit status:
# pass/fail and the exit status are unchanged; stdout is rebuilt from
# -test.v and matches the non-verbose output except in the cases below.
#
# Only stdout passes through the filter (stderr is untouched). The filter
# drops the lines -test.v adds: the === RUN/PAUSE/CONT/NAME lines, the
# --- PASS and --- SKIP lines, and the t.Log lines of tests that did not
# fail. A failed test's block is held until its result lines end, then
# printed as without -test.v: its own t.Log lines, indented to its
# --- FAIL line, with each failed subtest's block where that subtest
# started (a subtest that paused for t.Parallel ends after its parent's
# lines, so its block follows them, in result order). Other lines (the
# test's direct prints, PASS, FAIL, example output) pass through as is.
#
# Limits (the non-verbose output can differ; pass/fail and the exit
# status never do):
#   - A print without a trailing newline glues the next -test.v line to
#     it, so a verbose line (--- PASS, a passing test's t.Log line or an
#     === RUN line) is printed after the partial line, and a glued
#     === RUN is missed, so the next lines count for the previous test.
#   - A direct print shaped like a t.Log line ("    name.go:N: ...") is
#     dropped in a test that does not fail and moved into the test's
#     --- FAIL block in one that fails; so is an 8-space indented direct
#     print right after a t.Log line.
#
# The summary lists, sorted, one line per top-level test that skipped or
# has a skipped subtest, with the skip message and its file:line, then
# the count; with no skip it prints one line saying so. The message is
# the test's last t.Log line, used only when nothing else of that test
# was printed between it and the skip (t.Skip logs its message just
# before it skips); otherwise it reads "(no reason)". So a t.Log line
# directly followed by t.SkipNow() reads as the reason.
#
# If awk fails, cat passes on the rest of stdout unfiltered, so the
# binary never gets SIGPIPE from the filter.
run_skip_summary() {
	local rcfile rc
	rcfile=$(mktemp)
	{
		rc=0
		"$@" -test.v || rc=$?
		echo "$rc" >"$rcfile"
	} | {
		LC_ALL=C awk "$skip_summary_awk"
		cat
	}
	rc=$(cat "$rcfile")
	rm -f "$rcfile"
	return "$rc"
}

# shellcheck disable=SC2016 # an awk program, not shell expansions
skip_summary_awk='
# reason returns a log line ("    file.go:N: msg") as "msg (file.go:N)".
function reason(line,    loc, msg) {
	sub(/^    /, "", line)
	if (!match(line, /^[^ ]+:[0-9]+:/)) return line
	loc = substr(line, 1, RLENGTH - 1)
	msg = substr(line, RLENGTH + 1)
	sub(/^ /, "", msg)
	return msg == "" ? "(" loc ")" : msg " (" loc ")"
}
function emit(line) { print line; fflush() }
# parent returns the nearest started test that name is a subtest of.
function parent(name) {
	while (match(name, /\/[^\/]*$/)) {
		name = substr(name, 1, RSTART - 1)
		if (name in seen) return name
	}
	return ""
}
# held appends an item to test name: a log line, or "\001" child at the
# point where subtest child started. The marker of a child goes to every
# started test its name extends (for t.Run("a/b") next to t.Run("a"),
# both the parent and its subtest a); expand uses the one that is the
# parent of the child in the result lines.
function held(name, item) { items[name, ++nitems[name]] = item }
# expand prints the failed result line i of the block, the held items of
# its test, and its failed subtests at their start markers; paused (or
# unmarked) failed subtests follow, in result order. bp[j] is the parent
# of result line j.
function expand(i,    name, ind, k, item, c, j) {
	emit(bl[i])
	name = bn[i]
	ind = bind[i]
	done[name] = 1
	for (k = 1; k <= nitems[name]; k++) {
		item = items[name, k]
		if (substr(item, 1, 1) != "\001") {
			emit(ind item)
			continue
		}
		c = substr(item, 2)
		if (!(c in failed) || (c in paused) || (c in done)) continue
		if (bp[failed[c]] == name) expand(failed[c])
	}
	for (j = 1; j <= nb; j++)
		if (bp[j] == name && bs[j] == "FAIL" && !(bn[j] in done)) expand(j)
}
# finish prints the held block of result lines and forgets its tests.
# The result lines are in pre-order, indented 4 spaces per level, so
# the parent of a line is the nearest line above it one level up.
function finish(    i, k, name, d) {
	for (i = 1; i <= nb; i++) {
		d = length(bind[i]) / 4
		level[d] = i
		bp[i] = (d > 0 && ((d - 1) in level)) ? bn[level[d - 1]] : ""
		if (bs[i] == "FAIL") failed[bn[i]] = i
	}
	for (d in level) delete level[d]
	for (i = 1; i <= nb; i++)
		if (bs[i] == "FAIL" && !(bp[i] in failed) && !(bn[i] in done)) expand(i)
	for (i = 1; i <= nb; i++) {
		name = bn[i]
		for (k = 1; k <= nitems[name]; k++) delete items[name, k]
		delete nitems[name]
		delete last[name]
		delete seen[name]
		delete paused[name]
		delete failed[name]
		delete done[name]
	}
	nb = 0
}
nb && !/^ +--- (PASS|FAIL|SKIP): / { finish() }
/^=== (RUN|PAUSE|CONT|NAME) / {
	cur = $3
	inlog = 0
	if ($2 == "RUN") {
		seen[cur] = 1
		p = parent(cur)
		# A subtest start clears the reason of every test it may be under.
		for (q = p; q != ""; q = parent(q)) delete last[q]
		for (; p != ""; p = parent(p)) held(p, "\001" cur)
	} else if ($2 == "PAUSE") {
		paused[cur] = 1
	}
	next
}
/^ *--- (PASS|FAIL|SKIP): / {
	name = $3
	ind = substr($0, 1, match($0, /[^ ]/) - 1)
	bl[++nb] = $0
	bn[nb] = name
	bs[nb] = substr($2, 1, 4)
	bind[nb] = ind
	if ($2 == "SKIP:") {
		why = (name in last) ? reason(last[name]) : "(no reason)"
		top = name
		sub(/\/.*/, "", top)
		tops[top] = 1
		if (name == top) {
			own[top] = why
		} else {
			subs[top]++
			if (!(top in first) || name < first[top]) {
				first[top] = name
				firstwhy[top] = why
			}
		}
	}
	if (ind == "") cur = ""
	inlog = 0
	next
}
cur != "" && /^    [^ ]+:[0-9]+:( |$)/ {
	held(cur, $0)
	last[cur] = $0
	inlog = 1
	next
}
cur != "" && inlog && /^        / { held(cur, $0); next }
{
	inlog = 0
	if (cur != "") delete last[cur]
	emit($0)
}
END {
	if (nb) finish()
	count = 0
	for (t in tops) count++
	if (count == 0) {
		emit("== skipped tests: none")
		exit
	}
	emit("== skipped tests: " count)
	sorter = "LC_ALL=C sort"
	for (t in tops) {
		if (t in own) {
			line = t ": " own[t]
		} else {
			line = t ": subtest " first[t] ": " firstwhy[t]
			if (subs[t] > 1) line = line " (and " subs[t] - 1 " more skipped subtests)"
		}
		print "  " line | sorter
	}
	close(sorter)
}
'
