# Web shell boot hints: modulepreload and early head flush

**Date:** 2026-10-19
**Branch:** scion/webperf-a7

## Problem

The SPA shell (`spaShellTemplate` in `pkg/hub/web.go`) was rendered in one
piece after `prefetchPageData`, which can wait up to 2 seconds for the
in-process API call on detail routes. The browser received no bytes until the
prefetch finished, found `/assets/main.js` only at the end of the body, and
discovered the entry's static imports (the `shoelace` and `lit` chunks) only
after downloading and parsing `main.js`.

## Change

- The template has two named parts. `head` holds everything up to `</head>`
  and no per-request data. It now carries one `<link rel="modulepreload">`
  for each chunk the entry imports statically, plus the entry
  `<script type="module" src="/assets/main.js">`, moved up from the body.
  `body` holds the app element and the `__SCION_DATA__` script, now the last
  element of the body. Executing the template itself still renders both
  parts, so tests that read or render the whole shell are unchanged.
- `spaHandler` sets every header, executes `head`, flushes through
  `http.NewResponseController(w).Flush()`, then runs `prefetchPageData` and
  executes `body`.
- The Vite manifest is read once at startup (`readViteManifest`), shared by
  the existing fingerprinted-asset set and the new preload list
  (`shellModulePreloadsFromManifest`). The preload list walks the entry's
  static `imports` (manifest keys, looked up to their files) depth first,
  leaves out the entry, dynamic imports and CSS, and keeps only clean
  fingerprinted `/assets/*.js` paths made of URL-safe characters.
- A panic inside the in-process prefetch call now degrades to no page data,
  as a timeout or error status already did. Because the head has already
  been sent, a panic would otherwise end the document early.
- Client: `readInitialPageData(doc)` moved from `main.ts` to
  `ssr-page-data.ts` so a vitest can check the hand-off with the data script
  at the end of the body.

## Learnings

- Module scripts are deferred, so a data script placed after the entry script
  is still in the DOM when the entry runs. That is what makes the early flush
  safe.
- The real build's entry chunk has no CSS of its own. The Shoelace theme CSS
  belongs to the `shoelace` chunk, and Vite's preload helper adds it at
  runtime when a lazy chunk that needs it loads. Linking it from the shell
  would change when it applies and where it sits in the cascade (a visible
  change), so the shell links no stylesheet. Fonts are unchanged.
- Vite manifest `imports` are manifest keys, not file paths.
- All the shell tests are in pkg/hub hub shard 6
  (`hack/hubshard/hubshard.sh -locate`).
