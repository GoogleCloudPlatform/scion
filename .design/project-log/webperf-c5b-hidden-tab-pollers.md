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

## Not changed (deliberately)

- Chat's `ResumeRefresh` (a rail refresh after a long time in the
  background) is separate from the fallback poll and is unchanged. On return,
  both can fire: the rail refresh and the poll catch-up load different data.
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
(`_fallbackPollInterval`, `pollTimer`) now read the stop functions.

## Learnings

- The chat page startup awaits dynamic imports and requests, so faking all
  timers stalls it. Faking only `setInterval`, `clearInterval` and `Date`
  (`vi.useFakeTimers({ toFake: [...] })`) lets startup settle and still
  drives the poll and the wall-clock catch-up check.
- The chat poll fans out to several loaders, some through shared caches
  with reuse windows, so counting raw requests is fragile. The test stubs
  the loaders after startup and counts `loadUnreadDMPeers` calls (one per
  poll).
