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

# scripts/docs/build-release-snapshot.sh — Build and package a versioned docs snapshot tarball
#
# Extracts `docs-site/` from a release tag (`refs/tags/<TAG>`), overlays the
# versioning shell from `main` if the tag predates native versioning support,
# builds the site with `DOCS_BASE=/scion/<TAG>`, verifies that zero unprefixed
# `/scion/` links leaked into the HTML output, and writes:
#   <OUT_DIR>/scion-docs-<TAG>.tar.gz
#   <OUT_DIR>/scion-docs-<TAG>.tar.gz.sha256
#
# Usage:
#   ./scripts/docs/build-release-snapshot.sh --tag <TAG> --out-dir <DIR> [--main-docs <DIR>] [--source-docs <DIR>]

set -euo pipefail

STABLE_RE='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
PREVIEW_RE='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-(preview|rc)\.(0|[1-9][0-9]*)$'
NIGHTLY_RE='^nightly-[0-9]{8}([.-][0-9A-Za-z._-]+)?$'

usage() {
  echo "Usage: $0 --tag <TAG> --out-dir <DIR> [--main-docs <DIR>] [--source-docs <DIR>]" >&2
  exit 2
}

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/../.." && pwd)

TAG=""
OUT_DIR=""
MAIN_DOCS="${REPO_ROOT}/docs-site"
SOURCE_DOCS=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tag)
      [[ $# -ge 2 ]] || usage
      TAG="$2"
      shift 2
      ;;
    --out-dir)
      [[ $# -ge 2 ]] || usage
      OUT_DIR="$2"
      shift 2
      ;;
    --main-docs)
      [[ $# -ge 2 ]] || usage
      MAIN_DOCS="$2"
      shift 2
      ;;
    --source-docs)
      [[ $# -ge 2 ]] || usage
      SOURCE_DOCS="$2"
      shift 2
      ;;
    -h|--help)
      usage
      ;;
    *)
      echo "Unknown option: $1" >&2
      usage
      ;;
  esac
done

[[ -n "$TAG" && -n "$OUT_DIR" ]] || usage

TAG="${TAG#refs/tags/}"
CHANNEL=""
if [[ "$TAG" =~ $STABLE_RE ]]; then
  CHANNEL="stable"
elif [[ "$TAG" =~ $PREVIEW_RE ]]; then
  CHANNEL="preview"
elif [[ "$TAG" =~ $NIGHTLY_RE ]]; then
  CHANNEL="nightly"
else
  printf 'build-release-snapshot: unsupported release tag %q (expected vX.Y.Z, vX.Y.Z-preview.N, vX.Y.Z-rc.N, or nightly-YYYYMMDD)\n' "$TAG" >&2
  exit 2
fi

MAIN_DOCS=$(cd "$MAIN_DOCS" && pwd)
mkdir -p "$OUT_DIR"
OUT_DIR=$(cd "$OUT_DIR" && pwd)

WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/scion-docs-snapshot.XXXXXX")
cleanup() { rm -rf -- "$WORK_DIR"; }
on_signal() {
  trap - "$1"
  cleanup
  kill -"$1" "$$"
}
trap cleanup EXIT
trap 'on_signal INT' INT
trap 'on_signal TERM' TERM

WORK_DOCS="${WORK_DIR}/docs-site"

if [[ -n "$SOURCE_DOCS" ]]; then
  SOURCE_DOCS=$(cd "$SOURCE_DOCS" && pwd)
  mkdir -p "$WORK_DOCS"
  cp -R "${SOURCE_DOCS}/." "$WORK_DOCS/"
else
  if ! git -C "$REPO_ROOT" rev-parse --verify --quiet "refs/tags/${TAG}^{commit}" >/dev/null; then
    printf 'build-release-snapshot: git tag refs/tags/%s not found in %s\n' "$TAG" "$REPO_ROOT" >&2
    exit 1
  fi
  git -C "$REPO_ROOT" archive --format=tar "refs/tags/${TAG}" docs-site | tar -xf - -C "$WORK_DIR"
fi

if [[ ! -f "${WORK_DOCS}/astro.config.mjs" ]]; then
  printf 'build-release-snapshot: docs-site/astro.config.mjs not found for tag %s\n' "$TAG" >&2
  exit 1
fi

node "${MAIN_DOCS}/scripts/prepare-legacy-snapshot.mjs" \
  --main-docs "$MAIN_DOCS" \
  --target-docs "$WORK_DOCS"

(
  cd "$WORK_DOCS"
  npm ci
  DOCS_BASE="/scion/${TAG}" DOCS_CHANNEL="${CHANNEL}" DOCS_VERSION="${TAG}" npm run build
  DOCS_BASE="/scion/${TAG}" npm run verify-snapshot
)

TAR_NAME="scion-docs-${TAG}.tar.gz"
SHA_NAME="${TAR_NAME}.sha256"
TAR_PATH="${OUT_DIR}/${TAR_NAME}"

tar -czf "$TAR_PATH" -C "${WORK_DOCS}/dist" .
(
  cd "$OUT_DIR"
  sha256sum -- "$TAR_NAME" > "${WORK_DIR}/${SHA_NAME}"
  mv -- "${WORK_DIR}/${SHA_NAME}" "${OUT_DIR}/${SHA_NAME}"
)

printf 'build-release-snapshot: built %s (%s) -> %s\n' "$TAG" "$CHANNEL" "$TAR_PATH"
