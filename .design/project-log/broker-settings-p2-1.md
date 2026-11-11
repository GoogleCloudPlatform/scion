# Broker settings P2.1: per-broker `maxAgents` vertical slice (ptone/scion#2061 P2, ptone/scion#2177)

Base: started stacked on PR ptone/scion#2168 (frozen at `e8013da1` on `origin/feat/provider-capacity`);
that PR has since landed upstream as GoogleCloudPlatform/scion#2097, and this branch is rebased onto
`upstream-main` on top of it. Design: `/scion-volumes/scratchpad/projects/broker-settings/design.md`
§5, especially §5.1–§5.4, §5.6, §5.7 P2.1, §5.9. Branch `scion/broker-settings-p2-1`.

## What shipped

- **Ent schema + generated code**: new `broker_settings` table (`pkg/ent/schema/brokersetting.go`),
  one JSON document per runtime broker with an `int64` revision column for compare-and-set, mirroring
  `HubSetting`'s existing pattern rather than inventing a new one. `go generate ./pkg/ent` output
  committed.
- **Store layer**: `store.BrokerSettings{MaxAgents *int64}` / `store.BrokerSettingsRecord`, the
  `BrokerSettingStore` interface (`Get`/`Put` with CAS — `expectedRevision 0` = create-only,
  `store.ErrRevisionConflict` on mismatch — and `Delete`), and the `entadapter.BrokerSettingStore`
  implementation. `DeleteRuntimeBroker` now explicitly deletes the settings row (no FK edge exists
  between `runtime_brokers` and `broker_settings` by design, so heartbeats never contend with
  settings writes).
- **Key registry** (`pkg/hub/brokersettings`): `maxAgents` is the first and only P2.1 key —
  `>= 0` (0 = unlimited), declares `quota.update` as its write permission. Unknown keys in a PUT
  are rejected with 400 via `brokersettings.Lookup`.
