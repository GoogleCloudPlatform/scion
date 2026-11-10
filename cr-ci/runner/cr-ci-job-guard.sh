#!/usr/bin/env bash
# cr-ci-job-guard.sh: the runner's job-started hook (ACTIONS_RUNNER_HOOK_JOB_STARTED,
# exported read-only by entrypoint.sh, which refuses to start the runner unless this file
# is root-owned, not writable by the runner, and matches the sha256 pinned at image
# build). Runner.Worker runs it after a job is assigned and before any step of that job
# (actions/runner 2.338.0, src/Runner.Worker/JobExtension.cs L303-312, L585-586). It lets
# the job run only if it is what cr-ci serves:
#   - GITHUB_REPOSITORY is one of CR_JOB_GUARD_REPOS (comma-separated owner/name), the
#     repositories whose jobs the runner may take: its registration scope (GH_REPOS from
#     the mediator: the one repository in repo scope, every target repository of the
#     owner in org scope; exported read-only by entrypoint.sh), and
#   - GITHUB_EVENT_NAME push, or
#   - GITHUB_EVENT_NAME pull_request whose event payload (GITHUB_EVENT_PATH) has
#     pull_request.head.repo.full_name equal to GITHUB_REPOSITORY (not a fork).
# This is the same allowlist as the workflows' routing expression and the mediator's
# forkReason. A job GitHub hands to an idle cr-ci runner that is anything else (a fork
# pull request, pull_request_target, workflow_run, issue_comment, another repository's
# job, ...) is refused.
#
# Refusing is a HARD STOP, not "exit 1": a failed hook step only marks the job failed,
# and the worker would still run later steps whose condition is always(), failure() or
# !cancelled() (src/Runner.Worker/StepsRunner.cs L200-235), and those are under the
# workflow author's control. So the hook writes the marker CR_JOB_GUARD_MARKER (read by
# entrypoint.sh, which logs the refusal and exits 5) and SIGKILLs, in this order, its
# parent (Runner.Worker), the parent's process group, and its own group unless that group
# is its alone. The runner spawns children with no new session or group
# (src/Runner.Sdk/ProcessInvoker.cs L226-298), so normally both groups are run.sh's
# (Runner.Listener, Runner.Worker, this hook) and the hook dies last. Killing the parent
# first means the stop does not depend on the hook sharing the worker's group: in a group
# of its own, it still kills the worker and its group, then confirms they are gone. The
# worker cannot reach another step meanwhile: it is waiting for this hook to exit
# (src/Runner.Worker/JobHookProvider.cs, RunHook awaits handler.RunAsync).
# Fail closed: an unreadable or malformed payload refuses; if the kills do not take
# effect the hook retries for 10 s, then exits 1; and entrypoint.sh enforces the stop
# itself (SIGKILL to run.sh's group) when it sees the marker.
#
# The runner runs the hook as "bash -e" (src/Runner.Worker/Handlers/ScriptHandler.cs
# L213-216, ScriptHandlerHelpers.cs L19), so errexit is switched off first: no failing
# command may end the hook before or during the stop sequence. Nothing here relies on
# errexit; every check is explicit.
#
# Needs bash and jq (in the image). What this hook prints goes to the REFUSED job's log,
# which anyone who can see the job may read (for a fork pull request, the public), so it
# prints only the reason code, one of the fixed words below. The marker gets the reason
# and the job's own sanitized event, head and repository, which entrypoint.sh logs on the
# runner host (Cloud Logging) after run.sh has died, never to the job. The served list
# (under org scope it can name private repositories) is written by this hook nowhere:
# entrypoint.sh appends it from its own GH_REPOS to that host-side line. Neither output
# ever holds a token or the JIT config.
set +e -o pipefail

event=${GITHUB_EVENT_NAME:-}
repo=${GITHUB_REPOSITORY:-}
payload=${GITHUB_EVENT_PATH:-}
served=${CR_JOB_GUARD_REPOS:-}
head=""
reason=""

