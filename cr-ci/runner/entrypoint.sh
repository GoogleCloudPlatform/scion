#!/usr/bin/env bash
# cr-ci runner entrypoint (PID 1 of every runner execution).
#
#   1. Verify that /scratch is a mounted, writable volume (the Cloud Run
#      ephemeral-disk). The container root filesystem is in memory on Cloud
#      Run, so running without /scratch would silently eat RAM. Fail loudly.
#   2. Link ~/work, ~/.cache, ~/go and ~/.npm into /scratch and export ubuntu-latest's cache
#      paths, so actions/cache entries restore identically on both runner types while
#      the bytes land on the ephemeral disk (see section 3).
#   3. Exchange the one-time RUNNER_CLAIM_TOKEN with the mediator for the JIT
#      runner config (claim contract v2): fetch this execution's Google ID
#      token (audience MEDIATOR_URL) from the metadata server, then
#      POST ${MEDIATOR_URL}/internal/claim with "Authorization: Bearer <ID
#      token>", "X-Runner-Claim: <RUNNER_CLAIM_TOKEN>" and an empty body.
#      200 returns {"jit_config":"..."}; 401 means the identity was rejected,
#      410 that the claim is unknown/expired/used; 5xx is transient.
#   4. Run ./run.sh --jitconfig <config> as a child that runs exactly one job,
#      forward SIGTERM/SIGINT to it, and exit with the runner's real exit code
#      (section 5).
#
# Neither the claim token, the ID token nor the JIT config is ever printed,
# logged or written to disk. Do NOT add `set -x` or debug echoes of them. The
# claim and ID tokens reach curl on stdin (not argv), and responses stay in
# shell memory.
#
# Warm-up: RUNNER_WARMUP=1 (deploy-job.sh --warm-up) runs only the checks in
# sections 1-3 (HOME, the job guard files, /scratch, HOME links, run.sh present) and
# exits 0 before any claim; none of the required env below is needed then.
#
# Optional env (set on the job by deploy-job.sh from config.env):
#   RUNNER_BUILD_CACHE_IN_RAM  true: keep ~/.cache (Go build cache, analysis cache) in
#                              RAM instead of on /scratch (section 3); default false
#   RUNNER_IDLE_TIMEOUT_SECONDS stop the runner (exit 0) if it has not started a job within
#                              this many seconds of the container start (default 600; section 5)
#
# Required env (set by the mediator as Cloud Run Job overrides):
#   RUNNER_CLAIM_TOKEN  32 lowercase hex chars, single use
#   MEDIATOR_URL        https origin of the mediator, no trailing slash
#   GH_JOB_ID           GitHub workflow job ID (logging only)
#   RUNNER_NAME         cr-job-<GH_JOB_ID> (logging only)
#   GH_REPOS            comma-separated owner/name list of the repositories whose jobs
#                       the runner may take (repo scope: the one repository; org scope:
#                       every target repository of the owner); the job guard refuses a
#                       job of any other repository
#
# Exit codes:
#   2  configuration error: missing or invalid env (HOME included), the job-started hook
#      not the pinned, root-owned, read-only file (section 1, "Job guard"), /scratch
#      missing or not writable, a HOME link (~/work, ~/.cache, ~/go, ~/.npm) that is not
#      the expected symlink
#   3  claim failed: 401, 410, other non-retryable response (mediator or
#      metadata server), or retries exhausted; run.sh not started
#   128+n  SIGTERM (143) or SIGINT (130) received before run.sh started; run.sh not started
#   0      also when the runner was stopped for its idle timeout (logged as "idle_timeout")
#   4      the idle stop raced a job start: the SIGTERM cancelled a job that started just
#          as the deadline passed (logged as "ERROR idle_timeout_raced_job"; a 4 without
#          that line is run.sh's own code)
#   5      the job-started hook (cr-ci-job-guard.sh) refused the assigned job and the runner
#          was stopped before any of its steps (logged as "ERROR job_guard_refused")
#   otherwise run.sh's exit code, or, when run.sh exits 0, the runner exit code that
#      run-helper.sh reported as unknown (for example 134 after an abort)
#
# Overrides, for the offline test harness (runner/entrypoint_test.go) only. They take
# effect only in test mode, CR_TEST_MODE=1 with this script not PID 1, which production
# (always PID 1) cannot enable; otherwise they are logged and ignored (section 1):
#   CR_SCRATCH_DIR           scratch mount point          (default /scratch)
#   CR_MOUNTINFO             mount table to check against (default /proc/self/mountinfo)
#   CR_RUNNER_DIR            directory holding run.sh     (default /home/runner/actions-runner)
#   CR_CLAIM_BACKOFF_SECONDS first retry delay, doubled per retry (default 1)
#   CR_EXPECTED_HOME         required value of HOME       (default /home/runner)
#   CR_METADATA_URL          metadata server origin       (default http://metadata.google.internal;
#                            otherwise only http://127.0.0.1[:port] or http://localhost[:port])
#   CR_FIFO_TMPDIR           where the runner-output FIFOs go (default /tmp)
# (RUNNER_IDLE_TIMEOUT_SECONDS, below, is set on the job by deploy-job.sh; tests use small values.)
#   CR_RAM_CACHE_DIR         RAM target of /scratch/.cache when RUNNER_BUILD_CACHE_IN_RAM=true
#                            (default /tmp/cr-ci-build-cache)
#   CR_WATCHER_GRACE_SECONDS how long to wait for the output watcher (default 5)
#   CR_JOB_GUARD_HOOK        the job-started hook   (default /usr/local/bin/cr-ci-job-guard.sh)
#   CR_JOB_GUARD_SHA_FILE    its pinned sha256      (default /etc/cr-ci/job-guard.sha256)
#   CR_JOB_GUARD_OWNER_UID   required owner of both (default 0, root)
# The harness also puts stub `stat` and `sleep` commands first on PATH.
set -euo pipefail

