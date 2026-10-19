# webperf C5a — pause render-only timers in hidden tabs (#5309)

Date: 2026-10-19
Base: upstream main 0f5e36d
Developer: webperf-c5a-dev

## Summary

Seven web components ran a `setInterval` whose only job is to refresh what
is on screen (relative "x minutes ago" times every 15 s or 30 s, and a 1 s
expiry countdown). Those timers kept firing in hidden tabs, and in pages
kept connected behind `/terminals`, re-rendering for nobody.

Added `web/src/client/visible-interval.ts`:
`visibleInterval(fn, ms, { catchUpOnVisible = true }) => stop`. It clears
the timer while `document.hidden` is true. When the tab is shown again it
calls `fn` once if a tick was missed (judged by wall clock), then resumes
the normal cadence (see the follow-up below). `stop()` clears the timer and
removes the `visibilitychange` listener; it is idempotent and safe to call
from inside `fn`.

Applied at the render-only sites: `agent-detail.ts`, `brokers.ts`,
`broker-detail.ts`, `skill-detail.ts`, `project-settings.ts` (broker
relative times), `chat-scheduled-list.ts` and `access-boundary-preview.ts`
(expiry countdown). Each component now stores the stop function and calls it
in `disconnectedCallback` (or its existing `cleanup()`); restarts on
reconnect come from the same `connectedCallback` / load path as before.

## Follow-up (upstream review)

- `visibleInterval` now chains `setTimeout` against a wall-clock due time
  instead of using `setInterval`. A hide shorter than one interval resumes
  with the time left, so switching tabs faster than `ms` still ticks.
  Lateness does not add up from tick to tick, and after a catch-up the next
  tick keeps the original cadence. With `catchUpOnVisible: false` a missed
  tick starts a fresh full interval on show. If `fn` throws, the chain
  keeps going, as `setInterval` did.
- `access-boundary-preview` starts no countdown for a preview that has
  already expired.
- Wall clock (`Date.now()`) rather than `performance.now()`, which can stop
  while a device sleeps (the same reason as `resume-refresh.ts`). If the
  wall clock steps backwards, the next tick is clamped to at most one
  interval away.
- Test suites that fake only `setInterval` no longer drive `visibleInterval`;
  they need `setTimeout`/`clearTimeout` faked too.

## Not changed (deliberately)

Network pollers (`chat.ts` fallback poll, `health-dashboard.ts`,
`notification-tray.ts`) are C5b, which changes visible behaviour and needs
separate approval. Other intervals (`admin-*` polls, SSE stale check,
terminal heartbeat, chat palette watchdog, onboarding) are not render-only.

## Learnings

- `access-boundary-preview` stops its own timer from inside the tick when
  the preview expires. With a catch-up tick fired from the visibility
  handler, the helper must not restart the timer after `fn` has called
  `stop()`; a test covers this.
- vitest fake timers fake `Date` too, so a wall-clock "missed a tick" check
  is testable with `vi.advanceTimersByTime`. Spy on the `document.hidden`
  getter and dispatch `visibilitychange` to simulate tab switches in
  happy-dom.
- Acceptance grep: `rg "setInterval\(\(\) => this.requestUpdate" web/src`
  returns nothing.
