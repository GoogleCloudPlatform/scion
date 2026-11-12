# Choices web cards, phase 6: screenshots suite, sink test, docs (ptone/scion#7673)

Base: `scion/chat-choices` at `59cd62d` (phases 1-5 integrated). Design:
`design-7673.md` r4 (approved by ptone, msg
`6f8bd390-e2c5-4578-9e30-314d5a846bf7`), §4.9, §6 (Tests), AC 8 and 9.
Branch `scion/chat-choices-p6`, which choices-em integrates. Everything
stays behind `web.chat_choices`.

## What was built

- **Mocked-API Playwright suite.** `web/e2e/chat-mobile/choices.pw.ts`
  with fixtures in `choices-mock.ts`. It extends the chat-mobile pattern:
  `openChatRail` with an override that pins `web.chat_choices` in
  `__SCION_FEATURES__`, sets `scion-theme` in localStorage, and routes the
  #general history (with the five choice side maps and `canPost`), the
  members, and the asker lookup `GET /api/v1/agents/{id}` (ok, denied with
  `mode_lineage_no_ancestry`, or 404).
  - The theme is applied by the real `index.html` script, so the dark
    tokens are real. Earlier phases' ad-hoc harness lacked them.
  - It runs on `desktop-1440` and `chromium-390` (the other phone widths
    are skipped), each in light and dark: 14 tests × 2 themes × 2 projects
    = 56. Timezone UTC, 2x device scale.
  - States: single pending, descriptions, answered (Enter on the focused
    card, request body checked); set pending, stacked row (mock s13), ready
    then submitted (request body checked), replaced row, 409
    `answer_set_changed`; replaced card with jump (the target gets
    `permalink-highlight`); interpreted read-only; blocked by the asker;
    read-only thread; asker gone; response lines (single own/other, set
    collapsed and expanded, not delivered, jump).
  - Each test asserts text and ARIA (group labels, `aria-pressed`,
    `aria-disabled`, `aria-describedby` targets, the Submit tooltip and its
    hidden hint), then saves `choices-<project>-<theme>-<state>.png` with
    `testInfo.outputPath` (gitignored `web/test-results/`) and attaches it.
    The repo commits no screenshot baselines, and this suite adds none.
- **Sink test.** `chat-markup-sinks.test.ts` gained a choice-card case. The
  question is full markdown and the choice descriptions are inline
  markdown. Both render through the shared renderer and DOMPurify policy,
  with no plain-string sink. Script, iframe, `onerror` and `javascript:`
  are dropped, and a lone description paragraph is unwrapped.
- **Type-check gap.** `chat-choices-set.test.ts` and
  `chat-choices-terminal.test.ts` were added to
  `tsconfig.component-tests.json`. Both already passed. Client tests are
  covered by a glob.
- **Visual fixes found by the screenshots** (all in `chat-choices.ts`,
  scoped to the card):
  - Dark theme: an unselected enabled choice's label was barely visible.
    The app theme maps Shoelace `neutral-700` to `#334155` on a `#1e293b`
    button. The card now sets `--sl-color-neutral-700: var(--scion-text)`
    on its default buttons. The dark mock copied the same bug.
  - Dark theme: the 409 alert was a white panel. The app theme leaves
    `neutral-50`, the Shoelace panel colour, light. The alert now uses the
    card surface, border and text colours.
  - A multi-paragraph question was bold throughout. Blocks after the first
    are now regular weight and secondary colour, like the mock's s3 `.sub`
    line.
- **Docs (§4.9).** `hosted/user/messaging.md`:
  - New "The web cards" bullet: outlined recommended choice, N remaining,
    Submit tooltip naming the missing keys, "Submitted by", the 409 alert,
    a smaller set after a replaced or answered row, replaced with jump,
    interpreted read-only, disabled with a reason before clicking, the 403
    backstop, asker gone, compact response lines.
  - Removed "N of M selected".
  - The history side map list is now complete.
  - `chat.choice.updated` subjects: `project.<id>.chat.choice.updated` and
    `user.<uid>.chat.choice.updated`.

  `reference/experiments.md` has the same in brief. The camelCase set
  response (`responseMessageId`, `batchId`) was already documented by B0.

## Verification

- `npm run typecheck`, `lint` (0 errors), `test` (522 files, 12771 passed),
  and `build`. After the last CSS change, the choices and sink suites were
  re-run (102 passed) and lint was re-run.
- `choices.pw.ts`: 56 passed (desktop-1440 + chromium-390, light + dark).
- `make ci` (with gitignored `web/public/shoelace/` moved aside).
- Evidence, 64 PNGs plus a README mapping each to its mock:
  `/scion-volumes/scratchpad/projects/chat-choices/evidence/phase6/`.

## Deviations and left open

- Blocked set (mock s10): defaults stay pre-selected (filled) while the
  card is blocked, and the footer ("1 remaining", disabled Submit) is
  shown. The mock shows the recommended choice outlined, nothing selected,
  and no footer. Not changed. Raised with choices-em.
- Earlier accepted deviations still apply: 24-hour times, the blocked
  wording per design §4.5, and "not delivered" muted.
- Adjacent cleanup, not done: `web/src/styles/theme.css` dark mode maps
  `--sl-color-neutral-700` and `--sl-color-neutral-50` to their light
  values, so every default `sl-button` label and every `sl-alert` /
  panel background in the app has the problem the card now works around.
  A theme fix would let the two card overrides go. Its blast radius is
  every Shoelace component in dark mode.
- The docs-site build (needs Node 22 and d2) was not run. The edits add no
  links or diagrams.
