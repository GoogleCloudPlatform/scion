# Retained terminal lifecycle proof (P0.2)

This isolated fixture uses the installed real xterm parser, DOM renderer and fit
addon with Playwright-mocked PTY WebSocket frames. It demonstrates the proposed
retained lifecycle; it does **not** mount or change the production terminal page,
router, session registry, auth flow or agent runtime. The current production page
still detaches and disposes when removed.

## Run

From `web/`, after `npm ci`:

```sh
# Remove inherited orchestration context from child processes.
without_scion() {
  python3 -c 'import os,sys; os.execvpe(sys.argv[1],sys.argv[1:],{k:v for k,v in os.environ.items() if not k.startswith("SCION_")})' "$@"
}

# Omit CHROMIUM_EXECUTABLE to use the installed Playwright browser instead.
CHROMIUM_EXECUTABLE=/usr/bin/chromium without_scion npm run test:e2e -- \
  --config e2e/terminal-lifecycle/playwright.config.ts
without_scion npm run typecheck -- --project e2e/terminal-lifecycle/tsconfig.json
without_scion ./node_modules/.bin/eslint 'e2e/terminal-lifecycle/*.ts' \
  --parser-options '{"project":"./e2e/terminal-lifecycle/tsconfig.json"}' --max-warnings 0
without_scion ./node_modules/.bin/prettier --check e2e/terminal-lifecycle
```

The task-local config starts Vite on loopback port 4527, refuses an occupied port,
and runs `*.pw.ts`. The root `*.spec.ts` Hub suite does not discover these tests.
No Hub server, project credentials, container or active agent is used. Test output
goes to the existing ignored `web/test-results/terminal-lifecycle/` directory.

The separate `tsconfig.json` is necessary because root typecheck excludes E2E.
The explicit ESLint command applies the repository rules with that project file.

## Assertions

- Hidden output is parsed before reveal. Each byte of UTF-8 text and each byte of
  ANSI escape sequences arrives in a separate base64 PTY frame. Both buffer text
  and rendered DOM are checked after reveal, along with ANSI foreground color.
- Hide/reveal retains the exact terminal, socket and terminal DOM element, with
  one WebSocket open and no detach bytes. Zero geometry preserves the last valid
  terminal dimensions; a changed visible size sends one nonzero resize.
- Two hundred hidden lines remain in ordered scrollback through repeated layout
  changes. Tests wait past the resize debounce to expose late resize work.
- Explicit close sends the existing Ctrl-B/d sequence, closes the socket once,
  disposes the real xterm once, removes its DOM and cancels pending resize work.

The fixture's visibility/disposal adapter is proposed behavior, not production
coverage. It must be replaced by or pointed at the extracted production lifecycle
in a later phase. Browser mocks prove the messages sent, not server cleanup.

From the repository root, the complementary production Hub and broker tests run
with the same `without_scion` wrapper:

```sh
without_scion go test ./pkg/hub ./pkg/runtimebroker -run TestPTYLifecycle -count=1 -v
without_scion go test -race ./pkg/hub ./pkg/runtimebroker -run TestPTYLifecycle -count=1 -v
```

The broker test needs `tmux` in PATH (explicit skip otherwise). It starts only a
private socket under `t.TempDir()`, ignores user tmux config, uses `cat` as a live
agent surrogate, and kills only that private server in cleanup. A strict runtime
exec adapter sends production `StreamPTYHandler` commands to this local tmux.
No Docker/container/Kubernetes/sandbox execution is implied by that adapter.

See [the project log](../../../.design/project-log/terminal-workspace-p0-lifecycle.md)
for the lifecycle contract, runtime differences and validation limitations.
