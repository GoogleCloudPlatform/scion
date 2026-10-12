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

# testpart.sh splits a list of top-level test names into N parts in a
# deterministic way, so that N CI jobs can each run one part of one
# package's tests and every test runs in exactly one job.
#
# The names are read from stdin, one per line. This is usually the output
# of `go test -list '.*'`. Lines that are not Test, Fuzz or Example names
# are ignored, such as go test's trailing "ok" line or TestMain output.
# Benchmarks are left out because go test runs them only with -bench. The
# names are sorted (LC_ALL=C) and deduplicated. Name i (counting from 0)
# goes to part (i mod N) + 1, so the parts differ in size by at most one
# test, and the same input always gives the same parts.
#
# Usage:
#   testpart.sh list K N     print the names in part K of N
#   testpart.sh regex K N    print a go test -run pattern for part K of N:
#                            ^(A|B|...)$ (top-level names only, so every
#                            subtest of a selected test still runs)
#   testpart.sh parity N     check that parts 1..N are pairwise disjoint and
#                            that together they hold exactly the input names
#                            (prints a one-line summary; exit 1 on failure)

set -euo pipefail

usage() {
	sed -n 's/^# \{0,1\}//; /^Usage:/,$p' "${BASH_SOURCE[0]}" | sed '/^set -euo/,$d' >&2
	exit 2
}

die() {
	echo "ERROR: $*" >&2
	exit 1
}

is_count() {
	case "$1" in
	'' | *[!0-9]* | 0*) return 1 ;;
	esac
}

# names reads stdin and prints the sorted, deduplicated test names.
names() {
	{ grep -E '^(Test|Fuzz|Example)[^[:space:]]*$' || true; } | LC_ALL=C sort -u
}

# part prints part $1 of $2 from the sorted names on stdin.
part() {
	awk -v k="$1" -v n="$2" '(NR - 1) % n == k - 1'
}

[ $# -ge 2 ] || usage
cmd=$1
shift

case "$cmd" in
list | regex)
	[ $# -eq 2 ] || usage
	k=$1 n=$2
	is_count "$n" || die "N must be a positive number: $n"
	is_count "$k" && [ "$k" -le "$n" ] || die "K must be a number from 1 to $n: $k"
	selected=$(names | part "$k" "$n")
	[ -n "$selected" ] || die "part $k of $n is empty (fewer test names than parts?)"
	if [ "$cmd" = list ]; then
		echo "$selected"
	else
		echo "^($(echo "$selected" | paste -sd '|' -))\$"
	fi
	;;

parity)
	[ $# -eq 1 ] || usage
	n=$1
	is_count "$n" || die "N must be a positive number: $n"
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	names >"$tmp/all"
	total=$(wc -l <"$tmp/all" | tr -d ' ')
	[ "$total" -gt 0 ] || die "no test names on stdin"
	: >"$tmp/union"
	sizes=
	for k in $(seq 1 "$n"); do
		part "$k" "$n" <"$tmp/all" >"$tmp/part-$k"
		[ -s "$tmp/part-$k" ] || die "part $k of $n is empty ($total test names)"
		cat "$tmp/part-$k" >>"$tmp/union"
		sizes="$sizes${sizes:+ + }$(wc -l <"$tmp/part-$k" | tr -d ' ')"
	done
	dups=$(LC_ALL=C sort "$tmp/union" | uniq -d)
	[ -z "$dups" ] || die "these tests are in more than one part: $(echo "$dups" | tr '\n' ' ')"
	if ! diff -u "$tmp/all" <(LC_ALL=C sort "$tmp/union") >"$tmp/diff"; then
		cat "$tmp/diff" >&2
		die "the union of the $n parts differs from the test list (- listed, + in the parts)"
	fi
	echo "testpart parity: $n parts ($sizes) hold all $total tests, each exactly once"
	;;

*)
	usage
	;;
esac
