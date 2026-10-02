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

## Cold-mount duplicate walks and the view-change race (2026-10-01)

A connected-element repro showed a real cold `/chat` mount still ran two full
users/agents walks, not one, and a trailing walk could land after the user had
already navigated to a project or DM, overwriting that view's member list with
every user and agent in the hub. Fixes, each with a new test:

- **Join vs. refresh.** `loadHubMembers` now takes an optional
  `{ refresh?: boolean }`. Route/view re-parses (the route parse, `initV2`'s
  no-conversation branch, the rail-data re-parse, `handleResetView`) pass
  nothing and just join a walk already in flight, since none of them know of
  anything that could have changed since it started. Only the periodic
  fallback poll passes `{ refresh: true }`, since it exists specifically
  because the list might have changed — only that caller queues a trailing
  walk. This is what makes a real cold mount settle on exactly one walk per
  list instead of two.
- **View-change race.** A generation counter (`_hubMembersGeneration`, bumped
  on `disconnectedCallback`, same pattern as the existing
  `_unreadDMRequestId`) is captured at the start of each walk. Before
  publishing, and before looping for a trailing walk, the walk checks that
  counter plus `this.v2Conversation` — if a specific conversation has opened,
  or the element has disconnected, since the walk started, it skips
  publishing (and skips starting a trailing walk) rather than overwriting a
  project's or DM's member list, or the shared `v2Members` roster, with
  hub-wide data for a view that is no longer on screen.
- **User de-duplication.** `/api/v1/users` paginates by creation-time offset
  rather than a keyset cursor, so a signup or deletion mid-walk can shift page
  boundaries and return the same user on two pages (agents use a keyset
  cursor and are unaffected). `loadHubMembers` now de-dupes the walked users
  list by id before publishing.
- Removed an unused `signal`/abort option from `paginate-all.ts` (no caller
  passed one) and the fork-tracker issue reference from source comments
  (`ptone/scion#2367` doesn't resolve once this lands upstream); the tracker
  reference stays in this log entry and the branch/commit metadata per the
  project-log convention.
- Not changed: the quick-switcher palette's own loaders
  (`fetchAllPaletteAgents`/`fetchAllPaletteUsers` in `chat-palette-data.ts`)
  still hand-roll their own cursor walk — a reasonable follow-up is to move
  them onto `paginateAll` for consistency, but that file belongs to a
  different ownership split and was out of scope here. Also not changed: the
  sidebar still publishes each list only after its full walk completes, so a
  very large hub's first paint is `pages x page-time` rather than one page —
  this is the brief's intended behavior, not a regression, and progressive
  first-page publication is a possible future follow-up if that latency
  becomes a problem in practice.

## Reconnect handling, mid-walk cancellation, and consistency fixes (2026-10-02)

Two further gaps found after the walk above landed:

- **Stale walk on reconnect.** If the chat page disconnects and reconnects
  while a hub-members walk is still in flight, the reconnected view's own
  `loadHubMembers` call would just join the in-flight walk rather than
  starting its own — but that walk belongs to the old generation and fails
  its own generation check on completion without publishing anything,
  leaving the sidebar empty until the next fallback poll (latent today, since
  nothing currently reconnects the element, but reachable and worth closing).
  `_hubMembersInFlight` now has a paired `_hubMembersInFlightGeneration`: a
  caller only joins an in-flight walk if it belongs to the caller's current
  generation, otherwise it starts a fresh one. A walk only clears
  `_hubMembersInFlight` in its `finally` block if it is still the generation
  that owns it, so a stale walk settling after a reconnect can't clear the
  flag out from under the walk the reconnect started.
- **Unbounded walk after navigating away.** A walk whose view had already
  gone away (disconnect, or a project/DM opened) still fetched every
  remaining page before its result was discarded at publish time — a full
  `/api/v1/agents` walk alone is on the order of 18 seconds of server time,
  bounded only by the page-count safety net. `paginateAll` now takes an
  optional `shouldContinue` callback, checked before every page including the
  first; `chat.ts` passes one that mirrors the existing publish-time check
  (no open conversation, same generation), so a stale walk stops requesting
  further pages as soon as the view it was for is gone, instead of running to
  completion for no reason. (A `signal`/abort option was removed from
  `paginate-all.ts` earlier as unused; this reintroduces an
  equivalent `shouldContinue` option now that there is a real caller for it.)

Also, as consistency/hygiene fixes: `_fetchHubMembersOnce` now takes its
generation as a parameter from `_runHubMembersLoad` instead of re-reading
`_hubMembersGeneration` separately, so both always agree on which walk they
belong to; the coalescing-gate doc comment was reworded to describe the join
path rather than implying every call site fires in the same synchronous
turn; and the cold-mount test now sets `pageData` before appending the
element, matching the router's own order (`main.ts`'s route rendering sets
`pageData` before inserting the page into the shell).

