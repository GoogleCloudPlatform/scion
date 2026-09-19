# Production terminal coordinator browser tests

Run from `web` with dependencies installed:

```sh
npm run test:e2e -- --config e2e/terminal-coordinator/playwright.config.ts
```

The runner uses `/usr/bin/chromium` by default; set `TERMINAL_OWNER_CHROMIUM`
for another installed Chromium executable. Remove inherited `SCION_*` variables
from the test process environment. The fixture binds loopback port 4519, bundles
`src/client/terminal-coordinator.ts` with esbuild, and never proxies to a Hub.
Playwright intercepts metadata/preflight HTTP and WebSocket connections. Web Locks,
BroadcastChannel, document lifetime, and multiple pages are real browser APIs.
The actual session registry creates its sessions and WebSockets; renderer resources
and UI selection are minimal injected adapters. These are not live PTY, real-xterm,
or production router integration tests.

Coverage includes simultaneous claims, one initialization for repeated agent opens,
request-ID deduplication/conflicts, generation/agent/request-specific acknowledgments,
late acknowledgments, held-lock pending retries, unsupported APIs and runtime denial,
base-path/account separation, mode-independent ownership, and teardown before release.
The silent external lock holder is a controlled missing-acknowledgment boundary, not
an actual browser lifecycle freeze. Focus denial is injected; a separate test uses
the production focus observation. Headless document focus does not establish desktop
foreground activation. Carry the Phase 0 manual focus matrix to final QA #1662;
actual lifecycle freeze remains P3.3.

## Integration contract

Construct one `TerminalCoordinator(scope, adapter)` per authenticated document from
trusted Hub origin/base path and account identity. It creates the registry internally;
only its owner calls `open` on that registry. `sessions` exposes local retained handles
only to the owner. Do not construct the coordinator from route or channel identity.

`open(agentUUID, requestId?, timeoutMs?)` represents an explicit user open intent.
Reuse its request ID for retries after `pending`; use a fresh ID for a new intent.
The result carries status, request ID, normalized agent UUID, owner generation, and
separate focus observation. `selected` means the selection adapter completed, not
that the PTY handshake succeeded; session `state.connection` remains authoritative.
`focus: 'document-focused'` means a document observation, not guaranteed OS activation.
A `pending` deadline neither cancels the intent nor grants ownership. No automatic
retry, duplicate local attach, owner URL reload, or extension fallback exists.

`adapter.initialize` is the registry's existing resource initializer.
`adapter.select(session, signal)` activates the retained single-pane presentation.
Its signal covers **owner document/account lifetime only**, not requester navigation.
Explicit cross-tab user open intent can survive navigation in the requesting tab.
For local route resolution, P1.5 must separately validate its current navigation
generation/abort signal before committing selection. The adapter must reject a
superseded route selection rather than silently return success, so the coordinator
skips focus. The coordinator's signal alone does not implement navigation cancellation.
There is no per-request cross-tab cancellation protocol in this leaf.

An optional `adapter.focus()` reports observed focus, and must not perform delayed
activation after host cancellation. The default requests window focus and checks
visibility plus `document.hasFocus()`. Failed, denied, or delayed activation returns
`not-confirmed` while preserving the selection acknowledgment.

Keep the coordinator alive while its owner displays Chat or Dashboard. Never call
`stop()` on SPA route/mode changes. `stop()` is document/account teardown: abort
selection, finish pending requests as stopped, close sessions, then release the lock.
A disposal exception deliberately retains authority until document exit. `pagehide`
also stops this instance. BFCache restoration, account-wide broadcasts, ownership
recovery UX, and router/pane integration are later-phase work.
