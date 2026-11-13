# Choices Phase 3A: Project Decisions API, Chat-Key Backfill, and Retention-Safe Authz (ptone/scion#7799)

**Date:** 2026-11-13
**Branch:** `scion/choices-p3a`
**Issues:** `ptone/scion#7799` (sub-issue of `ptone/scion#7753`; epic `ptone/scion#7514`; blocks `ptone/scion#7800`)

## Problem

Phases 1 and 2 shipped structured choice prompts, direct web-card answers, and agent-interpreted answers inside individual chat conversations, but users had no project-wide API to inspect open decisions across project topics and their own agent DMs, review a chronological audit log of decisions made, or answer open decisions after 30-day chat message retention purged the original prompt `messages` row. In addition, `decisions` rows previously carried only `project_id` and `message_id` (not `chat_key`), and `decision_answers.surface` had no `"decisions_page"` enum value for answers submitted from the project Decisions page.

## Solution

Implemented the complete Phase 3A backend vertical slice across Ent schema/backfill, store queries, experiment registry, Hub project decisions endpoint, and retention-safe single/set answer authorization without modifying `pkg/hub/choices_view.go` (avoiding merge conflicts with parallel Phase 2 PR 2B `ptone/scion#7798`):

1. **Additive Schema, `decisions_page` Surface, and Startup Backfill (`pkg/ent/`, `pkg/store/`, `pkg/store/entadapter/`)**:
   - Added nullable `chat_key` string column (`Optional().Nillable().MaxLen(255)`) and composite index `(project_id, chat_key, status)` on `Decision`, plus `"decisions_page"` (`store.DecisionSurfaceDecisionsPage`) on `DecisionAnswer.surface`. Because the schema change is purely additive, `pkg/version.DowngradeFloor` is unchanged.
   - Updated `createChoicePromptTx` and `decisionToStore` in `pkg/store/entadapter/decision_store.go` to populate `chat_key` from the prompt message on write, and threaded optional `Surface` through `RecordAnswerSetInput` (defaulting to `native` when empty).
   - Added `BackfillDecisionChatKeys(ctx)` in `pkg/store/entadapter/decision_chat_key_backfill.go`, wired into `CompositeStore.Migrate(ctx)` with idempotent `HubSetting` marker `migration_decision_chat_keys_v1`: joins `decisions` (`chat_key IS NULL OR chat_key = ''`) to `messages.chat_key` in batches of 500, logs unbackfillable orphaned rows (`chat_key` left `NULL` so fail-closed queries exclude them), and writes the marker only when zero joinable rows remain.

2. **Experiment Registration (`pkg/experiments/`, `web/src/utils/feature-flags.ts`, `docs-site/`)**:
   - Registered `web.project_decisions` (`experiments.ProjectDecisions`, default `false`) across Go, TypeScript `FeatureFlags`, e2e-perf fixture/schema, admin policy allowlist, and reference documentation.

3. **Store Project-Decisions Queries (`pkg/store/decision.go`, `pkg/store/entadapter/decision_project_store.go`)**:
   - Added `ListProjectDecisionAgents(ctx, projectID)` returning distinct `asker_agent_id` values with non-empty `chat_key`.
   - Added `ListProjectOpenPrompts(ctx, ListProjectOpenPromptsOptions)`:
     - Visibility pushed into SQL via `chat_key LIKE 'thread:' || projectID || ':%' OR chat_key IN (exactParticipantDMKeys)` (never `LIKE '%user:%'`), failing closed when `chat_key` is `NULL`/`''`.
     - Openness predicate matches prompts with at least one `pending` row OR a `decided` row whose latest answer has `provenance = 'agent_interpreted'` (unconfirmed interpreted answer).
     - Fetches sibling rows (`pending`, `decided`, `replaced`) per open `message_id` in `(position ASC)` order and batch-checks `messages` existence (`PromptAvailable`, excluding soft-deleted rows via Hub `webChatStore.GetMessageExts`).
   - Added `ListProjectDecisionAnswers(ctx, ListProjectDecisionAnswersOptions)` for `tab=made`:
     - Keyset pagination on `(decision_answers.created_at DESC, decision_answers.id DESC)` (`limit` default 50, max 200) over `decision_answers` joined to `decisions` under the same SQL visibility predicate.
     - Filters: `agent`, `user`, `provenance` (`all`, `direct` where `provenance='direct' AND outcome='answer'`, `interpreted` where `provenance='agent_interpreted'`, `overridden` where `provenance='direct' AND outcome IN ('confirm','override')`), and UTC half-open `[from, to+24h)` date range (`YYYY-MM-DD`).
     - Batch-loads prompt availability, source-message availability, and `previousChoiceLabel` (from the latest preceding `agent_interpreted` row on `confirm`/`override`).
   - Added `ListProjectDecisionFacets(ctx, projectID, visibleDMChatKeys)` returning distinct asking agent IDs and answering user IDs strictly within the caller's visible conversation set so private DM participants never leak through filter dropdowns.

