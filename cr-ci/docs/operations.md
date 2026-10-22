# Operations

> Procedures use the P4 scripts and the P5 GitHub setup; items marked
> TODO(P6) are filled in after the PoC. The mediator behaviour described here
> (P2) is final. Roles for every operation:
> [permissions-matrix.md, section 3](permissions-matrix.md#3-operations-at-a-glance).

## Rollout

Per repository, in this order (details in
[github-setup.md](github-setup.md)):

1. The App is installed on the repo, the repo is in `TARGET_REPOS`, and a
   redelivered `ping` gets 200 ([step 5](github-setup.md#5-check-the-webhook)).
2. The [deploy preconditions](architecture.md#deploy-preconditions) hold,
   above all "Require approval for **all** external contributors".
3. The P6 [PoC checklist](poc-checklist.md) passed on this target.
4. `ci.yml` carries the routing expression (P7). Until then, the variable
   changes nothing.
5. `scripts/set-routing-variable.sh --repo <owner/repo> --apply` sets
   `ROUTING_VARIABLE_NAME` to `RUNNER_LABEL` (needs write access).
6. Watch for 48 hours: `dispatch` and `claim_consumed` for every routed job,
   no `launch_failed`, no `SECURITY_*` events, no `capacity_full` lasting
   across reconciles, and lint durations and memory as in the PoC. The current
   scope is `ptone/scion` only; upstream is future work
   ([matrix 0](permissions-matrix.md#0-summary-for-the-fork-vs-upstream-decision)).

## Fallback

- **Back to GitHub-hosted runners (seconds, no commit):**
  `scripts/set-routing-variable.sh --repo <owner/repo> --clear --apply`
  (same as `gh variable delete SCION_LARGE_RUNNER --repo <owner/repo>`).
  This needs write access. It affects newly queued jobs only. Cancel and
  re-run any job still queued on the label: it waits for a cr-ci runner, and
  GitHub drops it after 24 h.
- **To a larger GitHub-hosted runner** (if the org has one): set the variable
  to that runner's label instead of clearing it.
- **Emergency stop:** suspend or uninstall the App (G17). The mediator then
  receives no webhooks and cannot mint runners. Clear the variable as well,
  or routed jobs stay queued.

## Deploy preconditions

Before setting the routing variable on any repository, check the
[deploy preconditions](architecture.md#deploy-preconditions), above all
"Require approval for **all** external contributors" for fork PR workflows.

## Mediator: single instance

The mediator keeps pending claims and its launch records in memory. Deploy it
with **`max-instances=1`** (and `min-instances=0`); a second instance would
answer 410 to claims issued by the first, and capacity checks could race.

A restart (deploy, crash, scale to zero with a claim pending) loses pending
claims. The affected runner gets 410 and exits 3 without running a job; the
next `/reconcile` deletes its offline `cr-job-*` registration once it is five
minutes old and relaunches the still-queued job. Expect up to about two
reconcile intervals of extra latency for such a job, and no manual action.

## Deploy and upgrade

First deployment: [gcp-setup.md](gcp-setup.md), in the order of the
[README quick start](../README.md#quick-start). Every script prints its
commands without `--apply`; read that transcript before applying.

| Change | Command |
| :--- | :--- |
| Any config value the mediator uses (`TARGET_REPOS`, `MAX_CONCURRENT_RUNNERS`, `CLAIM_TTL`, ...) | edit `config.env`, `scripts/deploy-mediator.sh --apply` |
| Mediator code | commit, then `scripts/deploy-mediator.sh --apply` (new tag from the source tree hash) |
| Runner shape (`RUNNER_CPU`, `RUNNER_MEMORY`, `RUNNER_DISK_GI`, `TASK_TIMEOUT`, `RUNNER_IDLE_TIMEOUT`) | edit `config.env`, `scripts/deploy-job.sh --apply` |
| Runner image | see [Runner image updates](#runner-image-updates) |
| `RECONCILE_SCHEDULE` or `MEDIATOR_URL` | `scripts/deploy-scheduler.sh --apply` (after `deploy-mediator.sh` for `MEDIATOR_URL`) |

**Upgrade order: runner first.** When a change touches both, run
`build-runner-image.sh` and `deploy-job.sh` **before** `deploy-mediator.sh`.
The mediator registers runners with the work folder `/home/runner/work`, and
only a runner image that links that folder to the `/scratch` disk (cache
parity) keeps the work folder off the in-memory root filesystem. An older
image with a newer mediator risks OOM on large jobs. `deploy-mediator.sh`
warns when the job's image tag differs from the current `runner/` tree hash.

Every script is idempotent, so re-running one after a partial failure is the
recovery. A mediator deploy restarts the single instance and loses pending
claims; affected jobs are relaunched by the next reconcile
([single instance](#mediator-single-instance)). Deploy outside busy CI
periods when possible.

## Two stacks (region move)

A stack is one `REGION` and `RUNNER_JOB_NAME` (job, mediator, scheduler). Two stacks
may serve the same repos and `RUNNER_LABEL` for a while, for example while moving
to another region. What is scoped per stack and what is not:

- **Reaping is per stack.** Each runner is registered with the stack label
  `crci-stack-<REGION>-<RUNNER_JOB_NAME>` next to `RUNNER_LABEL` (jobs keep asking
  for `RUNNER_LABEL` only). A reconcile reaps only registrations with its own stack
  label or with none. Deploy this mediator version to **both** stacks before they
  overlap: an older mediator reaps every offline `cr-job-*` registration, the other
  stack's cold-starting runners included.
- **Backfill and the hijack guard are not.** Both reconcilers see the same queued
  jobs and the same `cr-job-*` runners. Runner names are `cr-job-<job id>` in every
  stack and GitHub refuses a second registration with the same name, so one job
  never gets two runners: the second stack sees the name registered and skips it.
  Either stack may serve a job (and its capacity limit applies to its own
  executions), and a hijacked runner is killed by whichever stack sees it first.
- **Webhooks go to one stack**, the App's webhook URL. After changing it, see
  [Webhook URL change](#webhook-url-change).

Procedure: deploy the new stack (`REGION` changed in its `config.env`), switch the
App's webhook URL to its `MEDIATOR_URL`, let the old stack's executions finish,
then remove the old stack's regional resources with `scripts/teardown.sh
--regional-only --config <old config.env> --apply` (it keeps the service accounts,
secrets, staging bucket and project binding the new stack shares).

## Secret rotation

All values go through `scripts/set-secrets.sh` (file at mode 0600, or stdin),
never as an argument ([gcp-setup.md, step 6](gcp-setup.md#6-secrets-scriptsset-secretssh)).

- **Webhook secret:** add the new value as a new Secret Manager version
  (`scripts/set-secrets.sh --webhook-secret-file F --apply`, or
  `--generate-webhook-secret OUT --apply`), then change it in the GitHub App
  settings, back to back. No restart is
  needed: on a signature mismatch the mediator re-reads the secret (at most
  once a minute), and the secret it replaces stays valid for one hour, so
  deliveries signed with either secret are accepted during the switch. If
  the two steps are more than an hour apart, deliveries with the old secret
  start failing (401); redeliver them from the App's "Advanced" page, and
  `/reconcile` backfills their jobs anyway.
- **App private key:** generate a new key in the App settings, add it as a
  new Secret Manager version (`scripts/set-secrets.sh --app-key-file F --apply`),
  restart the mediator with `scripts/deploy-mediator.sh --apply` (it caches
  the parsed key for its lifetime), then delete the old key in GitHub.
  Installation tokens are cached in memory only and refreshed five minutes
  before they expire.
- **Old versions:** after the switch, disable the previous version
  (`gcloud secrets versions disable VERSION --secret NAME --project GCP_PROJECT`,
  C9 `secretVersionManager`). The mediator always reads `latest`.

## Runner image updates

All tool versions and checksums are pinned in one place, the `ARG` block of
[`runner/Dockerfile`](../runner/Dockerfile) ([runner/README.md](../runner/README.md#build-arguments)).

1. Change the version **and** its sha256 there, and commit.
2. `scripts/build-runner-image.sh --apply`: builds and pushes a new tag
   (`git-<tree hash of runner/>`), as `BUILD_SA_NAME`.
3. `scripts/deploy-job.sh --apply --warm-up`: points the job at the new tag and
   runs one no-op execution, so the first real job does not pay the image import.
   New executions use the new tag; running ones finish on the old image.

**Rollback:** `scripts/deploy-job.sh --tag <previous tag> --apply`. Old tags
stay in `ARTIFACT_REPO` (list them with
`gcloud artifacts docker tags list REGION-docker.pkg.dev/GCP_PROJECT/ARTIFACT_REPO/RUNNER_IMAGE_NAME`).

**Bump the actions runner regularly.** GitHub stops sending jobs to runner
versions it considers outdated, and an outdated runner tries to update
itself at start-up on every execution. Check
<https://github.com/actions/runner/releases> at least monthly and bump
`ACTIONS_RUNNER_VERSION` soon after each release. Keep `GO_VERSION` in step
with `go.mod` and `GOLANGCI_LINT_VERSION` with the version CI pins.

## Capacity and quotas

`MAX_CONCURRENT_RUNNERS` caps running executions; jobs above it stay queued
and are dispatched first in, first out as executions finish. Each execution
uses `RUNNER_DISK_GI` of the regional ephemeral-disk quota (default 100 GiB:
5 x 10 GiB fits, 4 x 25 GiB fits). Above 10 GiB per runner, the per-instance
quota must be raised as well. To raise the cap, first request the quota increase
([gcp-setup.md, step 9](gcp-setup.md#9-quotas)), then raise
`MAX_CONCURRENT_RUNNERS` and run `scripts/deploy-mediator.sh --apply`. A
persistent `capacity_full` with few jobs actually running points to stuck
executions; see the troubleshooting section.

## Teardown

To remove cr-ci from a repository, clear its routing variable and uninstall
the App ([github-setup.md, step 10](github-setup.md#10-removing-cr-ci-from-a-repository)).
To remove the GCP side, run `scripts/teardown.sh` and read its transcript,
then run it with `--apply` (it asks you to type `GCP_PROJECT`;
`--keep-secrets` and `--keep-images` retain those; `--regional-only` removes one
region's stack and keeps everything project-wide). It deletes in reverse
order and skips what is gone; APIs stay enabled
([gcp-setup.md, step 10](gcp-setup.md#10-teardown-scriptsteardownsh)).

## Monitoring and alerts

The mediator writes one JSON object per line to stdout (Cloud Logging fields
`severity`, `message`, `time`) with a stable `event` attribute. Secrets,
JWTs, installation tokens, JIT configs, claim tokens and OIDC tokens are never
logged (a unit test enforces this).

| `event` | Severity | Meaning |
| :--- | :--- | :--- |
| `startup` | INFO / WARNING | Config loaded, listening; WARNING if `MEDIATOR_URL` is empty |
| `dispatch` | INFO | Runner registered and execution started (`job_id`, `runner`, `execution`, `active`) |
| `dispatch_skip` | INFO | Duplicate delivery or job already has a runner (`reason`) |
| `capacity_full` | INFO | `MAX_CONCURRENT_RUNNERS` reached; job left queued |
| `launch_failed` | ERROR | `jobs.run` failed; registration deleted, job retried by reconcile |
| `cancel` | INFO | Job finished while queued: its idle runner stopped, or left alone because it runs another job (`action=none_busy`) |
| `claim_consumed` / `claim_rejected` | INFO / WARNING | Runner fetched its JIT config / was refused: `identity_missing` or `identity_invalid` (401, claim kept; repeated ones from outside the runner are worth a look), or an unknown, used or expired token (410) |
| `SECURITY_REJECT_FORK_PR` | WARNING | Fork-PR or `pull_request_target` run refused a runner. **Alert on this.** |
| `SECURITY_KILL_HIJACKED_RUNNER` | ERROR | An untrusted job started on a cr-ci runner minted for another job; the execution was cancelled and the registration deleted ([architecture](architecture.md#residual-risk-label-matched-job-assignment)). **Alert on this and review the run.** |
| `runner_swap` | INFO | A runner started a different legitimate job; harmless. `inherited_budget` is the task time that job has left (`TASK_TIMEOUT` minus `execution_age`) |
| `runner_swap_low_budget` | WARNING | That job inherited less than 20 minutes; if it fails on the task timeout, re-run it. Frequent ones: lower `RUNNER_IDLE_TIMEOUT` or raise `TASK_TIMEOUT` ([architecture](architecture.md#flow)) |
| `webhook_rejected` | WARNING | Bad signature, malformed payload or repo outside `TARGET_REPOS` |
| `webhook_ignored` | INFO / DEBUG / WARNING | Not `workflow_job`, or job without `RUNNER_LABEL`; WARNING with `reason=labels_unservable` when a job has `RUNNER_LABEL` plus other labels a cr-ci runner cannot satisfy (fix its `runs-on`) |
| `reconcile` | INFO | Reconcile summary (reaped, backfilled, left queued, errors, skipped). Any error makes `/reconcile` answer 500; skipped items do not |
| `reconcile_skip` | WARNING | GitHub answered one repo's, run's or job's call in a reconcile with a 5xx (`stage`); that item is skipped and retried next pass, the rest of the pass goes on and `/reconcile` still answers 200 (listed under `skipped`). Only the same `stage` across many passes needs a look |
| `reconcile_denied` | WARNING | `/reconcile` called without a valid scheduler ID token |
| `runner_reaped` / `execution_reaped` | WARNING | Stale registration deleted / execution past the task timeout cancelled |
| `error` | ERROR | A GitHub, Cloud Run or Secret Manager call failed (`stage`, `error`) |

- Suggested alerts: any `SECURITY_REJECT_FORK_PR` or
  `SECURITY_KILL_HIJACKED_RUNNER`; repeated `launch_failed`
  or `error`; `capacity_full` persisting across several reconciles (raise
  capacity or check for stuck executions); missing `reconcile` lines for
  more than two intervals (scheduler broken).
- TODO(P6): execution results and memory utilisation of runner executions.

## Troubleshooting

- **Job stuck "waiting for a runner":** look for its `job_id` in the mediator
  logs. `capacity_full` means it is waiting for a slot; `SECURITY_REJECT_FORK_PR`
  or `labels_unservable` means it will never get one; no line at all means the webhook was not
  delivered (App settings, "Recent deliveries") and the next `/reconcile`
  should pick it up.
- **Job displaced by a hijack waits 5-15 min:** after a hijack kill the
  displaced job is re-served at once; if that re-serve fails (GitHub or Cloud
  Run error, or capacity full), `/reconcile` backfills it only after the
  killed runner's stale registration has been reaped (offline for five
  minutes, checked every `RECONCILE_SCHEDULE` interval), because the
  registration still carries the job's runner name. Look for
  `SECURITY_KILL_HIJACKED_RUNNER` followed by an `error` with
  `stage`/`source=hijack_recovery`; no action is needed.
- **Runner execution ends with `idle_timeout:` in its log:** expected, exit 0.
  The runner started no job within `RUNNER_IDLE_TIMEOUT` of its start (usually
  its job was taken by another runner or cancelled), so it stopped itself rather
  than wait for a job it could only give part of the task timeout. The next
  `/reconcile` reaps its registration (`runner_reaped`) and re-serves its job if
  that is still queued.
- **Legitimate job killed with `reason=run_lookup_failed`:** a GitHub API
  error hit the hijack guard during a runner swap; the guard fails closed.
  Re-run the job.
- **Runner exits 3 after a 503 storm on `/internal/claim`:** Google's signing
  keys could not be fetched (`error` with `stage=claim_identity`); the claim
  is kept and the runner retries 5xx, but five failed attempts exhaust its
  budget. The job is backfilled after its registration is reaped.
- **Webhook deliveries failing with 401:** the webhook secret in Secret
  Manager and in the App settings differ (see rotation above).
- **Webhook deliveries get 404:** the App's webhook URL lacks `/webhook`. It must
  be `MEDIATOR_URL/webhook` (App settings, **General**, **Webhook URL**), then
  redeliver from **Advanced**, **Recent Deliveries**.
- **503 from `/webhook` and `/reconcile` with `not_configured`:** `MEDIATOR_URL`
  is empty; set it and redeploy.
- **Image build fails with "the --chmod option requires BuildKit":** the
  build ran with the legacy Docker builder. `scripts/cloudbuild.yaml` sets
  `DOCKER_BUILDKIT=1` on its docker step, and the `--local` path runs
  `env DOCKER_BUILDKIT=1 docker build`; check that a modified or custom build
  config keeps it.
- **First build after bootstrap fails or has no logs:** the new IAM bindings
  (for example `BUILD_SA_NAME`'s `logging.logWriter`, needed by
  `CLOUD_LOGGING_ONLY`) can take a minute or more to propagate. Wait about
  60 s and re-run the build script; it is idempotent.
- **Quota errors:** `launch_failed` with `RESOURCE_EXHAUSTED` (or `429`)
  from `jobs.run` means a regional Cloud Run quota (ephemeral disk, CPU or
  memory) is used up. The job stays queued and is retried by reconcile; lower
  `MAX_CONCURRENT_RUNNERS` or raise the quota ([Capacity and quotas](#capacity-and-quotas)).
- TODO(P6): OOM in a runner.

## Cost

- TODO: expected cost per job and per month (design section 5); where to look
  in billing.
