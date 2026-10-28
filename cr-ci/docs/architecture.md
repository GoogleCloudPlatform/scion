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
| Runner job | Cloud Run Job `RUNNER_JOB_NAME`. Each execution is one ephemeral runner: `RUNNER_CPU` / `RUNNER_MEMORY`, with a `RUNNER_DISK_GI` ephemeral disk mounted at HOME (`/home/runner`) | `EXEC_SA_NAME` (no roles) |
| Reconciler | Cloud Scheduler job `RECONCILER_JOB_NAME` calling `POST /reconcile` | `SCHEDULER_SA_NAME` |
| Secrets | Secret Manager: `GITHUB_APP_PRIVATE_KEY_SECRET_NAME`, `GITHUB_WEBHOOK_SECRET_NAME` | Readable by `MEDIATOR_SA_NAME` only |
| Image | Artifact Registry `ARTIFACT_REPO`, image `RUNNER_IMAGE_NAME` | Pushed by `BUILD_SA_NAME` |

v1 uses **Cloud Run Jobs only** (GA in every region, no leak on exit). The
design's optional Cloud Run Instances backend (Preview, not in us-central1) is
deferred. The launcher stays behind an interface so it can be added later.

## Deploy preconditions

These are required, not recommended. Do not set the routing variable on a
repository until all of them hold:

