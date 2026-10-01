# Native Chat Quick Command Palette — Phase 4 (integration)

**Date:** 2026-09-30
**Design:** `/scion-volumes/scratchpad/projects/native-chat/palette/design.md`
**Branch:** `scion/nc-palette-integration`

## Problem

Bring the palette's four quadrants together into one shippable feature: wire the Documents group to
phase 3's `chatRecentFiles` store, mount a single page-level file preview outside the conditional
conversation rendering, complete the keyboard/empty/error/responsive model across all four groups,
pass a real axe accessibility pass in Chromium, remove the feature flag and the legacy flat switcher
entirely so the palette is the only presentation, prove the whole thing against a real ephemeral hub,
and fix a reopen race in `togglePalette`/`_handlePaletteAfterHide` that could leave an invisible-but-open
palette while keystrokes and focus went to the composer underneath it.

## Approach

- **Documents group** — `chat-palette-data.ts` gained `buildDocumentCandidates`/
  `documentSecondaryLabel`, kept minimal and appended at the end of the file per nc-palette-em's
  request to limit conflicts with phase 2's own in-flight work on that file. `chat.ts` subscribes to
  `chatRecentFiles.subscribe()` (gated on `isV2`) and feeds `v2PaletteGroups.documents` from its
  snapshot, sharing the same global ranking as the other three groups.
- **Page-level preview mount** — a single `<scion-chat-file-preview>` is mounted as a sibling before
  `.v2-panels`, outside the conditional conversation rendering, so it works from an empty `/chat` with
  no thread mounted. A document selection closes the palette first (`_pendingDocumentPreviewTarget`),
  and the preview opens only after `_handlePaletteAfterHide` fires — never two modals fighting over
  focus at once. Closing the preview restores the original invoker focus; a stale candidate (removed
  by a refresh between selection and hide) is a no-op.
- **AC 4.7 reopen race** — `togglePalette` now distinguishes three in-flight states: a genuinely open
  palette (close it), a still-pending initial open (cancel it), and a close whose own hide animation is
  still running (queue the reopen via `_palettePendingReopen` rather than racing it). The queued reopen
  is served by `_handlePaletteAfterHide` once the stale close's `sl-after-hide` actually arrives,
  guaranteeing the palette ends up visibly open and focused (or cleanly closed), never invisible-open
  with keys leaking to the composer. Proved with a prefix-control run (revert the fix alone, all 4
  delay-parametrized tests fail, dialog reported hidden where visible was expected) and a
  `--repeat-each=15` stress run over all 12 `reopen-race.pw.ts` tests under 6 synthetic CPU burners
  (180/180 passed) — burner count calibrated to nc-palette-em's guidance after the container's 2-CPU
  cgroup quota made 16 burners produce false-signal Chromium timeouts.
- **Accessibility (AC 4.4)** — running axe for real in Chromium (not assuming it would pass) found a
  genuine violation: a `role="listbox"` cannot contain an interactive `role="button"` descendant
  anywhere in its subtree, even nested through unroled `div`s, and the palette's per-group
  Retry/Show-more buttons lived inside the same listbox as the candidate rows. Fixed by restructuring
  `chat-switcher.ts` so `#palette-result-list` is an empty `role="listbox"
  aria-owns="..."` pointing at each group's own `role="group"` region (candidate rows only), with the
  interactive controls rendered as DOM siblings outside it.
- **Rollout (AC 4.5)** — `NATIVE_CHAT_PALETTE_FLAG` and the entire legacy flat-switcher call path
  (`toggleSwitcher`, `loadSwitcherConversations`, `handleSwitcherSelect`, `handleSwitcherClose`,
  `removeSwitcherConversation`, and `chat-switcher.ts`'s flat-mode `render()` branch/CSS) removed
  outright, not just disabled. The `native_chat_v2` boundary is untouched and still enforced.
- **Preview/palette focus handoff** — a Cmd/Ctrl+K pressed while the document preview is open or closing
  is queued and served when the preview's close arrives, subject to the same open guards; a deferred
  preview likewise re-checks those guards before it opens.
- **Real-API smoke (AC 4.6)** — an ephemeral dev-auth hub seeded with dedicated test data only (2
  agents, 2 users, 2 hub-managed projects, all `p4-`-prefixed), driven with real Chromium via
  `playwright-core`: existing/new agent and person DMs, a cross-project thread switch with no page
  recreation, an acknowledged attachment and a detected workspace path both matching real content in
  the preview, a preview opened from an empty `/chat`, persistence across a full reload, and clearing
  on logout. Headless Chromium has no browser chrome, so a real browser-omnibox Cmd/Ctrl+K collision
  cannot be proven from this environment — disclosed as an unavailable platform check rather than
  assumed or mocked.

## A real bug found by mutation testing and by taking AC 4.4 seriously

See the accessibility restructuring above (found by actually running axe, not mutation testing). Two
mutation-testing survivors in `chat.ts`'s document-select branch were closed with dedicated tests: the
`kind === 'document'`-and-`key`-match stale-candidate guard was originally only exercised by a fixture
where the two halves of the compound condition always agreed, masking each half; a second fixture with a
different-kind, same-key candidate now discriminates them independently.

## Scope boundaries kept

No change to `chat-recent-files.ts` or its capture hooks (phase 3's code, consumed as-is).
`chat-palette-data.ts` changes kept to two new functions appended at the end of the file, per the
explicit request to limit conflict surface with phase 2's own concurrent work on that file.
`chat-file-preview.ts` is changed: its client-side text/binary classification uses a known-binary
deny-list for paths, and MIME type (plus a recognized text name for `application/octet-stream`) for
attachments, and it resolves the URL-safety check before classifying, so an unsafe link shows the
generic error state rather than a binary placeholder.

## Rebase onto origin/main

Required by nc-palette-em after phase 2 merged upstream with content differing from the original base.
`git rebase --onto origin/main fdb8d73a723fc97d6f2a8e70a0446c3cf341b423` completed with zero manual
conflicts; the complete diff the rebase picked up for the two files nc-palette-em flagged
(`chat-palette-data.ts`, `chat.ts`) is reproduced in `evidence/phase4.md` and confirmed to be exactly
phase 2's own changes, with phase 4's code untouched. All gates re-run green on the rebased head. Full
details in `reviews/phase4-handoffs.md`.

## Gate results

Results on the branch's final head, except where a row says otherwise:

| Gate | Result |
| --- | --- |
| `npx tsc --noEmit` (project, client, and both e2e tsconfigs) | Clean |
| `npx vitest run` (scoped sweep, 31 files) | 1479/1479 passed |
| `npm run build` | Clean |
| Chromium: `e2e/chat-palette/` (full suite), run twice | 83/83 passed both times |
| Chromium: `e2e/chat-file-preview/` (full suite), run twice | 11/11 passed both times |
| Chromium: `reopen-race.pw.ts` + `document-preview.pw.ts`, `--repeat-each=15`, 6 CPU burners | 420/420 passed |
| Real-API smoke (ephemeral dev-auth hub, dedicated test data) | All checkpoints passed (last run by the developer in round 1; re-run by the phase-4 reviewers on later heads) |

Full evidence, exact commands, and output excerpts:
`/scion-volumes/scratchpad/projects/native-chat/palette/evidence/phase4.md`. Mutation table:
`/scion-volumes/scratchpad/projects/native-chat/palette/reviews/phase4-r0-notes.md`. Latest review
response: `/scion-volumes/scratchpad/projects/native-chat/palette/reviews/phase4-r5-response.md`.
