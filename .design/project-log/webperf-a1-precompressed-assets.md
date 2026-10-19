# webperf A1: precompressed static assets and strong validators

**Date:** 2026-10-19
**Branch:** scion/webperf-a1 (ptone/scion#5302, epic #5301)

## What changed

- `web/scripts/precompress.mjs` runs after `vite build` in `npm run build` and
  `build:dev`. It writes `file.br` (brotli q11) and `file.gz` (gzip -9, zero
  header mtime) next to each compressible file of 1 KiB or more (not source
  maps), keeping a variant only when it is under 95% of the raw size.
  `precompress.test.mjs` (`node --test`) checks the script and, after a build,
  the real output: every variant decodes byte-identical, and the byte budgets
  hold (main.js.br <= 85,000 B; shoelace-*.js.br <= 50,000 B; entry closure
  <= 160,000 B br). First build: main.js.br 79,775 B, shoelace 47,111 B,
  entry closure 139,488 B (4 files).
- New `pkg/webassets` (no pkg/hub dependency). `Index` hashes the embedded
  build once (sha256, ETag = base64url of the first 12 bytes, with `-br`/`-gz`
  inside the quotes for variants) and serves through `http.ServeContent`, so
  304s, Range and HEAD come for free. `DiskIndex` (`--web-assets-dir`) uses a
  weak size/mtime validator and ignores a sibling older than its source.
  `Negotiate` picks br > gzip > identity, honouring q=0 and `*`.
- `pkg/hub`: `serveStaticAsset` (and through it `tryServeStaticFile`) calls the
  index first and falls back to the unchanged `http.FileServer`, so directory
  listings and unindexed paths behave as before. Cache-Control is unchanged
  (now computed by `staticCacheControl`). Sibling names (`*.br`, `*.gz`) are
  never served by their own name. The disk fallback opens through `os.Root`
  (`webassets.DirFileSystem`), so no path or symlink leaves the directory.

## Learnings

- Many pkg/hub tests assign `ws.assets` / `ws.assetsDisk` after
  `NewWebServer`. The index therefore remembers the source it was built from
  and rebuilds (under a mutex, lock-free fast path via `atomic.Pointer`) when
  the source differs; FS identity is compared with `sameFS` (pointer identity
  for maps/pointers, `==` for comparable values, never a hash). The embedded
  build's index is built once per process and shared by every WebServer.
- `http.ServeContent` omits Content-Length when Content-Encoding is set;
  net/http then sizes or chunks the body itself.
- Go has no brotli decoder in this module, so the hub test checks the br body
  byte for byte against the stored variant; real decoding is proven in node.
- `vite build` aborts with `std::bad_alloc` under `ulimit -v 8000000` unless
  `NODE_OPTIONS=--max-old-space-size=3072` is set.
- Every release and image path (Makefile `web`, omni and cloudrun
  Dockerfiles, build-release.yml, ci.yml, web-e2e.yml) runs `npm run build`,
  so appending the step to `build` covers them all (design open question 9.2).

## Follow-ups (other phases)

A3 removes `copy:client` (stale chunks are currently precompressed too), A4
closes the directory listing, A6 changes the cache policy function.
