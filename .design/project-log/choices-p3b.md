# Choices Phase 3B: Project Decisions Web Page, Header Badge, Purged-Prompt Cards, and Live SSE (ptone/scion#7800)

**Date:** 2026-11-13
**Branch:** `scion/choices-p3b`
**Issues:** `ptone/scion#7800` (sub-issue of `ptone/scion#7753`; epic `ptone/scion#7514`; stacked on `ptone/scion#7799` and aligned with `ptone/scion#7798`; real-hub E2E uses `ptone/scion#7776`)

## Problem

Phase 3A (`ptone/scion#7799`) delivered the backend `GET /api/v1/projects/{id}/decisions` endpoint (`tab=pending|made`), `decisions.chat_key` backfill, `decisions_page` answer surface enum, and retention-safe answer authorization when 30-day chat message retention purges a prompt `messages` row. Users still lacked a project-level web page and navigation entry point to inspect pending choices across project topics and their own agent DMs, answer open prompts inline (including prompts whose original chat message has been purged), review and filter the append-only decision log on desktop and mobile viewports, and watch newly settled decisions leave `Waiting on you` live across open tabs.

## Solution

Implemented the Phase 3B web frontend slice across feature-flag gating, typed API clients, `<scion-chat-choices surface="decisions_page">`, the Project Detail header `Decisions` button and live badge, the `<scion-page-project-decisions>` page (`Waiting on you`, collapsed `Waiting on others`, and `Made` audit log), Vitest unit suites, mocked desktop/mobile Playwright tests, and a real-hub multi-user Playwright E2E test:

1. **Dual Experiment Gate, `surface` Prop, and Project Decisions Client (`web/src/utils/feature-flags.ts`, `web/src/client/`)**:
   - Exported `CHAT_CHOICES_FLAG = 'web.chat_choices'` and `projectDecisionsEnabled()` requiring both `web.project_decisions` and `web.chat_choices` to be enabled.
   - Added `ChoiceAnswerSurface = 'native' | 'decisions_page'` to `answerDecision` and `answerDecisionSet` (`web/src/client/chat-choices.ts`), omitting `surface` on default `'native'` calls and sending `"surface": "decisions_page"` when invoked from `<scion-chat-choices surface="decisions_page">` across single direct answers, interpreted confirmations, overrides, and multi-decision sets.
   - Added `web/src/client/project-decisions.ts` (`fetchProjectPendingDecisions`, `fetchProjectMadeDecisions`, `fetchProjectDecisionCounts`, `buildProjectDecisionsQuery`) with `AbortSignal` support and typed response models matching `pkg/hub/choices_project.go`.

2. **Project Detail Header `Decisions` Button + Live Badge & Icon Registration (`web/src/components/pages/project-detail.ts`, `web/src/components/app-shell.ts`, `web/scripts/copy-shoelace-icons.mjs`)**:
   - Registered `archive`, `chat-left-text`, `check2-circle`, `discord`, `microsoft-teams`, `slack`, `telegram`, and `window` in `USED_ICONS`.
   - Updated `app-shell.ts` `getPageTitle()` for `/projects/:id/decisions` (`Decisions` when `projectDecisionsEnabled()` is true, `Page Not Found` otherwise).
   - Added the `Decisions` `<sl-button>` (`signpost-split` prefix icon) and primary pill `<sl-badge>` (shown when `waitingOnYou > 0`, hidden at `0`) in `<scion-page-project-detail>`, probing `limit=0` on load and refreshing debounced on `choice-updated` and `chat-message-received` (`choice-prompt`), while issuing zero `/decisions` requests when either flag is off.

