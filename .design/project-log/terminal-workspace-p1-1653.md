# Terminal workspace P1.8 — Hidden terminal interaction isolation (#1653)

Date: 2026-09-20
Base: d6b7fff8 (scion/terminal-workspace)
Developer: tw-p1-1653-claude-dev (recovery)

## Summary

Implemented explicit visible/focused state isolation for retained hidden
terminal panes. Hidden panes continue parsing output and responding to
terminal protocol queries, but no longer interfere with focus, clipboard,
resizing, or file drops in other parts of the UI.

## Changes

### terminal-pane.ts

1. **Visibility state** — Added `_visible` field tracking explicit
   setVisible() state, used by all guards.

2. **Window drag prevention** — Moved global `dragover`/`drop` handlers
   from unconditional connectedCallback to visibility-scoped:
   `installWindowDragPrevention()` installs only when visible+connected,
   `setVisible(false)` removes them so Chat/Dashboard drops proceed normally.

3. **ClipboardAddon → custom OSC 52 handler** — Replaced `@xterm/addon-clipboard`
   with a direct OSC 52 parser handler that gates system clipboard read/write
   on `_visible`. Hidden agent output cannot read or write the system clipboard.
   Handles UTF-8 via TextEncoder/TextDecoder.

4. **Paste generation guards** — Ctrl+V and Ctrl+Shift+V handlers now capture
   session generation at keypress time and verify `_visible`, `!disposed`, and
   matching generation in the async `.then()` callback.

5. **Upload generation guards** — `_handleFileDrop` captures generation at
   drop time and checks `_visible`/generation before injecting file paths.

### Design: human input vs. protocol responses

The critical distinction for hidden panes: xterm.js `onData` carries both
human keyboard input AND terminal-generated protocol responses (DSR, DA).
We do NOT gate `sendData()` itself because protocol responses must flow.

Instead, each source of human input has its own guard:
- **Keyboard input via onData**: Naturally blocked — hidden panes are blurred
  and inert, so keyboard events can't reach xterm's textarea.
- **Clipboard paste (Ctrl+V)**: Guarded by visibility+generation check in
  the async readText() completion.
- **OSC 52 clipboard**: Guarded by visibility check in the OSC 52 parser handler.
- **File upload path injection**: Guarded by visibility+generation check
  after upload API response.
- **Terminal protocol responses**: Unrestricted — DSR, DA etc. flow through
  onData → sendData without any visibility gate.

## Files changed

| File | Change |
|------|--------|
| `web/src/components/terminal/terminal-pane.ts` | Visibility state, drag prevention scoping, OSC 52 handler, paste/upload guards |
| `web/src/components/terminal/terminal-pane.test.ts` | +3 Vitest tests for visibility isolation |
| `web/e2e/terminal-hidden/fixture.html` | New e2e fixture HTML |
| `web/e2e/terminal-hidden/fixture.ts` | New e2e fixture with gated clipboard mock |
| `web/e2e/terminal-hidden/hidden.pw.ts` | 7 Playwright real-browser tests |
| `web/e2e/terminal-hidden/playwright.config.ts` | Playwright config |
| `web/e2e/terminal-hidden/serve.mjs` | Vite dev server for fixture |
| `web/e2e/terminal-hidden/tsconfig.json` | TypeScript config for fixture |

## Tests

### Vitest (3 new, all pass)
- setVisible(false) blurs, cancels resize, removes window drag prevention
- Hidden pane does not auto-focus on late socket connect
- Window drag prevention not installed when pane starts hidden

### Playwright e2e — e2e/terminal-hidden (7 new, all pass)
1. Hide → inert/blur, reveal → changed size → resize sent
2. Late socket open while hidden does not steal focus
3. Hidden OSC 52 does not write to clipboard, visible OSC 52 does
4. Terminal protocol responses (DSR) continue while hidden
5. Chat/Dashboard file drops unaffected by hidden terminals
6. Delayed paste completing after hide does not send to terminal
7. Upload completing after hide does not inject paths

### Existing (all pass, no regressions)
- terminal-pane.test.ts: 10 tests pass
- terminal-sessions.test.ts: 23 tests pass
- terminal-pane e2e: 4 tests pass
- terminal-workspace e2e: 15 tests pass

## Verification commands

```
npm run typecheck                                           # clean
npm run build                                               # clean
npx eslint src/components/terminal/terminal-pane.ts \
    src/client/terminal-workspace-root.ts                   # 5 baseline errors, 0 new
npx vitest run src/components/terminal/terminal-pane.test.ts # 10 pass
CHROMIUM_EXECUTABLE=/usr/bin/chromium npx playwright test \
    --config e2e/terminal-pane/playwright.config.ts         # 4 pass
CHROMIUM_EXECUTABLE=/usr/bin/chromium npx playwright test \
    --config e2e/terminal-hidden/playwright.config.ts       # 7 pass
CHROMIUM_EXECUTABLE=/usr/bin/chromium npx playwright test \
    --config e2e/terminal-workspace/playwright.config.ts    # 15 pass
```

