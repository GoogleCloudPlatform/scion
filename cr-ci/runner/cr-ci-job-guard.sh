#!/usr/bin/env bash
# cr-ci-job-guard.sh: the runner's job-started hook (ACTIONS_RUNNER_HOOK_JOB_STARTED,
# exported by entrypoint.sh). Runner.Worker runs it after a job is assigned and before
# any step of that job (actions/runner 2.338.0, src/Runner.Worker/JobExtension.cs
# L303-312, L585-586). It lets the job run only if it is what cr-ci serves:
#   - GITHUB_EVENT_NAME push, or
#   - GITHUB_EVENT_NAME pull_request whose event payload (GITHUB_EVENT_PATH) has
#     pull_request.head.repo.full_name equal to GITHUB_REPOSITORY (not a fork).
# This is the same allowlist as the workflows' routing expression and the mediator's
# forkReason. A job GitHub hands to an idle cr-ci runner that is anything else (a fork
# pull request, pull_request_target, workflow_run, issue_comment, ...) is refused.
#
# Refusing is a HARD STOP, not "exit 1": a failed hook step only marks the job failed,
# and the worker would still run later steps whose condition is always(), failure() or
# !cancelled() (src/Runner.Worker/StepsRunner.cs L200-235), and those are under the
# workflow author's control. So the hook writes the marker CR_JOB_GUARD_MARKER (read by
# entrypoint.sh, which logs the refusal and exits 5) and SIGKILLs its own process group:
# run.sh's group, which holds Runner.Listener, Runner.Worker and this hook (the runner
# spawns children with no new session or group: src/Runner.Sdk/ProcessInvoker.cs
# L226-298). The worker cannot reach another step meanwhile: it is waiting for this hook
# to exit (src/Runner.Worker/JobHookProvider.cs, RunHook awaits handler.RunAsync).
# Fail closed: an unreadable or malformed payload refuses; if the kill does not take
# effect the hook keeps trying, then exits 1; and entrypoint.sh enforces the stop itself
# when it sees the marker.
#
# Needs bash and jq (in the image). The marker holds only sanitized fields (reason,
# event, head repository), never a token or the JIT config.
set -uo pipefail

event=${GITHUB_EVENT_NAME:-}
repo=${GITHUB_REPOSITORY:-}
payload=${GITHUB_EVENT_PATH:-}
head=""
reason=""

if [[ -z $repo ]]; then
  reason=repository_missing
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

# clean VALUE: at most 100 characters from a fixed safe set, for log and marker lines
# (event and head come from GitHub, the head repository name from a fork).
clean() { printf '%s' "${1:0:100}" | tr -c 'A-Za-z0-9._/:-' '_'; }

line="reason=$reason event=$(clean "$event") head=$(clean "$head") repository=$(clean "$repo")"
echo "::error title=cr-ci job guard::this runner runs only same-repo push and pull_request jobs; refused ($line)"
if [[ -n ${CR_JOB_GUARD_MARKER:-} ]]; then
  printf '%s\n' "$line" >"$CR_JOB_GUARD_MARKER" 2>/dev/null || true
fi

# Our process group (field 5 of /proc/self/stat; the command name in field 2 is in
# parentheses and may hold spaces, so cut after the last ')').
stat=$(</proc/$$/stat)
read -r _ _ pgid _ <<<"${stat##*) }"
for _ in 1 2 3 4 5 6 7 8 9 10; do
  kill -KILL -- "-$pgid" 2>/dev/null
  kill -KILL "$PPID" 2>/dev/null
  sleep 1
done
echo "::error title=cr-ci job guard::could not stop the runner; failing the job" >&2
exit 1