3. **Project Decisions Page (`web/src/components/pages/project-decisions.ts`, `web/src/client/main.ts`)**:
   - Wired lazy `/^\/projects\/[^/]+\/decisions$/` route in `web/src/client/main.ts`; renders `<scion-page-404>` and makes zero API calls unless `projectDecisionsEnabled()` is true.
   - **Tab 1 (`Waiting on you`)**:
     - Renders keyed (`repeat()`) `.pcard` wrappers with `aria-hidden` agent avatar initial, `in your DM` / `in <topic>` link (or muted plain text when `!conversationAvailable`), relative age (`formatRelative`), right-aligned `Open in chat` `#msg-...` permalink when `promptAvailable && conversationAvailable` vs muted `Original message no longer available` (`archive` icon, no link) when `!promptAvailable`, and `<scion-chat-choices surface="decisions_page">` with `@scroll-to-message` / `@choice-jump` navigation (`navigateTo`).
     - Renders the Mock P3-6 empty state (`Nothing is waiting on you` + `See decisions made` tab switcher), `Load more` cursor pagination, and collapsed-by-default `<sl-details summary="Waiting on others (N)">` listing keyed `other_user_confirm`, `read_only`, `message_denied`, and safe fallback items.
   - **Tab 2 (`Made`)**:
     - Combinable `Agent`, `Answered by` (`Anyone`, `You`, `answerers`), `Provenance` (`Any`, `Direct`, `Interpreted`, `Confirmed`, `Changed`), and `From`/`To` date filters converted to inclusive display-zone (`effectiveTimeZone()` / `parseWallClock`) RFC3339 instants with inline `From <= To` validation and `Clear filters`, collapsed behind an accessible (`aria-expanded` / `aria-controls`) `Filters` toggle button with active-count badge at `<= 640px` (Mock P3-5).
     - Keyed (`repeat()`) desktop 8-column `table.log` (`> 640px`, Mock P3-3) and mobile `.mlist` (`<= 640px`, Mock P3-5) with `.follow` row shading on confirmations/overrides, `you` / plain-text `(deleted user)` / name rendering, provenance badges (`interpreted` + conditional `reply` link gated on `sourceMessageAvailable && conversationAvailable`, `confirmed`, `changed` + `from <prev>`, `direct` + `recommended`), surface icons/labels (`Decisions page`, `Chat`, `Slack`, `Google Chat`, `Discord`, `Telegram`, `Teams`), availability-gated `Agent · where`, `Load more` keyset pagination, and empty/no-match states.
   - **Live SSE Updates**:
     - Subscribes `stateManager` to `{ type: 'project', projectId }` (alongside `user.<userId>.chat.>`) and listens to `choice-updated` (handling both project-scoped topic events and user-scoped DM events without `projectId`) and `chat-message-received` (`choice-prompt`), applying optimistic `patchDecisions` updates immediately and debouncing authoritative refreshes that preserve `Load more` pages (requesting `limit=max(50, min(200, loaded))` on `Pending`, prepending/merging page 1 on `Made` only while `Made` is visible and marking `madeStale` while hidden, and deferring rather than aborting any in-flight `Load more`).

## Files Changed

| File | Change |
|------|--------|
| `web/src/utils/feature-flags.ts` | Export `CHAT_CHOICES_FLAG` and `projectDecisionsEnabled()` dual-flag helper |
| `web/src/client/chat-choices.ts`, `chat-choices.test.ts` | Optional `ChoiceAnswerSurface` (`'native' \| 'decisions_page'`) on `answerDecision` and `answerDecisionSet` + unit tests |
| `web/src/components/shared/chat/chat-choices.ts`, `chat-choices.test.ts`, `chat-choices-set.test.ts` | `surface` `@property` on `<scion-chat-choices>` forwarded through single, confirm, override, and set answers + unit tests |
| `web/src/client/project-decisions.ts`, `project-decisions.test.ts` | Typed client and query builder for `GET /api/v1/projects/{id}/decisions` (`tab=pending\|made`, `limit=0`) + unit tests |
| `web/scripts/copy-shoelace-icons.mjs` | Register icons used on the Decisions page (`archive`, `chat-left-text`, `check2-circle`, `discord`, `microsoft-teams`, `slack`, `telegram`, `window`) |
| `web/src/components/app-shell.ts` | `/projects/:id/decisions` page-title handling gated on `projectDecisionsEnabled()` |
| `web/src/components/pages/project-detail.ts`, `project-detail-decisions.test.ts` | Header `Decisions` button and live `waitingOnYou` badge (Mock P3-1) + unit tests |
| `web/src/components/pages/project-decisions.ts`, `project-decisions.test.ts`, `web/src/components/tsconfig.component-tests.json`, `web/src/client/main.ts` | `<scion-page-project-decisions>` component, lazy route registration, and unit tests |
| `web/e2e/chat-mobile/project-decisions.pw.ts` | Mocked Playwright coverage for Mocks P3-1..P3-6 across desktop (`1440x900`) and mobile (`390x844`) in light and dark themes |
| `web/e2e/chat-choices/global-setup.ts`, `web/e2e/chat-choices/project-decisions.pw.ts` | Real-hub Playwright G3A browser scenario (multi-user DM isolation vs project admin, SQLite prompt purge durability, inline `decisions_page` answer, and live 2-tab SSE removal) |

## Verification

- Web unit & static checks: `npm run copy:shoelace-icons`, `npm run typecheck`, `npm run test`, `npm run lint`, `npm run lint:e2e`, `npm run test:e2e-perf`, `npm run build`, `npm run check:orphans`
- Mocked Playwright: `CHROMIUM_EXECUTABLE=/usr/bin/chromium npx playwright test -c e2e/chat-mobile/playwright.config.ts e2e/chat-mobile/project-decisions.pw.ts`
- Real-hub Playwright (run twice back-to-back for zero-flake verification): `CHROMIUM_EXECUTABLE=/usr/bin/chromium E2E_PREBUILT=1 npm --prefix web run test:e2e:chat-choices`
- Full repository CI gate: `GOGC=25 env -u SCION_MODEL make ci`
