# Experiments Phase 1b — web: client precedence and Experiments tab (ptone/scion#2217)

Branch: `scion/experiments-1b`, rebased onto upstream `main` after
ptone/scion#2360 (Phase 1a-ii, the Go API) merged upstream as
GoogleCloudPlatform/scion#2152.

## Scope

Web-only. No Go changes.

- `web/src/utils/feature-flags.ts`: `setServerFlags()` applies the hub-wide
  experiment map with lazy pinning — values already in
  `window.__SCION_FEATURES__` before the first call are never overwritten,
  so E2E-pinned values and `setFeatureFlag('web.native_chat*')` still win.
  `resetServerFlagStateForTests()` resets that state between tests. A
  shadowed localStorage override logs one `console.info` per flag per page
  load. The `TERMINAL_WORKSPACE_FLAG` constant is exported from here and
  used everywhere the flag name was a string literal, replacing the
  module-local copy in `header.ts`.
- `web/src/client/server-feature-flags.ts` (new): `applyServerFeatureFlags()`
  fetches `/api/v1/experiments` in parallel with `/api/v1/settings/public`
  (`Promise.allSettled`); a non-OK status, network error, or a non-JSON 200
  body on either side is treated as "no data" for that side only, so neither
  side blocks the other. Split out of `main.ts` into its own module so it
  can be unit tested directly without importing the whole app entry module.
  `main.ts` now only imports and awaits it.
