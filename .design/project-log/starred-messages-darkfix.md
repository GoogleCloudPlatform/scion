# Starred messages: dark-theme backgrounds in chat

**Date:** 2026-10-31
**Issue:** ptone/scion#6796, part of ptone/scion#5797

## Why

In the dark theme the starred-only view's footer banner kept a light
background, so its bold "Starred messages only" title (light text) nearly
disappeared. The banner read `var(--scion-surface-2, #f8fafc)`, and
`theme.css` never declares `--scion-surface-2`, so the light fallback always
applied. Seven other chat rules had the same defect: each read a token that
`theme.css` does not declare.

## What changed

All eight now read declared theme tokens. The light theme looks the same as
before, except for the artifact picker's row lines.

| Rule                                                            | Was                     | Now                 |
| --------------------------------------------------------------- | ----------------------- | ------------------- |
| `chat-thread.ts` `.star-filter-banner` background               | `--scion-surface-2`     | `--scion-bg`        |
| `chat-message.ts` `.reply-preview` background                   | `--scion-surface-50`    | `--scion-bg`        |
| `chat-message.ts` `.reply-preview:hover` background             | `--scion-surface-100`   | `--scion-bg-subtle` |
| `chat-message.ts` `.reply-preview.unavailable:hover` background | `--scion-surface-50`    | `--scion-bg`        |
| `chat-composer.ts` `.reply-bar` background                      | `--scion-surface-50`    | `--scion-bg`        |
| `chat-search.ts` `.result-item` row line                        | `--scion-border-subtle` | `--scion-bg-subtle` |
| `artifact-picker.ts` `td` row line                              | `--scion-border-subtle` | `--scion-border`    |
| `chat-members.ts` hover background                              | `--scion-surface-hover` | `--scion-bg-subtle` |

- `--scion-bg` is neutral-50 in the light theme (the old fallback colour) and
  neutral-900 in the dark one, so the banner, the reply preview and the reply
  bar sit a step darker than the dark thread surface (neutral-800).
- The artifact picker renders in a dialog whose dark panel is neutral-800, so
  a neutral-800 row line would vanish there. It uses `--scion-border`
  (neutral-200 light, neutral-700 dark). The search results sit on
  `--scion-bg`, where the `--scion-bg-subtle` line still shows.
- Text on the new dark backgrounds:
  - the reply preview's text uses `--scion-text-muted` in place of the fixed
    `--scion-neutral-500` (3.75:1 on neutral-900);
  - the reply bar's text uses `--scion-text-secondary` in place of
    `--scion-neutral-600`;
  - both sender names use `--scion-primary-hover` in place of the fixed
    `--scion-primary-600` (3.45:1 on neutral-900). That token is primary-600
    in the light theme and primary-300 in the dark one.
- The banner's Show all messages button sets its fill, border and label
  from theme tokens (`--scion-surface`, `--scion-border-hover`,
  `--scion-text`; hover label `--scion-primary-hover`). `theme.css` maps
  Shoelace's neutral scale to the same slate values in both themes, so
  whether a default button's label is readable in the dark theme depends on
  whether that mapping or Shoelace's dark theme comes last in the cascade.
  With the mapping last (the Vite dev server), the label is slate-700 on a
  neutral-800 fill. The tokens make the button independent of that order.
  In the light theme its label is one step darker (neutral-800, was
  neutral-700).
- The `.reply-preview.unavailable:hover` rule arrived from upstream while
  this change was in review, with the same undeclared `--scion-surface-50`.
  It now reads `--scion-bg`, the same colour as the preview at rest, as the
  upstream fallback intended.
- No new tokens were added to `theme.css`.

## Tests

`web/src/components/shared/chat/chat-theme-tokens.test.ts`:

- The guard fails when a file in any `components/**/chat/` directory, or a
  `components/pages/chat*.ts` page, reads a `var(--scion-*)` that `theme.css`
  does not declare. Names the chat tree declares itself (such as
  `--scion-chat-tight` from `chat-shell.ts`) and names `client/viewport.ts`
  sets at runtime (`--scion-app-height`, `--scion-kb-*`) count as defined.
  The test also checks how many files it scans, that a planted undefined
  token is flagged, and that a token named only in a comment is ignored.
- Contrast checks resolve the colours the banner title, its muted line and
  its Show all messages button,
  the reply preview (text, sender and hover) and the reply bar (text and
  sender) actually use, in the light and dark token maps, and need at least
  4.5:1 (4:1 for the hover state, whose light pair has always been 4.34:1).
- On the code before this fix the guard lists the seven references it was written against, and the
  contrast checks fail because `--scion-surface-2` and `--scion-surface-50`
  cannot be resolved.

## Other undefined tokens (not changed)

Outside chat, these `var(--scion-*)` names have no declaration anywhere in
`web/src`, so they always take their fallbacks. They are recorded for a
follow-up:

- `--scion-error-bg`, `--scion-error-border`, `--scion-error-text`,
  `--scion-success-bg`, `--scion-success-border`, `--scion-success-text`,
  `--scion-warning-bg`, `--scion-warning-border`, `--scion-warning-text`:
  admin-federation, admin-integrations, admin-server-config,
  settings-error-views and project-visitors-editor;
- `--scion-danger`, `--scion-success`, `--scion-warning`: agent-detail,
  broker-detail, admin-maintenance, broker-area-styles, header and
  notification-tray;
- `--scion-action-hover-{danger,primary,success,warning}-bg`: agents;
- `--scion-border-subtle`: admin-maintenance;
- `--scion-font`: invite and onboarding;
- `--scion-surface-alt`: profile-telegram;
- `--scion-badge-danger-border`: deletion-banner;
- `--scion-border-light`: maximum-permission-selector;
- `--scion-bg-hover`: resource-styles.
