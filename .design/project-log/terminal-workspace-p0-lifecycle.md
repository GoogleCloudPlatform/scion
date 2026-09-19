# Terminal workspace P0.2: retained xterm and detach lifecycle

Date: 2026-09-19. Base: `1bb405704a96c2a03f2a1b2b95719edc5d0cda94`.
Fixture scope: executable proof only. The combined candidate also imports the
separately authored, manager-authorized PTY cleanup fix described below; runtime
policy is unchanged. Design: [persistent terminal workspace](../hosted/terminal-workspace.md).

## Findings and contract

1. **Visibility is not lifetime.** A retained xterm can consume and parse raw PTY
   bytes while its host is `hidden`. On reveal it retains its screen, colors and
   scrollback, even with UTF-8 characters and ANSI sequences split across frames.
   Keep one xterm, WebSocket and DOM host until explicit close. The production
   `terminal.ts` still calls `cleanup()` from `disconnectedCallback()`; the spike
   does not make route removal safe or extract production functionality.
2. **Layout is a separate state.** Blur and make the hidden pane inert; keep
   consuming output. Suppress fit/resize while hidden or geometry is zero and
   retain the last nonzero dimensions. After reveal/layout, fit and refresh,
   sending a resize only when dimensions change. Cancel pending debounce work on
   hide/close and check geometry again when it fires. The fixture proves this
   adapter; production's resize observer currently fits without a visibility
   guard, and Hub/broker resize forwarding does not add a zero-size guard.
3. **Close releases one attachment.** Preserve current Ctrl-B/d data followed by
   WebSocket close; make disposal idempotent. The real Hub `PTYSession` forwards
   the bytes, cancels its context, closes/removes its stream, acknowledges browser
   close and emits only one stream-close despite repeated cleanup. The broker
   connection remains registered.
4. **An attachment is not the agent.** The production broker `StreamPTYHandler`
   closes its PTY FD, reaps its runtime exec process and cancels its context. Its
   control-channel cleanup removes the stream and notifies the Hub. Private
   tmux tests verify zero attached clients afterward and the same live pane PID
   through graceful detach, immediate detach-plus-close, abrupt stream close and
   another attach. The pane runs `cat` as an agent surrogate, not an LLM harness.
   These checks do not establish behavior of a remote container init process.
5. **Resize and FD teardown need synchronization.** The focused race run exposed
   `handleResize()` calling `pty.Setsize`/`os.File.Fd` while the PTY reader releases
   and destroys the descriptor during detach-plus-close. The original `Run()` canceled but did
   not join the resize goroutine before its deferred FD close. This is a concrete
   production defect, not a failed assertion in the fixture; the initial race
   trace is preserved in the shared report logs. The manager assigned a separate
   developer to the production fix; this worker retains fixture ownership. That
   exact atomic fix now joins the resize worker after cancellation and before FD
   teardown. Original commit `2c2ac3e7cc97dc03b4b418c86dac4e1d537e1741` maps to
   `cb252a740a4ba88006214a658d218eefcdd6f490` atop fixture head
   `b8da19661001a9215f160afaf936ef27de3abb49`; exact diff-byte equivalence was
   verified. Production changes were not reauthored by this worker.
6. **Do not infer runtime policy from the frontend comment.** The comment in
   `sendTmuxDetach()` says killing an attach would tear down the container. The
   local kernel-PTY/tmux proof shows that killing this attach leaves its pane
   alive; it cannot disprove or establish a runtime-specific container outcome.
   Preserve the detach sequence pending actual runtime evidence.

## Executable evidence

- `web/e2e/terminal-lifecycle/lifecycle.pw.ts`: three real-xterm browser cases.
  Asserts hidden parsing before reveal; UTF-8/ANSI byte boundaries; rendered text
  and red foreground; exact terminal/socket/DOM identity; one socket open; no
  detach on hide/reveal; changed-size-only nonzero resize; 200 lines of retained
  scrollback; repeated zero-geometry layouts; one explicit detach, one socket
  close and one real xterm disposal; no late resize after close.
- `pkg/hub/pty_lifecycle_test.go`: real loopback WebSockets and production
  `PTYSession.Run/Close`, with a control-channel peer. No auth/agent lookup or
  actual broker is involved. Stream ID and detach payload are verified.
- `pkg/runtimebroker/pty_lifecycle_test.go`: production PTY bridge, real kernel
  PTY, real tmux and real loopback control-channel transport. Only the runtime
  exec boundary is adapted. A strict executable accepts only the fixture target
  and dispatches tmux to a private socket under `t.TempDir()`. User tmux config is
  ignored; cleanup kills only that private server. The test skips explicitly if
  tmux is absent. It also observes resize from 80×24 to 100×30 at the tmux client.

The browser fixture is intentionally separate from production extraction.
The browser and Go tests are complementary proofs, not a single full-stack E2E
test. Authentication, route integration, asynchronous generation races, clipboard
policy, multiple concurrent clients and production SSE teardown are later gates.

## Runtime differences and coverage