- `web/src/components/pages/admin-experiments.ts` (new):
  `<scion-admin-experiments>`. Self-contained — owns its own fetch, state and
  writes. Lazy-loads on the first time its `.active` property becomes true.
  States: normal, 403 (permission message, no switches), malformed (banner +
  confirmed reset-all), and empty. Writes are strictly sequential (every
  switch and the reset button are disabled while one write is in flight, so
  the next write always carries the previous one's revision). On any write
  failure the switch reverts and the tab reloads from the server; a 409
  shows a concurrent-change message (or the malformed-settings message),
  and other failures show the server's error.
  "Reset to default" shows only when an override exists. Tab-level
  attribution ("Last changed by … at …") comes from the last write response.
  A one-line note lists any `unknown_overrides` by name.
- `web/src/components/pages/admin-server-config.ts`: exactly four changes:
  one import, the Experiments `<sl-tab>` last in the nav, its
  `<sl-tab-panel>` last, and the "Save & Reload"/"Reset" actions bar plus
  the harness-config error message are wrapped in one
  `activeTab !== 'experiments'` condition so both are hidden on that tab.
- `web/src/client/open-terminal.ts` and
  `web/src/components/shared/chat/chat-members.ts`: the `'web.terminal_workspace'`
  string literal is replaced by the shared `TERMINAL_WORKSPACE_FLAG` import.
- `web/src/components/shared/header.ts`: the module-local
  `TERMINAL_WORKSPACE_FLAG` constant is deleted; the shared one is imported
  instead, so the two never exist side by side.
- Tests: `feature-flags.test.ts` (precedence matrix, lazy pinning, shadowed-
  override dedup), `server-feature-flags.test.ts` (the parallel boot fetch
  and its failure modes), `admin-experiments.test.ts` (every state and write
  path, including sequential-write revision handoff across two rows, the
  malformed-PUT-409 branch, and the malformed reset-all request body), and
  an addition to `admin-server-config.test.ts` for the tab's position and
  the actions-bar hiding.
- `web/e2e/experiments.spec.ts` (new): under the main Playwright config,
  against the real hub. Asserts `override: null` up front (fails fast
  otherwise), toggles `web.terminal_workspace` off through the tab, waits for
  the PUT 200 and the "Last changed by" line, confirms
  `/agents/<uuid>/terminal` stays put, re-enables through the API, confirms
  the rewrite to `/terminals/<uuid>`, and always PUTs `null` back in
  `finally` for state hygiene across the shared main-suite hub.

## ptone/scion#2278 (terminal persistence) overlap

Checked before starting. ptone/scion#2278 is implemented on fork PR
ptone/scion#2289 (`scion/term-persist` → `main`), open and not merged. It
touches `web/src/client/main.ts` at the import line, `ensureTerminalCoordinator()`,
and the `/terminals` branch of `renderRoute()`. It does not touch
`feature-flags.ts`, `header.ts`, `admin-server-config.ts`, or
`applyServerFeatureFlags`. This branch's `main.ts` edits stayed in the
import line, the `applyServerFeatureFlags` call site, and the
`TERMINAL_WORKSPACE_FLAG` literal replacement, with no overlap on the hunks
ptone/scion#2289 owns. The two branches merge cleanly; whichever lands
second rebases onto the other.

## Verification

- `npm ci` in `web/`: clean install.
- `npx vitest run` (full suite, default parallel): intermittent test/hook
  timeouts under worker-pool contention in this environment, a different
  subset of files each run (including, sometimes, the touched
  `admin-server-config.test.ts`), with comparable counts on a clean pre-1b
  base run the same way. No assertion ever fails, only timeouts, and every
  file passes individually and under `--no-file-parallelism`.
- `npx vitest run --no-file-parallelism` (full suite): all files, all tests
  pass.
- `npm run typecheck`: clean.
- `npx prettier --check` on touched files: clean, except
  `admin-server-config.ts`, `admin-server-config.test.ts`, and `header.ts`,
  which fail on pre-existing hunks outside this diff (fails the same way on
  a clean upstream-main worktree).
- `npx eslint` scoped to the touched `src/` and `e2e/` files: no new errors
  or warnings in `feature-flags.ts`, `server-feature-flags.ts`,
  `server-feature-flags.test.ts`, `admin-experiments.ts`, `open-terminal.ts`,
  `chat-members.ts`. `main.ts`, `admin-server-config.ts`, and `header.ts`
  show only pre-existing errors, unchanged at their pre-1b lines. Every
  `*.test.ts`/`*.spec.ts` file hits a pre-existing, repo-wide tsconfig gap
  (`tsconfig.json` excludes `src/**/*.test.ts`, so `.eslintrc`'s typed-lint
  project can't parse any test file); confirmed against an untouched file.
- `go build -p 2 ./...`: clean (web-only change; run to confirm nothing else
  regressed).
- Playwright `web/e2e/experiments.spec.ts`, main config, real hub: passes on
  desktop-chromium and mobile-chromium.
- Playwright terminal `*.pw.ts` suites (coordinator, entrypoints, hidden,
  pane, owner, lifecycle, workspace): every failure observed on this branch
  was reproduced on a clean upstream-main worktree with no 1b code, using
  the same test, same assertion, same failure line. None fails only with
  1b. The one borderline case, a timing-sensitive layout-restore test, was
  settled with a 20-run-per-branch, interleaved comparison showing an
  identical pass rate on both.

## Size

13 web files changed against upstream `main` (excluding this log, which
otherwise drifts the total with every fix commit): roughly 650 production
lines (`feature-flags.ts`, `server-feature-flags.ts`, `admin-experiments.ts`,
the four-change `admin-server-config.ts` edit, and a few literal swaps) and
1,100 test lines (`feature-flags.test.ts`, `server-feature-flags.test.ts`,
`admin-experiments.test.ts`, the `admin-server-config.test.ts` addition, and
the e2e spec). Most of the total is `admin-experiments.ts` and its test
file: an admin tab with four states (normal, 403, malformed, empty),
sequential-write semantics, and a test for each state and write path.

## Follow-up hardening (GoogleCloudPlatform/scion#2191)

Four defensive fixes for malformed or missing response fields in the admin
experiments tab and the boot flag fetch; one possible guard was left out
because `exec()` already handles that input.

- `setServerFlags()` read `window.__SCION_FEATURES__` and iterated its
  argument unconditionally: a non-browser caller, or a null or undefined
  argument, would throw, and a string or array argument would silently
  write junk numeric keys into the bag. Added the `typeof window` guard
  already used by `isFeatureEnabled()` and `setFeatureFlag()`, and return
  early on a null, non-object, or array argument.
- An experiment row without a `layers` array threw out of `render()` on
  `exp.layers.map()`. Defaulted to an empty array so the row still renders.
- `updated_by` is null only when `updated_at` is null too (no stored row,
  so no attribution is shown). When attribution renders, the only
  degenerate value is an empty string (a write with no caller email);
  that, and a missing value, now renders as "unknown" instead of blank
  attribution text.
- `typeof [] === 'object'`, so an array-shaped `experiments` field in the
  `/api/v1/experiments` response would pass the call site's object check
  and reach `setServerFlags()`. Covered by the `Array.isArray` guard added
  to `setServerFlags()` above, rather than a second check at the call
  site.
- The `issueUrl()` regex match was checked for a possible null-argument
  throw, but `RegExp.prototype.exec()` coerces its argument to a string and
  never throws on `null`/`undefined`/empty input; the existing
  `m ? ... : null` ternary and the render-time `nothing` fallback already
  handle every outcome. Left unchanged.

Each fix added or extended a vitest case, and each new case was confirmed
to fail against the pre-fix code (mutation check) before being restored
alongside the fix. Gates run: `npm run typecheck` (clean), `npx eslint` on
the touched source files and `npx prettier --check` on all touched files
(clean), and targeted `npx vitest run` across the touched test files (all
passing).
