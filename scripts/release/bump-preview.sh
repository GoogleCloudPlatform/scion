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

# scripts/release/bump-preview.sh — Bump the preview number on a release branch
#
# Finds the highest existing preview tag for a release branch and creates the
# next one on the branch HEAD. Used after cherry-picks to produce a new preview
# build.
#
# Usage: ./scripts/release/bump-preview.sh [--dry-run] [release-branch]
#
# If release-branch is not specified, auto-detects the latest release/vX.Y branch.

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

die() { printf "${RED}error:${RESET} %s\n" "$1" >&2; exit 1; }
info() { printf "${BOLD}%s${RESET}\n" "$1"; }
success() { printf "${GREEN}%s${RESET}\n" "$1"; }
dry_run_msg() { printf "${YELLOW}[dry-run]${RESET} %s\n" "$1"; }

# --- Parse flags ----------------------------------------------------------

DRY_RUN=false
RELEASE_BRANCH=""
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=true ;;
    -h|--help)
      echo "Usage: $0 [--dry-run] [release-branch]"
      echo ""
      echo "Bump the preview number on a release branch."
      echo ""
      echo "Arguments:"
      echo "  release-branch  e.g. release/v0.3 (auto-detected if omitted)"
      echo ""
      echo "Options:"
      echo "  --dry-run  Show what would be done without executing"
      echo "  -h, --help Show this help message"
      exit 0
      ;;
    -*)  die "unknown option: $arg" ;;
    *)
      if [ -n "$RELEASE_BRANCH" ]; then
        die "unexpected argument: $arg (release branch already set to '${RELEASE_BRANCH}')"
      fi
      RELEASE_BRANCH="$arg"
      ;;
  esac
done

# --- Prerequisites --------------------------------------------------------

command -v git >/dev/null 2>&1 || die "git is not installed"
git rev-parse --is-inside-work-tree >/dev/null 2>&1 || die "not inside a git repository"
git remote get-url origin >/dev/null 2>&1 || die "no 'origin' remote configured"

# --- Fetch latest ---------------------------------------------------------

info "Fetching latest from origin..."
git fetch origin --tags

# --- Determine release branch --------------------------------------------

if [ -z "$RELEASE_BRANCH" ]; then
  # Auto-detect: pick the highest release/vX.Y from remote refs.
  RELEASE_BRANCH=""
  for ref in $(git for-each-ref --format='%(refname:short)' 'refs/remotes/origin/release/v*' | sort -V); do
    RELEASE_BRANCH="${ref#origin/}"
  done
  if [ -z "$RELEASE_BRANCH" ]; then
    die "no release branches found on origin"
  fi
  info "Auto-detected release branch: ${RELEASE_BRANCH}"
fi

# Validate the branch exists on origin.
if ! git rev-parse "origin/${RELEASE_BRANCH}" >/dev/null 2>&1; then
  die "branch '${RELEASE_BRANCH}' does not exist on origin"
fi

# --- Checkout and pull the release branch ----------------------------------

info "Checking out ${RELEASE_BRANCH}..."
git checkout "$RELEASE_BRANCH" 2>/dev/null || git checkout -b "$RELEASE_BRANCH" "origin/${RELEASE_BRANCH}"
git pull origin "$RELEASE_BRANCH"

# --- Extract version from branch name ------------------------------------

# release/vX.Y -> X.Y
BRANCH_VERSION="${RELEASE_BRANCH#release/v}"
MAJOR="${BRANCH_VERSION%%.*}"
MINOR="${BRANCH_VERSION#*.}"

# --- Find the highest preview tag for this release ------------------------

HIGHEST_PREVIEW=0
for tag in $(git tag -l "v${MAJOR}.${MINOR}.*-preview.*" | sort -V); do
  # Extract preview number: vX.Y.Z-preview.N -> N
  preview_num="${tag##*-preview.}"
  if [ "$preview_num" -gt "$HIGHEST_PREVIEW" ] 2>/dev/null; then
    HIGHEST_PREVIEW="$preview_num"
  fi
done

if [ "$HIGHEST_PREVIEW" -eq 0 ]; then
  die "no existing preview tags found for ${RELEASE_BRANCH}. Use cut-preview.sh to create the first one."
fi

NEXT_PREVIEW=$((HIGHEST_PREVIEW + 1))
NEW_TAG="v${MAJOR}.${MINOR}.0-preview.${NEXT_PREVIEW}"
BRANCH_HEAD="$(git rev-parse HEAD)"
PREV_TAG="v${MAJOR}.${MINOR}.0-preview.${HIGHEST_PREVIEW}"

# --- Confirm what we will do ----------------------------------------------

echo ""
info "=== Bump Preview Release ==="
echo "  Branch:       ${RELEASE_BRANCH}"
echo "  Previous tag: ${PREV_TAG}"
echo "  New tag:      ${NEW_TAG}"
echo "  Commit:       ${BRANCH_HEAD}"
echo ""

if [ "$DRY_RUN" = true ]; then
  dry_run_msg "Would create tag '${NEW_TAG}' on ${RELEASE_BRANCH} HEAD"
  dry_run_msg "Would push tag to origin"
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

info "Creating tag ${NEW_TAG}..."
git tag "$NEW_TAG" HEAD

info "Pushing tag to origin..."
git push origin "$NEW_TAG"

# --- Summary --------------------------------------------------------------

echo ""
success "=== Preview bump successful ==="
echo "  Branch: ${RELEASE_BRANCH}"
echo "  Tag:    ${NEW_TAG}"
echo "  Commit: ${BRANCH_HEAD}"
echo ""
echo "The build-release workflow should now be triggered by the tag push."
