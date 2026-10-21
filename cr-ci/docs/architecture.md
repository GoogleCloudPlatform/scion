# cr-ci architecture

A condensed view of the
[cr-runners design](https://storage.cloud.google.com/scion-xproject-exchange/cr-runners/design.md)
(`gs://scion-xproject-exchange/cr-runners/design.md`), as adopted for cr-ci.
The design has the measurements, cost model and alternatives. This page is the
working summary, and it records where cr-ci departs from the design. Variable
names refer to [`config.example.env`](../config.example.env).

## Components

| Component | What it is | Identity |
| :--- | :--- | :--- |
| GitHub App | Sends `workflow_job` webhooks and lets the mediator mint just-in-time (JIT) runner registrations | Installed on `TARGET_REPOS` |
| Mediator | Go Cloud Run service `MEDIATOR_SERVICE_NAME` (scale to zero, small, `max-instances=1`: claims live in memory) | `MEDIATOR_SA_NAME` |
| Runner job | Cloud Run Job `RUNNER_JOB_NAME`. Each execution is one ephemeral runner: `RUNNER_CPU` / `RUNNER_MEMORY`, with a `RUNNER_DISK_GI` ephemeral disk at `/scratch` | `EXEC_SA_NAME` (no roles) |
| Reconciler | Cloud Scheduler job `RECONCILER_JOB_NAME` calling `POST /reconcile` | `SCHEDULER_SA_NAME` |
| Secrets | Secret Manager: `GITHUB_APP_PRIVATE_KEY_SECRET_NAME`, `GITHUB_WEBHOOK_SECRET_NAME` | Readable by `MEDIATOR_SA_NAME` only |
| Image | Artifact Registry `ARTIFACT_REPO`, image `RUNNER_IMAGE_NAME` | Pushed by `BUILD_SA_NAME` |

v1 uses **Cloud Run Jobs only** (GA in every region, no leak on exit). The
design's optional Cloud Run Instances backend (Preview, not in us-central1) is
deferred. The launcher stays behind an interface so it can be added later.

## Flow

The mediator (`mediator/`, package `internal/server`) serves four endpoints:
`POST /webhook`, `POST /internal/claim`, `POST /reconcile` and `GET /health`.
It holds its state in memory (pending claims, jobs it launched, when it first
saw a runner offline), so it **must run with `max-instances=1`**. Dispatch,
cancellation and reconcile are serialised by one lock, so the capacity and
idempotency checks cannot race.

1. **Webhook (`POST /webhook`).** The mediator checks `X-Hub-Signature-256`
   (HMAC-SHA256 over the raw body, `hmac.Equal`) and answers 401 on a
   mismatch. On a mismatch it re-reads the secret from Secret Manager at most
   once a minute, so a rotated webhook secret takes effect without a restart.
   Events other than `workflow_job` are acknowledged and ignored. Jobs without
   `RUNNER_LABEL` (case-insensitive) are ignored. Repositories outside
   `TARGET_REPOS` are logged (`webhook_rejected`) and ignored. Ignored and
   rejected events get 200, so GitHub does not flag the delivery.
2. **Queued.**
   - **Fork guard:** the mediator reads the run with
     `GET /repos/{o}/{r}/actions/runs/{run_id}` (never trusting the payload)
     and refuses, logging `SECURITY_REJECT_FORK_PR` and answering 200, when
     the run's event is `pull_request_target`, its `head_repository` differs
     from its `repository` (or is missing), or its repository is not the
     webhook's repository.
   - **Idempotency:** no action (200) if a runner named `cr-job-<job_id>` is
     registered or an active execution carries `GH_JOB_ID=<job_id>`. This
     rests on GitHub and Cloud Run state, not on mediator memory, so it
     survives restarts and runner swaps (below).
   - **Capacity check:** active executions are the job's executions without a
     completion time. At `MAX_CONCURRENT_RUNNERS` or more, the job is left
     queued (`capacity_full`, 200). **No JIT config is minted** before
     capacity is confirmed.
   - **Launch:** `generate-jitconfig` with name `cr-job-<job_id>`,
     `runner_group_id` `GITHUB_RUNNER_GROUP_ID`, labels `[RUNNER_LABEL]` only
     and `work_folder` `/scratch/_work`. The labels deliberately omit
     `self-hosted`, `Linux` and `X64`, so a job that targets generic
     self-hosted labels can never be assigned to a cr-ci runner. The JIT
     config is stored under a fresh claim token, then `jobs.run` starts one
     execution with the env overrides below. If `jobs.run` fails, the
     mediator deletes the registration, drops the claim, logs
     `launch_failed` and answers 503; `/reconcile` retries the job later.
3. **Runner start.** The entrypoint points the workspace, temp dirs and Go
   caches at `/scratch`, so they do not consume RAM. Cloud Run's root
   filesystem is in memory. The entrypoint exchanges the claim token for the
   JIT config (contract below) and `exec`s `run.sh --jitconfig` as PID 1.
   GitHub assigns exactly one job; the process exits; the execution ends and
   billing stops.
4. **In progress (hijack guard).** GitHub assigns a queued job to *any* idle
   runner whose labels match, so the runner `cr-job-<X>` minted for job X may
   start job Y. The `in_progress` event names the runner (`runner_name`).
   When Y differs from X, the mediator runs the fork guard on Y's run (and
   treats a repo outside `TARGET_REPOS`, or a failed run lookup, as a
   failure: fail closed):
   - **Guard fails (hijack):** cancel runner X's execution, delete its
     registration, forget its claim and launch record, and log
     `SECURITY_KILL_HIJACKED_RUNNER`. Job X, still queued, is re-dispatched at
     once with a fresh runner (or by `/reconcile` if its run is unknown after
     a restart).
   - **Guard passes (swap):** harmless; log `runner_swap`, and dispatch for Y
     so that Y has a runner named for it (a no-op if it already has one).
     That runner, idle, serves job X. Runners and queued jobs stay one to one.
