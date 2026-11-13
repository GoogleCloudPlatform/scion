# Choices Phase 2B: Web Confirm/Change UX, Shared Actionability Table, and E2E Suites (ptone/scion#7798)

**Date:** 2026-11-13
**Branch:** `scion/choices-p2b`
**Issues:** `ptone/scion#7798` (sub-issue of `ptone/scion#7752`; epic `ptone/scion#7514`; depends on `ptone/scion#7775`, `ptone/scion#7776`, `ptone/scion#7797`)
**Stacked Prerequisite Base (`PREREQ_SHA`):** `3af4cd3bb5148889417897781652de03771c2394` (cherry-picked `scion/choices-p2-0` @ `d7c1ed0ac`, `scion/choices-p2-t` @ `56059c482`, and `scion/choices-p2a` @ `c8fb4c44d` onto `upstream-main` @ `a682c6dcc`)

## Problem

After Phase 2.0 (`ptone/scion#7775`), Phase 2.T (`ptone/scion#7776`), and Phase 2A (`ptone/scion#7797`), agents could record `agent_interpreted` answers from free-text replies and the Hub enforced `confirm` (logged without waking the agent) and `override` (waking the agent with `previousChoiceLabel`) semantics. However, the web `<scion-chat-choices>` card still treated all `status === 'decided'` rows as terminal read-only states (`d.status === 'pending'` guards), lacked out-of-order SSE ranking between `agent_interpreted` and `direct` answers when `confirm` preserved `decided_at`, omitted `previousChoiceLabel` on `AnswerView` when reloading overridden prompt cards from history, and omitted user attribution fields on live SSE interpreted set responses.

## Solution

Implemented the complete Phase 2B web confirm/change experience, Hub reload/SSE parity fixes, golden-table actionability pinning, and mocked + real-hub Playwright E2E coverage:

1. **Hub Reload Parity & Live SSE Set Attribution (`pkg/hub/`)**:
   - Added `PreviousChoiceLabel string` (`json:"previousChoiceLabel,omitempty"`) to `AnswerView` in `pkg/hub/choices_view.go` and populated `v.Decision.PreviousChoiceLabel = previousChoiceLabel(eff, d)` inside `decisionView`, ensuring overridden prompt cards retain `changed from the interpreted answer <Label>` across chat history reloads and `GET /api/v1/decisions/{rowId}`.
   - Updated `interpretedChoiceResponseSetView` in `pkg/hub/choices_agent.go` to construct per-row items via `choiceResponseSetAnswerOf(a, ds[i])`, preserving `answeredByUserId`, `answeredByName`, `interpretedByAgentId`, and `sourceMessageId` on live SSE `choice-response` set events.

2. **Shared `actionableReason` Golden Table & `statusRank` SSE Guard (`web/src/shared/`, `web/src/client/`)**:
   - Added `previousChoiceLabel?: string` and `DecisionAnswerView` alias in `web/src/shared/types.ts`.
   - Implemented `actionableReason(d, userId)` in `web/src/client/chat-choices.ts` mirroring `store.DecisionActionableReason` (`'' | 'replaced' | 'decided' | 'not_recorded_answerer' | 'not_actionable'`), pinned in `web/src/client/chat-choices.test.ts` against all 13 cases in `pkg/store/testdata/decision_actionable_cases.json`, and routed every actionability check in `web/src/client/chat-choices.ts` and `web/src/components/shared/chat/chat-choices.ts` through it.
   - Updated `statusRank` in `web/src/client/chat-choices.ts` (`pending: 0 < decided-interpreted: 1 < decided-direct: 2 < replaced: 3`) so out-of-order SSE or history merges never regress a `direct` confirm/override back to `agent_interpreted` even when `confirm` preserves `decided_at`.

