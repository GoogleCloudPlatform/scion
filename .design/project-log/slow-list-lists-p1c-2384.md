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

Review: the round-1 review document (`lists-p1c-rev-1.md`)
(REQUEST CHANGES: 1 critical, 5 required, 7 non-blocking). All 13 findings
(B1-B6, N1-N6; N7 no change) fixed in one commit, rebased twice since (P1a
moved to `043425ef` then `7c6140b0`); final SHA `a055b9f4`. Full mapping of
each finding to its fix, file and test is in the dev report
(`lists-p1c-dev.md`).

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

## Round 2 review addendum

Review: the round-2 review document (`lists-p1c-rev-2.md`)
(REQUEST CHANGES: 0 critical, 3 required (B1', B2', B3'), 5 non-blocking
(N1'-N5')). All 8 findings fixed in one commit, rebased onto P1a's latest
head (`4cdb0b54`); final SHA `11b6db22`. Full finding-to-fix-to-test mapping
in the dev report addendum (`lists-p1c-dev.md`).

Summary:
- **B1':** `setViewState` resets `pageIndex` only in the small state, so a
  label keystroke while paged no longer desyncs the pager/chip from the
  rows actually shown.
- **B2':** the paged-state chip rule now matches design §6.2 exactly in the
  three reproduced cases — a split on-page/off-page K-range predicate (page
  0's "absorbs anything newer" exception only applies off-page, and only to
  the *top*, not the bottom), a state-known non-member outside the
  committed label is ignored (no chip), and a newly created off-page member
  only chips if it could land on the page being viewed.
- **B3':** added the always-paged dir-flip and list<->grid x3 (x6 toggles)
  tests round 1 asked for; retitled the small-after-promotion test so it no
  longer implies it covers the always-paged case.
- **N1'-N5':** window navigation now supersedes a stale pending page-level
  load; the pager's row range tracks the real offset through short pages;
  the fit-path label-400 case has a test; `display` is memoized; a loading
  indicator replaces the empty-filter message during the paged->grid gap.

Full suite (`npx vitest run --no-file-parallelism`) after the rebase: all
109 files, 3126 tests passed — the previously-flaky
`agent-create-projects.test.ts` (unrelated to this branch) did not even
reproduce this run.

## Round 3 review addendum

Review: the round-3 review document (`lists-p1c-rev-3.md`)
(REQUEST CHANGES: 0 critical, 3 required (B1'', B2'', B3''), 3 non-blocking
(N1''-N3'')). Two were regressions from the round-2 fixes themselves
(B1''/B2'', from B1'/N1'); B3'' was a pre-existing design §6.3 deviation
the first two rounds' tests didn't catch. All 8 round-2 findings and the
two reopened round-1 items (B3, N1) were reconfirmed closed. Fixed in one
commit, rebased onto P1a's latest head (`6519b424`); final SHA `dca93bc4`.
Full mapping in the dev report (`lists-p1c-dev.md`).

Summary:
- **B1'':** `setSmall()` now resets `pageIndex` to 0 when the previous
  state was paged (a paged -> small transition always swaps in a different
  data set); small -> small still never resets.
- **B2'':** removed the `agentsLoadGen` bump from `onPagerNav` (it let a
  pager click race a page-level request and draw a legitimate 400).
  Replaced it with disabling the pager itself (`.loading = window.loading
  || agentsLoading`) plus a defense-in-depth guard with the same condition,
  so the race can't start even via a raw event dispatched on the pager host.
- **B3'':** the paged state's `items` now applies the live label-typing
  preview filter, matching design §6.3 — it previously left the page
  unfiltered while typing.
- **N1''-N3'':** `agentsLoading` is ref-counted; the loading indicator is
  gated on `this.agents` being empty (not the phase-filtered view), so a
  lifecycle refresh with an unmatched phase filter no longer flickers; the
  B3' toggle test now asserts the actual alternation and dir value.

Per the EM's instruction, reran all five original review probes (rounds
1-3) against this fix before pushing — every one now shows the corrected
behavior, including round 3's own raw-event-dispatch methodology for the
B2'' race, which is why `onPagerNav` was kept (not deleted) as a second
guard layer. Probes were not committed.

Full suite (`npx vitest run --no-file-parallelism`) after the rebase: all
109 files, 3137 tests passed; the previously-flaky
`agent-create-projects.test.ts` did not reproduce this run either.

