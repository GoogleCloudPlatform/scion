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

# End-to-end selftest for hack/hubshard/shard.sh -run handling (make
# hubshard-shard-run-selftest). Builds a tiny self-contained temporary module
# with a minimal pkg/hub package and copies hack/hubshard into it, so the real
# shard.sh, hubshard binary, overlay, and go test -c run in a couple of seconds.

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
unset HUB_SHARD_DEADLINE

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

repo="$tmp/repo"
mkdir -p "$repo/hack/hubshard" "$repo/pkg/hub"

cat >"$repo/go.mod" <<'EOF'
module example.com/hubshardtest

go 1.21
EOF

for f in "$root"/hack/hubshard/*.go; do
	case "$f" in
	*_test.go) continue ;;
	esac
	cp "$f" "$repo/hack/hubshard/"
done
cp "$root/hack/hubshard/shard.sh" "$root/hack/hubshard/lib.sh" "$root/hack/hubshard/hubshard.sh" "$repo/hack/hubshard/"
chmod +x "$repo/hack/hubshard/shard.sh" "$repo/hack/hubshard/hubshard.sh"

cat >"$repo/pkg/hub/doc.go" <<'EOF'
package hub
EOF

cat >"$repo/pkg/hub/common_helpers_test.go" <<'EOF'
package hub

import "testing"

func sharedHelper() {}

func TestCommonHelper(t *testing.T) {
	sharedHelper()
}
EOF

# Create 10 independent test files so tests land across shards 1..10.
for i in 01 02 03 04 05 06 07 08 09 10; do
	cat >"$repo/pkg/hub/f${i}_test.go" <<EOF
package hub

import (
	"os"
	"testing"
)

func TestShardFile${i}(t *testing.T) {
	sharedHelper()
	t.Run("sub", func(t *testing.T) {})
}

func TestShardFile${i}Other(t *testing.T) {
	sharedHelper()
	if os.Getenv("HUBSHARD_FAIL_OTHER") == "1" {
		t.Fatal("other test must not run when -run excludes it")
	}
}
EOF
done

cat >"$repo/pkg/hub/sqlite_only_test.go" <<'EOF'
//go:build !no_sqlite

package hub

import "testing"

func TestSQLiteOnly(t *testing.T) {
	sharedHelper()
}
EOF

fails=0
fail() {
	echo "FAIL: $*" >&2
	fails=$((fails + 1))
}

assert_contains() {
	local name=$1 file=$2 needle=$3
	if ! grep -Fq -- "$needle" "$file"; then
		fail "$name: expected $(basename "$file") to contain '$needle'"
		cat "$file" >&2
	fi
}

assert_not_contains() {
	local name=$1 file=$2 needle=$3
	if grep -Fq -- "$needle" "$file"; then
		fail "$name: did not expect $(basename "$file") to contain '$needle'"
		cat "$file" >&2
	fi
}

# Locate tests dynamically so the selftest does not hardcode shard numbers.
(cd "$repo" && ./hack/hubshard/hubshard.sh -locate \
	TestShardFile01 TestShardFile02 TestShardFile03 TestShardFile04 TestShardFile05 \
	TestShardFile06 TestShardFile07 TestShardFile08 TestShardFile09 TestShardFile10 \
	TestCommonHelper TestSQLiteOnly) >"$tmp/located"

other_test=$(awk '$2 != 1 && $1 ~ /^TestShardFile/ {print $1; exit}' "$tmp/located")
other_shard=$(awk -v t="$other_test" '$1 == t {print $2; exit}' "$tmp/located")
sqlite_shard=$(awk '$1 == "TestSQLiteOnly" {print $2; exit}' "$tmp/located")

if [ -z "$other_test" ] || [ -z "$other_shard" ] || [ -z "$sqlite_shard" ]; then
	echo "FATAL: failed to locate test shards:" >&2
	cat "$tmp/located" >&2
	exit 1
fi

run_shard() {
	local out=$1 err=$2
	shift 2
	local rc=0
	(cd "$repo" && ./hack/hubshard/shard.sh "$@") >"$out" 2>"$err" || rc=$?
	return "$rc"
}

# 1. Wrong-shard -run fails non-zero before compiling and names the right shard.
rc=0
run_shard "$tmp/out1" "$tmp/err1" test 1 -run "$other_test" || rc=$?
[ "$rc" -ne 0 ] || fail "wrong-shard -run: expected non-zero exit, got 0"
assert_contains "wrong-shard -run" "$tmp/err1" "matches no tests in pkg/hub shard 1"
assert_contains "wrong-shard -run" "$tmp/err1" "shard(s): $other_shard"
assert_contains "wrong-shard -run" "$tmp/err1" "$other_test (shard $other_shard)"
assert_not_contains "wrong-shard -run" "$tmp/out1" "== compile pkg/hub"

# 2. Common-file test requested in shard 2 fails non-zero and points to shard 1.
rc=0
run_shard "$tmp/out2" "$tmp/err2" test 2 -run '^TestCommonHelper$' || rc=$?
[ "$rc" -ne 0 ] || fail "common-test wrong shard: expected non-zero exit, got 0"
assert_contains "common-test wrong shard" "$tmp/err2" "shard(s): 1"
assert_contains "common-test wrong shard" "$tmp/err2" "TestCommonHelper (shard 1)"

# 3. Subtest pattern in wrong shard fails non-zero and points to the right shard.
rc=0
run_shard "$tmp/out3" "$tmp/err3" test 1 "--run=^${other_test}\$/sub" || rc=$?
[ "$rc" -ne 0 ] || fail "subtest wrong shard: expected non-zero exit, got 0"
assert_contains "subtest wrong shard" "$tmp/err3" "shard(s): $other_shard"

# 4. Matching-shard -run succeeds and passes -test.run to the binary (so the
# sibling *Other test that fails under HUBSHARD_FAIL_OTHER=1 does not run).
rc=0
HUBSHARD_FAIL_OTHER=1 run_shard "$tmp/out4" "$tmp/err4" test "$other_shard" -run "^${other_test}\$" || rc=$?
[ "$rc" -eq 0 ] || fail "matching-shard -run: expected exit 0, got $rc (stderr: $(cat "$tmp/err4"))"
assert_contains "matching-shard -run" "$tmp/out4" "ok  	pkg/hub shard $other_shard"
assert_not_contains "matching-shard -run" "$tmp/out4" "no tests to run"
assert_not_contains "matching-shard -run" "$tmp/err4" "also matches"

# 5. Normal shard run without -run behaves unchanged.
rc=0
run_shard "$tmp/out5" "$tmp/err5" test 1 || rc=$?
[ "$rc" -eq 0 ] || fail "normal shard run: expected exit 0, got $rc (stderr: $(cat "$tmp/err5"))"
assert_contains "normal shard run" "$tmp/out5" "ok  	pkg/hub shard 1"
assert_contains "normal shard run" "$tmp/out5" "== skipped tests: none"

# 6. Non-existent -run pattern fails non-zero cleanly.
rc=0
run_shard "$tmp/out6" "$tmp/err6" test 1 -run TestDoesNotExistAnywhere || rc=$?
[ "$rc" -ne 0 ] || fail "non-existent -run: expected non-zero exit, got 0"
assert_contains "non-existent -run" "$tmp/err6" "matches no tests in any pkg/hub shard (tags: default)"

# 7. --nosqlite excludes !no_sqlite tests and reports tags: no_sqlite.
rc=0
run_shard "$tmp/out7" "$tmp/err7" test "$sqlite_shard" --nosqlite -run TestSQLiteOnly || rc=$?
[ "$rc" -ne 0 ] || fail "nosqlite excluded test: expected non-zero exit, got 0"
assert_contains "nosqlite excluded test" "$tmp/err7" "matches no tests in any pkg/hub shard (tags: no_sqlite)"

# 8. Invalid regex fails non-zero cleanly.
rc=0
run_shard "$tmp/out8" "$tmp/err8" test 1 -run '[invalid' || rc=$?
[ "$rc" -ne 0 ] || fail "invalid regex: expected non-zero exit, got 0"
assert_contains "invalid regex" "$tmp/err8" "invalid -run pattern"

# 9. -run is rejected by build.
rc=0
run_shard "$tmp/out9" "$tmp/err9" build 1 -run TestCommonHelper || rc=$?
[ "$rc" -ne 0 ] || fail "build -run: expected non-zero exit, got 0"
assert_contains "build -run" "$tmp/err9" "-run is only valid for test"

# 10. Pattern matching both this shard and other shards succeeds and prints a note.
rc=0
run_shard "$tmp/out10" "$tmp/err10" test "$other_shard" -run '^TestShardFile' || rc=$?
[ "$rc" -eq 0 ] || fail "multi-shard -run: expected exit 0, got $rc (stderr: $(cat "$tmp/err10"))"
assert_contains "multi-shard -run" "$tmp/out10" "ok  	pkg/hub shard $other_shard"
assert_contains "multi-shard -run" "$tmp/err10" "NOTE: -run '^TestShardFile' also matches"

# 11. Makefile test-hub-shard preserves literal $ and single quotes in RUN=,
# and omits -run when RUN is unset.
make -n -C "$root" test-hub-shard SHARD=1 RUN='^TestA$/sub|^TestB$' >"$tmp/make1" 2>&1
assert_contains "make RUN with dollar" "$tmp/make1" "-run '^TestA\$/sub|^TestB\$'"
make -n -C "$root" test-hub-shard SHARD=1 RUN="it's" >"$tmp/make2" 2>&1
assert_contains "make RUN with quote" "$tmp/make2" "-run 'it'\''s'"
env -u RUN make -n -C "$root" test-hub-shard SHARD=1 >"$tmp/make3" 2>&1
assert_not_contains "make without RUN" "$tmp/make3" "-run"

if [ "$fails" -ne 0 ]; then
	echo "hubshard shard -run selftest: $fails failure(s)" >&2
	exit 1
fi
echo "hubshard shard -run selftest: ok"
