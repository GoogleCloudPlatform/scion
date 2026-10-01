# slow-list P4 — agents-changed consumers on project-detail and agents (#2385)

Branch `perf/2385-sse-consumers`, base `origin/main` 7ca2edd599128241ecf12ab064eff6a0a831a6e.
Touches `web/src/client/state.ts`, `web/src/client/agent-merge.ts` (new),
`web/src/components/pages/project-detail.ts`, `web/src/components/pages/agents.ts`,
plus four test files (two new, two touched). Includes one post-dev-report
follow-up fix (the tombstone-race fix below), ruled by slow-list-lists-em.

## What

Implemented the P4 slice from `lists-graph.md` §7/§11 (sections 4.3, 6.1-6.4, 7,
9, 11, 13 read before coding):

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
  `boundOnAgentsChanged` now does both halves of design §11's P4 row: calls
  `agentWindow.applyChanges` unconditionally (already wired in P1c, a no-op
  outside `'paged'`), then, when the window is not `'paged'`, calls the new
  `mergeAgentsChanged` method, which merges through `mergeChanged` with
  `shouldAdd: (agent) => agent.projectId === this.projectId` (today's project
  add rule) and the page's `agentScopeCapabilities` (including its existing
  lazy-derive-from-held-agents fallback, preserved as-is). No other
  project-detail diff: the window/REST-load functions (P1c) are untouched.
  Searched for `repeat(` in the agent grid and list render functions per the
  brief — none is used (both render through plain `.map()`); nothing to
  change there (noted as a deviation below).
- **`agents.ts`:** same shape — removed `onAgentsUpdated`/the `agents-updated`
  listener, added `onAgentsChanged` wired to `agents-changed`, merging through
  `mergeChanged` with `shouldAdd: () => this.agentScope === 'all'` (today's
  global add rule: scope `all` only) and `this.scopeCapabilities`. No
  `agent-list-window`/paging on this page yet (P5 scope per §11); this is a
  straight swap of the live-update mechanism only.
- **`state.ts` (TTL/seed-epoch fix carried into P4):** `bufferAgentDelta`'s
  30s expiry timer now also deletes the expiring ID's entry from every
  currently-open seed epoch's `deltas` map, not just from `pendingAgentDeltas`.
  Before this, a status delta for an unknown ID recorded into an open epoch
  would survive past its 30s buffer expiry *inside the epoch* even though
  `pendingAgentDeltas` (and therefore live state, on a later `created`) had
  already dropped it — a drain whose epoch stayed open that long would seed a
  stale value live state never showed. Both stores are now kept in sync at
  expiry; a later delta for the same ID still starts both fresh via the
  existing fold-or-create paths.
- Neither `markAgentSetComplete` nor `isAgentSetComplete` gained a caller in
  this phase (P5). `home.ts`, `agent-graph.ts` and `debug-log.ts` are
  untouched and stay on `agents-updated`, which `state.ts` still emits once
  per flush (P1a).

## Why

Design doc `lists-graph.md` r8 (md5 `2a21517bf376552064b727012ea67935`, binding
errata applied) §7 ("`agents-changed` consumers ... through `mergeChanged`
... or `window.applyChanges`. No per-event full rebuild remains") and §11's P4
row. The TTL/epoch fix closes a correctness gap in the already-merged P1a
state.ts that P4's brief carried forward as an explicit requirement, with its
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
  in a single flush with full identity-preservation checks (W2). A dedicated
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
  `onAgentsUpdated`; the new assertion makes the W2/A10 identity claim
  explicit rather than implicit in the existing phase/count assertions.
- **`agents-live-updates.test.ts` (new):** end-to-end W2/W3 coverage for the
  `/agents` page, mirroring the project-detail integration-test style (real
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
  CSS `grid-template-columns: repeat(auto-fill, ...)`. P1c evidently
  implemented the grid/list render with `.map()` rather than the design
  doc's `repeat(items, a => a.id, ...)` proposal. Flagged for the EM;
  **ruling: accepted, no change in P4** — keyed rendering of window items
  belongs to P5, which rebuilds grid and list from the window.

## Follow-up: tombstone-race fix (EM ruling)

Flagged as a second deviation: `onAgentsUpdated`'s old deleted-agent handling
scanned the *entire* persistent `stateManager.getDeletedAgentIds()` set on
every flush (a side effect of its full-rebuild-from-`getAgents()` design),
which incidentally also scrubbed a tombstoned ID that had raced into
`this.agents` via a REST response landing after its SSE `deleted` had
already been processed in an earlier flush. `mergeChanged`-based merging
only inspects the current flush's `change.deleted`, so that race was no
longer self-healed by an unrelated later flush.

**Ruling: fix it in P4** — removing the old per-flush rebuild removes its
incidental scrubbing too, so without a replacement P4 reintroduces the
stale-deleted-agent case. Fixed with a new `dropTombstoned(agents,
deletedIds)` in `agent-merge.ts` (returns the same array reference when
nothing needs dropping, so it adds no churn on the common case), applied at
every point a REST response is assigned into page-level state:
`project-detail.ts`'s `loadAgentsForViewImpl` (both the `complete` and
paged branches) and `loadLegacyAgentsImpl`, its paged window page fetcher
`fetchAgentsPage` (the EM's "if the paged window's server pages need the
same filter, apply it there too" — applied), and `agents.ts`'s
`fetchAndMergeAgents`. No request added anywhere.

Tests added per the EM's instruction: one end-to-end test per page (SSE
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

## Design mapping (P4 bullets to file:function)

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
6. (Follow-up, EM ruling.) `agent-merge.ts:dropTombstoned` — applied at
   `project-detail.ts:loadAgentsForViewImpl` (both branches),
   `loadLegacyAgentsImpl`, `fetchAgentsPage`, and `agents.ts:fetchAndMergeAgents`.

## W2/W3/W10 sub-cases to test names

- **W2 (coalescing/identity):** `agent-merge.test.ts` → "unchanged agents stay
  the same object...", "...20-agent burst...", "...applies deletes and
  upserts together...", "carries the untouched object through by
  reference...", and the `dropTombstoned` block (four tests);
  `project-detail-agent-window.test.ts` → "small state: live
  updates" (extended with the `===` assertion, plus the new "a REST response
  landing after an SSE delete does not resurrect the deleted agent");
  `agents-live-updates.test.ts` → "merges a status delta in place, preserving
  identity for every untouched agent...", plus its own new "a REST response
  landing after an SSE delete does not resurrect the deleted agent".
- **W3 (seed epoch):** `state-seed-epoch.test.ts` → "a TTL-expired buffered
  delta is not replayed by a later seed" > "matches live state once the
  buffer entry it was recorded from has expired".
- **W10 (request counts, rows 1-18 unchanged):** `project-detail-agent-window.test.ts`'s
  existing P1-subset-gate test ("page load issues exactly one agents
  request...") and every other request-count assertion in that file, rerun
  unchanged and still passing (zero added); `agents-live-updates.test.ts`'s
  tests each assert `requests.length` (or an equivalent fetch-call count)
  stays at its pre-delta count
  across every SSE delta.
