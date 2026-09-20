# P1.7 #1652 — Centralized Terminal Entry Points

**Date:** 2026-09-20
**Developer:** tw-p1-1652-claude-dev (recovery)
**Base:** d6b7fff8fe33601f8f0dedbcb0e9f83b148768fa
**Head:** 9a5036f4c21b2eb8db973f3db932ff20608c0c11 (scion/terminal-workspace)
**Status:** Candidate delivered, pending independent review

## Scope

Centralize all terminal entry points in the Scion web frontend to route
through the singleton terminal workspace coordinator when the
`web.terminal_workspace` feature flag is enabled.

## Changes

### New files
- `web/src/client/open-terminal.ts` — Central `terminalHref()` and
  `openTerminal()` helpers. Uses `nav-click` event dispatch to avoid
  circular imports with main.ts.
- `web/e2e/terminal-entrypoints/entrypoints.pw.ts` — 30 table-driven
  Playwright browser tests covering every entry point.
- `web/e2e/terminal-entrypoints/playwright.config.ts` — Test runner config.
- `web/e2e/terminal-entrypoints/tsconfig.json` — TypeScript config for tests.

### Modified files
- `web/src/client/main.ts` — Re-exports `openTerminal` and `terminalHref`.
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
  (flag-off). Chat source state preserved.
- `web/.eslintrc.cjs` — Added override for `e2e/terminal-entrypoints/*.ts`
  selecting its own tsconfig (authorized by manager).

### NOT modified (1653 owner)
- `terminal-pane.ts`
- `terminal-workspace-root.ts`
- Existing `e2e/terminal-workspace/` fixtures

## Commits (3 ahead of base)

1. `1785f658` feat: centralize terminal entry points through workspace coordinator
2. `08aed9c6` fix: correct browser test fixtures and locators for entry point coverage
3. `9a5036f4` test: expand browser coverage to 30 tests with all entry points and shell marker fix

## Verification

- **TypeScript:** `tsc --noEmit` — exit 0 (clean)
- **Lint (touched files):** 0 new errors; 88 pre-existing baseline errors
  across main.ts and other files (unsafe-any, no-unused-vars, etc.)
- **Lint (new test files):** 0 errors, exit 0
- **Unit tests:** 1203 passed, 4 failed (all pre-existing in terminal-transport,
  terminal-pane, agent-create-projects), 1 skipped
- **Production build:** succeeds (8.78s), exit 0
- **Browser tests:** 30 passed, 0 failed, exit 0

## Test Coverage

30 table-driven Playwright browser tests:

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
| 13 | clicking agent list (grid view) navigates to workspace and attaches | PASS |
| 14 | clicking agent list (table view) navigates to workspace and attaches | PASS |
| 15 | clicking agent detail page navigates to workspace and attaches | PASS |
| 16 | clicking project detail (grid view) navigates to workspace and attaches | PASS |
| 17 | clicking project detail (list/table view) navigates to workspace and attaches | PASS |
| 18 | clicking tree/graph view navigates to workspace and attaches | PASS |
| 19 | Ctrl-click on terminal link opens new tab via real href | PASS |
| 20 | chat membership terminal control routes through workspace when flag is ON | PASS |
| 21 | chat membership terminal control uses legacy popup when flag is OFF | PASS |
| 22 | chat source state is retained when opening terminal from chat | PASS |
| 23 | cross-tab: chat state retained when another tab owns terminals | PASS |
| 24 | legacy /agents/{id}/terminal redirects to /terminals/{id} when ON | PASS |
| 25 | legacy /agents/{id}/terminal loads standalone terminal when OFF | PASS |
| 26 | repeated navigation to same agent reuses one pane and socket | PASS |
| 27 | pending open during agent fetch does not create duplicate | PASS |
| 28 | second tab defers terminal to owning tab without attaching locally | PASS |
| 29 | terminal hrefs use full trusted UUID format | PASS |
| 30 | browser back/forward preserves retained terminal session | PASS |

## Design Decisions

1. **nav-click event dispatch:** Avoids circular dependency between
   entry-point components and main.ts. The router already listens for
   this event.

2. **openTerminalFromChat:** Checks feature flag at call time. When
   workspace is enabled, routes through `openTerminal()` (nav-click).
   When disabled, uses legacy `openTerminalPopout()` (window.open).

3. **Chat state preservation:** The route outlet is hidden (not destroyed)
   when the terminal workspace is shown. The chat shell (`scion-chat-shell`)
   is reused by `renderRoute()` — only the inner page element is swapped.
   Tests mark the shell element to prove it survives navigation.

4. **Shell marker vs page marker:** The router's `renderRoute()` replaces
   the page element (`scion-page-chat`) inside the shell when re-navigating
   to `/chat`, but the shell (`scion-chat-shell`) is reused. Tests verify
   shell survival, which is the actual state preservation guarantee.