5. **Completed.** Any `completed` event drops the job's claim. With
   `conclusion: cancelled`, the mediator stops the runner named in
   `runner_name` (the one that actually ran the job, which after a swap was
   minted for another job), or the runner minted for the job if it never
   started: it cancels the execution (found by `GH_JOB_ID` if this process
   did not launch it) and deletes the registration (`cancel`). Other
   conclusions need no action: Jobs executions stop when the runner exits.
6. **Reconcile (`POST /reconcile`).** Cloud Scheduler calls it with a Google
   ID token. The mediator verifies the token itself (RS256 against Google's
   JWKS, issuer `accounts.google.com`, audience `MEDIATOR_URL`, not expired,
   `email` = `SCHEDULER_SA_NAME@GCP_PROJECT.iam.gserviceaccount.com`,
   `email_verified`), because the service is public. Then it:
   - drops expired claims;
   - deletes offline, idle `cr-job-*` registrations older than five minutes
     (`runner_reaped`), aged from the launch when this process launched the
     runner and otherwise from the first reconcile that saw it offline. A
     runner whose claim is still pending (slow cold start) is kept;
   - cancels executions older than `TASK_TIMEOUT` plus five minutes, a safety
     net behind Cloud Run's own task timeout (`execution_reaped`);
   - backfills: lists `queued` and `in_progress` runs of every repo in
     `TARGET_REPOS`, applies the fork guard to each run, and dispatches their
     queued `RUNNER_LABEL` jobs oldest first through the same path as a
     webhook, stopping when capacity is full.

   It answers 200 with a JSON summary (500 if a step failed) and logs
   `reconcile`.

### Mediator-to-runner contract

- One execution per GitHub job, with exactly these env overrides:
  `RUNNER_CLAIM_TOKEN` (32 lowercase hex characters from `crypto/rand`, single
  use), `MEDIATOR_URL` (https origin, no trailing slash), `GH_JOB_ID` and
  `RUNNER_NAME` (`cr-job-<job_id>`). The JIT config is never in env, args or
  labels: execution env overrides are visible to anyone with
  `roles/run.viewer`.
- The runner calls `POST ${MEDIATOR_URL}/internal/claim` with
  `Authorization: Bearer <RUNNER_CLAIM_TOKEN>` and an empty body. Success is
  200, `Content-Type: application/json`, `Cache-Control: no-store`, body
  `{"jit_config":"<encoded_jit_config>"}`. An unknown, expired, already used
  or malformed token gets 410. Internal errors are 5xx.
- The token is consumed on the first successful read and expires after
  `CLAIM_TTL` (default 300 s, to cover the cold start of a large image). The
  mediator stores only the token's SHA-256.
- The runner retries 5xx and connection errors up to 5 times with backoff; on
  410 or after the last failure it exits 3 without starting `run.sh`.
- A mediator restart loses pending claims: the runner exits 3, and
  `/reconcile` deletes the orphaned `cr-job-*` registration and relaunches
  the still-queued job.

### Residual risk: label-matched job assignment

A fork-PR job that targets `RUNNER_LABEL` is refused a runner of its own, but
it stays queued, and GitHub may hand it to an idle cr-ci runner minted for
another job. The hijack guard (step 4) kills that runner as soon as GitHub
reports the job `in_progress`, with no extra App permissions. The kill lands
within seconds, but **after** the job may have started, so a few steps of
untrusted code can run. That window is bounded by the other layers:

- the job only exists if a maintainer approved the fork PR's workflows
  (layer 1 below), and the `runs-on` expression already routes cross-repo PRs
  to `ubuntu-latest` (layer 2), so the PR must also have edited the workflow;
- the runner's identity `EXEC_SA_NAME` holds no IAM roles, so its
  metadata-server token is worthless;
- GitHub gives fork-PR workflows a read-only `GITHUB_TOKEN` and no secrets;
- the execution is ephemeral: nothing it writes survives the kill.

