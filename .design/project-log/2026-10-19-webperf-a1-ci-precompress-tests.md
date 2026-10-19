# webperf A1 follow-up: run the precompress script tests in CI

**Date:** 2026-10-19
**Branch:** scion/webperf-a1-ci (ptone/scion#5378, part of #5301)

## What changed

- `web/package.json`: `test:build-scripts` now also runs
  `scripts/precompress.test.mjs`, next to the A3 suites
  (`clean-public-generated`, `check-orphans`).
- `web/README.md`: the script table notes that the precompress suite also
  checks the output of a prior build.

## Why this is enough

CI's "Check Web Build Orphans" step (ci.yml) already runs
`npm run test:build-scripts` right after `npm run build`. So the suite's unit
tests and its real-build checks (every `.br`/`.gz` sibling decodes to its
source; the A1 byte budgets for `main.js.br`, the Shoelace chunk and the
entry closure) now run against CI's build output, with no workflow change.

## Notes

- The byte budgets are now a CI gate: bundle growth past them fails
  Build & Test. Measured on this build: main.js.br 79,708 B (limit 85,000),
  shoelace 47,111 B (50,000), entry closure 139,421 B (160,000).
- Without a build the real-build checks skip, so a local
  `npm run test:build-scripts` on a fresh checkout still passes. Making a
  missing build fail in CI would mean setting `REQUIRE_BUILD=1` in the
  workflow step; not done here, since CI always builds first.
