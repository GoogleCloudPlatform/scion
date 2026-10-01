# P1b: project agents endpoint sorted mode (ptone/scion#2383)

Branch: `perf/2383-sorted-cursors`, commit 1, based on a fresh `origin/main` (fc30d18,
"cli: scion list attribute filters and relationship flags (#2153)"). Design:
gs://scion-xproject-exchange/slow-list/design/lists-graph.md (r8 final).

Scope, per the P1b brief: the **project** agents endpoint only —
`sort=updated` (both directions), `fit` with candidate-count completeness,
`stats=1` (Phase="" population), the COUNT-first candidate ceiling
(`authorizedListMaxCandidates` = 2000, reused unchanged from
`pkg/hub/authorized_list.go`), the v2 cursor decoded before any SQL, the new
project cursor binding on the full filter (legacy mode gains the same
`validateAuthorizedListCursor` call), 400s, and the sort echo. `sort=created`,
the global endpoint's sorted mode, the agent-JWT sorted read path, and
`view=compact` are later phases (P2/P3); an agent-JWT request carrying `sort`
gets a 400 here, before any SQL, per the design's explicit P1b carve-out.

## What changed

- `pkg/store/store.go`: `AgentMember` (the narrow projection over exactly the
  fields `agentResource` reads, plus positioning/stats fields) and its
  `ToAgent()` — the single construction path the step-5a race comparison
  uses. `CountAgents` and `ListAgentMembers` added to the `Store` interface.
- `pkg/store/agent_cursor.go`: the v2 cursor codec
  (`EncodeAgentCursor`/`DecodeAgentCursor`), independent of the legacy
  `message_store.go` codec, wrapping `ErrInvalidInput` on every rejection.
- `pkg/store/agentsort/` (new package): the single reference implementation
  of the section-4.2 total order (`KeyFor`, `Less`, `Compare`, `SortRows`),
  used by both the production positioning code and the test suites, so there
  is exactly one place that can get a tie-break wrong.
- `pkg/store/entadapter/agent_store.go`: `CountAgents` (the existing COUNT
  predicate, exposed standalone) and `ListAgentMembers` (fetches up to `max`
  filtered rows and sorts them in Go via `agentsort`, rather than asking each
  SQL dialect to reproduce the `COALESCE(last_activity_event, updated)`
  ordering — see Deviations).
- `pkg/hub/capabilities.go`: `ComputeCapabilitiesForActions` (additive; the
  thin ActionRead-only / explicit-action-list read-pass variant per design
  Q-B) and `mergeCapabilities` (recombines a read-pass result and a
  remaining-actions result in `ResourceActions` order). Neither touches
  `ComputeCapabilitiesBatch`'s existing body, to keep the diff in this
  file — shared with #2376/#2377's `withAuthzInputMemo` install site, not yet
  landed on `origin/main` — minimal.
- `pkg/hub/agent_sorted_project_list.go` (new): the sorted-mode project
  handler (`listProjectAgentsSorted`) implementing design 5.3 steps 0–7,
  including the step-5a race rule (missing row / project mismatch / filter
  mismatch / re-decision) and its exact decision-cost accounting (r8 NB-1).
- `pkg/hub/handlers_projects_core.go`: `listProjectAgents` now dispatches to
  sorted mode when `sort` is present; the agent-JWT 400 gate runs before any
  SQL; legacy mode gains the new project cursor binding
  (`scopedCursorBinding` + `validateAuthorizedListCursor`), matching design
  4.4's "new in both modes".
- `pkg/hub/handlers_agents_core.go`: `ListAgentsResponse` gains `Sort`, `Dir`,
  `Complete *bool`, `Stats *ListAgentsStats` (all `omitempty`/conditionally
  populated per design 4.6); this struct is shared with the global `listAgents`
  handler (untouched behaviorally — the new fields are simply never set there
  yet).

## Design-bullet to file:function map

See gs://scion-xproject-exchange/slow-list/reports/lists-p1b-dev.md for the
full per-bullet, per-test mapping and measured decision counts; this log
entry is the summary for the project log.

## Deviations from the design's literal text (and why)

1. **`ListAgentMembers` sorts in Go, not via a SQL `ORDER BY`.** The
   candidate set is already bounded to ≤ 2001 rows by the ceiling check, so
   an in-memory sort is cheap, and reusing `agentsort` (the same comparator
   the tests check pages against) removes any chance of the SQL and the Go
   reference disagreeing on a tie. The design's `agentOrder(sort,dir)`
   SQL-level order-by is deferred to the global endpoint (P2), where true
   keyset pagination needs it.
2. **`ListAgentMembers` decodes full rows internally and projects down**,
   rather than a narrower `SELECT`. This costs one JSON decode per candidate
   row that a column-level projection would avoid, but it trivially
   guarantees `AgentMember.ToAgent()` reconstructs a `Resource` identical to
   `agentResource(fullRow)` — the property the non-waivable S6 equality gate
   exists to prove. Narrowing the actual `SELECT` is a follow-up once it is
   covered by a fixture that also exercises the column list.