readonly CLAIM_MAX_ATTEMPTS=5
readonly EXIT_CONFIG=2
readonly EXIT_CLAIM=3
readonly EXIT_IDLE_RACE=4
readonly EXIT_JOB_REFUSED=5

log() { printf 'cr-ci-entrypoint: %s\n' "$*" >&2; }
die() {
  local code=$1
  shift
  log "FATAL: $*"
  [[ -n ${fifo_dir:-} ]] && rm -rf -- "$fifo_dir"
  exit "$code"
}

# Signals. Cloud Run sends SIGTERM to PID 1 (this script) on CancelExecution or the task
# timeout. Before run.sh runs, a TERM or INT is recorded and the script stops at the next
# checkpoint without starting the runner (exit 128+n; /reconcile reaps an unused
# registration). Once run.sh runs, it is forwarded to it (section 5). Trapped from the
# start: as PID 1, an untrapped TERM would simply be dropped.
runner_pid=""
pending_signal=""
# shellcheck disable=SC2317,SC2329 # invoked by the traps below (SC2317: shellcheck <0.10, SC2329: >=0.10)
on_signal() {
  if [[ -n $runner_pid ]]; then
    log "received SIG$1; forwarding it to run.sh (graceful job cancel)"
    kill -s "$1" "$runner_pid" 2>/dev/null || true
  else
    pending_signal=$1
    log "received SIG$1 before run.sh started; stopping without starting the runner"
  fi
}
trap 'on_signal TERM' TERM
trap 'on_signal INT' INT
# exit_if_signalled: a checkpoint before run.sh starts.
exit_if_signalled() {
  if [[ -n $pending_signal ]]; then
    [[ -n ${fifo_dir:-} ]] && rm -rf -- "$fifo_dir"
    exit $((128 + $(kill -l "$pending_signal")))
  fi
}

# Test mode: the CR_* overrides listed in the header take effect only when CR_TEST_MODE=1
# AND this script is not PID 1. In production it is always PID 1 (the image's exec-form
# ENTRYPOINT on Cloud Run), so production cannot enable test mode even if CR_TEST_MODE
# were set; outside test mode every override is ignored (logged and unset), and the
# defaults (the fixed paths in the image) apply. Nothing a workflow does can set any of
# them: this script's environment is the Cloud Run Job execution's (deploy-job.sh and the
# mediator's overrides), and it has finished its checks and exported the job guard's
# settings before run.sh, and so any job, exists.
readonly TEST_OVERRIDES=(CR_SCRATCH_DIR CR_MOUNTINFO CR_RUNNER_DIR CR_CLAIM_BACKOFF_SECONDS
  CR_EXPECTED_HOME CR_METADATA_URL CR_FIFO_TMPDIR CR_RAM_CACHE_DIR CR_WATCHER_GRACE_SECONDS
  CR_JOB_GUARD_HOOK CR_JOB_GUARD_SHA_FILE CR_JOB_GUARD_OWNER_UID)
test_mode=0
if [[ ${CR_TEST_MODE:-} == 1 && $$ -ne 1 ]]; then
  test_mode=1
else
  for var in "${TEST_OVERRIDES[@]}"; do
    if [[ -n ${!var+set} ]]; then
      log "warning: ignoring $var: test overrides take effect only in test mode"
      unset "$var"
    fi
  done
fi
unset CR_TEST_MODE
readonly test_mode

