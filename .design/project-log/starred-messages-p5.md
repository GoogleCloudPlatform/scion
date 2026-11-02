# Starred messages, phase 5: final compact navigation and accessibility

**Date:** 2026-10-27
**Issue:** ptone/scion#6384 (epic ptone/scion#5797)
**Experiment:** web.chat_starred_messages (unchanged; no API, data or route change)

## What shipped

- **Header list** (`chat-starred-list.ts`): on desktop an anchored nonmodal dialog named by its heading, kept inside
  the viewport (height capped to the room below the button, shifted sideways near an edge); on narrow screens the same
  list in a modal bottom sheet (native `<dialog>` in the top layer) with a heading, a 44px Close, safe-area padding
  and a scroll region whose touches stay inside. Rows are two compact lines (local saved date and author, then the
  excerpt) with the full sent time in accessible text. Loading, empty, retryable error, a scan continuation with no
  rows yet, a failed refresh that keeps the rows, and a failed Load more are distinct states. Escape returns focus to
  the star; focus moving out, a pointer down outside, or Back/Forward closes it.
- **Reconcile**: `VisibleReconciler` (`chat-stars.ts`) refetches a visible collection surface at most once a minute
  and once on returning to the page, never while hidden. The open header list and the starred-only center use it;
  the full conversation refetches star state on return only. A header refresh reloads every loaded row (pages of up
  to 100, stopping at a short page) and keeps the rows on screen; an invalidation during Load more drops that page.
- **Jump arrival**: reduced motion scrolls instantly and holds the highlight without the fade. A permalink, a header
  entry and Jump to message in conversation focus the landed message (`tabindex=-1` only until focus leaves it).
  A popup message menu opened while a jump is settling follows its message instead of closing; a user scroll still
  closes it (this was the earlier "menu lost after a permalink jump" follow-up).
- **Shared row menus** (`context-menu.ts`, used by messages, rail threads, rail groups and members): `renderMenu`
  renders a named `role=menu` with `menuitem` / `menuitemcheckbox` rows, focus moves into it, Up/Down/Home/End,
  Enter/Space, Escape back to the opener, Tab out in the normal direction. After an action, focus returns to the
  opener only if the action left nothing focused.
- **Message keyboard entry**: a More actions button per message (under the experiment) is the message list's single
  tab stop (roving tabindex); Up/Down/Home/End move it, Enter/Space, Shift+F10 and the ContextMenu key open the menu
  there. After a jump, the landed message holds the stop. Removing the focused message hands the stop and focus to the
  closest one (an empty starred-only center gives focus to Show all). The keyboard contract was settled by a design
  decision during the phase (a roving native button rather than focusable message rows).
- **Dense rows**: in renderings without a header the star sits in the gutter beside the bubble, so a starred row is
  no taller than its neighbours.
- **Starred-only center**: loading/error/empty are status or alert regions; its list is a labelled region.

## Things worth knowing

- **The menu's opener is captured at open time** (`MenuFocus.opened`). For the message menu it is the message's
  More actions button, also for a right-click, so Escape always lands on a keyboard-reachable control.
- **Tab out of a menu** focuses the opener during the keydown and lets the browser's own Tab step run from there; no
  later restore undoes it.
- **A right-click is a `pointerdown` on the scroller**, which cancels the jump settle watcher but not the smooth
  scroll: that scroll's events used to close the menu. Only a pointer down on the scroller itself (a scrollbar grab)
  counts as a user scroll now.
- **happy-dom's `ShadowRoot.activeElement` throws** while focus is on a detached node (a closed menu's row); test
  helpers that read it catch that.
- **The header list's short-page rule matches the rail's**: a reconcile never continues a hub scan on its own.
- **Browser cases on the phone sheet must wait for its slide-in** (`sheet-in`, 0.18 s) before measuring it: a touch drag
  computed from a box that is still moving lands below the list. Wait on the dialog's `getAnimations({ subtree: true })`.
  Locate the message menu by its accessible name: the composer's agent menu is also a `role="menu"`.

## Follow-ups

- The P4 rail's own reconcile poll (`chat-space-rail.ts`) predates `VisibleReconciler`; it could use it, which would
  remove one copy of the visibility logic.
