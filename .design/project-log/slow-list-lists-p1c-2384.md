# slow-list P1c — project-detail list window vertical slice (ptone/scion#2384)

Branch `perf/2384-agent-windows`, stacked on P1a (`perf/2385-sse-coalesce`,
`78c7c9ef1105b0aa7d18454195c7125aade657bb`), which is itself at the current
`origin/main` tip (`224eb0328ad2e001aab021adac76192f0214601d`) — no rebase
needed. Codes against the P1b API contract (`perf/2383-sorted-cursors`, in
review); no Go code touched.

## What

Implemented the web vertical slice from lists-graph.md §11 P1c (small and
paged window states only; held/capped land in P5 with `agent-drain.ts`):

- `web/src/shared/agent-sort.ts` (new): `agentCompare`/`sortAgents`, the
  `displayAgents` sort block moved out of project-detail.ts unchanged, plus
  `serverOrderCompare` (the server's `(K dir, created DESC, id DESC)` total
  order, design §4.2) used for the paged state's local re-sort (Q-D).
  `agents.ts` keeps its own inline copy for now — P1c does not touch it;
  migrating it is P4/P5's job per the file-ownership table.
- `web/src/client/agent-member-index.ts` (new): the `Map<id, phase>` member
  index from design §6.2, seeded from `stats.agents`.
- `web/src/client/agent-list-window.ts` (new): `AgentListWindow`, a small/
  paged state machine. `setSmall`/`setPaged` adopt a response; `items`/
  `display`/`total`/`stats` render either a local slice of the held complete
  set or the current server page; `setViewState` is local-only in the small
  state and triggers the window's own page-0 refetch when paged (sort/phase/
  page-size only — a label is never a reason to refetch by itself, so a
  `refetchIfPaged: false` escape hatch exists for live label-typing preview);
  `next`/`prev`/`refresh` page the server state; `applyChanges` implements the
  design §6.2 live-update table for the paged state (on-page replace + local
  re-sort, off-page/unknown member-index updates with the chip, idempotent
  deletes); `markResync` is the zero-cost reconnect signal.