4. **Retention-Safe Single & Set Answer Authz + Project Decisions Handler (`pkg/hub/`)**:
   - Updated `loadAuthorizedDecision` (`choices_answer.go`), `loadAuthorizedChoicePrompt` (`choices_batch.go`), `choiceResponseStoreMessage`, `choiceSetResponseStoreMessage`, `publishChoiceUpdatedAt`, and `publishReplacedDecisions` (`choices_send.go`) to authorize and publish against `d.ChatKey` even after chat message retention purges the prompt `messages` row (`PromptAvailable = false`), while failing closed (`404`) if `d.ChatKey == ""` or if a non-participant (including project owner / super-admin) probes a private DM decision.
   - Added `authorizeDecisionChatKey` so participants can still answer pending decisions when the asking agent was hard-deleted or later switched to `MessageModeNone`.
   - Accepted `"native"` (default when omitted/empty) and `"decisions_page"` on `POST /api/v1/decisions/{rowId}/answer` and `POST /api/v1/choice-prompts/{messageId}/answers` (`400 validation_error` on any other value).
   - Added `GET /api/v1/projects/{projectId}/decisions` (`choices_project.go`, `project_subroutes.go`, `handlers_projects_core.go`, `authzop/catalog_chat.go` op `chat.projectdecisions.list`):
     - Dual experiment gate: requires both `web.chat_choices` and `web.project_decisions`.
     - Caller authz: human web session or UAT with `project:read` (`403` for agents, visitors, federated runtime identities, and ungranted UATs); UATs only include participant DM keys when the token also grants `inbox:read` on `ScopeSelf`.
     - `tab=pending` returns `ProjectPendingDecisionsResponse` (`counts: { waitingOnYou, waitingOnOthers }`, `prompts: []ProjectPendingPromptView` with `waitingOnYou`, `canAnswer`, `waitingReasons` (`other_user_confirm` / `agent_access`), `promptAvailable`, `conversationLabel`, `conversationAvailable`, `decisions: []DecisionView`, `agents`, `answerers`, and `limit=0` badge-probe fast path).
     - `tab=made` returns `ProjectMadeDecisionsResponse` (`entries: []ProjectDecisionAnswerEntry`, `nextCursor`, `agents`, `answerers`) with `"Deleted user"` / stored-slug fallback for deleted principals.

## Files Changed

| File | Change |
|------|--------|
| `pkg/ent/schema/decision.go`, `pkg/ent/schema/decision_answer.go`, `pkg/ent/...` | Additive `decisions.chat_key` + index `(project_id, chat_key, status)` and `decisions_page` surface enum |
| `pkg/store/decision.go` | `ChatKey`, `DecisionSurfaceDecisionsPage`, `RecordAnswerSetInput.Surface`, and project-decisions store interfaces/types |
| `pkg/store/entadapter/decision_store.go` | Populate `chat_key` on prompt creation, expose `ChatKey`, and thread set-answer `Surface` |
| `pkg/store/entadapter/decision_chat_key_backfill.go`, `composite.go`, `decision_chat_key_backfill_test.go` | Idempotent `BackfillDecisionChatKeys` startup backfill (`migration_decision_chat_keys_v1`) and unit tests |
| `pkg/store/entadapter/decision_project_store.go`, `decision_project_store_test.go` | Store implementations and SQLite tests for `ListProjectDecisionAgents`, `ListProjectOpenPrompts`, `ListProjectDecisionAnswers`, and `ListProjectDecisionFacets` |
| `pkg/experiments/registry.go`, `registry_test.go`, `pkg/hub/test_admin_policy.go`, `web/src/utils/feature-flags.ts`, `web/e2e-perf/budgets/fixture.mjs`, `web/e2e-perf/budgets/fixture-schema.json`, `docs-site/src/content/docs/reference/experiments.md` | `web.project_decisions` experiment registration across backend, frontend, perf budgets, and docs |
| `pkg/hub/choices_answer.go`, `choices_batch.go`, `choices_send.go` | `surface` request validation, `authorizeDecisionChatKey`, retention-safe purged-prompt answer/set-answer/replace paths |
| `pkg/hub/choices_project.go`, `choices_project_test.go`, `project_subroutes.go`, `handlers_projects_core.go` | `GET /api/v1/projects/{id}/decisions` handler (`tab=pending\|made`) and end-to-end Hub tests |
| `pkg/hub/authzop/catalog_chat.go`, `.design/authorization-operation-catalog.md`, `pkg/hub/authzop_catalog_method_inventory_test.go`, `pkg/hub/bearer_disposition_matrix_test.go` | `chat.projectdecisions.list` operation catalog entry, generated report, and authz guardrail coverage |

## Verification

- `go test -buildvcs=false ./pkg/store/... ./pkg/ent/entc/... ./pkg/experiments/... ./pkg/hub/authzop/... ./pkg/util/logging/...`
- `env -u SCION_MODEL make test-hub-shard SHARD=4`, `SHARD=1`, `SHARD=5` and `make vet-hub-shard SHARD=4`
- `cd web && npm run typecheck && npm run lint && npm run test:e2e-perf && npx vitest run --maxWorkers=2 src/utils/`
- `env -u SCION_MODEL make golangci-lint` and `env -u SCION_MODEL make ci`
- Gate G3A live multi-user workstation Hub + SQLite verification (`/scion-volumes/scratchpad/projects/chat-choices/evidence/g3a/README.md`)
