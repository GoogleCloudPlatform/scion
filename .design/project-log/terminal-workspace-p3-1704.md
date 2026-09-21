# Terminal Workspace Header Polish (#1704)

**Date:** 2026-09-20
**Author:** dev-p3-1704
**Issue:** #1704

## Summary

Two visual polish changes to the terminal workspace header:

1. **Title change:** Header page title changed from "Terminals" to "🌱 Scion Terminal Viewer" to align with the "Scion Chat" naming convention used in the chat view header.

2. **Padding fix:** Restored left/right horizontal padding on the `scion-header` element when rendered inside the terminal workspace.

## Root cause (padding)

The `scion-header` custom element uses Shadow DOM `:host { padding: 0 1.5rem }` for its horizontal padding. In the chat-shell and app-shell, the header lives inside another LitElement's Shadow DOM, so the global CSS reset (`* { padding: 0 }` in `index.html`) cannot reach it. In the terminal workspace, however, the header is placed in **light DOM** (via `document.createElement`), where the global `*` selector **does** match the element and overrides the `:host` padding — making the sign-out button flush against the right edge.

The fix adds a scoped CSS rule in `terminal-workspace-root.ts`'s `installStyles()`:

```css
#terminal-workspace > scion-header {
  padding-inline: 1.5rem;
}
```

## Files changed

- `web/src/client/terminal-workspace-root.ts` — title text + padding CSS rule
- `web/e2e/terminal-workspace/workspace.pw.ts` — two new Playwright tests

## Gates run

| Gate                                                                 | Result                                   |
| -------------------------------------------------------------------- | ---------------------------------------- |
| `npm run build`                                                      | Pass                                     |
| `npx tsc --noEmit --project tsconfig.json`                           | Pass                                     |
| `npx tsc --noEmit --project tsconfig.client.json`                    | Pass                                     |
| `npx tsc --noEmit --project src/client/tsconfig.terminal-tests.json` | Pass                                     |
| `npx tsc --noEmit --project e2e/terminal-workspace/tsconfig.json`    | Pass                                     |
| `npx eslint e2e/terminal-workspace/ --ext .ts`                       | Pass (0 errors, 2 pre-existing warnings) |
| Playwright terminal-workspace tests                                  | 77/77 passed                             |
