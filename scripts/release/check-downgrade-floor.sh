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

# scripts/release/check-downgrade-floor.sh — Refuse releases dated before DowngradeFloor
#
# Read-only release gate. Reads pkg/version.DowngradeFloor from the release
# tree (or a specified git ref/file) and fails if the floor is dated after the
# release's own UTC date. A release whose DowngradeFloor is later than its own
# date sorts below the floor in pkg/version/update.BelowFloor and is refused as
# a downgrade target (below_downgrade_floor).
#
# Usage: ./scripts/release/check-downgrade-floor.sh [options] TAG
#
# Options:
#   --date DATE        Release cut/publish date in UTC: YYYY-MM-DD, YYYYMMDD,
#                      or UTC RFC3339 (e.g. 2026-11-20T15:18:38Z). Default:
#                      current UTC date. Ignored for nightly-YYYYMMDD[.N] tags,
#                      whose embedded tag date is what BelowFloor compares.
#   --floor-file PATH  Path to downgrade_floor.go (default: pkg/version/downgrade_floor.go).
#   --ref REV          Read --floor-file from git revision REV via `git show`
#                      instead of the working tree.
#   -h, --help         Show this help message.
#
# Exit status:
#   0  DowngradeFloor is empty or dated on/before the release date.
#   1  DowngradeFloor is malformed, missing, or dated after the release date.
#   2  Usage error (bad flags, invalid TAG, or invalid --date).

set -euo pipefail

FLOOR_FILE="pkg/version/downgrade_floor.go"
GIT_REF=""
DATE_OVERRIDE=""

usage() {
  sed -n '16,38p' "$0" | sed 's/^# \{0,1\}//'
}

emit_error() {
  printf '::error::%s\n' "$1" >&2
}

