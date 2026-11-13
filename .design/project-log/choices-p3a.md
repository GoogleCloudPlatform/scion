# Choices Phase 3A: Project Decisions API, Chat-Key Backfill, and Retention-Safe Authz (ptone/scion#7799)

**Date:** 2026-11-13
**Branch:** `scion/choices-p3a`
**Issues:** `ptone/scion#7799` (sub-issue of `ptone/scion#7753`; epic `ptone/scion#7514`; blocks `ptone/scion#7800`)

## Problem

Phases 1 and 2 shipped structured choice prompts, direct web-card answers, and agent-interpreted answers inside individual chat conversations, but users had no project-wide API to inspect open decisions across project topics and their own agent DMs, review an immutable chronological audit log of decisions made, or answer open decisions after 30-day chat message retention purged the original prompt `messages` row. In addition, `decisions` rows previously carried only `project_id` and `message_id` (not `chat_key`), and `decision_answers.surface` had no `"decisions_page"` enum value for answers submitted from the project Decisions page.

## Solution

Implemented the Phase 3A backend vertical slice across Ent schema/backfill, store queries, experiment registry, Hub project decisions endpoint, and retention-safe single/set answer authorization without modifying `pkg/hub/choices_view.go` (avoiding merge conflicts with parallel Phase 2 PR 2B `ptone/scion#7798`):

1. **Additive Schema, `decisions_page` Surface, and Startup Backfill (`pkg/ent/`, `pkg/store/`, `pkg/store/entadapter/`)**:
   - Added nullable `chat_key` string column (`Optional().Nillable().MaxLen(256).Immutable()`) and composite index `(project_id, chat_key, status)` on `Decision`, plus `"decisions_page"` (`store.DecisionSurfaceDecisionsPage`) on `DecisionAnswer.surface`. Because the schema change is purely additive, `pkg/version.DowngradeFloor` is unchanged.
   - Updated `createChoicePromptTx` and `decisionToStore` in `pkg/store/entadapter/decision_store.go` to populate `chat_key` from `msg.ThreadID` on write, and threaded optional `Surface` through `RecordAnswerSetInput` (defaulting to `native` when empty).
   - Added `BackfillDecisionChatKeys(ctx)` in `pkg/store/entadapter/decision_chat_key_backfill.go`, wired into `CompositeStore.Migrate(ctx)` with idempotent `HubSetting` completion marker `migration_decision_chat_keys_v1`: executes a set-based SQL `UPDATE decisions SET chat_key = (SELECT m.thread_id FROM messages m WHERE m.id = decisions.message_id AND m.thread_id IS NOT NULL AND m.thread_id <> '') WHERE chat_key IS NULL AND EXISTS (...)`, leaving rows whose prompt message was already purged or had a `NULL`/`''` `thread_id` as `NULL` so fail-closed queries exclude them.

2. **Experiment Registration (`pkg/experiments/`, `web/src/utils/feature-flags.ts`, `docs-site/`)**:
   - Registered `web.project_decisions` (`experiments.ProjectDecisions`, default `false`) across Go, TypeScript `FeatureFlags`, e2e-perf fixture/schema, admin policy allowlist, and reference documentation.

