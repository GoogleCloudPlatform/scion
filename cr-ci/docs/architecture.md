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
| Mediator | Stateless Go Cloud Run service `MEDIATOR_SERVICE_NAME` (scale to zero, small) | `MEDIATOR_SA_NAME` |
| Runner job | Cloud Run Job `RUNNER_JOB_NAME`. Each execution is one ephemeral runner: `RUNNER_CPU` / `RUNNER_MEMORY`, with a `RUNNER_DISK_GI` ephemeral disk at `/scratch` | `EXEC_SA_NAME` (no roles) |
| Reconciler | Cloud Scheduler job `RECONCILER_JOB_NAME` calling `POST /reconcile` | `SCHEDULER_SA_NAME` |
| Secrets | Secret Manager: `GITHUB_APP_PRIVATE_KEY_SECRET_NAME`, `GITHUB_WEBHOOK_SECRET_NAME` | Readable by `MEDIATOR_SA_NAME` only |
| Image | Artifact Registry `ARTIFACT_REPO`, image `RUNNER_IMAGE_NAME` | Pushed by `BUILD_SA_NAME` |

v1 uses **Cloud Run Jobs only** (GA in every region, no leak on exit). The
design's optional Cloud Run Instances backend (Preview, not in us-central1) is
deferred. The launcher stays behind an interface so it can be added later.

## Flow

1. **Webhook.** GitHub sends `workflow_job` to `POST /webhook`. The mediator
   checks `X-Hub-Signature-256` (HMAC-SHA256, constant-time compare) and drops
   events for repositories outside `TARGET_REPOS` or jobs without
   `RUNNER_LABEL`.
2. **Queued.**
   - **Fork guard:** reject if the run's head repository differs from the base
     repository, or if the event is `pull_request_target`.
   - **Idempotency:** skip if a runner for this job ID already exists.
   - **Capacity check:** if running executions are at `MAX_CONCURRENT_RUNNERS`,
     leave the job queued in GitHub. **No JIT config is minted** before
     capacity is confirmed.
   - **Launch:** mint the JIT config (`generate-jitconfig`, runner name derived
     from the job ID, work folder under `/scratch`). Then `RunJob` with a
     **one-time claim nonce**, not the JIT config, in the env override. If
     `RunJob` fails, delete the registration.
3. **Runner start.** The entrypoint points the workspace, temp dirs and Go
   caches at `/scratch`, so they do not consume RAM. Cloud Run's root
   filesystem is in memory. The entrypoint exchanges the nonce with the
   mediator for the JIT config (single use, 60 s TTL) and `exec`s
   `run.sh --jitconfig` as PID 1. GitHub assigns exactly one job; the process
   exits; the execution ends and billing stops.
4. **Completed or cancelled.** On `completed` with `conclusion: cancelled`, the
   mediator cancels the execution and deletes any leftover registration.
5. **Reconcile.** Every few minutes it backfills queued labelled jobs that have
   no runner (missed webhooks, backpressure), and deletes offline `cr-job-*`
   registrations older than a few minutes.

cr-ci adopts the design's claim-nonce exchange (design 3.2) from the start. The
reason: execution env overrides are visible to anyone with `roles/run.viewer`.

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
     even if a PR edits the workflow to target the label.

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
