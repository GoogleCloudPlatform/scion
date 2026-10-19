# webperf C3: lazy terminal workspace chunk and bundle budget (ptone/scion#5306)

Base: `730a338` (origin/main). Part of the webperf epic (#5301), C-workstream
vertical slice; C4 and C8 build on it.

## What changed

- The terminal workspace (coordinator, workspace root, terminal pane,
  sessions, persistence, layout, move, palette-open) was statically imported by
  `web/src/client/main.ts`, so about 150 KB of it shipped in `main.js` on every
  route, including login. It now loads through
  `client/terminal-workspace-loader.ts` (a single-flight `import()` of the
  barrel `client/terminal-workspace-entry.ts`) the first time a `/terminals`
  route renders. Rollup emits one `terminal-workspace-entry-[hash].js` chunk.
- `ensureTerminalCoordinator(thisNav)` is async. After the load it checks the
  `navigationId` guard (and account teardown) before creating or mounting
  anything, so a navigation away during the load wins. The route outlet is
  hidden before the load starts, as it was before the chunk became lazy; a
  newer navigation un-hides it. A failed load logs, shows a toast, and shows
  the previous page again (the same as a failed page chunk). The loader does
  not cache a rejection, so the next visit retries.
- `main.ts` imports terminal modules as types only. `terminal-move.ts`,
  `terminal-palette-open.ts` and `terminal-palette-data.ts` already used
  type-only imports for the heavy modules; they did not need changes.
- `web/scripts/check-bundle-budget.mjs` + `web/bundle-budget.json`: limits on
  `main.js` raw/gzip and the entry static closure gzip (JS and CSS), a
  required `assets/terminal-workspace-` chunk, and no `terminal-*` module in
  the static closure of `client/main.ts`, `pages/login.ts` and `pages/home.ts`
  (the entry is its own root, so it is checked even if a route chunk stops
  importing it). Module lists come
  from source maps, found next to the chunk or under `dist/sourcemaps/`, so the
  check keeps working if A5 moves maps out of `dist/client`. Two exact-path
  allowlist entries, each with a reason: `terminal-workspace-events.ts`
  (constants imported by the header) and `terminal-workspace-loader.ts`
  (the loader itself). CI runs the check after the web build, plus its
  `node --test` unit tests; `make ci-full` runs both through a new
  `web-budget` target.

## Numbers (dev-time vite build)

| | before | after | budget |
|---|---|---|---|
| main.js raw | 369,572 | 216,523 | 240,000 |
| main.js gzip | 97,803 | 58,378 | 65,000 |
| entry closure gzip (main + shoelace + shoelace.css + lit) | 174,959 | 135,539 | 140,000 |
| terminal-workspace chunk | n/a | 145 kB raw / 37.5 kB gzip | n/a |

The entry closure has only about 4.5 KB of headroom. Most of it is the
Shoelace chunk (about 62 KB gzip), which later phases cannot shrink by
trimming `main.ts`.

## Learnings

- Rollup names a dynamic-import chunk after its entry module, so the barrel's
  file name decides the chunk name (`terminal-workspace-entry-*`).
- The Vite manifest lists chunks, not modules, so a module that is inlined
  into `main.js` can only be found through the source map.
- Testing `main.ts` routes needs a fresh module per scenario: once the
  coordinator exists, the lazy path never runs again. The three route
  scenarios (lazy load, navigated away, load failure) are therefore separate
  test files that share
  `__fixtures__/main-boot.ts` and `__fixtures__/terminal-workspace-fake.ts`.
- First version hid the outlet only after the load, to keep the previous
  page on screen meanwhile. That left the old page (and its header)
  interactive under a `/terminals` URL for the whole load. On the Vite dev
  server, which the chat mobile e2e suite uses, the load is long, and two
  e2e tests (dm-mode-switch, mode-switch-state) clicked the stale header's
  mode switch and failed their first attempt. Fix: hide the outlet before
  awaiting the load, as before the chunk was lazy. Vitest
  `terminal route hides the previous page while the workspace loads`
  fails without the fix. The cause was not Vite dependency optimization: its
  scan follows literal `import()` calls, and xterm was already a dynamic
  import before C3.
- Mutation checks run at dev time: removing the `navigationId` check after the
  load makes `navigating away during workspace load does not mount` fail;
  putting back a static value import of `TerminalCoordinator` makes the budget
  script fail with 11 findings (size limits and login/home closure).