| Runtime                    | Existing resize/cleanup path                                                                                                                                                                                                 | Evidence here / remaining coverage                                                                                                                                               |
| -------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Docker / Apple `container` | Runtime exec uses `tmux attach-session`; kernel PTY `Setsize` supplies window dimensions. Cleanup closes PTY and kills/reaps the local exec process.                                                                         | Local exec adapter proves the bridge and tmux client lifecycle. Neither runtime CLI/daemon is available; actual container survival and remote exec cleanup are untested.         |
| Kubernetes                 | Go client SPDY exec in the `agent` container with configured exec user; `TerminalSizeQueue` sends initial and subsequent sizes. Cleanup cancels exec context and closes stdin/stdout pipes; no local PTY or kubectl process. | Code inspection only; kubectl is installed but no disposable cluster/pod was supplied or contacted. Existing size-queue unit tests are included in repository CI.                |
| Cloud Run sandbox          | Resize runs sandbox `tmux resize-window -t scion -x/-y` as well as launcher-side `Setsize`, because SIGWINCH does not cross that boundary. Cleanup handles the sandbox exec process/PTY.                                     | Code inspection only; sandbox executable/runtime absent. Explicit session-window resizing may affect other attaches; concurrent-client policy must be tested before changing it. |

No production project agents, containers or tmux sockets were exercised. No
runtime policy was changed. Session-prefix customization and actual remote
runtime survival remain outside this isolated fixture's evidence.

## Verification

All test/build subprocesses remove inherited `SCION_*` variables. The fixture
[README](../../web/e2e/terminal-lifecycle/README.md) supplies a runnable wrapper.
Observed environment: Debian GNU/Linux 12 container, Linux 6.8.0-1064-gcp x86_64,
Node 20.20.2, Go 1.26.1, tmux 3.3a, Playwright 1.62.1, Chromium
152.0.7977.82, xterm 5.5.0, fit addon 0.10.0. Browser tests ran headlessly; they
provide **no real-desktop foreground/focus proof**.

Commands from `web/`:

- `npm run test:e2e -- --config e2e/terminal-lifecycle/playwright.config.ts`
  with `CHROMIUM_EXECUTABLE=/usr/bin/chromium`: **3 passed**. Initial two tests
  were first run against an empty fixture and failed waiting for initial resize,
  then passed with the lifecycle adapter. The third adds scrollback/zero geometry.
- `npm run typecheck -- --project e2e/terminal-lifecycle/tsconfig.json`: **pass**.
- `./node_modules/.bin/eslint 'e2e/terminal-lifecycle/*.ts' --parser-options
'{"project":"./e2e/terminal-lifecycle/tsconfig.json"}' --max-warnings 0`: **pass**.
- `./node_modules/.bin/prettier --check e2e/terminal-lifecycle`: **pass**.
- `npm run typecheck`, `npm run test`, `npm run build`: **pass**; 57 Vitest files,
  1,151 tests. No shared package/config/harness files changed.
- `npm run lint`: **baseline failure**, 806 errors and 2,105 warnings. It lints
  only unchanged `src`, including test files excluded by its configured tsconfig,
  plus existing formatting/type-safety errors. Exact comparison
  `git diff --exit-code 1bb405704a96c2a03f2a1b2b95719edc5d0cda94 -- web/src
web/package.json web/package-lock.json web/tsconfig.json web/.eslintrc.cjs`
  passes with empty diff. `git ls-files --others --exclude-standard web/src`
  returns nothing. This debt was reported to the manager, not fixed in the spike.

Commands from repository root:

- `go test ./pkg/hub ./pkg/runtimebroker -run TestPTYLifecycle -count=1 -v`:
  **pass**, Hub case and broker's four attachment rounds.
- Initial `go test -race ./pkg/hub ./pkg/runtimebroker -run TestPTYLifecycle
-count=1 -v`: **failed before the fix**, production PTY resize/close race in the
  broker's detach-plus-close round; Hub case passed. The trace remains preserved.
- Combined `go test -race ./pkg/hub ./pkg/runtimebroker -run TestPTYLifecycle
-count=30 -v`: **pass**, 30 Hub cases and 120 broker lifecycle rounds, no skips.
- Combined `go test -race ./pkg/runtimebroker -count=1`: **pass**, complete broker
  package under the race detector.
- Combined three browser fixtures and their typecheck, ESLint and Prettier gates:
  **pass** again after the exact fix import. Production web sources are unchanged;
  the earlier full web unit/typecheck/build results above remain applicable.
- Combined `make ci`: **pass** (format, vet, custom checks, all no-SQLite tests, build).
  `make ci-full` was not run; the separate web gates above were run, while the
  additional full-repository golangci-lint gate was not part of this fixture run.

Combined checks ran at `cb252a740a4ba88006214a658d218eefcdd6f490`; the subsequent
fixture-owned commit updates only this findings document. Both the fixture unit
and the mapped fix still require independent review before integration.

Local commits are delivered through a verified shared Git bundle as required by
the brief, with exact head, prerequisites, checksums and logs in the shared P0.2
report. No remote branch is pushed by this worker.
