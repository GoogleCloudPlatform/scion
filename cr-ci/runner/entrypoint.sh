#!/usr/bin/env bash
# cr-ci runner entrypoint (PID 1 of every runner execution).
#
#   1. Verify that /scratch is a mounted, writable volume (the Cloud Run
#      ephemeral-disk). The container root filesystem is in memory on Cloud
#      Run, so running without /scratch would silently eat RAM. Fail loudly.
#   2. Link ~/work, ~/.cache and ~/go into /scratch and export ubuntu-latest's cache
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
# Required env (set by the mediator as Cloud Run Job overrides):
#   RUNNER_CLAIM_TOKEN  32 lowercase hex chars, single use
#   MEDIATOR_URL        https origin of the mediator, no trailing slash
#   GH_JOB_ID           GitHub workflow job ID (logging only)
#   RUNNER_NAME         cr-job-<GH_JOB_ID> (logging only)
#
# Exit codes:
#   2  configuration error: missing or invalid env (HOME included), /scratch missing or
#      not writable, a HOME link (~/work, ~/.cache, ~/go) that is not the expected symlink
#   3  claim failed: 401, 410, other non-retryable response (mediator or
#      metadata server), or retries exhausted; run.sh not started
#   otherwise run.sh's exit code, or, when run.sh exits 0, the runner exit code that
#      run-helper.sh reported as unknown (for example 134 after an abort)
#
# Overrides, for the offline test harness (runner/entrypoint_test.go) only;
# production leaves them unset:
#   CR_SCRATCH_DIR           scratch mount point          (default /scratch)
#   CR_MOUNTINFO             mount table to check against (default /proc/self/mountinfo)
#   CR_RUNNER_DIR            directory holding run.sh     (default /home/runner/actions-runner)
#   CR_CLAIM_BACKOFF_SECONDS first retry delay, doubled per retry (default 1)
#   CR_EXPECTED_HOME         required value of HOME       (default /home/runner)
#   CR_METADATA_URL          metadata server origin       (default http://metadata.google.internal;
#                            otherwise only http://127.0.0.1[:port] or http://localhost[:port])
#   CR_FIFO_TMPDIR           where the runner-output FIFOs go (default /tmp)
#   CR_WATCHER_GRACE_SECONDS how long to wait for the output watcher (default 5)
# The harness also puts stub `stat` and `sleep` commands first on PATH.
set -euo pipefail

readonly CLAIM_MAX_ATTEMPTS=5
readonly EXIT_CONFIG=2
readonly EXIT_CLAIM=3

