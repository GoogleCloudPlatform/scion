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

# Guard: every Dockerfile that compiles the root Go module must copy every
# top-level directory that holds the module's Go code, and no Docker or Cloud
# Build ignore file may drop one of those directories from the build context.
#
# ptone/scion#5453: pkg/config started importing internal/settingslockhook,
# but image-build/scion-base/Dockerfile and image-build/omni/Dockerfile copy
# source directories one by one and did not copy internal/, and .gcloudignore
# dropped internal/ from the Cloud Build upload, so the image builds failed.
#
# Scope:
#   - Required directories: top-level directories with a tracked non-test .go
#     file of the root module (files under testdata/, vendor/, node_modules/
#     or a nested go.mod are not part of it), minus EXCLUDED_DIRS below.
#   - Checked Dockerfiles: tracked files named Dockerfile* that run
#     `go build`, outside extras/, plus those under an extras/<name>/ module
#     whose go.mod replaces the root module with ../../ (they compile root
#     packages too). Self-contained extras modules are skipped.
#   - A Dockerfile covers a directory when a COPY (without --from) has a
#     source whose first path component is that directory (dir/, dir/*.go),
#     or copies the whole context (`COPY . .`).
#   - Ignore files: the tracked .dockerignore, .gcloudignore and
#     image-build/gcloudignore-* files (Cloud Build --ignore-file variants).
#     A line that names a required directory as a whole (dir, /dir, dir/,
#     /dir/, dir/**) is a violation.
#   - An excluded directory must not be imported by non-test code in any
#     required directory; that is checked too, so an exclusion cannot hide a
#     real dependency.
#
# Usage:
#   hack/check-image-go-dirs.sh              check the repository
#   hack/check-image-go-dirs.sh --self-test  run the built-in fixture test
#
# Exit codes: 0 clean, 1 violations, 2 could not run (nothing was analysed).

set -uo pipefail
export LC_ALL=C

NAME="check-image-go-dirs"
MODULE="github.com/GoogleCloudPlatform/scion"

# Top-level Go directories of the root module that no image needs.
# Format: "<dir>  <reason>".
EXCLUDED_DIRS=(
  "hack     developer and CI tooling, never imported by the binaries"
  "perf     benchmark programs, never imported by the binaries"
  "scripts  helper programs behind //go:build ignore, never imported by the binaries"
)

