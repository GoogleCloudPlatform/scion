#!/bin/bash
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

# scripts/release/cut-preview.sh — Cut a new preview release from main
#
# Creates a release branch from the current main HEAD and tags the first
# preview build on it. The tag push triggers the build-release workflow.
#
# Usage: ./scripts/release/cut-preview.sh [--dry-run]

set -euo pipefail

# --- Color output (degrades gracefully outside a terminal) ----------------

if [ -t 1 ]; then
  RED='\033[0;31m'
  GREEN='\033[0;32m'
  YELLOW='\033[1;33m'
  BOLD='\033[1m'
  RESET='\033[0m'
else
  RED=''
  GREEN=''
  YELLOW=''
  BOLD=''
  RESET=''
fi

# --- Helpers --------------------------------------------------------------

die() { printf '%b%s%b\n' "${RED}" "error: $1" "${RESET}" >&2; exit 1; }
info() { printf '%b%s%b\n' "${BOLD}" "$1" "${RESET}"; }
success() { printf '%b%s%b\n' "${GREEN}" "$1" "${RESET}"; }
dry_run_msg() { printf '%b%s%b\n' "${YELLOW}" "[dry-run] $1" "${RESET}"; }

# --- Parse flags ----------------------------------------------------------

DRY_RUN=false
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=true ;;
    -h|--help)
      echo "Usage: $0 [--dry-run]"
      echo ""
      echo "Cut a new preview release from main."
      echo ""
      echo "Options:"
      echo "  --dry-run  Show what would be done without executing"
      echo "  -h, --help Show this help message"
      exit 0
      ;;
    *) die "unknown argument: $arg" ;;
  esac
done

# --- Prerequisites --------------------------------------------------------

command -v git >/dev/null 2>&1 || die "git is not installed"
git rev-parse --is-inside-work-tree >/dev/null 2>&1 || die "not inside a git repository"
git remote get-url origin >/dev/null 2>&1 || die "no 'origin' remote configured"

# --- Ensure we are on main and up to date --------------------------------

CURRENT_BRANCH="$(git symbolic-ref --short HEAD 2>/dev/null || true)"
if [ "$CURRENT_BRANCH" != "main" ]; then
  die "must be on the 'main' branch (currently on '${CURRENT_BRANCH}')"
fi

info "Fetching latest from origin..."
git fetch origin
git fetch origin --tags

# --- Determine next version -----------------------------------------------

# Find the highest existing release/vX.Y branch by inspecting remote refs.
HIGHEST_MINOR=""
for ref in $(git for-each-ref --sort=v:refname --format='%(refname:short)' 'refs/remotes/origin/release/v*'); do
  # Extract the version part: origin/release/vX.Y -> X.Y
  version="${ref#origin/release/v}"
  HIGHEST_MINOR="$version"
done

if [ -z "$HIGHEST_MINOR" ]; then
  # No existing release branches — start at 0.3 as specified.
  NEXT_MAJOR=0
  NEXT_MINOR=3
else
  NEXT_MAJOR="${HIGHEST_MINOR%%.*}"
  PREV_MINOR="${HIGHEST_MINOR#*.}"
  NEXT_MINOR=$((PREV_MINOR + 1))
fi

RELEASE_VERSION="v${NEXT_MAJOR}.${NEXT_MINOR}"
BRANCH_NAME="release/${RELEASE_VERSION}"
TAG_NAME="${RELEASE_VERSION}.0-preview.1"
MAIN_HEAD="$(git rev-parse HEAD)"

# --- Pre-existence checks -------------------------------------------------

# Check if branch already exists
if git rev-parse "origin/${BRANCH_NAME}" >/dev/null 2>&1; then
  die "branch '${BRANCH_NAME}' already exists on origin"
fi

# Check if tag already exists
if git rev-parse "${TAG_NAME}" >/dev/null 2>&1; then
  die "tag '${TAG_NAME}' already exists"
fi

# --- Confirm what we will do ----------------------------------------------

echo ""
info "=== Cut Preview Release ==="
echo "  Release branch: ${BRANCH_NAME}"
echo "  Tag:            ${TAG_NAME}"
echo "  Base commit:    ${MAIN_HEAD}"
echo ""

if [ "$DRY_RUN" = true ]; then
  dry_run_msg "Would create branch '${BRANCH_NAME}' from main HEAD"
  dry_run_msg "Would create tag '${TAG_NAME}' on that branch"
  dry_run_msg "Would push branch and tag to origin"
  echo ""
  dry_run_msg "No changes were made."
  exit 0
fi

# --- Confirm before proceeding --------------------------------------------

printf "Proceed? [y/N] "
read -r CONFIRM
case "$CONFIRM" in
  y|Y|yes|YES) ;;
  *) echo "Aborted."; exit 1 ;;
esac

# --- Execute --------------------------------------------------------------

info "Creating branch ${BRANCH_NAME}..."
git branch "$BRANCH_NAME" HEAD

info "Creating tag ${TAG_NAME}..."
git tag "$TAG_NAME" "$BRANCH_NAME"

info "Pushing branch and tag to origin..."
git push origin "$BRANCH_NAME" "$TAG_NAME"

# --- Summary --------------------------------------------------------------

echo ""
success "=== Preview release cut successfully ==="
echo "  Branch: ${BRANCH_NAME}"
echo "  Tag:    ${TAG_NAME}"
echo "  Commit: ${MAIN_HEAD}"
echo ""
echo "The build-release workflow should now be triggered by the tag push."
