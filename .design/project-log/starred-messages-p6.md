# Starred messages, phase 6: release verification and controlled enablement (code items)

**Date:** 2026-10-28
**Issue:** ptone/scion#6465 (epic ptone/scion#5797)
**Experiment:** web.chat_starred_messages (unchanged in this PR; still default off)

## What shipped

- **Maintenance operation `chat-star-reconcile`** (`pkg/hub/chat_star_reconcile.go`, seeded in
  `pkg/store/entadapter/maintenance_store.go` as `ChatStarReconcileKey`, a routine operation). It drives the
  upkeep's store scanner (`PurgeMessageStarCandidates`) from the start of the table in batches of 500, each under
  its own 30 s context, until it wraps (one sweep), and repeats sweeps until one repairs and purges nothing. At
  most 3 sweeps; a run that is still changing stars after the third fails with "run the operation again". The whole
  run is bounded by `maintenanceRunTimeouts` (30 min); if the time limit or a shutdown stops a run, the run fails
  with that reason, the totals so far and "run the operation again". There is no resume state: every run starts
  over. The run log lists every sweep and the totals (examined, repaired, purged); completion is the run's status
  (the framework records no result on success). No parameters: the key is in `operationParamAllowlist` with an
  empty list, so any parameter is a 400 before a run is created. Without a chat store with star support the run
  fails visibly.
- **Concurrency:** the framework's in-flight check stops a second run of the same operation on the same instance
  (check-then-create is serialized only within one process), and usually across instances through the run row. The
  real guarantee is that concurrent runs, on two replicas or beside the replicas' upkeep passes, are harmless: each
  batch repairs and purges in its own transaction and both steps are idempotent. The operation does not share the
  upkeep's per-replica `busy` flag, so it never skips or delays an upkeep pass.
- **Registration:** no new route or authz catalog entry. The operation runs through
  `/api/v1/admin/maintenance/operations/{id}/run`, already under `hub.maintenance.execute`; the key is resolved in
  `resolveMaintenanceExecutor`. No request-path registry change (no new handler reads the path).
- **Tests:** the driver against a scripted scanner (batching, exact-multiple end, sweep bound, store error,
  cancellation, the time limit, invalid limits); a both-dialect contract case in `testMessageStarLifecycleContract`
  (repairs and purges to exhaustion in two sweeps, between another replica's batches (sequential), idempotent
  re-run); the operation through the operations handler with the experiment off and on; registration (seeded
  category, parameters refused, bounded run context).
- **Deferred tests folded in:** ptone/scion#6414 (semicolon-separated starred queries in the experiment-off 404
  cases; a web case where a refresh far up the starred-only list, by reload and by around, leaves it empty) and
  ptone/scion#6413 items 2-4 (the Starred rail cursor end to end across two key-ring replicas with binding,
  retirement and closed issuance; starred-history paging across a rotation and a legacy-mode upgrade; the position
  AAD case "kid in AAD, not in wire" moved to a ring whose legacy readers include gen1, with a positive control).

## Things worth knowing

- **"Exhausted" means a full sweep with no change.** One sweep already fixes everything it examines, so a clean
  table costs one sweep and a dirty one two; the confirming sweep is what lets the operator enable the experiment.
- **A routine operation's counts live in the run log.** `launchMaintenanceRun` writes `Result` only on failure, so
  an executor that wants to report on success writes to its logger.
- **The item text of ptone/scion#6413 items 2-4 came from the phase 4 code follow-ups**, not phase 2; read the
  follow-up notes before guessing at a terse issue line.
- **Every pkg/hub test this phase touched sits in one shard** at this head (`hack/hubshard/hubshard.sh -locate`),
  so one shard compile covers them.

## Follow-ups

- Flipping the experiment's default to on is a separate change, after a browser acceptance pass on an integration
  hub.
