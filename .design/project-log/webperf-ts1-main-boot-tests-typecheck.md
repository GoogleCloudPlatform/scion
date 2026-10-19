# webperf TS1: type-check the main.ts boot tests (ptone/scion#5328)

Base: C7 head `2c4dcfd` (stacked on PR #5324, on #5321, on #5318; none is
upstream yet). Part of the webperf epic (#5301). Follow-up asked for during
the C3/C4/C7 reviews. Config only; no change to tests or production code.

## What changed

- `web/src/client/tsconfig.client-tests.json` now includes the seven
  `main-*.test.ts` boot suites (`main-admin-status`, `main-auth-me`,
  `main-boot-waterfall`, `main-chat-flag-gate`, `main-terminal-lazy`,
  `main-terminal-load-failure`, `main-terminal-superseded`) and the two
  fixtures only they import (`__fixtures__/main-boot.ts`,
  `__fixtures__/terminal-workspace-fake.ts`). `npm run typecheck` already runs
  this project. The root `tsconfig.json` leaves out `*.test.ts` and
  `__fixtures__/**`, so before this change tsc never checked these files.
- The suites and fixtures were already type-clean under the strict root
  options (`exactOptionalPropertyTypes`, `noUnused*`), so no cast or fixture
  type change was needed. They went into the existing client-tests project:
  main.ts imports the Shoelace alert/dialog modules itself, so the
  `HTMLElementTagNameMap` workaround in the terminal-tests project is not
  needed here.

## How it was verified

- A deliberate `const x: number = "x"` in `main-terminal-lazy.test.ts` and in
  `__fixtures__/main-boot.ts` makes `npm run typecheck` fail (TS2322 in both
  files) with the new config. With the old config, the same edits pass
  typecheck. The edits were not committed.
- Clean `npm run typecheck` passes; vitest of the seven suites passes (8 tests).

## Learned / follow-ups

- The client and component test projects list files one by one, opt-in, so a
  new test file is not type-checked until someone adds it. About 50 other
  `web/src/client/*.test.ts` files (for example `api.test.ts`,
  `server-feature-flags.test.ts`, `hub-capabilities.test.ts`, the `state-*`
  and `agent-list-window*` suites) are still in no typecheck project. Adding
  them, or switching to a glob plus an exclusion list, is a possible
  follow-up. It would likely turn up real type errors to fix first.
- `__fixtures__/main-stub.ts` and `state-stub.ts` are used only by component
  tests that are not in a typecheck project, so they stay unchecked too.
