# Heartbeat skips slug-matched entries for another agent ID (ptone/scion#7983)

Follow-on to ptone/scion#7930: in `handleBrokerHeartbeat` (`pkg/hub/handlers_runtime_brokers.go`), when an inventory entry matched by `(project, slug)` carries a canonical `agentId` that differs from the matched agent's ID, the entry is recorded as a runtime orphan candidate and skipped for the matched agent (`continue`).

- **Skipped updates:** the superseded entry no longer marks the current agent `report.present` or `report.observed`, adds no `runEntries.addOwn` labels, and runs no status, config backfill or runtime-target update for the current agent, so an older instance under a reused slug cannot hide a missing current instance.
- **Compatibility and orphans:** entries without `agentId` (older brokers or non-UUID legacy labels cleared by `heartbeatAgentID`) keep slug-only matching; mismatched entries still reach `recordRuntimeOrphans` and classify as `superseded`.
