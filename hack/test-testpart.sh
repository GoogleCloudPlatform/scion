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

# Tests for hack/testpart.sh (make testpart-selftest). Bash only, no go.

set -euo pipefail

cd "$(dirname "$0")/.."
tp=./hack/testpart.sh

fails=0
fail() {
	echo "FAIL: $*" >&2
	fails=$((fails + 1))
}

# go test -list style input: unsorted, a duplicate, a benchmark, TestMain
# output and go test's trailing ok line.
input='TestC
TestA
some TestMain output
ExampleE
TestB
BenchmarkX
FuzzD
TestA
ok  	example.com/pkg	0.012s'

# Sorted: ExampleE FuzzD TestA TestB TestC.
got=$(echo "$input" | $tp list 1 2 | paste -sd ' ' -)
[ "$got" = "ExampleE TestA TestC" ] || fail "list 1 2: got '$got'"
got=$(echo "$input" | $tp list 2 2 | paste -sd ' ' -)
[ "$got" = "FuzzD TestB" ] || fail "list 2 2: got '$got'"
got=$(echo "$input" | $tp list 1 1 | paste -sd ' ' -)
[ "$got" = "ExampleE FuzzD TestA TestB TestC" ] || fail "list 1 1: got '$got'"

got=$(echo "$input" | $tp regex 2 2)
[ "$got" = '^(FuzzD|TestB)$' ] || fail "regex 2 2: got '$got'"

# The regex selects exactly the part's names, not prefixes of them.
re=$(printf 'TestA\nTestAB\nTestB\n' | $tp regex 1 2)
echo TestAB | grep -qE "$re" && fail "regex '$re' matches TestAB, which is in part 2"
echo TestA | grep -qE "$re" || fail "regex '$re' does not match TestA"

for n in 1 2 3 5; do
	echo "$input" | $tp parity "$n" >/dev/null || fail "parity $n failed"
done

# The union of the parts is the input, each name once, for a larger list.
seq -f 'TestN%04g' 1 1271 >/tmp/testpart-selftest.$$
for n in 2 3 4; do
	union=$(for k in $(seq 1 "$n"); do $tp list "$k" "$n" </tmp/testpart-selftest.$$; done | LC_ALL=C sort)
	[ "$union" = "$(LC_ALL=C sort /tmp/testpart-selftest.$$)" ] || fail "union of $n parts differs from the input"
	$tp parity "$n" </tmp/testpart-selftest.$$ >/dev/null || fail "parity $n on 1271 names failed"
done
sizes=$(for k in 1 2 3; do $tp list "$k" 3 </tmp/testpart-selftest.$$ | wc -l | tr -d ' '; done | paste -sd ' ' -)
[ "$sizes" = "424 424 423" ] || fail "part sizes of 1271 names in 3 parts: got '$sizes'"
rm -f /tmp/testpart-selftest.$$

# Errors: more parts than names, bad K or N, empty input.
echo "$input" | $tp parity 6 >/dev/null 2>&1 && fail "parity 6 on 5 names passed"
echo "$input" | $tp list 6 6 >/dev/null 2>&1 && fail "list 6 6 on 5 names passed"
echo "$input" | $tp list 3 2 >/dev/null 2>&1 && fail "list 3 2 passed"
echo "$input" | $tp list 0 2 >/dev/null 2>&1 && fail "list 0 2 passed"
echo "$input" | $tp list 1 x >/dev/null 2>&1 && fail "list 1 x passed"
echo "$input" | $tp parity 0 >/dev/null 2>&1 && fail "parity 0 passed"
$tp parity 2 </dev/null >/dev/null 2>&1 && fail "parity on empty input passed"

if [ "$fails" -ne 0 ]; then
	echo "testpart selftest: $fails failure(s)" >&2
	exit 1
fi
echo "testpart selftest: ok"
