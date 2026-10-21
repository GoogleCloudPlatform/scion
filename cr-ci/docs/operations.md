# Operations (outline)

> **TODO(P4/P5/P6):** outline only; procedures are filled in as the scripts and
> the PoC land. Roles for every operation:
> [permissions-matrix.md, section 3](permissions-matrix.md#3-operations-at-a-glance).

## Rollout

- TODO: PoC workflow first; then set the routing variable per repo; watch 48 h.

## Fallback

- TODO: clear or reset `ROUTING_VARIABLE_NAME` (needs write); re-run queued
  jobs; emergency stop by suspending the App.

## Secret rotation

- TODO: webhook secret (App settings + new Secret Manager version + mediator
  restart, in an order that avoids rejected deliveries); App private key
  (new key, new version, restart, delete old key).

## Runner image updates

- TODO: bumping Go, golangci-lint or the actions runner (one pinned place);
  rebuild and redeploy the job; rollback to the previous tag.

## Capacity and quotas

- TODO: raising `MAX_CONCURRENT_RUNNERS` together with the ephemeral-disk quota.

## Monitoring and alerts

- TODO: mediator `launch_failed` logs; queued-job starvation; execution results;
  memory utilisation of runner executions.

## Troubleshooting

- TODO: job stuck "waiting for a runner"; webhook delivery failures (signature);
  orphaned registrations; OOM in a runner; quota errors.

## Cost

- TODO: expected cost per job and per month (design section 5); where to look
  in billing.
