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
| B1 | Cloud Build staging access (U10) | `scripts/build-runner-image.sh --apply` | The build succeeds. On a storage 403 on `gs://P_cloudbuild` (or `BUILD_STAGING_BUCKET`), grant `roles/storage.objectViewer` on that bucket only, and record it in matrix C7 and 2.2 | [matrix U10](permissions-matrix.md#5-design-corrections-and-open-items) |
| B2 | Job volume flags (U9) | `scripts/deploy-job.sh --apply` twice (create, then update), then `gcloud run jobs describe JOB --region R --project P --format='yaml(spec.template.spec.template.spec.volumes,spec.template.spec.template.spec.containers[0].volumeMounts)'` | Both runs succeed with the GA `gcloud run jobs` group; one `ephemeral-disk` volume of `RUNNER_DISK_GI` GiB mounted at `/scratch` after the update (no duplicate). If GA rejects it, switch the script to `gcloud beta` and record it | [matrix U9](permissions-matrix.md#5-design-corrections-and-open-items) |
| B3 | Mediator `max-instances=1` (precondition 3) | `gcloud run services describe SVC --region R --project P --format='value(spec.template.metadata.annotations."autoscaling.knative.dev/maxScale")'` | `1` (`deploy-mediator.sh` also checks it) | [architecture: deploy preconditions](architecture.md#deploy-preconditions) |
| B4 | Runner identity has no roles (precondition 4) | `gcloud projects get-iam-policy P --flatten='bindings[].members' --filter='bindings.members:serviceAccount:EXEC' --format='value(bindings.role)'` | Empty output | [architecture: deploy preconditions](architecture.md#deploy-preconditions); matrix 2.2 |
| B5 | Narrower mediator role (U3, optional) | `gcloud policy-intelligence troubleshoot-policy iam //run.googleapis.com/projects/P/locations/R/jobs/JOB --principal-email=MED --permission=run.jobs.runWithOverrides`, and the same for `run.executions.cancel`, `run.executions.list` and `iam.serviceAccounts.actAs` on `EXEC` | Record which role grants each. Only if all are covered by `roles/run.jobsExecutorWithOverrides` plus a list/cancel role, open a follow-up to narrow C10/C11 | [matrix U3](permissions-matrix.md#5-design-corrections-and-open-items) |

## C. GitHub setup

| ID | What | How | Expected | Source |
| :--- | :--- | :--- | :--- | :--- |
| C1 | Webhook URL and secret | App settings, **Advanced**, **Recent Deliveries**, redeliver `ping`; `medlog 'jsonPayload.event="webhook_ignored"'` | Response `200` `ignored`; a `webhook_ignored` line with `github_event=ping`, `reason=event_type` | [github-setup.md 5](github-setup.md#5-check-the-webhook) |
| C2 | Fork PR approval precondition (and U4) | Repo **Settings**, **Actions**, **General**, "Approval for running fork pull request workflows from contributors" | "Require approval for all external contributors" saved. Record the role of the person who could change it (expected: repo admin), and close U4 with it | [github-setup.md 6](github-setup.md#6-deploy-precondition-fork-pr-approval-g8); [matrix U4](permissions-matrix.md#5-design-corrections-and-open-items) |
| C3 | Installation found by the mediator | First PoC dispatch (D1), then `medlog 'jsonPayload.event=("dispatch" OR "error")'` | A `dispatch` line; no `error` with an installation or `generate-jitconfig` stage | [github-setup.md 4](github-setup.md#4-install-the-app) |
| C4 | `runner_group_id` 1 on the repo endpoint (U1) | Same dispatch | `generate-jitconfig` accepted (a `dispatch` line, no 4xx `error`) | [matrix U1](permissions-matrix.md#5-design-corrections-and-open-items) |
| C5 | Default labels on JIT runners (U7) | While the PoC job runs: `gh api repos/REPO/actions/runners --jq '.runners[] \| select(.name \| startswith("cr-job-")) \| {name, labels: [.labels[].name]}'` | Record the labels. `[scion-cr-8c32g]` only: close U7. `self-hosted`/`X64` also present: update U7 and architecture's "Residual risk" note (the hijack guard still covers it) | [matrix U7](permissions-matrix.md#5-design-corrections-and-open-items); [architecture](architecture.md#residual-risk-label-matched-job-assignment) |

## D. PoC run

Dispatch the PoC ([github-setup.md 7](github-setup.md#7-poc-run-before-any-routing)):
`gh workflow run cr-runner-poc.yml --repo REPO -f runner-label=scion-cr-8c32g`.
Run it at least twice: the first run may start from a cold build cache if the
lint workload's cache is missing or old.

| ID | What | How | Expected | Source |
| :--- | :--- | :--- | :--- | :--- |
| D1 | Queue-to-start latency | `gh api repos/REPO/actions/runs/RUN_ID/jobs --jq '.jobs[] \| {name, created_at, started_at}'` | `started_at - created_at` < 20 s | Design 7 phase 1; [workflows/README](../workflows/README.md#poc-success-criteria) |
| D2 | `/scratch` mount detection on gVisor | `runlog 'textPayload:"/scratch is a mount"'` | `/scratch is a mount (<fstype>): listed in /proc/self/mountinfo` (or the device fallback); no execution exits 2. tmpfs means the volume is missing | [runner README, step 2](../runner/README.md#runtime-contract) |
| D3 | Metadata ID token format and claim | `runlog 'textPayload:"claim"'`; `medlog 'jsonPayload.event=("claim_consumed" OR "claim_rejected")'` | `claim succeeded on attempt 1`; one `claim_consumed`; no `claim_rejected` with `identity_missing`/`identity_invalid` and no "metadata server returned 200 without an ID token" | [runner README, step 4](../runner/README.md#runtime-contract) |
| D4 | HOME, links, zstd, lexical `GITHUB_WORKSPACE` | The PoC's "Check cr-ci cache parity" and "Runner facts" step logs | Every parity line `ok`; `GITHUB_WORKSPACE=/home/runner/work/scion/scion (physically /scratch/_work/scion/scion)`; `go env GOCACHE=/home/runner/.cache/go-build` | [runner README, step 3](../runner/README.md#runtime-contract); [cache sharing](../workflows/README.md#cache-sharing) |
| D5 | Warm Go cache restored from ubuntu-latest | "Check that the Go cache is warm" step log | `go cache warm: yes` (matched key and >= 1000 files in `$(go env GOCACHE)`). If NO: check D4 first | [cache sharing](../workflows/README.md#cache-sharing) |
| D6 | Lint duration (warm) | "golangci-lint" step log, `golangci-lint exit=0 duration=...s` | < 14 min, and comparable with CI's warm 584-776 s on ubuntu-latest (expected: shorter, without `-c=1`) | Design 7 phase 1 |
| D7 | Peak memory and disk use | "Memory and disk report" step log | `memory.peak` about 15.5-17 GiB (about 50% of 32 GiB); caches (5-8 GiB) under `/scratch`; `du -shx ~` small (MBs). If `memory.peak` is unavailable under gVisor, use the execution's `run.googleapis.com/container/memory/utilizations` metric | Design 7 phase 1 |
| D8 | Deregistration and billing stop | After the job: `gh api repos/REPO/actions/runners --jq '[.runners[].name]'`; `gcloud run jobs executions list --job JOB --region R --project P --limit 3 --format='table(name, status.completionTime, status.succeededCount, status.failedCount)'` | No `cr-job-*` runner left; the execution completed within seconds of the job's end, succeeded | Design 7 phase 1; architecture "Flow" step 3 |
| D9 | No stray mediator errors | `medlog 'severity>=WARNING'` over the PoC window | No `launch_failed`, `error`, `SECURITY_*`, `labels_unservable`, `runner_reaped` for the PoC jobs; `reconcile` lines every `RECONCILE_SCHEDULE` interval | [operations: monitoring](operations.md#monitoring-and-alerts) |
| D10 | Restart recovery (optional) | During a run's cold start, redeploy the mediator (`deploy-mediator.sh --apply`) | The runner exits 3; the next reconciles delete its registration and relaunch the job, which then passes | [operations: single instance](operations.md#mediator-single-instance) |

## E. Cache sharing in the other direction

D5 proves an ubuntu-latest save restores on cr-ci. The reverse also has to
hold after P7, when cr-ci saves the lint cache on `main` and a fallback run
on ubuntu-latest restores it. That relies on `@actions/glob` archiving the
logical `$HOME/...` paths rather than their `/scratch` realpaths (cr-ci-rev-5,
P5 review round 3).

| ID | What | How | Expected | Source |
| :--- | :--- | :--- | :--- | :--- |
| E1 | A cr-ci save restores on ubuntu-latest | A throwaway `workflow_dispatch` workflow on a test branch of the fork (needs ptone's word, like the PoC) with two jobs. Job 1 runs on `scion-cr-8c32g`: `go build ./cmd/...`, then `actions/cache/save` with `path: /home/runner/.cache/go-build` and `key: crci-parity-<run_id>`. Job 2 runs on `ubuntu-latest` with `needs:` job 1: `actions/cache/restore` with the same path and key, then `find ~/.cache/go-build -type f \| wc -l` and `ls -d /home/home`. Afterwards: `gh cache delete crci-parity-<run_id> --repo REPO` | Job 2 reports `cache-hit` true and thousands of files under `/home/runner/.cache/go-build`, and `/home/home` does not exist | [cache sharing](../workflows/README.md#cache-sharing); P5 review round 3 |

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
