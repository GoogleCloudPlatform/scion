# webperf A6: immutable caching for fingerprinted assets

**Date:** 2026-10-19
**Branch:** scion/webperf-a4 (ptone/scion#5348, combined with A4 #5346 in one PR), stacked on A1

## What changed

- `staticCacheControl` now has three classes:
  - listed in the Vite manifest (`ws.fingerprintedAssets`):
    `public, max-age=31536000, immutable`. Vite names these by content
    hash, so a changed file always gets a new URL;
  - a hex-hashed name the manifest does not list (`isHashedAsset` alone):
    `public, max-age=86400`, as before. The name only looks
    content-addressed;
  - everything else (the entry `assets/main.js`, icons, root files):
    `no-cache`, as before. Revalidation is cheap since A1 added ETags.
- The policy is applied by `webassets.writeHeaders` before
  `http.ServeContent`, so the br/gzip variants, the 304 of a conditional
  request and HEAD all carry the same value. The tests check each of them.
- Tests: `web_static_cache_test.go` (`TestCacheControl_ManifestAssetImmutable`,
  `TestCacheControl_HexHeuristicOneDay`, `TestCacheControl_EntryNoCache`,
  embedded and disk mode). `TestStaticAssetFingerprintedCaching` now expects
  the immutable value for manifest-listed files and one day for the
  unlisted hex name.

## Learnings

- DiskIndex ignores a sibling older than its source, so disk fixtures that
  write a file and its `.br`/`.gz` siblings in map order can drop a variant
  when the clock ticks between writes. The new fixture sets one shared
  mtime with `os.Chtimes`, and A1's `writeValidatorDisk` got the same fix
  in its own test-only commit.
