# Agent-list live-update consumers: project-detail and agents pages (#2385)

Branch `perf/2385-sse-consumers`, base `origin/main` 7ca2edd599128241ecf12ab064eff6a0a831a6e.
Touches `web/src/client/state.ts`, `web/src/client/agent-merge.ts` (new),
`web/src/components/pages/project-detail.ts`, `web/src/components/pages/agents.ts`,
plus four test files (two new, two touched). Includes a tombstone-race fix
(described below) plus further follow-up fixes described later in this log.

## What

Implemented this slice of the agent-list live-updates design (§7/§11;
sections 4.3, 6.1-6.4, 7, 9, 11, 13 read before coding):

- **`web/src/client/agent-merge.ts` (new):** `mergeChanged(held, change, options)`
  applies one coalesced `agents-changed` payload to a held agent array.
  Identity is preserved on two levels: every untouched agent object is carried
  over by reference, and the returned array is `held` itself (same reference)
  when nothing in `change` actually altered membership or content. `options`
  carries `getAgent` (the full-object lookup, typically `stateManager.getAgent`),
  an optional `shouldAdd` predicate (the page's add rule — gates only *new* IDs;
  an ID already held keeps getting updates regardless), and optional
  `scopeCapabilities` to inherit onto a brand-new agent with none of its own
  (design §7: "scope-capability inheritance for SSE-created agents ... applies
  to new IDs only"). `change.unknown` is not consulted — there is no full
  object for an unknown ID to adopt yet; that stays the paged window's
  member-index concern.
- **`project-detail.ts`:** removed `onAgentsUpdated` (the per-event full
  rebuild listening on `agents-updated`) and its listener registration.
  `boundOnAgentsChanged` now does both halves of design §11's row for this
  work: calls `agentWindow.applyChanges` unconditionally (already wired by
  the earlier window work, a no-op outside `'paged'`), then, when the
  window is not `'paged'`, calls the new
  `mergeAgentsChanged` method, which merges through `mergeChanged` with
  `shouldAdd: (agent) => agent.projectId === this.projectId` (today's project
  add rule) and the page's `agentScopeCapabilities` (including its existing
  lazy-derive-from-held-agents fallback, preserved as-is). No other
  project-detail diff: the window/REST-load functions (already landed) are
  untouched.
  Searched for `repeat(` in the agent grid and list render functions per the
  brief — none is used (both render through plain `.map()`); nothing to
  change there (noted as a deviation below).
- **`agents.ts`:** same shape — removed `onAgentsUpdated`/the `agents-updated`
  listener, added `onAgentsChanged` wired to `agents-changed`, merging through
  `mergeChanged` with `shouldAdd: () => this.agentScope === 'all'` (today's
  global add rule: scope `all` only) and `this.scopeCapabilities`. No
  `agent-list-window`/paging on this page yet (left for a later change per
  §11); this is a straight swap of the live-update mechanism only.
- **`state.ts` (TTL/seed-epoch fix carried into this slice):** `bufferAgentDelta`'s
  30s expiry timer now also deletes the expiring ID's entry from every
  currently-open seed epoch's `deltas` map, not just from `pendingAgentDeltas`.
  Before this, a status delta for an unknown ID recorded into an open epoch
  would survive past its 30s buffer expiry *inside the epoch* even though
  `pendingAgentDeltas` (and therefore live state, on a later `created`) had
  already dropped it — a drain whose epoch stayed open that long would seed a
  stale value live state never showed. Both stores are now kept in sync at
  expiry; a later delta for the same ID still starts both fresh via the
  existing fold-or-create paths.
- Neither `markAgentSetComplete` nor `isAgentSetComplete` gained a caller
  here; that is left for a later change. `home.ts`, `agent-graph.ts` and
  `debug-log.ts` are untouched and stay on `agents-updated`, which
  `state.ts` still emits once per flush (already landed).

## Why

