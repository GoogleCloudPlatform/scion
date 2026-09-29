# Native Chat Quick Command Palette — Phase 1 (Agents/DM slice)

**Date:** 2026-09-29
**Design:** `/scion-volumes/scratchpad/projects/native-chat/palette/design.md`
**Branch:** `scion/nc-palette-dm-slice`

## Problem

Replace the flat, hand-rolled `scion-chat-switcher` (`Cmd/Ctrl+K`) with a grouped quick command
palette. Phase 1 is one vertical slice — not a platform-only PR — proving the whole path end to
end for exactly one group (Agents/DMs) before phases 2-4 fan out to People/Threads/Documents:
real paginated `GET /api/v1/agents` + `GET /api/v1/chat/dms` → normalized candidates → fuzzy
match → keyboard-selected option in a grouped `sl-dialog` → the existing `openDM` with a typed
peer kind and deterministic DM key.

## Approach

- **`client/chat-palette-types.ts`** — discriminated `PaletteTarget`/`PaletteCandidate`/
  `GroupState` types, forward-compatible with phases 2-4's additional groups but only populating
  `'agents'` here.
- **`utils/chat-palette-match.ts`** — pure NFC-normalized match classification (exact/prefix/
  substring/subsequence), a single deterministic comparator (tier, recency, group order,
  normalized label, stable ID), and original-string-coordinate highlight ranges. No fuzzy-match
  dependency.
- **`client/chat-palette-data.ts`** — fully paginated agent fetch (cursor-following until
  `nextCursor` is empty, not until a page is empty; a repeated cursor is a load error, not an
  infinite loop), joined with `/api/v1/chat/dms` for recency, with `_messageability.canMessage`
  authoritative over the `_capabilities` fallback and fail-closed when both are absent.
- **`components/shared/chat/chat-switcher.ts`** — added an opt-in `paletteMode` property
  (default `false`) rendering a labeled `sl-dialog` with a native query input and a single
  Agents group. The pre-existing flat-overlay code path, its properties, and its render output
  are completely untouched — `chat-switcher.test.ts` was not modified and still passes unmodified,
  satisfying the phase's "existing switcher regression suite still passes with rollout flag off"
  requirement by construction.
- **`components/pages/chat.ts`** — one shortcut owner. `_handleGlobalKeydown` runs every guard
  (exactly one of Ctrl/Meta, no Alt/Shift, `!repeat`, `!isComposing`, `!defaultPrevented`, a
  terminal/xterm `composedPath()` exclusion, a `/chat`-or-below route guard, a page-visibility
  walk that follows ancestor `hidden` attributes for the terminal-workspace-hides-chat-page case,
  and a Shoelace-modal guard tracked via `sl-show`/`sl-after-hide`) before dispatching to exactly
  one of the legacy `toggleSwitcher()` or the new `togglePalette()`, gated by the temporary
  `web.native_chat_palette` flag (default off, meaningful only with `web.native_chat_v2`).
- **`e2e/chat-palette/`** — a Chromium fixture (mirrors `e2e/terminal-pane`'s convention) mounting
  the real `scion-page-chat`/`scion-chat-switcher`, and for the terminal scenario a real
  `scion-terminal-pane`/xterm, with endpoint-shaped request interception standing in for the Hub.

## Real bugs found while wiring against a real `sl-dialog`

happy-dom cannot observe either of these; both only surfaced once the palette was driven inside a
real Chromium `sl-dialog`.

1. **`v2SwitcherLoaded` was a plain field, not `@state`.** `togglePalette()`'s "load module, then
   open" sequence collapsed into a single Lit render pass, so `<scion-chat-switcher>`'s internal
   `<sl-dialog>` was created already `open=true`. Shoelace's `@watch('open')` only reacts to a
   *transition*, so the dialog never fired `sl-show`/`sl-initial-focus` and the query input never
   received focus on first open. Fixed by making the field reactive and awaiting
   `this.updateComplete` between the mount (open=false) and the `open=true` write.
2. **Focus-after-selection checked for the new composer's `sl-textarea` exactly once** (a single
   `requestAnimationFrame`) before falling back to a generic element. `scion-chat-thread`/
   `scion-chat-composer` mount in a separate async update this page's own `updateComplete` does
   not wait for, so under load the check could lose the race (observed directly: failed 2 of 4
   full Chromium suite runs before the fix, 0 of 4 after). Fixed with a bounded 2s poll.

