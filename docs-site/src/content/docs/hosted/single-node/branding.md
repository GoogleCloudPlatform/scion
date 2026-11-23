---
title: Branding & White-Labeling
description: Customize a Scion Hub's product name, logos, icons, color palette, corner style, and typeface.
---

Hub administrators can white-label a Scion Hub's web interface, pre-auth pages, PWA manifest, and CLI login callback using the `server.hub.branding.*` operational settings. Branding is **hub-wide** and works in Workstation, Single-node hosted, and HA hosted deployments.

Configure and manage branding in the Web Admin UI at **Admin > Branding** (`/admin/branding`), or via `scion hub settings`, the [Admin Server Config API](#admin-server-config-api), `settings.yaml`, or `SCION_SEED_*` environment variables.

## What Branding Covers

When `server.hub.branding` is customized, the Hub applies your branding across:

- **Web SPA**: browser `<title>`, `/login`, `/invite`, `/unauthorized`, `/onboarding`, sidebar navigation (`<scion-nav>` and `<scion-profile-nav>`), Chat header, Terminal Workspace header, sandboxed artifact banner, and OS desktop notifications.
- **Hub-rendered Go pages**: maintenance (`503`), suspended account (`403`), port-proxy error pages, and the no-assets fallback page.
- **PWA manifest and icons**: dynamic `GET /manifest.webmanifest`, `<link rel="icon">`, `<link rel="apple-touch-icon">`, and `<meta name="theme-color">`.
- **CLI browser login callback**: `scion hub auth login` performs a best-effort `GET /api/v1/settings/public` (1.5-second timeout) to theme the localhost OAuth callback page, falling back to stock defaults if unreachable.

### What Stays "Scion"

Audit, cloud, and protocol identifiers always remain `"Scion"` regardless of branding settings:

- GCP service account prefixes and cloud resource identifiers
- Git commit authorship (`Scion Agent`)
- Session cookies (`scion_sess`), `localStorage` keys, and `scion://` URIs
- CLI binary name and command help text
- Chat bridge integrations (Slack, Discord, Teams, Telegram)

## Configuration Reference (`server.hub.branding`)

All branding keys belong to the Layer-1 **`branding`** section under `server.hub.branding`. You can inspect the live JSON Schema on any Hub with `scion hub settings schema branding`.

### Identity

| Key | Type | Default | Limits & Behavior |
| :--- | :--- | :--- | :--- |
| `name` | string | `"Scion"` | 1–64 Unicode characters (counted on the raw value), not blank; no control or bidirectional control characters. Leading and trailing whitespace is ignored when displayed. |
| `short_name` | string | `name` (truncated to 24 chars) | 1–24 Unicode characters (counted on the raw value), not blank; no control or bidirectional control characters. Leading and trailing whitespace is ignored when displayed. Used in `manifest.webmanifest`. |
| `subtitle` | string | `""` | 0–80 characters; no control or bidirectional control characters. Shown on `/login` and as the manifest `description`. When `name` is customized and `subtitle` is omitted, stock Scion's tagline is hidden. |
| `attribution` | enum | `"shown"` | `"shown"` or `"hidden"` (string enum, not a boolean). When `"shown"` and custom identity (`name != "Scion"` or a custom logo) is active, renders `"Powered by Scion"` on `/login`, `/invite`, expanded sidebar footers, Hub-rendered status pages, and the CLI callback. Omitted in full-viewport Chat/Terminal views and on unbranded `"Scion"` Hubs. |

### Shape & Typography

| Key | Type | Default | Accepted Values |
| :--- | :--- | :--- | :--- |
| `corners` | enum | `"default"` | `"default"` (`0.25rem`–`1rem`), `"square"` (`0`), or `"rounded"` (`0.375rem`–`1.25rem`). Applies across the SPA, Shoelace components, Go status pages, and CLI callback. Pill and circular elements (toggle switches, radio buttons, avatars, status dots, pill badges) always keep their rounded shape. |
| `font_sans` | string | `"default"` | Controls `--scion-font-sans` across the UI. Accepted values: `"default"` (Inter via Google Fonts), `"system"` (native OS sans-serif stack; drops the Google Fonts Inter stylesheet, while the JetBrains Mono stylesheet and preconnect hints remain and fall back to local monospace fonts when unreachable), a curated preset slug (`"ibm-plex-sans"`, `"plus-jakarta-sans"`, `"dm-sans"`, `"source-sans-3"`), or any Google Fonts family name matching `^[A-Za-z0-9]([A-Za-z0-9 -]{0,62}[A-Za-z0-9-])?$` (1–64 ASCII characters, no leading/trailing whitespace). |

Monospace typography (`--scion-font-mono`, JetBrains Mono) is intentionally unaffected so code editors and `xterm.js` terminals preserve column alignment.

### Logos & Icons

| Key | Slot (`/brand/{slot}`) | Fallback | Notes |
| :--- | :--- | :--- | :--- |
| `logo.light` | `logo-light` | `logo.dark` (if set) | Logo displayed in light mode. |
| `logo.dark` | `logo-dark` | `logo.light` (if set) | Logo displayed in dark mode. |
| `icons.favicon` | `favicon` | Stock `/favicon.ico` + `/favicon.svg` in `<head>` | When set, replaces both stock favicon `<link>` tags in `<head>`. Also affects `/manifest.webmanifest` (see below). |
| `icons.apple_touch_icon` | `apple-touch-icon` | `icons.icon_192`, then stock `/apple-touch-icon.png` in `<head>` | Recommended 180×180 PNG. `data:` URIs must be `image/png`. |
| `icons.icon_192` | `icon-192` | Stock `/icon-192.png` only when all manifest icons are unset | 192×192 PNG for PWA manifest and desktop notifications. `data:` URIs must be `image/png`. |
| `icons.icon_512` | `icon-512` | Stock `/icon-512.png` only when all manifest icons are unset | 512×512 PNG for PWA manifest. `data:` URIs must be `image/png`. |
| `icons.icon_maskable_512` | `icon-maskable-512` | Stock `/icon-maskable-512.png` only when all manifest icons are unset | 512×512 maskable PNG for Android adaptive icons. `data:` URIs must be `image/png`. |

If any of `icons.favicon`, `icons.icon_192`, `icons.icon_512`, or `icons.icon_maskable_512` is set, `/manifest.webmanifest` lists **only** the configured icons and drops all stock manifest icons (for example, setting only `icons.favicon` emits a manifest containing only that favicon). Configure `icons.icon_192` and `icons.icon_512` together to keep the Hub installable as a PWA.

OS desktop notifications resolve their icon in order: `icons.icon_192` → `icons.apple_touch_icon` → `icons.favicon` → stock `/scion-notification-icon.png`.

#### Accepted Asset URI Formats & Limits

Every logo and icon field accepts one of three URI forms:

1. **HTTPS URL (`https://...`)**:
   - Maximum length: **2,048 characters**.
   - Must have a non-empty host and no user credentials, whitespace, control characters, quotes, parentheses, angle brackets, or backslashes.
   - Plain `http://` URLs are rejected (`422`) because the Hub's `Content-Security-Policy` restricts `img-src` to `'self' data: blob: https:`.
2. **Same-origin path (`/...`)**:
   - Maximum length: **512 characters**.
   - Must start with a single `/` (protocol-relative `//` is rejected), contain no `%`, `?`, `#`, `\`, `.` or `..` segments, and use only ASCII `[A-Za-z0-9._@-]` per path segment.
   - Must end with `.svg`, `.png`, `.webp`, or `.ico`.
   - Must not target reserved Hub route prefixes (`/auth`, `/api`, `/brand`, `/events`, `/.well-known`, `/health`, `/healthz`, `/readyz`).
3. **Base64 `data:` URI (`data:image/<subtype>;base64,<payload>`)**:
   - Allowed media types: `image/svg+xml`, `image/png`, `image/webp`, and `image/x-icon` (`image/vnd.microsoft.icon` is normalized to `image/x-icon`).
   - **`apple_touch_icon`, `icon_192`, `icon_512`, and `icon_maskable_512` require `image/png`** when supplied as `data:` URIs.
   - **Size limits**: **128 KiB** decoded per asset and **512 KiB** decoded total across all `data:` assets in `server.hub.branding`.
   - **Content verification**: Binary images are validated against PNG, RIFF/WEBP, or ICO magic bytes. SVGs must be valid UTF-8 XML restricted to the SVG namespace using only **allowlisted static SVG elements** (rejecting `DOCTYPE`/`ENTITY`, processing instructions, `on*` event attributes, CSS backslash escapes, external `href` or `url()` references, and `@import`, `expression()`, `image-set()`, `javascript:`, `vbscript:`, or `data:` in styles).

#### How `/brand/{slot}` and `/manifest.webmanifest` Are Served

`data:` URI assets are decoded once in memory and served from unauthenticated routes at `GET /brand/{slot}?v={sha8}` (where `{sha8}` is the first 8 hex characters of the asset's SHA-256 digest):

- Responses carry `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, and a strict sandboxed `Content-Security-Policy: default-src 'none'; img-src data:; style-src 'unsafe-inline'; sandbox`.
- Requests with a matching `?v={sha8}` query parameter are cached with `Cache-Control: public, max-age=31536000, immutable`; requests without a matching version receive `Cache-Control: no-cache` and an `ETag`.
- `GET /manifest.webmanifest` is rendered dynamically from the active branding snapshot (`Content-Type: application/manifest+json`, `Cache-Control: no-cache`, and content-hash `ETag`). Its `theme_color` and `background_color` use the resolved `palette.dark.surface` when custom colors are active (default `#1e293b`).

Both `/brand/*` and `/manifest.webmanifest` bypass web session authentication and maintenance-mode blocking so `/login`, browser favicon fetches, and maintenance pages render with custom branding.

### Colors & WCAG 2.1 AA Contrast Validation

All color values must be 6-digit `#RRGGBB` hex strings.

| Key | Type | Description |
| :--- | :--- | :--- |
| `accent_color` | `#RRGGBB` | Shorthand brand color. Generates a deterministic 11-step (`50`–`950`) OKLCH primary shade ramp anchored to calibrated WCAG relative luminance targets while preserving the input hue. |
| `palette.primary` | map (`50`–`950` → `#RRGGBB`) | Optional step-by-step overrides merged onto the generated (or stock) primary scale. |
| `palette.neutral` | map (`50`–`950` → `#RRGGBB`) | Optional neutral scale overrides. |
| `palette.success` | map (`50`–`950` → `#RRGGBB`) | Optional success scale overrides. |
| `palette.warning` | map (`50`–`950` → `#RRGGBB`) | Optional warning scale overrides. |
| `palette.danger` | map (`50`–`950` → `#RRGGBB`) | Optional danger scale overrides. |
| `palette.light.*` | object of `#RRGGBB` | Optional light-mode semantic token overrides: `bg`, `bg_subtle`, `surface`, `surface_raised`, `text`, `text_secondary`, `text_muted`, `border`, `primary`, `primary_hover`, `primary_active`, `primary_text`. |
| `palette.dark.*` | object of `#RRGGBB` | Optional dark-mode semantic token overrides (same 12 keys as `palette.light.*`). |

#### How Primary Colors and Contrast Are Resolved

- **Preserving your exact accent hex**: If `palette.light.primary` is omitted and `accent_color` achieves at least `4.5:1` contrast against both `light.bg` and `light.surface`, Scion uses `accent_color` directly; otherwise it uses generated `primary-600`. Symmetrically in dark mode, `accent_color` is used directly if it achieves `4.5:1` against both `dark.bg` and `dark.surface`, else generated `primary-400`.
- **Automatic button foreground (`primary_text`)**: When `palette.light.primary_text` or `palette.dark.primary_text` is omitted, Scion automatically picks whichever of `#ffffff` or `neutral-950` (`#0f172a` default) achieves higher contrast across `primary`, `primary_hover`, and `primary_active`.
- **Strict WCAG 2.1 AA gate (`HTTP 422`)**: Every save validates both light and dark themes:
  - **4.5:1 minimum** for `text`, `text_secondary`, and `text_muted` against `bg`, `bg_subtle`, `surface`, and `surface_raised`.
  - **4.5:1 minimum** for `primary` against `bg` and `surface`, and for `primary_text` against `primary`, `primary_hover`, and `primary_active`.
  - **4.5:1 minimum** for filled Shoelace button/badge text and status badges in light and dark modes.
  - **3.0:1 minimum** for non-text UI border contrast (`neutral-600` vs `light.surface`, `neutral-400` vs `dark.surface`).
  - Any failing pair rejects the save with `422` naming the mode, token pair, measured ratio, and required minimum.

## Managing Branding Settings

### Permissions & Credential Requirements

Writing or resetting the `branding` section requires Hub administrator privileges (`hub.config.update`) authenticated with an **interactive sign-in session** (`scion hub auth login`) or a workstation dev credential. **User Access Tokens (UATs) are refused** for `branding` because it controls unauthenticated `/login` content and external image URLs.

### Using the Web Admin UI (`/admin/branding`)

Navigate to **Admin > Branding** (`/admin/branding`) to configure branding interactively with instant pre-save feedback. Viewing the page requires `hub.config.read`; editing and resetting require `hub.config.update` (read-only administrators see a read-only banner with save and reset controls disabled).

- **Scoped Live Preview**: The top of the page renders side-by-side **Light Mode** and **Dark Mode** previews of your unsaved draft (product name, subtitle, logo, corners, typeface, primary controls, and attribution footer) along with an advisory WCAG 2.1 AA contrast summary. Live Hub styling updates only when you click **Save Branding**.
- **Identity & Attribution**: Edit `name`, `short_name`, `subtitle`, and `attribution` (`shown` or `hidden`).
- **Shape & Typography**: Choose a `corners` preset (`default`, `square`, or `rounded`) and a `font_sans` preset (`default`, `system` for air-gapped deployments, curated Google Fonts presets, or a custom Google Fonts family name).
- **Logos & Icons**: For each logo and icon slot, enter an `https://` URL or same-origin `/path`, or upload a local file (converted in the browser to a base64 `data:` URI). Uploaded assets render as compact metadata chips with **Replace file**, **Switch to URL**, and **Clear** actions, inline light/dark swatches, and a budget meter tracking total embedded `data:` bytes against the 128 KiB per-asset and 512 KiB total limits.
- **Colors & Advanced Palette Overrides**:
  - Pick an `accent_color` to preview the generated 11-step (`50`–`950`) OKLCH primary ramp and live Light/Dark WCAG 2.1 AA contrast badges.
  - Expand **Advanced Palette Overrides** to edit individual `palette.light.*` and `palette.dark.*` semantic tokens (grouped by Surfaces, Text, and Borders & Primary Controls) and the five 50–950 shade scales (`primary`, `neutral`, `success`, `warning`, `danger`). Each token displays its live WCAG 2.1 AA contrast badge and a per-key reset control; each shade scale also provides a **Clear overrides** button. If the Hub returns a `422` validation error on a palette key, the relevant palette panel expands automatically with inline error messages.
- **Dirty-Diff Saves, Resets, & Environment Overrides**:
  - **Save Branding** sends only changed fields (deep-merged on the server, with modified shade-scale maps sent whole) with optimistic-locking revision checks (`409` prompts a reload) and applies changes live across the Hub without restarting.
  - Every customized field has a per-key reset button. **Reset All Branding** opens a confirmation dialog and deletes the managed database row (or clears all editable keys in file-backed mode).
  - Keys locked by environment variables render read-only with their active value and are omitted from save payloads.

### Using the CLI (`scion hub settings`)

Inspect the current branding section or its schema:

```bash
# View all keys in the branding section
scion hub settings get --section branding

# View a single key
scion hub settings get server.hub.branding.name

# Print the JSON Schema for the branding section
scion hub settings schema branding
```

Set one or more keys (changes are deep-merged key-by-key into the stored section, with compare-and-set revision checks). Because `scion hub settings set` parses argument values as YAML—where an unquoted `#` begins a comment—pass `#RRGGBB` hex colors with YAML quoting (such as `'"#0d9488"'`) or with `--string`:

```bash
scion hub settings set --string \
  server.hub.branding.name "Acme Cloud" \
  server.hub.branding.short_name "Acme" \
  server.hub.branding.subtitle "Engineering Platform" \
  server.hub.branding.accent_color "#0d9488" \
  server.hub.branding.corners "square" \
  server.hub.branding.font_sans "ibm-plex-sans"
```

Apply a YAML or JSON file (useful for full palettes or base64 `data:` logos):

```bash
scion hub settings set --file branding.yaml
```

Clear individual keys or reset the entire `branding` section back to bootstrap/stock defaults:

```bash
# Clear a single key
scion hub settings unset server.hub.branding.subtitle

# Reset the entire branding section to bootstrap defaults
scion hub settings unset --section branding --reset --yes
```

### Admin Server Config API

Branding uses the standard Hub operational settings endpoints (see [Admin Settings Model](/scion/reference/admin-settings/)):

- **Read**: `GET /api/v1/admin/server-config` returns the active config under `server.hub.branding` and section metadata under `section_metadata.branding`.
- **Update**: `PUT /api/v1/admin/server-config` accepts branding nested under `server.hub.branding` (with optional `expected_revisions` for optimistic locking):

```json
{
  "expected_revisions": { "branding": 1 },
  "server": {
    "hub": {
      "branding": {
        "name": "Acme Cloud",
        "accent_color": "#0d9488",
        "corners": "rounded",
        "font_sans": "Plus Jakarta Sans"
      }
    }
  }
}
```

Nested objects (`logo`, `icons`, `palette.light`, `palette.dark`) are deep-merged key-by-key onto the stored section; sending `null` for a key clears it. The five shade-scale maps (`palette.primary`, `palette.neutral`, `palette.success`, `palette.warning`, `palette.danger`) are replaced whole when sent, so include every step you want to keep in a given scale.

- **Reset section**: `DELETE /api/v1/admin/server-config/sections/branding` deletes the saved database row so the Hub falls back to bootstrap configuration (`settings.yaml` / `SCION_SEED_*`) or stock defaults.
- **Schema**: `GET /api/v1/admin/server-config/schema` returns the derived JSON Schema and `koanf_paths` for `branding`.
- **Public read**: `GET /api/v1/settings/public` exposes the resolved `branding` object (omitted when stock defaults apply). This endpoint requires no authentication and is exempted from maintenance-mode `503` responses so `/login` and CLI auth can load branding during maintenance.

### Bootstrap Configuration (`settings.yaml` & Environment Variables)

You can seed branding at startup in `settings.yaml` or via `SCION_SEED_*` environment variables (for example `SCION_SEED_SERVER_HUB_BRANDING_NAME`, `SCION_SEED_SERVER_HUB_BRANDING_ACCENTCOLOR`, `SCION_SEED_SERVER_HUB_BRANDING_CORNERS`, `SCION_SEED_SERVER_HUB_BRANDING_FONTSANS`).

The five shade-scale map keys (`palette.primary`, `palette.neutral`, `palette.success`, `palette.warning`, `palette.danger`) are structured maps and must be configured via the Web Admin UI (`/admin/branding`), `settings.yaml`, `scion hub settings`, or the Admin API rather than environment variables.

As with all Layer-1 settings, once an admin saves `branding` through the CLI or API, the database row takes precedence over `settings.yaml` and `SCION_SEED_*` until the section is reset.

## Live-Apply vs. Restart Behavior

- **No restart required**: Every key in `server.hub.branding.*` is a live-applied Layer-1 setting. Saves take effect immediately on the serving Hub node and propagate to other HA replicas within about 2 seconds (with a 60-second poll backstop).
- **Browser refresh**: New page loads immediately receive the updated `<head>` stylesheet (`<style id="scion-branding-tokens">`), `<title>`, icons, and font link with zero flash of unstyled content. Open browser tabs update on reload or when public settings refresh.
- **Malformed startup config**: If a hand-edited `settings.yaml` contains invalid branding values (such as a failing contrast ratio or disallowed SVG), the Hub logs a warning and falls back to stock Scion defaults instead of failing to start.

## Examples

### Full White-Label (`settings.yaml` or `scion hub settings set --file`)

```yaml
server:
  hub:
    branding:
      name: "Acme Cloud"
      short_name: "Acme"
      subtitle: "Internal Agent Platform"
      attribution: "hidden"
      corners: "rounded"
      font_sans: "plus-jakarta-sans"
      accent_color: "#0d9488"
      logo:
        light: "https://cdn.acme.example/logo-light.svg"
        dark: "https://cdn.acme.example/logo-dark.svg"
      icons:
        favicon: "https://cdn.acme.example/favicon.svg"
        apple_touch_icon: "https://cdn.acme.example/apple-touch-icon.png"
        icon_192: "https://cdn.acme.example/icon-192.png"
        icon_512: "https://cdn.acme.example/icon-512.png"
        icon_maskable_512: "https://cdn.acme.example/icon-maskable-512.png"
```

### Air-Gapped / Offline Hub

Use `font_sans: "system"` to drop the Google Fonts Inter stylesheet and embed logos as base64 `data:` URIs (served same-origin from `/brand/{slot}`). Note that the HTML shell still requests the JetBrains Mono stylesheet from Google Fonts (and includes preconnect hints); when unreachable, it falls back gracefully to local monospace fonts:

```yaml
server:
  hub:
    branding:
      name: "Corp Agents"
      font_sans: "system"
      corners: "square"
      accent_color: "#2563eb"
      logo:
        light: "data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHZpZXdCb3g9IjAgMCAyNCAyNCI+PGNpcmNsZSBjeD0iMTIiIGN5PSIxMiIgcj0iMTAiIGZpbGw9IiMyNTYzZWIiLz48L3N2Zz4="
```

## Related Documentation

- [Admin Settings Model](/scion/reference/admin-settings/) — Layer-1 precedence, seeded vs. managed lifecycle, and section resets.
- [Server Configuration Reference](/scion/reference/server-config/) — Complete `settings.yaml` structure and environment variable conventions.
- [Authentication & Identity](/scion/hosted/single-node/auth/#web-oauth-setup) and [Security Architecture](/scion/reference/security/#12-user-authentication-oauth-20) — Unauthenticated web routes (`/brand/*`, `/manifest.webmanifest`) and CSP guarantees.
