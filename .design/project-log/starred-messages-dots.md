# Starred messages: always-visible More actions dots

**Date:** 2026-10-29
**Issue:** ptone/scion#6556, part of ptone/scion#5797

## What changed

- The per-message More actions button (`button.msg-actions` in
  `web/src/components/shared/chat/chat-message.ts`) is always visible. Before,
  it appeared only on hover or keyboard focus, as an outlined box outside the
  bubble at its lower corner, and on devices without hover (`hover: none`) it
  was `display: none`.
- It is now bare dots (the existing `three-dots-vertical` icon) with no
  border or background. They sit in the upper-right corner inside the text
  bubble, on the viewer's own messages and on other people's. The touch
  target is 1.5rem (24 px); the glyph is smaller. The colour comes from
  `--scion-text-muted`, and hover (only where hover is available) uses
  `--scion-text`.
- The button renders inside `.bubble-content`. When the button is on,
  `.bubble-content.has-actions` reserves a 2rem right-hand column, so text
  (long words, code blocks, wrapped lines) never runs under the dots. An
  attachment-only message has no text bubble: `.bubble.actions-corner`
  reserves the same column beside the attachments (with `box-sizing:
  border-box`, so the bubble keeps its max-width), and the button sits there. A deleted message keeps the dots in its "This message was deleted"
  bubble, because the menu still opens for it (see ptone/scion#6566).
- The tab stop's usage hint (`#actions-help`, the button's
  `aria-describedby`) now renders outside the text bubble, so the bubble's
  text is the message alone.
- Unchanged:
  - the accessible label ("More actions, message from <sender>, <time>");
  - the roving tab stop and arrow keys (`chat-thread.ts`);
  - the focus ring, shown on `:focus-visible` only;
  - the menu opening (the popup above 768 px, the action sheet at or
    below), positioned from the button and clamped by
    `placeMenuInViewport`;
  - right-click, tap, the ContextMenu key and Shift+F10;
  - the `actionsEnabled` gating (`web.chat_starred_messages`).
- Removed: the `has-star-meta` bubble class. It existed only to push the old
  outside button past the gutter star, which no longer shares that space.

## Tests

- `chat-message.test.ts`, "More actions dots (ptone/scion#6556)":
  - the button sits inside `.bubble-content.has-actions`, for own and other
    people's messages;
  - the exact label and the menu attributes;
  - the usage hint stays outside the text bubble;
  - deleted rows;
  - the attachment-only corner;
  - no reserved corner while the button is off;
  - checks on the component's CSS text:
    - no opacity, visibility or `display: none` on the button in any rule,
      and no `@media (hover: none)` rule for it;
    - bare dots, a target of at least 24 px, a theme colour, and top/right
      placement;
    - the reserved padding is at least the button's inset plus its width;
    - `outline: none` on `:focus` and a ring on `:focus-visible`.
  - The layout and visibility tests fail on the previous code.
- `chat-thread-jump-a11y.test.ts`: the headerless starred-row test no longer
  checks `has-star-meta`. It checks that the gutter star stays outside the
  bubble and that the button is inside it.
- No Playwright locators changed:
  - `star-header.pw.ts` uses `button.msg-actions` by class.
  - The `More actions` role lookups in `agent-dm.pw.ts` find the header
    menu; stars are off in that spec.
  - The root e2e specs do not reference the button.
- Screenshots at 390 px and 1440 px, in light and dark, before and after,
  with mock data only, were approved by ptone.

## Learnings

- Chromium does not launch under `ulimit -v` (its V8 address-space
  reservation fails with SIGTRAP). For a capped window, set a soft limit for
  node and Vite and lift it in a small launcher passed as
  `CHROMIUM_EXECUTABLE`. Only the browser then runs without the limit.
- In happy-dom, media queries and computed styles are not reliable, so the
  visibility requirements are tested against the component's `styles`
  cssText, the same way as `chat-space-rail-unread-dms.test.ts`.
