#!/usr/bin/env bash
# cr-ci runner entrypoint (PID 1 of every runner execution).
#
#   1. Verify that /scratch is a mounted, writable volume (the Cloud Run
#      ephemeral-disk). The container root filesystem is in memory on Cloud
#      Run, so running without /scratch would silently eat RAM. Fail loudly.
#   2. Point the runner workspace, temp dirs and Go caches at /scratch.
#   3. Exchange the one-time RUNNER_CLAIM_TOKEN with the mediator for the JIT
#      runner config: POST ${MEDIATOR_URL}/internal/claim with
#      "Authorization: Bearer <token>" and an empty body; 200 returns
#      {"jit_config":"..."}, 410 means unknown/expired/used, 5xx is transient.
#   4. exec ./run.sh --jitconfig <config> so that the runner is PID 1 and runs
#      exactly one job.
#
# Neither the token nor the JIT config is ever printed, logged or written to
# disk. Do NOT add `set -x` or debug echoes of either variable. The token is
# passed to curl on stdin (not argv) and the response stays in shell memory.
#
# Required env (set by the mediator as Cloud Run Job overrides):
#   RUNNER_CLAIM_TOKEN  32 lowercase hex chars, single use
#   MEDIATOR_URL        https origin of the mediator, no trailing slash
#   GH_JOB_ID           GitHub workflow job ID (logging only)
#   RUNNER_NAME         cr-job-<GH_JOB_ID> (logging only)
#
# Exit codes (run.sh's own exit code once it is exec'd):
#   2  configuration error: missing or invalid env, /scratch missing or not writable
#   3  claim failed: 410, non-retryable response, or retries exhausted; run.sh not started
#
# Overrides, for the offline test harness (runner/entrypoint_test.go) only;
# production leaves them unset:
#   CR_SCRATCH_DIR           scratch mount point          (default /scratch)
#   CR_MOUNTINFO             mount table to check against (default /proc/self/mountinfo)
#   CR_RUNNER_DIR            directory holding run.sh     (default /home/runner/actions-runner)
#   CR_CLAIM_BACKOFF_SECONDS first retry delay, doubled per retry (default 1)
# The harness also puts stub `stat` and `sleep` commands first on PATH.
set -euo pipefail

readonly CLAIM_MAX_ATTEMPTS=5
readonly EXIT_CONFIG=2
readonly EXIT_CLAIM=3

scratch=${CR_SCRATCH_DIR:-/scratch}
mountinfo=${CR_MOUNTINFO:-/proc/self/mountinfo}
runner_dir=${CR_RUNNER_DIR:-/home/runner/actions-runner}
backoff=${CR_CLAIM_BACKOFF_SECONDS:-1}

log() { printf 'cr-ci-entrypoint: %s\n' "$*" >&2; }
die() {
  local code=$1
  shift
  log "FATAL: $*"
  exit "$code"
}

# --- 1. Required env ----------------------------------------------------------

for var in RUNNER_CLAIM_TOKEN MEDIATOR_URL GH_JOB_ID RUNNER_NAME; do
  if [[ -z ${!var:-} ]]; then
    die "$EXIT_CONFIG" "required environment variable $var is not set"
  fi
done

if [[ ! $RUNNER_CLAIM_TOKEN =~ ^[0-9a-f]{32}$ ]]; then
  # Never echo the value, not even a malformed one.
  die "$EXIT_CONFIG" "RUNNER_CLAIM_TOKEN is not 32 lowercase hex characters"
fi
if [[ ! $GH_JOB_ID =~ ^[0-9]+$ ]]; then
  die "$EXIT_CONFIG" "GH_JOB_ID must be numeric"
fi

