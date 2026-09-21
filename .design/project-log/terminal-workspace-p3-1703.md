# Terminal Workspace P3 #1703 — Rail Sorting Widget

## Summary

Added a sorting widget to the terminal workspace rail (left sidebar) that lets
users order rail entries by three criteria: "Added" (default, chronological),
"Alphabetical" (agent name), and "Last activity" (most recent first).

## Changes

### Production (`web/src/client/terminal-workspace-root.ts`)

- **RailEntry interface**: Added `addedAt: number` field for stable chronological
  ordering via a monotonic counter.
- **Class state**: Added `railSort` property (`'added' | 'alpha' | 'activity'`,
  default `'added'`) and `entryCounter` monotonic counter.
- **Sort widget**: `buildRailSortWidget()` creates an `sl-dropdown` with
  `sl-icon-button[name="sort-down"]` trigger and `sl-menu` with 3 checkable
  `sl-menu-item`s matching the chat-members pattern.
- **Sort logic**: `sortEntries()` sorts in place with deterministic tie-breaking
  (agent name, then session key). Activity sort uses `lastActivityEvent` falling
  back to `lastSeen`, descending.
- **CSS**: Added `.terminal-sort-dropdown` and `.terminal-sort-btn` styles.
- **No reattach on sort**: Sorting only reorders rail list items; it does not
  create, close, or reassign sessions or layout slots.

### Tests (`web/e2e/terminal-workspace/workspace.pw.ts`)

Added 7 new Playwright tests:

1. Sort widget renders with 3 menu items
2. Default sort is "Added" and entries appear in creation order
3. Alphabetical sort reorders entries by agent name
4. Activity sort reorders entries by most recent activity first
5. Sort preserves active rail selection
6. Sort preserves layout preset slot assignments
7. Deterministic ties: agents with same name sort consistently by session key

Also extended the `AgentFixture` interface with optional `lastActivityEvent` and
`lastSeen` fields to support activity-sort test fixtures.

## Gate Results

| Gate                                                             | Result                                                         |
| ---------------------------------------------------------------- | -------------------------------------------------------------- |
| `npm run build`                                                  | ✅ Pass                                                        |
| `tsc --noEmit --project tsconfig.json`                           | ✅ Pass                                                        |
| `tsc --noEmit --project tsconfig.client.json`                    | ✅ Pass                                                        |
| `tsc --noEmit --project src/client/tsconfig.terminal-tests.json` | ✅ Pass                                                        |
| `tsc --noEmit --project e2e/terminal-workspace/tsconfig.json`    | ✅ Pass                                                        |
| `eslint e2e/terminal-workspace/ --ext .ts`                       | ✅ Pass (0 errors, 2 pre-existing warnings in reconnect.pw.ts) |
| `prettier --check src/client/terminal-workspace-root.ts`         | ✅ Pass                                                        |
| Playwright tests (88 total: 81 existing + 7 new)                 | ✅ All pass                                                    |

## Test Count

- 88 total Playwright tests in `workspace.pw.ts` (7 new for #1703)

## Files Modified

- `web/src/client/terminal-workspace-root.ts` (production)
- `web/e2e/terminal-workspace/workspace.pw.ts` (tests)
