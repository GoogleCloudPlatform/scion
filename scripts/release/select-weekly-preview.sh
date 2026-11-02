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

# scripts/release/select-weekly-preview.sh — Select the weekly preview roll-forward
#
# Read-only. Works out which nightly tag the latest release/vX.Y branch should
# fast-forward to, and which vX.Y.Z-preview.N tag to create there. It never
# creates, moves or pushes refs; .github/workflows/build-preview-weekly.yml
# does that with the result.
#
# Usage: ./scripts/release/select-weekly-preview.sh [options]
#
# Options:
#   --remote NAME         Remote whose release/* branches are used (default: origin).
#                         Its refs must already be fetched, with tags.
#   --nightly-tag TAG     Use this nightly tag instead of the automatic pick.
#                         Skips the CI guard.
#   --repo OWNER/NAME     Repository for the CI lookup (default: $GITHUB_REPOSITORY,
#                         else GoogleCloudPlatform/scion).
#   --ci-status-cmd CMD   Command printing the "Build & Test" conclusion for the
#                         commit SHA given as its last argument. Default: a
#                         lookup through `gh api`. For tests.
#   --skip-ci-check       Take the newest nightly without the CI guard.
#
# Results are written as key=value lines to $GITHUB_OUTPUT (stdout if unset):
#   selected  true when a fast-forward and tag should be made, else false
#   branch    release branch, e.g. release/v0.3
#   nightly   chosen nightly tag
#   sha       commit of the chosen nightly
#   tag       next preview tag, e.g. v0.3.0-preview.4
# A Markdown explanation goes to $GITHUB_STEP_SUMMARY (stderr if unset).
#
# Exit status: 0 when a selection was made or there is nothing to do;
# non-zero when the run must fail loudly (no release branch, a diverged
# release branch, a bad --nightly-tag, an existing tag, a promoted line).

set -euo pipefail

CHECK_NAME="Build & Test"

REMOTE="origin"
NIGHTLY_OVERRIDE=""
REPO="${GITHUB_REPOSITORY:-GoogleCloudPlatform/scion}"
CI_STATUS_CMD=""
SKIP_CI=false

usage() { sed -n '17,46p' "$0" | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
  case "$1" in
    --remote) REMOTE="${2:?--remote needs a value}"; shift 2 ;;
    --nightly-tag) NIGHTLY_OVERRIDE="${2:?--nightly-tag needs a value}"; shift 2 ;;
    --repo) REPO="${2:?--repo needs a value}"; shift 2 ;;
    --ci-status-cmd) CI_STATUS_CMD="${2:?--ci-status-cmd needs a value}"; shift 2 ;;
    --skip-ci-check) SKIP_CI=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "error: unknown argument: $1" >&2; exit 2 ;;
  esac
done

# --- Output helpers --------------------------------------------------------

summary() {
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    printf '%s\n' "$*" >> "$GITHUB_STEP_SUMMARY"
  else
    printf '%s\n' "$*" >&2
  fi
}

output() {
  if [ -n "${GITHUB_OUTPUT:-}" ]; then
    printf '%s=%s\n' "$1" "$2" >> "$GITHUB_OUTPUT"
  else
    printf '%s=%s\n' "$1" "$2"
  fi
}

log() { printf '%s\n' "$*" >&2; }

fail() {
  # ::error:: is a workflow annotation; harmless noise outside Actions.
  printf '::error::%s\n' "$1" >&2
  summary "**Failed:** $1"
  output selected false
  exit 1
}

skip() {
  log "$1"
  summary "$1"
  output selected false
  exit 0
}

# ci_conclusion SHA — print the conclusion of the latest "Build & Test" check
# run on SHA, or "missing" when there is none, or "pending" while it runs.
ci_conclusion() {
  local sha="$1"
  if [ -n "$CI_STATUS_CMD" ]; then
    # Word splitting of CI_STATUS_CMD is intended: it is a command with args.
    # shellcheck disable=SC2086
    $CI_STATUS_CMD "$sha"
    return
  fi
  local result
  if ! result="$(gh api -X GET "repos/${REPO}/commits/${sha}/check-runs" \
      -f check_name="$CHECK_NAME" -f filter=latest -F per_page=100 \
      --jq '.check_runs | sort_by(.id) | last | if . == null then "missing" else (.conclusion // "pending") end')"; then
    echo "lookup-error"
    return
  fi
  echo "$result"
}

# --- Release branch ------------------------------------------------------------

BRANCH=""
for ref in $(git for-each-ref --sort=v:refname --format='%(refname)' "refs/remotes/${REMOTE}/release/v*"); do
  name="${ref#refs/remotes/"${REMOTE}"/}"
  if [[ "$name" =~ ^release/v[0-9]+\.[0-9]+$ ]]; then
    BRANCH="$name"
  fi
done
[ -n "$BRANCH" ] || fail "No release/vX.Y branch found on ${REMOTE}. Cut the first preview of a release line with scripts/release/cut-preview.sh."

HEAD_SHA="$(git rev-parse "refs/remotes/${REMOTE}/${BRANCH}^{commit}")"
LINE="${BRANCH#release/v}"   # X.Y
log "Release branch: ${BRANCH} at ${HEAD_SHA}"
output branch "$BRANCH"

summary "## Weekly preview"
summary ""
summary "- Release branch: \`${BRANCH}\` at \`${HEAD_SHA:0:12}\`"

