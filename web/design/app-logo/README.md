# App logo directions (ptone/scion#2570)

Design sources for the Scion app logo and PWA icon set. Nothing here is
wired into the app yet: these files are the inputs for picking a
direction. All marks are drawn with paths only (no emoji `<text>`) on a
512x512 canvas with a 112 px corner radius.

| File | Direction |
|------|-----------|
| `direction-a.svg` | Green tile, white two-leaf sprout |
| `direction-b.svg` | Scion-blue tile (`--scion-primary-500/700`), green leaves on a white stem growing from a branch node |
| `direction-b-small.svg` | B simplified for 16/32 px: ring dropped, thicker stem, flat leaves |
| `direction-c.svg` | Slate tile, single bold green leaf on a curved stem |

Status: previews sent for a pick. roadmap-lead recommended B, with
`direction-b-small.svg` for the 16/32 px sizes.

## Rendering previews

```sh
web/design/app-logo/render-previews.sh [output-dir]
```

This uses headless Chromium (`CHROME` overrides the binary) with
`preview-sheet.html` to write one PNG per direction, showing 16 px (plus
a 6x zoom of the 16 px raster), 32, 180 and 512 px on light and dark
backgrounds. The PNGs are not committed. Copies of the previews sent for
the pick are in
`gs://scion-xproject-exchange/small-issues/i2570-previews/`.

## Notes for wiring in the chosen mark

- Assets to generate: `favicon.svg`, `favicon.ico` (16/32/48 PNG
  entries), `apple-touch-icon.png` (180), `icon-192.png`,
  `icon-512.png`, and `icon-maskable-512.png` (mark scaled to fit the
  central 80% safe zone on a full-bleed background, no rounded corners).
  For B, use the small variant for the favicons.
- Keep every icon and `manifest.webmanifest` at the root of
  `web/public/`. Root-level files with an extension already skip
  session auth (`isRootLevelStaticFile` in `pkg/hub/web.go`), and
  `spaHandler` serves them through `tryServeStaticFile`.
- Admin (maintenance) mode only allows `/favicon.ico` through
  (`adminModeWebMiddleware` in `pkg/hub/admin_mode.go`), so even today's
  `/favicon.svg` is blocked there. Reusing `isRootLevelStaticFile` in
  that check would let the manifest and icons through.
- Go's built-in MIME table has no `.webmanifest` or `.ico` entries, so
  `serveStaticAsset` should set `application/manifest+json` and
  `image/x-icon` explicitly. Add a test for unauthenticated GETs,
  including in admin mode.
- The production SPA shell (`spaShellTemplate` in `pkg/hub/web.go`)
  has no icon `<link>` at all; add the icon, apple-touch-icon, manifest
  and theme-color tags there as well as in `web/index.html`.
- `@playwright/test` is already a dev dependency and can rasterize the
  SVGs for a reproducible generation script without adding dependencies.