## Coordination note

The new `e2e/terminal-hidden/` fixture has its own `tsconfig.json` for type
checking. A scoped ESLint override in `.eslintrc.cjs` is needed to lint it
with its own tsconfig; this requires manager coordination since #1652
currently owns narrow fixture overrides in that shared file.

## Revision history

### Rev 2 (cfa1cba, da726b6) — Focused-state guards, drag scoping

Per manager review: added `_focused` field requiring `_visible && _focused`
for all human input guards. Window drag prevention scoped via `composedPath()`.
ESLint override added for `e2e/terminal-hidden/*.ts`. Expanded to 15 e2e tests.

### Rev 3 (18f424f) — R1 review fixes

Per independent R1 review findings:

**R1: OSC 52 read generation gap** — Captured session generation before
`readText()`, added generation recheck at async completion. Prevents clipboard
content leaking to a reconnected session (browser-confirmed by reviewer probe).

**R2: OSC 52 selection type parity** — Extracts selection prefix from OSC 52
data. Only `c` (system clipboard) supported; non-`c` selections (`p`, `q`, `s`)
silently ignored, matching original `BrowserClipboardProvider`. Response echoes
the requested selection type in the reply.

**R3: writeText async documentation** — Corrected comments: `writeText()` is
async per Clipboard API spec. Pre-call guard prevents unauthorized initiation
but cannot revoke an already-dispatched OS write. Inherent API limitation.

**DOM focus ownership** — Promoted from Phase 2 optional to required. Wired
`focusin`/`focusout` event listeners in `connectedCallback` to automatically
track `_focused`. Focus moving to sibling elements (rail, header) clears
`_focused` via `focusout` handler. Focus moving within the pane (toolbar,
file picker) preserves `_focused` via `relatedTarget`/`contains()` check.
File drops restore `_focused` as an explicit user interaction.

**Stale mock cleanup** — Removed `vi.mock('@xterm/addon-clipboard')` from
vitest (addon no longer imported).

**Lint nits** — Added return type annotations to getter properties, fixing
2 lint warnings.

Tests expanded to 22 browser e2e + 10 vitest. New tests:
- DOM focus: sibling click clears _focused, toolbar preserves it, refocus restores it
- OSC 52 generation: read response blocked after session reconnect
- OSC 52 selection: non-'c' types ignored, 'c' echoed in response
- OSC 52 malformed: invalid base64 and missing semicolon handled gracefully

### Revision 4 — R2 review findings

**R2 O1: setVisible(true) focus bypass** — Manager elevated to Required. Changed
`_focused` initial value from `true` to `false`. `setVisible(true)` now derives
`_focused` from actual DOM state (`this.contains(document.activeElement) ||
this.shadowRoot?.contains(document.activeElement as Node) || false`) instead of
unconditionally setting `true`. Auto-focus path
(`shouldAutoFocusTerminal()` → `terminal.focus()` → `focusin` → `_focused = true`)
establishes correct state when nothing else has focus.

**R2 O2: Window blur test** — Added maintained test for `focusout` with `null`
`relatedTarget` (window blur / Alt-Tab scenario).

**R2 FYI: Non-'c' OSC 52 read response** — Non-'c' reads now send empty protocol
response `\x1b]52;${sel};\x07` matching original `BrowserClipboardProvider` which
returned `Promise.resolve('')`. Non-'c' writes remain no-op. No OS clipboard
access for unsupported selections.

**R2 FYI: Drop focus back door** — `_onDrop` now calls `this.terminal?.focus()`
for real DOM focus path instead of directly setting `_focused = true`. The
`focusin` event naturally establishes `_focused`. If focus leaves during async
upload, `focusout` clears `_focused` and completion guard correctly blocks.

**Malformed base64 disposition** — Invalid base64 write: original addon decoded
to `''` and called `writeText('')` (clearing clipboard). Custom handler: `atob()`
throws, caught silently, no clipboard mutation. Deliberate safer behavior
documented in code, not a claim of exact parity.

**Protocol DSR/DA coverage** — Added test verifying DSR (CSI 6n) and DA (CSI c)
responses continue through focused, unfocused, and hidden states.

Tests expanded to 28 browser e2e (+ non-'c' dedicated = 29 test functions in file).
New tests:
- Initial visible without DOM focus blocks OSC 52
- Hide/reveal while sibling focused blocks until terminal focused
- Window blur (null relatedTarget) clears _focused and blocks
- Protocol DSR/DA unchanged through focus state transitions
- File drop establishes real DOM focus (not back door)
- Non-'c' OSC 52 read returns empty response matching original addon

## Baseline failures (pre-existing, not introduced)

- `src/components/shared/role-binding-assignment-form.test.ts` — beforeAll
  hook timeout (10000ms). Unrelated to terminal workspace.
- 5 lint errors in owned files: 2× unsafe-assignment, 2× unsafe-member-access,
  1× no-useless-escape. All pre-existing from prior phases.
