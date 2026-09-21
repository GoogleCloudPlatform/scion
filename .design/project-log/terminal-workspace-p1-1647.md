# Terminal workspace P1.2: retained pane extraction

2026-09-19 — Extracted the accepted P1.1 terminal UI into
`web/src/components/terminal/terminal-pane.ts`. The legacy page now supplies its
authenticated account/Hub scope and resolves the agent UUID from its supplied
`pageData.path` snapshot (or explicit `agentId`), then mounts the same pane. The
pane never reads the current route. Registry, transport, router, coordinator and
shared subscription modules are unchanged.

## Ownership and integration contract

- Create one `scion-terminal-pane` per retained session key. Call
  `pane.open(registry, agentUUID)` before or after mounting; it synchronously
  returns `TerminalSession`. Same-pane repeated opens return that session without
  reconnecting. A different UUID/registry cannot rebind the pane; opening a session
  already in the registry rejects instead of creating a second renderer. Reuse
  the original element. Disposing a rejected second pane does not affect its owner.
- `pane.session` and `pane.agentId` are read-only getters. The registry owns the
  transport and the initializer's xterm resource adapter. Initialization checks
  cancellation across imports/layout and cleans up partial allocations on failure.
- `pane.setVisible(boolean)` sets hidden/inert, blurs and cancels pending resize on
  hide, and waits for layout before fitting on reveal. Hidden output continues
  parsing; last nonzero geometry survives. Initial attach waits for measurable
  geometry. Resize callbacks check geometry/visibility at scheduling and send time;
  unchanged geometry is not resent.
- DOM disconnection removes window listeners and cancels resize work but never
  closes a retained session. `pane.dispose()` or `pane.session.close()` closes and
  disposes once, including pending initialization. `dispose('navigation')` exists
  only for the disposable legacy adapter. Retained mode/route changes must not call
  either close/dispose method.
- Existing toolbar, xterm/addons/CSS, OSC 7337, keyboard, selection, clipboard,
  exposed ports, capture-auth and upload code lives in the pane. The port dropdown's
  document listener and pending installation timer now belong to explicit disposal;
  a regression test reproduced the old listener leak before the fix.

## P1.4 handoff and explicit limits

Per-pane SSE is temporarily retained for parity. Its owner is the pane:
`applySessionState` calls private `connectSSE` when the metadata object changes;
`cleanup` calls private `disconnectSSE`. The event handler updates `exposedPorts`,
`agentPhase` and `agentActivity`. `applySessionState` accepts the existing complete
session snapshot and applies `agent` metadata by reference; `refreshAgentData`
updates capture-auth metadata locally. There is no new public metadata-mutation
API in this leaf. P1.4 requires exclusive pane edits to replace this adapter with
workspace-owned subscriptions and agree on the metadata update hook. It must
preserve connection notifications not overwriting capture-auth-refreshed metadata.
No metadata depends on route-cleared global state.

P1.5 owns real SPA roots/mode retention. P1.8 still owns active-pane human input,
global drop scoping, OSC 52 visibility/focus policy and generation checks for
clipboard/upload completions. This leaf preserves the existing handlers; it does
not claim those policies. Native macOS Option/Shift selection, OS clipboard
permissions, headed desktop focus and actual lifecycle freeze were unavailable or
explicitly assigned to later QA. No active agents or real backend were exercised.

## Verification

All test/build children removed inherited `SCION_*` variables. Focused final unit
run: 54 passing (registry, legacy transport adapter, all 22 existing upload cases,
and five pane lifecycle/ownership cases). Full web suite passed 60 files / 1,182
tests before the final dropdown cleanup regression; final focused tests and build
cover that change. Production and supplemental source/test typechecks pass.

New isolated production-pane Playwright fixture: four passing headless Linux
Chromium cases with real xterm/addons/Shoelace, mocked HTTP/WebSocket/EventSource
and clipboard boundaries. Covers hidden bytewise UTF-8/ANSI, ordered 200-line
scrollback, exact terminal/DOM/socket retention through history/layout/remount,
initial hidden geometry, explicit close, OSC window selection, Agent/Shell,
Shift+Enter, selection copy/paste, OSC 52, clickable links, ports, capture-auth
scope/conflict retry and browser multipart drop. The separate original real-xterm
lifecycle fixture also passes all three cases. The browser README records the
platform checklist and limitations.

Production build passes. Root lint remains baseline debt, not green; exact-base
comparison shows five identical production diagnostics and zero introduced errors.
New unit/browser files pass supplemental typed lint. Root ESLint excludes tests
through its tsconfig, so the new test adds another parser-project mismatch there.
No repository lint config was changed. `make ci` / `make ci-full` and real runtime
checks were not repeated for this frontend-only leaf; cumulative CI belongs to
the manager/integration writer.

Durable commands, failures/corrections, exact base/head, reviewed scope and verified
Git bundle are delivered under the externally mounted
`/scion-volumes/scratchpad/projects/terminal-workspace/` in
`reports/p1-1647-developer.md`, `reports/p1-1647-logs/` and
`transfers/p1-1647-candidate.bundle` with its adjacent JSON manifest. No remote Git
operations were performed. Independent review and integration remain manager gates.
