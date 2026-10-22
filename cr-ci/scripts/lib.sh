# shellcheck shell=bash
# Variables set here are used by the scripts that source this file.
# shellcheck disable=SC2034
# lib.sh: shared helpers for the cr-ci deploy scripts. Source it; do not run it.
#
#   - crci_parse_common_arg / crci_finish_args: --config FILE, --dry-run (the
#     default), --apply, -h/--help.
#   - crci_load_config FILE: parses config.env line by line (KEY=value; never
#     sourced, so values may contain spaces and nothing in them runs), rejects
#     unknown and duplicate keys, fills defaults from config.example.env (the
#     single contract), and validates the same rules as mediator/config.go
#     (kept in lockstep by a parity test in mediator/config_test.go). Values
#     land in the associative array CFG.
#   - crci_run CMD...: prints "+ <command>" (shell-quoted) and, with --apply
#     only, runs it. A failure stops the script and names the step.
#   - crci_exists CMD...: a read-only describe used for idempotency. Prints
#     "? <command>". Dry-run assumes nothing exists (returns 1). With --apply it
#     returns 0 if the resource exists, 1 if the error is a not-found error (see
#     crci_is_not_found), and stops on any other error. A PERMISSION_DENIED is
#     never taken as "missing", even when gcloud adds "(or it may not exist)".
#   - crci_read CMD...: a read-only query whose output the script needs (apply
#     only; prints "? <command>" and the placeholder in dry-run).
#   - crci_require_tool NAME MIN: version check, apply mode only.
#
# No function here ever prints or passes a secret value: secrets reach gcloud
# only on stdin (see set-secrets.sh).

if ((BASH_VERSINFO[0] < 4)); then
  echo "cr-ci scripts need bash >= 4 (macOS: brew install bash)" >&2
  exit 1
fi

CRCI_SCRIPTS_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
CRCI_ROOT=$(cd "$CRCI_SCRIPTS_DIR/.." && pwd)
CRCI_EXAMPLE="$CRCI_ROOT/config.example.env"
CRCI_PROG=$(basename "$0")
CRCI_MODE=dry-run
CRCI_CONFIG="$CRCI_ROOT/config.env"
CRCI_SHIFT=0
CRCI_HELP=0

# Minimum tool versions for --apply. UNVERIFIED: the oldest gcloud with every
# flag used here (--no-invoker-iam-check, ephemeral-disk volumes on jobs) has
# not been pinned down; this floor is a recent release (permissions-matrix U8).
CRCI_MIN_GCLOUD=530.0.0
CRCI_MIN_DOCKER=20.10.0
CRCI_MIN_GH=2.40.0

declare -gA CFG=()
declare -ga CRCI_KEYS=()

crci_die() {
  printf '%s: error: %s\n' "$CRCI_PROG" "$*" >&2
  exit 1
}

crci_note() { printf '# %s\n' "$*"; }

# crci_quote ARG...: POSIX shell quoting; plain words stay bare.
crci_quote() {
  local out="" a q
  for a in "$@"; do
    if [[ -n $a && $a =~ ^[A-Za-z0-9_./:=@%+,^-]+$ ]]; then
      q=$a
    else
      q="'${a//\'/\'\\\'\'}'"
    fi
    out+="${out:+ }$q"
  done
  printf '%s' "$out"
}

crci_run() {
  printf '+ %s\n' "$(crci_quote "$@")"
  if [[ $CRCI_MODE == apply ]]; then
    "$@" || crci_die "step failed (rc $?): $(crci_quote "$@")"
  fi
}

# crci_is_not_found ERRTEXT: true if a gcloud error says the target does not
# exist. Recognised (and nothing else):
#   - "PERMISSION_DENIED" or "denied" anywhere: never not-found (checked first);
#   - "NOT_FOUND" (API status; secrets, scheduler, service accounts, IAM);
#   - "Cannot find job [" / "Cannot find service [" (gcloud run describe,
#     which raises its own error instead of the API status);
#   - "not found" (Artifact Registry images and repositories, Cloud Storage,
#     "Policy binding ... not found!" from remove-iam-policy-binding);
#   - "does not exist" (an IAM binding on a deleted account).
crci_is_not_found() {
  grep -qE 'PERMISSION_DENIED|[Dd]enied' <<<"$1" && return 1
  grep -qE 'NOT_FOUND|Cannot find (job|service) \[|[Nn]ot found|does not exist' <<<"$1"
}

