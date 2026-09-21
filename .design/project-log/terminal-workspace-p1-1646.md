# Terminal workspace P1.1: session registry and PTY transport (#1646)

Implemented from accepted Phase 0 base `0680bd90f915e7b634331bd5e433952571724d62`.

`TerminalSessionRegistry` registers an agent UUID synchronously before metadata,
preflight or renderer work. Keys include trusted Hub origin/base path and the
bootstrap-authenticated account. Repeated opens preserve the original initializer
and return the existing record without reconnecting it. WebSockets, abort
controllers, pending promises and renderer handles remain private.

`connect()` shares pending setup work and resolves after socket setup, **not**
after a successful handshake. Consumers observe the immediately delivered state
subscription; only `state.connection === 'connected'` means transport establishment.
Connection state remains separate from the Agent metadata's phase/activity.
An explicit reconnect repeats metadata and authorization. It retains the renderer
through failed authorization or handshake and invokes `TerminalResources.reset()`
only on the current socket's successful open. Old callbacks cannot write or reset
replacement sessions. Input is sent immediately or dropped, never replayed.

The resource initializer receives an AbortSignal and returns `write`, `size`,
`reset`, and `dispose`. It must guard asynchronous UI work and clean up partially
allocated resources on cancellation; late returned adapters are disposed by the
session. `close()` aborts the generation and disposes once. Explicit disposal
preserves Ctrl-B d followed by socket close. Only the disposable legacy page uses
`close('navigation')`, which closes without sending detach. Future retained hosts
must not close sessions merely because navigation hides them.

The legacy terminal page delegates metadata/preflight/transport to this interface
while keeping its existing xterm controls, upload behavior, SSE, and toolbar.
Authenticated bootstrap identity is required; route names/slugs cannot establish
session identity. The page uses Vite's trusted deployment base URL for the Hub
scope. Pane, coordinator, route retention and aggregated subscriptions remain
later leaves. Registry `list()` and per-session subscriptions are available;
collection notifications are a documented P1.4 extension point, not implemented.

## Validation

- 48 focused tests pass: registry, legacy transport adapter, and unchanged upload
  coverage. Tests cover pending deduplication, fetch/body/preflight/resource/frame
  cancellation, close/reopen races, stale callbacks, ordered bytes and resize,
  rejected input, disposal once, failed reconnect retention, and reset-on-open.
- Repository `make ci` passes. Web typecheck and production build pass.
- Full web suite passes with `--maxWorkers=2`: 59 files, 1,176 tests. One subsequent
  page metadata regression was added and passes in the final 48-test focused run.
  The initial unrestricted run timed out two unrelated import hooks; its logs and
  the passing limited-worker recheck/full run are retained.
- New files pass typed ESLint with a supplemental external test tsconfig (the
  repository tsconfig excludes tests). The repository lint config reports five
  existing terminal-page errors versus six at the exact base, with no introduced
  production errors. Root lint is not green; the initial run reported 806 errors
  and 2,105 warnings across existing debt and the test-config mismatch.
- Every test/build child environment removes inherited `SCION_*`. No active
  agents were used as fixtures. Headed/browser integration and cumulative
  `make ci-full` remain the phase manager's gates.

Evidence, exact commands, candidate review status and bundle provenance live in
`/scion-volumes/scratchpad/projects/terminal-workspace/reports/p1-1646-developer.md`
and its `p1-1646-logs/` directory. Delivery is a verified shared-volume Git bundle;
this leaf does not push, merge, rebase, open PRs, or change sibling modules.