## Scope boundaries kept

No People, Threads, or Documents groups (real or stubbed) were added — only `'agents'` is ever
populated this phase. No mock production layer: the real `/api/v1/agents` and `/api/v1/chat/dms`
adapters are what the palette actually calls; only the Chromium test *fixtures* use endpoint
interception, per design.md's explicit allowance for browser test infrastructure.

## Gate results

| Gate | Result |
| --- | --- |
| `npx tsc --noEmit` (project + `e2e/chat-palette/tsconfig.json`) | Clean |
| `npx vitest run` (chat components, palette types/matcher/data, feature-flags, chat.ts shortcut) | 457/457 passed |
| `chat-switcher.test.ts` (unmodified, legacy overlay) | 10/10 passed, unchanged |
| Chromium: `focus-and-guards.pw.ts` (AC 1.3) | 7/7 passed |
| Chromium: `agent-selection.pw.ts` (AC 1.4) | 5/5 passed |
| Chromium: `terminal-and-modal.pw.ts` (AC 1.5) | 6/6 passed |
| Real-API smoke (local ephemeral hub, `--dev-auth`) | `GET /api/v1/agents`, `GET /api/v1/chat/dms` both responded correctly against the real handler chain; empty list only (no runtime broker available in this sandbox to provision a real agent — same constraint the repo's own E2E harness has) |

Full evidence, exact commands, and output excerpts:
`/scion-volumes/scratchpad/projects/native-chat/palette/evidence/phase1.md`.

## Addendum: review round 2 (2026-09-29)

A second independent review (`nc-palette-p1-rev-2`) found two more real bugs and several
coverage/rigor gaps, all fixed on the same branch (see
`/scion-volumes/scratchpad/projects/native-chat/palette/reviews/phase1-r2-response.md` for the full
disposition table):

- **Real bug (AC 1.2):** the grapheme-aware highlight mapping from round 1 mapped `starts`/`ends`
  one entry per Unicode *code point*, but the normalized string it indexes into is UTF-16
  *code-unit* indexed — any astral character (emoji) desynced every later highlight, duplicating
  the rendered label. Fixed by mapping one entry per code unit instead.
- **Real bug (AC 1.5):** the palette closed itself (and dropped focus to `<body>`) on *any*
  Shoelace `sl-show` event, not just a modal one — a toast notification while a user was in the
  palette silently killed it. Fixed by sharing the same modal-tag predicate the born-open-dialog
  guard already used.
- **Test rigor:** one Chromium test used `document.body.focus()` (a no-op) to try to move focus out
  of a terminal, so it never actually did — the route/visibility guards it claimed to cover were
  unproven. Fixed by walking down through nested shadow roots to blur the real focused element.
- **Coverage gaps:** round 1's born-open-dialog, dialog-removal, and hover-doesn't-move-selection
  fixes were correct in production code but only had happy-dom (not real Chromium/shadow-DOM)
  regression tests, despite the round-1 response claiming otherwise. Added the missing Chromium
  tests and corrected the over-claim.
- **Performance:** ranking (filter + classify + highlight computation + an O(N log N) sort) ran up
  to three times per render plus once per arrow-key press. Memoized on `(queryText, groups)`
  reference equality; also hoisted a per-call `Intl.Segmenter` construction to a lazy singleton.
- **Nits (all required by the EM this round, not optional):** two stale comments, two
  insufficiently-filtered dialog lifecycle handlers, a missing code-point-vs-code-unit tiebreak test
  row, an unguarded JSON parse on the DM-list fetch, and a double-Ctrl+K race during the palette's
  first-open lazy import (fixed with a synchronous pending-open flag, per explicit EM instruction
  not to decline this one).

Every new/changed test was mutation-verified (temporarily broke the fix, confirmed the test failed,
restored, confirmed it passed, confirmed `git status` clean) before being reported fixed. Branch was
rebased onto a newer `origin/main` (`d2b405def`) and force-pushed after confirming no other agent had
pushed to it in the interim.