## Round 4 review addendum

Review: the round-4 review document (`lists-p1c-rev-4.md`)
(**APPROVE** at `d00261f3`: 0 critical, 0 required, 2 optional (N1''',
N2'''), 2 nits (N3''', N4''')). All six round-3 findings reconfirmed closed
with no regression. Fixed in one commit, rebased onto P1a's latest head
(`44c71d37`); final SHA `e8c8e0747`. Full mapping in the dev report
addendum (`lists-p1c-dev.md`).

Summary:

- **N1''':** added `AgentListWindow.invalidateCursors()`, called when a
  `view-change` trigger's request fails while the window is still `'paged'`
  — `hasNext`/`hasPrev` now report `false` until the next successful
  `setPaged()`. Also fixed `next()`/`prev()` to check the public
  `hasNext`/`hasPrev` getters instead of the private `_hasNext` field/raw
  `_pageIndex` check, since the latter would have bypassed the new
  invalidation for a direct `agentWindow.next()` call.
- **N2''':** the pager's `.loading` binding and `onPagerNav`'s guard now
  only factor in `agentsLoading` while paged, so a held lifecycle refresh
  or page-level load no longer disables small-state local pagination
  (which sends no request and has nothing to race).
- **N3'''/N4''':** reworded the stale `agent-list-window.ts` module doc
  comment (still described pre-round-3 `setSmall()` behavior) and the
  stale `project-detail-agent-window.test.ts` comment claiming `onPagerNav`
  was removed entirely (it is still the defense-in-depth guard).

Reran all probes from rounds 1-4 (24 tests across 5 probe files) twice —
pre-rebase and post-rebase onto P1a's `44c71d37` — all passing both times.
Probes were not committed.

Full suite (`npx vitest run --no-file-parallelism`) after the rebase: all
110 files, 3146 tests passed; the previously-flaky
`agent-create-projects.test.ts` passed this run too.

## Round 5 review addendum

