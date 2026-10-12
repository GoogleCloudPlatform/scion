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

# shard.sh builds, tests and vets one pkg/hub test shard (see README.md).
# The make targets test-hub-shard, build-hub-shard, vet-hub-shard and the
# pkg/hub Postgres targets call it; CI runs those targets.
#
# Every command first writes the shard overlay into a temporary directory
# (go run ./hack/hubshard -overlay, which also prints the shard sizes) and
# compiles with go -overlay. The working tree is never modified, and the
# temporary directory is removed on exit.
#
# Usage:
#   shard.sh test K [--nosqlite] [--race]
#       Compile shard K (go test -c), check that the binary's test list is
#       exactly what hubshard -list K expects, then run that binary (no
#       second compile), skipping the common files' tests outside shard 1.
#       Running the binary skips the vet checks go test runs before a test
#       (go test's built-in vet pass). CI vets every shard fully (make
#       vet-hub-shard; make hubshard-parity checks those vets cover every
#       pkg/hub test file); local make test-hub-shard(s) runs and the
#       nightly Full Test Suite and Race Detection loops do not vet.
#   shard.sh build K [--nosqlite]
#       Compile shard K only (binary discarded).
#   shard.sh vet K
#       go vet shard K with -tags no_sqlite and with -tags 'integration
#       volume_test tzcontract': together, the shards' vets cover every
#       file the module-wide vets (make lint, make vet-integration) cover.
#   shard.sh postgres K|all TAGS TEST...
#       Run the named tests (the shard binary with -test.v and -test.run)
#       in the shard that holds each (in every shard holding one for
#       "all"), with build tags TAGS ("" or for example integration). Fails if a name is in no test file, if
#       any selected test skips, or if a named test prints no PASS line.
#
# Environment:
#   HUB_SHARD_TIMEOUT  the test binary's -test.timeout for "test" (default 30m)
#   HUB_SHARD_P        go build -p for the compiles (default 1: one package
#                      at a time keeps the peak at the pkg/hub compile)
#   GOGC               for the compiles (default 25, as HUB_TEST_GOGC in the
#                      Makefile; the tests themselves run with Go's default)

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"

usage() {
	sed -n 's/^# \{0,1\}//; /^Usage:/,/^Environment:/p' "${BASH_SOURCE[0]}" | sed '$d' >&2
	exit 2
}

die() {
	echo "ERROR: $*" >&2
	exit 1
}

[ $# -ge 2 ] || usage
cmd=$1
shard=$2
shift 2

case "$shard" in
all) [ "$cmd" = postgres ] || die "shard 'all' is only for postgres" ;;
'' | *[!0-9]*) die "shard must be a number (or 'all' for postgres): $shard" ;;
esac

timeout=${HUB_SHARD_TIMEOUT:-30m}
par=${HUB_SHARD_P:-1}
build_gogc=${GOGC:-25}

goroot_bin="$(go env GOROOT)/bin"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

hubshard() {
	go run ./hack/hubshard "$@"
}

echo "== pkg/hub shard overlay"
hubshard -overlay "$tmp/overlay" >&2
overlay="$tmp/overlay/overlay.json"

# with_tags joins the extra tags and the shard tags with commas.
with_tags() {
	local k=$1 extra=$2
	if [ -n "$extra" ]; then
		echo "$extra,hubshard,hubshard_$k"
	else
		echo "hubshard,hubshard_$k"
	fi
}

nosqlite=
race=
parse_flags() {
	while [ $# -gt 0 ]; do
		case "$1" in
		--nosqlite) nosqlite=no_sqlite ;;
		--race) race=-race ;;
		*) die "unknown flag: $1" ;;
		esac
		shift
	done
}

compile() {
	local k=$1 extra=$2 out=$3
	echo "== compile pkg/hub shard $k (tags: $(with_tags "$k" "$extra"))"
	mkdir -p "$out"
	GOGC=$build_gogc go test -c -p "$par" ${race:+"$race"} -overlay "$overlay" \
		-tags "$(with_tags "$k" "$extra")" -o "$out/" ./pkg/hub
}

