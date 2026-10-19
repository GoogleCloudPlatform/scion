# webperf A3: stop stale chunks piling up in the web build (#3842)

**Date:** 2026-10-19
**Branch:** scion/webperf-a3 (fork issue ptone/scion#5303, epic #5301)

## Problem

`npm run build` ended with `copy:client`, which copied `dist/client/assets/*`
into `web/public/assets/`. Vite's `publicDir: 'public'` copies `public/` back
into `dist/client` on the next build, so each build's chunks piled up in every
later build and in the hub binary that embeds `dist/client`. Nothing reads
`public/assets` (repo-wide grep: only `copy:client` itself, plus two Dockerfile
comments and a README row).

## What changed

- `web/package.json`: removed `copy:client` and its uses in `build` and
  `build:dev`. Added `prebuild` and `prebuild:dev`, both running
  `scripts/clean-public-generated.mjs`, so trees that already hold the debris
  are cleaned. Added `check:orphans` and `test:build-scripts`.
- `web/scripts/clean-public-generated.mjs`: removes `web/public/assets/`. It
  refuses (exit 1, deletes nothing) if git tracks any file under it. If git is
  not installed or the tree is not a checkout (Docker frontend stage), nothing
  can be tracked, so it proceeds. Any other git error fails closed.
- `web/scripts/check-orphans.mjs`: exits 1 if any `.js` in
  `dist/client/assets` is not the `file` of an entry in
  `dist/client/.vite/manifest.json`.
- node:test suites for both scripts. CI runs them plus `check:orphans` in a
  separate "Check Web Build Orphans" step after the web build.

## Learnings

- npm pre-hooks are name-based: `prebuild:dev` fires for `npm run build:dev`
  (checked: the build log shows `> prebuild:dev`), and `prebuild` does not.
  Both are skipped under `--ignore-scripts`. The Dockerfiles use that flag only
  on `npm ci`, not on `npm run build`.
- Clean build: 140 `.js` files in `dist/client/assets`, 140 distinct `.js`
  manifest files (one entry). This held across consecutive builds.
- `vite build` under `ulimit -v 8000000` aborted once with `std::bad_alloc`
  while computing gzip sizes (one in four runs). The address-space cap, not real
  memory use, is the limit, so a LIGHT gate window that runs the build should
  expect this.
