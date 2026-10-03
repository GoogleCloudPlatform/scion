# tz-refactor task 4: UTC for access windows and JSON-embedded times

Fork issue: ptone/scion#2497 (part of ptone/scion#2457). Design §2.1.3.

## What changed
- `pkg/hub/handlers_access_constraints.go`: `constraintConditionReq.utcWindow()`
  converts `notBefore`/`expiresAt` to UTC (non-nil only, copies, request untouched)
  at all three sites that copy the window into `store.AccessConstraint`:
  `createAccessConstraint`, `updateAccessConstraint` and the preview conversion
  `draftToStoreConstraint`.
- `pkg/store/entadapter/agent_store.go`: `utcExposedPorts` normalises
  `ExposedPort.ExposedAt` in `CreateAgent` and `UpdateAgentExposedPorts`, the only
  writers of `agents.exposed_ports`. The handler's existing `time.Now().UTC()` stays.
- `pkg/ent/entc/json_embedded_time_test.go`: enumerates every ent entity (through
  the `Get` method of each `ent.Client` field), walks every non-scalar field, and fails
  on any nested `time.Time` that is not on the allowlist. It also fails on stale
  allowlist entries. A self-test checks the walker on a synthetic entity.

## Scope deviation (agreed with tz-em)
The issue assumed `PolicyConditions.ValidFrom`/`ValidUntil` have create/update paths.
On main they have none: the `/api/v1/policies` handlers return 410, the legacy
`PolicyStore` was removed, and nothing outside `pkg/ent` calls `SetConditions`. So
they are allowlisted with a comment saying any future writer must normalise them.
No ent hook and no unused helper were added.

## Observations
- On create/update, task 2's ent `UTCTimeHook` and the SQLite `_timezone=UTC` option
  already return the scalar window columns in UTC. So the end-to-end handler test
  passes with or without the ingest change. The ingest `.UTC()` is defence in depth
  there. It is the only fix for the preview conversion, which never persists.
- PR ptone/scion#2476 (fireAt/fireIn) touches no JSON-embedded field, so it needs no
  allowlist entry.

## Tests
`go test -p 2 -count=1 ./pkg/ent/entc/ ./pkg/store/entadapter/` and the
access-constraint/port tests in `./pkg/hub/`, run under `TZ=UTC`, `Asia/Tokyo` and
`Asia/Kathmandu`: all pass. Each new test fails when its fix is reverted, except the
end-to-end handler test (see above).
golangci-lint (`--new-from-rev=upstream/main`, changed packages): 0 issues.

## Follow-ups
- U3 (`utc-timestamp-normalize`) must still rewrite existing JSON-embedded values.
