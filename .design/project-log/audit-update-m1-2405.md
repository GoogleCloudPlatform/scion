# Audit update milestone 1 / #2405

## Checkpoint status

This checkpoint is based on the exact approved #2404 head
`6fbf81f85957b653134ea1abf7dbc69432060751`. It implements the owned
store-backed access-constraint history endpoint, response mapping, authorization
decision, and tuple cursor. Full #2405 integration is **gated** at the shared
authentication layer, so the existing web timeline was intentionally not changed
after the engineering-manager stop instruction.

## Endpoint and cursor contract

`GET /api/v1/admin/access-constraints/{id}/audit` now resolves the live access
constraint first, authorizes `hub.audit.read` against that constraint and its
project parent when applicable, and reads only `ListConstraintHistory` rows for
the exact live constraint. It has no process-memory fallback. Missing live
constraints, handler-level missing identity, denied permission, wrong project
scope, and deleted constraints use the same not-found response. A store failure
is returned explicitly rather than represented as an empty timeline.

Responses are ordered by `(occurred_at DESC, event_id DESC)`. The opaque
base64url JSON cursor is versioned, bound to the constraint ID, and contains the
last occurrence-time/event-ID tuple. Pagination resumes with a strict older-than
comparison, so equal timestamps are deterministic and newer concurrent inserts
do not duplicate or skip the older walk. Malformed, structurally invalid,
unknown-version, oversized, and cross-constraint cursors fail before the history
store call. Page size defaults to 50 for missing, non-numeric, zero, or negative
values and is capped at 200.

The response maps typed history columns directly and parses only the two typed
composite JSON columns (`impact_counts` and `changed_fields`), never the rendered
generic audit event. `totalCount` is the current retained matching row count,
`totalCountExact` is true for that current query, and the response describes the
fixed 1,000-row retained window. It is not a lifetime count or pagination
snapshot guarantee.

## Gated shared-auth dependency

The production `UnifiedAuthMiddleware` and `DevAuthMiddleware` return 401 for a
credential-less request before the endpoint or its route authorization can run.
The normative #2405 contract requires absent authentication, denial, and absence
to be indistinguishable 404 responses. Those middleware are a separately owned
shared-auth layer and were not modified. Consequently, handler-boundary tests
prove the uniform 404 behavior, but the full-stack absent-authentication case is
still gated. The existing API client/timeline/detail view and its web tests were
not changed because endpoint/view integration was stopped at this dependency
boundary pending an approved shared-auth solution.

Residual work after that dependency is approved:

- make the credential-less full-stack endpoint path reach the privacy-preserving
  handler without weakening authentication for any other route;
- add the full-stack absent-authentication 404 regression;
- wire and test the existing timeline/client/detail path, including pagination,
  stale-resource clearing, empty/error/404 states, and chronological rendering;
- run the remaining scoped Go and web verification gates.

## Verification

- `go test -count=1 -p 2 ./pkg/hub -run '^TestConstraintAuditHistory_'` — PASS.
  This covers equal-timestamp tuple ordering, a concurrent newer insert between
  pages, last-page empty token, current retained count semantics, page-size
  bounds, malformed/unknown/cross-constraint cursors, no cross-resource rows,
  handler-level missing identity, denied permission, wrong project scope,
  missing/deleted constraints with cascade, and explicit store failure.
- `gofmt` on all changed Go files — clean.
- `git diff --check` — PASS before the checkpoint commit.

`make ci` and `make ci-full` were not run because the campaign broker-workload
rule explicitly prohibits them. Broader targeted route-metadata, vet/build, and
lint gates are deferred until after the required implementation checkpoint push.

After checkpoint `387a8c5d` was pushed, the first route reconciliation run
correctly failed because the new exact route was not yet represented in the
authorization-operation catalog (`186/187` routes covered). A narrow
authentication-only entry-point exemption now records that this read is guarded
by the handler's live-resource-scoped `hub.audit.read` decision; this is the
authorization-operation route registry, not the frozen audit-event catalog.
The corrected targeted run passed:

- `go test -count=1 -p 2 ./pkg/hub ./pkg/hub/authzop -run '^(TestConstraintAuditHistory_|TestB7_GetConstraintAudit|TestB7_RouteMetadata_ReadPermission|TestEntryPointsCoverRouteMetadata|TestStaleExemptionDetection)$'` — PASS.
