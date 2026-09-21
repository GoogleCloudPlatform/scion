# Project Log: F1 Toolbar Window-State Fix — Part 1

**Date**: 2026-09-21
**Branch**: scion/dev-p3-f1-osc0
**Base**: ce7ff971
**Author**: dev-p3-f1-osc0

## Problem

The toolbar's Agent/Shell toggle incorrectly shows "Agent" highlighted after a
user switches tmux windows via keyboard (Ctrl-B n/p). The `activeWindow` state
was only set once at attach via OSC 7337, with no ongoing sync mechanism.

## Approach: tmux `set-titles` + OSC 0 Handler

tmux natively emits OSC 0 (`\033]0;<title>\007`) to each client's PTY on window
switch when `set-titles` is enabled. By setting `set-titles-string '#W'`, the
title is exactly the window name ("agent" or "shell").

The frontend registers an OSC 0 handler that parses these title updates and
sets `activeWindow` accordingly. Only exact matches of "agent" or "shell" are
accepted; all other values are ignored.

This approach requires zero Go backend changes and zero additional docker exec
calls. Data flows through the existing PTY I/O path.

## Changes

### Template (.tmux.conf)

Added two lines to both source template copies (after `allow-passthrough on`):

```
set -g set-titles on
set -g set-titles-string '#W'
```

Files modified:

- `resources/templates/default/home/.tmux.conf`
- `pkg/config/embeds/templates/default/home/.tmux.conf`

Both files verified identical.

### Frontend (terminal-pane.ts)

Added OSC 0 handler after the existing OSC 7337 handler. The handler:

- Trims whitespace from the title data
- Only matches exact "agent" or "shell" values
- Sets `activeWindow` on match
- Returns `false` to allow other OSC 0 handlers to also process

The existing OSC 7337 handler is preserved unchanged as initial-state fallback.

### Unit Tests (terminal-pane.test.ts)

Added 5 tests in `describe('OSC 0 window-state tracking (F1 fix)')`:

1. OSC 0 "agent" sets activeWindow to agent
2. OSC 0 "shell" sets activeWindow to shell
3. OSC 0 with unknown value ("bash", "") does not change activeWindow
4. OSC 0 overrides OSC 7337 — last value wins
5. OSC 7337 still works as initial state fallback (regression check)

### Playwright Browser Test (pane.pw.ts)

Added browser test verifying the full OSC 0 → toolbar state flow:

- OSC 7337 sets initial state to "shell"
- OSC 0 "agent" overrides toolbar to Agent active
- OSC 0 "shell" switches back
- Unknown OSC 0 value ("bash") leaves toolbar unchanged

## Gates Passed

- TypeScript: `npx tsc --noEmit` (tsconfig.json, tsconfig.client.json) — clean
- ESLint: no new errors (pre-existing warnings/errors unchanged)
- Prettier: all modified files pass format check
- Unit tests: 15/15 pass (terminal-pane.test.ts)
- Playwright: 5/5 pass (pane.pw.ts)
- Build: `npm run build` — clean

## Note: Part 2 Required

This Part 1 change only affects source templates (new agent creation). Existing
running agents still have the old .tmux.conf without `set-titles on`. Part 2
(attach-time `tmux set-option` activation in pty_handlers.go) is needed to fix
F1 for existing agents. Part 2 is serialized after #1661 nonce work clears
pty_handlers.go ownership.
