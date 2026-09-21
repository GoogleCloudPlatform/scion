# Terminal Pane Interaction Improvements (#1701)

**Date:** 2026-09-21
**Branch:** dev-p3-1701
**Base:** fbdb113c (scion/terminal-workspace)
**Commit:** 223af4d7

## Summary

Three terminal pane interaction improvements for the multi-pane workspace:

1. **Drag feedback**: Terminal session drags (from the rail) no longer trigger
   the file-upload overlay on panes. The pane's drag handlers now check for
   `TERMINAL_DRAG_MIME` and skip the upload overlay, letting the workspace
   root's placement feedback show through instead.

2. **Empty layout placeholders**: Multi-pane layouts (two-columns, two-rows,
   quad) now show dotted-line placeholder slots even when zero agents are
   assigned. Previously the "No terminals are open." empty state covered
   the grid, hiding placeholders. The fix hides the empty overlay when a
   multi-pane layout is active.

3. **Focus indicator**: Terminal panes now reflect focus state via a
   `data-focused` attribute, styled with a blue outline in multi-pane view.
   Pane borders are clearer (`1px solid #333`) to visually separate adjacent
   terminals.

## Files Changed

- `web/src/client/terminal-workspace-events.ts` — Added `TERMINAL_DRAG_MIME`
  constant (moved from workspace root to avoid circular imports)
- `web/src/client/terminal-workspace-root.ts` — Import TERMINAL_DRAG_MIME
  from events module; hide empty/status overlays in multi-pane layouts with
  zero agents; skip single-layout placeholder creation; add focus/border CSS
- `web/src/components/terminal/terminal-pane.ts` — Import TERMINAL_DRAG_MIME;
  skip file-upload overlay for terminal drags in all drag handlers
  (_onDragEnter, _onDragLeave, _onDragOver, _onDrop); set/clear
  `data-focused` attribute on focusin/focusout/setVisible
- `web/e2e/terminal-workspace/workspace.pw.ts` — 4 new Playwright tests

## Tests

All 81 tests pass (77 existing + 4 new):

- Terminal drag does not trigger file upload overlay on pane
- Empty multi-pane layout shows dotted placeholders for all slots
- Focused pane has data-focused attribute in multi-pane view
- Pane borders are visible in multi-pane layout

## Gates Run

- `npm run build` — pass
- `tsc --noEmit --project tsconfig.json` — pass
- `tsc --noEmit --project tsconfig.client.json` — pass
- `tsc --noEmit --project src/client/tsconfig.terminal-tests.json` — pass
- `tsc --noEmit --project e2e/terminal-workspace/tsconfig.json` — pass
- `eslint e2e/terminal-workspace/ --ext .ts` — pass (0 errors, 2 pre-existing warnings)
- `playwright test --config e2e/terminal-workspace/playwright.config.ts` — 81/81 pass

## Delivery

Bundle: `transfers/p3-1701-223af4d7.bundle`
