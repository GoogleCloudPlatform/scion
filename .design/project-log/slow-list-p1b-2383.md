# P1b: project agents endpoint sorted mode (ptone/scion#2383)

Branch: `perf/2383-sorted-cursors`, commit 1, based on a fresh `origin/main` (fc30d18,
"cli: scion list attribute filters and relationship flags (#2153)"). Design:
the lists-graph design doc (r8 final).

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

See the P1b dev report for the
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

## Formal round-1 review (slow-list-lists-rev-p1b-1): REQUEST CHANGES, now closed

8 blocking, 7 non-blocking, 2 nit. The two hard gates S6 and A15 both failed
as submitted:

- **S6 failed**: `resourceEqual` was a hand field list (already silently
  missing `Resource.ScopeUserID`), not the whole-Resource compare the design
  and security sign-off require, and the "reflection-filled" equality test
  was neither reflection-filled nor able to fail. Fixed: `resourceEqual` is
  now `reflect.DeepEqual` over normalized copies, with a mutation test that
  reflection-fills a `Resource` and mutates every field; the equality gate
  itself is now a reflection-filled `store.Agent` written and read back
  through the real `ListAgentMembers`/`GetAgentsByIDs`. Re-ran the
  reviewer's exact `ScopeUserID: a.Slug` mutation after the fix and
  confirmed the test now fails with the predicted diff, then reverted
  cleanly (`git diff` empty).
- **A15 failed**: sorted mode never clamped `limit`, so `limit=700` over 700
  agents cost 5,605 decisions with no race at all. Fixed: `limit` is clamped
  to 500 before the `fit>=limit` check, with a regression test pinning the
  exact reproduction case to 500 items / 4,205 decisions.