3. **Store Project-Decisions Queries (`pkg/store/decision.go`, `pkg/store/entadapter/decision_project_store.go`)**:
   - Added `ListProjectDecisionAgents(ctx, projectID)` using SQL `GROUP BY asker_agent_id` (`MAX(asker_agent_name)`) to return 1 row per distinct asking agent in the project.
   - Added `ListProjectOpenPrompts(ctx, projectID, chatKeys)`:
     - Visibility pushed into SQL via exact `project_id = ? AND chat_key IN (readableChatKeys)` (never `LIKE '%user:%'`), failing closed when `chat_key` is `NULL`/`''`.
     - Openness predicate matches prompts with at least one `pending` row OR a `decided` row with `NOT EXISTS` a `direct` answer in `decision_answers` (unconfirmed `agent_interpreted` answer).
     - Fetches sibling rows (`pending`, `decided`, `replaced`) per open `message_id` in `(position ASC)` order via `GetDecisionsByMessageIDs` and batch-checks `messages` existence (`PromptExistsInMessages`).
   - Added `ListProjectDecisionAnswers(ctx, projectID, chatKeys, filter, cursor, limit)` for `tab=made`:
     - Keyset pagination on `(decision_answers.created_at DESC, decision_answers.id DESC)` (`limit` default 50, max 200) over `decision_answers` filtered via `EXISTS (SELECT decisions.id WHERE project_id = ? AND chat_key IN (readableChatKeys))`.
     - Combinable filters: `agent` (UUID), `user`, `provenance` (`direct` where `provenance='direct' AND outcome='answer'`; `interpreted` / alias `agent_interpreted` where `provenance='agent_interpreted'`; `confirmed` where `provenance='direct' AND outcome='confirm'`; `overridden` where `provenance='direct' AND outcome='override'`), and inclusive UTC date bounds (`YYYY-MM-DD` mapped to `[00:00:00Z, 23:59:59.999999999Z]`, or RFC3339).
     - Batch-loads prompt and source-message existence in `messages` plus `previousChoiceLabel` (from the `agent_interpreted` row on `override` outcomes).
   - Added `ListProjectDecisionFacets(ctx, projectID, chatKeys)` using SQL `GROUP BY` (`LIMIT 500`) over `decisions` (`asker_agent_id`, `MAX(asker_agent_name)`) and `decision_answers` (`answered_by_user_id`, `MAX(answered_by_name)`, `MAX(answered_by_email)`) within `readableChatKeys`, so private DM participants never leak through filter dropdowns.

4. **Retention-Safe Single & Set Answer Authz + Project Decisions Handler (`pkg/hub/`)**:
   - Added `GetTopicIncludingDeleted` and `ListTopicsIncludingDeleted` on `WebChatStore` (`webchannel_store.go`, `webchannel_store_postgres.go`) so soft-deleting a topic never erases historical audit entries (`conversationAvailable: false`, `userCanPost: false`).
   - Updated `loadAuthorizedDecision` (`choices_answer.go`), `loadAuthorizedChoicePrompt` (`choices_batch.go`), `choiceResponseStoreMessage`, `choiceSetResponseStoreMessage`, `publishChoiceUpdatedAt`, and `publishReplacedDecisions` (`choices_send.go`) to authorize and publish against `d.ChatKey` (falling back to `prompt.ThreadID` when `d.ChatKey == ""` and the prompt still exists in `messages`), failing closed (`404`) when neither resolves or when a non-participant probes a private DM decision.
   - Tightened `authorizeDecisionChatKey` (`choices_answer.go`): for DM keys (`dm:agent:<peerID>:user:<userID>`), gone askers (`store.ErrNotFound` or soft-deleted `!ag.DeletedAt.IsZero()`) remain readable and answerable by the DM participant under undeliverable semantics, while live agent peers require `ActionRead` on the agent (so revoked project members receive `404`, while active members can still `GET` and `confirm` even if the live agent is on `MessageModeNone`); for soft-deleted project topics, project readers retain `GET` read access while `POST .../answer` continues to return `404`.
   - Unified `choiceAsker` (`choices_answer.go`) with `classifyProjectOpenPrompts` (`choices_project.go`) so both hard-deleted (`store.ErrNotFound`) and soft-deleted (`!ag.DeletedAt.IsZero()`) asking agents follow gone-asker undeliverable semantics.
   - Accepted `"native"` (default when omitted/empty) and `"decisions_page"` on `POST /api/v1/decisions/{rowId}/answer` and `POST /api/v1/choice-prompts/{messageId}/answers` (`400 validation_error` on any other value).
   - Added `GET /api/v1/projects/{projectId}/decisions` (`choices_project.go`, `project_subroutes.go`, `handlers_projects_core.go`, `authzop/catalog_chat.go` op `chat.projectdecisions.list`):
     - Dual experiment gate: requires both `web.chat_choices` and `web.project_decisions`.
     - Caller authz: human web session or UAT with `project:read` (`403` for agents, visitors, federated runtime identities, and ungranted UATs); UATs only include participant DM keys when the token also grants `inbox:read` on `ScopeSelf`.
     - Hoisted `chatUserAccess` outside the topic loop and derived viewer DM candidate keys (`dm:agent:<agentID>:user:<viewerID>`) solely from `ListProjectDecisionAgents`; skips `ListProjectDecisionFacets` completely when `limit == 0` (header badge probe).
     - `tab=pending` returns `ProjectPendingDecisionsResponse` (`counts: { waitingOnYou, waitingOnOthers }`, `prompts: []ProjectDecisionPromptView` with `waitingOnYou`, `canAnswer`, `disabledReason`, `waitingReasons: map[rowId]reason` (`read_only`, `message_denied`, `other_user_confirm`), `promptAvailable`, `conversationLabel`, `conversationAvailable`, `decisions: []DecisionView`, `agents`, `answerers`, `nextCursor`).
     - `tab=made` returns `ProjectMadeDecisionsResponse` (`counts`, `entries: []ProjectDecisionLogEntryView`, `agents`, `answerers`, `nextCursor`), preserving stored `answeredByName` (with `answeredByUserId` cleared by `ON DELETE SET NULL` and omitted) when a user is deleted and stored `askerAgentName` when an agent is deleted.