scratch=${CR_SCRATCH_DIR:-/scratch}
mountinfo=${CR_MOUNTINFO:-/proc/self/mountinfo}
runner_dir=${CR_RUNNER_DIR:-/home/runner/actions-runner}
backoff=${CR_CLAIM_BACKOFF_SECONDS:-1}
metadata_base=${CR_METADATA_URL:-http://metadata.google.internal}
expected_home=${CR_EXPECTED_HOME:-/home/runner}
ram_cache_dir=${CR_RAM_CACHE_DIR:-/tmp/cr-ci-build-cache}
fifo_tmpdir=${CR_FIFO_TMPDIR:-/tmp}
watcher_grace=${CR_WATCHER_GRACE_SECONDS:-5}
# The job-started hook and its pinned sha256 (written at image build, Dockerfile), both
# owned by root and not writable by the runner (section 1, "Job guard").
job_guard_hook=${CR_JOB_GUARD_HOOK:-/usr/local/bin/cr-ci-job-guard.sh}
job_guard_sha_file=${CR_JOB_GUARD_SHA_FILE:-/etc/cr-ci/job-guard.sha256}
job_guard_owner=${CR_JOB_GUARD_OWNER_UID:-0}

# --- 1. Required env ----------------------------------------------------------

# Warm-up mode (deploy-job.sh --warm-up): an execution with RUNNER_WARMUP=1 and no
# claim env pulls and imports the image, runs the /scratch and HOME checks below,
# and exits 0 before any claim, so the first real job does not pay the image import.
warmup=0
case ${RUNNER_WARMUP:-} in
  "" | 0) ;;
  1) warmup=1 ;;
  *) die "$EXIT_CONFIG" "RUNNER_WARMUP must be 1 or unset" ;;
esac

case ${RUNNER_BUILD_CACHE_IN_RAM:-false} in
  true) cache_in_ram=1 ;;
  false) cache_in_ram=0 ;;
  *) die "$EXIT_CONFIG" "RUNNER_BUILD_CACHE_IN_RAM must be true or false" ;;
esac

if ((warmup)); then
  required_vars=(HOME)
else
  required_vars=(RUNNER_CLAIM_TOKEN MEDIATOR_URL GH_JOB_ID RUNNER_NAME GH_REPOS HOME)
fi
for var in "${required_vars[@]}"; do
  if [[ -z ${!var:-} ]]; then
    die "$EXIT_CONFIG" "required environment variable $var is not set"
  fi
done
# GH_REPOS: the repositories (owner/name, comma-separated, 1-100 of them) whose jobs this
# runner may be handed. The job guard refuses a job of any other repository (section 5,
# "Job guard"). Never echoed: a malformed value may hold anything.
if ((!warmup)); then
  [[ $GH_REPOS =~ ^[A-Za-z0-9-]+/[A-Za-z0-9._-]+(,[A-Za-z0-9-]+/[A-Za-z0-9._-]+){0,99}$ ]] ||
    die "$EXIT_CONFIG" "GH_REPOS must be 1-100 comma-separated owner/name entries"
fi