Also fixed: `includeDeleted=true` silently dropped soft-deleted agents in
sorted mode (full rows are now loaded via `ListAgents`, which honors it,
instead of `GetAgentsByIDs`, which doesn't); the step-5a filter re-check was
a hand duplicate missing `HarnessConfig` (replaced with a store-driven
recheck using the store's own predicate); plus the full B6/B7/B8/N1-N7/nit
list — new tests for the OwnerID-change race, exact race-drop decision
counts, a short-page-continues case, an end-to-end nil/empty-Labels
zero-redecision count, four cursor-rejection gaps, an HTTP page walk at
n=1,200 with a committed `agentsort` golden fixture, two more S9 real-row
cases, an S10 R<=fit case, a bumped CLI-walk size, a store-level fail-closed
check on sort/dir, and an S2 byte-identity test for the EM's N6 ruling.

One deviation disclosed, not silently dropped: B6(b) (scoped-UAT/hub-admin/
super-admin identity classes through `ComputeCapabilitiesForActions`) was
not added — that code path is identical to what `ComputeCapabilitiesBatch`
already uses and has its own coverage in the general authz suites.

Rebasing onto `origin/main` for this round hit one real conflict: another
branch had independently widened the same `test-launch-store-postgres`
Makefile `-run` line (adding broker-settings test names) in parallel with
this branch's own D7 widening. Merged both sets additively and verified
with `go test -tags integration -list`.

New head after this round: `c661c2d21fdeddc7c6ac30ae1242f9195ec2a0c9`
(force-pushed after the rebase; the branch is exclusively owned by this
task, so this follows normal feature-branch-rebase practice, not a shared
ref).

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

Full commands, results, and the S-test/design-bullet mapping: see the P1b
dev report.

## B6(b) resolution (round-1 follow-up)

The EM rejected the B6(b) deviation above: `ComputeCapabilitiesForActions` is
a separate copy of `ComputeCapabilitiesBatch`'s loop, not shared code, so its
`DecideFromContext` branch needed its own end-to-end identity-class proof,
not just a pointer to pre-existing coverage of the batch path.

Added `pkg/hub/agent_sorted_project_list_identity_classes_test.go` (test-only
commit), four tests, each minting the identity via the real token/role-
binding path:

- scoped UAT (`agent:manage`) — the identity class that actually exercises
  `DecideFromContext` instead of `CheckAccess`.
- super-admin, not a project member.
- hub-admin, not a project member — documents a real finding: hub-admin's
  curated permission set does not include `agent.list`/`agent.read` (those
  are project-scoped), so this case is a 403 at the gate (1 decision), not a
  200. The literal brief wording ("hub admin (non-member)") turns out to
  describe a denial test, not a capability-loop test.
- hub-admin combined with ordinary project membership — the shape in which
  hub-admin actually reaches the per-item capability loop.

Each non-denial case asserts the sorted page's merged `_capabilities`
deep-equal the legacy `ComputeCapabilitiesBatch` output (action order
included) and that the decision/audit count matches `5+8n` exactly. All four
pass; `go vet ./pkg/hub/...` clean. B6 is now fully closed, no open
deviation.

New head after this round: `e9f9585f2` (fast-forward push, no rebase
needed). Round-2 review is picking this up.

## Round 2 review response

Verdict: REQUEST CHANGES (1 blocking, 5 non-blocking, 1 nit). S6, S9, A3
passed; A15 failed.

**B-1 (blocking, A15 FAIL):** the round-1 B3 fix (clamp `limit` to 500) kept
a single request's page from growing unbounded, but at limit=500 a paged
request still costs `5+n+7*limit` decisions -- over the 4,005 A15 ceiling
for any n > 500 with no race at all (4,205 at n=700, the round-1 regression
test's own pinned value; 5,505 at n=2,000, the reviewer's probe). The design
author ruled **erratum E2** (lists-graph-errata.md): bound the paged
branch's actual page size to `P_eff = min(limit, floor((4000-n)/7))`, where
`n` is the step-1 binding count (`len(members)`), not the step-0 COUNT.
`P_eff` deliberately is not part of the cursor binding, so `n` can differ
page to page without invalidating a cursor; the complete branch is
unchanged (ignores `limit`, costs `5+n+7R`). Implemented as
`effectivePagedPageSize`. The old `TestListProjectAgentsSorted_LimitClampedTo500`
(which pinned the now-known-wrong 4,205) is replaced with three tests
matching erratum E2's own list: an exact page-size/decision-count table at
the design's n values (500, 501, 700, 1200, 2000; all ≤ 4,005), a limit=500
walk at n=2,000 with R=n and R=400 (every readable agent returned exactly
once despite P_eff < limit), and an all-page-items-raced variant (n=501,
4,498 decisions, inside the 4,504 raced exception).

**N-1..N-5 and nit-1 (non-blocking, all closed, per CLAUDE.md "non-blocking
does not mean optional"):**
- N-1: `countingAgentStore`'s `getByIDsCalls` assertion was vacuous after
  round 1's B4 fix moved the full-row read off `GetAgentsByIDs`. Added
  `ListAgents` call/IDs tracking and a real assertion (exactly one call per
  request, IDs exactly the page).
- N-2: the S6 fixture's skip list was missing `HarnessConfig` (same
  "enriched, not persisted" field class as the three it did list). Added it,
  plus `assertNonSkippedFieldsNonZero` so the fixture proves its own
  coverage rather than trusting a comment.
- N-3: extended the E1 byte-identity test to invalid values, a cursor
  already in play (raw-byte comparison including the emitted
  nextCursor/binding), and the global endpoint.
- N-4: made the nil-vs-empty end-to-end test table-driven over
  {Labels, Ancestry} x {nil->empty, empty->nil}, all 4 cases at exactly 13
  decisions.
- N-5: added a walk using a non-owner member identity with a strict
  readable subset (R=15/n=40), agents across 3 projects (cross-project-leak
  noise), mixed phases, an empty-value label, checked against a reference
  order built independently of `ListAgentMembers` (agentsort.Less/SortRows
  over `GetAgentsByIDs`-fetched rows).
- nit-1: fixed together with B-1 (the clamp comment's A15 claim was wrong;
  now points to `effectivePagedPageSize`/erratum E2).

Verification: `go build ./...` pass; `go vet ./pkg/hub/... ./pkg/store/...`
pass; `go test -p 2 -count=1 ./pkg/hub/... -run
'TestListProjectAgentsSorted|TestListProjectAgentsLegacy|TestResourceEqual|TestMergeCapabilities'
-v` -- 62 subtests, 0 failures, including every new/changed test this round.

New head after this round: `b4dedcc7e487cf2a6ace65d63c0cca88fb4dfa41`
(fast-forward push, no rebase needed). Full disposition table and test
output: see the P1b dev report.

## Rebase onto origin/main 224eb0328

`origin/main` moved 10 commits past this branch's base (`8429bb7e` ->
`224eb0328`) during round 2; the EM flagged overlap in
`handlers_agents_core.go`, `models.go` and `server.go` and asked for a
rebase before round 3. `git rebase origin/main` replayed all 10 commits
cleanly with **zero conflicts**: the only new-range commit touching those
files (`224eb0328`, GCP passthrough runtime-awareness) is confined to
`createAgentInProject`'s GCP-passthrough branch, `GCPIdentityConfig`/
`RuntimeBroker.DefaultProfile`, and `RemoteGCPIdentityConfig` -- nowhere
near this branch's own edits to those files (`ListAgentsResponse` gaining
`Sort`/`Dir`/`Complete`/`Stats`; no `RuntimeBroker`/`GCPIdentityConfig`
changes at all here).

