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
# It also creates the next release branch, release/vX.(Y+1), in the same
# atomic push, so the weekly preview workflow has a line to roll forward. The
# next branch starts at the stable commit when the newest nightly on main
# contains it, else at that newest nightly; it never starts past the newest
# nightly, so the weekly workflow can fast-forward it. No preview tag is
# created for the next line.
#
# Usage: ./scripts/release/promote-stable.sh [--dry-run] [--no-next-branch] [release-branch]
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

die() { printf '%b%s%b\n' "${RED}" "error: $1" "${RESET}" >&2; exit 1; }
info() { printf '%b%s%b\n' "${BOLD}" "$1" "${RESET}"; }
success() { printf '%b%s%b\n' "${GREEN}" "$1" "${RESET}"; }
dry_run_msg() { printf '%b%s%b\n' "${YELLOW}" "[dry-run] $1" "${RESET}"; }

# --- Parse flags ----------------------------------------------------------

DRY_RUN=false
NEXT_BRANCH_ENABLED=true
RELEASE_BRANCH=""
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=true ;;
    --no-next-branch) NEXT_BRANCH_ENABLED=false ;;
    -h|--help)
      echo "Usage: $0 [--dry-run] [--no-next-branch] [release-branch]"
      echo ""
      echo "Promote the latest preview to a stable release."
      echo ""
      echo "Arguments:"
      echo "  release-branch  e.g. release/v0.3 (auto-detected if omitted)"
      echo ""
      echo "Options:"
      echo "  --dry-run         Show what would be done without executing"
      echo "  --no-next-branch  Push only the stable tag; do not create the next"
      echo "                    release/vX.(Y+1) branch"
      echo "  -h, --help        Show this help message"
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
git fetch origin
git fetch origin --tags

# --- Determine release branch --------------------------------------------

if [ -z "$RELEASE_BRANCH" ]; then
  # Auto-detect: pick the highest release/vX.Y from remote refs, ignoring
  # names that are not exactly release/vX.Y (as select-weekly-preview.sh does).
  RELEASE_BRANCH=""
  for ref in $(git for-each-ref --sort=v:refname --format='%(refname)' 'refs/remotes/origin/release/v*'); do
    name="${ref#refs/remotes/origin/}"
    if [[ "$name" =~ ^release/v[0-9]+\.[0-9]+$ ]]; then
      RELEASE_BRANCH="$name"
    fi
  done
  if [ -z "$RELEASE_BRANCH" ]; then
    die "no release branches found on origin"
  fi
  info "Auto-detected release branch: ${RELEASE_BRANCH}"
fi

[[ "$RELEASE_BRANCH" =~ ^release/v[0-9]+\.[0-9]+$ ]] \
  || die "'${RELEASE_BRANCH}' is not a release/vX.Y branch"

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

LATEST_PREVIEW_TAG=$(git tag -l --sort=v:refname "v${MAJOR}.${MINOR}.*-preview.*" | tail -n 1)

if [ -z "$LATEST_PREVIEW_TAG" ]; then
  die "no preview tags found for ${RELEASE_BRANCH}"
fi

# --- Safety check: preview tag commit == branch HEAD ----------------------

PREVIEW_COMMIT="$(git rev-parse "$LATEST_PREVIEW_TAG")"
BRANCH_HEAD="$(git rev-parse "origin/${RELEASE_BRANCH}")"

if [ "$PREVIEW_COMMIT" != "$BRANCH_HEAD" ]; then
  echo ""
  printf '%b%s%b\n' "${RED}" "Safety check failed!" "${RESET}"
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

# --- Determine the next release branch ------------------------------------

# release/vX.Y -> release/vX.(Y+1). The weekly preview workflow refuses a
# promoted line, so the next line must exist for it to tag
# vX.(Y+1).0-preview.1.
NEXT_BRANCH=""
NEXT_BASE=""
NEXT_RULE=""
NEWER_NIGHTLY=false
if [ "$NEXT_BRANCH_ENABLED" = true ]; then
  NEXT_BRANCH="release/v${MAJOR}.$((10#${MINOR} + 1))"

  git rev-parse -q --verify "refs/remotes/origin/main^{commit}" >/dev/null \
    || die "origin/main not found; cannot choose a base for ${NEXT_BRANCH} (use --no-next-branch to push only the stable tag)"

  # Ask origin directly: a stale remote-tracking ref must not hide or invent
  # the branch.
  NEXT_LS_STATUS=0
  git ls-remote --exit-code --heads origin "refs/heads/${NEXT_BRANCH}" >/dev/null || NEXT_LS_STATUS=$?
  case "$NEXT_LS_STATUS" in
    0) die "branch '${NEXT_BRANCH}' already exists on origin (use --no-next-branch to push only the stable tag)" ;;
    2) ;;
    *) die "could not list branches on origin (git ls-remote exit ${NEXT_LS_STATUS})" ;;
  esac

  # Newest nightly-YYYYMMDD tag on main, newest by name as in
  # select-weekly-preview.sh.
  NEWEST_NIGHTLY=""
  NEWEST_NIGHTLY_COMMIT=""
  while IFS= read -r t; do
    [[ "$t" =~ ^nightly-[0-9]{8}$ ]] || continue
    sha="$(git rev-parse "refs/tags/${t}^{commit}")"
    if git merge-base --is-ancestor "$sha" "refs/remotes/origin/main"; then
      NEWEST_NIGHTLY="$t"
      NEWEST_NIGHTLY_COMMIT="$sha"
      break
    fi
  done < <(git tag -l --sort=-v:refname 'nightly-*')

  [ -n "$NEWEST_NIGHTLY" ] \
    || die "no nightly-YYYYMMDD tag is reachable from origin/main; cannot choose a base for ${NEXT_BRANCH} (use --no-next-branch to push only the stable tag)"

  if git merge-base --is-ancestor "$PREVIEW_COMMIT" "$NEWEST_NIGHTLY_COMMIT"; then
    NEXT_BASE="$PREVIEW_COMMIT"
    NEXT_RULE="stable commit (contained in ${NEWEST_NIGHTLY}, the newest nightly on main)"
    # The weekly selection only takes a nightly the branch does not contain.
    if [ "$NEWEST_NIGHTLY_COMMIT" != "$PREVIEW_COMMIT" ]; then
      NEWER_NIGHTLY=true
    fi
  else
    NEXT_BASE="$NEWEST_NIGHTLY_COMMIT"
    NEXT_RULE="${NEWEST_NIGHTLY}, the newest nightly on main (the stable commit is not in it)"
  fi