Review: the round-5 review document (`lists-p1c-rev-5.md`)
(**APPROVE** at `facf6486`: 0 critical, 0 required, 2 optional (N1'''',
N2''''), 2 nits). Range-diff confirmed a pure rebase (all 7 round-4
commits patch-identical). N2'''/N3'''/N4''' reconfirmed closed; N1''' was
closed for the non-OK-response path as filed, but two adjacent failure
paths could still replay a stale cursor. Fixed in one commit, rebased onto
P1a's latest head (`987d2969`); final SHA `243bb5bbb`. Full mapping in the
dev report addendum (`lists-p1c-dev.md`).

Summary:

- **N1'''':** hoisted cursor invalidation into one helper,
  `onViewChangeFailed(trigger)`, called from every failure branch of a
  paged `view-change` request — the network-error catch and the non-OK
  branch in `loadAgentsForViewImpl`, plus the matching branches in
  `loadLegacyAgentsImpl` (the 422-then-legacy-failure path round 4 missed).
- **N2'''':** `refresh()` now refetches page 0 (always cursor-free, so it
  can't mismatch) instead of the current page when the cursor stack is
  invalid; a successful page-0 fetch marks the stack valid again. Gives the
  user a way off a stranded page via the resync chip.
- **nit 1/nit 2:** added the missing `hasPrev`-on-page>0 unit test, and
  reworded a misleading test comment ("retrying" implied the same phase;
  it's actually a different one).

Reran all probes from rounds 1-5 (29 tests across 6 probe files) twice —
pre-rebase and post-rebase onto P1a's `987d2969` — all passing both times.
Probes were not committed.

Full suite (`npx vitest run --no-file-parallelism`) after the rebase: all
110 files, 3150 tests passed, including P1a's previously-flaky
`state-compaction-fuzz.test.ts` (now has explicit timeouts upstream) and
the pre-existing `agent-create-projects.test.ts` flake.

Round 6 is the last review round allowed per the EM's instruction.

## Round 6 review addendum

Review: the round-6 (final) review document (`lists-p1c-rev-6.md`)
(**APPROVE** at `4ff62895`: 0 critical, 0 required, 1 optional (N1), 2 nits,
plus an FYI gap on a sibling trigger). Range-diff confirmed the round-5
rebase was pure. All 4 round-5 items (N1''''/N2'''' optional, nit 1/nit 2)
reconfirmed closed. Two commits this round, per the EM's instruction:

- **Commit A (code + tests)**, SHA `49c491fc5`:
  - **N1:** `invalidateCursors()` now bumps `generation` and clears the
    loading flag before notifying, the same way `setPaged`/`setSmall` do.
    Closes a race the round-5 fix introduced: a window fetch already in
    flight when a view-change fails could land afterward and, if it
    happened to be an index-0 fetch, re-validate the cursor stack using a
    cursor minted under the now-stale params. Unit test (deferred
    `fetchPage`, invalidate mid-flight, resolve, assert `hasNext` stays
    false) fails without the fix.
  - **nit 1:** the index-0-revalidation test now gives the refetched page a
    `nextCursor` and asserts `hasNext` becomes `true` and that the next
    navigation uses that fresh cursor, instead of only checking a `false`
    value that held regardless of whether revalidation ran at all.
  - **nit 2:** added a legacy-fallback-rejects-with-a-network-error variant
    of the 422-then-legacy-failure test, covering the one failure branch
    that previously had no test.
  - **FYI (label-commit revert on network error):** hoisted the
    `committedLabel` revert into the same shared failure helper used for
    cursor invalidation (renamed `onViewChangeFailed` to
    `onAgentsLoadFailed`), so a label commit that fails with a network
    error reverts the label the same way a non-OK response already did.
    Component test added that fails without it.
  - Reran all probes from rounds 1-6 (35 tests across 7 probe files)
    against this fix; all pass, including the two R6 cases that
    reproduced the N1 race and the FYI gap before the fix.
- **Commit B (comment-only hygiene)**, the commit immediately on top of A
  (see `git log` on this branch for its SHA): stripped internal
  review/design IDs (round numbers, B/N/R/W/Q/E/D/T
  finding IDs, nit numbers, "P1a FYI", phase labels like P1c/P1a/P5, and
  `gs://` paths) from source comments, test names and assertion messages
  across all 9 touched `web/` files, describing the invariant in plain
  language instead and keeping `§N` design-section references where they
  read naturally. `gs://` paths in this project log were replaced with
  plain file-name references; round/finding IDs were left as-is here,
  since this log is the one place they are still useful shorthand. No
  logic changed in this commit.

Full finding-to-fix-to-test mapping and gate output for commit A in the dev
report (`lists-p1c-dev.md`). Per the EM's instruction this is the final
review round.

## 2026-10-01 — fit threshold lowered from 500 to 50 (interim follow-up)

A profile of the live hub showed the project page's first list request
(fit=500, limit=25) returning all 107 rows with 861 authorization decisions
in about 3.7 s, while a paged limit=25 request took about 1.3 s. On a seeded
500-agent bench hub the fit=500 load took about 21 s versus 3.3 s at fit=50.
The project page now sends fit = max(PROJECT_AGENTS_FIT_THRESHOLD, page size),
with the threshold at 50 (agent-list-window.ts). The max keeps fit at least
the limit for the 100-row page size, which the server requires.

Effect: projects of 50 agents or fewer keep the small state unchanged (one
request on load, client-only interactions free). Larger projects page from
the first request: Next, Prev, a sort-direction change, a phase change, a
label commit and a lifecycle refresh cost one request each, and list to grid
costs one legacy load, which completes the set (up to 500), after which
toggles are free.

Tests: the zero-request small-state test now runs at the threshold (50
agents); new tests cover threshold+1 (paged), 100 agents (exact per-action
counts) and page size 100 (fit raised to 100). The realistic test handler now
reports complete only when fit is sent, as the server does. Mutation check:
with the threshold set back to 500, the 100-agent and page-size tests fail.
vitest: project-detail-agent-window and agent-list-window, 76/76 pass; tsc
clean; eslint reports no errors on changed lines.

2026-10-01 (fit threshold, review follow-up): the 100-agent test's label
commit matched no agents, so its lifecycle refresh ran in the small state. The
fixture agents now carry env=prod, so the label commit keeps the window paged,
and the test asserts the paged state after the phase clear, label commit,
lifecycle refresh and label clear, with exact counts for each. Added a test
for a runtime page-size change while paged (fit=100, limit=100). Corrected the
handler comment about which sorted requests omit fit. vitest 77/77; reverting
the threshold to 500 fails 3 tests.
