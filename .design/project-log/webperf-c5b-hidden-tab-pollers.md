# webperf C5b — pause network pollers in hidden tabs (#5350)

Date: 2026-10-19
Base: webperf C5a head 3791729 (stacked; needs `visibleInterval`)
Developer: webperf-c5b-dev

## Summary

Three components polled the hub on a fixed `setInterval` whether or not
anyone could see the tab:

- `pages/chat.ts` fallback poll, 60 s (unread DM state, human space
  membership, hub-member presence resync)
- `pages/health-dashboard.ts` auto-refresh, 30 s
- `shared/notification-tray.ts` fallback poll, 5 min

Each now uses C5a's `visibleInterval(fn, ms)` (catch-up on by default). The
poll stops while `document.hidden` is true. When the tab is shown again, one
catch-up poll runs if a full interval was missed, and then the normal cadence
resumes. Each component keeps the returned stop function and calls it where
it used to call `clearInterval` (disconnect, sign-out, auto-refresh toggle).

User-visible: in a background tab, polled data and the notification badge
update when the user returns, not while the tab is hidden. SSE-driven
updates are unaffected.

## Chat: one duplicate DM request on return (accepted)

Chat's `ResumeRefresh` (a rail refresh after 30 s or more in the
background) is separate from the fallback poll and is unchanged. After at
least 60 s hidden, both run on return, and both load the DM list fresh
(`loadUnreadDMPeers()`; the rail refresh through its `rail-refresh` event),
so the page sends two `GET /api/v1/chat/dms` requests. That duplicate is
accepted. It is still fewer requests than before, when the poll also ran
while hidden.

Sharing one request was tried in review and dropped. The poll asked with
`maxAgeMs` to join the rail refresh's request, which worked only while the
rail refresh started first. C11 (#5317) makes the resume catch-up trailing
(a 250 ms coalescer with `leading: false`, so it starts after an SSE
reconnect), so the poll then fetches first and the rail refresh fetches
again. Joining in the other direction (`startedAfter` in the rail refresh)
would undo the reason C11 made the catch-up trailing.

## Not changed (deliberately)

- Chat's presence heartbeat (60 s, already gated on `document.hasFocus()`)
  and the palette watchdog are not in C5b's scope.
- The initial load on mount still runs in a hidden tab; only the repeating
  poll pauses.

## Tests

New files, so they do not collide with C11's edits to `chat.test.ts` and
`notification-tray.test.ts`:
`chat-fallback-poll-visibility.test.ts`,
`health-dashboard-visibility.test.ts`,
`notification-tray-visibility.test.ts`. Each covers: hidden plus 10 minutes
of fake time gives 0 polls; becoming visible gives exactly 1; started
hidden; normal cadence while visible; no polls after disconnect, even across
a later visibility change. Existing tests that read the old timer fields
(`_fallbackPollInterval`, `pollTimer`) now read the stop functions. The
chat file also keeps the real DM loader in one test: a regular tick fetches
the DM list fresh.

The tests do not depend on how `visibleInterval` builds its timer (C5a's
fix moves it from `setInterval` to a `setTimeout` chain):

- Tests that fake only some timers fake `setTimeout`/`clearTimeout` as well
  (`chat-fallback-poll-visibility`, `notification-tray-user-switch`,
  `tray-count-events`). The last two keep the real `setTimeout` for their
  `flush()` helper.
- `chat.test.ts` counts live polls through a pass-through mock of
  `visibleInterval` (`livePolls`: stop functions handed out and not yet
  called), not live `setInterval` handles.
- The touched suites pass with the current helper and with C5a fix1's.

## Learnings

- The chat page startup awaits dynamic imports and requests, so faking all
  timers stalls it. Faking only `setInterval`, `clearInterval` and `Date`
  (`vi.useFakeTimers({ toFake: [...] })`) lets startup settle and still
  drives the poll and the wall-clock catch-up check.
- The chat poll fans out to several loaders, some through shared caches
  with reuse windows, so counting raw requests is fragile. The test stubs
  the loaders after startup and counts `loadUnreadDMPeers` calls (one per
  poll).
- Check a cross-branch timing assumption against the branch that changes
  it. C11's coalescer defaults to `leading: true`, but the resume catch-up
  sets `leading: false`, and the DM sharing above relied on the default.