Alternatives considered (with cr-runners-arch): giving the App
*Actions: write* to cancel rejected fork runs widens the token for every
repository and still races; org runner groups restricted to selected
workflows do not exist at repo scope and block `pull_request` runs when
pinned to a ref.

**Future hardening: per-run composite label.** Workflows would target
`<RUNNER_LABEL>-run-<run_id>` (for example
`format('{0}-run-{1}', vars.SCION_LARGE_RUNNER, github.run_id)` in the
`runs-on` expression), and the mediator would require that label on the job
and mint each runner with only that label. A job of another run, including a
fork run, could then never match a cr-ci runner, closing the window
entirely. It costs a dynamic label in workflows, logs and reconcile matching,
so it is deferred.

## Security model

- **Secrets:** only the App private key and the webhook secret are long-lived.
  Both live in Secret Manager, readable only by the mediator SA. The
  installation token (1 h) and the JIT config stay in mediator memory and are
  never logged.
- **Zero-privilege runner identity:** workflow code can read the runner's
  metadata-server token, so `EXEC_SA_NAME` holds no roles.
- **One job per runner,** in three layers: GitHub JIT runners take at most one
  job; the entrypoint `exec`s `run.sh` as PID 1; the job uses
  `--max-retries=0` and a hard `TASK_TIMEOUT`.
- **Fork PRs on public repositories,** in three layers:
  1. The repo or org setting requires approval for all external contributors.
  2. The `runs-on` expression routes cross-repo PRs to `ubuntu-latest`.
  3. The mediator independently refuses fork-PR and `pull_request_target` runs,
     even if a PR edits the workflow to target the label, and kills any cr-ci
     runner such a job manages to start on (the hijack guard; see the
     [residual risk](#residual-risk-label-matched-job-assignment)).

  GitHub warns that self-hosted runners on public repos are risky. The layered
  guard plus ephemeral, unprivileged runners is how cr-ci answers that warning.
- **Least privilege** for each service account: see
  [permissions-matrix.md](permissions-matrix.md#22-runtime-identities-what-the-deployed-system-holds).

## Constraints

- **No Docker:** Cloud Run runs in gVisor, so Docker-in-Docker and workflow
  `services:` containers do not work. Only host-process jobs may be routed:
  `lint`, `build-and-test`, `sqlite-nonhub-tests`, the full test suite and race
  detection. Jobs that use `postgres` services stay on GitHub-hosted runners.
- **Shape limits:** at most 8 vCPU / 32 GiB, and memory must fit the CPU count
  (4 vCPU up to 16 GiB, 6 up to 24 GiB, 8 up to 32 GiB). The loader enforces
  this. The design's "> 16 GiB needs 4 vCPU" statement is wrong (see the
  [matrix, X6](permissions-matrix.md#5-design-corrections-and-open-items)).
- **Ephemeral-disk quota:** the regional default is 100 GiB, which is why
  `MAX_CONCURRENT_RUNNERS` defaults to 5 with 20 GiB disks. Raise both
  together after a quota increase.

## Failure modes

| Failure | What you see | Recovery |
| :--- | :--- | :--- |
| Webhook missed or mediator 5xx | Job waits for a runner | `/reconcile` backfills it |
| Capacity or quota exhausted | Excess jobs stay queued | Dispatched first in, first out as executions finish; no orphaned registrations |
| Job cancelled (superseded run) | `cancelled` | Mediator cancels the execution within seconds |
| Runner hangs | Job runs long | Workflow `timeout-minutes`, then `TASK_TIMEOUT` hard-kills the execution |
| Runner OOM | "Runner lost communication" | `--max-retries=0`; investigate memory and raise the shape or split the job |
| Container crash before connecting | Offline `cr-job-*` registration | `/reconcile` deletes it |
| Mediator restart (deploy, crash) | Pending claims lost; runner exits 3 | `/reconcile` deletes the orphaned registration and relaunches the queued job |
| `jobs.run` fails after minting | `launch_failed` log, webhook answered 503 | Registration deleted at once; `/reconcile` retries the job |
| Regional outage or bad image | No labelled job starts | **Fallback toggle** (below) |

## Fallback toggle

Workflows choose the runner with:

```yaml
runs-on: ${{ (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository) && vars.SCION_LARGE_RUNNER || 'ubuntu-latest' }}
```

- **Unset variable** (any unconfigured fork, or before rollout): `ubuntu-latest`.
- **Set to `RUNNER_LABEL`:** same-repo PRs and pushes go to Cloud Run.
- **Fall back:** delete the variable, or set it to `ubuntu-latest` (or to a
  larger GitHub-hosted runner label). This takes effect for newly queued jobs
  with no commit. Already-queued jobs need a re-run.

Changing the variable needs **write** access to the repo, not admin
([matrix G10, G11](permissions-matrix.md#1-github-steps)). Procedures are in
[operations.md](operations.md).
