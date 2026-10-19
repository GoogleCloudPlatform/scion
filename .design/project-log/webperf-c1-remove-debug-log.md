# webperf C1: remove the always-on debug log (ptone/scion#5349)

Base: C7 head `2c4dcfd` (stacked on #5324, on #5321, on #5318; none upstream
yet). Part of the webperf epic (#5301). Client only; no hub change.

## What changed

- Deleted `web/src/client/debug-log.ts` and dropped its import and the
  `debugLog.attach(stateManager)` call from `main.ts`. The module listened to
  every SSE update and every `agents-updated` / `projects-updated` /
  `brokers-updated`, and on each of the latter called
  `stateManager.getStateSnapshot()`: an O(N) copy of every agent, project and
  broker per flush, kept in a 500-entry ring buffer. Nothing read it: there was
  no UI, no `window` hook and no other importer (the `debug-panel.ts` named in
  `web/AGENTS.md` no longer exists). Approved option: delete, not
  experiment-gate.
- Remaining `debug-log` mentions are in older project-log entries
  (`slow-list-lists-p1a/p4-2385.md`); they are history and stay unchanged.

## Test

- `state-snapshot-free.test.ts`, `stateManager agents-updated does not call
getStateSnapshot`: boots the real entry module (`__fixtures__/main-boot.ts`),
  spies on the shared `stateManager.getStateSnapshot`, then feeds 100 agent
  status updates through `handleUpdate`, flushing each, and asserts 100
  `agents-updated` events and 0 snapshot calls.
- It boots `main.ts` on purpose. A test on a fresh `StateManager` would pass
  with or without the debug log, because the cost came from a listener
  `main.ts` attached. Run before the deletion, the test failed with 101 calls
  (the seed flush plus the 100 updates).
- It lives in its own file because `main.ts` boots once per file and leaves
  global stubs and listeners behind, which would leak into the
  fresh-instance tests in `state.test.ts`.

## Numbers (vite build, gzip level 6)

|                       | C7 head | C1             |
| --------------------- | ------- | -------------- |
| `assets/main.js` raw  | 217084  | 215461 (-1623) |
| `assets/main.js` gzip | 58522   | 58091 (-431)   |
| entry closure gzip    | 135683  | 135252 (-431)  |

The main win is runtime, not bytes: no full-state copy per agent flush.