# crci_run_unless_missing CMD...: like crci_run, but a failure whose error
# says the target is already gone (crci_is_not_found) is a no-op, so removals stay idempotent. Other failures stop the script.
crci_run_unless_missing() {
  printf '+ %s\n' "$(crci_quote "$@")"
  [[ $CRCI_MODE == apply ]] || return 0
  local err rc
  if err=$("$@" 2>&1 >/dev/null); then
    return 0
  else
    rc=$?
  fi
  if crci_is_not_found "$err"; then
    crci_note "already absent"
    return 0
  fi
  printf '%s\n' "$err" >&2
  crci_die "step failed (rc $rc): $(crci_quote "$@")"
}

crci_exists() {
  printf '? %s\n' "$(crci_quote "$@")"
  [[ $CRCI_MODE == apply ]] || return 1
  local err rc
  if err=$("$@" 2>&1 >/dev/null); then
    return 0
  else
    rc=$?
  fi
  if crci_is_not_found "$err"; then
    return 1
  fi
  printf '%s\n' "$err" >&2
  crci_die "check failed (rc $rc), stopping (permission denied or other error): $(crci_quote "$@")"
}

# crci_read VAR PLACEHOLDER CMD...: runs a read-only query (apply) and stores
# its stdout in VAR; in dry-run VAR gets PLACEHOLDER.
crci_read() {
  local __var=$1 placeholder=$2 out
  shift 2
  printf '? %s\n' "$(crci_quote "$@")"
  if [[ $CRCI_MODE != apply ]]; then
    printf -v "$__var" '%s' "$placeholder"
    return 0
  fi
  out=$("$@") || crci_die "query failed (rc $?): $(crci_quote "$@")"
  printf -v "$__var" '%s' "$out"
}

# ---- arguments -----------------------------------------------------------------

# crci_parse_common_arg "$@": consumes one common flag and sets CRCI_SHIFT, or
# dies on an unknown argument.
crci_parse_common_arg() {
  case $1 in
    --config)
      [[ $# -ge 2 ]] || crci_die "--config needs a file"
      CRCI_CONFIG=$2
      CRCI_SHIFT=2
      ;;
    --config=*)
      CRCI_CONFIG=${1#--config=}
      CRCI_SHIFT=1
      ;;
    --dry-run)
      CRCI_MODE=dry-run
      CRCI_SHIFT=1
      ;;
    --apply)
      CRCI_MODE=apply
      CRCI_SHIFT=1
      ;;
    -h | --help)
      CRCI_HELP=1
      CRCI_SHIFT=1
      ;;
    *) crci_die "unknown argument: $1 (see --help)" ;;
  esac
}

