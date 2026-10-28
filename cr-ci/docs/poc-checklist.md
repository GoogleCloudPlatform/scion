# P6 PoC checklist

This is the single list of everything cr-ci assumes but has not yet seen
work against real GitHub and GCP. It collects the open items from the
[permissions matrix](permissions-matrix.md#5-design-corrections-and-open-items)
(U2-U10), the [runner README](../runner/README.md#runtime-contract) checkpoints,
the [cache-sharing](../workflows/README.md#cache-sharing) assumptions, and the
design's PoC success criteria
([workflows/README.md](../workflows/README.md#poc-success-criteria)).

P6 needs ptone's explicit go-ahead. Work through the list in order; it
follows the [quick start](../README.md#quick-start). Record each result (pass,
fail or value) next to its ID. After the PoC, fold every result back into the
source doc named in the item, and close or rewrite its U-item.

Placeholders: `P` = `GCP_PROJECT`, `R` = `REGION`, `REPO` = the target
(`ptone/scion` for the fork PoC), `JOB` = `RUNNER_JOB_NAME`
(`cr-gh-runner-8c32g`), `SVC` = `MEDIATOR_SERVICE_NAME` (`cr-runner-mediator`),
`EXEC` = `EXEC_SA_NAME@P.iam.gserviceaccount.com`, `MED` =
`MEDIATOR_SA_NAME@P.iam.gserviceaccount.com`.

Two log queries are used throughout:

```bash
# Mediator (structured JSON; jsonPayload.event is the event name)
medlog() { gcloud logging read --project P --freshness=2h --limit=100 \
  "resource.type=\"cloud_run_revision\" AND resource.labels.service_name=\"SVC\" AND $1" \
  --format='table(timestamp, jsonPayload.event, jsonPayload.job_id, jsonPayload.runner, jsonPayload.reason, jsonPayload.message)'; }
# Runner executions (entrypoint and runner output)
runlog() { gcloud logging read --project P --freshness=2h --limit=200 \
  "resource.type=\"cloud_run_job\" AND resource.labels.job_name=\"JOB\" ${1:+AND $1}" \
  --format='value(timestamp, labels."run.googleapis.com/execution_name", textPayload)'; }
```

## A. Before deploying

| ID | What | How | Expected | Source |
| :--- | :--- | :--- | :--- | :--- |
| A1 | Deploy order | Follow the [quick start](../README.md#quick-start): config with `GITHUB_APP_ID=1`, bootstrap, image, job, `MEDIATOR_URL`, App, secrets, mediator, scheduler, install, preconditions, PoC, routing variable | `deploy-mediator.sh --apply` refuses `GITHUB_APP_ID=1`; `deploy-scheduler.sh` refuses an empty `MEDIATOR_URL`; `deploy-mediator.sh` prints no URL warning | README quick start; [github-setup.md 1-2](github-setup.md#1-fix-the-mediator-url) |
| A2 | gcloud version floor (U8) | `gcloud --version`; then every script once without `--apply` | Version >= `CRCI_MIN_GCLOUD` (530.0.0); no "unrecognized arguments" in any `--apply` run below. Lower or raise the floor in `scripts/lib.sh` from what you saw | [matrix U8](permissions-matrix.md#5-design-corrections-and-open-items) |
| A3 | GCP org policy on `allUsers` (U6) | `gcloud resource-manager org-policies describe iam.allowedPolicyMemberDomains --project P --effective` | Not enforced: `deploy-mediator.sh` may bind `allUsers`. Enforced: deploy with the `--no-invoker-iam-check` path instead (no org change needed) | [matrix U6](permissions-matrix.md#5-design-corrections-and-open-items) |

## B. GCP deploy

| ID | What | How | Expected | Source |
| :--- | :--- | :--- | :--- | :--- |
| B1 | Cloud Build staging bucket (U10, resolved by C19) | `scripts/build-runner-image.sh --apply` | The build uploads its source to the dedicated bucket `BUILD_STAGING_BUCKET` (default `P-cr-ci-staging`, created by `bootstrap-gcp.sh`) and succeeds. A storage 403 means the C19 grant (`roles/storage.objectViewer` for `BUILD_SA_NAME` on that bucket) is missing: re-run `scripts/bootstrap-gcp.sh --apply`. Never grant anything on the shared `gs://P_cloudbuild` | [matrix C19, U10](permissions-matrix.md#5-design-corrections-and-open-items) |
| B2 | Job volume flags (U9) | `scripts/deploy-job.sh --apply` twice (create, then update), then `gcloud run jobs describe JOB --region R --project P --format='yaml(spec.template.spec.template.spec.volumes,spec.template.spec.template.spec.containers[0].volumeMounts)'` | Both runs succeed with the GA `gcloud run jobs` group; one `ephemeral-disk` volume of `RUNNER_DISK_GI` GiB mounted at `/scratch` after the update (no duplicate). If GA rejects it, switch the script to `gcloud beta` and record it | [matrix U9](permissions-matrix.md#5-design-corrections-and-open-items) |
| B3 | Mediator `max-instances=1` (precondition 3) | `gcloud run services describe SVC --region R --project P --format='value(spec.template.metadata.annotations."autoscaling.knative.dev/maxScale")'` | `1` (`deploy-mediator.sh` also checks it) | [architecture: deploy preconditions](architecture.md#deploy-preconditions) |
| B4 | Runner identity has no roles (precondition 4) | `gcloud projects get-iam-policy P --flatten='bindings[].members' --filter='bindings.members:serviceAccount:EXEC' --format='value(bindings.role)'` | Empty output | [architecture: deploy preconditions](architecture.md#deploy-preconditions); matrix 2.2 |
| B5 | Narrower mediator role (U3, optional) | `gcloud policy-intelligence troubleshoot-policy iam //run.googleapis.com/projects/P/locations/R/jobs/JOB --principal-email=MED --permission=run.jobs.runWithOverrides`, and the same for `run.executions.cancel`, `run.executions.list` and `iam.serviceAccounts.actAs` on `EXEC` | Record which role grants each. Only if all are covered by `roles/run.jobsExecutorWithOverrides` plus a list/cancel role, open a follow-up to narrow C10/C11 | [matrix U3](permissions-matrix.md#5-design-corrections-and-open-items) |

## C. GitHub setup

| ID | What | How | Expected | Source |
| :--- | :--- | :--- | :--- | :--- |
| C1 | Webhook URL and secret | App settings, **Advanced**, **Recent Deliveries**, redeliver `ping`; `medlog 'jsonPayload.event="webhook_ignored"'` | Response `200` `ignored`; a `webhook_ignored` line with `github_event=ping`, `reason=event_type` | [github-setup.md 5](github-setup.md#5-check-the-webhook) |
| C2 | Fork PR approval precondition (and U4) | Repo **Settings**, **Actions**, **General**, "Approval for running fork pull request workflows from contributors" | "Require approval for all external contributors" saved. Record the role of the person who could change it (expected: repo admin), and close U4 with it. A PAT cannot read this setting (403 on `GET /repos/REPO/actions/permissions/fork-pr-contributor-approval`, seen in P6): check it in the UI as the repo owner or an admin | [github-setup.md 6](github-setup.md#6-deploy-precondition-fork-pr-approval-g8); [matrix U4](permissions-matrix.md#5-design-corrections-and-open-items) |
| C3 | Installation found by the mediator | First PoC dispatch (D1), then `medlog 'jsonPayload.event=("dispatch" OR "error")'` | A `dispatch` line; no `error` with an installation or `generate-jitconfig` stage | [github-setup.md 4](github-setup.md#4-install-the-app) |
| C4 | `runner_group_id` 1 on the repo endpoint (U1) | Same dispatch | `generate-jitconfig` accepted (a `dispatch` line, no 4xx `error`) | [matrix U1](permissions-matrix.md#5-design-corrections-and-open-items) |
| C5 | Default labels on JIT runners (U7) | While the PoC job runs: command C5 below | Record the labels. `[scion-cr-8c32g]` only: close U7. A PAT without repository Administration read gets 403 on the runners API (P6); use a token with that permission, or the UI (**Settings**, **Actions**, **Runners**) while the job runs. `self-hosted`/`X64` also present: update U7 and architecture's "Residual risk" note (the hijack guard still covers it) | [matrix U7](permissions-matrix.md#5-design-corrections-and-open-items); [architecture](architecture.md#residual-risk-label-matched-job-assignment) |

Command C5:

```bash
gh api repos/REPO/actions/runners \
  --jq '.runners[] | select(.name | startswith("cr-job-")) | {name, labels: [.labels[].name]}'
```

## D. PoC run

Run the PoC by pushing a throwaway `cr-ci-poc-*` branch that adds
`workflows/cr-runner-poc.yml` under `.github/workflows/`
([workflows/README.md](../workflows/README.md#running-the-poc-a-push-triggered-throwaway-branch);
[github-setup.md 7](github-setup.md#7-poc-run-before-any-routing)). Not
`workflow_dispatch`: that needs the file on the default branch, and the fork's
main must mirror upstream. Run it at least twice, from a new branch or a new
commit: the first run may start from a cold build cache if the lint
workload's cache is missing or old. Delete the branches and their caches
afterwards. Before the PoC, `RUNNER_DISK_GI` must be about 25 (after the
per-instance quota increase, C15) or `RUNNER_BUILD_CACHE_IN_RAM=true`: with the
default 10 GiB, a warm lint runs out of disk (P6). Deploy the job with
`--warm-up`, or the first run's D1 includes the image import.

| ID | What | How | Expected | Source |
| :--- | :--- | :--- | :--- | :--- |
| D1 | Queue-to-start latency | Command D1 below | `started_at - created_at` < 20 s | Design 7 phase 1; [workflows/README](../workflows/README.md#poc-success-criteria) |
| D2 | `/scratch` mount detection on gVisor | `runlog 'textPayload:"/scratch is a mount"'` | `/scratch is a mount (<fstype>): listed in /proc/self/mountinfo` (or the device fallback); no execution exits 2. tmpfs means the volume is missing | [runner README, step 2](../runner/README.md#runtime-contract) |
| D3 | Metadata ID token format and claim | `runlog 'textPayload:"claim"'`; `medlog 'jsonPayload.event=("claim_consumed" OR "claim_rejected")'` | `claim succeeded on attempt 1`; one `claim_consumed`; no `claim_rejected` with `identity_missing`/`identity_invalid` and no "metadata server returned 200 without an ID token" | [runner README, step 4](../runner/README.md#runtime-contract) |
| D4 | HOME, links, zstd, lexical `GITHUB_WORKSPACE` | The PoC's "Check cr-ci cache parity" and "Runner facts" step logs | Every parity line `ok`; `GITHUB_WORKSPACE=/home/runner/work/scion/scion (physically /scratch/_work/scion/scion)`; `go env GOCACHE=/home/runner/.cache/go-build` | [runner README, step 3](../runner/README.md#runtime-contract); [cache sharing](../workflows/README.md#cache-sharing) |
| D5 | Warm Go cache restored from ubuntu-latest | "Check that the Go cache is warm" step log | `go cache warm: yes` (matched key and >= 1000 files in `$(go env GOCACHE)`). If NO: check D4 first | [cache sharing](../workflows/README.md#cache-sharing) |
| D6 | Lint duration (warm) | "golangci-lint" step log, `golangci-lint exit=0 duration=...s` | < 14 min. CI's warm lint on ubuntu-latest took 1310-1802 s in October 2026 (ci.yml's comment still says 584-776 s); P6b measured 772-796 s at concurrency 8, GOGC 100 | Design 7 phase 1 |
| D7 | Peak memory and disk use | The lint step's `sample peaks:` line, and the "Memory and disk report" step | Memory: `memory.peak` and `memory.current` are **not available under gVisor**; read the sampled `mem_used_mb` peak (`MemTotal - MemAvailable`, which includes RAM-held files such as the RAM build cache), or the job-level `run.googleapis.com/container/memory/utilizations` metric. P6: compile near 21 GB; 27.9 GB of 32 GiB with the build cache in RAM. Disk: the sampled `scratch_used_mb` peak stays below `RUNNER_DISK_GI` with margin. P6 measured the Go build cache growing to 8-9.5 GB during the lint and about 15-20 GiB in total, so the PoC needs `RUNNER_DISK_GI` about 25 (after C15) or `RUNNER_BUILD_CACHE_IN_RAM=true`; at the default 10 GiB it runs out of space. `du -shx ~` small (MBs) | Design 7 phase 1; P6 results |
| D8 | Deregistration and billing stop | After the job: `gh api repos/REPO/actions/runners --jq '[.runners[].name]'`; `gcloud run jobs executions list --job JOB --region R --project P --limit 3 --format='table(name, status.completionTime, status.succeededCount, status.failedCount)'` | No `cr-job-*` runner left; the execution completed within seconds of the job's end, and its result matches the job's (the entrypoint now reports the runner's real exit code, F-C). The runners API needs repository Administration read (a PAT without it gets 403); otherwise use the UI, or the mediator's view: `reconcile` lines with `runners_reaped` 0 and no `runner_reaped` events | Design 7 phase 1; architecture "Flow" step 3 |
| D9 | No stray mediator errors | `medlog 'severity>=WARNING'` over the PoC window | No `launch_failed`, `error`, `SECURITY_*`, `labels_unservable`, `runner_reaped` for the PoC jobs; `reconcile` lines every `RECONCILE_SCHEDULE` interval | [operations: monitoring](operations.md#monitoring-and-alerts) |
| D11 | Job guard hard stop (live; needs ptone's go-ahead and a throwaway fork) | Events outside the allowlist that fire only from the default branch (`workflow_dispatch`, `schedule`, `workflow_run`, ...) cannot be staged on a throwaway branch, so use a fork pull request: from a throwaway fork, open a PR whose workflow pins `runs-on` to `RUNNER_LABEL` and whose first step is `if: always()` and second `if: failure()`, each printing a marker line; approve it to run. Make a cr-ci runner idle when it queues (start a legitimate routed job and cancel it once its runner is up), so GitHub hands it the fork job. Then `runlog 'textPayload:"job_guard_refused"'`, the job's step list, and the runners list (D8) | The mediator refuses the fork run a runner of its own (`SECURITY_REJECT_FORK_PR`, `reason=head_repository_differs`). The idle runner's execution exits 5 with `ERROR job_guard_refused: ... reason=head_repository_differs event=pull_request`. GitHub shows the job failed with **neither** marker line (no step ran, not even the always() and failure() ones). The `cr-job-*` registration is gone afterwards (the hijack guard's `SECURITY_KILL_HIJACKED_RUNNER` for that runner, or `runner_reaped`). Close the PR and delete the fork afterwards | [runner README, "Job guard"](../runner/README.md) |
| D12 | Job guard integrity at startup (any execution, warm-up included) | `runlog 'textPayload:"job guard:"'` and `runlog 'textPayload:"ignoring CR_"'` over the executions since the deploy | Every execution logs `job guard: /usr/local/bin/cr-ci-job-guard.sh matches its pinned sha256` before its claim; none logs `refusing to start the runner` or `ignoring CR_...` | [runner README, "No fail-open"](../runner/README.md) |
| D10 | Restart recovery (optional) | During a run's cold start, redeploy the mediator (`deploy-mediator.sh --apply`) | The runner exits 3; the next reconciles delete its registration and relaunch the job, which then passes | [operations: single instance](operations.md#mediator-single-instance) |

Command D1:

```bash
gh api repos/REPO/actions/runs/RUN_ID/jobs --jq '.jobs[] | {name, created_at, started_at}'
```

## E. Cache sharing in the other direction

D5 proves an ubuntu-latest save restores on cr-ci. The reverse also has to
hold after P7, when cr-ci saves the lint cache on `main` and a fallback run
on ubuntu-latest restores it. That relies on `@actions/glob` archiving the
logical `$HOME/...` paths rather than their `/scratch` realpaths (cr-ci-rev-5,
P5 review round 3).

| ID | What | How | Expected | Source |
| :--- | :--- | :--- | :--- | :--- |
| E1 | A cr-ci save restores on ubuntu-latest | The throwaway workflow below, on a throwaway branch `cr-ci-poc-e1` (needs ptone's word, like the PoC); pushing the branch runs it. Afterwards delete the branch and its cache (`gh cache delete crci-parity-<run_id> --repo REPO`) | Job 2 reports `cache-hit` true and thousands of files under `/home/runner/.cache/go-build`, and `/home/home` does not exist | [cache sharing](../workflows/README.md#cache-sharing); P5 review round 3 |

Workflow E1, as `.github/workflows/cr-ci-cache-parity-e1.yml` on the branch
`cr-ci-poc-e1` (actions pinned to the same SHAs as `ci.yml` and the PoC). It
triggers on the push of that branch, so the default branch never carries it
(P6 ran it this way and it passed):

```yaml
name: cr-ci cache parity E1
on:
  push:
    branches: [cr-ci-poc-e1]
permissions:
  contents: read
jobs:
  save-on-cr-ci:
    runs-on: scion-cr-8c32g
    steps:
      - uses: actions/checkout@d23441a48e516b6c34aea4fa41551a30e30af803 # v6
        with:
          persist-credentials: false
      - run: go build -o /dev/null ./cmd/...
      - uses: actions/cache/save@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0
        with:
          path: /home/runner/.cache/go-build
          key: crci-parity-${{ github.run_id }}
  restore-on-ubuntu:
    needs: save-on-cr-ci
    runs-on: ubuntu-latest
    steps:
      - id: restore
        uses: actions/cache/restore@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0
        with:
          path: /home/runner/.cache/go-build
          key: crci-parity-${{ github.run_id }}
      - run: |
          echo "cache-hit=${{ steps.restore.outputs.cache-hit }}"
          find /home/runner/.cache/go-build -type f | wc -l
          ls -d /home/home 2>&1 || true
```

Both jobs pass the same literal path string, as setup-go-cache does on both
runner types. On cr-ci it resolves to `/scratch/.cache/go-build`, and the
archive must still name it `../../../.cache/go-build`.

## F. Items with no PoC action

| ID | What | Why no action | Source |
| :--- | :--- | :--- | :--- |
| F1 | JIT config lifetime before first use (U2) | `CLAIM_TTL` (300 s) bounds the cr-ci side; D3 shows the config is used within seconds. Revisit only if claims fail after long cold starts | [matrix U2](permissions-matrix.md#5-design-corrections-and-open-items) |
| F2 | Larger runners for personal accounts (U5) | Only matters if a GitHub larger runner becomes the fallback target (G18) | [matrix U5](permissions-matrix.md#5-design-corrections-and-open-items) |

## G. After the PoC

- Update each source doc: close or rewrite U2-U10 in the matrix, turn the
  runner README's "P6 PoC checkpoint" notes into facts, fill the
  `TODO(P6)` items in [operations.md](operations.md), and record measured
  latency, duration and memory in [workflows/README.md](../workflows/README.md#poc-success-criteria).
- Go or no-go for P7 rests on D1, D5-D8 and E1.