**Known limitation, tracked separately:** `loadV2Members` (the per-project/DM
member load) has no equivalent view guard — a late-arriving project walk can
still overwrite a different, newer view. This already exists on `main` and is
tracked as ptone/scion#2564; out of scope here.

### Files changed

| File | Change |
| --- | --- |
| `web/src/client/paginate-all.ts` | Added a `shouldContinue` option, checked before every page. |
| `web/src/client/paginate-all.test.ts` | New tests for `shouldContinue`. |
| `web/src/components/pages/chat.ts` | Generation-owned in-flight tracking for reconnect; `shouldContinue` wired into both pagination walks; `_fetchHubMembersOnce` takes `generation` as a parameter; doc-comment reword. |
| `web/src/components/pages/chat-hub-members-coalesce.test.ts` | New reconnect and mid-walk-cancellation tests; cold-mount test reordered to match the router's `pageData` timing. |

### Scenarios covered (tests)

- Reconnecting while a walk is in flight starts a fresh walk for the new view;
  the stale walk completing afterward does not publish over it.
- A walk stops requesting further pages once the element disconnects
  mid-walk.
- A walk stops requesting further pages once a conversation opens mid-walk.
- `paginateAll` stops before fetching a page once `shouldContinue` returns
  false, returning what it already fetched rather than throwing.

## Open-then-close mid-walk publishing a truncated list (2026-10-02)

A further gap in the `shouldContinue`/publish-time guard: both
only read the *current* value of `v2Conversation`. If a conversation opened
mid-walk (stopping that walk's users leg, say, at a genuine page boundary —
the users leg resolves with only the pages fetched so far, by design) and the
user then returned to the hub-wide `/chat` view before the walk's slower
agents leg settled, `v2Conversation` was clear again by the time the walk's
`Promise.allSettled` resolved. The publish guard saw no open conversation and
no generation change (the generation only moved on `disconnectedCallback`,
not on opening a conversation) and published the truncated users list as the
full hub roster. Returning to `/chat` also called `loadHubMembers` again, but
since the stale walk was still (wrongly) considered current for that
generation, the call joined it instead of starting a fresh one, so nothing
corrected the truncated result until the next periodic poll.

Fix: `_hubMembersGeneration` is now also bumped whenever `v2Conversation` is
assigned a truthy value, from the single centralized `updated()` handler for
`v2Conversation` changes (the same spot that already reports conversation
changes for desktop notifications) rather than from each of the many
assignment sites — including ones that are not "opening a
conversation" in the user-facing sense, such as a mute toggle or a
default-agent edit on the conversation already open. A walk started before
that bump is stale by generation once `updated()` has run and observed the
open, regardless of what `v2Conversation` itself reads by the time the
walk's promises settle — so the publish guard discards its result, and
`loadHubMembers`'s in-flight check (already generation-aware from the
reconnect handling) starts a fresh walk instead of joining the
stale one, which then publishes the complete list. This covers a
conversation that opens and later closes in two separate Lit update
batches; a conversation opened and closed again within one batch is a
separate gap, closed independently rather than by generation — see
"Open-then-close within one Lit update batch" below.

The existing `this.v2Conversation` checks in `shouldContinue` and the publish
guard were kept rather than removed in favor of the generation alone: they
are a synchronous field read that reflects a conversation opening the instant
it is assigned, stopping a page fetch or a publish immediately, whereas the
generation bump runs from `updated()`, deferred to Lit's own update cycle.
Relying on the generation alone would make the "stop immediately" behavior
depend on the relative ordering of two independent microtask queues (Lit's
scheduler and the fetch promise chain) instead of a direct state check. Both
checks are necessary; neither subsumes the other.