# --- Job guard: the job-started hook must be the one pinned in the image ----------
# Fail closed: if Runner.Worker cannot run the hook (missing, not executable), its
# pre-job step only fails, and the job's always()/failure() steps still run. So run.sh
# is not started (exit 2, before any claim, in warm-up too) unless the hook, its sha256
# file and the directories holding them are owned by root (in production) and not
# writable by this uid, the hook is a regular executable file (not a symlink), and its
# sha256 equals the value pinned at image build. The runner itself must not run as
# root, which would make all of them writable.
my_uid=$(id -u)
# require_protected <path> <what> <-f|-d>: owned by job_guard_owner, no group or other
# write bit, not writable by this uid (in production: also, this uid is not the owner).
require_protected() {
  local path=$1 what=$2 kind=$3 meta owner mode
  [[ -L $path ]] && die "$EXIT_CONFIG" "job guard: $what $path is a symlink; refusing to start the runner"
  test "$kind" "$path" || die "$EXIT_CONFIG" "job guard: $what $path is missing or of the wrong type; refusing to start the runner"
  meta=$(stat -c '%u %a' -- "$path") || die "$EXIT_CONFIG" "job guard: cannot stat $what $path; refusing to start the runner"
  read -r owner mode <<<"$meta"
  [[ $owner == "$job_guard_owner" ]] ||
    die "$EXIT_CONFIG" "job guard: $what $path is owned by uid $owner, want $job_guard_owner; refusing to start the runner"
  ((8#$mode & 8#022)) &&
    die "$EXIT_CONFIG" "job guard: $what $path is group- or world-writable (mode $mode); refusing to start the runner"
  if [[ $owner != "$my_uid" && -w $path ]]; then
    die "$EXIT_CONFIG" "job guard: $what $path is writable by uid $my_uid; refusing to start the runner"
  fi
  return 0
}
if ((!test_mode)) && [[ $my_uid == "$job_guard_owner" ]]; then
  die "$EXIT_CONFIG" "job guard: the runner runs as uid $my_uid, the owner of the job-started hook; refusing to start the runner"
fi
require_protected "$(dirname -- "$job_guard_hook")" "the directory of the job-started hook" -d
require_protected "$job_guard_hook" "the job-started hook" -f
require_protected "$(dirname -- "$job_guard_sha_file")" "the directory of the job guard sha256 file" -d
require_protected "$job_guard_sha_file" "the job guard sha256 file" -f
[[ -x $job_guard_hook ]] ||
  die "$EXIT_CONFIG" "job guard: the job-started hook $job_guard_hook is not executable; refusing to start the runner"
job_guard_want=$(<"$job_guard_sha_file") ||
  die "$EXIT_CONFIG" "job guard: cannot read $job_guard_sha_file; refusing to start the runner"
[[ $job_guard_want =~ ^[0-9a-f]{64}$ ]] ||
  die "$EXIT_CONFIG" "job guard: $job_guard_sha_file does not hold one sha256 (64 lowercase hex characters); refusing to start the runner"
job_guard_got=$(sha256sum -- "$job_guard_hook") ||
  die "$EXIT_CONFIG" "job guard: cannot hash $job_guard_hook; refusing to start the runner"
job_guard_got=${job_guard_got%% *}
[[ $job_guard_got == "$job_guard_want" ]] ||
  die "$EXIT_CONFIG" "job guard: the sha256 of $job_guard_hook is $job_guard_got, want $job_guard_want (pinned at image build); refusing to start the runner"
log "job guard: $job_guard_hook matches its pinned sha256"

if ((warmup)); then
  : # no claim env to check
elif [[ ! $RUNNER_CLAIM_TOKEN =~ ^[0-9a-f]{32}$ ]]; then
  # Never echo the value, not even a malformed one.
  die "$EXIT_CONFIG" "RUNNER_CLAIM_TOKEN is not 32 lowercase hex characters"
fi
# Cache parity needs ubuntu-latest's HOME (set in the image config).
if [[ $HOME != "$expected_home" ]]; then
  die "$EXIT_CONFIG" "HOME is '$HOME', want $expected_home (cache paths must match ubuntu-latest)"
fi
if ((!warmup)); then
  if [[ ! $GH_JOB_ID =~ ^[0-9]+$ ]]; then
    die "$EXIT_CONFIG" "GH_JOB_ID must be numeric"
  fi
  claim_base=${MEDIATOR_URL%/}
  # https only; plain http is accepted for loopback so the offline harness works.
  if [[ ! $claim_base =~ ^https://[^/?#[:space:]]+$ &&
    ! $claim_base =~ ^http://(127\.0\.0\.1|localhost)(:[0-9]+)?$ ]]; then
    die "$EXIT_CONFIG" "MEDIATOR_URL must be an https origin with no path, got '$MEDIATOR_URL'"
  fi
fi

if [[ $metadata_base != http://metadata.google.internal &&
  ! $metadata_base =~ ^http://(127\.0\.0\.1|localhost)(:[0-9]+)?$ ]]; then
  die "$EXIT_CONFIG" "CR_METADATA_URL must be http://metadata.google.internal or a loopback origin, got '$metadata_base'"
fi

idle_timeout=${RUNNER_IDLE_TIMEOUT_SECONDS:-600}
if [[ ! $idle_timeout =~ ^[1-9][0-9]{0,5}$ ]]; then
  die "$EXIT_CONFIG" "RUNNER_IDLE_TIMEOUT_SECONDS must be a positive whole number of seconds"
fi

if [[ ! $backoff =~ ^[0-9]+(\.[0-9]+)?$ ]]; then
  die "$EXIT_CONFIG" "CR_CLAIM_BACKOFF_SECONDS must be a non-negative number"
fi

if ((warmup)); then
  log "warm-up execution: checking the image, /scratch and the HOME links; no claim"
else
  if [[ $RUNNER_NAME != "cr-job-$GH_JOB_ID" ]]; then
    log "warning: RUNNER_NAME '$RUNNER_NAME' does not match cr-job-$GH_JOB_ID"
  fi
  log "starting runner $RUNNER_NAME for job $GH_JOB_ID"
fi

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
# Cache parity with ubuntu-latest (gs://scion-xproject-exchange/cr-ci/cache-parity.md).
# actions/cache stores paths relative to GITHUB_WORKSPACE and extracts them with
# tar -P -C "$GITHUB_WORKSPACE". So the work folder must read /home/runner/work, and
# GOCACHE, GOMODCACHE and GOPATH must read as on ubuntu-latest ($HOME/.cache/go-build,
# $HOME/go/pkg/mod, $HOME/go). The bytes must still land on /scratch, which is why the
# three HOME entries are symlinks into /scratch. Their targets MIRROR $HOME under
# /scratch: the kernel resolves ".." physically, so from the physical workspace
# /scratch/_work/<repo>/<repo>, an archive entry ../../../.cache/X lands in
# /scratch/.cache/X, which therefore has to be what ~/.cache points to.
#   ~/work   -> /scratch/_work   (the mediator's JIT work_folder is /home/runner/work)
#   ~/.cache -> /scratch/.cache  (GOCACHE, GOLANGCI_LINT_CACHE, any XDG cache)
#   ~/go     -> /scratch/go      (GOPATH, GOMODCACHE)
#   ~/.npm   -> /scratch/.npm    (npm's cache; actions/setup-node's npm cache step also
#                                 requires the directory to exist, as on ubuntu-latest)
# The links are the parents of the cached paths, so tar never replaces a link.
# XDG_CACHE_HOME stays unset, as on ubuntu-latest; tools then default to ~/.cache.
# The runner sets its own RUNNER_WORKSPACE and RUNNER_TEMP for job steps; the
# exports below cover the listener and anything that runs before the job.

# ensure_link <path> <target>: <path> must end up a symlink whose literal target is
# <target>. A correct link is kept, an absent path is created, and anything else (a
# real file or dir, or a link elsewhere) exits 2 before the claim token is spent.
ensure_link() {
  local path=$1 target=$2
  mkdir -p -- "$target"
  if [[ -L $path ]]; then
    local current
    current=$(readlink -- "$path")
    [[ $current == "$target" ]] ||
      die "$EXIT_CONFIG" "$path is a symlink to $current, want $target (cache parity)"
  elif [[ -e $path ]]; then
    die "$EXIT_CONFIG" "$path exists and is not a symlink; want a symlink to $target (cache parity)"
  else
    ln -s -- "$target" "$path" || die "$EXIT_CONFIG" "cannot create symlink $path -> $target"
  fi
}

ensure_link "$HOME/work" "$scratch/_work"
if ((cache_in_ram)); then
  # Optional RAM build cache (RUNNER_BUILD_CACHE_IN_RAM=true) for a small /scratch:
  # /scratch/.cache itself becomes a link to a directory on the in-memory root
  # filesystem. ~/.cache still points to /scratch/.cache, so cache parity holds:
  # tar follows links in parent components, and an archive entry
  # ../../../.cache/go-build/X lands in RAM. Everything under ~/.cache (the Go build
  # cache, golangci-lint's analysis cache) then counts against the container's
  # memory limit; see docs/architecture.md, "Constraints".
  ensure_link "$scratch/.cache" "$ram_cache_dir"
  log "build cache in RAM: $scratch/.cache -> $ram_cache_dir (counts against the memory limit)"
fi
ensure_link "$HOME/.cache" "$scratch/.cache"
ensure_link "$HOME/go" "$scratch/go"
ensure_link "$HOME/.npm" "$scratch/.npm"
unset XDG_CACHE_HOME

export RUNNER_WORKSPACE="$HOME/work"
export RUNNER_TEMP="$scratch/_temp"
export TMPDIR="$scratch/_temp"
export GOPATH="$HOME/go"
export GOCACHE="$HOME/.cache/go-build"
export GOMODCACHE="$HOME/go/pkg/mod"
export GOLANGCI_LINT_CACHE="$HOME/.cache/golangci-lint"

# The runner's _diag (its own logs) is NOT on /scratch (see Dockerfile): a job that
# fills /scratch must not take the runner, and the job's log, down with it.
mkdir -p -- "$RUNNER_TEMP" "$GOCACHE" "$GOMODCACHE" "$GOLANGCI_LINT_CACHE"
log "work folder, temp and caches on $scratch ($scratch_fstype) via $HOME/{work,.cache,go,.npm}"

# Check the runner before spending the single-use token.
if [[ ! -x $runner_dir/run.sh ]]; then
  die "$EXIT_CONFIG" "$runner_dir/run.sh not found or not executable"
fi
cd -- "$runner_dir"

if ((warmup)); then
  log "warm-up OK: image imported, $scratch mounted and writable, HOME links in place; exiting 0 before any claim"
  exit 0
fi

# The runner-output FIFOs (section 5) are created before the claim, so a failure here
# still exits 2 with the single-use token unspent.
fifo_dir=$(mktemp -d "$fifo_tmpdir/cr-ci-entrypoint.XXXXXX") ||
  die "$EXIT_CONFIG" "cannot create a private temp directory"
out_fifo="$fifo_dir/runner-output"
done_fifo="$fifo_dir/watcher-done"
idle_fifo="$fifo_dir/idle-wait"
listener_code_file="$fifo_dir/listener-exit-code"
job_started_file="$fifo_dir/job-started"
idle_stopped_file="$fifo_dir/idle-stopped"
guard_marker="$fifo_dir/job-guard-refused"
mkfifo -m 600 -- "$out_fifo" "$done_fifo" "$idle_fifo" || die "$EXIT_CONFIG" "cannot create FIFOs in $fifo_dir"

# --- 4. Claim exchange (claim contract v2) -----------------------------------

claim_url="$claim_base/internal/claim"
identity_url="$metadata_base/computeMetadata/v1/instance/service-accounts/default/identity"
id_token=""
jit_config=""

# All curl calls start with -q, which must stay first: it stops ~/.curlrc
# from turning on verbose output, which would print request headers (the
# tokens) or response bodies.

# Fetch this execution's Google ID token for audience MEDIATOR_URL. Sets the
# globals http_code and body (the token on 200). The request carries no
# secret; the response is the secret and stays in memory. The metadata
# server is link-local, so never go through a proxy.
identity_once() {
  local out rc=0
  out=$(
    curl -q --silent --show-error --noproxy '*' \
      --get --data-urlencode "audience=$claim_base" --data-urlencode "format=full" \
      --header 'Metadata-Flavor: Google' \
      --connect-timeout 5 --max-time 15 \
      --write-out '\n%{http_code}' \
      -- "$identity_url"
  ) || rc=$?
  if ((rc != 0)); then
    http_code=000
    body=""
    log "identity token fetch failed: curl exit $rc (connection error or timeout)"
    return 0
  fi
  http_code=${out##*$'\n'}
  body=${out%$'\n'*}
}

# One claim request. Sets the globals http_code, content_type and body. Both
# headers come from a curl config on stdin, so neither token appears in argv
# (/proc/<pid>/cmdline).
claim_once() {
  local out rc=0
  out=$(
    printf 'header = "Authorization: Bearer %s"\nheader = "X-Runner-Claim: %s"\n' \
      "$id_token" "$RUNNER_CLAIM_TOKEN" |
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

# retry_or_die <step>: called after a transient failure (5xx or connection
# error) of <step>. Metadata and claim failures share one budget of
# CLAIM_MAX_ATTEMPTS attempts.
retry_or_die() {
  if ((attempt >= CLAIM_MAX_ATTEMPTS)); then
    die "$EXIT_CLAIM" "claim failed after $attempt attempts (last: $1 status $http_code); not starting the runner"
  fi
  log "attempt $attempt: $1 status $http_code; retrying in ${delay}s"
  exit_if_signalled
  sleep "$delay"
  exit_if_signalled
  attempt=$((attempt + 1))
  delay=$(awk -v d="$delay" 'BEGIN { print d * 2 }')
}

attempt=1
delay=$backoff
while :; do
  exit_if_signalled
  # The ID token is fetched once (valid for an hour) and reused on claim retries.
  if [[ -z $id_token ]]; then
    identity_once
    case $http_code in
      200)
        # Tolerate trailing CR/LF after the token, then require a JWT: three
        # base64url segments. Anything else is not sent anywhere.
        while [[ $body == *$'\n' || $body == *$'\r' ]]; do body=${body%?}; done
        if [[ ! $body =~ ^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$ ]]; then
          body=""
          die "$EXIT_CLAIM" "metadata server returned 200 without an ID token; not starting the runner"
        fi
        id_token=$body
        body=""
        ;;
      000 | 5??)
        retry_or_die "metadata server"
        continue
        ;;
      *)
        die "$EXIT_CLAIM" "metadata server returned status $http_code for the ID token; not starting the runner"
        ;;
    esac
  fi

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
    401)
      die "$EXIT_CLAIM" "mediator rejected the runner identity (401); not starting the runner"
      ;;
    410)
      die "$EXIT_CLAIM" "claim token unknown, expired or already used (410); not starting the runner"
      ;;
    000 | 5??)
      retry_or_die "claim"
      ;;
    *)
      die "$EXIT_CLAIM" "claim returned unexpected status $http_code; not starting the runner"
      ;;
  esac
done
body=""
id_token=""

# The token is spent; keep it out of the job's environment.
unset RUNNER_CLAIM_TOKEN

# --- 5. Run the runner and report its real exit code --------------------------
# run.sh is started as a child, not exec'd, for two reasons:
#   - actions/runner's run-helper.sh maps any Runner.Listener exit code it does not
#     know (an abort, 134, included) to 0, and run.sh then exits 0 too. An aborted
#     runner would therefore look like a successful Cloud Run execution. The output
#     is watched for run-helper's "Exiting with unknown error code: N" line, and N
#     becomes this script's exit code when run.sh itself exits 0.
#   - Cloud Run delivers SIGTERM (cancel, task timeout) to PID 1, this script. It is
#     forwarded to run.sh, whose RUNNER_MANUALLY_TRAP_SIG handler turns it into a
#     graceful job cancel (SIGINT to the runner's process group). Job control (set -m)
#     starts run.sh in its own process group with SIGINT NOT ignored; a plain
#     background job would inherit an ignored SIGINT, and the graceful cancel would
#     silently stop working.
# The JIT config is in run.sh's argv (the runner requires it); it is single use and
# never printed.
# Opened read-write, so neither side ever blocks on open.
exec {done_fd}<>"$done_fifo"

# Copies the runner's output to stdout line by line and records the exit code that
# run-helper.sh reports as unknown. Signals a done marker at end of input.
watch_runner_output() {
  local line
  while IFS= read -r line || [[ -n $line ]]; do
    printf '%s\n' "$line"
    # Exactly run-helper.sh's line (anchored, a decimal number without leading zeros,
    # so (( )) never reads it as octal); nothing else is captured.
    if [[ $line =~ ^Exiting\ with\ unknown\ error\ code:\ (0|[1-9][0-9]{0,2})$ ]]; then
      printf '%s\n' "${BASH_REMATCH[1]}" >"$listener_code_file"
    fi
    # The runner listener logs "<time>: Running job: <name>" when it starts a job.
    if [[ ! -e $job_started_file && $line == *"Running job: "* ]]; then
      : >"$job_started_file"
    fi
  done <"$out_fifo"
  printf 'done\n' >&"$done_fd"
}

# Last checkpoint before anything is started: an early signal must not leave the
# watcher behind, blocked on the FIFO.
exit_if_signalled
watch_runner_output &
watcher_pid=$!

# The job-started hook (cr-ci-job-guard.sh): Runner.Worker runs it before any step of an
# assigned job (it reads ACTIONS_RUNNER_HOOK_JOB_STARTED from its own environment, which
# it inherits from run.sh; a workflow cannot change it). A job from a repository not in
# GH_REPOS, or one that is not a same-repo push or pull_request, is refused with a
# hard stop: the hook writes the marker and SIGKILLs Runner.Worker and run.sh's process
# group (section 5, "Job guard").
# Read-only and exported: the verified hook path (section 1), the marker, and the
# repositories this runner serves (GH_REPOS), which GITHUB_REPOSITORY must be one of.
readonly ACTIONS_RUNNER_HOOK_JOB_STARTED="$job_guard_hook" CR_JOB_GUARD_MARKER="$guard_marker" CR_JOB_GUARD_REPOS="$GH_REPOS"
export ACTIONS_RUNNER_HOOK_JOB_STARTED CR_JOB_GUARD_MARKER CR_JOB_GUARD_REPOS

log "starting run.sh (ephemeral JIT runner)"
set -m
# Opened read-write (never blocks); nothing is ever written, so read -t on it is a
# timer that needs no external sleep.
exec {idle_fd}<>"$idle_fifo"
# {done_fd}>&- {idle_fd}>&-: the watcher's FIFOs are not passed on to run.sh or the job steps.
./run.sh --jitconfig "$jit_config" >"$out_fifo" 2>&1 {done_fd}>&- {idle_fd}>&- &
runner_pid=$!
set +m
jit_config=""
# A signal between the last checkpoint and runner_pid being set was only recorded.
if [[ -n $pending_signal ]]; then
  log "forwarding SIG$pending_signal, which arrived while run.sh was starting"
  kill -s "$pending_signal" "$runner_pid" 2>/dev/null || true
fi

# Idle deadline. GitHub may hand ANY queued job with this label to an idle runner, and
# that job only gets what is left of this execution's task timeout. A runner that has
# not started a job within RUNNER_IDLE_TIMEOUT_SECONDS of the container start ($SECONDS
# counts from the script start, so the claim and checks count too) is stopped gracefully
# (SIGTERM to run.sh, as for a cancel), so a job never inherits less than TASK_TIMEOUT
# minus the idle timeout. The watchdog is a subshell timing out on read -t: no child
# process to orphan.
idle_left=$((idle_timeout - SECONDS))
((idle_left >= 1)) || idle_left=1
(
  trap - TERM INT
  read -r -t "$idle_left" -u "$idle_fd" _ || true
  [[ -e $job_started_file ]] && exit 0
  # Not atomic with the TERM below: a job assigned now, or whose "Running job:" line is
  # still in the pipe, is cancelled by it. The exit logic reports that as a race (exit 4).
  : >"$idle_stopped_file"
  log "idle timeout: no job started within ${idle_timeout}s of the container start; stopping the runner gracefully"
  kill -s TERM "$runner_pid" 2>/dev/null || true
) {done_fd}>&- &
idle_pid=$!

# Job guard enforcer (fail closed). The hook stops the runner itself; if its kill did not
# take effect, this subshell (PID 1's child, same uid) sees the marker and SIGKILLs
# run.sh's process group (run.sh leads its own group: set -m above) until it is gone, so
# no later step of a refused job can run. Polls once a second with read -t (no external
# sleep). The mediator's hijack guard deletes the refused job's registration.
(
  trap - TERM INT
  until [[ -e $guard_marker ]]; do
    kill -0 "$runner_pid" 2>/dev/null || exit 0
    read -r -t 1 -u "$idle_fd" _ || true
  done
  while kill -0 -- "-$runner_pid" 2>/dev/null; do
    kill -KILL -- "-$runner_pid" 2>/dev/null || true
    read -r -t 1 -u "$idle_fd" _ || true
  done
) {done_fd}>&- &
guard_pid=$!

runner_rc=0
while :; do
  if wait "$runner_pid"; then runner_rc=0; else runner_rc=$?; fi
  # wait returns early (128+n) when a trapped signal arrives while run.sh still runs.
  kill -0 "$runner_pid" 2>/dev/null || break
done
# If a trapped signal interrupted wait and run.sh exited before the kill -0 check,
# runner_rc holds the interrupted wait's 128+n. Waiting once more returns run.sh's
# real status, which bash keeps; 127 means it was already collected above.
if wait "$runner_pid" 2>/dev/null; then
  runner_rc=0
else
  rc=$?
  ((rc == 127)) || runner_rc=$rc
fi
trap - TERM INT
kill "$idle_pid" 2>/dev/null || true
kill "$guard_pid" 2>/dev/null || true

# Let the watcher drain the last lines (bounded: a process left holding the pipe
# open must not keep the execution alive).
read -r -t "$watcher_grace" -u "$done_fd" _ || log "warning: runner output watcher did not finish in time"
listener_code=""
[[ -s $listener_code_file ]] && listener_code=$(<"$listener_code_file")
idle_stopped=0
[[ -e $idle_stopped_file ]] && idle_stopped=1
job_started=0
[[ -e $job_started_file ]] && job_started=1
guard_refused=""
if [[ -e $guard_marker ]]; then
  # Written by the hook; sanitized again here (letters, digits, ._/:=- and spaces only,
  # at most 300 characters), since its fields come from GitHub and from a fork's name.
  # It holds the reason and the refused job's own event, head and repository, never the
  # served list. It sits in this script's private temp dir; the job runs as the same uid
  # but never gets to read it: the hook writes it and then kills the worker before any
  # step of the refused job runs.
  guard_refused=$(head -n 1 -- "$guard_marker" | tr -d '\n' | head -c 300 | tr -c 'A-Za-z0-9._/:= -' '_')
  [[ -n $guard_refused ]] || guard_refused="(empty marker)"
fi
kill "$watcher_pid" 2>/dev/null || true
rm -rf -- "$fifo_dir"

if [[ -n $guard_refused ]]; then
  # The job-started hook refused the job GitHub assigned to this runner (not a same-repo
  # push or pull_request of a served repository) and the runner was stopped before any of
  # its steps ran. This line goes to the container log (Cloud Logging) only, after run.sh
  # has died, never to the job's log; so only here is the served list (GH_REPOS, validated
  # in section 1; under org scope it can name private repositories) added. The hook
  # prints only the reason code into the job's log.
  log "ERROR job_guard_refused: the job-started hook refused the job assigned to this runner ($guard_refused served=$GH_REPOS); runner stopped before any step; exiting $EXIT_JOB_REFUSED"
  exit "$EXIT_JOB_REFUSED"
fi
if ((idle_stopped && job_started)); then
  # The watchdog found no job and sent SIGTERM, but a job started in that gap (or its
  # "Running job:" line was still in the pipe): the SIGTERM cancelled that job. Rare, and
  # not reversible; fail the execution so it does not pass for an idle stop.
  log "ERROR idle_timeout_raced_job: the idle stop after ${idle_timeout}s raced a job start; the job was cancelled (run.sh exited $runner_rc); exiting $EXIT_IDLE_RACE"
  exit "$EXIT_IDLE_RACE"
fi
if ((idle_stopped)); then
  # Expected, not a failure: the unused registration is reaped by the mediator's next
  # /reconcile (offline, idle, older than the reap age), which also re-serves any job
  # still queued for this label.
  log "idle_timeout: runner stopped after ${idle_timeout}s without a job (run.sh exited $runner_rc); exiting 0"
  exit 0
fi
if ((runner_rc != 0)); then
  log "run.sh exited $runner_rc"
  exit "$runner_rc"
fi
if [[ $listener_code =~ ^[0-9]+$ ]] && ((listener_code != 0)); then
  log "run.sh exited 0, but run-helper.sh reported the runner's unknown exit code $listener_code; exiting $listener_code so the execution shows as failed"
  exit $((listener_code > 255 ? 1 : listener_code))
fi
log "run.sh exited 0"
exit 0