1. **Fork PR approval: "Require approval for all external contributors"**
   (repo or org setting, [matrix G8](permissions-matrix.md#1-github-steps)),
   not only for first-time contributors. A fork job only exists after a
   maintainer approves it, which is what bounds the residual risk below.
2. The workflow `runs-on` expression carries the fork guard (see
   [Fallback toggle](#fallback-toggle)).
3. The mediator runs with `max-instances=1`.
4. `EXEC_SA_NAME` holds no IAM roles, and `roles/run.viewer` on the project is
   granted narrowly (it shows execution env overrides, including the claim
   token).

## Flow

The mediator (`mediator/`, package `internal/server`) serves four endpoints:
`POST /webhook`, `POST /internal/claim`, `POST /reconcile` and `GET /health`.
It holds its state in memory (pending claims, the runners it launched and
which job each serves, when it first saw a runner offline), so it **must
run with `max-instances=1`**. Dispatch, runner stops and the mutating part of
reconcile are serialised by one lock, so the capacity and duplicate checks
cannot race. Webhook work runs on a context detached from the delivery
(GitHub gives up after 10 s) with a 60 s budget, and compensating actions
(deleting a registration after a failed launch) run on a fresh context, so
they happen even if the step they undo timed out.

**Runners and jobs are not one to one.** GitHub gives a queued job to *any*
idle runner whose labels match, so the runner `cr-job-X` launched for job X
may run job Y. The mediator therefore identifies runners by name, learns
which job a runner actually runs from `workflow_job.runner_name`, and finds
a runner's execution by its `RUNNER_NAME` env, never by job ID.

1. **Webhook (`POST /webhook`).** The mediator checks `X-Hub-Signature-256`
   (HMAC-SHA256 over the raw body, `hmac.Equal`). Responses: 401 for a bad
   signature, 400 for a malformed payload or unreadable body, 413 for a body
   over 5 MiB, 503 when the secret or `MEDIATOR_URL` is unavailable or a
   GitHub or Cloud Run call fails. Everything else gets 200, including events
   it ignores: events other than `workflow_job`; jobs without `RUNNER_LABEL`
   (case-insensitive); jobs with labels a cr-ci runner cannot satisfy
   (`labels_unservable`, below); repositories outside `TARGET_REPOS`
   (`webhook_rejected`); and fork-guard rejections. On a signature mismatch
   the secret is re-read at most once a minute (every 10 s while none could
   be loaded yet), and a replaced secret stays valid for an hour, so rotation
   needs no restart and the order of its two steps does not matter.
2. **Queued.**
   - **Labels:** a cr-ci runner carries `RUNNER_LABEL` only, and GitHub
     requires every job label on the runner, so only jobs whose labels are
     all `RUNNER_LABEL` are dispatched. Others (for example
     `runs-on: [self-hosted, RUNNER_LABEL]`) are logged once as
     `labels_unservable` (WARNING) and never minted for.
   - **Fork guard:** the mediator reads the run with
     `GET /repos/{o}/{r}/actions/runs/{run_id}` (never trusting the payload)
     and refuses, logging `SECURITY_REJECT_FORK_PR` and answering 200, unless
     the run's event is on the allowlist, `push` or `pull_request` (the two the
     workflows' routing expression routes). Every other event, named or not
     (`pull_request_target`, `workflow_run`, `issue_comment`,
     `repository_dispatch`, `workflow_dispatch`, `schedule`, `merge_group`, ...),
     is refused fail-closed (`reason=event_not_allowed:<event>`). It also refuses
     a run whose `head_repository` differs from its `repository` (or is
     missing), or whose repository is not the webhook's repository. The same
     check guards reconcile backfill and the hijack guard (step 4).
   - **Duplicate check:** no action if a runner named `cr-job-<job_id>` is
     registered, or a running execution has that `RUNNER_NAME`, or serves the
     job (`GH_JOB_ID`) and has not been reassigned to another job. This rests
     on GitHub and Cloud Run state, so it survives restarts.
   - **Capacity check:** running executions, minus those this process just
     cancelled, plus launches from the last two minutes that
     `executions.list` does not show yet. At `MAX_CONCURRENT_RUNNERS` or
     more, the job is left queued (`capacity_full`, 200). **No JIT config is
     minted** before capacity is confirmed.
   - **Still queued:** `GET /repos/{o}/{r}/actions/jobs/{job_id}` must say
     `queued`, so a redelivered or replayed event for a finished job cannot
     mint an orphan runner.
   - **Launch:** `generate-jitconfig` with name `cr-job-<job_id>`,
     `runner_group_id` `GITHUB_RUNNER_GROUP_ID`, labels `[RUNNER_LABEL]` and
     `work_folder` `/home/runner/work`, the same path as on GitHub-hosted
     `ubuntu-latest`. `actions/cache` stores paths relative to
     `GITHUB_WORKSPACE`, so only an identical work folder (and `HOME`) lets
     cache entries move between the two runner types in both directions. On
     the cr-ci runner that folder is a real directory on the ephemeral disk,
     which the job mounts at HOME (cache parity contract), so
     `GITHUB_WORKSPACE` is `/home/runner/work/<repo>/<repo>`, physically too.
     The JIT config is stored under a fresh
     claim token, then `jobs.run` starts one execution with the env
     overrides below. If `jobs.run` fails, the mediator deletes the
     registration, drops the claim, logs `launch_failed` and answers 503;
     `/reconcile` retries the job later.
3. **Runner start.** The entrypoint checks that HOME is the ephemeral disk and
   creates the work folder and the Go and npm caches on it as real directories,
   so they do not consume RAM. Cloud Run's root filesystem is in memory. The entrypoint exchanges the claim token for the
   JIT config (contract below) and starts `run.sh --jitconfig` as its child
   (it forwards Cloud Run's SIGTERM and reports the runner's real exit code;
   [runner README](../runner/README.md)). GitHub assigns exactly one job; the
   process exits; the execution ends and billing stops. A runner that gets no
   job within `RUNNER_IDLE_TIMEOUT` stops itself (step 4, inherited time budget).
4. **Hijack guard (in progress or completed on a cr-ci runner).** For any
   `in_progress` or `completed` event whose `runner_name` is `cr-job-*`,
   whatever the job's labels, the mediator compares the job with the job the
   runner serves (from memory, or the ID in its name after a restart). If
   they differ, it runs the fork guard on the job's run; a repo outside
   `TARGET_REPOS` or a failed run lookup also fails (fail closed).
   - **Guard fails (hijack):** cancel that runner's execution, delete its
     registration (GitHub refuses while the job still runs; the reaper
     removes it later), drop its claim, and log
     `SECURITY_KILL_HIJACKED_RUNNER`. The displaced job, if still queued, is
     served at once by a new runner named after the hijacking job's ID (that
     job never gets a runner of its own, so the name is free and cannot
     collide with the stale registration or with the hijacking job's later
     events).
   - **Guard passes (swap):** harmless; log `runner_swap`, mark the runner as
     reassigned, and make sure the job that took it has a runner named for it
     (a no-op if it has one). That runner, idle, serves the displaced job.
     Runners and queued jobs stay one to one.

   **Inherited time budget.** The job that takes a runner gets only what is left
   of that execution's `TASK_TIMEOUT` (P6: a job taken by a 40-minute-old runner
   was killed 5 minutes later). Two things bound this:
   - **Idle timeout.** The entrypoint stops a runner that has not started a job
     within `RUNNER_IDLE_TIMEOUT` (default 10m, at least 1m, at most half of
     `TASK_TIMEOUT`) of the container start, gracefully (SIGTERM to `run.sh`), and
     exits 0 with an `idle_timeout:` log line. (The check and the SIGTERM are not
     atomic: a job assigned in between is cancelled, and the entrypoint then logs
     `ERROR idle_timeout_raced_job` and exits 4, so that failure is visible.) So a
     runner can only take a job
     while it is young, and **every job gets at least
     `TASK_TIMEOUT - RUNNER_IDLE_TIMEOUT`** (35 minutes with the defaults). The
     offline registration is reaped by the next `/reconcile` (it is aged from its
     launch, so with the default idle timeout it is already past the five-minute
     reap age), which also re-serves the job it was launched for if that is still
     queued.
   - **Logging.** `runner_swap` carries `execution_age` and `inherited_budget`
     (`TASK_TIMEOUT` minus the age, from the launch time in memory or, after a
     restart, from the execution's creation time). Below 20 minutes a WARNING
     `runner_swap_low_budget` follows. The taker is not moved: GitHub has already
     started it there.

   `/reconcile` repeats this check for every `in_progress` job on a `cr-job-*`
   runner, which covers a missed webhook.

   Fail closed has a cost: if the run lookup fails during a legitimate swap
   (a GitHub API error), the mediator cannot tell it from a hijack and kills
   the runner, so that legitimate job fails and needs a re-run. This needs a
   swap and an API failure at the same moment; refusing to vouch for an
   unknown job is the safer trade.

   When the taker already has a runner of its own (idle, since its job runs
   elsewhere), that runner's record is rebound to the displaced job, so
   cancelling the displaced job while queued stops it.
5. **Completed.**
   - **On a cr-ci runner** (`runner_name` set): after the hijack guard, the
     runner's record and claim are dropped. Nothing is stopped: GitHub ends a
     cancelled job itself and the ephemeral runner exits.
   - **Without a cr-ci runner** (cancelled, skipped or failed while queued):
     the runners launched for the job are stopped only if their registration
     is absent or idle. A busy registration, or a 422 from the delete (it
     just picked up a job), means the runner is running another job, and it
     is left alone (`cancel` with `action=none_busy`).
6. **Reconcile (`POST /reconcile`).** Cloud Scheduler calls it with a Google
   ID token. The mediator verifies the token itself (RS256 against Google's
   JWKS, fetched at most once a minute; issuer `accounts.google.com`;
   audience `MEDIATOR_URL`; not expired; `email` =
   `SCHEDULER_SA_NAME@GCP_PROJECT.iam.gserviceaccount.com`;
   `email_verified`), because the service is public. Then, with a 4 min
   budget:
   - discovery, without holding the lock: `queued` and `in_progress` runs of
     every repo in `TARGET_REPOS` and their jobs;
   - the hijack guard for jobs running on `cr-job-*` runners (step 4);
   - drops expired claims;
   - deletes offline, idle `cr-job-*` registrations of **this stack** older
     than five minutes (`runner_reaped`), aged from the launch when this
     process launched the runner and otherwise from the first reconcile that
     saw it offline. A runner whose claim is still pending (slow cold start)
     is kept. Every runner is registered with `RUNNER_LABEL` plus a stack
     label, `crci-stack-<REGION>-<RUNNER_JOB_NAME>` (unique per project); a
     registration with another stack's label is reaped only after
     `TASK_TIMEOUT` plus five minutes offline (it cannot be live then), one with
     no stack label (made before stack labels) like this stack's own. So two stacks serving the same repo, during a
     region move, do not reap each other's cold-starting runners
     ([operations](operations.md#two-stacks-region-move));
   - cancels executions older than `TASK_TIMEOUT` plus five minutes, a safety
     net behind Cloud Run's own task timeout (`execution_reaped`);
   - backfills queued, servable jobs of runs that pass the fork guard, oldest
     first, through the same dispatch path as a webhook, listing runners and
     executions once for the whole pass, and stopping when capacity is full.
     This is the only path that serves a job left queued for capacity, so
     with cr-ci as the primary runner `RECONCILE_SCHEDULE` should be every
     minute. (No in-process timer: request-based CPU throttles work done after
     a response.)

   It answers 200 with a JSON summary (500 if a step failed; a GitHub 5xx for
   one repo, run or job only skips that item, `reconcile_skip`, unless every
   repo was skipped, `reconcile_all_skipped`, which is a 500) and logs
   `reconcile`.

### Mediator-to-runner contract (claim contract v2)

- One execution per runner, with exactly these env overrides:
  `RUNNER_CLAIM_TOKEN` (32 lowercase hex characters from `crypto/rand`, single
  use), `MEDIATOR_URL` (https origin, no trailing slash), `GH_JOB_ID` (the job
  the runner is launched to serve), `RUNNER_NAME` (normally
  `cr-job-<GH_JOB_ID>`; after a swap or a hijack it carries another job's ID,
  and the runner only logs the mismatch) and `GH_REPOS` (comma-separated
  `owner/name` list of the repositories whose jobs the runner may be handed:
  the one repository in repo scope, every `TARGET_REPOS` entry of the owner in
  org scope, since org-level runners take any of those jobs; the runner's job
  guard refuses a job of any other repository). The JIT config is never in env,
  args or labels: execution env overrides are visible to anyone with
  `roles/run.viewer`.
- The runner fetches a Google ID token for itself from the metadata server
  (`.../service-accounts/default/identity?audience=<MEDIATOR_URL>&format=full`,
  no IAM role needed) and calls `POST ${MEDIATOR_URL}/internal/claim` with
  `Authorization: Bearer <ID token>`, `X-Runner-Claim: <RUNNER_CLAIM_TOKEN>`
  and an empty body.
- The mediator verifies the ID token **before** looking at the claim:
  audience `MEDIATOR_URL`, issuer `accounts.google.com`, not expired,
  `email` = `EXEC_SA_NAME@GCP_PROJECT.iam.gserviceaccount.com`,
  `email_verified`. A missing or invalid identity gets 401 and does **not**
  consume the claim, so someone who read the claim token from the execution
  env can neither use nor burn it. Then: a missing, malformed, unknown,
  expired or already used claim token gets 410; success is 200,
  `Content-Type: application/json`, `Cache-Control: no-store`, body
  `{"jit_config":"<encoded_jit_config>"}`. Internal errors are 5xx.
- The token is consumed on the first successful read and expires after
  `CLAIM_TTL` (default 300 s, to cover the cold start of a large image). The
  mediator stores only the token's SHA-256.
- The runner retries 5xx, connection errors and metadata-server failures
  within one budget of 5 attempts with backoff; on 401, 410 or after the last
  failure it exits 3 without starting `run.sh`.
- A mediator restart loses pending claims: the runner exits 3, and
  `/reconcile` deletes the orphaned `cr-job-*` registration and relaunches
  the still-queued job.

### Residual risk: label-matched job assignment

A fork-PR job that targets `RUNNER_LABEL` is refused a runner of its own, but
it stays queued (up to 24 h), and GitHub may hand it to the next idle cr-ci
runner launched for another job.

**The window is closed on the runner side by the job-started hook.** The
entrypoint sets `ACTIONS_RUNNER_HOOK_JOB_STARTED` to `cr-ci-job-guard.sh`.
Runner.Worker reads it from its own environment and runs the hook after the job
is assigned and before any of its steps, checkout included. The hook allows only
`push`, or `pull_request` whose head repository is the repository (the same
allowlist as the routing expression and the fork guard), and only jobs of the
repositories the runner serves (`GH_REPOS`: its registration scope). Anything else is a
**hard stop**: the hook kills the worker and the runner's process group, so no
step of the job runs. A failed hook alone would not do: the runner would still
run steps marked `if: always()` or `failure()`. For the same reason the
entrypoint does not start the runner at all (exit 2) unless the hook is the
root-owned, read-only file whose sha256 was pinned at image build; its path is
fixed in production. The entrypoint exits 5 on a refusal
([runner README](../runner/README.md), "Job guard", which cites the actions/runner
2.338.0 source for each of these points).

The hijack guard (step 4) stays as the next layer: it kills a runner whose job does
not match as soon as GitHub reports the job, with no extra App permissions, and
deletes the registration. Before the hook, that kill landed within seconds but
**after** the job may have started, so a few steps of untrusted code could run.
That case and any gap in the hook are also bounded by the other layers:

- the job only exists if a maintainer approved the fork PR's workflows
  (deploy precondition 1), and the `runs-on` expression already routes
  cross-repo PRs to `ubuntu-latest`, so the PR must also have edited the
  workflow;
- the runner's identity `EXEC_SA_NAME` holds no IAM roles, so its access
  token grants nothing in GCP. Its ID token is one of the two factors of a
  claim (claim contract v2), but useless alone: the other factor, a pending
  claim token, sits only in another execution's env (readable with
  `roles/run.viewer`, which hijacking code does not have) and is consumed by
  that runner within seconds of its start;
- GitHub gives fork-PR workflows a read-only `GITHUB_TOKEN` and no secrets;
- the execution is ephemeral: nothing it writes survives the kill.

Unverified: cr-ci registers runners with `RUNNER_LABEL` only, on the
assumption that `generate-jitconfig` adds no default labels. GitHub's own
example response shows `self-hosted` and `X64` on a JIT runner, so a job with
`runs-on: self-hosted` might also match a cr-ci runner
([matrix U7](permissions-matrix.md#5-design-corrections-and-open-items); to be
checked in the PoC). The hijack guard keys on `runner_name`, not on labels,
so it covers that case too; the label choice is not relied on as a defence.

Alternatives considered (with cr-runners-arch): giving the App
*Actions: write* to cancel rejected fork runs widens the token for every
repository and still races; org runner groups restricted to selected
workflows do not exist at repo scope and block `pull_request` runs when
pinned to a ref.

**Further hardening (optional): per-run composite label.** Workflows would target
`<RUNNER_LABEL>-run-<run_id>` (for example
`format('{0}-run-{1}', vars.SCION_LARGE_RUNNER, github.run_id)` in the
`runs-on` expression), and the mediator would require that label on the job
and mint each runner with only that label. A job of another run, including a
fork run, could then never match a cr-ci runner at all (the job-started hook
already refuses it before any step). It costs a dynamic label in workflows, logs
and reconcile matching, so it is deferred.

### Residual risk: job code can read the runner's credentials

The entrypoint, `run.sh`, the runner listener and worker, and the job's steps all run as
one uid (`runner`, 1001) in the execution, as on any non-containerized self-hosted
runner. Job code can therefore read:
- the JIT config on `run.sh`'s argv: this ephemeral runner's registration, usable to act
  as that runner until GitHub removes the registration after the job;
- the claim token in `/proc/1/environ`: already spent (single use).

sec-review-3 rates it LOW (record `crci-img-651578be-v3.md`, re-rated for the job
guard by its fork-guard record), on these conditions:
- the window in which untrusted code could run on a cr-ci runner is closed by the
  runner-side job-started hook (residual risk above), on top of the routing
  expression's guard and the mediator's fork guard (step 2). **Residual:** a
  compromised trusted job can still read its own runner's registration
  (ptone/scion#5978);
- one job runs per runner;
- the hijack guard (step 4) and the reaper bound what a leaked registration could do.

If untrusted code were ever routed to cr-ci, the risk would be Medium/High. The fork
guards are therefore load-bearing for this, not only defense in depth. The structural
fix is tracked in ptone/scion#5978: a runner-only uid with job steps dropped to another
uid, or passing the JIT config by file and deleting it once the listener has started.

## Security model

- **Secrets:** only the App private key and the webhook secret are long-lived.
  Both live in Secret Manager, readable only by the mediator SA. The
  installation token (1 h) and the JIT config stay in mediator memory and are
  never logged.
- **Zero-privilege runner identity:** workflow code can read the runner's
  metadata-server token, so `EXEC_SA_NAME` holds no roles. Its ID token is
  used only to prove, on `/internal/claim`, that the caller runs as the
  runner identity.
- **Claim tokens:** the claim token sits in the execution env, readable with
  `roles/run.viewer`, for up to `CLAIM_TTL`. On its own it is useless: the
  claim also needs the runner identity's ID token (claim contract v2). Keep
  `roles/run.viewer` narrow anyway.
- **One job per runner,** in three layers: GitHub JIT runners take at most one
  job; the entrypoint runs `run.sh` once and exits when it does; the job uses
  `--max-retries=0` and a hard `TASK_TIMEOUT`.
- **Fork PRs on public repositories,** in three layers:
  1. The repo or org setting requires approval for **all** external
     contributors (a deploy precondition, not a suggestion).
  2. The `runs-on` expression routes cross-repo PRs to `ubuntu-latest`.
  3. The mediator independently refuses every run that is not a same-repo `push`
     or `pull_request` (an allowlist), even if a PR edits the workflow to target
     the label, and kills any cr-ci runner such a job manages to start on (the
     hijack guard).
  4. On the runner, the job-started hook refuses such a job before any of its
     steps, with a hard stop (see the
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
- **Runner layout (cr-ci FIXUP-5):** the ephemeral disk is mounted at HOME
  (`/home/runner`), the runner is installed at `/opt/actions-runner` (its `_diag`
  stays on the root filesystem), and the entrypoint creates `~/work`, `~/.cache`,
  `~/go` and `~/.npm` as real directories on the disk, with no symlinks: physical
  paths equal logical ones (actions/setup-node's lock-file hashing compares
  `process.cwd()` with `GITHUB_WORKSPACE`), and whole-directory cache archives
  restore in place. `TMPDIR` is unset, as on ubuntu-latest (`/tmp`); `GOTMPDIR` is
  on the disk. Residuals:
  - **HOME is the volume root, created world-writable** ("so that any user can read
    or write to it", per Cloud Run). The entrypoint logs its owner and mode at every
    start and sets it to 0755 only if the runner's uid owns it. Otherwise HOME stays
    777. Only the runner's uid runs in the container, and the tools in use check
    file modes (ssh) or the repository's owner (git), not HOME's; starting as root
    to chmod it was rejected (a trust-model change).
  - **Temp files are in RAM.** `/tmp` is on the in-memory root filesystem, so Go
    tests' `t.TempDir()` and other temp files count against `RUNNER_MEMORY`. Go's
    build and test temp work is on the disk (`GOTMPDIR`). Measure peak memory
    before routing temp-heavy jobs such as ci.yml's `build-and-test` (not routed).
    extras-ci.yml's `build-and-test` (a per-module `go test -count=1 ./...` matrix)
    **is** already routed, so its `t.TempDir()` files are now in RAM too: its fixtures
    are small, but check its peak memory in the live verification job after the
    layout switch ([operations.md, "Runner layout switch"](operations.md#runner-layout-switch)).
- **Shape limits:** at most 8 vCPU / 32 GiB, and memory must fit the CPU count
  (4 vCPU up to 16 GiB, 6 up to 24 GiB, 8 up to 32 GiB). The loader enforces
  this. The design's "> 16 GiB needs 4 vCPU" statement is wrong (see the
  [matrix, X6](permissions-matrix.md#5-design-corrections-and-open-items)).
- **Ephemeral-disk size and quotas:** two quotas apply. Per instance, Cloud Run
  allows 10 GiB of ephemeral disk by default
  (`run.googleapis.com/max_per_instance_ephemeral_disk`, raisable on request;
  `RUNNER_DISK_GI` accepts 1-100). Per region, the allocation is 100 GiB by
  default (`run.googleapis.com/ephemeral_disk_allocation`), consumed as
  `RUNNER_DISK_GI` x running executions. The default 10 GiB is **not** enough
  for a warm golangci-lint: the P6 PoC measured about 15-20 GiB on disk
  (restored Go cache 4.8 GB growing to ~8-9.5 GB, workspace 1.1 GB, modules
  ~1.3 GB, Go temp dirs up to ~2 GB), and runs with 10 GiB ran out of space. An
  on-disk build cache needs at least 20 GiB: P6b peaked at 16.5 GB on a 23 GiB
  disk. Raise the per-instance quota first; the limit the server then enforces
  depends on the region and the grant (us-west2 runs with `RUNNER_DISK_GI=23`, its
  limit after the grant). Size the regional quota for
  `MAX_CONCURRENT_RUNNERS` x `RUNNER_DISK_GI`: at 23-25 GiB the default 100 GiB
  allows `MAX_CONCURRENT_RUNNERS=4`, and a 5th runner would fail as
  `launch_failed` (`RESOURCE_EXHAUSTED`)
  ([gcp-setup.md, "Quotas"](gcp-setup.md#9-quotas)).
- **RAM build cache (`RUNNER_BUILD_CACHE_IN_RAM=true`):** the alternative for a
  small disk. The entrypoint makes `~/.cache` a link to a directory on the
  in-memory root filesystem, so the Go build cache and golangci-lint's analysis
  cache live in RAM (archive entries under `~/.cache/...` still restore there: tar
  follows a link in a leading component; a whole-`~/.cache` archive would replace
  the link).
  It counts against `RUNNER_MEMORY`: in P6 PoC run 4 the lint passed at 27.9 GB of
  32 GiB (a ~21 GB compile plus the cache grown to ~9.5 GB), leaving about 4 GB.
  It requires `RUNNER_MEMORY=32Gi` (the config loaders reject less), and the
  disk quota is preferable when available: the margin
  shrinks as the cache or pkg/hub grows. A Cloud Run in-memory volume
  (`--add-volume type=in-memory,size-limit=…`) would be an alternative with a hard
  size cap, untested.

## Failure modes

| Failure | What you see | Recovery |
| :--- | :--- | :--- |
| Webhook missed or mediator 5xx | Job waits for a runner | `/reconcile` backfills it |
| Capacity or quota exhausted | Excess jobs stay queued | Dispatched first in, first out as executions finish; no orphaned registrations |
| Job cancelled (superseded run) | `cancelled` | On a runner: GitHub stops it and the runner exits. While queued: its runner is stopped if idle, left alone if busy with another job |
| Job labels a cr-ci runner cannot satisfy | `labels_unservable` warning; job stays queued | Fix the workflow's `runs-on` to `RUNNER_LABEL` alone (or the routing expression) |
| Untrusted job lands on a cr-ci runner | `SECURITY_KILL_HIJACKED_RUNNER` | Runner killed within seconds; the displaced job is re-served. Review the run |
| Runner hangs | Job runs long | Workflow `timeout-minutes`, then `TASK_TIMEOUT` hard-kills the execution |
| Runner OOM | "Runner lost communication" | `--max-retries=0`; investigate memory and raise the shape or split the job |
| Container crash before connecting | Offline `cr-job-*` registration | `/reconcile` deletes it |
| Mediator restart (deploy, crash) | Pending claims lost; runner exits 3 | `/reconcile` deletes the orphaned registration and relaunches the queued job |
| `jobs.run` fails after minting | `launch_failed` log, webhook answered 503 | Registration deleted at once; `/reconcile` retries the job |
| Regional outage or bad image | No labelled job starts | **Fallback toggle** (below) |

## Fallback toggle

Workflows choose the runner with:

```yaml
runs-on: ${{ (github.event_name == 'push' || (github.event_name == 'pull_request' && github.event.pull_request.head.repo.full_name == github.repository)) && vars.SCION_LARGE_RUNNER || 'ubuntu-latest' }}
```

- **Unset variable** (any unconfigured fork, or before rollout): `ubuntu-latest`.
- **Set to `RUNNER_LABEL`:** same-repo PRs and pushes go to Cloud Run.
- **Fork PRs and every other event** (`pull_request_target`, `workflow_run`,
  ...): `ubuntu-latest`. This allow-list replaces the design's
  `github.event_name != 'pull_request' || ...`, which would also route
  `pull_request_target` and `workflow_run` jobs to the label, where the
  mediator refuses them and they would wait up to 24 h
  ([workflows/README.md](../workflows/README.md#the-routing-expression)).
- **Fall back:** delete the variable, or set it to `ubuntu-latest` (or to a
  larger GitHub-hosted runner label). This takes effect for newly queued jobs
  with no commit. Already-queued jobs need a re-run.

Changing the variable needs **write** access to the repo, not admin
([matrix G10, G11](permissions-matrix.md#1-github-steps)). Procedures are in
[operations.md](operations.md).