**Known limitation, still tracked separately:** `loadV2Members` has no
equivalent view guard at all (see ptone/scion#2564, noted above) — unchanged
by this change.

### Files changed

| File | Change |
| --- | --- |
| `web/src/components/pages/chat.ts` | `updated()` now bumps `_hubMembersGeneration` when a conversation opens; doc comments on `_hubMembersGeneration`, the coalescing gate, `loadHubMembers`, and `_fetchHubMembersOnce` updated to describe the fix and why the existing `v2Conversation` checks were kept alongside it. |
| `web/src/components/pages/chat-hub-members-coalesce.test.ts` | New regression test driving the real `loadHubMembers` path for open-then-close mid-walk; new test strengthening `_runHubMembersLoad`'s `finally` ownership check (a stale walk settling after reconnect must not clear the in-flight flag out from under a still-running fresh walk). |

### Scenarios covered (tests)

- Opening then closing a conversation mid-walk does not publish a truncated
  list; a fresh walk started on return to `/chat` publishes the full one, with
  no extra requests beyond the stale walk's partial fetch and the fresh
  walk's full one.
- A stale walk settling after a disconnect/reconnect, while the fresh walk it
  was superseded by is still in flight, does not clear the shared in-flight
  flag out from under that fresh walk — a later call joins the fresh walk
  rather than starting a redundant third one.

## Open-then-close within one Lit update batch (2026-10-02)

The generation bump runs from `updated()`, which only sees
the *final* value of a batch of `v2Conversation` writes. If a conversation
is opened and then cleared again before Lit has run that update cycle —
possible when both writes land in the same microtask drain — `updated()`
never observes the open at all, so `_hubMembersGeneration` never moves.
`shouldContinue`, a live field read, still observes the conversation as open
for the window between the two writes and stops a page fetch there — a
genuine, intentional truncation — but with no generation bump to mark the
walk stale, the publish guard at the end of the walk saw the (by-then-clear)
`v2Conversation` and the unmoved generation and published the truncated list
as the full hub roster; a join arriving in the same window, believing the
in-flight walk would still produce a usable result, did not start its own
walk either.

Fix: `paginateAll` now rejects with `PaginationStoppedError` when
`shouldContinue` returns false, instead of resolving with the partial list
it had accumulated. `_fetchHubMembersOnce`'s publish logic already only acts
on each leg's `Promise.allSettled` `'fulfilled'` result, so a stopped leg is
never published, independent of the generation or `v2Conversation` at
publish time. `_fetchHubMembersOnce` returns whether a leg of its attempt
stopped this way (a per-attempt value, see "Per-attempt stopped result"
below); the publish guard also bails out on it for *both* legs of the
attempt, not only the one that stopped — otherwise a single-page
leg whose own `shouldContinue` check had already passed before the
conversation opened would complete normally and publish once
`Promise.allSettled` resolves. That leg's data is for the hub view still on
screen, but the attempt is re-run regardless, so publishing it would publish
the same list twice, from two different attempts; skipping it keeps each
publish to a single attempt. `_runHubMembersLoad`'s loop treats
that result the same as a queued refresh and runs the walk once more, so a
caller that joined the walk still ends up with a complete list instead of a
stale one that never gets corrected until the next periodic poll. This
closes the gap without depending on the relative timing of Lit's update
scheduler and the fetch promise chain — the stale walk's generation-based
invalidation and the new stopped-leg handling are independent and cover
different windows: the former for a walk whose legs complete normally (no
page ever observes the conversation as open) while a conversation opens and
later closes in two separate update batches; the latter for a page that
does observe it, in either one batch or two.

### Disconnect racing a just-scheduled walk

`loadHubMembers` coalesces same-turn callers behind a `queueMicrotask` before
the actual walk starts. If the walk read `_hubMembersGeneration` only once that
queued callback ran, rather than when `loadHubMembers` scheduled it, a
`disconnectedCallback` landing in between — after the call
that scheduled the walk, before its queued callback runs — bumps the
generation first, so the walk would read the *post-disconnect* value as its
own, making it believe it was the legitimate walk for the current
generation and letting it fetch and publish into a page that is no longer
connected. Fix: `loadHubMembers` now captures the generation at schedule
time and passes it through to the walk, so a disconnect in that window
correctly leaves the walk stale before it ever issues a request.

### Files changed

| File | Change |
| --- | --- |
| `web/src/client/paginate-all.ts` | `shouldContinue` returning false now rejects with a new `PaginationStoppedError` (carrying the partial list) instead of resolving with it. |
| `web/src/client/paginate-all.test.ts` | Updated the two `shouldContinue`-stops-the-walk tests for the rejection; added coverage for the partial list attached to the rejection. |
| `web/src/components/pages/chat.ts` | `_fetchHubMembersOnce` records when either leg rejects with `PaginationStoppedError`, and its publish guard also bails out on it — for *both* legs, not only the one that stopped, so a single-page leg whose own `shouldContinue` check already passed before the conversation opened doesn't publish the same list twice, from two different attempts (its data is for the hub view still on screen, and the re-run publishes both lists); `_runHubMembersLoad`'s loop re-runs on it the same as a queued refresh; `loadHubMembers` captures the generation at schedule time rather than reading it inside the queued callback; doc comments on `_hubMembersGeneration`, `loadHubMembers`, `_runHubMembersLoad`, and `_fetchHubMembersOnce` updated to describe the fix and reworded to say the generation bumps on any truthy `v2Conversation` assignment, not only on "opening a conversation". |
| `web/src/components/pages/chat-hub-members-coalesce.test.ts` | New regression test reproducing the same-Lit-batch open/close truncation and asserting the full list publishes with the expected request count; new regression test isolating the generation check from the stopped-leg handling (both legs single-page, so neither ever stops, yet a conversation opening and closing in two separate update batches must still discard the stale result); new regression test for a walk scheduled just before a disconnect in the same microtask drain, asserting it issues no requests. |

### Scenarios covered (tests)

- Opening and closing a conversation within one Lit update batch, while a
  page fetch is in flight, does not publish a truncated list — the walk
  re-runs itself and the sidebar ends up with the full one, with the
  expected total request count.
- A walk whose legs are both already on their one-and-only (single) page
  when a conversation opens and later closes again, in two separate update
  batches, does not let that stale, fully-resolved result overwrite a fresh
  walk's — exercising the generation check alone, independent of the
  stopped-leg handling above.
- `paginateAll` rejects with `PaginationStoppedError` (carrying whatever it
  had accumulated) rather than resolving, both when `shouldContinue` stops a
  walk already in progress and when it is already false before the first
  page.
- A walk scheduled via `loadHubMembers` immediately before a disconnect in
  the same microtask drain issues no requests and publishes nothing into the
  disconnected page.

## Per-attempt stopped result (2026-10-02)

The stopped state is now a per-attempt value: `_fetchHubMembersOnce` returns
whether either leg of its own attempt rejected with `PaginationStoppedError`,
and `_runHubMembersLoad` uses that return value for its re-run decision. With
a shared field instead, a superseded walk stopping at its next page boundary
(after a conversation opened and closed, or a disconnect and reconnect) could
set the field while a fresh walk for the current generation was in flight;
the fresh walk then discarded its complete result at the publish guard and
walked both lists again (three users and three agents requests instead of two
and two in the single-page repro).

- The join path in `loadHubMembers` only queues a trailing walk for
  `{ refresh: true }`. A stopped attempt re-runs on its own, so a join does
  not need to force one.
- The publish guard still skips a completed leg from an attempt whose other
  leg stopped. That leg's data is for the hub view still on screen, but the
  attempt is re-run regardless; skipping it avoids publishing that list
  twice, with the two lists coming from different attempts in between.
- The `loadHubMembers` doc notes that, because the generation is captured at
  schedule time, a synchronous call landing after a disconnect in the same
  microtask drain coalesces into the already-scheduled (pre-disconnect) walk
  and is dropped with it; the reconnect path reaches `loadHubMembers` only
  after `initV2` awaits its lazy imports, so this window is not reachable as
  the code stands.

### Request counts per trigger

Two-page users and agents lists, first users request held so the trigger lands
mid-walk. Counts are users/agents requests; the same scenarios were run
against the branch as it stood before an early stop rejected instead of
resolving, for comparison.

| Trigger | Before early stops rejected | Now | Published lists |
| --- | --- | --- | --- |
| Cold mount | 2/2 | 2/2 | full |
| Open a conversation, return to `/chat` (separate update batches) | 3/4 | 3/4 | full |
| Open and close within one update batch | 1/2 | 3/4 | full now; truncated users list before early stops rejected |
| Disconnect and reconnect | 3/4 | 3/4 | full |
| Fallback poll, three refresh calls | 4/4 | 4/4 | full (one trailing walk) |

The one difference is the same-batch case: before early stops rejected, the stopped users leg
resolved with its partial list and was published; now it rejects, and the
attempt re-runs once (one users page from the stopped attempt plus two pages
each from the re-run).

### Files changed

| File | Change |
| --- | --- |
| `web/src/components/pages/chat.ts` | `_fetchHubMembersOnce` returns its attempt's stopped result; `_runHubMembersLoad` uses it; shared stopped field and the join-path clause removed; doc comments updated. |
| `web/src/components/pages/chat-hub-members-coalesce.test.ts` | Tests for a superseded walk stopping after open/close and after disconnect/reconnect (fresh walk stays at one request per list and publishes); a completed leg is not published from a stopped attempt; a per-trigger request-count table; comments describe current behaviour. |

## Per-page request timeout in paginateAll (2026-10-02)

### Problem

The hub-members load allows one walk in flight per view: other callers join
it, and the fallback poll only queues a trailing reload behind it. Neither
`paginateAll` nor `apiFetch` applied a timeout, so a page request (or its
body read) that never settled kept the in-flight marker set indefinitely,
and the members list stopped refreshing until the view changed.

### Change

`paginateAll` gives each page its own `AbortController` and timer, covering
both the `apiFetch` call (which passes the signal through to `fetch`) and
the `res.json()` body read. A new optional `pageTimeoutMs` option sets the
limit, defaulting to 60000 ms (well above the 17-19 s the agents list
measured on the server). When the timer fires, the request is aborted, the
resulting rejection is reported as a `PaginationError` naming the list and
saying it timed out, and the walk rejects. The timer is cleared in a
`finally` once each page settles, so no timer is left pending. Other
failures keep their existing errors and messages: a non-OK status, invalid
JSON and a non-object body are reported as before, and a network error from
`apiFetch` is still rethrown unchanged. There is no retry, and
`shouldContinue`, `maxPages` and the repeated-cursor check are unchanged.

`chat.ts` is unchanged. A timed-out leg already rejects like any other
failed walk, so `_fetchHubMembersOnce` keeps that list as it was and still
publishes the other list, and `_runHubMembersLoad` clears the in-flight
marker in its `finally`. The next fallback poll then starts a fresh walk.

The timeout relies on `fetch` rejecting a pending request or body read
when its signal is aborted, instead of wrapping each page in a
`Promise.race`. A race wrapper adds microtask hops between a page settling
and the next `shouldContinue` check. The same-batch open/close tests depend
on the exact number of hops, so the wrapper would break them.

### Files changed

| File | Change |
| --- | --- |
| `web/src/client/paginate-all.ts` | `pageTimeoutMs` option; per-page abort signal and timer around the request and body read; timeout reported as `PaginationError`; header and doc comments updated. |
| `web/src/client/paginate-all.test.ts` | A stalled request and a stalled body read each reject with `PaginationError` after the timeout and abort the signal; the 60000 ms default; a normal multi-page walk and non-timeout failures leave no pending timer and keep their existing errors. |
| `web/src/components/pages/chat-hub-members-coalesce.test.ts` | A stalled users or agents page: the walk times out, the in-flight marker clears, the stalled list keeps its previous contents, the other list publishes, and the next poll issues fresh requests (exact counts) and publishes. |
