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

## Resumed full-stack integration

The shared-auth dependency was approved at exact head
`b4b2d7bb27de6bf3a8cd75ef21cb3e47fd070fbd`. Its exact-route response
normalizer makes authentication-produced 400/401/403 responses outwardly
identical to the canonical access-constraint 404 while preserving authenticated
downstream responses and infrastructure failures. The approved auth files were
frozen and remain unchanged by the resumed work.

Exactly one existing view path is now wired: the API client's `listAudit`
contract, `scion-access-boundary-audit-timeline`, and its existing use on the
admin access-boundary detail page. The former speculative generic-envelope type
and fixture were replaced with the actual purpose-specific retained fields. The
timeline renders operation, time, actor kind/ID, revision, classification,
preview/audit/correlation IDs, and typed impact counts. It deliberately does not
render draft hashes, credential data, raw payloads, internal scope, or raw error
text.

Initial loads clear prior rows. Resource changes invalidate earlier requests,
and generation/resource checks prevent late responses from replacing the new
resource state. Pagination consumes `nextPageToken`, filters rows to the active
constraint, removes duplicate event IDs, and stops repeated cursors. A 404
clears rows and renders the non-enumerating `Audit history is unavailable.`;
other failures clear rows and render an explicit bounded service-error message.
Loading and genuinely empty retained history remain distinct states.

Focused resume evidence before the implementation checkpoint:

- `npm test -- --run src/client/access-boundaries-api.test.ts src/components/shared/access-boundary-audit-timeline.test.ts src/components/pages/admin-access-boundary-detail-audit.test.ts src/shared/access-boundaries.test.ts` — PASS, 4 files / 68 tests. The runner emitted non-failing localhost:3000 connection noise from imported app infrastructure.
- `npm run typecheck` — PASS.
- `go test -count=1 -p 2 ./pkg/hub -run '^TestConstraintAuditHistory_CreateIdentityFlowsThroughEndpoint$'` — PASS. The approved create path's audit event ID is identical in the retained store row and authorized history response, whose typed shape is the one consumed by the client/timeline tests.

The first focused web attempt could not load the project Vitest configuration
because dependencies were absent. `npm ci` installed the lockfile-pinned tree;
no package or lockfile changed. Its audit summary reported three pre-existing
dependency findings (one low, two high); dependency remediation is outside this
unit and no dependency was added or upgraded.

Post-checkpoint verification at `c1f908c5`:

- `go test -count=1 -p 2 ./pkg/hub -run '^(TestConstraintAuditHistory_|TestUnifiedConstraintAudit|TestConstraintAuditAuth|TestConstraintAuditValidCredentials|TestConstraintAuditBrokerAuth_)'` — PASS (package 8.414s). This covers the endpoint/cursor/create identity suite plus missing, malformed, expired, valid, neighboring-route, broker-HMAC, and on-behalf-of exact-route authentication behavior.
- `go test -count=1 -p 2 ./pkg/store/entadapter -run '^TestConstraintHistory_(PrunesDeterministicallyPerConstraint|CascadesWithLiveConstraint|SurvivesRestartAndSecondStoreInstance)$'` — PASS (package 1.067s).
- Focused web tests above rerun — PASS, 4 files / 68 tests.
- `npm run typecheck` — PASS.
- Production-file `npx eslint` for the three changed source files — PASS with 28 pre-existing explicit-return-type warnings and zero errors. Typed ESLint cannot parse any `*.test.ts` because the repository `tsconfig.json` excludes tests; the four changed test files are covered by Vitest and TypeScript instead.
- `npm run build` — PASS (481 modules transformed; production assets copied).
- `go vet -p 2 ./pkg/hub` — PASS.
- `go build -buildvcs=false -p 2 ./pkg/hub` — PASS.
- `timeout 10m env GOGC=40 golangci-lint run --new-from-rev=b4b2d7bb27de6bf3a8cd75ef21cb3e47fd070fbd --concurrency=1 ./pkg/hub/...` — environment-limited: exited 124 at the required ten-minute bound with no diagnostics emitted.
- Go formatting, focused Prettier formatting, and `git diff --check` — clean.

The approved shared-auth review already supplied a focused race pass at exact
dependency head `b4b2d7bb`. The resumed delta changes no production Go or auth
code, so that accepted race evidence was not repeated. `make ci` and
`make ci-full` remain prohibited by the campaign workload rule.

## Review round 1 disposition

Required finding 1 is resolved. The Go retained-history response intentionally
uses `omitempty` for an empty correlation ID, and the matching
`AccessBoundaryAuditEvent.correlationId` client field is now optional. The
adjacent commit-response contract remains required; it was restored after the
earlier edit was found to have matched that wrong declaration. No Go wire,
cursor, auth, store, persistence, or view behavior changed.

Focused regressions prove all three sides of the correction:

- a retained row with an empty correlation ID produces an endpoint item with no
  `correlationId` property;
- the retained fixture and API client accept the omitted property without
  synthesizing a value, and the existing timeline does not render a correlation
  label/value for it;
- populated endpoint, fixture, client, and timeline behavior remains unchanged
  and renders the original correlation value.

Pre-checkpoint evidence:

- `npm test -- --run src/client/access-boundaries-api.test.ts src/components/shared/access-boundary-audit-timeline.test.ts src/shared/access-boundaries.test.ts` — PASS, 3 files / 67 tests. The fixture assertion first failed because it still required every correlation ID to be a string, then passed after the optional contract correction.
- `npm run typecheck` — PASS.
- `go test -count=1 -p 2 ./pkg/hub -run '^TestConstraintAuditHistory_(OmitsEmptyCorrelationID|CreateIdentityFlowsThroughEndpoint)$'` — PASS (package 1.462s).
- Focused Prettier and Go formatting — clean.

Post-checkpoint verification at `99f72331`:

- `go test -count=1 -p 2 ./pkg/hub -run '^TestConstraintAuditHistory_'` — PASS (package 4.201s).
- `npm test -- --run src/client/access-boundaries-api.test.ts src/components/shared/access-boundary-audit-timeline.test.ts src/shared/access-boundaries.test.ts` — PASS, 3 files / 67 tests.
- `npm run typecheck` — PASS.
- `npx eslint src/shared/access-boundaries.ts` — PASS with no output.
- Focused `npx prettier --check` — PASS.
- Go formatting and `git diff --check` — clean.

Scoped Go lint was not rerun: the round-one fix changes one Go regression test
but no production Go implementation, and the immediately preceding full-slice
lint evidence remains recorded above. No long lint job was started.
