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
# Rules for the image tag version (the version prefix of the tag, e.g.
# 1.26.1 in golang:1.26.1-bookworm or golang:1.26.1@sha256:...):
#   - A patch-pinned tag (X.Y.Z) or a pre-release tag (X.YrcN, X.YbetaN)
#     must be >= the go directive, in Go's version order
#     (X.Y < X.Ybeta1 < X.Yrc1 < X.Y.0 < X.Y.1).
#   - A minor-only tag (X.Y) tracks the latest X.Y.Z release, so it must
#     be >= the go directive's X.Y.
#   - A golang image without a version tag (golang, golang:latest,
#     golang@sha256:..., golang:${ARG}) and a Dockerfile without a go.mod or
#     go directive next to it are violations: the check cannot compare them.
# FROM is matched case-insensitively and backslash line continuations are
# joined, as Docker does.
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

# version_key VERSION FLOATING: prints a key that sorts (with sort -V) in
# Go's version order: MAJOR.MINOR.STAGE.N, where STAGE is 0 for a bare X.Y
# language version, 1 for betaN, 2 for rcN and 3 for a release X.Y.N. With
# FLOATING=1, a bare X.Y is an image tag that tracks the latest X.Y release
# and sorts after every X.Y.N. Prints nothing when VERSION is not exactly
# one of those forms.
version_key() {
  local v="$1" floating="$2"
  if [[ "$v" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
    echo "${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.3.${BASH_REMATCH[3]}"
  elif [[ "$v" =~ ^([0-9]+)\.([0-9]+)(beta|rc)([0-9]+)$ ]]; then
    local stage=1
    [[ "${BASH_REMATCH[3]}" == "rc" ]] && stage=2
    echo "${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.${stage}.${BASH_REMATCH[4]}"
  elif [[ "$v" =~ ^([0-9]+)\.([0-9]+)$ ]]; then
    if [[ "$floating" == 1 ]]; then
      echo "${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.4.0"
    else
      echo "${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.0.0"
    fi
  fi
}

# key_ge A B: true when version key A >= version key B.
key_ge() {
  [[ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -n1)" == "$2" ]]
}

# module_go_version GOMOD: prints the go directive of GOMOD (empty if none).
module_go_version() {
  awk '$1 == "go" { print $2; exit }' "$1"
}

# golang_from_lines DOCKERFILE: prints each FROM instruction that mentions
# golang, with backslash continuations joined into one line. This is a coarse
# filter: it also matches a stage alias named golang, which scan() drops by
# checking the image reference itself.
golang_from_lines() {
  awk '{
    s = $0
    while (s ~ /\\[[:space:]]*$/) {
      sub(/\\[[:space:]]*$/, "", s)
      if ((getline nxt) <= 0) break
      s = s " " nxt
    }
    print s
  }' "$1" | grep -iE '^[[:space:]]*FROM[[:space:]](.*[[:space:]/])?golang([:@][^[:space:]]*)?([[:space:]]|$)' || true
}

# scan ROOT: checks ROOT/extras/*/Dockerfile*. Prints one line per golang
# image to stdout and one line per violation to stderr. Returns 1 on any
# violation, 0 otherwise.
scan() {
  local root="$1" failed=0 checked=0
  local dockerfile dir gomod rel want want_key line ref tag have have_key
  shopt -s nullglob
  for dockerfile in "$root"/extras/*/Dockerfile*; do
    dir="$(dirname "$dockerfile")"
    gomod="$dir/go.mod"
    rel="${dockerfile#"$root"/}"
    while IFS= read -r line; do
      # Image reference: the first FROM argument that is not a flag.
      ref="$(awk '{ for (i = 2; i <= NF; i++) if ($i !~ /^--/) { print $i; exit } }' <<<"$line")"
      # Only the image itself counts, not a stage alias (FROM x AS golang).
      if [[ ! "$ref" =~ (^|/)golang([:@]|$) ]]; then
        continue
      fi
      checked=$((checked + 1))
      # The tag follows "golang:"; a ref without one (golang, golang@sha256:...)
      # has no tag and so no version.
      tag=""
      if [[ "$ref" == *golang:* ]]; then
        tag="${ref##*golang:}"
      fi
      have="${tag%%[-@]*}"
      if [[ ! -f "$gomod" ]]; then
        echo "  $rel: $ref has no go.mod next to it to compare against" >&2
        failed=1
        continue
      fi
      want="$(module_go_version "$gomod")"
      want_key="$(version_key "$want" 0)"
      if [[ -z "$want_key" ]]; then
        echo "  $rel: ${gomod#"$root"/} has no usable go directive" >&2
        failed=1
        continue
      fi
      have_key="$(version_key "$have" 1)"
      if [[ -z "$have_key" ]]; then
        echo "  $rel: $ref has no Go version in its tag (module wants go $want)" >&2
        failed=1
        continue
      fi
      if ! key_ge "$have_key" "$want_key"; then
        echo "  $rel: golang:$have is older than the module's go $want" >&2
        failed=1
        continue
      fi
      echo "  $rel: golang:$have satisfies go $want"
    done < <(golang_from_lines "$dockerfile")
  done
  shopt -u nullglob
  echo "  ($checked golang image(s) checked)"
  return "$failed"
}

self_test() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # write_case NAME GO_DIRECTIVE DOCKERFILE: one fixture module. In
  # DOCKERFILE, <NL> stands for a newline and <BS> for a backslash. An empty
  # GO_DIRECTIVE writes no go.mod; "-" writes a go.mod without a go line.
  write_case() {
    local body="$3"
    mkdir -p "$tmp/$1/extras/m"
    if [[ "$2" == "-" ]]; then
      printf 'module example.com/m\n' >"$tmp/$1/extras/m/go.mod"
    elif [[ -n "$2" ]]; then
      printf 'module example.com/m\n\ngo %s\n' "$2" >"$tmp/$1/extras/m/go.mod"
    fi
    body="${body//<NL>/$'\n'}"
    body="${body//<BS>/\\}"
    printf '%s\nRUN true\n' "$body" >"$tmp/$1/extras/m/Dockerfile"
  }

  # name|go directive|Dockerfile|expected exit (0 pass, 1 violation)
  local cases=(
    "patch-equal|1.26.1|FROM golang:1.26.1 AS builder|0"
    "patch-newer|1.26.1|FROM golang:1.26.2-bookworm AS builder|0"
    "patch-older|1.26.1|FROM golang:1.26.0 AS builder|1"
    "patch-double-digit|1.26.9|FROM golang:1.26.10 AS builder|0"
    "minor-older|1.26.1|FROM golang:1.25 AS builder|1"
    "minor-floating|1.26.1|FROM golang:1.26-alpine AS builder|0"
    "minor-directive|1.25|FROM golang:1.25 AS builder|0"
    "rc-older-than-release|1.26.1|FROM golang:1.26rc1 AS builder|1"
    "rc-newer-rc|1.26rc1|FROM golang:1.26rc2-alpine AS builder|0"
    "rc-older-rc|1.26rc2|FROM golang:1.26rc1 AS builder|1"
    "release-after-rc|1.26rc1|FROM golang:1.26.0 AS builder|0"
    "beta-before-rc|1.26rc1|FROM golang:1.26beta1 AS builder|1"
    "digest-pinned|1.26.1|FROM golang:1.26.1@sha256:0000 AS builder|0"
    "digest-only|1.26.1|FROM golang@sha256:0000 AS builder|1"
    "no-tag|1.26.1|FROM golang AS builder|1"
    "latest-tag|1.26.1|FROM golang:latest AS builder|1"
    "platform-flag|1.26.1|FROM --platform=\$BUILDPLATFORM golang:1.25 AS builder|1"
    "library-prefix|1.26.1|FROM docker.io/library/golang:1.26.1 AS builder|0"
    "lowercase-from|1.26.1|from golang:1.25 as builder|1"
    "continuation|1.26.1|FROM <BS><NL>  golang:1.25 AS builder|1"
    "two-stage-one-stale|1.26.1|FROM golang:1.26.1 AS a<NL>FROM golang:1.25 AS b|1"
    "two-stage-both-ok|1.26.1|FROM golang:1.26.1 AS a<NL>FROM golang:1.26 AS b|0"
    "arg-tag|1.26.1|FROM golang:\${GO_VERSION} AS builder|1"
    "no-gomod||FROM golang:1.26.1 AS builder|1"
    "no-go-directive|-|FROM golang:1.26.1 AS builder|1"
    "not-golang|1.26.1|FROM debian:bookworm-slim|0"
    "golang-lookalike|1.26.1|FROM golangci/golangci-lint:v2 AS lint|0"
    "alias-named-golang|1.26.1|FROM debian:bookworm AS golang|0"
    "alias-of-golang-stage|1.26.1|FROM golang:1.26.1 AS b<NL>FROM b AS golang|0"
    "minor-with-suffix-junk|1.26.1|FROM golang:1.26foo AS builder|1"
    "beta-without-number|1.26.1|FROM golang:1.26beta AS builder|1"
    "patch-with-junk|1.26.1|FROM golang:1.26.1foo AS builder|1"
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