# served_has REPO: REPO is in the served list (case-insensitive, whole entries only).
served_has() {
  local entry
  local -a entries
  IFS=, read -r -a entries <<<"$served"
  for entry in "${entries[@]}"; do
    [[ ${entry,,} == "${1,,}" ]] && return 0
  done
  return 1
}

if [[ -z $served ]]; then
  reason=served_repositories_missing
elif [[ ! $served =~ ^[A-Za-z0-9-]+/[A-Za-z0-9._-]+(,[A-Za-z0-9-]+/[A-Za-z0-9._-]+)*$ ]]; then
  reason=served_repositories_malformed
elif [[ -z $repo ]]; then
  reason=repository_missing
elif ! served_has "$repo"; then
  reason=repository_not_served
else
  case $event in
    push) ;;
    pull_request)
      if [[ -z $payload || ! -r $payload ]]; then
        reason=payload_missing
      elif ! head=$(jq -er '.pull_request.head.repo.full_name // ""' "$payload" 2>/dev/null); then
        reason=payload_unparsable
      elif [[ -z $head ]]; then
        reason=head_repository_missing
      elif [[ ${head,,} != "${repo,,}" ]]; then
        reason=head_repository_differs
      fi
      ;;
    *) reason=event_not_allowed ;;
  esac
fi

[[ -z $reason ]] && exit 0

# clean VALUE: at most 100 characters from a fixed safe set, for the marker line
# (event and head come from GitHub, the head repository name from a fork).
clean() { printf '%s' "${1:0:100}" | tr -c 'A-Za-z0-9._/:-' '_'; }

# Host side only (the marker): the job's own details, no served list.
line="reason=$reason event=$(clean "$event") head=$(clean "$head") repository=$(clean "$repo")"
if [[ -n ${CR_JOB_GUARD_MARKER:-} ]]; then
  printf '%s\n' "$line" >"$CR_JOB_GUARD_MARKER" 2>/dev/null
fi
# Job log: the reason code only.
echo "::error title=cr-ci job guard::job refused by the cr-ci runner's job guard (reason=$reason)"

# pgid PID: the process group of PID (field 5 of /proc/PID/stat; the command name in
# field 2 is in parentheses and may hold spaces, so cut after the last ')').
pgid() {
  local stat g
  stat=$(<"/proc/$1/stat") || return 1
  read -r _ _ g _ <<<"${stat##*) }"
  [[ $g =~ ^[0-9]+$ ]] && printf '%s' "$g"
}
own_pgid=$(pgid $$)
parent=$PPID
parent_pgid=$(pgid "$parent")

for _ in 1 2 3 4 5 6 7 8 9 10; do
  # Never PID 1 (the entrypoint) or its group: the entrypoint reports the refusal.
  if ((parent > 1)); then
    kill -KILL "$parent" 2>/dev/null
  fi
  if [[ -n $parent_pgid && $parent_pgid != 1 && $parent_pgid != "$own_pgid" ]]; then
    kill -KILL -- "-$parent_pgid" 2>/dev/null
  fi
  # Our own group last, and only if it is not ours alone: killing that would end the
  # retries and stop nothing else.
  if [[ -n $own_pgid && $own_pgid != 1 && $own_pgid != "$$" ]]; then
    kill -KILL -- "-$own_pgid" 2>/dev/null
  fi
  sleep 1
  # Still here (a group of our own): done once the parent and its group are gone.
  if ! kill -0 "$parent" 2>/dev/null &&
    { [[ -z $parent_pgid ]] || ! kill -0 -- "-$parent_pgid" 2>/dev/null; }; then
    echo "::error title=cr-ci job guard::runner stopped" >&2
    exit 1
  fi
done
echo "::error title=cr-ci job guard::could not stop the runner; failing the job" >&2
exit 1
