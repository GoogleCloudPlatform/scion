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

# Tests for hack/golangci-lint-split.sh (make golangci-lint-split-selftest).
# Bash only: go and golangci-lint are stubs on PATH, so nothing is compiled
# or linted.

set -euo pipefail

cd "$(dirname "$0")/.."
root=$(pwd -P)
split="$root/hack/golangci-lint-split.sh"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fails=0
cases=0
fail() {
	echo "FAIL: $*" >&2
	fails=$((fails + 1))
}

# Stub go: "go list -e -f {{.Dir}} ./..." prints $STUB_DIRS (one per line)
# and exits $STUB_GO_RC.
mkdir "$tmp/bin"
cat >"$tmp/bin/go" <<'EOF'
#!/usr/bin/env bash
[ "$*" = "list -e -f {{.Dir}} ./..." ] || { echo "stub go: unexpected args: $*" >&2; exit 99; }
printf '%s' "$STUB_DIRS"
exit "${STUB_GO_RC:-0}"
EOF
# Stub golangci-lint: appends its arguments (one line per call) to
# $STUB_CALLS and exits with the Nth word of $STUB_RCS for call N.
cat >"$tmp/bin/golangci-lint" <<'EOF'
#!/usr/bin/env bash
echo "$*" >>"$STUB_CALLS"
n=$(wc -l <"$STUB_CALLS")
read -r -a rcs <<<"$STUB_RCS"
exit "${rcs[$((n - 1))]:-0}"
EOF
chmod +x "$tmp/bin/go" "$tmp/bin/golangci-lint"
# A PATH with the go stub but no golangci-lint at all: the stub go, plus the
# two programs the split script and the stub need from PATH (bash, for their
# #!/usr/bin/env bash lines, and dirname).
mkdir "$tmp/bin-nogl"
cp "$tmp/bin/go" "$tmp/bin-nogl/go"
ln -s "$(command -v bash)" "$tmp/bin-nogl/bash"
ln -s "$(command -v dirname)" "$tmp/bin-nogl/dirname"

default_dirs="$root
$root/cmd/scion
$root/pkg/hub
$root/pkg/hub/authzop
$root/pkg/hubclient
"

# run <name> <want rc> <STUB_RCS> [args...]; leaves the calls in $tmp/<name>.calls.
# GOLANGCI_LINT is unset unless GL is set, so the other cases use the script's
# default name (golangci-lint, found as the stub on PATH).
run() {
	local name=$1 want=$2 rcs=$3 rc=0
	shift 3
	cases=$((cases + 1))
	: >"$tmp/$name.calls"
	env -u GOLANGCI_LINT ${GL:+"GOLANGCI_LINT=$GL"} \
		PATH="${P:-$tmp/bin:$PATH}" STUB_DIRS="${DIRS-$default_dirs}" STUB_GO_RC="${GO_RC:-0}" \
		STUB_CALLS="$tmp/$name.calls" STUB_RCS="$rcs" \
		"$split" "$@" >"$tmp/$name.out" 2>&1 || rc=$?
	[ "$rc" -eq "$want" ] || fail "$name: exit $rc, want $want ($(cat "$tmp/$name.out"))"
}
calls() { cat "$tmp/$1.calls"; }
ncalls() { wc -l <"$tmp/$1.calls" | tr -d ' '; }

# 1. Both passes, exact package lists and flags: pass 1 gets every package but
#    ./pkg/hub (its sub-packages and the root package included), pass 2 only
#    ./pkg/hub with --tests=false.
run ok 0 "0 0" --new-from-merge-base=origin/main --concurrency=4
want='run --new-from-merge-base=origin/main --concurrency=4 . ./cmd/scion ./pkg/hub/authzop ./pkg/hubclient
run --new-from-merge-base=origin/main --concurrency=4 --tests=false ./pkg/hub'
[ "$(calls ok)" = "$want" ] || fail "ok: calls were:
$(calls ok)
want:
$want"
grep -qx 'golangci-lint-split: pass 1: 4 packages (all but ./pkg/hub), tests: true' "$tmp/ok.out" ||
	fail "ok: no pass 1 summary line"