`go build ./...` and `go vet ./pkg/hub/... ./pkg/store/...` both pass.
Force-pushed with `--force-with-lease`. Targeted re-run:
`go test -p 2 -count=1 ./pkg/hub/... -run
'TestListProjectAgentsSorted|TestListProjectAgentsLegacy|TestResourceEqual|TestMergeCapabilities'`
(62 subtests) plus `go test -p 2 -count=1 ./pkg/store/... -run
'TestCountAgents_|TestListAgentMembers_'` (5 subtests) -- 0 failures, exit 0
on both.

New head: `8edbf3f58b12f9ac216a928b2e77658053e92daf`.

## Round 3 review response

Verdict: **APPROVE** (0 blocking, 1 non-blocking, 3 nits). S6, S9, A3, A15
all PASS -- the reviewer independently re-measured decision counts (4,000-
4,002 unraced, 4,285-4,496 raced, all inside A15) and confirmed both prior
rebases were clean replays via `git range-diff`.

All four remaining findings closed, test-only, in `5799d5ff8`:

- **N-1**: the E2 page-size tests computed their expected `P_eff` by calling
  `effectivePagedPageSize` -- the function under test -- so the reviewer's
  `/7`->`/8` mutation passed both tests anyway. Hard-coded erratum E2's
  literal table (`{500:500, 501:499, 700:471, 1200:400, 2000:285}`) as the
  actual expected values, with a separate sanity check still
  cross-referencing the live function (so a real future formula/ceiling
  change is still caught, just not conflated with the test's own
  correctness). Replayed the reviewer's exact mutation myself: all 5
  `BoundedByN_DesignSizes` subtests and `PagedRaced_E2` now fail (caught in
  0.00s by the sanity check, before any HTTP request); reverted; `git
  status` clean; re-confirmed both pass again.
- **nit-1**: deleted the orphan doc comment for the already-removed
  `LimitClampedTo500` test, folded into `BoundedByN_DesignSizes`'s own
  comment.
- **nit-2**: the E1 cursor-present sub-case's compared page-2 responses
  never actually carried a `nextCursor` with only 2 fixture agents, despite
  the comment's claim. Added a 3rd agent and an explicit assertion that page
  2 does emit one.
- **nit-3**: the N-5 walk fixture had `team=""` rows but no walk filtered on
  them. Added a second walk over the identical fixture with `label=team=`,
  checked against the same independent oracle, additionally filtered to
  `team=""` in the test.

