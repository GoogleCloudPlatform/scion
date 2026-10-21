# webperf C11: coalesce event-driven refetches (#5314)

**Date:** 2026-10-19
**Issue:** ptone/scion#5314 (epic #5301)
**Branch:** scion/webperf-c11

## Problem

Several client handlers refetch a resource once per already-delivered SSE
event (findings S2, S3, S5, S6). A burst of chat or notification events
therefore produced a burst of identical GETs (`/chat/dms`, conversation
history, `/notifications`, project members). Two hand-rolled trailing
debounces (the rail reload after a message, and the members refetch after a
new agent) were reset by every event, so a steady stream starved them.

## What changed

- `web/src/client/coalesced-refresh.ts`: one small helper,
  `coalescedRefresh(fn, {waitMs, maxWaitMs, leading})`. It is a leading +
  trailing throttle with a max wait. The first call in a quiet period runs
  at once, a burst collapses into one trailing run, and `maxWaitMs` bounds
  how long a queued call waits. `fn` receives the newest `Event.timeStamp`,
  so callers can pass `{ startedAfter }` to the shared chat list loads and
  join a request another owner started.
- `chat.ts`: the DM dots refresh only for `dm:` (or keyless) messages,
  coalesced. The rail reload is trailing with a 6 s cap. The members refetch
  for new agents is coalesced per project. The resync and resume catch-ups
  share one coalescer.
- `chat-unread.ts`: the 500 ms badge debounce gets a 4 s max wait.
- `chat-thread.ts`: lightweight-event backfills go through a coalescer in
  front of the single-flight `backfillV2()`. A keyless event routed to
  another project is ignored. Stream and subscription setup are untouched.
- `notification-tray.ts`: a user-scoped event that carries a whole row is
  shown without a request. Anything else is fetched through a coalescer.

## Deviations from the design (agreed with webperf-em / webperf-lead)

- **S5 guard:** the hub's `UserNotificationEvent` has no `acknowledged`
  field. A just-created notification is unacknowledged by definition, so a
  missing field passes and only an explicit `true` is rejected. A
  `subscriberId` for another user is rejected, and nothing is upserted
  before the first fetch has finished (no seen set yet, so push detection
  would be wrong).
- **S6b:** the catch-up coalescer is `leading: false`, not `leading: true`.
  With leading + trailing, a resume followed by a resync costs two refreshes.
  A leading refresh on resume can also start before the feed reconnects and
  miss events published in that gap.

- **S6a:** the members refetch coalescer is `leading: false` (1000/4000).
  The design snippet omits `leading`, and the helper defaults it to true.
  Trailing keeps the old 1 s debounce behaviour and the design's "one
  members fetch" test.
- **S3 keyless other-project filter (new):** a space thread ignores a
  keyless message event whose `projectId` names another project. DM
  threads are exempt: a DM's `projectId` is inherited from the view it was
  opened from (`inheritedProjectId()`), while its messages carry the sender
  agent's project or none.
- **S5 in-flight and stale responses:** list requests are numbered. A
  response keeps rows that an event upserted after that request was sent,
  along with their seen state. A response to an older request that lands
  after a newer one was applied (`appliedSeq`) is ignored. Without these,
  an upserted row could be dropped and then popped a second time.

## Learnings

- Leading-edge throttles change test timing for existing tests that fire
  two events back to back. The tray's user-switch test exposed a real
  issue: a coalescer must be reset on a user change, otherwise the new
  user's first event is queued behind the old user's window.
- `Event.timeStamp` and `chatLoadClock()` share the `performance.now()`
  clock, so passing the newest event time through the coalescer keeps the
  existing `startedAfter` joins working unchanged.
- In zsh, `$F` does not word-split. Use `${=F}` when passing a file list to
  eslint or prettier.
