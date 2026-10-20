# webperf A4: no directory listings, no public dot-paths

**Date:** 2026-10-19
**Branch:** scion/webperf-a4 (ptone/scion#5346), stacked on A1 (scion/webperf-a1)

## What changed

- `serveStaticAsset`: after the precompressed index misses, a request that
  names a directory of the asset source (`/assets/`, `/shoelace/assets/icons/`,
  with or without the trailing slash) gets the plain 404 the handler already
  uses for refused paths. `http.FileServer` is now only reached for files, so
  it never lists a directory or serves a directory's `index.html`.
- Dot segments: `hasDotSegment` refuses any path with a segment starting with
  `.` in `serveStaticAsset` (404 under `/assets/` and `/shoelace/`) and in
  `tryServeStaticFile` (never a file). The SPA catch-all returns 404 when the
  first segment starts with a dot (`/.vite/manifest.json`, `/.env`); a deeper
  dot segment (`/projects/x/artifacts/.env`) can be a client-side route and
  still gets the shell. `/.well-known/` has its own mux entry and is
  unaffected.
- `loadFingerprintedAssets` still reads `.vite/manifest.json` from the asset
  source; only its public URL is gone.
- Tests: `web_static_listing_test.go` (`TestStaticAsset_DirectoryListing404`,
  `TestStaticFile_DotPath404`, `TestFingerprintedAssets_StillLoadedFromManifest`,
  embedded and disk mode). A1's `TestStaticAsset_DirectoryFallbackUnchanged`
  is removed (its disk symlink case moved into the new listing test) and
  `TestStaticAsset_SiblingNameForms` now expects the redirect landing on
  `/assets/` to be a 404.

## Learnings

- Root-level dot paths reach the SPA catch-all, which sits behind session
  auth: in tests without dev auth they return 401, so root-path checks use
  the dev-auth server, as the A1 sibling-name test does.
- Fork issue ptone/scion#3843 (asset path normalization) overlaps: its
  encoded dot-segment part was done by A1 (non-clean decoded paths refused),
  its dot-segment part by A4, and its disk existence check by A1's
  `os.Root`-backed `DirFileSystem`.