## Files Changed

| File | Change |
|------|--------|
| `pkg/ent/schema/decision.go`, `pkg/ent/schema/decision_answer.go`, `pkg/ent/...` | Additive `decisions.chat_key` (`MaxLen(256)`) + index `(project_id, chat_key, status)` and `decisions_page` surface enum |
| `pkg/store/decision.go` | `ChatKey`, `DecisionSurfaceDecisionsPage`, `RecordAnswerSetInput.Surface`, and project-decisions store interfaces/types |
| `pkg/store/entadapter/decision_store.go` | Populate `chat_key` on prompt creation, expose `ChatKey`, and thread set-answer `Surface` |
| `pkg/store/entadapter/decision_chat_key_backfill.go`, `composite.go`, `decision_chat_key_backfill_test.go` | Idempotent set-based `BackfillDecisionChatKeys` startup backfill (`migration_decision_chat_keys_v1`) and unit tests |
| `pkg/store/entadapter/decision_project_store.go`, `decision_project_store_test.go` | SQL-deduplicated `ListProjectDecisionAgents` & `ListProjectDecisionFacets`, `ListProjectOpenPrompts`, `ListProjectDecisionAnswers`, and SQLite tests |
| `pkg/experiments/registry.go`, `registry_test.go`, `pkg/hub/test_admin_policy.go`, `web/src/utils/feature-flags.ts`, `web/e2e-perf/budgets/fixture.mjs`, `web/e2e-perf/budgets/fixture-schema.json`, `docs-site/src/content/docs/reference/experiments.md` | `web.project_decisions` experiment registration across backend, frontend, perf budgets, and docs |
| `pkg/hub/webchannel_store.go`, `pkg/hub/webchannel_store_postgres.go`, `pkg/hub/handlers_read_switch_test.go` | `GetTopicIncludingDeleted` and `ListTopicsIncludingDeleted` on `WebChatStore` (SQLite, PostgreSQL, and test double) |
| `pkg/hub/choices_answer.go`, `choices_batch.go`, `choices_send.go` | `surface` request validation, `authorizeDecisionChatKey` (live DM agent `ActionRead` + soft-deleted topic read), soft-deleted `choiceAsker` parity, retention-safe purged-prompt paths |
| `pkg/hub/choices_project.go`, `choices_project_test.go`, `project_subroutes.go`, `handlers_projects_core.go` | `GET /api/v1/projects/{id}/decisions` handler (`tab=pending\|made`) and comprehensive Hub tests |
| `pkg/hub/authzop/catalog_chat.go`, `.design/authorization-operation-catalog.md`, `pkg/hub/authzop_catalog_method_inventory_test.go`, `pkg/hub/bearer_disposition_matrix_test.go` | `chat.projectdecisions.list` operation catalog entry, generated report, and authz guardrail coverage |

## Verification

- `go test -buildvcs=false ./pkg/store/... ./pkg/ent/entc/... ./pkg/experiments/... ./pkg/hub/authzop/... ./pkg/util/logging/...`
- `GOGC=25 env -u SCION_MODEL make test-hub-shard SHARD=4` and `SHARD=1`
- `env -u SCION_MODEL make golangci-lint` and `env -u SCION_MODEL make vet-hub-shard SHARD=4`
- `GOGC=25 env -u SCION_MODEL make ci && env -u SCION_MODEL make ent-check && env -u SCION_MODEL make tidy-check`
- Gate G3A live multi-user workstation Hub + SQLite verification (`/scion-volumes/scratchpad/projects/chat-choices/evidence/g3a/README.md`)
