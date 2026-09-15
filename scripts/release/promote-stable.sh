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

# scripts/release/promote-stable.sh — Promote the latest preview to stable
#
# Finds the latest preview tag on a release branch, confirms that the tagged
# commit matches the branch HEAD, and creates the corresponding stable tag
# (without the -preview.N suffix).
#
# Usage: ./scripts/release/promote-stable.sh [--dry-run] [release-branch]
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
      echo "Promote the latest preview to a stable release."
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

# --- Extract version from branch name ------------------------------------

# release/vX.Y -> X.Y
BRANCH_VERSION="${RELEASE_BRANCH#release/v}"
MAJOR="${BRANCH_VERSION%%.*}"
MINOR="${BRANCH_VERSION#*.}"

# --- Find the latest preview tag for this release -------------------------

LATEST_PREVIEW_TAG=""
HIGHEST_PREVIEW=0
for tag in $(git tag -l "v${MAJOR}.${MINOR}.*-preview.*" | sort -V); do
  preview_num="${tag##*-preview.}"
  if [ "$preview_num" -gt "$HIGHEST_PREVIEW" ] 2>/dev/null; then
    HIGHEST_PREVIEW="$preview_num"
    LATEST_PREVIEW_TAG="$tag"
  fi
done

if [ -z "$LATEST_PREVIEW_TAG" ]; then
  die "no preview tags found for ${RELEASE_BRANCH}"
fi

# --- Safety check: preview tag commit == branch HEAD ----------------------

PREVIEW_COMMIT="$(git rev-parse "$LATEST_PREVIEW_TAG")"
BRANCH_HEAD="$(git rev-parse "origin/${RELEASE_BRANCH}")"

if [ "$PREVIEW_COMMIT" != "$BRANCH_HEAD" ]; then
  echo ""
  printf "${RED}Safety check failed!${RESET}\n"
  echo "  Preview tag ${LATEST_PREVIEW_TAG} points to: ${PREVIEW_COMMIT}"
  echo "  Branch ${RELEASE_BRANCH} HEAD is at:         ${BRANCH_HEAD}"
  echo ""
  die "the latest preview tag does not match the release branch HEAD. Cherry-pick or bump-preview first."
fi

# --- Determine stable tag ------------------------------------------------

# Extract the patch version from the preview tag: vX.Y.Z-preview.N -> Z
PREVIEW_BASE="${LATEST_PREVIEW_TAG%-preview.*}"  # vX.Y.Z
STABLE_TAG="${PREVIEW_BASE}"                      # vX.Y.Z (no suffix)

# Check if the stable tag already exists.
if git rev-parse "$STABLE_TAG" >/dev/null 2>&1; then
  die "stable tag '${STABLE_TAG}' already exists"
fi

# --- Confirm what we will do ----------------------------------------------

echo ""
info "=== Promote to Stable Release ==="
echo "  Branch:          ${RELEASE_BRANCH}"
echo "  Preview tag:     ${LATEST_PREVIEW_TAG}"
echo "  Stable tag:      ${STABLE_TAG}"
echo "  Commit:          ${PREVIEW_COMMIT}"
echo ""

if [ "$DRY_RUN" = true ]; then
  dry_run_msg "Would create stable tag '${STABLE_TAG}' on commit ${PREVIEW_COMMIT}"
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

info "Creating stable tag ${STABLE_TAG}..."
git tag "$STABLE_TAG" "$PREVIEW_COMMIT"

info "Pushing tag to origin..."
git push origin "$STABLE_TAG"

# --- Summary --------------------------------------------------------------

echo ""
success "=== Stable release promoted successfully ==="
echo "  Branch:      ${RELEASE_BRANCH}"
echo "  Preview tag: ${LATEST_PREVIEW_TAG} (promoted from)"
echo "  Stable tag:  ${STABLE_TAG}"
echo "  Commit:      ${PREVIEW_COMMIT}"
echo ""
echo "The build-release workflow should now be triggered by the tag push."
