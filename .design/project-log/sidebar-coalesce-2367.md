# Chat sidebar hub-members load: coalesce and paginate fully (ptone/scion#2367)

**Date:** 2026-10-01
**Issue:** ptone/scion#2367
**Branch:** perf/2367-sidebar-coalesce

## Problem

`chat.ts`'s `loadHubMembers` (the chat members sidebar's hub-level load, used
when no space/project is selected) had two issues:

1. No in-flight coalescing. It is called from at least four places (a route
   parse, `initV2`'s no-conversation branch, a rail-data re-parse, and a
   periodic fallback poll), none of which checked whether a load was already
   running. A cold `/chat` navigation could issue several overlapping
   `/api/v1/users`/`/api/v1/agents` requests.
2. Only the first page. Both lists were fetched with `limit=100` and never
   followed `nextCursor`, so a hub with more than 100 users or agents showed a
   silently truncated member list.

## Approach

### Shared pagination helper

Added `web/src/client/paginate-all.ts`, a generic `paginateAll<T>()` that
walks a `nextCursor`-paginated list endpoint via `apiFetch` until the cursor is
empty (not until a page's `items` are empty — a filtered intermediate page can
legitimately be empty while still carrying a cursor), with a repeated-cursor
guard and a page-count safety bound. This mirrors the existing
`fetchAllPaletteAgents`/`fetchAllPaletteUsers` contract in
`chat-palette-data.ts` (the quick-switcher palette's own full-list loaders),
factored out so other full-list consumers don't hand-roll the same cursor
loop. The palette module itself was not touched — it owns its own loaders and
may adopt this helper separately.

`chat.ts`'s `loadHubMembers` now calls `paginateAll` for both `/api/v1/users`
and `/api/v1/agents` (kept at the same 100-row page size, kept as a parallel
`Promise.allSettled` fetch pair), and only publishes each list once its own
walk completes — no progressive/partial publication.

### Coalescing gate

`loadHubMembers` is now a synchronous, fire-and-forget method (so every
existing `void this.loadHubMembers();` call site is unchanged) backed by a
two-stage gate:

- **Batching window** (`_hubMembersScheduled`): every call that arrives before
  the walk has actually started collapses into a single `queueMicrotask`
  -deferred walk. This covers the common case of several call sites firing in
  the same synchronous turn (e.g. a cold mount's route parse immediately
  followed by `initV2`'s own no-conversation check) with zero extra requests.
- **In-flight flag + trailing reload** (`_hubMembersInFlight`,
  `_hubMembersReloadQueued`): once the walk's network requests are actually
  in flight, a further call sets a single pending-reload flag instead of
  starting a second walk. `_runHubMembersLoad`'s loop performs exactly one
  trailing walk after the current one settles if that flag is set; further
  calls during the trailing walk re-set the same flag rather than queuing a
  second one, so the sidebar is never staler than the latest trigger without
  ever running more than one extra request per settled walk.

### Error handling

A failed walk for one list (e.g. a page request or JSON parse failure) leaves
that list exactly as it was — the other list, fetched in parallel, still
updates independently on its own success. This restores (and extends to the
paginated path) the original method's blanket try/catch, now wrapping the
publish step so an unexpected failure there also can't escape as an unhandled
rejection from the `queueMicrotask`-scheduled call.

## Files changed

| File | Change |
| --- | --- |
| `web/src/client/paginate-all.ts` | New shared `paginateAll<T>()` cursor-pagination helper. |
| `web/src/client/paginate-all.test.ts` | New unit tests for the helper. |
| `web/src/components/pages/chat.ts` | `loadHubMembers` rewritten: coalescing gate, full pagination via `paginateAll`, unchanged call sites. |
| `web/src/components/pages/chat-hub-members-coalesce.test.ts` | New coalescing/pagination/error-preservation tests for `loadHubMembers`. |

## Scenarios covered (tests)

- A cold mount triggering all call sites in one synchronous turn issues
  exactly one users walk and one agents walk.
- A single later trigger during an in-flight walk causes exactly one trailing
  reload; three such triggers still cause only one.
- No trailing reload when nothing new is triggered.
- Over-100-item users and agents lists are walked across every page, with the
  full set shown and exactly one request per page.
- A failed second page (one list) leaves that list's previously loaded
  members untouched while the other list still updates.
- Helper-level: single page, multi-page, empty, mid-walk error, repeated
  cursor, and page-count safety bound.

## Invariants preserved

- Presence merging for users (existing `presenceState` values are kept across
  a reload, since `/api/v1/users` carries no presence of its own).
- `stateManager.seedAgents` baseline seeding on a successful agents walk.
- Legacy `v2Members` (thread @-mention roster), rebuilt from whatever the
  current human/agent lists are after each walk.
- No change to SSE handling or shared-state semantics beyond what
  `loadHubMembers` already seeded.
