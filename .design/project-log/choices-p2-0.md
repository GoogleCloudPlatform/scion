# Choices Phase 2.0: Answerer Identity and Outcomes on Choice-Response Views (ptone/scion#7775)

**Date:** 2026-11-13
**Issue:** ptone/scion#7775 (part of epic ptone/scion#7514; unblocks ptone/scion#7798)
**Branch:** scion/choices-p2-0

## Problem

Phase 1 choice-response views (`ChoiceResponseView` and `ChoiceResponseSetView`) omitted the answering user's identity and answer outcome metadata, and `<scion-chat-message>` (`renderChoiceResponse`) inferred `"you"` and lane alignment from `!this.fromAgent`. That assumption breaks for:
1. Agent-sent `agent_interpreted` choice responses (`fromAgent` is true even when the recorded answerer is the viewing user).
2. Multi-user threads and unset `currentUserId` viewers.
3. Phase 2 response outcomes (`confirm` and `override` of an interpreted answer), which require distinct Mock P2-7 wordings, icons, and the overridden choice label (`previousChoiceLabel`).

## Changes

### Hub (`pkg/hub/choices_view.go`, `pkg/hub/choices_answer.go`, `pkg/hub/choices_batch.go`)
- Extended `ChoiceResponseView` with `outcome`, `answeredByUserId`, `answeredByName`, `interpretedByAgentId`, and `previousChoiceLabel` (`omitempty`).
- Extended `ChoiceResponseSetAnswer` with `answeredByUserId`, `answeredByName`, `interpretedByAgentId`, `sourceMessageId`, `promptMessageId`, and `previousChoiceLabel` (`omitempty`), and made `ChoiceResponseSetView.PromptMessageID` `omitempty` (omitted when a batch spans multiple prompts, while preserving per-answer `promptMessageId`).
- Added shared view builders (`choiceResponseViewOf`, `choiceResponseSetAnswerOf`, `previousChoiceLabel`, `answererDisplayName`) used by both history enrichment (`choiceResponseViews` / `choiceResponseSetViewOf`) and live SSE publishing (`publishChoiceResponse` / `choiceResponseSetView`), eliminating drift between live events and history reloads.
- In `choiceResponseViews`, when any answer on the history page has `outcome == "override"`, batch-load the prompt decisions with answers via `GetDecisionsByMessageIDs` so `previousChoiceLabel` is populated on reload without extra queries on pages without overrides and without modifying `pkg/store/` (avoiding merge conflicts with parallel PR 2A, `ptone/scion#7797`).

### Web (`web/src/shared/types.ts`, `chat-message.ts`, `chat-choice-response.ts`, `copy-shoelace-icons.mjs`)
- Mirrored the new fields on `ChoiceResponseView`, `ChoiceResponseSetAnswer`, and `ChoiceResponseSetView` in `web/src/shared/types.ts`.
- Registered `patch-check` in `USED_ICONS` (`web/scripts/copy-shoelace-icons.mjs`); `robot` and `arrow-left-right` were already registered.
- Updated `ScionChatMessage.renderChoiceResponse` to compute `own = Boolean(this.currentUserId && answeredByUserId === this.currentUserId)` (never `!this.fromAgent`), place `agent_interpreted` responses in the left (`other`) lane while preserving `.isOwnAnswerer=${own}`, place direct responses in `own` vs `other` by `own`, and pass the recording agent's name (`senderName || sender`) on interpreted responses.
- Updated `<scion-chat-choice-response>` to render all four Mock P2-7 wordings across single and set responses:
  1. Direct (`answer`): Phase 1 wording preserved.
  2. Interpreted (`agent_interpreted`): robot icon, `<agent> recorded your/<who>'s answer ... from your/their reply`, `View reply` (`scroll-to-message` to `sourceMessageId`), footer `Jump to question` for single-prompt sets (`rs.promptMessageId` present), and `(other question)` + per-item `Jump` buttons (`a.promptMessageId`) without a footer `Jump to question` for multi-prompt sets (`rs.promptMessageId` omitted).
  3. Confirmed (`confirm`): `patch-check` icon, `<who> confirmed <label> for <key> · <time> · not sent to the agent · Jump`; `(confirmed)` per item in sets (and `not sent to the agent` in summary when all items are `confirm`).
  4. Overridden (`override`): `arrow-left-right` icon, `<who> changed <key> to <label> (was interpreted as <previousChoiceLabel>) · <time> · Jump`; `(changed from <previousChoiceLabel>)` / `(changed)` per item in sets.
- Updated existing Phase 1 response fixtures (`chat-thread-choices-answerability.test.ts` and `web/e2e/chat-mobile/choices-mock.ts`) to include `answeredByUserId` and `answeredByName`.

## Verification

- Hub shards & lint:
  - `make test-hub-shard SHARD=4` (existing choices suite) — passed.
  - `make test-hub-shard SHARD=5` (`TestChoiceResponseView_AttributionAndOutcomes`, `TestChoiceResponseSetView_AttributionAndOutcomes`) — passed.
  - `GOGC=40 golangci-lint run --new-from-rev=upstream-main --concurrency=1 --tests=false ./pkg/hub` + `make vet-hub-shard SHARD=4` + `make vet-hub-shard SHARD=5` — 0 issues.
- Web checks:
  - `cd web && npm run typecheck && npm run lint && npm run lint:e2e && npm test && npm run build` — passed (all 522 Vitest files).
  - `cd web && CHROMIUM_EXECUTABLE=/usr/bin/chromium npx playwright test -c e2e/chat-mobile/playwright.config.ts e2e/chat-mobile/choices.pw.ts` — passed on `chromium-390` and `desktop-1440` (light & dark), including new Mock P2-7 test.
- Full repository fast CI (`make ci`) — passed after cleaning untracked build/test artifacts.
