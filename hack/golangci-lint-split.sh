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

# golangci-lint over the whole root module, in two passes:
#   pass 1: every package except ./pkg/hub, with tests (golangci-lint's default);
#   pass 2: ./pkg/hub with --tests=false.
#
# Why: with tests, golangci-lint's loader (go list -export -test) compiles
# pkg/hub's test variant (~2,300 files in one package) for export data that
# nothing reads. That one compile peaks at ~14.5 GB RSS: it exhausted the
# 16 GB ubuntu-latest runner (ptone/scion#4208) and dominated the lint job's
# time. Without it the whole lint takes ~3 min and peaks at ~7 GB.
# The cost: errcheck, staticcheck, unused and ineffassign no longer see
# pkg/hub's _test.go files (govet still does, through go vet in the hub shard
# jobs), and
# unused reports pkg/hub helpers that only tests use (see
# hack/LINT-CONVENTIONS.md, "golangci-lint: two passes").
#
# Only ./pkg/hub itself is in pass 2. Its sub-packages (pkg/hub/...) are
# separate packages whose test variants import pkg/hub's non-test build, so
# they stay in pass 1 with their tests. pkg/hub has no external (hub_test)
# test package.
#
# Usage: hack/golangci-lint-split.sh [golangci-lint run flags...]
#   Flags are passed to both passes. Use the --flag=value form; package
#   arguments and --tests are rejected (the script chooses both).
#   GOLANGCI_LINT selects the binary (default: golangci-lint on PATH).
# Used by .github/workflows/ci.yml (lint job) and make golangci-lint.
#
# Exit: pass 1's exit code if non-zero, else pass 2's (both passes always
# run, so one run reports every issue). golangci-lint's own codes pass through
# unchanged, and those include 3 (failure) and 4 (timeout). Before any pass
# runs, the script itself can exit, following hack/LINT-CONVENTIONS.md, with:
# 3 = a required tool is missing or go list failed; 4 = the package list is not
# what the split assumes (no ./pkg/hub, ./pkg/hub twice, nothing else, an
# empty or outside directory), so nothing was linted; 64 = bad arguments. The
# script's own errors are on stderr, prefixed "golangci-lint-split:", and no
# "pass 1" line was printed; that is how to tell them from a pass's 3 or 4.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
root=$(pwd -P)

hub=./pkg/hub
golangci_lint=${GOLANGCI_LINT:-golangci-lint}

for a in "$@"; do
  case "$a" in
    --tests | --tests=*)
      echo "golangci-lint-split: --tests is set by the script (pass 1: true, pass 2: false)" >&2
      exit 64
      ;;
    -*) ;;
    *)
      echo "golangci-lint-split: unexpected argument '$a': pass flags only, in --flag=value form (the script chooses the packages)" >&2
      exit 64
      ;;
  esac
done

command -v go >/dev/null || { echo "golangci-lint-split: go not found" >&2; exit 3; }
command -v "$golangci_lint" >/dev/null || { echo "golangci-lint-split: $golangci_lint not found" >&2; exit 3; }

# -e: a package that does not build still gets listed, so golangci-lint can
# report it (as it would on a plain ./... run) instead of go list failing here.
if ! dirs=$(go list -e -f '{{.Dir}}' ./...); then
  echo "golangci-lint-split: go list failed" >&2
  exit 3
fi

rest=()
nhub=0
while IFS= read -r d; do
  case "$d" in
    "")
      echo "golangci-lint-split: go list returned an empty directory line" >&2
      exit 4
      ;;
    "$root") rel=. ;;
    "$root"/*) rel="./${d#"$root"/}" ;;
    *)
      echo "golangci-lint-split: go list returned a directory outside $root: $d" >&2
      exit 4
      ;;
  esac
  if [ "$rel" = "$hub" ]; then
    nhub=$((nhub + 1))
  else
    rest+=("$rel")
  fi
done <<<"$dirs"

if [ "$nhub" -ne 1 ]; then
  echo "golangci-lint-split: expected $hub exactly once in go list ./..., found it $nhub times; the split no longer matches the tree" >&2
  exit 4
fi
if [ "${#rest[@]}" -eq 0 ]; then
  echo "golangci-lint-split: no packages besides $hub; nothing for pass 1" >&2
  exit 4
fi

echo "golangci-lint-split: pass 1: ${#rest[@]} packages (all but $hub), tests: true"
rc1=0
"$golangci_lint" run "$@" "${rest[@]}" || rc1=$?
echo "golangci-lint-split: pass 2: $hub, tests: false"
rc2=0
"$golangci_lint" run "$@" --tests=false "$hub" || rc2=$?
echo "golangci-lint-split: pass 1 rc=$rc1, pass 2 rc=$rc2"
if [ "$rc1" -ne 0 ]; then
  exit "$rc1"
fi
exit "$rc2"