3. **Single & Set Choice Card Confirm/Change UX (`web/src/components/shared/chat/chat-choices.ts`)**:
   - **Single cards (Mocks P2-1, P2-2, P2-3, P2-6):** Added header status badges (`interpreted`, `confirmed`, `changed`), interactive buttons + `Confirm "<label>"` row + `View reply` scroll action (`scroll-to-message`) for the recorded answerer, Enter-key no-op on focused interpreted cards, inline `.confirmbar` (`Change your answer from <Old> to <New>? <agent> will be told.`) with `Cancel` and `Change answer`, disabled buttons with `Only <name> can confirm or change this answer` tooltip for other viewers, `403 not_recorded_answerer` toast handling, and confirmed/changed terminal footers.
   - **Set cards (Mocks P2-4, P2-5):** Preserved native `<button type="button" aria-pressed="...">` elements, link/image markdown unwrapping inside buttons, `sameOriginImagesOnly`, and dark-theme `#fff` primary button contrast; pre-selected the viewer's own interpreted choice over `defaultChoiceId` (`will confirm` / `will change` chips, subtle `robot` outline on the original choice when changing); rendered other users' interpreted rows as `.readonly` with `<Name>'s answer` chip and tooltip while excluding them from the counter and `POST /api/v1/choice-prompts/{messageId}/answers` payload; rendered `M of M ready · N confirm(s) · N change(s)` when actionable rows include interpreted rows.

4. **Mocked & Real-Hub Playwright E2E (`web/e2e/`)**:
   - Added mocked Playwright scenes for Mocks P2-1..P2-6 across `desktop-1440` and `chromium-390` in light and dark modes in `web/e2e/chat-mobile/choices.pw.ts`, asserting ARIA `[pressed]` states and `rgb(255, 255, 255)` primary button contrast.
   - Added a 5s bounded cold-boot `503` readiness retry in `createProjectTopic` and two end-to-end Phase 2 multi-user SSE specs in `web/e2e/chat-choices/choices.pw.ts` verifying live SSE interpreted transitions (`will confirm` vs `Only <Name> can confirm or change this answer`), set submission convergence, single-card `confirm` (`logged_without_delivery`) vs `override` (`attempted_delivery` after the 2s broker debounce window), and `previousChoiceLabel` reload parity.

## Files Changed

| File | Change |
|------|--------|
| `pkg/hub/choices_view.go` | Added `PreviousChoiceLabel` to `AnswerView` and populated it in `decisionView` |
| `pkg/hub/choices_agent.go` | Used `choiceResponseSetAnswerOf` in `interpretedChoiceResponseSetView` for live SSE attribution |
| `pkg/hub/choices_test.go`, `choices_batch_test.go`, `choices_agent_test.go` | Added tests for `AnswerView.PreviousChoiceLabel` reload parity and live SSE set attribution |
| `web/src/shared/types.ts` | Added `previousChoiceLabel?: string` on `AnswerView` and `DecisionAnswerView` export |
| `web/src/client/chat-choices.ts` | Added `actionableReason` and 4-level `statusRank` (`pending < decided-interpreted < decided-direct < replaced`) |
| `web/src/client/chat-choices.test.ts` | Added golden parity test against `pkg/store/testdata/decision_actionable_cases.json` and out-of-order `statusRank` tests |
| `web/src/components/shared/chat/chat-choices.ts` | Implemented single & set card confirm/change UX (Mocks P2-1..P2-6) |
| `web/src/components/shared/chat/chat-choices.test.ts`, `chat-choices-set.test.ts` | Added Phase 2 unit tests for single and set card confirm, change, tooltips, and reload parity |
| `web/e2e/chat-mobile/choices.pw.ts` | Added mocked Playwright E2E scenes for Mocks P2-1..P2-6 on desktop and mobile (light & dark) |
| `web/e2e/chat-choices/choices.pw.ts` | Added cold-boot `503` retry guard and real-hub Phase 2 multi-user SSE + delivery classification + reload parity specs |

## Verification

- `make test-hub-shard SHARD=4`, `SHARD=5`, `SHARD=6` and `make vet-hub-shard SHARD=6`
- `GOGC=40 golangci-lint run --tests=false --new-from-rev=upstream-main --concurrency=1 ./pkg/hub`
- `cd web && npm test && npm run typecheck && ./node_modules/.bin/tsc --noEmit -p e2e/chat-choices/tsconfig.json && npm run lint && npm run lint:e2e`
- `cd web && CHROMIUM_EXECUTABLE=/usr/bin/chromium ./node_modules/.bin/playwright test -c e2e/chat-mobile/playwright.config.ts e2e/chat-mobile/choices.pw.ts` (72 passed)
- `cd web && CHROMIUM_EXECUTABLE=/usr/bin/chromium E2E_PREBUILT=1 npm run test:e2e:chat-choices` (2 consecutive runs, 0 flakes)
- `env -u SCION_MODEL make ci`
