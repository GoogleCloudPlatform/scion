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

- **Webhook secret:** add the new value as a new Secret Manager version
  first, then change it in the GitHub App settings. No restart is needed: on
  a signature mismatch the mediator re-reads the secret, at most once a
  minute, so deliveries signed with the new secret are accepted within about
  a minute. Deliveries rejected in that minute (401) can be redelivered from
  the App's "Advanced" page, and `/reconcile` backfills their jobs anyway.
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
| `cancel` | INFO | Cancelled job: execution cancelled, registration deleted |
| `claim_consumed` / `claim_rejected` | INFO / WARNING | Runner fetched its JIT config / presented an unknown, used or expired token |
| `SECURITY_REJECT_FORK_PR` | WARNING | Fork-PR or `pull_request_target` run refused a runner. **Alert on this.** |
| `webhook_rejected` | WARNING | Bad signature, malformed payload or repo outside `TARGET_REPOS` |
| `webhook_ignored` | INFO / DEBUG | Not `workflow_job`, or job without `RUNNER_LABEL` |
| `reconcile` | INFO | Reconcile summary (reaped, backfilled, left queued, errors) |
| `reconcile_denied` | WARNING | `/reconcile` called without a valid scheduler ID token |
| `runner_reaped` / `execution_reaped` | WARNING | Stale registration deleted / execution past the task timeout cancelled |
| `error` | ERROR | A GitHub, Cloud Run or Secret Manager call failed (`stage`, `error`) |

- Suggested alerts: any `SECURITY_REJECT_FORK_PR`; repeated `launch_failed`
  or `error`; `capacity_full` persisting across several reconciles (raise
  capacity or check for stuck executions); missing `reconcile` lines for
  more than two intervals (scheduler broken).
- TODO(P6): execution results and memory utilisation of runner executions.

## Troubleshooting

- **Job stuck "waiting for a runner":** look for its `job_id` in the mediator
  logs. `capacity_full` means it is waiting for a slot; `SECURITY_REJECT_FORK_PR`
  means it will never get one; no line at all means the webhook was not
  delivered (App settings, "Recent deliveries") and the next `/reconcile`
  should pick it up.
- **Webhook deliveries failing with 401:** the webhook secret in Secret
  Manager and in the App settings differ (see rotation above).
- **503 from `/webhook` and `/reconcile` with `not_configured`:** `MEDIATOR_URL`
  is empty; set it and redeploy.
- TODO: OOM in a runner; quota errors.

## Cost

- TODO: expected cost per job and per month (design section 5); where to look
  in billing.