# run_binary runs a compiled shard test binary the way go test runs it:
# from the package directory, with $(go env GOROOT)/bin first on PATH (so
# a test that execs go gets the same toolchain) and -test.paniconexit0
# (go test always passes both). The tests run with the caller's GOGC (Go's
# default unless set). Compiling once and running the binary avoids a
# second compile and link of the shard (go test would relink it). The exit status is the binary's: non-zero on a
# failed test, a panic or -test.timeout. Prints an ok/FAIL line like go
# test's.
run_binary() {
	local bin=$1 label=$2 start rc
	shift 2
	start=$(date +%s)
	rc=0
	(cd pkg/hub && PATH="$goroot_bin:$PATH" "$bin" -test.paniconexit0 "$@") || rc=$?
	if [ "$rc" -eq 0 ]; then
		echo "ok  	$label	$(($(date +%s) - start))s"
	else
		echo "FAIL	$label	$(($(date +%s) - start))s (exit $rc)"
	fi
	return "$rc"
}

case "$cmd" in
build)
	parse_flags "$@"
	compile "$shard" "$nosqlite" "$tmp/bin"
	;;

test)
	parse_flags "$@"
	compile "$shard" "$nosqlite" "$tmp/bin"

	# The binary must hold exactly the tests the tool expects: a file
	# that go/build placed differently from the tool, or a test that
	# exists only in the binary, fails here instead of silently going
	# missing. -test.list runs TestMain's setup but no test.
	echo "== check the shard $shard test list"
	# Only test names count: TestMain's helpers may print to stdout.
	(cd pkg/hub && "$tmp/bin/hub.test" -test.list '.*') |
		{ grep -E '^(Test|Benchmark|Fuzz|Example)[^[:space:]]*$' || true; } | LC_ALL=C sort >"$tmp/got"
	hubshard -tags "$nosqlite" -list "$shard" | LC_ALL=C sort >"$tmp/want"
	if ! diff -u "$tmp/want" "$tmp/got" >"$tmp/list.diff"; then
		cat "$tmp/list.diff" >&2
		die "the shard $shard binary's test list differs from hubshard -list $shard (- expected, + compiled)"
	fi
	echo "shard $shard compiles $(wc -l <"$tmp/got") tests, as expected"

	skip=$(hubshard -tags "$nosqlite" -skip "$shard")
	echo "== run pkg/hub shard $shard (the binary checked above)"
	run_binary "$tmp/bin/hub.test" "pkg/hub shard $shard (tags: $(with_tags "$shard" "$nosqlite"))" \
		-test.count=1 -test.timeout="$timeout" ${skip:+-test.skip="$skip"}
	;;

vet)
	[ $# -eq 0 ] || usage
	for extra in no_sqlite integration,volume_test,tzcontract; do
		echo "== go vet pkg/hub shard $shard (tags: $(with_tags "$shard" "$extra"))"
		GOGC=$build_gogc go vet -p "$par" -overlay "$overlay" -tags "$(with_tags "$shard" "$extra")" ./pkg/hub
	done
	;;

postgres)
	[ $# -ge 2 ] || usage
	tags=$1
	shift
	# Fails (set -e) if a name is in no test file compiled with $tags.
	hubshard -tags "$tags" -locate "$@" >"$tmp/located"
	if [ "$shard" = all ]; then
		shards=$(awk '{print $2}' "$tmp/located" | sort -nu)
	else
		shards=$shard
	fi
	ran=0
	for k in $shards; do
		names=$(awk -v k="$k" '$2 == k {print $1}' "$tmp/located")
		[ -n "$names" ] || continue
		ran=1
		regex="^($(echo "$names" | paste -sd '|' -))\$"
		log="$tmp/postgres-$k.log"
		echo "== Postgres tests in pkg/hub shard $k (tags: $(with_tags "$k" "$tags")): $(echo "$names" | tr '\n' ' ')"
		compile "$k" "$tags" "$tmp/bin-$k"
		# The selected tests are short; they run with the compile GOGC,
		# as they did when go test built and ran them in one command.
		GOGC=$build_gogc run_binary "$tmp/bin-$k/hub.test" "pkg/hub shard $k Postgres tests" \
			-test.count=1 -test.timeout=20m -test.v -test.run="$regex" 2>&1 | tee "$log"
		if grep -qE '^[[:space:]]*--- SKIP' "$log"; then
			die "one or more pkg/hub Postgres tests were skipped -- see '--- SKIP' lines above."
		fi
		for t in $names; do
			grep -qE "^[[:space:]]*--- PASS: $t( |\$)" "$log" || die "$t did not pass."
		done
	done
	if [ "$ran" = 0 ]; then
		echo "no listed Postgres test is in pkg/hub shard $shard"
	fi
	;;

*)
	usage
	;;
esac