# 2-4. Exit code: pass 1's if non-zero, else pass 2's; both passes always run.
run p1fail 1 "1 0"
[ "$(ncalls p1fail)" = 2 ] || fail "p1fail: $(ncalls p1fail) calls, want 2"
run p2fail 1 "0 1"
[ "$(ncalls p2fail)" = 2 ] || fail "p2fail: $(ncalls p2fail) calls, want 2"
run bothfail 3 "3 1"
[ "$(ncalls bothfail)" = 2 ] || fail "bothfail: $(ncalls bothfail) calls, want 2"

# 5. No ./pkg/hub in the list: exit 4, nothing linted.
DIRS="$root
$root/pkg/hubclient
" run nohub 4 "0 0"
[ "$(ncalls nohub)" = 0 ] || fail "nohub: golangci-lint ran"

# 6. Only ./pkg/hub: exit 4 (nothing for pass 1), nothing linted.
DIRS="$root/pkg/hub
" run onlyhub 4 "0 0"
[ "$(ncalls onlyhub)" = 0 ] || fail "onlyhub: golangci-lint ran"

# 7. A directory outside the module root: exit 4.
DIRS="$root/pkg/hub
/elsewhere/pkg
" run outside 4 "0 0"
[ "$(ncalls outside)" = 0 ] || fail "outside: golangci-lint ran"
[ "$(cat "$tmp/outside.out")" = "golangci-lint-split: go list returned a directory outside $root: /elsewhere/pkg" ] ||
	fail "outside: wrong cause: $(cat "$tmp/outside.out")"

# 8. ./pkg/hub listed twice: exit 4, nothing linted.
DIRS="$root/pkg/hub
$root/pkg/hub
$root/pkg/hubclient
" run twohub 4 "0 0"
[ "$(ncalls twohub)" = 0 ] || fail "twohub: golangci-lint ran"
[ "$(cat "$tmp/twohub.out")" = 'golangci-lint-split: expected ./pkg/hub exactly once in go list ./..., found it 2 times; the split no longer matches the tree' ] ||
	fail "twohub: wrong cause: $(cat "$tmp/twohub.out")"

# 9. An empty directory line from go list: exit 4, nothing linted.
DIRS="$root/pkg/hub

$root/pkg/hubclient
" run emptyline 4 "0 0"
[ "$(ncalls emptyline)" = 0 ] || fail "emptyline: golangci-lint ran"
[ "$(cat "$tmp/emptyline.out")" = 'golangci-lint-split: go list returned an empty directory line' ] ||
	fail "emptyline: wrong cause: $(cat "$tmp/emptyline.out")"

# 10. go list fails: exit 3, nothing linted.
GO_RC=1 run golistfail 3 "0 0"
[ "$(ncalls golistfail)" = 0 ] || fail "golistfail: golangci-lint ran"

# 11. golangci-lint (the default name) not on PATH: exit 3. PATH holds only
#     bin-nogl, so neither the stub nor a real golangci-lint can be found.
P="$tmp/bin-nogl" run noglpath 3 "0 0"
[ "$(cat "$tmp/noglpath.out")" = 'golangci-lint-split: golangci-lint not found' ] ||
	fail "noglpath: wrong cause: $(cat "$tmp/noglpath.out")"
[ "$(ncalls noglpath)" = 0 ] || fail "noglpath: golangci-lint ran"

# 12. GOLANGCI_LINT names a binary that does not exist: exit 3.
GL="$tmp/bin/no-such-golangci-lint" run nogl 3 "0 0"
[ "$(cat "$tmp/nogl.out")" = "golangci-lint-split: $tmp/bin/no-such-golangci-lint not found" ] ||
	fail "nogl: wrong cause: $(cat "$tmp/nogl.out")"

# 13-15. Bad arguments: exit 64, nothing linted.
run posarg 64 "0 0" ./pkg/foo/...
run tests 64 "0 0" --tests=true
run tests2 64 "0 0" --tests
for n in posarg tests tests2; do
	[ "$(ncalls "$n")" = 0 ] || fail "$n: golangci-lint ran"
done

# The summary carries the number of cases that ran, so a run that stopped
# early cannot look complete: a failing run ends with exactly one line
# "golangci-lint-split selftest: FAILED: <n> failure(s) in <cases> case(s)",
# where <n> is the number of "FAIL: " lines above it.
if [ "$fails" -ne 0 ]; then
	echo "golangci-lint-split selftest: FAILED: $fails failure(s) in $cases case(s)" >&2
	exit 1
fi
echo "golangci-lint-split selftest: ok ($cases cases)"
