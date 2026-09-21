# P1.7 #1652 — Centralized Terminal Entry Points

**Date:** 2026-09-20
**Developer:** tw-p1-1652-claude-dev (recovery)
**Base:** d6b7fff8fe33601f8f0dedbcb0e9f83b148768fa
**Status:** Candidate delivered, pending independent review

## Scope

Centralize all terminal entry points in the Scion web frontend to route
through the singleton terminal workspace coordinator when the
`web.terminal_workspace` feature flag is enabled. Preserve chat page
user state across terminal workspace round-trips.

## Changes

### New files
- `web/src/client/open-terminal.ts` — Central `terminalHref()` and
  `openTerminal()` helpers. Uses `nav-click` event dispatch to avoid
  circular imports with main.ts.
- `web/e2e/terminal-entrypoints/entrypoints.pw.ts` — 30 table-driven
  Playwright browser tests covering every entry point.
- `web/e2e/terminal-entrypoints/playwright.config.ts` — Test runner config.
- `web/e2e/terminal-entrypoints/tsconfig.json` — TypeScript config for tests.
- `.design/project-log/terminal-workspace-p1-1652.md` — This file.

### Modified files
- `web/src/client/main.ts` — Re-exports `openTerminal` and `terminalHref`.
  Added `returningFromTerminal` detection in `renderRoute()` to preserve
  the existing page element when returning from terminal workspace to
  the same route, instead of destroying and re-creating it.
- `web/src/components/pages/agents.ts` — Terminal button href uses
  `terminalHref(agent.id)`.
- `web/src/components/pages/agent-detail.ts` — Terminal link href uses
  `terminalHref(this.agentId)`.
- `web/src/components/pages/project-detail.ts` — Both list and grid view
  terminal buttons use `terminalHref(agent.id)`.
- `web/src/components/shared/agent-tree-view.ts` — Terminal icon-button
  href uses `terminalHref(agent.id)`.
- `web/src/components/shared/chat/chat-members.ts` — Terminal link uses
  `terminalHref(a.id)`, click handler routes through `openTerminalFromChat()`
  which delegates to workspace coordinator (flag-on) or legacy popup
  (flag-off).
- `web/.eslintrc.cjs` — Added override for `e2e/terminal-entrypoints/*.ts`
  selecting its own tsconfig (authorized by manager).

### NOT modified (1653 owner)
- `terminal-pane.ts`
- `terminal-workspace-root.ts`
- Existing `e2e/terminal-workspace/` fixtures

## Verification

All gates run via mandatory prefix-filter Python runner with SCION_*
stripped (44 variables) and exact HEAD asserted.

- **TypeScript:** `tsc --noEmit` — exit 0 (clean)
- **Lint (touched source files):** Pre-existing baseline errors; 0 new
  errors on any line introduced by this change. Cause of baseline
  errors UNKNOWN.
- **Lint (new test files):** 0 errors, exit 0
- **Unit tests:** `npm run test -- --maxWorkers=2` — 1207 passed, 0 failed,
  exit 0. Earlier runs without `--maxWorkers=2` showed 4 failures in
  files not touched by this change; cause UNKNOWN.
- **Production build:** exit 0
- **Browser tests:** 30 passed, 0 failed, exit 0

## Test Coverage

30 table-driven Playwright browser tests. Chat state tests (#22, #23)
verify page element identity (unique id stamp — a destroyed and
re-created page loses it) and appended draft content survival (a child
element simulating in-progress user content — a destroyed page loses
children). Both tests fail at the prior implementation without the
page-preservation routing fix and pass with it (regression proof in
`reports/p1-1652-logs/regression-r2.log`).

| # | Test | Result |
|---|---|---|
| 1 | agent list (grid view) has workspace href when flag is ON | PASS |
| 2 | agent list (table view) has workspace href when flag is ON | PASS |
| 3 | agent detail page has workspace href when flag is ON | PASS |
| 4 | project detail (grid view) has workspace href when flag is ON | PASS |
| 5 | project detail (list/table view) has workspace href when flag is ON | PASS |
| 6 | tree/graph view has workspace href when flag is ON | PASS |
| 7 | agent list (grid view) has legacy href when flag is OFF | PASS |
| 8 | agent list (table view) has legacy href when flag is OFF | PASS |
| 9 | agent detail page has legacy href when flag is OFF | PASS |
| 10 | project detail (grid view) has legacy href when flag is OFF | PASS |
| 11 | project detail (list/table view) has legacy href when flag is OFF | PASS |
| 12 | tree/graph view has legacy href when flag is OFF | PASS |
| 13 | clicking agent list (grid) navigates to workspace and attaches | PASS |
| 14 | clicking agent list (table) navigates to workspace and attaches | PASS |
| 15 | clicking agent detail navigates to workspace and attaches | PASS |
| 16 | clicking project detail (grid) navigates to workspace and attaches | PASS |
| 17 | clicking project detail (list) navigates to workspace and attaches | PASS |
| 18 | clicking tree/graph view navigates to workspace and attaches | PASS |
| 19 | Ctrl-click opens new tab via real href | PASS |
| 20 | chat membership control routes through workspace (flag ON) | PASS |
| 21 | chat membership control uses legacy popup (flag OFF) | PASS |
| 22 | chat source state retained (page identity + draft content) | PASS |
| 23 | cross-tab: chat state retained after denied foreground focus | PASS |
| 24 | legacy redirect /agents/{id}/terminal -> /terminals/{id} | PASS |
| 25 | legacy standalone terminal (flag OFF) | PASS |
| 26 | repeated navigation reuses one pane and socket | PASS |
| 27 | pending open during agent fetch does not create duplicate | PASS |
| 28 | second tab defers to owning tab | PASS |
| 29 | terminal hrefs use full trusted UUID format | PASS |
| 30 | browser back/forward preserves retained terminal session | PASS |

## Design Decisions

1. **nav-click event dispatch:** Avoids circular dependency between
   entry-point components and main.ts. The router already listens for
   this event.

2. **openTerminalFromChat:** Checks feature flag at call time. When
   workspace is enabled, routes through `openTerminal()` (nav-click).
   When disabled, uses legacy `openTerminalPopout()` (window.open).
   Kept in chat-members.ts (not centralized) because the legacy popup
   fallback is a chat-specific import with only one caller.

3. **Chat page preservation:** When navigating from `/chat` to
   `/terminals/{id}`, the terminal route handler hides the route outlet
   (`appContainer.hidden = true`) without clearing it. The page element
   inside remains live. When returning to `/chat`, `renderRoute()`
   detects that the outlet was hidden (returning from terminal) and that
   the existing page tag and path match, and skips the normal
   `oldPage.remove()` + re-creation. This preserves the actual
   `scion-page-chat` element — all in-memory reactive properties,
   child nodes, event listeners, and DOM state survive the round-trip.
   Explicit navigation to a different chat destination (e.g.,
   `/chat/space/xyz`) still renders normally.