- **HTTP API**: `GET`/`PUT /api/v1/runtime-brokers/{id}/settings`, dispatched from the existing
  `/api/v1/runtime-brokers/` mux pattern (string-split subpath routing, same shape as the existing
  `env`/`secrets` subresources) rather than a new registered route — this meant no new
  `routeMetadataTable` entry was needed, only extending the existing `broker.read` and
  `quota.update` authzop catalog entries with a new `EntryPoint` each (`.design/authorization-operation-catalog.md`
  regenerated via `go test ./pkg/hub/authzop/...`). GET requires `broker.read` on the broker
  resource (so an owner can always see their own broker's settings, even read-only); PUT requires
  `broker.read` as a precondition, plus a stale-revision check *before* authorization (a caller's
  `expectedRevision` must match the freshly-read current revision, or 409), plus the declared
  permission of every key whose value actually *changes* between the stored document and the new
  one — not just the keys present in the request, since PUT is a full replace and an omitted or
  `null` key clears it. For `maxAgents` that's `quota.update` on `Resource{quota, hub}`, hub-admin
  only, matching P2-D3. A no-op write (nothing changes) skips the store entirely: no revision bump,
  no `updatedBy` rewrite, no audit event, no row created for a broker that never had one.
- **One read model, shared by enforcement and every read path** (design.md §5.9, AC-P2-10):
  `effectiveBrokerLimit`/`brokerCapacity` (`pkg/hub/broker_capacity.go`) implement the P2-D2
  precedence — broker setting > entitlement binding > hub-wide default. `QuotaService` gained a
  minimal `limitOverride` hook (`quota.go`), wired in `server.go` to `brokerSettingLimitOverride`,
  consulted by `Reserve` before `ResolveEffectiveLimit`; the existing providers-listing helper
  `resolveBrokerCapacity` (from PR ptone/scion#2168) became a thin wrapper that also surfaces an
  additive, `omitempty` `agentLimitSource` field, per small-issues-lead-2's agreement recorded in
  design.md §5.9. `ResolveEffectiveLimit` itself is unchanged for existing callers; its logic was
  split into an internal `resolveEffectiveLimitWithSource` so the new code can tell "resolved from
  an entitlement binding" from "resolved from the hub-wide default" without duplicating the merge
  rule.
- **Web**: hand-written `BrokerSettings`/`BrokerSettingsResponse`/`EffectiveSetting` TypeScript types
  in `web/src/shared/types.ts` mirroring the Go JSON tags exactly (no generator exists — this is the
  one place JSON-tag drift has to be caught by review). `broker-detail.ts` gained a "Settings" card:
  "Use hub default (N)" / "Custom" radio (0 = unlimited), a live usage line (counted agents +
  effective source), Save gated on `_capabilities.update`, and a 409 handler that reloads and shows
  "changed by someone else". Did not touch the unrelated `createdAt` drift noted in findings.

## Surprises / notes for review

- **`brokerCapacity` looked unused at first.** `effectiveBrokerLimit` alone was enough to satisfy
  every P2.1 call site (Reserve's override hook, the providers listing, and the settings response),
  so the `BrokerCapacity{Limit, Count, Source}` struct and its constructor were briefly dead code —
  `golangci-lint` caught this immediately (`unused`). Fixed by routing the settings GET/PUT response
  through `brokerCapacity` instead of `effectiveBrokerLimit` directly, which is what design.md §5.9
  asks for anyway ("the broker settings GET returns `effective` built from `brokerCapacity`") and
  incidentally means a resolution failure now degrades the settings response the same way it already
  degrades the providers listing (value left unset, not a 500), rather than introducing a second,
  stricter failure convention for the same underlying computation.
- **Two different "unlimited" conventions had to be reconciled deliberately.** `BrokerCapacity.Limit`
  is `nil` for unlimited (the existing providers-listing convention: omit the JSON key entirely). The
  broker-settings API instead always shows a concrete number (`0` for unlimited) per design.md §5.4's
  illustrative response. `buildBrokerSettingsResponse` converts explicitly at the boundary rather
  than picking one convention and forcing the other caller to match it.
- **Heartbeat can't be exercised through HTTP as a "dev" user token.** `handleBrokerHeartbeat`'s
  user-identity path checks `Resource{Type: "runtime_broker", ...}`, a resource-type string that has
  no matching entry in the permissions registry (only `Resource{Type: "broker", ...}` does, via
  `brokerResource()`) — so even the super-admin dev token gets a real 403 there. This looks like a
  pre-existing inconsistency unrelated to this change (heartbeats work fine in production via the
  broker's own HMAC identity, which bypasses the permission check entirely). Rather than route around
  it with an HMAC test fixture, `TestBrokerSettings_HeartbeatLeavesSettingsUnchanged` calls
  `store.UpdateRuntimeBrokerHeartbeat` directly — the property under test (a `runtime_brokers` write
  never touches the separate `broker_settings` row) is a store-layer property by construction, since
  the two live in different tables. Flagged to the EM/reviewers rather than silently worked around.
- **P1b (the enforcement switch, PR ptone/scion#2270) is not wired.** Per the brief, `quota.go`
  changes are kept minimal and localized (the `limitOverride` hook is additive, no existing behavior
  changed) specifically so that rebase is a small diff: add one more precedence check ahead of the
  broker-settings override, and one more `BrokerLimitSource*` value (`not_enforced`, already declared
  as a constant in `broker_capacity.go` with a comment marking it unused until then).

## Verification

- `go build ./...` — clean.
- `go vet ./...` — clean.
- `golangci-lint run ./pkg/hub/... ./pkg/store/...` — 12 pre-existing findings, all in files this PR
  does not touch (verified via `git diff --name-only`); zero findings in any file this PR adds or
  changes.
- `make test-hub-sqlite` — full pass (`pkg/hub`, `pkg/hub/auth`, `pkg/hub/authzop`, `pkg/hub/githubapp`,
  `pkg/hub/imagecheck` all `ok`), ~932s.
- `make test-fast` — `pkg/hub`, `pkg/store`, `pkg/store/entadapter`, `pkg/ent` all `ok`. Pre-existing
  failures in `cmd`, `pkg/agent`, `pkg/config`, `pkg/harness`, `pkg/runtime`, `pkg/runtimebroker`,
  `pkg/sciontool/supervisor` — none touched by this PR, consistent with sandbox/environment
  limitations for those subprocess/provisioning-heavy suites rather than a regression here.
- **Postgres**: installed PostgreSQL 15 locally (`apt-get install postgresql`) since no external
  instance was provided, started it, and ran the `entadapter` package's `-tags integration` suite
  against it end to end (`SCION_TEST_POSTGRES_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable
  go test -tags integration -timeout 30m ./pkg/store/entadapter/...`), which provisions a fresh
  ephemeral database and a schema-per-test — this is the same path `pkg/store/enttest` documents for
  CI-style Postgres verification. All `BrokerSettingStore`/`DeleteRuntimeBroker` tests
  (`TestGetBrokerSettings_*`, `TestPutBrokerSettings_*`, `TestDeleteBrokerSettings_*`,
  `TestDeleteRuntimeBroker_*`) pass against real Postgres, exercising the `SELECT ... FOR UPDATE` CAS
  path that SQLite's single-writer lock never touches. The full `entadapter` package run has 4
  failures — `TestBackfillAgentIdentityKeys_{EarlierSlugBeatsLaterDisplayKey,LaterSlugSurvivesEarlierDisplayKey,DisplayVsDisplayKeepsFirst}`
  and `TestUpsertConversationByExternalRef_FieldClassification` — confirmed (by an independent
  reviewer, running in parallel against the same server) to fail identically on the unmodified base
  `e8013da1`; they are pre-existing and unrelated to this change.
- `npx tsc --noEmit` (web/) — clean.
- `hack/check-authz-guards.sh` — no violations.
- `go test ./pkg/hub/authzop/...` — regenerated and validated `.design/authorization-operation-catalog.md`.

## Deliverables

- PR ptone/scion#2275 against `ptone/scion` `main` from `scion/broker-settings-p2-1`, marked ready
  for review. PR ptone/scion#2168 (GoogleCloudPlatform/scion#2097) squash-merged upstream and this
  branch is rebased onto `upstream-main` on top of it.
- This log entry.
- `scion message` to `broker-settings-em` with PR number, head SHA, and the test evidence above.

## Upstream review: GoogleCloudPlatform/scion#2126

`ptone/scion#2275` is tracked upstream as `GoogleCloudPlatform/scion#2126`. gemini-code-assist
opened two review threads there, both on `pkg/store/entadapter/brokersetting_store.go`'s
`usesRowLocks`:

- discussion_r4144103907 (line 38)
- discussion_r4144103949 (line 55)

**Claim.** The old implementation cached the dialect with a `sync.Once`:

```go
func (s *BrokerSettingStore) usesRowLocks(ctx context.Context) bool {
	s.dialectOnce.Do(func() {
		_, _ = s.client.BrokerSetting.Query().
			Where(func(sel *entsql.Selector) { s.dialectName = sel.Dialect() }).
			Exist(ctx)
	})
	return s.dialectName == dialect.Postgres
}
```

Gemini's claim: if the first probe fails (a transient DB error, or a cancelled `ctx`), `sync.Once`
permanently caches an empty `dialectName`, so `usesRowLocks` reports `false` forever afterwards —
on Postgres, that means `PutBrokerSettings` silently stops taking `ForUpdate()` (a lost row lock,
i.e. a correctness bug on the CAS path), not just a retriable failure.

**Evaluation (done before writing any fix, per the brief).** Traced the actual call path `Exist`
takes in ent v0.14.5 with this repo's generated code, rather than assuming: `Exist(ctx)` →
`FirstID(ctx)` → `Limit(1).IDs` (`pkg/ent/brokersetting_query.go:88,184`) → `Select(FieldID).Scan` →
`prepareQuery` → `scanWithInterceptors` → `BrokerSettingSelect.sqlScan` → `root.sqlQuery(ctx)`, where
the predicates run (`for _, p := range _q.predicates { p(selector) }`) → `driver.Query(...)`. The
predicate `Where(...)` callback that captures `sel.Dialect()` is applied inside `sqlQuery`, which
builds the SQL selector as a pure in-memory string-building step — no I/O — and this always
completes *before* `driver.Query` (the actual network round trip) is called. So:

- A **transient DB error** can only surface from `driver.Query`, which runs strictly after the
  predicate already set `s.dialectName`. It cannot prevent the capture.
- A **cancelled ctx** is likewise only observed by the driver during the round trip; nothing in the
  selector-building step inspects `ctx` in this codepath.
- The only earlier exit is `prepareQuery`, and it cannot fail here: there are no interceptors, no
  traversal path, and the field names are valid — neither of which is a "transient DB error" or
  "cancelled ctx" in Gemini's framing. Also checked: no ent interceptors are registered on any
  client anywhere in this codebase.

(Note: `sqlAll`/`sqlgraph.QueryNodes` are the path `All`/`Only` take, not `Exist`/`IDs` — an earlier
draft of this section named that path incorrectly; corrected in review round 6, F1/F2.)

**Conclusion: the claim's two named triggers (transient DB error, cancelled ctx) do not reach the
failure mode described.** The `sync.Once` was not, in fact, capable of caching an empty
`dialectName` from either of those causes in this codebase's actual call path. That said, the
probe-query machinery was doing real work (a throwaway `Exist` query per process lifetime) for a
value that never needs a query at all.

**Chosen fix: construction-time dialect, no query.** `client.Driver().Dialect()` is a property of
the already-constructed `*ent.Client`, fixed at startup — never a query result, so there is no probe
to fail and nothing to cache. This is exactly the "preferred" option in the brief, and it isn't a
new pattern: `CompositeStore.isPostgres()` (`locking.go`) already does the same read, and
`role_store.go`, `external_store.go`, `skill_registry_store.go`, and `project_store.go` all compare
`client.Driver().Dialect() == dialect.Postgres` directly. `usesRowLocks` now does the same:

```go
func (s *BrokerSettingStore) usesRowLocks(context.Context) bool {
	return s.client.Driver().Dialect() == dialect.Postgres
}
```

The `sync.Once`/`dialectName` fields and the `sync`/`entsql` imports were removed; the method still
takes a `context.Context` parameter (unused) so both call sites in `PutBrokerSettings` are
unchanged.

**Why not the fallback (cache-only-on-success, or a mutex+retry)?** The brief's own fallback branch
only applies "if a query probe must remain." Since a query-free read is both possible and already
the established local idiom, keeping any probe at all (Gemini's suggested `sync.Mutex` + retry, or
a success-only cache) would be strictly more complex for no benefit, so neither was implemented.

**Scope check for other stores.** `git diff --name-only $(git merge-base HEAD upstream-main)...HEAD`
shows only `brokersetting_store.go` (among files using the `dialectOnce`/cached-probe pattern) was
added or changed by P2.1. `access_constraint_store.go`, `lifecyclehook_store.go`, `launch_store.go`,
`agent_store.go`, and `hubsetting_store.go` all use the same older probe pattern, but they predate
this PR and are untouched by it (confirmed via the diff), so per the brief's own scoping — "every
other store or helper added or changed in P2.1" — they are out of scope here and were left as-is.
(`launch_store.go`'s `dialect(ctx)` helper even documents, in its own comment, that it deliberately
*reuses* `AgentStore`'s cached probe so the launch store and the row-lock gating share one cache
instead of two — a cross-method sharing reason that doesn't apply to `BrokerSettingStore`, which has
no sibling method to share a cache with.)

**Tests.** Added `TestUsesRowLocks_ReflectsBackend` to
`pkg/store/entadapter/brokersetting_store_test.go`: it asserts `usesRowLocks` against the actual
active test backend (`enttest.Active()`) rather than re-deriving the expected value from the same
`client.Driver().Dialect()` expression the implementation uses, so a wrong dialect-constant
comparison in the implementation would still be caught. Ran green on both:

- SQLite (default `go test ./pkg/store/entadapter/...`): `usesRowLocks` → `false`.
- Postgres (`-tags integration` with `SCION_TEST_POSTGRES_URL` pointed at the local Postgres 15
  instance): `usesRowLocks` → `true`.

All 12 pre-existing `BrokerSetting`/`DeleteRuntimeBroker` CAS tests remain green, unchanged, on both
backends (13 total including the new test).

**Fixing commit:** `store(brokersettings): read dialect from the driver, not a probe query`.

**Gates re-run after the fix:** `go build ./...`; `go vet ./pkg/store/... ./pkg/hub/`, plus
`-tags no_sqlite`; targeted `go test ./pkg/store/... ./pkg/hub/... -run 'BrokerSetting|Broker|Quota|Capacity'`;
`make test-hub-sqlite`; Postgres `entadapter` suite (`-tags integration`); `golangci-lint run
--new-from-rev=upstream-main ./pkg/store/entadapter/...` (0 new issues; the 3 pre-existing
`staticcheck` findings are unrelated deprecated-API usage in `access_constraint_store.go`, untouched
by this PR). All `SCION_*`/`CLAUDE_*` environment variables were unset for every gate invocation.
See the response note for the exact commands and full output:
`/scion-volumes/scratchpad/projects/broker-settings/notes/p2-1-upstream-2126-fix.md`.

### Round 6 (text-only REQUEST CHANGES)

Review: `/scion-volumes/scratchpad/projects/broker-settings/reviews/broker-settings-rev-p2-1-6.md`.
Verdict: REQUEST CHANGES, code logic approved — findings were all in the written reasoning, not the
fix itself.

- **F1/F2 (Required): wrong call chain.** The doc comment on `usesRowLocks`, the stale
  dialect-probe-deadlock comment above the `PutBrokerSettings` call site, this log's evaluation
  section (above), and the notes/draft-reply file all cited `Exist → FirstID → sqlAll →
  sqlgraph.QueryNodes`. That path belongs to `All`/`Only`, not `Exist`/`IDs` — `sqlAll` is never
  called for `Exist`. The actual path for `Exist` in this repo's generated code is `Exist → FirstID
  → Limit(1).IDs → Select(FieldID).Scan → prepareQuery → BrokerSettingSelect.sqlScan →
  root.sqlQuery(ctx)` (predicates applied here) `→ driver.Query(...)`. The conclusion was unaffected
  (the predicate still runs during in-memory selector construction, before `driver.Query`), but the
  chain itself was wrong everywhere it appeared. Fixed by: trimming the `usesRowLocks` doc comment
  to state only the conclusion (no narrated call chain at all, since it isn't needed to justify the
  code); rewording the stale deadlock comment (there is no probe query left to deadlock); correcting
  the chain in this log's evaluation section above; and correcting the chain in the notes file and
  its draft reply. Text-only changes — no logic touched.
- **F5: draft reply must not imply CI covers Postgres.** Reworded to say the Postgres branch of
  `TestUsesRowLocks_ReflectsBackend` was verified locally against a real Postgres 15 instance, and
  that this repo's CI Postgres job ("T1 Launch Store PostgreSQL Tests") does not currently run the
  entadapter `BrokerSetting`/`DeleteRuntimeBroker`/`UsesRowLocks` tests (F3).
- **F3 (Optional follow-up, widen the Postgres CI job's test regex to cover
  `TestUsesRowLocks_|TestPutBrokerSettings_|TestDeleteBrokerSettings`): declined for this PR.** The
  EM's call: the shared `Makefile`/CI target ("T1 Launch Store PostgreSQL Tests") is shared
  infrastructure outside P2.1's scope, and changing its test selection affects every PR that runs
  that job, not just this one. Routed to the lead as a follow-up rather than changed here.
- **F4 (FYI, no action):** `usesRowLocks(context.Context)` keeps an unused `ctx` parameter — correct
  as-is, kept for signature parity with sibling stores' row-lock helpers.

**Fixing commit:** `store(brokersettings): correct the usesRowLocks call-chain description (review round 6, F1/F2/F5)`.