scratch=${CR_SCRATCH_DIR:-/scratch}
mountinfo=${CR_MOUNTINFO:-/proc/self/mountinfo}
runner_dir=${CR_RUNNER_DIR:-/home/runner/actions-runner}
backoff=${CR_CLAIM_BACKOFF_SECONDS:-1}
metadata_base=${CR_METADATA_URL:-http://metadata.google.internal}
expected_home=${CR_EXPECTED_HOME:-/home/runner}

log() { printf 'cr-ci-entrypoint: %s\n' "$*" >&2; }
die() {
  local code=$1
  shift
  log "FATAL: $*"
  exit "$code"
}

# --- 1. Required env ----------------------------------------------------------

for var in RUNNER_CLAIM_TOKEN MEDIATOR_URL GH_JOB_ID RUNNER_NAME HOME; do
  if [[ -z ${!var:-} ]]; then
    die "$EXIT_CONFIG" "required environment variable $var is not set"
  fi
done

if [[ ! $RUNNER_CLAIM_TOKEN =~ ^[0-9a-f]{32}$ ]]; then
  # Never echo the value, not even a malformed one.
  die "$EXIT_CONFIG" "RUNNER_CLAIM_TOKEN is not 32 lowercase hex characters"
fi
# Cache parity needs ubuntu-latest's HOME (set in the image config).
if [[ $HOME != "$expected_home" ]]; then
  die "$EXIT_CONFIG" "HOME is '$HOME', want $expected_home (cache paths must match ubuntu-latest)"
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

if [[ $metadata_base != http://metadata.google.internal &&
  ! $metadata_base =~ ^http://(127\.0\.0\.1|localhost)(:[0-9]+)?$ ]]; then
  die "$EXIT_CONFIG" "CR_METADATA_URL must be http://metadata.google.internal or a loopback origin, got '$metadata_base'"
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
ensure_link "$HOME/.cache" "$scratch/.cache"
ensure_link "$HOME/go" "$scratch/go"
unset XDG_CACHE_HOME

export RUNNER_WORKSPACE="$HOME/work"
export RUNNER_TEMP="$scratch/_temp"
export TMPDIR="$scratch/_temp"
export GOPATH="$HOME/go"
export GOCACHE="$HOME/.cache/go-build"
export GOMODCACHE="$HOME/go/pkg/mod"
export GOLANGCI_LINT_CACHE="$HOME/.cache/golangci-lint"

# _diag is the target of the runner's _diag symlink (runner logs; see Dockerfile).
mkdir -p -- "$RUNNER_TEMP" "$GOCACHE" "$GOMODCACHE" "$GOLANGCI_LINT_CACHE" "$scratch/_diag"
log "work folder, temp and caches on $scratch ($scratch_fstype) via $HOME/{work,.cache,go}"

# Check the runner before spending the single-use token.
if [[ ! -x $runner_dir/run.sh ]]; then
  die "$EXIT_CONFIG" "$runner_dir/run.sh not found or not executable"
fi
cd -- "$runner_dir"

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
  sleep "$delay"
  attempt=$((attempt + 1))
  delay=$(awk -v d="$delay" 'BEGIN { print d * 2 }')
}

attempt=1
delay=$backoff
while :; do
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
fifo_dir=$(mktemp -d "${CR_FIFO_TMPDIR:-/tmp}/cr-ci-entrypoint.XXXXXX") ||
  die "$EXIT_CONFIG" "cannot create a private temp directory"
out_fifo="$fifo_dir/runner-output"
done_fifo="$fifo_dir/watcher-done"
listener_code_file="$fifo_dir/listener-exit-code"
mkfifo -m 600 -- "$out_fifo" "$done_fifo" || die "$EXIT_CONFIG" "cannot create FIFOs in $fifo_dir"
# Opened read-write, so neither side ever blocks on open.
exec {done_fd}<>"$done_fifo"

# Copies the runner's output to stdout line by line and records the exit code that
# run-helper.sh reports as unknown. Signals a done marker at end of input.
watch_runner_output() {
  local line
  while IFS= read -r line || [[ -n $line ]]; do
    printf '%s\n' "$line"
    # Exactly run-helper.sh's line (anchored, digits only); nothing else is captured.
    if [[ $line =~ ^Exiting\ with\ unknown\ error\ code:\ ([0-9]{1,3})$ ]]; then
      printf '%s\n' "${BASH_REMATCH[1]}" >"$listener_code_file"
    fi
  done <"$out_fifo"
  printf 'done\n' >&"$done_fd"
}

watch_runner_output &
watcher_pid=$!

log "starting run.sh (ephemeral JIT runner)"
set -m
./run.sh --jitconfig "$jit_config" >"$out_fifo" 2>&1 &
runner_pid=$!
set +m
jit_config=""

# shellcheck disable=SC2329 # invoked by the traps below
forward_signal() {
  log "received SIG$1; forwarding it to run.sh (graceful job cancel)"
  kill -s "$1" "$runner_pid" 2>/dev/null || true
}
trap 'forward_signal TERM' TERM
trap 'forward_signal INT' INT

runner_rc=0
while :; do
  if wait "$runner_pid"; then runner_rc=0; else runner_rc=$?; fi
  # wait returns early (128+n) when a trapped signal arrives while run.sh still runs.
  kill -0 "$runner_pid" 2>/dev/null || break
done
trap - TERM INT

# Let the watcher drain the last lines (bounded: a process left holding the pipe
# open must not keep the execution alive).
read -r -t "${CR_WATCHER_GRACE_SECONDS:-5}" -u "$done_fd" _ || log "warning: runner output watcher did not finish in time"
listener_code=""
[[ -s $listener_code_file ]] && listener_code=$(<"$listener_code_file")
kill "$watcher_pid" 2>/dev/null || true
rm -rf -- "$fifo_dir"

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
