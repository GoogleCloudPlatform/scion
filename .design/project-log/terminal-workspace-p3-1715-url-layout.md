# Terminal Workspace: URL Layout Encoding (#1715)

**Date**: 2026-09-21
**Issue**: #1715
**Stacked on**: #1716 (86f30928)

## Summary

Encodes the terminal workspace layout (active preset + ordered slot assignments
by agent ID) in URL query parameters so layout state survives page reload and
can be shared via URL.

## URL Format

Versioned query parameter format:

```
/terminals?lv=1&lp=two-columns&s0=<agentId>&s1=<agentId>
/terminals?lv=1&lp=four&s0=<agentId>&s1=&s2=<agentId>&s3=
```

- `lv`: layout version (currently `1`). Unknown versions cause fallback.
- `lp`: layout preset name (`single`, `two-columns`, `two-rows`, `four`).
- `s0`..`s3`: ordered slot agent IDs. Empty string = empty slot.

**Single-pane URLs** do NOT include layout params — the path
`/terminals/{agentId}` is sufficient and cleaner. Layout params are only added
for multi-pane presets.

## Route Precedence

When both a path agent and layout query params exist:
- The query layout state takes precedence for the preset + slot assignments
- Layout is restored BEFORE default single-agent selection to avoid flash

## History Management

| Action | History API |
|--------|-------------|
| Layout preset change | `replaceState` |
| Drag to slot | `replaceState` |
| Open new agent (rail click) | `pushState` (via `navigateTo`) |
| Close agent | `replaceState` |
| Zoom/unzoom | No URL update (transient) |
| Back/forward | `popstate` restores layout from URL |

## Validation

- Unknown version: ignore layout query entirely, fall back to default
- Malformed UUID: slot treated as empty
- Duplicate agent ID in URL: keep first occurrence, clear duplicates
- Over-capacity: truncate to slot count for the preset
- Missing/unauthorized agents: slot stays empty (coordinator.open handles auth)

## Files Changed

- `web/src/client/terminal-layout.ts` — Added `restore()` method,
  `serializeLayoutUrl()`, `parseLayoutUrl()`, `buildLayoutUrl()` functions
- `web/src/client/terminal-workspace-root.ts` — Added URL sync subscriber,
  `getActiveSlotAgentIds()`, `findSessionKeyByAgentId()`, `syncUrlFromLayout()`,
  `setSuppressUrlSync()` methods
- `web/src/client/main.ts` — Parse URL layout before default selection in
  `renderRoute()`, pass query string in `popstate` handler and initial render
- `web/src/client/terminal-layout.test.ts` — 32 new tests (restore + URL
  serialization/deserialization/canonicalization)
- `web/src/client/terminal-workspace-root.test.ts` — 8 new tests (URL sync,
  session key mapping, suppress flag)

## Test Results

- `terminal-layout.test.ts`: 102 tests passed (70 existing + 32 new)
- `terminal-workspace-root.test.ts`: 19 tests passed (11 existing + 8 new)
- `terminal.test.ts`: 22 tests passed (all existing, unchanged)
- TypeScript: both tsconfig checks pass
- Prettier: all files pass
- ESLint: pre-existing warnings only (no new issues)
- Build: passes

## Existing Behavior Preserved

- Direct single-agent routes (`/terminals/{agentId}` without query params) work
  exactly as before
- #1701 overflow behavior preserved
- #1716 focus-outline behavior preserved (data-effective-layout attribute)
- #1703 sorting unchanged
- Feature flag off: layout params ignored (terminal workspace not rendered)
