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
calls `fn` once if a full interval passed since the last tick (judged by
wall clock), then restarts the normal cadence. `stop()` clears the timer and
removes the `visibilitychange` listener; it is idempotent and safe to call
from inside `fn`.

Applied at the render-only sites: `agent-detail.ts`, `brokers.ts`,
`broker-detail.ts`, `skill-detail.ts`, `project-settings.ts` (broker
relative times), `chat-scheduled-list.ts` and `access-boundary-preview.ts`
(expiry countdown). Each component now stores the stop function and calls it
in `disconnectedCallback` (or its existing `cleanup()`); restarts on
reconnect come from the same `connectedCallback` / load path as before.

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
