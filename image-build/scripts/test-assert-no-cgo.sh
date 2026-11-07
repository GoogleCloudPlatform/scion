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

# Tests for image-build/lib/assert-no-cgo.sh's exit contract. Pure bash: no
# go, no docker, no network.
#
# assert-no-cgo.sh reads the build settings of a Go binary through
# `go version -m`. Each case points its GO override at a stub that prints a
# fixed `go version -m` answer (or fails), and checks the exit code:
# 0 built with CGO_ENABLED=0, 1 not, 2 build info unreadable or bad usage
# (no argument, two arguments, or an empty argument).
#
# Usage: bash image-build/scripts/test-assert-no-cgo.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ASSERT="$(cd "${SCRIPT_DIR}/.." && pwd)/lib/assert-no-cgo.sh"
[ -f "${ASSERT}" ] || { echo "assert-no-cgo.sh not found at ${ASSERT}" >&2; exit 2; }

TMP="$(mktemp -d "${TMPDIR:-/tmp}/test-assert-no-cgo.XXXXXX")"
trap 'rm -rf -- "${TMP:?}"' EXIT

FAILS=0
PASSES=0
pass() { PASSES=$((PASSES + 1)); echo "ok   - $1"; }
fail() { FAILS=$((FAILS + 1)); echo "FAIL - $1" >&2; }
check_eq() {
  if [ "$2" = "$3" ]; then
    pass "$1"
  else
    fail "$1"
    printf '       want: %s\n       got:  %s\n' "$2" "$3" >&2
  fi
}

BIN="${TMP}/sciontool"
: > "${BIN}"

# stub NAME BODY: writes an executable stub go whose body is BODY.
stub() {
  printf '#!/usr/bin/env bash\n%s\n' "$2" > "${TMP}/$1"
  chmod +x "${TMP}/$1"
}

# run_assert STUB [ARGS...]: runs assert-no-cgo.sh with GO=STUB; sets RC and OUT.
run_assert() {
  local go=$1
  shift
  RC=0
  OUT=$(GO="${TMP}/${go}" bash "${ASSERT}" "$@" 2>/dev/null) || RC=$?
}

# The answering stubs accept only the call assert-no-cgo.sh makes,
# `go version -m BIN`; any other argv exits 99, which assert-no-cgo.sh
# reports as unreadable build info (exit 2), so the 0 and 1 cases fail loudly.
# shellcheck disable=SC2016  # evaluated later by the go stub: $1, $2, $# and $* must stay literal here
ARGV_CHECK='[ "$1" = version ] && [ "$2" = -m ] && [ "$#" -eq 3 ] || { echo "unexpected go argv: $*" >&2; exit 99; }'
stub go-cgo0 "${ARGV_CHECK}
printf '%s: go1.25.0\n\tpath\tgithub.com/GoogleCloudPlatform/scion/cmd/sciontool\n\tbuild\tCGO_ENABLED=0\n' \"\$3\""
stub go-cgo1 "${ARGV_CHECK}
printf '%s: go1.25.0\n\tpath\tgithub.com/GoogleCloudPlatform/scion/cmd/sciontool\n\tbuild\tCGO_ENABLED=1\n' \"\$3\""
stub go-nocgo "${ARGV_CHECK}
printf '%s: go1.25.0\n\tpath\tgithub.com/GoogleCloudPlatform/scion/cmd/sciontool\n\tbuild\t-trimpath=true\n' \"\$3\""
stub go-fail "echo 'go: not a Go executable' >&2; exit 1"

run_assert go-cgo0 "${BIN}"
check_eq "CGO_ENABLED=0: exit 0" 0 "${RC}"
check_eq "CGO_ENABLED=0: success line" "assert-no-cgo: ${BIN} is built with CGO_ENABLED=0" "${OUT}"

run_assert go-cgo1 "${BIN}"
check_eq "CGO_ENABLED=1: exit 1" 1 "${RC}"

run_assert go-nocgo "${BIN}"
check_eq "no CGO_ENABLED build setting: exit 1" 1 "${RC}"

run_assert go-fail "${BIN}"
check_eq "go version -m fails: exit 2" 2 "${RC}"

run_assert go-cgo0
check_eq "no argument: exit 2" 2 "${RC}"

run_assert go-cgo0 "${BIN}" "${BIN}"
check_eq "two arguments: exit 2" 2 "${RC}"

run_assert go-cgo0 ""
check_eq "empty argument: exit 2" 2 "${RC}"

echo ""
echo "${PASSES} passed, ${FAILS} failed"
[ "${FAILS}" -eq 0 ]
