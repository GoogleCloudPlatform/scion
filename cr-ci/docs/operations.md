# Operations (outline)

> **TODO(P4/P5/P6):** partly an outline; procedures are filled in as the
> scripts and the PoC land. The mediator behaviour described here (P2) is
> final. Roles for every operation:
> [permissions-matrix.md, section 3](permissions-matrix.md#3-operations-at-a-glance).

## Rollout

- TODO: PoC workflow first; then set the routing variable per repo; watch 48 h.

## Fallback

- TODO: clear or reset `ROUTING_VARIABLE_NAME` (needs write); re-run queued
  jobs; emergency stop by suspending the App.

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

## Secret rotation

- **Webhook secret:** add the new value as a new Secret Manager version,
  then change it in the GitHub App settings, back to back. No restart is
  needed: on a signature mismatch the mediator re-reads the secret (at most
  once a minute), and the secret it replaces stays valid for one hour, so
  deliveries signed with either secret are accepted during the switch. If
  the two steps are more than an hour apart, deliveries with the old secret
  start failing (401); redeliver them from the App's "Advanced" page, and
  `/reconcile` backfills their jobs anyway.
- **App private key:** generate a new key in the App settings, add it as a
  new Secret Manager version, restart the mediator (it caches the parsed key
  for its lifetime), then delete the old key in GitHub. Installation tokens
  are cached in memory only and refreshed five minutes before they expire.

## Runner image updates

- TODO: bumping Go, golangci-lint or the actions runner (one pinned place);
  rebuild and redeploy the job; rollback to the previous tag.

## Capacity and quotas

- TODO: raising `MAX_CONCURRENT_RUNNERS` together with the ephemeral-disk quota.

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
| `runner_swap` | INFO | A runner started a different legitimate job; harmless |
| `webhook_rejected` | WARNING | Bad signature, malformed payload or repo outside `TARGET_REPOS` |
| `webhook_ignored` | INFO / DEBUG / WARNING | Not `workflow_job`, or job without `RUNNER_LABEL`; WARNING with `reason=labels_unservable` when a job has `RUNNER_LABEL` plus other labels a cr-ci runner cannot satisfy (fix its `runs-on`) |
| `reconcile` | INFO | Reconcile summary (reaped, backfilled, left queued, errors) |
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
- **Legitimate job killed with `reason=run_lookup_failed`:** a GitHub API
  error hit the hijack guard during a runner swap; the guard fails closed.
  Re-run the job.
- **Runner exits 3 after a 503 storm on `/internal/claim`:** Google's signing
  keys could not be fetched (`error` with `stage=claim_identity`); the claim
  is kept and the runner retries 5xx, but five failed attempts exhaust its
  budget. The job is backfilled after its registration is reaped.
- **Webhook deliveries failing with 401:** the webhook secret in Secret
  Manager and in the App settings differ (see rotation above).
- **503 from `/webhook` and `/reconcile` with `not_configured`:** `MEDIATOR_URL`
  is empty; set it and redeploy.
- TODO: OOM in a runner; quota errors.

## Cost

- TODO: expected cost per job and per month (design section 5); where to look
  in billing.