fi

# --- Confirm what we will do ----------------------------------------------

echo ""
info "=== Promote to Stable Release ==="
echo "  Branch:          ${RELEASE_BRANCH}"
echo "  Preview tag:     ${LATEST_PREVIEW_TAG}"
echo "  Stable tag:      ${STABLE_TAG}"
echo "  Commit:          ${PREVIEW_COMMIT}"
if [ "$NEXT_BRANCH_ENABLED" = true ]; then
  echo "  Next branch:     ${NEXT_BRANCH}"
  echo "  Next base:       ${NEXT_BASE}"
  echo "  Next base rule:  ${NEXT_RULE}"
else
  echo "  Next branch:     (not created: --no-next-branch)"
fi
echo ""

if [ "$DRY_RUN" = true ]; then
  dry_run_msg "Would create stable tag '${STABLE_TAG}' on commit ${PREVIEW_COMMIT}"
  if [ "$NEXT_BRANCH_ENABLED" = true ]; then
    dry_run_msg "Would create branch '${NEXT_BRANCH}' on commit ${NEXT_BASE}"
    dry_run_msg "Would push tag and branch to origin in one atomic push"
  else
    dry_run_msg "Would push tag to origin"
  fi
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

if [ "$NEXT_BRANCH_ENABLED" = true ]; then
  # One atomic push: the stable tag never exists without the next line, and
  # the next line never exists without the stable tag. The empty lease makes
  # the branch push a create: it is refused if the branch appeared on origin
  # after the check above, even as a fast-forward.
  info "Pushing tag ${STABLE_TAG} and branch ${NEXT_BRANCH} to origin (atomic)..."
  if ! git push --atomic --force-with-lease="refs/heads/${NEXT_BRANCH}:" origin \
      "refs/tags/${STABLE_TAG}" "${NEXT_BASE}:refs/heads/${NEXT_BRANCH}"; then
    git tag -d "$STABLE_TAG" >/dev/null
    die "atomic push failed; neither ${STABLE_TAG} nor ${NEXT_BRANCH} was created on origin (local tag removed)"
  fi
else
  info "Pushing tag to origin..."
  git push origin "refs/tags/${STABLE_TAG}"
fi

# --- Summary --------------------------------------------------------------

echo ""
success "=== Stable release promoted successfully ==="
echo "  Branch:      ${RELEASE_BRANCH}"
echo "  Preview tag: ${LATEST_PREVIEW_TAG} (promoted from)"
echo "  Stable tag:  ${STABLE_TAG}"
echo "  Commit:      ${PREVIEW_COMMIT}"
if [ "$NEXT_BRANCH_ENABLED" = true ]; then
  echo "  Next branch: ${NEXT_BRANCH} at ${NEXT_BASE}"
fi
echo ""
echo "The build-release workflow should now be triggered by the tag push."
if [ "$NEXT_BRANCH_ENABLED" = true ]; then
  NEXT_PREVIEW="${NEXT_BRANCH#release/}.0-preview.1"
  if [ "$NEWER_NIGHTLY" = true ]; then
    echo "The weekly preview workflow tags ${NEXT_PREVIEW} on ${NEXT_BRANCH} on Friday, once a"
    echo "nightly newer than ${NEXT_BRANCH} has passed CI."
    echo "To tag it sooner, dispatch build-preview-weekly.yml by hand: a nightly on main is"
    echo "newer than ${NEXT_BRANCH}, and the newest such nightly that passed CI is used."
  else
    echo "No nightly on main is newer than ${NEXT_BRANCH} yet, so the weekly preview workflow,"
    echo "scheduled or dispatched by hand, tags ${NEXT_PREVIEW} only after the next nightly"
    echo "that passes CI."
  fi
fi