# check ROOT: print violations; exit 0 clean, 1 violations, 2 could not run.
check() {
  local root=$1
  local files
  if ! files=$(git -C "$root" ls-files 2>/dev/null) || [[ -z "$files" ]]; then
    echo "$NAME: cannot list tracked files under $root" >&2
    return 2
  fi

  local excluded=() line
  for line in "${EXCLUDED_DIRS[@]}"; do excluded+=("${line%% *}"); done

  # Nested modules: directories (other than the root) holding a go.mod.
  local nested
  nested=$(grep -E '/go\.mod$' <<<"$files" | sed 's|/go\.mod$|/|')

  local gofiles
  gofiles=$(grep -E '\.go$' <<<"$files" | grep -vE '_test\.go$' |
    grep -vE '(^|/)(testdata|vendor|node_modules)/' |
    awk -v nested="$nested" 'BEGIN { n = split(nested, p, "\n") }
      { for (i = 1; i <= n; i++) if (p[i] != "" && index($0, p[i]) == 1) next; print }')

  local required=() d
  for d in $(grep / <<<"$gofiles" | cut -d/ -f1 | sort -u); do
    [[ " ${excluded[*]} " == *" $d "* ]] || required+=("$d")
  done
  if [[ ${#required[@]} -eq 0 ]]; then
    echo "$NAME: found no Go directories of the root module under $root" >&2
    return 2
  fi

  local bad=0 f

  # Excluded directories must not be imported by required code.
  for d in "${excluded[@]}"; do
    local hits
    hits=$(grep -E "^($(IFS='|'; echo "${required[*]}"))/" <<<"$gofiles" |
      (cd "$root" && xargs grep -lE "\"$MODULE/$d(/[^\"]*)?\"" 2>/dev/null))
    if [[ -n "$hits" ]]; then
      echo "EXCLUDED BUT IMPORTED: $d/ is in EXCLUDED_DIRS but imported by:"
      sed 's/^/  /' <<<"$hits"
      bad=1
    fi
  done

  # Dockerfiles that compile the root module.
  local dockerfiles=()
  for f in $(grep -E '(^|/)Dockerfile[^/]*$' <<<"$files"); do
    grep -qE '\bgo build\b' "$root/$f" || continue
    if [[ "$f" == extras/* ]]; then
      local mod=${f#extras/}; mod="extras/${mod%%/*}"
      grep -qE "^replace[[:space:]]+$MODULE[[:space:]]+=>[[:space:]]+\.\./\.\./?[[:space:]]*$" \
        "$root/$mod/go.mod" 2>/dev/null || continue
    fi
    dockerfiles+=("$f")
  done
  if [[ ${#dockerfiles[@]} -eq 0 ]]; then
    echo "$NAME: found no Dockerfile that builds the root module under $root" >&2
    return 2
  fi

  for f in "${dockerfiles[@]}"; do
    # First path component of every COPY source (COPY --from=... skipped).
    local copied
    copied=$(awk 'toupper($1) == "COPY" {
        from = 0; n = 0
        for (i = 2; i <= NF; i++) {
          if ($i ~ /^--from=/) from = 1
          else if ($i !~ /^--/) a[++n] = $i
        }
        if (from) next
        for (i = 1; i < n; i++) { s = a[i]; sub(/^\.\//, "", s); split(s, c, "/"); print (c[1] == "" ? "." : c[1]) }
      }' "$root/$f" | sort -u)
    grep -qx '\.' <<<"$copied" && continue
    for d in "${required[@]}"; do
      if ! grep -qx "$d" <<<"$copied"; then
        echo "MISSING COPY: $f does not copy $d/ (add 'COPY $d/ ...' before its go build, or add $d to EXCLUDED_DIRS in hack/check-image-go-dirs.sh with a reason)"
        bad=1
      fi
    done
  done

  for f in $(grep -E '^(\.dockerignore|\.gcloudignore|image-build/gcloudignore-[^/]+)$' <<<"$files"); do
    for d in "${required[@]}"; do
      local hit
      hit=$(grep -nE "^/?$d(/|/\*\*)?[[:space:]]*$" "$root/$f")
      if [[ -n "$hit" ]]; then
        echo "IGNORED: $f excludes required Go directory $d/ (line ${hit%%:*})"
        bad=1
      fi
    done
  done

  if [[ $bad -eq 0 ]]; then
    echo "$NAME: ok (${#dockerfiles[@]} Dockerfiles copy ${required[*]})"
  fi
  return $bad
}

self_test() {
  # The fixture is its own git repository: never let a leaked environment
  # point the git commands below at the real one.
  unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_COMMON_DIR GIT_OBJECT_DIRECTORY
  local tmp rc out
  tmp=$(mktemp -d) || return 2
  trap 'rm -rf "$tmp"' RETURN
  mkdir -p "$tmp"/{cmd/x,internal/y,hack/z,pkg/p/testdata,web,extras/plug,extras/solo,img,img2,image-build}
  echo 'module x' >"$tmp/go.mod"
  echo 'package main' >"$tmp/cmd/x/main.go"
  echo 'package y' >"$tmp/internal/y/y.go"
  echo 'package z' >"$tmp/hack/z/z.go"
  echo 'package web' >"$tmp/web/w.go"
  echo 'package p' >"$tmp/pkg/p/p_test.go"
  echo 'package t' >"$tmp/pkg/p/testdata/t.go"
  printf 'module plug\nreplace %s => ../../\n' "$MODULE" >"$tmp/extras/plug/go.mod"
  echo 'package main' >"$tmp/extras/plug/main.go"
  echo 'module solo' >"$tmp/extras/solo/go.mod"
  # ./dir and dir/*.go sources count; COPY --from does not.
  printf 'COPY go.mod ./\nCOPY ./cmd/ ./cmd/\nCOPY web/*.go ./web/\nRUN go build ./cmd/x\n' >"$tmp/img/Dockerfile"
  printf 'FROM a AS src\nFROM b\nCOPY --from=src internal/ ./internal/\nCOPY cmd/ cmd/\nCOPY web/ web/\nRUN go build ./cmd/x\n' >"$tmp/img2/Dockerfile"
  printf 'COPY cmd/ cmd/\nCOPY web/ web/\nRUN go build .\n' >"$tmp/extras/plug/Dockerfile"
  printf 'COPY . .\nRUN go build .\n' >"$tmp/extras/solo/Dockerfile"
  printf 'COPY . .\nRUN go build ./cmd/x\n' >"$tmp/Dockerfile"
  printf '/hack/\n' >"$tmp/.gcloudignore"
  printf 'web/node_modules/\n/internal/**\n' >"$tmp/image-build/gcloudignore-extra"
  git -C "$tmp" init -q && git -C "$tmp" add -A || return 2

  out=$(check "$tmp"); rc=$?
  local want
  for want in "MISSING COPY: img/Dockerfile does not copy internal/" \
              "MISSING COPY: img2/Dockerfile does not copy internal/" \
              "MISSING COPY: extras/plug/Dockerfile does not copy internal/" \
              "IGNORED: image-build/gcloudignore-extra excludes required Go directory internal/ (line 2)"; do
    if [[ $rc -ne 1 || "$out" != *"$want"* ]]; then
      echo "$NAME --self-test: FAIL (exit $rc), expected '$want' in:" >&2
      echo "$out" >&2
      return 2
    fi
  done
  if [[ $(grep -c . <<<"$out") -ne 4 ]]; then
    echo "$NAME --self-test: FAIL, unexpected extra findings:" >&2
    echo "$out" >&2
    return 2
  fi

  local f
  for f in img/Dockerfile img2/Dockerfile extras/plug/Dockerfile; do
    sed -i 's|^RUN go build|COPY internal/ internal/\nRUN go build|' "$tmp/$f"
  done
  printf 'web/node_modules/\n' >"$tmp/image-build/gcloudignore-extra"
  printf 'package main\nimport _ "%s/hack/z"\n' "$MODULE" >"$tmp/cmd/x/main.go"
  out=$(check "$tmp"); rc=$?
  if [[ $rc -ne 1 || "$out" != *"EXCLUDED BUT IMPORTED: hack/"* || $(grep -c '^[A-Z]' <<<"$out") -ne 1 ]]; then
    echo "$NAME --self-test: FAIL (exit $rc), expected only an EXCLUDED BUT IMPORTED finding:" >&2
    echo "$out" >&2
    return 2
  fi

  echo 'package main' >"$tmp/cmd/x/main.go"
  out=$(check "$tmp"); rc=$?
  if [[ $rc -ne 0 ]]; then
    echo "$NAME --self-test: FAIL, fixed fixture gave exit $rc:" >&2
    echo "$out" >&2
    return 2
  fi

  echo "$NAME --self-test: ok (missing COPY, COPY --from, discovered ignore file with dir/**, and imported exclusion flagged; ./dir, dir/*.go, whole-context COPY, test files, testdata and self-contained extras accepted)"
}

if [[ "${1:-}" == "--self-test" ]]; then
  self_test
  exit $?
fi

repo_root=$(git rev-parse --show-toplevel 2>/dev/null) || {
  echo "$NAME: not inside a git checkout" >&2
  exit 2
}
check "$repo_root"