Also a second rebase: `origin/main` advanced one more commit
(`224eb0328` -> `009227cb0`, #2193 reincarnate timeout bound), confined to
files this branch doesn't touch. Clean replay, zero conflicts.
`go build`/`go vet` pass; force-pushed with lease.

Verification: targeted hub (62 subtests) and store (5 subtests) tests, 0
failures, exit 0 on both, plus the N-1 mutation-test proof above.

New head: `5799d5ff8215e1e560e60b217e7ca9e75d137fd3`. Full disposition table:
see the P1b dev report.

## Hygiene pass (comment/name/file-name cleanup before going upstream)

The branch was approved for behavior, but still carried internal review-round
IDs (`B-1`, `N-3`, `nit-2`, `rev-4`, `S6`, `Q-G`, `NB-1`, round numbers,
erratum/errata references, `review1`, and similar) in code comments, test
names, subtest names and assertion messages, plus references to the design
doc and review reports by their storage path. None of that means anything to
an upstream reviewer who has no access to the internal design doc or review
history, so this pass:

- Renamed `agent_sorted_project_list_review1_test.go` (`git mv`) and split it
  by subject into three files: the paged-page-size decision-budget bound
  (`agent_sorted_project_list_paged_page_size_test.go`), the step-5a
  race/re-decision and includeDeleted behavior
  (`agent_sorted_project_list_race_redecision_test.go`), and cursor
  validation plus the fit/completeness and legacy byte-identical cases
  (`agent_sorted_project_list_validation_test.go`). No test function bodies
  changed, only their doc comments, a handful of assertion messages, and in
  one case a test fixture's display name string.
- Reworded every internal review/design-process ID found in comments, test
  names and assertion messages across `pkg/hub`, `pkg/store` and the
  Makefile into plain descriptions of the invariant or reason, keeping
  design section numbers (e.g. "design 5.3 step 5a") where they read
  naturally on their own.
- Replaced every storage-path reference in this file with plain wording
  (e.g. "the P1b dev report", "the lists-graph design doc"); IDs are left
  alone elsewhere in this file, since this log itself is internal-only.

No behavior, logic, identifier (other than test-function renames), or
assertion value changed. Verification: `go vet ./pkg/hub/... ./pkg/store/...`
passes; `go test -p 2 ./pkg/store/...` passes; `go test -p 2 -run
'Sorted|Cursor|AgentSort' ./pkg/hub/...` passes (288s, 0 failures).

### Hygiene pass, round 2 (follow-up review requested changes)

A follow-up hygiene review found the first pass incomplete: the first pass's
own ID-detection check used word-boundary matching, which does not match an
ID glued to an underscore (e.g. two test names still carried "_E2_"), and
was case-sensitive, so a lowercase fixture-ID fragment survived. It also
found four lingering pointers to internal documents and roles (a
non-existent "dev report", three "EM ruling" references) that a hygiene pass
should have replaced with the underlying technical reason, some leftover
review-process narrative phrasing, and several ragged comment paragraphs
left behind by the first pass's edits. One new commit fixed all of it:

- Renamed the two test functions still carrying the erratum ID in their
  names (and every doc comment cross-reference) to name the invariant
  instead (page-size bound) rather than the erratum.
- Replaced the four dangling pointers with the underlying technical reason
  inline, with no document or role reference left.
- Reworded the remaining review-process narrative phrasing (e.g. "a past
  reviewer's probe", "a past finding") to describe the property or fixture
  directly.
- Renamed the remaining finding-ID-derived test fixture literals (IDs,
  slugs, emails, names) to describe what they are for instead of which
  finding named them.
- Hand-rewrapped every ragged comment paragraph the first pass's edits left
  behind.

Verification: `go vet ./pkg/hub/... ./pkg/store/...` passes; `go test -p 2
./pkg/store/...` passes (store/agentsort/entadapter/enttest/storetest all
ok); `go test -p 2 -run 'Sorted|Cursor|AgentSort' ./pkg/hub/...` passes
(393s, 0 failures). Pushed before running tests, per convention.

### Rebase onto origin/main 499c07187 (P1a merged upstream)

`origin/main` advanced past this branch's base (`009227cb0` -> `499c0718`,
six commits: web-chat unread filter, SSE notification coalescing, files
placeholder test hardening, experiments tab, dev_local initiator
attribution, runtime-broker project-settings env collision) -- none confined
to files this branch touches. `git rebase origin/main` replayed cleanly,
zero conflicts, matching the EM's merge-tree dry run. `git range-diff
009227c..43504ff 499c071..1697be7` shows all 16 commits `=` (byte-identical
replay).

New head: `1697be76f6b31da0293e917f777f6f4743bc821b`. `go vet
./pkg/hub/... ./pkg/store/...` and `go build ./...` both pass. Force-pushed
with `--force-with-lease`. Long test suites not re-run (no conflicts), per
instruction; CI covers them.
