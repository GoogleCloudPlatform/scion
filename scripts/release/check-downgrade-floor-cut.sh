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
# scripts/release/check-downgrade-floor-cut.sh — DowngradeFloor vs the nightly cut
#
# Read-only CI guard (ptone/scion#9831). When a commit changes
# pkg/version.DowngradeFloor, the floor must name the first nightly that will
# contain the commit: nightly-<UTC date of the next scheduled build-nightly.yml
# run at or after the commit lands on main>. A later floor makes that nightly
# refuse to tag (check-downgrade-floor.sh); an earlier floor lets a nightly
# without the migration through as a downgrade target.
#
# The cut time comes from the single daily cron in build-nightly.yml. GitHub
# starts scheduled runs late, so a landing time within --grace-minutes after
# the cut accepts either that day's or the next day's nightly.
#
# Usage: ./scripts/release/check-downgrade-floor-cut.sh [options]
#
# Options:
#   --base REV           Revision to compare the floor against (default HEAD^1).
#   --head REV           Revision under test (default HEAD).
#   --at TIME            Landing time, UTC RFC3339 (e.g. 2026-11-27T23:02:00Z).
#                        Default: the committer date of --head with --landed,
#                        else the current time (a pull request lands no
#                        earlier than now).
#   --landed             --head is already on main (push event).
#   --workflow PATH      Nightly workflow, read from --head
#                        (default .github/workflows/build-nightly.yml).
#   --floor-file PATH    Default pkg/version/downgrade_floor.go.
#   --grace-minutes N    Default 60.
#   -h, --help           Show this help message.
#
# Exit status:
#   0  The floor is unchanged, empty, or names an accepted nightly.
#   1  The floor names the wrong nightly, or a file could not be parsed.
#   2  Usage error.

set -euo pipefail

BASE_REV="HEAD^1"
HEAD_REV="HEAD"
AT=""
LANDED=false
WORKFLOW=".github/workflows/build-nightly.yml"
FLOOR_FILE="pkg/version/downgrade_floor.go"
GRACE_MINUTES=60

usage() {
  sed -n '16,47p' "$0" | sed 's/^# \{0,1\}//'
}

emit_error() {
  printf '::error::%s\n' "$1" >&2
}

need_value() {
  if [ "$2" -lt 2 ]; then
    echo "error: $1 requires a value" >&2
    exit 2
  fi
}

while [ $# -gt 0 ]; do
  case "$1" in
    --base) need_value "$1" $#; BASE_REV="$2"; shift 2 ;;
    --head) need_value "$1" $#; HEAD_REV="$2"; shift 2 ;;
    --at) need_value "$1" $#; AT="$2"; shift 2 ;;
    --landed) LANDED=true; shift ;;
    --workflow) need_value "$1" $#; WORKFLOW="$2"; shift 2 ;;
    --floor-file) need_value "$1" $#; FLOOR_FILE="$2"; shift 2 ;;
    --grace-minutes) need_value "$1" $#; GRACE_MINUTES="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "error: unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [[ ! "$GRACE_MINUTES" =~ ^[0-9]+$ ]] || (( 10#$GRACE_MINUTES >= 1440 )); then
  echo "error: --grace-minutes must be an integer from 0 to 1439" >&2
  exit 2
