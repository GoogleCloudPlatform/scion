# Terminal Workspace P3 — #1702 Chat Navigation Icons

**Date:** 2026-09-21
**Issue:** #1702
**Branch:** dev-p3-1702
**Base:** 450f2728

## Summary

Added Terminal and Graph navigation icons to the agent DM header in the chat view, and replaced the pop-out icon in the member list with a Graph icon.

## Changes

### `web/src/components/pages/chat.ts`

- Imported `openTerminal` and `terminalHref` from `open-terminal.js`
- Added `getAgentProjectId()` helper to resolve the project ID for an agent DM peer
- Added Terminal icon button (`sl-icon name="terminal"`) in header-actions:
  - Uses `terminalHref()` as the link href for accessibility (modified clicks open in new tab)
  - Primary click dispatches through `openTerminal()` which respects `web.terminal_workspace` flag
- Added Graph icon button (`sl-icon name="diagram-3"`) in header-actions:
  - Navigates to `/agents/graph?project={projectId}&focus={agentId}` via `navigateTo()`
  - Only renders when projectId is available
- Both buttons gated on `conv.isDM && conv.peerKind === 'agent'` — do not appear for user DMs or threads

### `web/src/components/shared/chat/chat-members.ts`

- Replaced `agent-popout` link (box-arrow-up-right icon, `target="_blank"`) with `agent-graph` link (diagram-3 icon)
- Graph link navigates to `/agents/graph?project={projectId}&focus={agentId}` in-app via `navigateTo()`
- Preserved modified-click (Ctrl/Cmd/Shift/Alt) handling so those still open in new tabs
- Only renders when the agent has a `projectId`
- Updated CSS class names: `agent-popout` → `agent-graph` across styles

## Design Decisions

- Placed Terminal and Graph icons before the density toggle for visual prominence
- Graph icon only appears when projectId is available (matches terminal-pane behavior)
- Members list graph icon uses same `<a>` pattern as the terminal icon for accessibility
- Reused existing `openTerminal()` helper to respect feature flag routing

## Gates

- `tsc --noEmit --project tsconfig.json` ✅
- `tsc --noEmit --project tsconfig.client.json` ✅
- `tsc --noEmit --project src/client/tsconfig.terminal-tests.json` ✅
- `tsc --noEmit --project e2e/terminal-workspace/tsconfig.json` ✅
- `eslint e2e/terminal-workspace/ --ext .ts` ✅ (0 errors, 2 pre-existing warnings)
- `npm run build` ✅
- `playwright test --config e2e/terminal-workspace/playwright.config.ts` ✅ (75 passed)

## Testing Notes

No existing chat e2e tests cover the DM header actions. The changes are UI-only additions to the chat header and member list. The Playwright terminal workspace tests (75 tests) all pass, confirming no regression in terminal workspace functionality.
