# App logo (ptone/scion#2570)

Sources for the Scion app logo and the PWA icon set in `web/public/`.
The maintainer picked direction C (slate tile, single bold leaf on a
curved stem) from three candidates; the other two were removed. All
marks are drawn with paths only (no emoji `<text>`) on a 512x512 canvas.

| File | Use |
|------|-----|
| `logo.svg` | Direction C, 112 px corner radius. A subtle lighter inner edge keeps the tile visible on dark backgrounds. Used for `icon-192.png` and `icon-512.png`. |
| `logo-small.svg` | Simplified for 16-48 px: flat colours, no midrib, thicker stem, larger leaf, lighter edge. Used for `favicon.svg` and `favicon.ico`. |
| `logo-maskable.svg` | Full-bleed square, mark scaled to 86% so it stays inside the central 80% safe-zone circle. Used for `icon-maskable-512.png` and `apple-touch-icon.png` (iOS applies its own corner mask). |

## Regenerating the icons

```sh
web/design/app-logo/generate-icons.sh
```

This rasterizes the SVGs with headless Chromium (`CHROME` overrides the
binary) and packs `favicon.ico` (16/32/48 PNG entries) with a short
standard-library `python3` snippet. It writes into `web/public/`:
`favicon.svg`, `favicon.ico`, `apple-touch-icon.png` (180),
`icon-192.png`, `icon-512.png` and `icon-maskable-512.png`. Commit the
outputs after editing any `logo*.svg`.

## Rendering previews

```sh
web/design/app-logo/render-previews.sh [output-dir]
```

This uses headless Chromium with `preview-sheet.html` to write one PNG
per logo SVG, showing 16 px (plus a 6x zoom of the 16 px raster), 32,
180 and 512 px on light and dark backgrounds. The PNGs are not
committed.

## Serving

- `web/public/manifest.webmanifest` lists the 192/512 icons, the
  maskable icon and `favicon.svg`. `web/index.html` and the production
  shell (`spaShellTemplate` in `pkg/hub/web.go`) carry the same icon,
  apple-touch-icon, manifest and theme-color tags between
  `app-icons:start`/`app-icons:end` markers (`TestSPAShellAppIconTags`).
- Root-level files skip session auth (`isRootLevelStaticFile`).
  Admin (maintenance) mode lets through exactly the paths in
  `appIconPaths` (`pkg/hub/admin_mode.go`). Adding an icon means
  adding it there too; `TestAppIcons_AllowlistMatchesPublicFiles`
  catches drift.
- `serveStaticAsset` sets `image/x-icon` and
  `application/manifest+json`, which Go's built-in MIME table lacks.