fi
GRACE_MINUTES=$((10#$GRACE_MINUTES))

# --- Calendar arithmetic (pure bash; no GNU/BSD date differences) ------------

# days_from_civil Y M D — days since 1970-01-01 (Howard Hinnant's algorithm).
days_from_civil() {
  local y=$1 m=$2 d=$3
  (( m <= 2 )) && y=$((y - 1))
  local era=$(( (y >= 0 ? y : y - 399) / 400 ))
  local yoe=$(( y - era * 400 ))
  local mp=$(( m > 2 ? m - 3 : m + 9 ))
  local doy=$(( (153 * mp + 2) / 5 + d - 1 ))
  local doe=$(( yoe * 365 + yoe / 4 - yoe / 100 + doy ))
  echo $(( era * 146097 + doe - 719468 ))
}

# civil_from_days N — prints YYYYMMDD for N days since 1970-01-01.
civil_from_days() {
  local z=$(( $1 + 719468 ))
  local era=$(( (z >= 0 ? z : z - 146096) / 146097 ))
  local doe=$(( z - era * 146097 ))
  local yoe=$(( (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365 ))
  local y=$(( yoe + era * 400 ))
  local doy=$(( doe - (365 * yoe + yoe / 4 - yoe / 100) ))
  local mp=$(( (5 * doy + 2) / 153 ))
  local d=$(( doy - (153 * mp + 2) / 5 + 1 ))
  local m=$(( mp < 10 ? mp + 3 : mp - 9 ))
  (( m <= 2 )) && y=$((y + 1))
  printf '%04d%02d%02d\n' "$y" "$m" "$d"
}

format_date() {
  printf '%s-%s-%s' "${1:0:4}" "${1:4:2}" "${1:6:2}"
}

# --- Landing time -------------------------------------------------------------

if [ -z "$AT" ]; then
  if [ "$LANDED" = true ]; then
    if ! AT="$(TZ=UTC git log -1 --format=%cd --date=format-local:%Y-%m-%dT%H:%M:%SZ "$HEAD_REV" 2>/dev/null)"; then
      emit_error "Could not read the committer date of '${HEAD_REV}'."
      exit 1
    fi
  else
    AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  fi
fi

AT_RE='^([0-9]{4})-([0-9]{2})-([0-9]{2})T([0-9]{2}):([0-9]{2})(:([0-9]{2})(\.[0-9]+)?)?Z$'
if [[ ! "$AT" =~ $AT_RE ]]; then
  echo "error: invalid --at '${AT}' (expected UTC RFC3339 ending in Z)" >&2
  exit 2
fi
AT_Y=$((10#${BASH_REMATCH[1]})); AT_M=$((10#${BASH_REMATCH[2]})); AT_D=$((10#${BASH_REMATCH[3]}))
AT_H=$((10#${BASH_REMATCH[4]})); AT_MIN=$((10#${BASH_REMATCH[5]}))
if (( AT_M < 1 || AT_M > 12 || AT_D < 1 || AT_D > 31 || AT_H > 23 || AT_MIN > 59 )); then
  echo "error: invalid --at '${AT}'" >&2
  exit 2
fi
AT_DAYS=$(days_from_civil "$AT_Y" "$AT_M" "$AT_D")
if [ "$(civil_from_days "$AT_DAYS")" != "$(printf '%04d%02d%02d' "$AT_Y" "$AT_M" "$AT_D")" ]; then
  echo "error: invalid calendar date in --at '${AT}'" >&2
  exit 2
fi
AT_MINUTE_OF_DAY=$(( AT_H * 60 + AT_MIN ))

# --- Floor values -------------------------------------------------------------

FLOOR_DECL_RE='^[[:space:]]*const[[:space:]]+DowngradeFloor([[:space:]]+string)?[[:space:]]*=[[:space:]]*"([^"]*)"([[:space:]]*//.*)?[[:space:]]*$'

# read_floor REV — prints the floor at REV; "<absent>" when the file is missing.
read_floor() {
  local content line value="" count=0
  if ! content="$(git show "$1:${FLOOR_FILE}" 2>/dev/null)"; then
    echo "<absent>"
    return 0
  fi
  while IFS= read -r line; do
    if [[ "$line" =~ $FLOOR_DECL_RE ]]; then
      value="${BASH_REMATCH[2]}"
      count=$((count + 1))
    fi
  done <<< "$content"
  if [ "$count" -ne 1 ]; then
    emit_error "Expected exactly one 'const DowngradeFloor = \"...\"' in $1:${FLOOR_FILE}, found ${count}."
    return 1
  fi
  printf '%s\n' "$value"
}

if ! git rev-parse -q --verify "${HEAD_REV}^{commit}" >/dev/null; then
  emit_error "Unknown --head revision '${HEAD_REV}'."
  exit 1
fi
if ! git rev-parse -q --verify "${BASE_REV}^{commit}" >/dev/null; then
  emit_error "Unknown --base revision '${BASE_REV}' (fetch at least two commits of history)."
  exit 1
fi

HEAD_FLOOR="$(read_floor "$HEAD_REV")" || exit 1
BASE_FLOOR="$(read_floor "$BASE_REV")" || exit 1

if [ "$HEAD_FLOOR" = "$BASE_FLOOR" ]; then
  echo "DowngradeFloor unchanged ('${HEAD_FLOOR}'); nothing to check."
  exit 0
fi
if [ "$HEAD_FLOOR" = "<absent>" ] || [ -z "$HEAD_FLOOR" ]; then
  echo "DowngradeFloor removed or emptied (was '${BASE_FLOOR}'); nothing to check."
  exit 0
fi

NIGHTLY_RE='^nightly-([0-9]{8})(\.[0-9]+)?$'
if [[ ! "$HEAD_FLOOR" =~ $NIGHTLY_RE ]]; then
  emit_error "Invalid pkg/version.DowngradeFloor '${HEAD_FLOOR}': must be a nightly-YYYYMMDD tag."
  exit 1
fi
FLOOR_DATE="${BASH_REMATCH[1]}"

# --- Nightly cut time from the workflow cron -----------------------------------

if ! WORKFLOW_CONTENT="$(git show "${HEAD_REV}:${WORKFLOW}" 2>/dev/null)"; then
  emit_error "Could not read '${WORKFLOW}' at '${HEAD_REV}'."
  exit 1
fi
CRON_RE="^[[:space:]]*-[[:space:]]*cron:[[:space:]]*['\"]([^'\"]*)['\"]"
CRONS=()
while IFS= read -r line; do
  if [[ "$line" =~ $CRON_RE ]]; then
    CRONS+=("${BASH_REMATCH[1]}")
  fi
done <<< "$WORKFLOW_CONTENT"
if [ "${#CRONS[@]}" -ne 1 ]; then
  emit_error "Expected exactly one cron schedule in ${WORKFLOW}, found ${#CRONS[@]}; update check-downgrade-floor-cut.sh."
  exit 1
fi
DAILY_RE='^([0-9]{1,2})[[:space:]]+([0-9]{1,2})[[:space:]]+\*[[:space:]]+\*[[:space:]]+\*$'
if [[ ! "${CRONS[0]}" =~ $DAILY_RE ]] || (( 10#${BASH_REMATCH[1]} > 59 || 10#${BASH_REMATCH[2]} > 23 )); then
  emit_error "Cron '${CRONS[0]}' in ${WORKFLOW} is not a single daily 'M H * * *' schedule; update check-downgrade-floor-cut.sh."
  exit 1
fi
CUT_MINUTE_OF_DAY=$(( 10#${BASH_REMATCH[2]} * 60 + 10#${BASH_REMATCH[1]} ))
CUT_HHMM="$(printf '%02d:%02d' $((CUT_MINUTE_OF_DAY / 60)) $((CUT_MINUTE_OF_DAY % 60)))"

# --- Accepted nightly dates ----------------------------------------------------
# The landing day's cut is the first nightly to contain the commit when it
# lands at or before the cut. Within the grace window after the cut, the
# delayed run may or may not include it. The window may cross midnight.

TODAY="$(civil_from_days "$AT_DAYS")"
TOMORROW="$(civil_from_days $((AT_DAYS + 1)))"
YESTERDAY="$(civil_from_days $((AT_DAYS - 1)))"
ACCEPTED=()
if (( AT_MINUTE_OF_DAY <= CUT_MINUTE_OF_DAY )); then
  ACCEPTED=("$TODAY")
  # Still inside yesterday's cut window when that window crosses midnight.
  if (( AT_MINUTE_OF_DAY + 1440 - CUT_MINUTE_OF_DAY <= GRACE_MINUTES )); then
    ACCEPTED=("$YESTERDAY" "$TODAY")
  fi
elif (( AT_MINUTE_OF_DAY - CUT_MINUTE_OF_DAY <= GRACE_MINUTES )); then
  ACCEPTED=("$TODAY" "$TOMORROW")
else
  ACCEPTED=("$TOMORROW")
fi

for d in "${ACCEPTED[@]}"; do
  if [ "$FLOOR_DATE" = "$d" ]; then
    echo "DowngradeFloor check passed: '${HEAD_FLOOR}' is the first nightly to contain a commit landing at ${AT} (cut ${CUT_HHMM} UTC, grace ${GRACE_MINUTES} min)."
    exit 0
  fi
done

WANT=""
for d in "${ACCEPTED[@]}"; do
  WANT="${WANT:+${WANT} or }nightly-${d}"
done
if [ "$LANDED" = true ]; then
  WHEN="landed at ${AT}"
else
  WHEN="landing now (${AT})"
fi
if [[ "$FLOOR_DATE" > "${ACCEPTED[${#ACCEPTED[@]}-1]}" ]]; then
  emit_error "pkg/version.DowngradeFloor '${HEAD_FLOOR}' ($(format_date "$FLOOR_DATE")) is later than the first nightly to contain this commit, ${WHEN} (cut ${CUT_HHMM} UTC): set it to ${WANT}. Otherwise that nightly refuses to tag."
else
  emit_error "pkg/version.DowngradeFloor '${HEAD_FLOOR}' ($(format_date "$FLOOR_DATE")) is earlier than the first nightly to contain this commit, ${WHEN} (cut ${CUT_HHMM} UTC): set it to ${WANT}. Otherwise a nightly without the migration stays an allowed downgrade target."
fi
if [ "$LANDED" = false ]; then
  printf 'If this pull request will merge later, re-run the check after updating the floor for the expected merge time.\n' >&2
fi
exit 1