# is_valid_yyyymmdd YYYYMMDD — returns 0 if YYYYMMDD is a real Gregorian date.
is_valid_yyyymmdd() {
  local d="$1"
  [[ "$d" =~ ^[0-9]{8}$ ]] || return 1
  local y=$((10#${d:0:4}))
  local m=$((10#${d:4:2}))
  local day=$((10#${d:6:2}))
  (( m >= 1 && m <= 12 )) || return 1
  local max_day
  case "$m" in
    1|3|5|7|8|10|12) max_day=31 ;;
    4|6|9|11) max_day=30 ;;
    2)
      if (( (y % 4 == 0 && y % 100 != 0) || (y % 400 == 0) )); then
        max_day=29
      else
        max_day=28
      fi
      ;;
  esac
  (( day >= 1 && day <= max_day ))
}

format_date() {
  printf '%s-%s-%s' "${1:0:4}" "${1:4:2}" "${1:6:2}"
}

# parse_utc_date STR — prints YYYYMMDD for a UTC date or timestamp string.
parse_utc_date() {
  local raw="$1"
  local ymd=""
  if [[ "$raw" =~ ^([0-9]{4})([0-9]{2})([0-9]{2})$ ]]; then
    ymd="${BASH_REMATCH[1]}${BASH_REMATCH[2]}${BASH_REMATCH[3]}"
  elif [[ "$raw" =~ ^([0-9]{4})-([0-9]{2})-([0-9]{2})(T([0-9]{2}):([0-9]{2})(:([0-9]{2})(\.[0-9]+)?)?Z)?$ ]]; then
    ymd="${BASH_REMATCH[1]}${BASH_REMATCH[2]}${BASH_REMATCH[3]}"
    if [[ -n "${BASH_REMATCH[4]:-}" ]]; then
      local hh=$((10#${BASH_REMATCH[5]}))
      local mm=$((10#${BASH_REMATCH[6]}))
      local ss=0
      if [[ -n "${BASH_REMATCH[8]:-}" ]]; then
        ss=$((10#${BASH_REMATCH[8]}))
      fi
      if (( hh > 23 || mm > 59 || ss > 59 )); then
        return 1
      fi
    fi
  else
    return 1
  fi
  is_valid_yyyymmdd "$ymd" || return 1
  printf '%s\n' "$ymd"
}

TAG=""
while [ $# -gt 0 ]; do
  case "$1" in
    --date)
      if [ $# -lt 2 ]; then
        echo "error: --date requires a value" >&2
        exit 2
      fi
      DATE_OVERRIDE="$2"
      shift 2
      ;;
    --floor-file)
      if [ $# -lt 2 ]; then
        echo "error: --floor-file requires a value" >&2
        exit 2
      fi
      FLOOR_FILE="$2"
      shift 2
      ;;
    --ref)
      if [ $# -lt 2 ]; then
        echo "error: --ref requires a value" >&2
        exit 2
      fi
      GIT_REF="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    --)
      shift
      break
      ;;
    -*)
      echo "error: unknown option: $1" >&2
      exit 2
      ;;
    *)
      if [ -n "$TAG" ]; then
        echo "error: unexpected extra argument: $1" >&2
        exit 2
      fi
      TAG="$1"
      shift
      ;;
  esac
done

if [ $# -gt 0 ]; then
  if [ -n "$TAG" ] || [ $# -gt 1 ]; then
    echo "error: unexpected extra arguments: $*" >&2
    exit 2
  fi
  TAG="$1"
fi

if [ -z "$TAG" ]; then
  echo "error: missing required TAG argument" >&2
  usage >&2
  exit 2
fi

# Strip refs/tags/ if a full ref was passed.
TAG="${TAG#refs/tags/}"

# Validate TAG and determine the release date (YYYYMMDD) and channel.
NIGHTLY_RE='^nightly-([0-9]{8})(\.([0-9]+))?$'
SEMVER_RE='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(preview|rc)\.(0|[1-9][0-9]*))?$'

IS_NIGHTLY=false
TAG_SEQ=0
RELEASE_DATE=""

if [ -n "$DATE_OVERRIDE" ]; then
  if ! PARSED_OVERRIDE=$(parse_utc_date "$DATE_OVERRIDE"); then
    echo "error: invalid --date '${DATE_OVERRIDE}' (expected YYYY-MM-DD, YYYYMMDD, or UTC RFC3339 ending in Z)" >&2
    exit 2
  fi
fi

if [[ "$TAG" =~ $NIGHTLY_RE ]]; then
  IS_NIGHTLY=true
  RELEASE_DATE="${BASH_REMATCH[1]}"
  TAG_SEQ_RAW="${BASH_REMATCH[3]:-}"
  if ! is_valid_yyyymmdd "$RELEASE_DATE"; then
    echo "error: nightly tag '${TAG}' does not contain a valid calendar date" >&2
    exit 2
  fi
  if [[ -n "$TAG_SEQ_RAW" ]]; then
    TAG_SEQ=$((10#$TAG_SEQ_RAW))
  fi
elif [[ "$TAG" =~ $SEMVER_RE ]]; then
  if [ -n "$DATE_OVERRIDE" ]; then
    RELEASE_DATE="$PARSED_OVERRIDE"
  else
    RELEASE_DATE="$(date -u +%Y%m%d)"
  fi
else
  echo "error: '${TAG}' is not a recognized release tag (expected vX.Y.Z, vX.Y.Z-preview.N, vX.Y.Z-rc.N, or nightly-YYYYMMDD[.N])" >&2
  exit 2
fi

# Read the floor file contents.
SOURCE_DESC="$FLOOR_FILE"
if [ -n "$GIT_REF" ]; then
  SOURCE_DESC="${GIT_REF}:${FLOOR_FILE}"
  if ! FLOOR_CONTENT="$(git show "${GIT_REF}:${FLOOR_FILE}" 2>/dev/null)"; then
    emit_error "Could not read '${FLOOR_FILE}' at git ref '${GIT_REF}'."
    exit 1
  fi
else
  if [ ! -f "$FLOOR_FILE" ]; then
    emit_error "DowngradeFloor source file not found: '${FLOOR_FILE}'."
    exit 1
  fi
  FLOOR_CONTENT="$(cat "$FLOOR_FILE")"
fi

# Extract `const DowngradeFloor [string] = "..."` from the Go file.
FLOOR_DECL_RE='^[[:space:]]*const[[:space:]]+DowngradeFloor([[:space:]]+string)?[[:space:]]*=[[:space:]]*"([^"]*)"([[:space:]]*//.*)?[[:space:]]*$'
FLOOR=""
MATCH_COUNT=0
while IFS= read -r line; do
  if [[ "$line" =~ $FLOOR_DECL_RE ]]; then
    FLOOR="${BASH_REMATCH[2]}"
    MATCH_COUNT=$((MATCH_COUNT + 1))
  fi
done <<< "$FLOOR_CONTENT"

if [ "$MATCH_COUNT" -eq 0 ]; then
  emit_error "Could not find 'const DowngradeFloor = \"...\"' in ${SOURCE_DESC}."
  exit 1
fi
if [ "$MATCH_COUNT" -gt 1 ]; then
  emit_error "Found multiple DowngradeFloor declarations in ${SOURCE_DESC}."
  exit 1
fi

if [ -z "$FLOOR" ]; then
  echo "DowngradeFloor check passed: DowngradeFloor is empty (no floor) in ${SOURCE_DESC}; release '${TAG}' is allowed."
  exit 0
fi

# DowngradeFloor must be a nightly-YYYYMMDD[.N] tag so cross-channel BelowFloor
# comparisons have an embedded floor date.
if [[ ! "$FLOOR" =~ $NIGHTLY_RE ]]; then
  emit_error "Invalid pkg/version.DowngradeFloor '${FLOOR}' in ${SOURCE_DESC}: must be a nightly-YYYYMMDD tag so cross-channel BelowFloor comparisons have a floor date."
  exit 1
fi

FLOOR_DATE="${BASH_REMATCH[1]}"
FLOOR_SEQ=0
FLOOR_SEQ_RAW="${BASH_REMATCH[3]:-}"
if [[ -n "$FLOOR_SEQ_RAW" ]]; then
  FLOOR_SEQ=$((10#$FLOOR_SEQ_RAW))
fi

if ! is_valid_yyyymmdd "$FLOOR_DATE"; then
  emit_error "Invalid pkg/version.DowngradeFloor '${FLOOR}' in ${SOURCE_DESC}: '${FLOOR_DATE}' is not a valid calendar date."
  exit 1
fi

RELEASE_FMT="$(format_date "$RELEASE_DATE")"
FLOOR_FMT="$(format_date "$FLOOR_DATE")"

if [[ "$FLOOR_DATE" > "$RELEASE_DATE" ]]; then
  emit_error "Refusing to cut release '${TAG}' (UTC date ${RELEASE_FMT}): pkg/version.DowngradeFloor is '${FLOOR}' (${FLOOR_FMT}), which is dated after the release."
  printf 'A release whose DowngradeFloor is later than its own date sorts below the floor and is refused as a downgrade target (below_downgrade_floor).\n' >&2
  printf 'Set pkg/version.DowngradeFloor to nightly-%s (or earlier) before cutting %s.\n' "$RELEASE_DATE" "$TAG" >&2
  exit 1
fi

if [ "$IS_NIGHTLY" = true ] && [ "$FLOOR_DATE" = "$RELEASE_DATE" ] && (( FLOOR_SEQ > TAG_SEQ )); then
  emit_error "Refusing to cut release '${TAG}': pkg/version.DowngradeFloor is '${FLOOR}', which sorts after '${TAG}'."
  printf 'Set pkg/version.DowngradeFloor to %s (or earlier) before cutting %s.\n' "$TAG" "$TAG" >&2
  exit 1
fi

echo "DowngradeFloor check passed: release '${TAG}' (UTC date ${RELEASE_FMT}) is on or after DowngradeFloor '${FLOOR}' (${FLOOR_FMT})."