- `web/src/components/shared/agent-pager.ts` (new): "a-b of N", Prev/Next, a
  persisted page size (25/50/100, default 25), loading/error, the chip, and
  the capped-banner shape (unused until P5's capped state exists).
- `web/src/components/pages/project-detail.ts`: `loadData` and
  `fetchAndMergeAgents` both now funnel into one new `loadAgentsForView(trigger)`,
  which sends exactly one request per trigger — the fit request when the view
  state is P1-eligible (list view, `updated` sort, label empty or `k=v`) and
  not refused, otherwise today's legacy request (`loadLegacyAgents`). A 422
  (`sorted_view_unavailable`) falls back to the legacy load and is remembered
  per committed label (`sortedRefusedForLabel`) so a retry only happens on a
  new label commit. `complete: true` keeps `this.agents`/`displayAgents`/grid/
  tree/stats/Stop-all exactly as before and additionally feeds the same array
  into `agentWindow.setSmall()` for the list view's pager. `complete: false`
  empties `this.agents` (grid/tree/stats run from `agentWindow.stats` via the
  new `agentStats` getter instead) and feeds the page into
  `agentWindow.setPaged()`. `onAgentsUpdated` (today's SSE merge over
  `this.agents`) is skipped while paged; `agents-changed` instead drives
  `agentWindow.applyChanges`, and `agents-resync` drives `agentWindow.markResync()`.
  A view/sort change that could make the view state P1-eligible (or that
  empties the window's paged data back to a not-eligible view) issues at most
  one request via the new `syncAgentsForViewState`, never more.

## Interim costs above 500 (documented per §11, for the PR body)

- Paged, then a switch to grid/tree or to a `name`/`status` sort: one legacy
  load (`loadLegacyAgents`, today's truncated ≤500 set), then free until the
  next trigger (P1c has no drain, so this is "held" only in the sense that
  nothing re-fetches it — a true held/capped state lands in P5).
- A legacy load in grid with `nextCursor` present (truncated), then a switch
  to the list view with `updated` sort: one fit request.
- Paged: optimistic lifecycle updates are not applied to the visible page —
  `onAgentsUpdated` is skipped while paged, and the row updates only on the
  next SSE delta (via `applyChanges`) or the next refresh (T6).
- At 500 candidates or fewer there is no interim cost beyond the new paged
  list slice.

## Q-A note (true-time ordering, for the PR body)

`serverOrderCompare`'s tie-break uses a string comparison of the same ISO
timestamps the server keys on (not the server's true-time comparison), so a
local re-sort within the paged state can drift from the server's order by
the same sub-second edge case the design already documents and accepts
(§4.2 Q-A, §14 R2). The view self-corrects on the next fetch.

## Deviations from the brief

None. `home.ts`, `agents.ts` and `agent-graph.ts` are untouched; no
completeness-flag callers were added; `state.ts` was not touched.

## Tests

- `web/src/shared/agent-sort.test.ts` (W1): `agentCompare`/`sortAgents`
  checked pairwise against both pre-change inline comparator snapshots
  (project-detail.ts's and agents.ts's) across 80 randomized agents, all
  four sort fields and both directions; a stable-tie test; `serverOrderCompare`
  tie-break tests.
- `web/src/client/agent-member-index.test.ts`: seed/set/delete/stats, delete
  idempotency.
- `web/src/client/agent-list-window.test.ts` (W4, window level): small-state
  display/pagination/stats parity and zero-fetch view-state changes; paged
  next/prev with correct cursors, the empty-last-page step-back, the R2-B2
  off-page-upsert-of-a-known-ID case, on-page replace+re-sort (Q-D), an
  on-page phase-filter failure (backfill), an on-page delete, a safe-no-op
  delete for an unknown ID, an unknown-ID delta for a member vs. a
  non-member, `markResync` (no request), `refresh()` (the chip click), and
  that `applyChanges` no-ops in the small state.
- `web/src/components/pages/project-detail-agent-window.test.ts` (W10 P1
  subset + the named W4 items, full component level): at 100 agents in list
  view with `updated` sort, page load issues exactly one request (the fit
  request) and every client-only interaction (grid/list/tree switches, sort
  dir flip, sort -> name, phase change, page navigation, label typing) issues
  zero, while a label commit and a lifecycle refresh each issue exactly one;
  the same page opened in grid view issues exactly one legacy request and
  grid -> list issues zero; a 422 falls back to the legacy load, is
  remembered for that label (a same-label lifecycle refresh does not retry
  sorted mode), and a new label commit retries once; a label-commit 400
  keeps the previously loaded agents; in the paged state, an off-page upsert
  of a page-0 agent (driven through the real `handleUpdate` -> coalesced
  flush -> `agents-changed` pipeline) raises the chip and updates stats with
  no request, and an `agents-resync` raises the chip with no request.
- A5 (small-state display identical to pre-change `displayAgents`) is
  established by construction (`agent-sort.ts`'s W1 parity) plus the
  `agent-list-window.test.ts` small-state display test.

Commands: `npm run typecheck` (pass), `npx eslint` on every changed/new file
(new files clean; `project-detail.ts`'s error count is unchanged from its
pre-P1c baseline on this branch, 40 — only new `missing-return-type`
warnings, consistent with the file's existing style; new `*.test.ts` files
hit the same pre-existing "TSConfig does not include this file" parse error
every test file in this repo hits), `npx prettier --check` on every changed/
new file (pass), `npx vitest run` (full suite, pass).

## Round 1 review addendum

Review: `gs://scion-xproject-exchange/slow-list/reviews/lists-p1c-rev-1.md`
(REQUEST CHANGES: 1 critical, 5 required, 7 non-blocking). All 13 findings
(B1-B6, N1-N6; N7 no change) fixed in one commit, rebased twice since (P1a
moved to `043425ef` then `7c6140b0`); final SHA `a055b9f4`. Full mapping of
each finding to its fix, file and test is in the dev report addendum:
`gs://scion-xproject-exchange/slow-list/reports/lists-p1c-dev.md`.

Summary of the fixes:
- **B1 (critical):** the small-state window read a copied agent array, so
  SSE updates never reached the list view. Now reads `this.agents` live via
  a `getHeldAgents()` callback.
- **B2:** a truncated legacy load left the window stuck in `'paged'`,
  freezing live updates and stats for grid/tree/name-sort views above 500
  agents. `loadLegacyAgents` now always exits `'paged'`.
- **B3:** a view/sort change while paged could issue two requests (the
  window's own `setViewState` plus `syncAgentsForViewState`). The window no
  longer fetches on its own; `project-detail.ts` is the single decision
  point.
- **B4:** the persisted page size wasn't read by the host, so the first
  request used the wrong `limit` while the pager showed the stored size.
  Now read once in `connectedCallback`; the pager is a controlled component.
- **B5:** no stale-response guard on the page-level loads. Added a
  generation counter plus an `AbortController`.
- **B6:** an off-page SSE upsert could inflate the member index for an
  agent outside the committed label. New members are now gated by the
  server's own add rule (project + label match).
- **N1:** the paged-state chip now follows the design's table (counts-only
  vs. newly-relevant) instead of firing on every off-page change.
- **N2-N6:** stats assertions added to the R2-B2 test, report wording
  corrected, a non-OK label commit restores the previous label, the delete
  dialog falls back through the window/state for an agent's name while
  paged, `serverOrderCompare` is reflexive, and W1 gained a dedicated
  createdAt/updatedAt fallback case.

Full suite (`npx vitest run --no-file-parallelism`), run after each of the
two rebases: 3104/3106 then 3106/3107 passing; the one/two failures both
times are the same pre-existing `agent-create-projects.test.ts` flake,
unrelated to this branch (confirmed in the original report).