The agent-list live-updates design (binding errata applied), §7
("`agents-changed` consumers ... through `mergeChanged` ... or
`window.applyChanges`. No per-event full rebuild remains") and §11's row
for this work. The TTL/epoch fix closes a correctness gap in the already-merged state.ts
slice that this brief carried forward as an explicit requirement, with its
own test.

## Tests

- **`agent-merge.test.ts` (new):** no-op short-circuit (same array reference)
  when nothing changed or an upserted ID has no full agent yet; unchanged
  agents stay `===` across an unrelated upsert; an upsert already `===` the
  held object is a no-op; delete drops the row and changes array identity; a
  delete for an unheld ID is a safe no-op; `shouldAdd` gates a new ID but
  never an already-held one; scope-capability inheritance on a brand-new
  agent only (not on an existing one, and not when the new agent already
  carries its own); combined deletes+upserts in one call; unknown-ID deltas
  ignored; a 20-agent burst exercising multiple upserts/one delete/one create
  in a single flush with full identity-preservation checks. A dedicated
  reference-equality test ("carries the untouched object through by
  reference") pins the mutation-check contract.
- **Mutation check (brief requirement):** temporarily changed the final
  `Array.from(byId.values())` to `.map((a) => ({ ...a }))` (breaking identity
  preservation) and reran `agent-merge.test.ts`: 7 of 16 tests failed, all and
  only the ones asserting `.toBe(...)` reference equality on a passed-through
  object (the other 9, asserting content/membership only, still passed, as
  expected). Reverted; all 16 pass again. Result recorded in the test file's
  own comment next to that assertion.
- **`state-seed-epoch.test.ts`:** added one test (fake timers) for exactly the
  design's cited sequence — a status delta for an unknown ID with a field
  (`labels`) only that delta ever sets, recorded into an open epoch; 30s pass
  with nothing else touching the ID; `created` arrives (live state already
  shows the post-expiry value, confirmed); the seed lands (fed the same
  token) and must agree with live state, not resurrect the expired delta.
  Verified it fails without the `state.ts` fix (`labels` resurfaces as
  `{env: 'stale'}`) and passes with it, via a stash/run/pop cycle.
- **`project-detail-agent-window.test.ts`:** existing small-state live-update
  test extended with an explicit `===` identity-preservation assertion (the
  array identity changes when a status delta lands, but an untouched sibling
  agent is the same object before and after) — this test already exercised
  the new `mergeAgentsChanged` path end-to-end (real `stateManager`, real SSE
  delta via `handleUpdate`, zero extra requests) since it supersedes the old
  `onAgentsUpdated`; the new assertion makes the identity-preservation claim
  explicit rather than implicit in the existing phase/count assertions.
- **`agents-live-updates.test.ts` (new):** end-to-end coverage of
  coalescing and identity preservation for the `/agents` page, mirroring
  the project-detail integration-test style (real
  `stateManager`, faked `fetch`/`EventSource`/`localStorage`): a status delta
  updates one row while every untouched agent stays `===` and zero requests
  are issued; an SSE-created agent is added under scope `all`; an SSE delete
  removes a row; under a `mine` scope filter a brand-new SSE-created agent in
  a different project is *not* adopted, but an already-held agent still gets
  its updates. (No prior test covered `agents.ts`'s live-update merge path at
  all — `agents-scope.test.ts` only covers `loadedScope` tracking and mocks
  `stateManager` out entirely, so this is new coverage, not a port of an
  existing one.) One `beforeEach` note: `stateManager` is a process-wide
  singleton and the page always opens the same `{type: 'dashboard'}` scope,
  which `setScope` no-ops on when unchanged — each test first calls
  `stateManager.setScope({type: 'brokers-list'})` to force a real scope
  change (and therefore a real state clear) before mounting, or state seeded
  by an earlier test in the file leaks into the next one's hydrated-data
  reuse check.

## Commands and results

- `npm run typecheck` (`tsc --noEmit`): pass, no output.
- `npx eslint` on the four touched/new non-test files
  (`state.ts`, `agents.ts`, `project-detail.ts`, `agent-merge.ts`): 52 errors /
  108 warnings — byte-identical in count to a baseline run against the
  pre-change versions of the three existing files (verified by stashing this
  branch's changes and relinting); zero new errors or warnings introduced.
  `agent-merge.ts` alone: clean. `*.test.ts` files cannot be typed-linted at
  all in this sandbox — `tsconfig.json` excludes `src/**/*.test.ts` and
  `eslint --ext .ts` still tries to type-check every `.test.ts` file it
  walks, failing with "TSConfig does not include this file"; confirmed this
  is a pre-existing, repo-wide sandbox limitation (not something this PR
  introduced) by running the full `npm run lint` and observing the identical
  parsing error against every other pre-existing `.test.ts` file under `src`.
- `npx prettier --check` on every touched/new file: pass (one file needed
  `--write` first, `agent-merge.test.ts`, then re-checked clean).
- Targeted `npx vitest run`: `agent-merge.test.ts`, `state-seed-epoch.test.ts`,
  `state-coalescing.test.ts`, `state-compaction-fuzz.test.ts`,
  `state-completeness-flag.test.ts`, `state.test.ts`, `agent-list-window.test.ts`,
  `project-detail.test.ts`, `project-detail-agent-window.test.ts`,
  `project-detail-files.test.ts`, `agents-scope.test.ts`,
  `agents-live-updates.test.ts`, `agent-create-projects.test.ts` — 13 files,
  202 tests, all passing. (Per the brief's web-only instruction: no Go
  builds/tests run; the whole web suite and `make ci`/`make ci-full` were not
  run.)

## Deviations from the brief

- The brief says to search for `repeat(` in the agent grid and list renders
  and remove any identity-defeating keying there. Neither render path in
  either file uses the `repeat()` directive at all (both use plain
  `.map()`); the only `repeat(` hit in `project-detail.ts` is an unrelated
  CSS `grid-template-columns: repeat(auto-fill, ...)`. The earlier window
  work evidently implemented the grid/list render with `.map()` rather than the design
  doc's `repeat(items, a => a.id, ...)` proposal. Raised during development;
  **decision: accepted, no change here** — keyed rendering of window items
  belongs to a later change that rebuilds grid and list from the window.

## Follow-up: tombstone-race fix

Flagged as a second deviation: `onAgentsUpdated`'s old deleted-agent handling
scanned the *entire* persistent `stateManager.getDeletedAgentIds()` set on
every flush (a side effect of its full-rebuild-from-`getAgents()` design),
which incidentally also scrubbed a tombstoned ID that had raced into
`this.agents` via a REST response landing after its SSE `deleted` had
already been processed in an earlier flush. `mergeChanged`-based merging
only inspects the current flush's `change.deleted`, so that race was no
longer self-healed by an unrelated later flush.

**Decision: fix it now** — removing the old per-flush rebuild removes its
incidental scrubbing too, so without a replacement this work reintroduces the
stale-deleted-agent case. Fixed with a new `dropTombstoned(agents,
deletedIds)` in `agent-merge.ts` (returns the same array reference when
nothing needs dropping, so it adds no churn on the common case), applied at
every point a REST response is assigned into page-level state:
`project-detail.ts`'s `loadAgentsForViewImpl` (both the `complete` and
paged branches) and `loadLegacyAgentsImpl`, its paged window page fetcher
`fetchAgentsPage` too, since a mid-race delete can land on any page fetch,
not only the first, and `agents.ts`'s `fetchAndMergeAgents`. No request
added anywhere.

Tests added for this: one end-to-end test per page (SSE
delete processed, then a REST response still listing that ID lands, agent
not shown) — `project-detail-agent-window.test.ts` > "small state: live
updates" > "a REST response landing after an SSE delete does not resurrect
the deleted agent"; `agents-live-updates.test.ts` > "a REST response
landing after an SSE delete does not resurrect the deleted agent" — plus
four `agent-merge.test.ts` unit tests for `dropTombstoned` itself (no-op
when no IDs are deleted at all; no-op when none of the held agents are
tombstoned; drops one; drops several). Verified each end-to-end test fails
without its corresponding fix: reverted the `dropTombstoned` call at the
relevant site (`sed`, not committed), reran, confirmed the failure, restored.

Commands and results for this follow-up: `npm run typecheck` pass; `npx
eslint` on the four non-test files — 160 problems (52 errors, 108
warnings), unchanged from the prior commit's baseline; `npx prettier
--check` pass; targeted `npx vitest run` across the same 13 files —
208 tests, all passing (202 before this commit + 6 new).

## Design mapping (brief items to file:function)

1. `agent-merge.ts:mergeChanged` — new helper, identity-preserving merge,
   scope-capability inheritance moved here (applies to new IDs only).
2. `project-detail.ts:boundOnAgentsChanged` / `mergeAgentsChanged` —
   `onAgentsUpdated` removed; small state via `mergeChanged`, paged state via
   the already-wired `agentWindow.applyChanges`; no `repeat()` found to fix
   (see Deviations); no request added anywhere.
3. `agents.ts:onAgentsChanged` — `onAgentsUpdated`/`.map` full-rebuild site
   removed, replaced with `mergeChanged`.
4. Verified: no `markAgentSetComplete`/`isAgentSetComplete` callers added.
5. `state.ts:bufferAgentDelta`'s expiry timer — epoch/buffer TTL agreement
   fix, with `state-seed-epoch.test.ts`'s new test for the exact cited
   sequence.
6. (Follow-up.) `agent-merge.ts:dropTombstoned` — applied at
   `project-detail.ts:loadAgentsForViewImpl` (both branches),
   `loadLegacyAgentsImpl`, `fetchAgentsPage`, and `agents.ts:fetchAndMergeAgents`.

## Test coverage map

- **Coalescing and identity preservation:** `agent-merge.test.ts` → "unchanged
  agents stay the same object...", "...20-agent burst...", "...applies
  deletes and upserts together...", "carries the untouched object through
  by reference...", and the `dropTombstoned` block (four tests);
  `project-detail-agent-window.test.ts` → "small state: live
  updates" (extended with the `===` assertion, plus the new "a REST response
  landing after an SSE delete does not resurrect the deleted agent");
  `agents-live-updates.test.ts` → "merges a status delta in place, preserving
  identity for every untouched agent...", plus its own new "a REST response
  landing after an SSE delete does not resurrect the deleted agent".
- **Seed-epoch behavior:** `state-seed-epoch.test.ts` → "a TTL-expired buffered
  delta is not replayed by a later seed" > "matches live state once the
  buffer entry it was recorded from has expired".
- **Request counts stay unchanged:** `project-detail-agent-window.test.ts`'s
  existing request-count gate test ("page load issues exactly one agents
  request...") and every other request-count assertion in that file, rerun
  unchanged and still passing (zero added); `agents-live-updates.test.ts`'s
  tests each assert `requests.length` (or an equivalent fetch-call count)
  stays at its pre-delta count across every SSE delta.

## Follow-up: capability carry-forward, pending-buffer timer and tombstone filtering

Fixes, each landed with a regression test that was verified to fail
against the pre-fix code and pass after:

- **An SSE-created agent lost its inherited scope capabilities on its next
  status delta (a regression vs `main`):** `mergeChanged`'s existing-member
  branch replaced the held copy outright with `stateManager`'s object,
  which never carried capabilities a *page* had inherited on top for
  display. Fixed in `agent-merge.ts`'s existing-member branch: when the
  incoming update has no `_capabilities` of its own and the held object
  does, carry the held object's `_capabilities` forward (a spread copy of
  only that one changed object; everything else keeps its reference).
  Tests: a `mergeChanged` unit test, plus one page-level test per page
  (SSE create, then a status delta, then `_capabilities` still present).
- **The TTL-expiry fix's epoch purge could wipe known-state deltas it
  never meant to:** an open epoch's entry is shared by buffered
  (unknown-ID) deltas and later known-state deltas; if an ID became known
  through an untokened `seedAgents` call (every page-level seed today),
  that path never cleared the ID's pending-buffer timer, so a stale timer
  firing later wiped the whole epoch entry, including known-state deltas
  recorded after the ID became known. Fixed in two places: `seedAgents`
  now clears the pending buffer and its timer for every seeded ID, tokened
  or not (previously only when a recorded epoch delta was found); the
  timer callback also guards its epoch purge with `state.agents.has(id)`,
  as defense in depth. Test: a stale-timer sequence that reproduces the bug,
  reproduced and confirmed to fail pre-fix.
- **Tombstones were not applied to paged stats:** `dropTombstoned` filtered
  page rows but not a paged response's `stats.agents`, so a deleted agent
  could be re-seeded into the member index, inflating paged total/running
  counts and Stop-all visibility with nothing to ever correct it. Fixed
  with a new `dropTombstonedPairs` in `agent-merge.ts`, applied via a
  shared `project-detail.ts:freshStats` helper at both sites that feed the
  window's stats (the paged branch of `loadAgentsForViewImpl` and
  `fetchAgentsPage`). Test: a paged-state test confirming an SSE-deleted
  agent's count stays dropped across a stats-bearing refetch — later
  extended with an assertion covering `fetchAgentsPage`'s own stats
  filtering specifically (see "Stats filtering test and comment
  consolidation" below).

Smaller fixes, also closed:

- **Closed a test gap in tombstone-drop coverage:** mutation testing had
  shown 3 of 5 `dropTombstoned` call sites untested (the paged branch of
  `loadAgentsForViewImpl`, `loadLegacyAgentsImpl`, and `fetchAgentsPage`).
  Added one test per site, each verified to fail with that site's
  `dropTombstoned` call reverted.
- **Corrected an overstated doc comment:** the `agent-merge.ts`
  `scopeCapabilities` comment had implied `stateManager` preserves
  capabilities a *page* inherited on top of its own object — the false
  premise behind the capabilities-dropped fix above.
- **Removed design-process tags from new comments and test names:**
  dropped the acceptance/test-plan tags this change had introduced
  (`agent-merge.ts`, `agent-merge.test.ts`, `agents-live-updates.test.ts`,
  `project-detail-agent-window.test.ts`, `state-seed-epoch.test.ts`),
  replacing them with plain wording. Pre-existing occurrences already on
  `main` (for example the "seed epoch" describe name in
  `state-seed-epoch.test.ts`) were left as-is.
- **Fixed misleading doc wording:** `project-detail.ts`'s
  `mergeAgentsChanged` doc said it was a no-op while paged; it is actually
  never called while paged (the caller gates it). Corrected.

Other items considered:

- **A created event landing mid-load has a weaker self-heal than `main`
  had:** accepted as a known limitation, deferred to a later change. A
  created event that lands while a page's own REST load is already in
  flight is added by `mergeChanged` and then overwritten by that load's
  REST assignment if the snapshot predates the create; it now heals only
  on that one agent's own next delta, instead of at any agent's next live
  update (flush) as `main`'s full rebuild happened to provide. The seed-epoch
  machinery already used for drains is the natural fix once these load
  paths adopt it. Not blocking: the design explicitly removes full
  rebuilds. **Decided by ptone (2026-10-01 23:05Z): postponed to a later
  change, tracked in `ptone/scion#2560`.** Noted in the PR body as a known
  limitation referencing that issue.
- **A paged-fetcher page-shortening edge case:** no action needed.
  Dropping a tombstoned row can shorten a server page, which can make the
  "empty page past the first steps back" rule step back one page early if
  that page held only the just-deleted agent. Self-corrects on the next
  navigation.
- **Tombstones are never cleared by a later same-ID create:** no action
  needed. Unreachable in practice, since IDs are UUIDs — the same
  characteristic `stateManager.seedAgents` already relies on for its own
  tombstone-skip.
- **The page copy is replaced outright instead of spread-merged:** no
  action needed. A semantic change from `main`'s spread merge, noted for
  awareness; no concrete case exists on either page's routes today.
- **Internal agent-role names and a workstream document's filename
  appeared in this log:** fixed. Replaced the specific agent name with
  generic role wording (matching the precedent in earlier logs on `main`,
  which use "the lead" rather than a specific agent's name), and replaced
  the internal design document's filename with a description of what it
  covers.

### Commands and results

- `npm run typecheck`: pass, no output.
- `npx vitest run` on five targeted files
  (`agent-merge.test.ts`, `state-seed-epoch.test.ts`,
  `state-coalescing.test.ts`, `agents-live-updates.test.ts`,
  `project-detail-agent-window.test.ts`): 125/125 passing (117 before
  this follow-up + 8 new: 3 for the capability-preservation fix, 1 for the
  stale-timer fix, 1 for the paged-stats fix, 3 for the tombstone-site
  test-gap closures).
- Full targeted suite (the same 13 files tracked throughout this work):
  216/216 passing.
- `npx eslint` on the four non-test files: 160 problems (52 errors, 108
  warnings), unchanged from the prior baseline — zero new issues.
- `npx prettier --check`: pass on every touched file.
- Each new regression test (7 total, each paired with a specific fix) was
  verified to fail when its corresponding fix was reverted locally, then
  to pass again once restored.

## Follow-up: stats filtering test and comment consolidation

Three items closed:

- **A stats-filtering call site had no dedicated assertion:** the window's
  own page-fetcher test (the one covering `fetchAgentsPage`'s row
  filtering) did not separately assert its stats filtering, so a mutant
  that skipped filtering there survived. Added an assertion on the member
  index's total count after the fetch, right alongside the existing
  rows assertion. Verified it fails when that one call site's filtering is
  reverted, then passes once restored.
- **Repeated reasoning across several comments:** four separate comments
  in `state.ts` (the buffering function's own doc comment, its expiry
  timer's callback, the seeding function's doc comment, and the seeding
  loop's own inline comment) restated the same multi-paragraph reasoning
  about why a stale timer must not survive into the known phase. Kept the
  full reasoning in one place (the buffering function's doc comment) and
  reduced the other three to one-line pointers at it.
- **This log's own write-up used bare finding labels and an internal
  document's filename:** rewritten throughout to describe the underlying
  behavior instead of leading with a label, keeping issue numbers such as
  `ptone/scion#2560` and section references.

### Commands and results

- `npm run typecheck`: pass, no output.
- `npx vitest run` on the same five targeted files: all passing,
  with one new assertion added to an existing test (no new test count
  change).
- `npx eslint` on the four non-test files: unchanged from the prior
  baseline — zero new issues.
- The new assertion was verified to fail when its corresponding call
  site's filtering was reverted (that site alone, via a scripted
  single-line edit), then to pass again once restored.

## Follow-up: paged-gate and project add-rule test coverage

Two more call sites had no dedicated test:

- **The paged/small-state gate itself was untested:** `boundOnAgentsChanged`
  only calls `mergeAgentsChanged` when the window is not `'paged'`, but no
  test exercised an SSE create and status delta while genuinely paged and
  checked that `this.agents` stayed empty throughout. Added a test to the
  paged-state live-updates suite: mount paged, emit an SSE create for a
  project agent plus a status delta for an existing one, and assert
  `this.agents` stays at length 0 while `agentStats` (the member index)
  reflects both — 5 original members plus the new create. Verified it
  fails when the gate is forced to always run (replacing the condition
  with an unconditional branch), then passes once restored.
- **The project page's add rule was untested for a foreign project:** the
  rule that gates adding a *new* ID (`agent.projectId === this.projectId`)
  had no test for an agent whose own `projectId` field names a different
  project. Added a test: an SSE create arrives on this project's own
  subject, but the payload's `projectId` names another project; the agent
  must not be added. Verified it fails when the rule is replaced with one
  that always allows new IDs, then passes once restored.

### Commands and results

- `npm run typecheck`: pass, no output.
- `npx vitest run` on the same five targeted files: all passing, with 2
  new tests (127 total, up from 125).
- `npx eslint` on the four non-test files: unchanged from the prior
  baseline — zero new issues.
- `npx prettier --check`: pass on the changed file.
- Both new tests were verified to fail under their respective mutants
  (scripted single-line edits, not committed), then to pass again once
  restored.
