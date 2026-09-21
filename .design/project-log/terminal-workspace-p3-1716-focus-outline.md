# Fix: Focus Outline Retained on Multi-to-Single Pane Switch (#1716)

**Date:** 2026-09-21
**Issue:** #1716
**Commit:** 472019c

## Problem

When switching from a multi-pane terminal layout (twoColumns, twoRows, four)
to single-pane, the 2px blue focus outline persisted on the sole visible pane.
In single-pane mode focus is implicit (there is nothing else to focus), so the
outline was misleading visual noise.

## Root Cause

The CSS rule `scion-terminal-pane[data-focused]` in
`terminal-workspace-root.ts` applied unconditionally regardless of the number
of visible panes. Since `setVisible(true)` re-derives focus from
`document.activeElement` (which was never blurred on layout switch), the
`data-focused` attribute persisted and the outline rendered.

## Approach: CSS-Only (Approach A)

Rather than fighting the DOM focus system by clearing `data-focused` in JS,
the fix scopes the CSS rule to only match multi-pane layouts:

1. **`data-effective-layout` attribute**: `updateGridTemplate()` now sets
   `this.paneHost.dataset.effectiveLayout = effectivePreset` on the
   `.terminal-pane-host` container. The effective preset already accounts for
   narrow viewport and zoom overrides.

2. **Scoped CSS selector**: The focus outline rule now targets:
   ```css
   .terminal-pane-host[data-effective-layout='two-columns'] scion-terminal-pane[data-focused],
   .terminal-pane-host[data-effective-layout='two-rows'] scion-terminal-pane[data-focused],
   .terminal-pane-host[data-effective-layout='four'] scion-terminal-pane[data-focused] { ... }
   ```
   The `single` layout is omitted, so the outline never renders in
   single-pane mode.

## Scenarios Covered

- Multi-to-single transition: outline disappears
- Single-to-multi transition: outline reappears on focused pane
- Direct single load: no outline
- Keyboard/pointer focus in single: no outline
- Narrow viewport override: treated as single, no outline
- Zoomed pane: treated as single, no outline

## Invariants Preserved

- Terminal/socket/session/generation identity unchanged
- `setVisible()` contract unchanged
- Multi-pane focus behavior unchanged
- Rail selection (`data-selected`) unaffected
- Drag-over outline (`data-drag-over`) unaffected

## Files Changed

| File                                             | Lines      | Change                       |
| ------------------------------------------------ | ---------- | ---------------------------- |
| `web/src/client/terminal-workspace-root.ts`      | +8/-1      | CSS scoping + data attribute |
| `web/src/client/terminal-workspace-root.test.ts` | +260 (new) | 11 tests                     |

## Gate Results

| Gate                                    | Result       |
| --------------------------------------- | ------------ |
| `terminal-pane.test.ts`                 | 15/15 passed |
| `terminal-layout.test.ts`               | 70/70 passed |
| `terminal.test.ts`                      | 22/22 passed |
| `terminal-workspace-root.test.ts` (new) | 11/11 passed |