3. **Test data volume.** The design's test plan asks for S1/S6 fixtures at
   n ∈ {25, 100, 500, 501, 1200}. This sandbox's SQLite test harness inserts
   agents one row at a time under `MaxOpenConns: 1`, and is heavily
   CPU-throttled (a cold `go build ./...` of this repo took on the order of
   40 minutes of wall clock here). S1's order-parity and S6/S9's
   decision-count and ceiling tests are implemented and pass, but at n on the
   order of 5–12 for real-row cases, and the ceiling/race cases use a
   store-decorator that fakes `CountAgents`/`ListAgentMembers`/`GetAgentsByIDs`
   results instead of materializing 2001 real rows. The formulas being
   verified (`5+8n`, `5+n+7P`, the ceiling's exactly-one-decision bound, the
   race's net-+1-per-item bound) are linear in n and do not depend on n's
   magnitude, so this proves the same logic the design's larger fixtures
   would; it does not prove SQLite/Postgres performance at scale, which is
   #2392's territory. Flagged to slow-list-lists-em for a call on whether a
   larger-N run should be done on faster infrastructure before merge.
4. **Non-UTC `time.Time` (design R1)** is proven at the `agentsort`
   pure-function level (`TestSortRows_NonUTC`) rather than round-tripped
   through the SQLite store: writing a non-UTC `time.Time` through this
   store's ent/SQLite adapter hits a pre-existing, unrelated scan error
   ("unsupported Scan ... storing driver.Value type string into type
   *time.Time") that is not specific to P1b's new code — every time column
   in this adapter would hit the same issue. Not fixed here (out of scope);
   noted for whoever owns that adapter's time handling.
5. **Found and fixed during testing:** the first implementation set
   `totalCount` for a complete response before applying the step-5a race
   rule, so a raced-and-dropped item stayed counted. Design 5.3's closing
   text ("a complete response simply has fewer items (totalCount =
   len(page))") requires the post-5a count; fixed, and covered by
   `TestListProjectAgentsSorted_Race_MissingRow`.

## Review round 1 (slow-list-lists-em) — addressed

- **D2 (required):** `ListAgentMembers` initially decoded full rows and
  projected down in Go, rather than a genuine SQL `SELECT`. Reviewer called
  this out as defeating the point for the server's `WriteTimeout`. Fixed:
  it now selects exactly `agentMemberSelectFields` (the `agentResource`
  inputs plus positioning/stats fields) via ent's `.Select()`, converted by
  a new `entAgentToMember`.
- **D3 (hard gate, required):** the first pass tested S6/S9 at small N
  (1–12), reasoning that this sandbox's SQLite inserts were too slow for the
  design's own sizes (25–1200, plus 2000/2001). That reasoning was wrong —
  the slowness was `go build`'s cold `GOCACHE` compile time, not insert
  throughput. `store.Store.WithTx`-wrapped bulk fixture creation (1200 rows
  in ~0.6s) made the real sizes practical, and all of S6's sizes (25, 100,
  500, 501, 1200) plus the two R<n sub-cases (n=1200/R=400 paged=1380;
  n=500/R=200 complete=1905) and S9's real 2000/2001-row ceiling cases are
  now covered. The R<n fixture uses a project-scoped role granting only
  `agent.list` (not `agent.read`) combined with per-agent ownership — an
  unconditional relationship grant independent of role bindings — to split
  readability within one project for one caller without an access
  constraint.
- **D4:** reviewer asked for a 20-minute repro of the non-UTC `time.Time`
  scan error through the *normal* store API (not a raw ent bypass). Done:
  overriding `time.Local` and calling plain `CreateAgent`/`GetAgent`
  reproduces the identical scan error. This is a real, pre-existing,
  store-wide bug (any write while the process's local timezone isn't UTC
  breaks the next read of that row) — not fixed here, flagged as a
  follow-up for whoever owns `pkg/store/entadapter`'s time handling.
- **D6 (required):** added a synthetic-decorator test for the reverse key
  crossing (a row's key regressing below the cursor, causing it to resurface
  on a later page) — the mirror case of the forward "skip" crossing, which
  real write paths cannot produce (every write stamps a monotonically
  non-decreasing `time.Now()`).
- **D7:** checked whether CI runs these tests against Postgres. It does not:
  CI's only Postgres job scopes `pkg/store/entadapter` to a `-run` pattern
  (`^(TestLaunchStore_|TestReaper_|TestReport_H1_)`) that excludes the new
  P1b tests. Flagged to slow-list-lists-em as a CI-scoping gap; the test
  code itself is dialect-portable (`enttest.NewClient`) with no changes
  needed if that job's scope is ever widened.
- **D1, D5:** accepted as-is (P2 must add the global endpoint's SQL
  ordering).

New head after this round: `f74135786f75988d1209a42e50701cf4e4113851`.

## Verification

- `go build ./...`: pass.
- `go vet ./pkg/store/... ./pkg/hub/...`: pass.
- `go test -p 2 ./pkg/store/... ./pkg/hubclient/...`: pass.
- `go test -p 2 ./pkg/hub/...`: the full package exceeds this sandbox's
  default 10-minute test timeout (confirmed unrelated to this change — the
  timeout lands mid-run in a pre-existing, unmodified authz characterization
  test, `TestSystemAuthorityProof_GroupAndGCPServiceAccount_PerPermissionCharacterization`).
  Re-run with `-timeout 40m`; see the dev report for the final result.
- New tests (21 in `pkg/hub`, 9 in `pkg/store`, 5 in `pkg/store/entadapter`,
  8 in `pkg/store/agentsort`) all pass; see the dev report for the S-test
  mapping.

Full commands, results, and the S-test/design-bullet mapping:
gs://scion-xproject-exchange/slow-list/reports/lists-p1b-dev.md.
