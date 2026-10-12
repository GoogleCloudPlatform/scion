#!/usr/bin/env bash
# Checks that every golang base image in an extras/*/Dockerfile provides at
# least the Go version that the module's go.mod declares.
#
# Each extras/ module is a separate Go module with its own `go` directive.
# When a Dockerfile's golang image is older than that directive, the build
# only works because the go command downloads a newer toolchain at build
# time. With GOTOOLCHAIN=local, or with no network access, it fails
# (ptone/scion#4794).
#
# Rules for the image tag version (the numeric prefix of the tag, e.g.
# 1.26.1 in golang:1.26.1-bookworm or golang:1.26.1@sha256:...):
#   - A patch-pinned tag (X.Y.Z) must be >= the go directive.
#   - A minor-only tag (X.Y) tracks the latest X.Y.Z release, so it must
#     be >= the go directive's X.Y.
#   - A tag without a numeric version (golang:latest, golang:${ARG}) and a
#     Dockerfile without a go.mod next to it are violations: the check cannot
#     compare them.
#
# Severity: FORMATTING-GRADE
#   - extras/ missing: exit 4 (nothing analysed)
#   - No golang images found: exit 0
#   - Violations found: exit 1
#   - Clean: exit 0
#
# Usage: check-extras-go-images.sh [--self-test]
#
# See hack/LINT-CONVENTIONS.md for conventions.
set -euo pipefail

cd "$(dirname "$0")/.."

# version_ge A B: true when version A >= version B.
version_ge() {
  [[ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -n1)" == "$2" ]]
}

# module_go_version GOMOD: prints the go directive of GOMOD (empty if none).
module_go_version() {
  awk '$1 == "go" { print $2; exit }' "$1"
}

# scan ROOT: checks ROOT/extras/*/Dockerfile*. Prints one line per golang
# image to stdout and one line per violation to stderr. Returns 1 on any
# violation, 0 otherwise.
scan() {
  local root="$1" failed=0 checked=0
  local dockerfile dir gomod want line ref tag have want_minor
  shopt -s nullglob
  for dockerfile in "$root"/extras/*/Dockerfile*; do
    dir="$(dirname "$dockerfile")"
    gomod="$dir/go.mod"
    rel="${dockerfile#"$root"/}"
    while IFS= read -r line; do
      checked=$((checked + 1))
      # Image reference: the first FROM argument that is not a flag.
      ref="$(awk '{ for (i = 2; i <= NF; i++) if ($i !~ /^--/) { print $i; exit } }' <<<"$line")"
      tag="${ref#*golang:}"
      have="$(grep -oE '^[0-9]+\.[0-9]+(\.[0-9]+)?' <<<"$tag" || true)"
      if [[ ! -f "$gomod" ]]; then
        echo "  $rel: $ref has no go.mod next to it to compare against" >&2
        failed=1
        continue
      fi
      want="$(module_go_version "$gomod")"
      if [[ -z "$want" ]]; then
        echo "  $rel: ${gomod#"$root"/} has no go directive" >&2
        failed=1
        continue
      fi
      if [[ -z "$have" ]]; then
        echo "  $rel: $ref has no numeric Go version in its tag (module wants go $want)" >&2
        failed=1
        continue
      fi
      if [[ "$have" =~ ^[0-9]+\.[0-9]+$ ]]; then
        want_minor="$(grep -oE '^[0-9]+\.[0-9]+' <<<"$want")"
        if ! version_ge "$have" "$want_minor"; then
          echo "  $rel: golang:$have is older than the module's go $want" >&2
          failed=1
          continue
        fi
      elif ! version_ge "$have" "$want"; then
        echo "  $rel: golang:$have is older than the module's go $want" >&2
        failed=1
        continue
      fi
      echo "  $rel: golang:$have satisfies go $want"
    done < <(grep -E '^[[:space:]]*FROM[[:space:]](.*[[:space:]/])?golang:' "$dockerfile" || true)
  done
  shopt -u nullglob
  echo "  ($checked golang image(s) checked)"
  return "$failed"
}

self_test() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # write_case NAME GO_DIRECTIVE FROM_LINE: one fixture module.
  write_case() {
    mkdir -p "$tmp/$1/extras/m"
    if [[ -n "$2" ]]; then
      printf 'module example.com/m\n\ngo %s\n' "$2" >"$tmp/$1/extras/m/go.mod"
    fi
    printf '%s\nRUN true\n' "$3" >"$tmp/$1/extras/m/Dockerfile"
  }

  # name|go directive|FROM line|expected exit (0 pass, 1 violation)
  local cases=(
    "patch-equal|1.26.1|FROM golang:1.26.1 AS builder|0"
    "patch-newer|1.26.1|FROM golang:1.26.2-bookworm AS builder|0"
    "patch-older|1.26.1|FROM golang:1.26.0 AS builder|1"
    "minor-older|1.26.1|FROM golang:1.25 AS builder|1"
    "minor-floating|1.26.1|FROM golang:1.26-alpine AS builder|0"
    "minor-directive|1.25|FROM golang:1.25 AS builder|0"
    "digest-pinned|1.26.1|FROM golang:1.26.1@sha256:0000 AS builder|0"
    "platform-flag|1.26.1|FROM --platform=\$BUILDPLATFORM golang:1.25 AS builder|1"
    "library-prefix|1.26.1|FROM docker.io/library/golang:1.26.1 AS builder|0"
    "arg-tag|1.26.1|FROM golang:\${GO_VERSION} AS builder|1"
    "no-gomod||FROM golang:1.26.1 AS builder|1"
    "not-golang|1.26.1|FROM debian:bookworm-slim|0"
  )
  local c name go from want got failed=0
  for c in "${cases[@]}"; do
    IFS='|' read -r name go from want <<<"$c"
    write_case "$name" "$go" "$from"
    got=0
    scan "$tmp/$name" >/dev/null 2>&1 || got=$?
    if [[ "$got" != "$want" ]]; then
      echo "self-test FAIL: $name: exit $got, want $want" >&2
      failed=1
    fi
  done
  if [[ "$failed" != 0 ]]; then
    return 1
  fi
  echo "check-extras-go-images: self-test passed (${#cases[@]} cases)"
}

if [[ "${1:-}" == "--self-test" ]]; then
  self_test
  exit $?
fi

sha="$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")"
if [[ "$sha" != "unknown" && -n "$(git status --porcelain 2>/dev/null)" ]]; then
  sha="${sha}-dirty"
fi

if [[ ! -d extras ]]; then
  echo "check-extras-go-images: analysed ${sha}: extras/ not found, nothing analysed" >&2
  exit 4
fi

echo "check-extras-go-images: analysed ${sha}"
if ! scan "$PWD"; then
  echo "check-extras-go-images: FAIL: bump the golang image to at least the module's go version (see image-build/core-base/Dockerfile GO_VERSION)" >&2
  exit 1
fi
echo "check-extras-go-images: OK"
