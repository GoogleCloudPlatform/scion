# Production terminal pane parity (P1.2)

This fixture mounts the production `scion-terminal-pane` and session registry,
with the installed real xterm, addons and Shoelace controls. Playwright supplies
isolated HTTP/WebSocket responses; an injected EventSource and clipboard avoid
live agents, account credentials and system clipboard access. The link destination
is also served by a fixture. No Hub, Runtime Broker or container is contacted.

From `web/` after `npm ci`, run with inherited orchestration variables removed:

```sh
without_scion() {
  python3 -c 'import os,sys; os.execvpe(sys.argv[1],sys.argv[1:],{k:v for k,v in os.environ.items() if not k.startswith("SCION_")})' "$@"
}
CHROMIUM_EXECUTABLE=/usr/bin/chromium without_scion npm run test:e2e -- \
  --config e2e/terminal-pane/playwright.config.ts
without_scion npm run typecheck -- --project e2e/terminal-pane/tsconfig.json
without_scion ./node_modules/.bin/eslint 'e2e/terminal-pane/*.ts' \
  --parser-options '{"project":"./e2e/terminal-pane/tsconfig.json"}'
```

The separate config uses loopback port 4528 and refuses an occupied port. The root
Hub E2E suite does not discover these `*.pw.ts` tests. Output is under the existing
ignored `web/test-results/terminal-pane/` directory.

## Browser parity checklist

- [x] One real xterm/DOM/WebSocket identity through hide, route-history changes,
      resize and DOM remount. Hidden bytewise UTF-8/ANSI parses before reveal; 200
      ordered lines remain in scrollback. No hidden resize or detach frames.
- [x] Initially hidden pane waits for geometry; explicit close during that wait
      disposes once and cannot attach after reveal. Explicit connected close sends
      Ctrl-B d, closes once and removes xterm.
- [x] Initial OSC 7337 selects Shell; Agent/Shell clicks emit existing commands.
- [x] Shift+Enter emits one ESC CR with no extra plain Enter.
- [x] Selection copy and paste use real xterm keyboard handlers and an injected
      clipboard boundary. Real ClipboardAddon consumes OSC 52 writes.
- [x] WebLinksAddon recognizes a URL and opens the correct isolated popup target.
- [x] Exposed-port link retains agent-specific proxy URL.
- [x] Real capture-auth dialogs select user scope, display conflict and retry with
      force; requests retain pane identity after route-history change.
- [x] Browser File/DataTransfer drop posts multipart upload and inserts a quoted
      path through the production session transport.
- [ ] Native macOS Option/Shift mouse-selection behavior: unavailable on Linux.
- [ ] Real OS clipboard permissions and desktop activation: not represented by
      the injected clipboard or headless browser; final headed QA owns those checks.

This is production **pane** coverage, not production SPA roots/mode retention
(P1.5), real backend execution, SSE fanout (P1.4), detailed hidden input/clipboard
and asynchronous completion isolation (P1.8), or lifecycle-freeze evidence (P3.3).
The original P0 real-xterm lifecycle fixture remains a separate regression check.
