#!/usr/bin/env bash
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

# Tests for run_skip_summary in hack/hubshard/lib.sh (make
# hubshard-skip-summary-selftest). Bash only, no go: a fake test binary
# prints canned go test -test.v output. Each case checks that stdout is
# the output the binary prints without -test.v, followed by the skip
# summary, that stderr passes through untouched, and that the exit status
# is the binary's.

set -euo pipefail

cd "$(dirname "$0")/../.."
# shellcheck source=hack/hubshard/lib.sh
. ./hack/hubshard/lib.sh

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fails=0
fail() {
	echo "FAIL: $*" >&2
	fails=$((fails + 1))
}

# The fake binary prints $tmp/verbose.txt to stdout and $tmp/stderr.txt
# to stderr and exits with FAKE_RC. It fails unless its last argument is
# -test.v, which run_skip_summary appends.
fake="$tmp/fake.test"
cat >"$fake" <<EOF
#!/usr/bin/env bash
last=
for a in "\$@"; do last=\$a; done
if [ "\$last" != -test.v ]; then
	echo "fake.test: last argument is '\$last', not -test.v" >&2
	exit 99
fi
cat "$tmp/verbose.txt"
cat "$tmp/stderr.txt" >&2
exit "\${FAKE_RC:-0}"
EOF
chmod +x "$fake"

# check NAME RC: runs the fake binary under run_skip_summary and compares
# stdout with $tmp/want.txt, stderr with $tmp/stderr.txt and the exit
# status with RC.
check() {
	local name=$1 want_rc=$2 rc=0
	FAKE_RC=$want_rc run_skip_summary "$fake" -test.paniconexit0 -test.count=1 \
		>"$tmp/out" 2>"$tmp/err" || rc=$?
	[ "$rc" -eq "$want_rc" ] || fail "$name: exit status $rc, want $want_rc"
	if ! diff -u "$tmp/want.txt" "$tmp/out" >"$tmp/diff"; then
		fail "$name: stdout differs (- want, + got)"
		cat "$tmp/diff" >&2
	fi
	if ! diff -u "$tmp/stderr.txt" "$tmp/err" >"$tmp/diff"; then
		fail "$name: stderr differs (- want, + got)"
		cat "$tmp/diff" >&2
	fi
}

# 1. No skips: the passing tests' t.Log lines and the -test.v lines are
# dropped, a direct print and PASS stay.
: >"$tmp/stderr.txt"
cat >"$tmp/verbose.txt" <<'EOF'
=== RUN   TestA
    a_test.go:9: passing log
direct stdout from A
--- PASS: TestA (0.00s)
=== RUN   TestC
=== RUN   TestC/one
    c_test.go:12: ok sub
        continued
--- PASS: TestC (0.00s)
    --- PASS: TestC/one (0.00s)
PASS
EOF
cat >"$tmp/want.txt" <<'EOF'
direct stdout from A
PASS
== skipped tests: none
EOF
check "no skips" 0

# 2. Skips with reasons: a top-level skip, a skip without a message, and
# skips inside subtests, which count once for their top-level test. The
# summary is sorted, not in run order.
echo 'stderr line' >"$tmp/stderr.txt"
cat >"$tmp/verbose.txt" <<'EOF'
=== RUN   TestZed
    z_test.go:10: needs postgres
--- SKIP: TestZed (0.00s)
=== RUN   TestC
=== RUN   TestC/one
    c_test.go:12: ok sub
=== RUN   TestC/two
    c_test.go:13: no docker
        second line
--- PASS: TestC (0.00s)
    --- PASS: TestC/one (0.00s)
    --- SKIP: TestC/two (0.00s)
=== RUN   TestM
=== RUN   TestM/b
    m_test.go:20: skip b
=== RUN   TestM/a
    m_test.go:20: skip a
--- PASS: TestM (0.00s)
    --- SKIP: TestM/b (0.00s)
    --- SKIP: TestM/a (0.00s)
=== RUN   TestB
    b_test.go:5: some log first
    b_test.go:6:
--- SKIP: TestB (0.00s)
PASS
EOF
cat >"$tmp/want.txt" <<'EOF'
PASS
== skipped tests: 4
  TestB: (b_test.go:6)
  TestC: subtest TestC/two: no docker (c_test.go:13)
  TestM: subtest TestM/a: skip a (m_test.go:20) (and 1 more skipped subtests)
  TestZed: needs postgres (z_test.go:10)
EOF
check "skips with reasons" 0

# 3. A failing run: the exit status is the binary's, the failed tests'
# t.Log lines follow their --- FAIL lines indented as without -test.v,
# and the summary is still printed.
echo 'panic-ish stderr' >"$tmp/stderr.txt"
cat >"$tmp/verbose.txt" <<'EOF'
=== RUN   TestB
    b_test.go:10: needs postgres
--- SKIP: TestB (0.00s)
=== RUN   TestF
=== RUN   TestF/sub
    f_test.go:19: before
    f_test.go:19: boom
        more
--- FAIL: TestF (0.00s)
    --- FAIL: TestF/sub (0.00s)
=== RUN   TestG
    g_test.go:3: top-level failure
--- FAIL: TestG (0.00s)
FAIL
EOF
cat >"$tmp/want.txt" <<'EOF'
--- FAIL: TestF (0.00s)
    --- FAIL: TestF/sub (0.00s)
        f_test.go:19: before
        f_test.go:19: boom
            more
--- FAIL: TestG (0.00s)
    g_test.go:3: top-level failure
FAIL
== skipped tests: 1
  TestB: needs postgres (b_test.go:10)
EOF
check "failing run" 1

# 4. Parallel tests with interleaved output: lines are attributed by the
# === CONT and === NAME lines; one skips, one fails, one passes.
: >"$tmp/stderr.txt"
cat >"$tmp/verbose.txt" <<'EOF'
=== RUN   TestP1
=== PAUSE TestP1
=== RUN   TestP2
=== PAUSE TestP2
=== RUN   TestP3
=== PAUSE TestP3
=== CONT  TestP1
    p_test.go:1: p1 log
=== CONT  TestP2
    p_test.go:2: p2 log
=== CONT  TestP3
direct stdout from P3
=== NAME  TestP1
    p_test.go:1: p1 skipping
=== NAME  TestP2
    p_test.go:2: p2 broke
--- SKIP: TestP1 (0.00s)
=== NAME  TestP3
    p_test.go:3: p3 log
--- PASS: TestP3 (0.00s)
--- FAIL: TestP2 (0.00s)
FAIL
EOF
cat >"$tmp/want.txt" <<'EOF'
direct stdout from P3
--- FAIL: TestP2 (0.00s)
    p_test.go:2: p2 log
    p_test.go:2: p2 broke
FAIL
== skipped tests: 1
  TestP1: p1 skipping (p_test.go:1)
EOF
check "parallel" 1

# 5. A binary killed with no output (a timeout(1) exit, for example):
# the status is kept and the summary still printed.
: >"$tmp/verbose.txt"
: >"$tmp/stderr.txt"
echo '== skipped tests: none' >"$tmp/want.txt"
check "killed, no output" 124

if [ "$fails" -ne 0 ]; then
	echo "hubshard skip summary selftest: $fails failure(s)" >&2
	exit 1
fi
echo "hubshard skip summary selftest: ok"