# --- Nightly choice --------------------------------------------------------------

NIGHTLY=""
if [ -n "$NIGHTLY_OVERRIDE" ]; then
  [[ "$NIGHTLY_OVERRIDE" =~ ^nightly-[0-9]{8}$ ]] \
    || fail "nightly_tag '${NIGHTLY_OVERRIDE}' does not match nightly-YYYYMMDD."
  git rev-parse -q --verify "refs/tags/${NIGHTLY_OVERRIDE}" >/dev/null \
    || fail "nightly_tag '${NIGHTLY_OVERRIDE}' does not exist."
  NIGHTLY="$NIGHTLY_OVERRIDE"
  summary "- Nightly: \`${NIGHTLY}\`, given by hand (CI guard not applied)"
else
  CANDIDATES=()
  while IFS= read -r t; do
    [[ "$t" =~ ^nightly-[0-9]{8}$ ]] || continue
    sha="$(git rev-parse "refs/tags/${t}^{commit}")"
    # Only nightlies the release branch does not already contain.
    if git merge-base --is-ancestor "$sha" "$HEAD_SHA"; then
      break
    fi
    CANDIDATES+=("$t")
  done < <(git tag -l --sort=-v:refname 'nightly-*')

  if [ "${#CANDIDATES[@]}" -eq 0 ]; then
    skip "No new nightly since last preview: the newest nightly is already on \`${BRANCH}\`."
  fi

  if [ "$SKIP_CI" = true ]; then
    NIGHTLY="${CANDIDATES[0]}"
    summary "- Nightly: \`${NIGHTLY}\`, the newest nightly (CI guard skipped)"
  else
    summary "- CI guard: newest nightly whose \`${CHECK_NAME}\` check passed"
    for t in "${CANDIDATES[@]}"; do
      sha="$(git rev-parse "refs/tags/${t}^{commit}")"
      conclusion="$(ci_conclusion "$sha")"
      log "${t} (${sha:0:12}): ${CHECK_NAME} ${conclusion}"
      if [ "$conclusion" = "success" ]; then
        NIGHTLY="$t"
        summary "  - \`${t}\` (\`${sha:0:12}\`): ${conclusion}, picked"
        break
      fi
      summary "  - \`${t}\` (\`${sha:0:12}\`): ${conclusion}, skipped"
    done
    if [ -z "$NIGHTLY" ]; then
      printf '::warning::%s\n' "No nightly newer than ${BRANCH} passed ${CHECK_NAME}; no preview this week." >&2
      skip "No nightly newer than \`${BRANCH}\` passed \`${CHECK_NAME}\`; no preview this week."
    fi
  fi
fi

SHA="$(git rev-parse "refs/tags/${NIGHTLY}^{commit}")"
output nightly "$NIGHTLY"
output sha "$SHA"

# --- Skip, or require a fast-forward ---------------------------------------------

if git merge-base --is-ancestor "$SHA" "$HEAD_SHA"; then
  skip "No new nightly since last preview: \`${NIGHTLY}\` is already on \`${BRANCH}\`."
fi

if ! git merge-base --is-ancestor "$HEAD_SHA" "$SHA"; then
  fail "${BRANCH} (${HEAD_SHA:0:12}) is not an ancestor of ${NIGHTLY} (${SHA:0:12}), so it cannot be fast-forwarded. The release branch has commits that are not on main, usually cherry-picks for a freeze before stable. The weekly roll-forward never force-pushes: tag previews by hand with scripts/release/bump-preview.sh until the line is promoted or the branch is realigned."
fi

# --- Next preview tag ------------------------------------------------------------

# Highest patch Z first, then the highest N within that patch, both numeric:
# with v0.3.0-preview.8 and v0.3.1-preview.7 the next tag is v0.3.1-preview.8.
PATCH=-1
HIGHEST=0
for t in $(git tag -l "v${LINE}.*-preview.*"); do
  [[ "$t" =~ ^v${LINE//./\\.}\.([0-9]+)-preview\.([0-9]+)$ ]] || continue
  z="$((10#${BASH_REMATCH[1]}))"
  n="$((10#${BASH_REMATCH[2]}))"
  if [ "$z" -gt "$PATCH" ]; then
    PATCH="$z"
    HIGHEST="$n"
  elif [ "$z" -eq "$PATCH" ] && [ "$n" -gt "$HIGHEST" ]; then
    HIGHEST="$n"
  fi
done

if [ "$PATCH" -lt 0 ]; then
  PATCH=0
fi
BASE="v${LINE}.${PATCH}"
TAG="${BASE}-preview.$((HIGHEST + 1))"

if git rev-parse -q --verify "refs/tags/${BASE}" >/dev/null; then
  fail "${BASE} has already been promoted to stable, so ${BRANCH} takes no more ${BASE} previews. Cut the next release line with scripts/release/cut-preview.sh."
fi
if git rev-parse -q --verify "refs/tags/${TAG}" >/dev/null; then
  fail "Tag ${TAG} already exists."
fi

output tag "$TAG"
output selected true
log "Selected ${NIGHTLY} (${SHA}) -> ${BRANCH}, tag ${TAG}"
summary "- Fast-forward \`${BRANCH}\` from \`${HEAD_SHA:0:12}\` to \`${NIGHTLY}\` (\`${SHA:0:12}\`)"
summary "- Tag: \`${TAG}\`"
