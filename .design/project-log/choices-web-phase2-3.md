# Choices web cards, phases 2 and 3: set card, replaced, interpreted (ptone/scion#7673)

Base: `scion/chat-choices` at `100708a` (phase 1). Design: `design-7673.md`
r4 (approved by ptone, msg `6f8bd390-e2c5-4578-9e30-314d5a846bf7`), §8
items 2 and 3, §4.2, §4.3, §4.7, AC 3–5. Branch `scion/chat-choices-p23`,
integrated by choices-em.

## What was built

- **Set call.** `client/chat-choices.ts` `answerDecisionSet(messageId,
  answers, key)` → `POST /api/v1/choice-prompts/{id}/answers` with
  `{idempotency_key, answers: [{decision_row_id, choice_id}]}`. It reads the
  camelCase 200 (`responseMessageId`, `batchId`, `decisions`) and the 409
  `answer_set_changed` `details.decisions` + `details.reasons` (by row id).
- **Set card** (`answerMode=set`, or 2+ decisions). Header "N decisions"
  (`list-check`) + key badges. Each row is a `role=group` with its key badge
  and question. A row's selection is the viewer's click, else its
  `defaultChoiceId` (local only). Selected = filled primary + `check-lg`,
  `aria-pressed=true`. Recommended and not selected = primary outline.
  Footer: "N remaining" (count only, warning colour while >0) plus excluded
  rows ("· 1 replaced", "· 1 answered by Bob"). Submit answers stays
  disabled while N>0, inside an `sl-tooltip` and with an `aria-describedby`
  sr-only hint, both reading "Choose an answer for 17 first". No card-level
  Enter.
- **After submit.** The 200's views are applied at once and emitted as
  `choice-answered` (all rows). Rows read "Answered by …". When every decided
  row shares one `batchId`, the footer reads "Submitted by X · time", so the
  same footer appears after a reload and in a second tab.
- **409 `answer_set_changed`.** The card holds `details.decisions`, keeps
  picks only on rows still pending and shows an inline warning `sl-alert`
  naming the changed keys until the next submit. Reason → phrase map:
  replaced → "was replaced"; decided / not_recorded_answerer → "was answered
  by <name>"; missing → "still needs an answer"; other → "is no longer
  needed". The tail reads "Your other selections are kept — review and
  submit again." or, with nothing left, "Your selections were not sent."
- **Answered elsewhere.** A `decisions` update that turns a row pending →
  decided by another user (not during the card's own submit) is recorded. The
  row locks and the set shrinks. Once no row is actionable, the card adds the
  note "Bob answered while you were choosing; your selections were not sent."
  An answer by the viewer in another tab adds no note.
- **Replaced** (single and set rows). Choices disabled and question/choices
  faded. Footer: `arrow-repeat` "Replaced by vN · time · Jump to vN", where
  the link is `<a href="#msg-<id>">` and emits `choice-jump {messageId}`. The
  thread binds `@choice-jump` to its existing `handleScrollToMessage`, so
  `scrollToMessageById` runs (or the starred filter exits to the target). The
  single header shows `vN` when version > 1 or replaced. The remaining rows
  of an old set submit as a smaller set.
- **Interpreted** (`provenance=agent_interpreted`). Read-only: chosen marked,
  `robot` footer "Interpreted by <agent> from <name>'s|your reply · time". The
  agent is the asker unless `interpretedByAgentId` names another. No
  confirm/override.
- `scion-chat-message` now renders the card for any choice-prompt with
  decisions (the phase 1 "single only" restriction is gone) and passes
  `answerMode`. `list-check` added to `USED_ICONS`.

## Verification

- `npm run typecheck`, `npm run lint` (0 errors), `npm test`, `npm run build`.
- New Vitest: `chat-choices-set.test.ts` (header/badges, defaults,
  aria-pressed, counter + warning, tooltip/aria-describedby keys, select-only
  clicks, outline return, group labels, no card Enter, stacked rows, POST
  body, in-flight, answered + Submitted footer, error toast, 409 keep/drop +
  alert text, alert until next submit + smaller set, reason map, nothing-left
  tail, concurrent lock, not-sent note, own-tab no note, replaced row + jump +
  exclusion, smaller set, all replaced). `chat-choices-terminal.test.ts`
  (replaced single: version, disabled/faded, no Enter hint, footer, jump
  event; interpreted single/set). `chat-thread-choices.test.ts` (set renders
  through the message, SSE locks a set row, `choice-jump` → scroll).
  `client/chat-choices.test.ts` (`answerDecisionSet`).
- Visual: standalone Vite harness + headless Chromium, compared with
  `mocks/png` 04/06/08 (light). Screenshots in
  `/scion-volumes/scratchpad/projects/chat-choices/evidence/phase2-3/`.

## Deviations and left open

- **Interpreted rows in a set are excluded.** The hub's actionable-set rule
  counts a row decided only by an interpreted answer recording the viewer as
  actionable (a confirm). Design §2 makes interpreted answers read-only, so
  the card leaves such a row out, and a submit would come back 409 `missing`
  for it. No live route produces interpreted answers yet. Phase 2 of the
  hub design (confirm/override) must handle this.
- `missing` / `not_actionable` phrases are additions. The design names only
  replaced / answered by someone else / no longer needed.
- Header key badges show in every set state. The mock shows them only in
  scene 4, while design §4.3 says header + key badges without qualification.
- Clicking the selected choice does not deselect it.
- The disabled/answerability input (phase 4) and response lines (phase 5)
  are not touched. Docs reconciliation (§4.9) is phase 6.

## Process notes

- The dark harness render lacks the app's dark Shoelace token overrides,
  which live inline in `web/index.html`, so default buttons look washed out.
  Phase 1's single card renders the same way there. Phase 6's Playwright
  against the real app is the dark check.
- Unregistered `sl-button` in Vitest has no `disabled` property: assert the
  attribute.
