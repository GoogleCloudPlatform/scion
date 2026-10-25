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
# appended and prints its output as it would print without -test.v, then
# a summary of the skipped top-level tests. It returns CMD's exit status:
# the summary changes neither the result nor the lines printed before it.
#
# Only stdout passes through the filter (stderr is untouched). The filter
# drops the lines -test.v adds: the === RUN/PAUSE/CONT/NAME lines, the
# --- PASS and --- SKIP lines, and the t.Log lines of tests that did not
# fail. A failed test's t.Log lines follow its --- FAIL line, indented as
# without -test.v. Other lines (the test's direct prints, PASS, FAIL)
# pass through unchanged.
#
# The summary lists, sorted, one line per top-level test that skipped or
# has a skipped subtest, with the skip message and its file:line, then
# the count; with no skip it prints one line saying so.
run_skip_summary() {
	local rcfile rc
	rcfile=$(mktemp)
	{
		rc=0
		"$@" -test.v || rc=$?
		echo "$rc" >"$rcfile"
	} | awk "$skip_summary_awk"
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
/^=== (RUN|PAUSE|CONT|NAME) / { cur = $3; inlog = 0; next }
/^ *--- (PASS|FAIL|SKIP): / {
	name = $3
	ind = substr($0, 1, match($0, /[^ ]/) - 1)
	if ($2 == "FAIL:") {
		emit($0)
		if (name in buf) {
			n = split(buf[name], lines, "\n")
			for (i = 1; i <= n; i++) emit(ind lines[i])
		}
	} else if ($2 == "SKIP:") {
		why = (name in last) ? reason(last[name]) : ""
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
	delete buf[name]
	delete last[name]
	if (ind == "") cur = ""
	inlog = 0
	next
}
cur != "" && /^    [^ ]+:[0-9]+:( |$)/ {
	# Not buf[cur] = (cur in buf) ? ...: the assignment can create
	# buf[cur] before the test runs.
	held = (cur in buf) ? buf[cur] "\n" $0 : $0
	buf[cur] = held
	last[cur] = $0
	inlog = 1
	next
}
cur != "" && inlog && /^        / { buf[cur] = buf[cur] "\n" $0; next }
{ inlog = 0; emit($0) }
END {
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
			line = t (own[t] == "" ? "" : ": " own[t])
		} else {
			line = t ": subtest " first[t] (firstwhy[t] == "" ? "" : ": " firstwhy[t])
			if (subs[t] > 1) line = line " (and " subs[t] - 1 " more skipped subtests)"
		}
		print "  " line | sorter
	}
	close(sorter)
}
'