claim_base=${MEDIATOR_URL%/}
# https only; plain http is accepted for loopback so the offline harness works.
if [[ ! $claim_base =~ ^https://[^/?#[:space:]]+$ &&
  ! $claim_base =~ ^http://(127\.0\.0\.1|localhost)(:[0-9]+)?$ ]]; then
  die "$EXIT_CONFIG" "MEDIATOR_URL must be an https origin with no path, got '$MEDIATOR_URL'"
fi

if [[ ! $backoff =~ ^[0-9]+(\.[0-9]+)?$ ]]; then
  die "$EXIT_CONFIG" "CR_CLAIM_BACKOFF_SECONDS must be a non-negative number"
fi

if [[ $RUNNER_NAME != "cr-job-$GH_JOB_ID" ]]; then
  log "warning: RUNNER_NAME '$RUNNER_NAME' does not match cr-job-$GH_JOB_ID"
fi
log "starting runner $RUNNER_NAME for job $GH_JOB_ID"

# --- 2. /scratch must be a mounted, writable volume ---------------------------

# Prints the filesystem type of the mount whose mount point is exactly $1, or
# nothing if $1 is not a mount point. mountinfo field 5 is the mount point
# and the field after the " - " separator is the filesystem type. Mount
# points containing whitespace are octal-escaped there and never match,
# which is fine for the fixed /scratch path.
mount_fstype() {
  awk -v mp="$1" '
    $5 == mp {
      for (i = 7; i <= NF; i++) if ($i == "-") { fstype = $(i + 1); break }
    }
    END { if (fstype != "") print fstype }
  ' "$mountinfo"
}

if [[ ! -d $scratch ]]; then
  die "$EXIT_CONFIG" "$scratch does not exist; the Cloud Run Job must mount an ephemeral-disk volume there (--add-volume=name=scratch,type=ephemeral-disk,size=<N>Gi --add-volume-mount=volume=scratch,mount-path=$scratch). Refusing to run on the in-memory root filesystem."
fi
# Primary check: /scratch is listed as a mount point in the mount table.
# Fallback: /scratch sits on a different device than its parent directory,
# which holds for any mount point even if the sandbox kernel (gVisor on Cloud
# Run) were to present the mount table differently. Which check matched is
# logged so the P6 PoC can confirm the primary one works on Cloud Run.
scratch_fstype=""
if [[ -r $mountinfo ]]; then
  scratch_fstype=$(mount_fstype "$scratch")
  mount_check="listed in $mountinfo"
else
  log "warning: cannot read $mountinfo; falling back to the device check for $scratch"
fi
if [[ -z $scratch_fstype ]]; then
  scratch_dev=$(stat -c %d -- "$scratch") ||
    die "$EXIT_CONFIG" "cannot stat $scratch to verify that it is a mount"
  parent_dev=$(stat -c %d -- "$(dirname -- "$scratch")") ||
    die "$EXIT_CONFIG" "cannot stat the parent of $scratch to verify that it is a mount"
  if [[ $scratch_dev == "$parent_dev" ]]; then
    die "$EXIT_CONFIG" "$scratch exists but is not a mount point; the Cloud Run Job must mount an ephemeral-disk volume there. Refusing to run on the in-memory root filesystem."
  fi
  scratch_fstype=$(stat -f -c %T -- "$scratch") || scratch_fstype=unknown
  mount_check="device $scratch_dev differs from parent device $parent_dev"
fi
log "$scratch is a mount ($scratch_fstype): $mount_check"
if [[ $scratch_fstype == tmpfs ]]; then
  log "warning: $scratch is tmpfs (in-memory); use type=ephemeral-disk so caches do not consume RAM"
fi

probe="$scratch/.cr-ci-write-probe.$$"
if ! : >"$probe" 2>/dev/null; then
  die "$EXIT_CONFIG" "$scratch is mounted ($scratch_fstype) but not writable by uid $(id -u)"
fi
rm -f -- "$probe"

# --- 3. Redirect workspace, temp dirs and caches to /scratch ------------------
# The runner sets its own RUNNER_WORKSPACE and RUNNER_TEMP for job steps
# (under its work folder, i.e. the _work symlink into /scratch). The exports
# here cover the listener and anything that runs before the job; the _work
# and _diag symlinks in the image are what carry the runner's own redirection.

export RUNNER_WORKSPACE="$scratch/_work"
export RUNNER_TEMP="$scratch/_temp"
export TMPDIR="$scratch/_temp"
export GOPATH="$scratch/gopath"
export GOCACHE="$scratch/gocache"
export GOMODCACHE="$scratch/gomodcache"
export GOLANGCI_LINT_CACHE="$scratch/golangci-lint-cache"
export XDG_CACHE_HOME="$scratch/cache"

# _diag is the target of the runner's _diag symlink (runner logs; see Dockerfile).
mkdir -p -- "$RUNNER_WORKSPACE" "$RUNNER_TEMP" "$GOPATH" "$GOCACHE" \
  "$GOMODCACHE" "$GOLANGCI_LINT_CACHE" "$XDG_CACHE_HOME" "$scratch/_diag"
log "workspace, temp and caches redirected to $scratch ($scratch_fstype)"

# Check the runner before spending the single-use token.
if [[ ! -x $runner_dir/run.sh ]]; then
  die "$EXIT_CONFIG" "$runner_dir/run.sh not found or not executable"
fi
cd -- "$runner_dir"

# --- 4. Claim exchange --------------------------------------------------------

claim_url="$claim_base/internal/claim"
jit_config=""

# One claim request. Sets the globals http_code, content_type and body. curl's
# options come from a config on stdin so the token never appears in argv
# (/proc/<pid>/cmdline). -q must stay first: it stops ~/.curlrc from turning
# on verbose output, which would print the request headers.
claim_once() {
  local out rc=0
  out=$(
    printf 'header = "Authorization: Bearer %s"\n' "$RUNNER_CLAIM_TOKEN" |
      curl -q --config - \
        --silent --show-error \
        --request POST --data-binary '' \
        --proto '=https,http' --connect-timeout 10 --max-time 30 \
        --write-out '\n%{http_code} %{content_type}' \
        -- "$claim_url"
  ) || rc=$?
  if ((rc != 0)); then
    http_code=000
    content_type=""
    body=""
    log "claim attempt failed: curl exit $rc (connection error or timeout)"
    return 0
  fi
  local trailer=${out##*$'\n'}
  body=${out%$'\n'*}
  http_code=${trailer%% *}
  content_type=${trailer#* }
}

attempt=1
delay=$backoff
while :; do
  claim_once
  case $http_code in
    200)
      if [[ $content_type != application/json* ]]; then
        die "$EXIT_CLAIM" "claim returned 200 with Content-Type '$content_type', want application/json"
      fi
      if ! jit_config=$(printf '%s' "$body" | jq -er '.jit_config | select(type == "string" and length > 0)' 2>/dev/null); then
        die "$EXIT_CLAIM" "claim returned 200 without a non-empty jit_config string"
      fi
      log "claim succeeded on attempt $attempt"
      break
      ;;
    410)
      die "$EXIT_CLAIM" "claim token unknown, expired or already used (410); not starting the runner"
      ;;
    000 | 5??)
      if ((attempt >= CLAIM_MAX_ATTEMPTS)); then
        die "$EXIT_CLAIM" "claim failed after $attempt attempts (last status $http_code); not starting the runner"
      fi
      log "claim attempt $attempt got status $http_code; retrying in ${delay}s"
      sleep "$delay"
      attempt=$((attempt + 1))
      delay=$(awk -v d="$delay" 'BEGIN { print d * 2 }')
      ;;
    *)
      die "$EXIT_CLAIM" "claim returned unexpected status $http_code; not starting the runner"
      ;;
  esac
done
body=""

# The token is spent; keep it out of the job's environment.
unset RUNNER_CLAIM_TOKEN

# --- 5. Hand over to the runner as PID 1 --------------------------------------

log "exec run.sh (ephemeral JIT runner)"
exec ./run.sh --jitconfig "$jit_config"
