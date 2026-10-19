# Project Log: Keep web source maps out of the served build

**Date:** 2026-10-19
**Task:** Stop serving and embedding the web client's `.js.map` files while still producing them for debugging (ptone/scion#5347, webperf design phase A5, option (a)).

## Changes
- `web/vite.config.ts`: `build.sourcemap: 'hidden'`. Vite still writes the maps but drops the `//# sourceMappingURL` comment from the bundles.
- `web/scripts/move-sourcemaps.mjs`: moves every `dist/client/**/*.map` to the same relative path under `dist/sourcemaps/` (emptied first). It fails closed: exit 1 if any `.map` (or `.map.br`/`.map.gz`) file or any symbolic link (never followed) is left under `dist/client`, if any `.js`/`.css` there still has a `sourceMappingURL` comment (`[#@]\s*sourceMappingURL=`, so code that only mentions the word does not fail the build), or if no map was found. The error names each file left behind and says to remove `web/public/assets` if it holds chunks from an earlier build. Directories that overlap are refused.
- `web/package.json`: `build` and `build:dev` run the move right after `vite build` (chained with `&&`, so a failure fails the build) and before `copy:client`. New `test:move-sourcemaps` (node:test, `scripts/move-sourcemaps.test.mjs`).
- `.github/workflows/ci.yml` (Build & Test): checks the build output on every run, and uploads `web/dist/sourcemaps/` as the `web-sourcemaps` workflow artifact only for pushes to main, with 7-day retention.

## Where the maps can and cannot end up
- `web/embed.go` embeds `all:dist/client`, so the binary no longer carries maps.
- `Dockerfile` and `Dockerfile.hub` copy only `/web/dist/client` from the frontend stage. `image-build/omni/Dockerfile` builds web and Go in a builder stage and copies only the binary into the final image. `build-release.yml` uploads only binary tarballs. All of them run `npm run build`, so none of them carries maps.
- The CI artifact (pushes to main only) is the only remaining copy. On a public repository, workflow artifacts are downloadable by signed-in GitHub users; the maps contain only the TypeScript sources already in the repository.

## Measurements
- Clean build: 140 maps, 14,351,952 bytes moved out of `dist/client` (19.9 MB to 5.6 MB).
- Compiled `./web` package archive (the embed): 20,095,218 to 5,696,018 bytes (-14,399,200 bytes, about -13.7 MiB). The binary shrinks by about the same amount because `embed` stores files uncompressed.

## Learnings
- Keeping the `assets/` subdirectory under `dist/sourcemaps` keeps the maps at the same depth as the bundles, so their relative `sources` paths (`../../../src/...`) still resolve. Bundle attribution decoding the VLQ mappings of `dist/sourcemaps/assets/main.js.map` against `dist/client/assets/main.js` reproduces the findings' per-module numbers (terminal-workspace-root 51,511 bytes, terminal-pane 45,684, header 35,945).
- A stale `web/public/assets` from builds before #3842 is fixed holds chunks that still end in `//# sourceMappingURL`. Vite copies them into `dist/client`, so the move fails the build; the error message says to remove that directory.
- `vite build` under `ulimit -v 8000000` with a 3072 MB heap aborted once with `std::bad_alloc` while rendering chunks and passed on the next run. Treat a single abort there as an environment limit, not a regression.
- Order with the other build-chain changes: maps must leave `dist/client` before any precompression step, so no `.map.br`/`.map.gz` is produced, and before `copy:client` while it still exists, so `public/assets` never receives maps (Vite copies `public/` back into `dist/client` on the next build).