# crci_need_value FLAG "$@": dies unless FLAG has a value.
crci_need_value() {
  [[ $# -ge 3 && -n $3 ]] || crci_die "$2 needs a value"
}

# crci_header TEXT: prints the usage text if --help was given and exits.
crci_help_exit() {
  if [[ $CRCI_HELP == 1 ]]; then
    printf '%s\n' "$1"
    exit 0
  fi
}

crci_common_help="Common options:
  --config FILE  config file (default: cr-ci/config.env, a filled-in copy of config.example.env)
  --dry-run      print every command instead of running it (the default)
  --apply        run the commands against GCP (never used in tests)
  -h, --help     this help"

# crci_start: loads the config, checks tools in apply mode, prints the header
# and moves to the module root, so printed paths are relative and stable.
crci_start() {
  crci_load_config "$CRCI_CONFIG"
  if [[ $CRCI_MODE == apply ]]; then
    crci_require_tool gcloud "$CRCI_MIN_GCLOUD"
  fi
  cd "$CRCI_ROOT" || crci_die "cannot cd to $CRCI_ROOT"
  crci_note "cr-ci $CRCI_PROG ($CRCI_MODE) project=${CFG[GCP_PROJECT]} region=${CFG[REGION]}"
  if [[ $CRCI_MODE != apply ]]; then
    crci_note "dry-run: nothing is changed; '?' lines are read-only checks, assumed to find nothing"
  fi
}

# ---- tool versions -----------------------------------------------------------------

# crci_version_ge A B: true if dotted version A >= B.
crci_version_ge() {
  local IFS=. i
  local -a a b
  read -r -a a <<<"$1"
  read -r -a b <<<"$2"
  for ((i = 0; i < 3; i++)); do
    local x=${a[i]:-0} y=${b[i]:-0}
    x=${x%%[!0-9]*} y=${y%%[!0-9]*}
    ((10#${x:-0} > 10#${y:-0})) && return 0
    ((10#${x:-0} < 10#${y:-0})) && return 1
  done
  return 0
}

crci_require_tool() {
  local tool=$1 min=$2 v=""
  command -v "$tool" >/dev/null 2>&1 || crci_die "$tool not found on PATH"
  case $tool in
    gcloud) v=$(gcloud --version 2>/dev/null | sed -n 's/^Google Cloud SDK \([0-9.]*\).*/\1/p' | head -n1) ;;
    docker) v=$(docker version --format '{{.Client.Version}}' 2>/dev/null) ;;
    gh) v=$(gh --version 2>/dev/null | sed -n 's/^gh version \([0-9.]*\).*/\1/p' | head -n1) ;;
    *) crci_die "no version check for $tool" ;;
  esac
  [[ -n $v ]] || crci_die "cannot read the $tool version"
  crci_version_ge "$v" "$min" || crci_die "$tool $v is older than the required $min"
}

# ---- config ----------------------------------------------------------------------

# crci_parse_env_file FILE CALLBACK: calls CALLBACK KEY VALUE LINENO for every
# KEY=value line. Blank lines and lines starting with # are skipped. Nothing
# is expanded or executed.
crci_parse_env_file() {
  local file=$1 cb=$2 line n=0 key value
  [[ -f $file && -r $file ]] || crci_die "cannot read config file $file"
  while IFS= read -r line || [[ -n $line ]]; do
    n=$((n + 1))
    line=${line%$'\r'}
    [[ $line =~ ^[[:space:]]*$ || $line =~ ^[[:space:]]*# ]] && continue
    if [[ $line =~ ^([A-Z][A-Z0-9_]*)=(.*)$ ]]; then
      key=${BASH_REMATCH[1]}
      value=${BASH_REMATCH[2]}
      value=${value#"${value%%[![:space:]]*}"}
      value=${value%"${value##*[![:space:]]}"}
      "$cb" "$key" "$value" "$n"
    else
      crci_die "$file:$n: not a KEY=value line"
    fi
  done <"$file"
}

declare -gA CRCI_DEFAULTS=()
declare -gA CRCI_SEEN=()

_crci_example_cb() {
  CRCI_KEYS+=("$1")
  CRCI_DEFAULTS[$1]=$2
}

_crci_config_cb() {
  local key=$1 value=$2 n=$3
  [[ -v CRCI_DEFAULTS[$key] ]] || crci_die "$CRCI_CONFIG:$n: unknown key $key (not in config.example.env)"
  [[ -v CRCI_SEEN[$key] ]] && crci_die "$CRCI_CONFIG:$n: $key is set twice"
  CRCI_SEEN[$key]=1
  [[ -n $value ]] && CFG[$key]=$value
  return 0
}

crci_load_config() {
  CRCI_CONFIG=$1
  CRCI_KEYS=()
  CRCI_DEFAULTS=()
  CRCI_SEEN=()
  CFG=()
  crci_parse_env_file "$CRCI_EXAMPLE" _crci_example_cb
  local k
  for k in "${CRCI_KEYS[@]}"; do
    CFG[$k]=${CRCI_DEFAULTS[$k]}
  done
  crci_parse_env_file "$CRCI_CONFIG" _crci_config_cb
  crci_validate_config || exit 1
}

# ---- validation (mirrors mediator/config.go) ---------------------------------------

declare -ga CRCI_ERRORS=()
_crci_fail() { CRCI_ERRORS+=("$1: $2"); }

_crci_match() { # KEY ERE WHAT
  local v=${CFG[$1]}
  if [[ -n $v ]] && ! [[ $v =~ $2 ]]; then
    _crci_fail "$1" "\"$v\" is not a valid $3"
  fi
}

_crci_required() {
  [[ -n ${CFG[$1]} ]] || _crci_fail "$1" "required"
}

# _crci_int VALUE: prints the value of a Go strconv-style int64 (optional +,
# leading zeros) and returns 0, or returns 1.
_crci_int() {
  local v=$1
  [[ $v =~ ^\+?[0-9]+$ ]] || return 1
  v=${v#+}
  v=${v#"${v%%[!0]*}"}
  [[ -z $v ]] && v=0
  ((${#v} > 19)) && return 1
  if ((${#v} == 19)); then # compare with 9223372036854775807 without overflow
    ((10#${v:0:18} > 922337203685477580)) && return 1
    ((10#${v:0:18} == 922337203685477580 && ${v:18} > 7)) && return 1
  fi
  printf '%s' "$v"
}

_crci_posint() { # KEY -> prints value or empty
  local n
  if n=$(_crci_int "${CFG[$1]}") && ((n >= 1)); then
    printf '%s' "$n"
  else
    _crci_fail "$1" "\"${CFG[$1]}\" is not a positive integer"
  fi
}

# _crci_duration_ms VALUE: Go time.ParseDuration subset (units ns us µs ms s m
# h, decimal fractions, optional sign) in whole milliseconds; returns 1 if the
# value is not a duration.
_crci_duration_ms() {
  local s=$1 sign="" total=0 num unit
  [[ $s == 0 ]] && {
    printf '0'
    return 0
  }
  [[ $s == [+-]* ]] && {
    sign=${s:0:1}
    s=${s:1}
  }
  [[ -n $s ]] || return 1
  local -a parts=()
  while [[ -n $s ]]; do
    if [[ $s =~ ^([0-9]*(\.[0-9]*)?)(ns|us|µs|ms|s|m|h)(.*)$ ]]; then
      num=${BASH_REMATCH[1]}
      unit=${BASH_REMATCH[3]}
      s=${BASH_REMATCH[4]}
      [[ $num =~ [0-9] ]] || return 1
      parts+=("$num" "$unit")
    else
      return 1
    fi
  done
  total=$(awk -v sign="$sign" 'BEGIN {
      t = 0
      for (i = 1; i < ARGC; i += 2) {
        n = ARGV[i] + 0; u = ARGV[i + 1]
        if (u == "ns") f = 1e-6; else if (u == "us" || u == "µs") f = 1e-3
        else if (u == "ms") f = 1; else if (u == "s") f = 1e3
        else if (u == "m") f = 6e4; else f = 3.6e6
        t += n * f
      }
      if (sign == "-") t = -t
      printf "%.0f", t
      exit
    }' "${parts[@]}")
  printf '%s' "$total"
}

# crci_validate_config: appends every problem to CRCI_ERRORS, prints them and
# returns 1 if there are any.
crci_validate_config() {
  CRCI_ERRORS=()
  local k v n

  # TARGET_REPOS
  _crci_required TARGET_REPOS
  local -a owners=()
  if [[ -n ${CFG[TARGET_REPOS]} ]]; then
    local raw=${CFG[TARGET_REPOS]} part owner name
    local -A seen=()
    local IFS=,
    local -a entries
    read -r -a entries <<<"$raw,"
    unset IFS
    # read -a drops a trailing empty field only; an inner one stays empty.
    for part in "${entries[@]}"; do
      part=${part#"${part%%[![:space:]]*}"}
      part=${part%"${part##*[![:space:]]}"}
      if [[ -z $part ]]; then
        _crci_fail TARGET_REPOS "empty entry in \"$raw\""
        continue
      fi
      owner=${part%%/*}
      name=${part#*/}
      if [[ $part != */* || ${#owner} -gt 39 ]] ||
        ! [[ $owner =~ ^[A-Za-z0-9](-?[A-Za-z0-9])*$ ]] ||
        ! [[ $name =~ ^[A-Za-z0-9._-]{1,100}$ ]] || [[ $name == . || $name == .. ]]; then
        _crci_fail TARGET_REPOS "\"$part\" is not owner/repo"
        continue
      fi
      k=${part,,}
      if [[ -v seen[$k] ]]; then
        _crci_fail TARGET_REPOS "\"$part\" is listed twice"
        continue
      fi
      seen[$k]=1
      owners+=("$owner")
    done
  fi

  _crci_required GITHUB_APP_ID
  if [[ -n ${CFG[GITHUB_APP_ID]} ]]; then
    if ! n=$(_crci_int "${CFG[GITHUB_APP_ID]}") || ((n < 1)); then
      _crci_fail GITHUB_APP_ID "\"${CFG[GITHUB_APP_ID]}\" is not a positive integer"
    fi
  fi

  v=${CFG[GITHUB_APP_INSTALLATION_SCOPE]}
  if [[ $v != repo && $v != org ]]; then
    _crci_fail GITHUB_APP_INSTALLATION_SCOPE "\"$v\" must be \"repo\" or \"org\""
  fi
  if [[ $v == org && ${#owners[@]} -gt 0 ]]; then
    for k in "${owners[@]:1}"; do
      [[ ${k,,} == "${owners[0],,}" ]] ||
        _crci_fail GITHUB_APP_INSTALLATION_SCOPE "org scope serves a single org, but owner \"$k\" is not \"${owners[0]}\""
    done
  fi

  _crci_required GCP_PROJECT
  _crci_match GCP_PROJECT '^[a-z][a-z0-9-]{4,28}[a-z0-9]$' "GCP project ID"
  _crci_required REGION
  _crci_match REGION '^[a-z]+-[a-z]+[0-9]+$' "GCP region"

  _crci_match RUNNER_LABEL '^[A-Za-z0-9._-]{1,64}$' "runner label"
  case ${CFG[RUNNER_LABEL],,} in
    self-hosted | linux | x64 | arm64) _crci_fail RUNNER_LABEL "\"${CFG[RUNNER_LABEL]}\" is a default self-hosted runner label; use a unique label" ;;
  esac
  local cloudrun='^[a-z]([a-z0-9-]{0,47}[a-z0-9])?$'
  local resid='^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$'
  local image='^[a-z0-9]+([._-][a-z0-9]+)*$'
  _crci_match RUNNER_JOB_NAME "$cloudrun" "Cloud Run job name"
  _crci_match RUNNER_IMAGE_NAME "$image" "image name"
  _crci_match MEDIATOR_IMAGE_NAME "$image" "image name"
  _crci_distinct image RUNNER_IMAGE_NAME MEDIATOR_IMAGE_NAME

  local cpu="" mi=""
  cpu=$(_crci_posint RUNNER_CPU) || true
  [[ -n $cpu ]] || _crci_fail RUNNER_CPU "\"${CFG[RUNNER_CPU]}\" is not a positive integer"
  case $cpu in
    "" | 1 | 2 | 4 | 6 | 8) ;;
    *) _crci_fail RUNNER_CPU "$cpu is not a Cloud Run Jobs CPU value (1, 2, 4, 6 or 8)" ;;
  esac
  v=${CFG[RUNNER_MEMORY]}
  if [[ $v =~ ^([0-9]+)(Mi|Gi)$ ]] && n=$(_crci_int "${BASH_REMATCH[1]}"); then
    if [[ ${BASH_REMATCH[2]} == Gi ]]; then
      if ((${#n} > 6 || n > 32)); then mi=$((32 * 1024 + 1)); else mi=$((n * 1024)); fi
    else
      if ((${#n} > 6)); then mi=$((32 * 1024 + 1)); else mi=$n; fi
    fi
    if ((mi < 512 || mi > 32 * 1024)); then
      _crci_fail RUNNER_MEMORY "\"$v\" must be between 512Mi and 32Gi"
    else
      local lo="" hi=""
      case $cpu in
        1) lo=512 hi=4096 ;;
        2) lo=512 hi=8192 ;;
        4) lo=2048 hi=16384 ;;
        6) lo=4096 hi=24576 ;;
        8) lo=4096 hi=32768 ;;
      esac
      if [[ -n $hi ]] && ((mi < lo || mi > hi)); then
        _crci_fail RUNNER_MEMORY "$v is outside the $cpu-CPU range ${lo}Mi-${hi}Mi"
      fi
    fi
  else
    _crci_fail RUNNER_MEMORY "\"$v\" must be an integer followed by Mi or Gi (for example 32Gi)"
  fi

  n=$(_crci_posint RUNNER_DISK_GI) || true
  [[ -n $n ]] || _crci_fail RUNNER_DISK_GI "\"${CFG[RUNNER_DISK_GI]}\" is not a positive integer"
  [[ -n $n ]] && ((n > 100)) && _crci_fail RUNNER_DISK_GI "$n exceeds the 100 GiB ephemeral-disk maximum"
  n=$(_crci_posint MAX_CONCURRENT_RUNNERS) || true
  [[ -n $n ]] || _crci_fail MAX_CONCURRENT_RUNNERS "\"${CFG[MAX_CONCURRENT_RUNNERS]}\" is not a positive integer"
  case ${CFG[RUNNER_BUILD_CACHE_IN_RAM]} in
    true | false) ;;
    *) _crci_fail RUNNER_BUILD_CACHE_IN_RAM "\"${CFG[RUNNER_BUILD_CACHE_IN_RAM]}\" must be true or false" ;;
  esac

  _crci_duration_check TASK_TIMEOUT $((60 * 1000)) $((168 * 3600 * 1000)) "1m" "168h"
  _crci_duration_check CLAIM_TTL $((30 * 1000)) $((3600 * 1000)) "30s" "1h0m0s"

  _crci_match ARTIFACT_REPO "$resid" "Artifact Registry repository ID"
  _crci_match BUILD_STAGING_BUCKET '^[a-z0-9][a-z0-9_-]{1,61}[a-z0-9]$' "Cloud Storage bucket name"
  # Cloud Storage forbids the "goog" prefix and "google" anywhere in a name.
  if [[ ${CFG[BUILD_STAGING_BUCKET]} == goog* || ${CFG[BUILD_STAGING_BUCKET]} == *google* || ${CFG[BUILD_STAGING_BUCKET]} == *..* ]]; then
    _crci_fail BUILD_STAGING_BUCKET "\"${CFG[BUILD_STAGING_BUCKET]}\" is not a valid Cloud Storage bucket name"
  fi
  _crci_match MEDIATOR_SERVICE_NAME "$cloudrun" "Cloud Run service name"
  _crci_match RECONCILER_JOB_NAME "$resid" "Cloud Scheduler job ID"
  _crci_match RECONCILE_SCHEDULE '^[0-9*,/-]+( [0-9*,/-]+){4}$' "five-field cron schedule"

  v=${CFG[MEDIATOR_URL]}
  if [[ -n $v ]]; then
    if [[ $v =~ ^https://[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]+)?/?$ ]]; then
      CFG[MEDIATOR_URL]=${v%/}
    else
      _crci_fail MEDIATOR_URL "\"$v\" must be a bare https:// origin such as https://cr-runner-mediator-abc123-uc.a.run.app"
    fi
  fi

  if [[ -n ${CFG[GITHUB_RUNNER_GROUP_ID]} ]]; then
    if ! n=$(_crci_int "${CFG[GITHUB_RUNNER_GROUP_ID]}") || ((n < 1)); then
      _crci_fail GITHUB_RUNNER_GROUP_ID "\"${CFG[GITHUB_RUNNER_GROUP_ID]}\" is not a positive integer"
    fi
  fi
  _crci_match ROUTING_VARIABLE_NAME '^[A-Za-z_][A-Za-z0-9_]*$' "GitHub Actions variable name"
  [[ ${CFG[ROUTING_VARIABLE_NAME]^^} == GITHUB_* ]] &&
    _crci_fail ROUTING_VARIABLE_NAME "\"${CFG[ROUTING_VARIABLE_NAME]}\": names starting with GITHUB_ are reserved by GitHub"

  for k in BUILD_SA_NAME MEDIATOR_SA_NAME SCHEDULER_SA_NAME EXEC_SA_NAME; do
    _crci_match "$k" '^[a-z][a-z0-9-]{4,28}[a-z0-9]$' "service account ID"
  done
  _crci_distinct "service account" BUILD_SA_NAME EXEC_SA_NAME MEDIATOR_SA_NAME SCHEDULER_SA_NAME
  for k in GITHUB_APP_PRIVATE_KEY_SECRET_NAME GITHUB_WEBHOOK_SECRET_NAME; do
    _crci_match "$k" '^[A-Za-z0-9_-]{1,255}$' "Secret Manager secret name"
  done
  _crci_distinct secret GITHUB_APP_PRIVATE_KEY_SECRET_NAME GITHUB_WEBHOOK_SECRET_NAME

  if ((${#CRCI_ERRORS[@]} > 0)); then
    printf '%s: invalid cr-ci config %s:\n' "$CRCI_PROG" "$CRCI_CONFIG" >&2
    printf '  %s\n' "${CRCI_ERRORS[@]}" >&2
    return 1
  fi
  return 0
}

_crci_duration_check() { # KEY MIN_MS MAX_MS MIN_TEXT MAX_TEXT
  local v=${CFG[$1]} ms
  [[ -n $v ]] || return 0
  if ! ms=$(_crci_duration_ms "$v"); then
    _crci_fail "$1" "\"$v\" is not a duration"
  elif ((ms < $2 || ms > $3)); then
    _crci_fail "$1" "$v must be between $4 and $5"
  fi
}

# _crci_distinct WHAT KEY...: keys sorted; the later duplicate is reported.
_crci_distinct() {
  local what=$1 k
  shift
  local -A owner=()
  for k in "$@"; do
    local v=${CFG[$k]}
    [[ -n $v ]] || continue
    if [[ -v owner[$v] ]]; then
      _crci_fail "$k" "$what name \"$v\" is also used by ${owner[$v]}; each must be distinct"
      continue
    fi
    owner[$v]=$k
  done
}

# crci_abspath PATH: PATH made absolute against the current directory (the
# file need not exist). Use it before crci_start, which changes directory.
crci_abspath() {
  case $1 in
    /*) printf '%s' "$1" ;;
    *) printf '%s/%s' "$PWD" "$1" ;;
  esac
}

# ---- derived names ------------------------------------------------------------------

crci_sa_email() { printf '%s@%s.iam.gserviceaccount.com' "$1" "${CFG[GCP_PROJECT]}"; }

# crci_staging_bucket: BUILD_STAGING_BUCKET, or <GCP_PROJECT>-cr-ci-staging.
crci_staging_bucket() {
  if [[ -n ${CFG[BUILD_STAGING_BUCKET]} ]]; then
    printf '%s' "${CFG[BUILD_STAGING_BUCKET]}"
  else
    printf '%s-cr-ci-staging' "${CFG[GCP_PROJECT]}"
  fi
}

crci_registry() { printf '%s-docker.pkg.dev' "${CFG[REGION]}"; }

# crci_image_ref IMAGE_NAME TAG
crci_image_ref() {
  printf '%s/%s/%s/%s:%s' "$(crci_registry)" "${CFG[GCP_PROJECT]}" "${CFG[ARTIFACT_REPO]}" "$1" "$2"
}

# crci_duration_seconds VALUE: whole seconds, rounded up (for gcloud flags).
crci_duration_seconds() {
  local ms
  ms=$(_crci_duration_ms "$1") || crci_die "not a duration: $1"
  printf '%s' $(((ms + 999) / 1000))
}

# crci_default_tag PATH...: "git-" + 12 hex chars of the git tree hash of the
# given module-relative paths at HEAD, so an unchanged source keeps its tag.
# With --apply, uncommitted changes under the paths are refused (the image
# would not match its tag); pass --tag to override.
crci_default_tag() {
  local prefix h p
  local -a trees=()
  git -C "$CRCI_ROOT" rev-parse --git-dir >/dev/null 2>&1 ||
    crci_die "not a git checkout; pass --tag TAG"
  prefix=$(git -C "$CRCI_ROOT" rev-parse --show-prefix) || crci_die "git rev-parse failed"
  for p in "$@"; do
    h=$(git -C "$CRCI_ROOT" rev-parse "HEAD:$prefix$p") || crci_die "git: $p is not committed; pass --tag TAG"
    trees+=("$h")
  done
  if [[ $CRCI_MODE == apply && -n $(git -C "$CRCI_ROOT" status --porcelain -- "$@") ]]; then
    crci_die "uncommitted changes under $*; commit them or pass --tag TAG"
  fi
  if ((${#trees[@]} == 1)); then
    h=${trees[0]}
  else
    h=$(printf '%s\n' "${trees[@]}" | git hash-object --stdin)
  fi
  printf 'git-%s' "${h:0:12}"
}

# crci_try_default_tag PATH...: like crci_default_tag but prints nothing
# (instead of dying) outside a git checkout or when a path is not committed.
crci_try_default_tag() {
  local prefix h p
  local -a trees=()
  git -C "$CRCI_ROOT" rev-parse --git-dir >/dev/null 2>&1 || return 0
  prefix=$(git -C "$CRCI_ROOT" rev-parse --show-prefix 2>/dev/null) || return 0
  for p in "$@"; do
    h=$(git -C "$CRCI_ROOT" rev-parse "HEAD:$prefix$p" 2>/dev/null) || return 0
    trees+=("$h")
  done
  ((${#trees[@]} == 1)) || h=$(printf '%s\n' "${trees[@]}" | git hash-object --stdin)
  printf 'git-%s' "${h:0:12}"
}

# crci_check_exec_sa_has_no_roles: EXEC_SA_NAME must hold no project roles.
crci_check_exec_sa_has_no_roles() {
  local email roles
  email=$(crci_sa_email "${CFG[EXEC_SA_NAME]}")
  crci_read roles "" gcloud projects get-iam-policy "${CFG[GCP_PROJECT]}" \
    --flatten=bindings[].members --filter="bindings.members:serviceAccount:$email" --format='value(bindings.role)'
  if [[ -n $roles ]]; then
    crci_die "$email holds project roles ($(tr '\n' ' ' <<<"$roles")); it must hold none (permissions-matrix 2.2)"
  fi
}

# crci_build_image NAME DOCKERFILE CONTEXT TAG LOCAL: builds and pushes
# ARTIFACT_REPO/NAME:TAG as BUILD_SA_NAME, skipping the build if the tag exists.
crci_build_image() {
  local name=$1 dockerfile=$2 context=$3 tag=$4 local_build=$5
  local ref build_sa
  ref=$(crci_image_ref "$name" "$tag")
  build_sa=$(crci_sa_email "${CFG[BUILD_SA_NAME]}")
  if crci_exists gcloud artifacts docker images describe "$ref" --project="${CFG[GCP_PROJECT]}" --format='value(image_summary.digest)'; then
    crci_note "image $ref already exists; not rebuilding"
    return 0
  fi
  if [[ $local_build == 1 ]]; then
    [[ $CRCI_MODE == apply ]] && crci_require_tool docker "$CRCI_MIN_DOCKER"
    # BuildKit explicitly: the Dockerfiles may use BuildKit-only syntax
    # (COPY --chmod), and older daemons still default to the legacy builder.
    crci_run env DOCKER_BUILDKIT=1 docker build --platform=linux/amd64 -f "$dockerfile" -t "$ref" "$context"
    # Push as the build SA: its short-lived token goes from gcloud to docker
    # through a pipe, never through argv.
    local reg
    reg="https://$(crci_registry)"
    printf '+ %s | %s\n' "$(crci_quote gcloud auth print-access-token "--impersonate-service-account=$build_sa")" \
      "$(crci_quote docker login -u oauth2accesstoken --password-stdin "$reg")"
    if [[ $CRCI_MODE == apply ]]; then
      # Sequential, so a failure names its step; the token lives only in this
      # shell variable and reaches docker through the printf builtin (stdin).
      local token
      token=$(gcloud auth print-access-token --impersonate-service-account="$build_sa") ||
        crci_die "step failed: gcloud auth print-access-token as $build_sa"
      printf '%s' "$token" | docker login -u oauth2accesstoken --password-stdin "$reg" >/dev/null ||
        crci_die "step failed: docker login to $reg"
      token=""
    fi
    crci_run docker push "$ref"
    crci_run docker logout "$reg"
  else
    crci_run gcloud builds submit "$context" --config=scripts/cloudbuild.yaml \
      --substitutions="_IMAGE=$ref,_DOCKERFILE=$dockerfile" \
      --gcs-source-staging-dir="gs://$(crci_staging_bucket)/source" \
      --service-account="projects/${CFG[GCP_PROJECT]}/serviceAccounts/$build_sa" \
      --region="${CFG[REGION]}" --project="${CFG[GCP_PROJECT]}"
  fi
}
