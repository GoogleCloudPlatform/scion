# webperf C10: keyed agents/projects lists

**Date:** 2026-10-19
**Fork issue:** ptone/scion#5313 (epic #5301)
**Branch:** scion/webperf-c10 (from upstream main 0f5e36d)

## Summary

The `/agents` and `/projects` list pages re-rendered more DOM than they had to
on a live update (webperf finding #13).

- **Keyed rows.** `pages/agents.ts` (grid cards and table rows) and
  `pages/projects.ts` (grid cards and table rows) now render with
  `repeat(items, x => x.id, ...)`, not an unkeyed `.map`. The agents page sorts
  by `updated` by default, so one agent's status update moved that agent to the
  top. Lit then re-filled every row above its old position with a neighbour's
  data. With keys, Lit moves that one row's node and leaves the others alone.
- **`mergeChanged` identity** (`client/agent-merge.ts`). It already returned
  `held` when a flush named nothing applicable. It now also keeps the held
  object, and so the array, when an upsert matches the held object on every
  top-level field. The check is one pass over top-level keys with `Object.is`
  and no deep compare. Here "window" means the coalescing flush window.
- **Projects merge in place.** `onProjectsUpdated` used to respread every
  project and build a new array on each `projects-updated`. It now records which
  store object each row was merged from (`mergedFrom`) and re-merges only the
  projects whose store object changed. When nothing changed, the array keeps its
  identity.

## Learnings

- `state.ts` already drops agent deltas that change nothing against its own
  copy (§7 `agentsShallowEqual`), so such a delta never reaches a flush. The new
  check in `mergeChanged` only matters when the page's held copy differs from
  the store's copy, for example after an optimistic lifecycle patch
  (`applyOptimisticAgents`) or capabilities carried forward. A page-level test
  that sends a no-op status delta proves nothing about `mergeChanged`, so that
  case is covered by unit tests in `agent-merge.test.ts`.
- Project and broker events are still not coalesced to one per frame
  (`state.ts` `handleProjectEvent`), so each project event still runs one
  `onProjectsUpdated`. After this change that pass costs an O(n) reference walk
  and touches only the changed rows. Coalescing is SSE handoff S4 and out of
  scope here.

## Tests

- `agents-live-updates.test.ts`: `agents list keeps row DOM nodes across an
  unrelated agent update` (grid, the design's exact name) and `... in the table
  view`. Agent a3 moves to the top of the sort, and every agent's own DOM node
  stays the same (`toBe`).
- `projects-live-updates.test.ts` (new): in-place merge identity and keyed row
  nodes across a delete, in grid and list views.
- `agent-merge.test.ts`: tests that a content-identical copy is a no-op, that
  real changes still apply (a field change, a new nested reference, a dropped
  key), and the case where capabilities were carried forward.
- All the new tests failed against the pre-change sources (checked by hand).
- Fork CI's Web perf budgets job (`e2e-perf`) covers the domElements and
  longTasks budgets. Playwright was not run locally.
