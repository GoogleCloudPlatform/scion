# Experiments Phase 1b — web: client precedence and Experiments tab (ptone/scion#2217)

Branch: `scion/experiments-1b`, based on `scion/experiments-1a-ii` (fork PR
ptone/scion#2360, head `75c3f13b` after rebase).

## Scope

Web-only. No Go changes.

- `web/src/utils/feature-flags.ts`: `setServerFlags()` with lazy pinning
  (values already in `window.__SCION_FEATURES__` before the first call are
  never overwritten), `resetServerFlagStateForTests()`, a shadowed-override
  `console.info` (once per flag per page load, deduped in the same reset
  hook), and the shared `TERMINAL_WORKSPACE_FLAG` export.
- `web/src/client/main.ts`: `applyServerFeatureFlags()` now fetches
  `/api/v1/experiments` and `/api/v1/settings/public` in parallel with
  `Promise.allSettled`; a non-OK status, network error, or a non-JSON 200
  body on either side is treated as "no data" for that side only — the other
  side's effect still applies. `applyServerFeatureFlags` is re-exported (as
  `applyServerFeatureFlagsForTests`) for direct unit testing, since importing
  `main.ts` otherwise runs the whole app boot as a side effect.
- `web/src/components/pages/admin-experiments.ts` (new):
  `<scion-admin-experiments>`. Self-contained — owns its own fetch, state and
  writes. Lazy-loads on the first time its `.active` property becomes true.
  States: normal, 403 (permission message, no switches), malformed (banner +
  confirmed reset-all), and empty. Writes are strictly sequential (every
  switch and the reset button are disabled while one write is in flight, so
  the next write always carries the previous one's revision). A 409 reverts
  and shows a message; any other write failure reverts and reloads with GET.
  "Reset to default" shows only when an override exists. Tab-level
  attribution ("Last changed by … at …") comes from the last write response.
  A one-line note lists any `unknown_overrides` by name.
- `web/src/components/pages/admin-server-config.ts`: exactly the four
  changes in the design — one import, the Experiments `<sl-tab>` last in the
  nav, its `<sl-tab-panel>` last, and the "Save & Reload"/"Reset" actions bar
  plus the harness-config error message are wrapped in one
  `activeTab !== 'experiments'` condition so both are hidden on that tab.
- `web/src/client/open-terminal.ts` and
  `web/src/components/shared/chat/chat-members.ts`: the `'web.terminal_workspace'`
  string literal is replaced by the shared `TERMINAL_WORKSPACE_FLAG` import.
- `web/src/components/shared/header.ts`: the module-local
  `TERMINAL_WORKSPACE_FLAG` constant is deleted; the shared one is imported
  instead, so the two never exist side by side.
- Tests: `feature-flags.test.ts` (precedence matrix, lazy pinning, shadowed-
  override dedup), a new `main.test.ts` (the parallel boot fetch and its
  failure modes — this function lives in `main.ts`, which has no prior test
  file, so a small new one was added specifically for it), a new
  `admin-experiments.test.ts` (every state and write path above), and an
  addition to `admin-server-config.test.ts` for the tab position and the
  actions-bar/harness-message hiding.
- `web/e2e/experiments.spec.ts` (new): under the main Playwright config,
  against the real hub. Asserts `override: null` up front (fails fast
  otherwise), toggles `web.terminal_workspace` off through the tab, waits for
  the PUT 200 and the "Last changed by" line, confirms
  `/agents/<uuid>/terminal` stays put, re-enables through the API, confirms
  the rewrite to `/terminals/<uuid>`, and always PUTs `null` back in
  `finally` for state hygiene across the shared main-suite hub.

## ptone/scion#2278 (terminal persistence) overlap — findings

Checked before starting, per the brief:

- ptone/scion#2278 is open, unassigned to this stream. Its implementation is
  on fork PR ptone/scion#2289 (`scion/term-persist` → `main`), also open, not
  merged, and not found on GoogleCloudPlatform/scion.
- PR #2289 touches `web/src/client/main.ts` at exactly the areas the brief
  named: the import line (adds `TerminalWorkspacePersistence`,
  `restoreUrlIntent`), `ensureTerminalCoordinator()` (adds
  `terminalPersistence`, extends the `create` adapter signature with an
  `options` param), and the `/terminals` branch of `renderRoute()` (adds a
  `terminalPersistence.restore()` call before the URL-driven layout code).
  It does not touch `feature-flags.ts`, `header.ts`,
  `admin-server-config.ts`, or `applyServerFeatureFlags`.
- This branch's `main.ts` edits were kept to the agreed area only: the
  import line, `applyServerFeatureFlags()`, the
  `isFeatureEnabled('web.terminal_workspace')` → `TERMINAL_WORKSPACE_FLAG`
  literal replacement, and no edits inside `ensureTerminalCoordinator` or the
  `/terminals` branch of `renderRoute`. No hunks from #2289 were touched.
- Since #2289 has not landed, this branch rebases onto it (or onto whichever
  lands second), per the brief's rule.

## Verification (pre-halt)

A shared-infra disk-space incident (broker-01) paused all builds/tests
mid-review; gates below are what completed before the halt.

- `npm ci` in `web/`: clean install.
- `npx vitest run` (full suite): **2821 tests passed across 100 files** (run
  with `--no-file-parallelism` — the default parallel run hit unrelated
  worker-pool timeouts from sandbox resource contention, not real failures;
  every file that timed out under parallelism passed individually/serially).
- `npm run typecheck`: clean.
- `go build -p 2 ./...`: clean (web-only change; run to confirm nothing else
  regressed).
- `npm run lint`: run twice, but both invocations were piped through `tail`
  and the earlier portion of the output (alphabetically, everything before
  `src/shared/...`) was lost before the halt landed. The captured tail shows
  only pre-existing errors in files this branch does not touch (parsing
  errors from `.test.ts` files not covered by `tsconfig.json` — a repo-wide,
  pre-existing config gap — plus unrelated warnings/errors in
  `audio.ts`, `lineage.ts`, `markdown.ts`, `message-mode.ts`,
  `terminal-pane.ts`, `view-toggle.ts`). **Not yet confirmed clean
  specifically against this branch's changed files**; re-run scoped to them
  (`npx eslint <files>`) once builds are cleared.
- Playwright (`experiments.spec.ts` and the existing terminal `*.pw.ts`
  suites): **not run yet** — blocked by the halt from the start pending the
  em's clearance, as instructed.

## Size

12 files changed, 1501 insertions / 53 deletions (`git diff --stat` against
`scion/experiments-1a-ii`). Above the ~550–700 line M target in the brief;
`admin-experiments.ts` (416 lines) plus its test file (396 lines) account
for most of the overrun — a full CRUD admin tab with four states (normal,
403, malformed, empty) and sequential-write semantics. Trimmed the
component's CSS once already (75 → ~45 lines); further cuts risk losing
state coverage rather than boilerplate, so size is reported as-is per the
brief rather than force-fit to the target.
