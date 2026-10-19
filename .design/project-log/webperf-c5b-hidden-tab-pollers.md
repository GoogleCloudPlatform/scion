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

## Chat: the resume refresh and the poll catch-up share one DM request

- Chat's `ResumeRefresh` (a rail refresh after 30 s or more in the
  background) is separate from the fallback poll and is unchanged. After at
  least 60 s hidden, both fire on return, and both load the DM list
  (`loadUnreadDMPeers()`; the rail refresh through its `rail-refresh`
  event). To avoid a second `GET /api/v1/chat/dms`, the poll now asks with
  `maxAgeMs: FALLBACK_POLL_DM_REUSE_MS` (5 s), so it joins the request the
  rail refresh has just started. The rail refresh still loads fresh. This
  depends on order: the `ResumeRefresh` listener is registered before the
  poll's, and the rail refresh dispatches `rail-refresh` synchronously
  (C11's leading-edge coalescer keeps that true). A regular tick finds no
  request that recent and fetches fresh, as before.

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
chat file also keeps the real DM loader in two tests: a regular tick fetches
fresh, and a return after more than 60 s hidden sends one `/chat/dms`
request although both the resume refresh and the catch-up ask for it.

## Learnings

- The chat page startup awaits dynamic imports and requests, so faking all
  timers stalls it. Faking only `setInterval`, `clearInterval`, `Date` and `performance`
  (`vi.useFakeTimers({ toFake: [...] })`) lets startup settle and still
  drives the poll and the wall-clock catch-up check.
- The chat poll fans out to several loaders, some through shared caches
  with reuse windows, so counting raw requests is fragile. The test stubs
  the loaders after startup and counts `loadUnreadDMPeers` calls (one per
  poll).
- `SharedJsonLoad` measures `maxAgeMs` on `performance.now()`, so the test
  also fakes `performance`; otherwise two fake 60 s ticks are microseconds
  apart on that clock and the second reuses the first.
