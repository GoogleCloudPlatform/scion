# Choices Phase 2A: Agent Decision API, Interpreted Answers, and CLI (ptone/scion#7797)

**Date:** 2026-11-13
**Branch:** `scion/choices-p2a`
**Issues:** `ptone/scion#7797` (sub-issue of `ptone/scion#7752`; epic `ptone/scion#7514`; blocks `ptone/scion#7798`)

## Problem

Phase 1 enabled structured choice prompts and direct web-card answers, but agents had no API or CLI to inspect their own decisions across restarts or record interpreted answers when a user replied in free text (in web chat or over a bridge). In addition, `store.DecisionActionableReason` needed a shared golden case table so Phase 2B's TypeScript actionability logic stays pinned against the Go store without drift.

## Solution

Implemented the complete Phase 2A vertical slice across store, Hub agent routes, Hub client, and `scion decisions` CLI without schema migrations:

1. **Store (`pkg/store/`, `pkg/store/entadapter/`)**:
   - Added `pkg/store/testdata/decision_actionable_cases.json` (13 named cases covering pending, addressed/unaddressed, replaced, direct outcomes, and `agent_interpreted` match/mismatch/deleted-user/anonymous) and `pkg/store/decision_actionable_test.go` pinning `store.DecisionActionableReason`.
   - Added `ListAgentDecisions` scoped to `asker_agent_id` (`pending`, `decided`, `all`), ordered `created_at DESC, id DESC` with opaque cursor pagination (`limit` default 50, max 200) and `Answers` loaded.
   - Added `GetAgentDecisionVersions` returning all versions (`version ASC`) for `(agentID, normalizedKey)` with every `decision_answer` row (`agent_interpreted` and `direct`, `created_at ASC`).
   - Added `RecordInterpretedAnswers` executing an all-or-nothing transaction with row locking for 1 to 10 `{key, choice_id}` pairs across one or more prompts in the same conversation, scoped idempotency (`(caller_agent_id, idempotency_key)` with `200 replayed` or `422 idempotency_mismatch`), shared `batch_id` when 2+ pairs, and a single `choice-response` message (`sender = agent:<slug>`, marked `dispatched` without waking the asking agent).

2. **Hub Agent API, Surface Mapper, SSE & Guardrails (`pkg/hub/`, `pkg/util/logging/`)**:
   - Added `GET /api/v1/agent/decisions`, `GET /api/v1/agent/decisions/{key}`, and `POST /api/v1/agent/decisions/interpreted` in `pkg/hub/choices_agent.go`, gated by `requireExperiment(experiments.ChatChoices)` and restricted to `AgentIdentity` (`403 agents_only` otherwise).
   - Enforced the 8-step validation order on `POST /api/v1/agent/decisions/interpreted`: shape (`1..10`, non-empty, unique normalized keys), active-version resolution (`404 decision_not_found`), source message checks (`400 source_wrong_conversation`, `400 source_not_user`, `400 source_before_prompt` across every prompt in the batch), source author permission (`authorizeChatSend` and DM addressee match -> `403 source_author_not_allowed`), transactional state/choice checks (`409 already_answered` with descriptive per-key reason and `DecisionView`, `409 replaced`, `400 invalid_choice`, `422 idempotency_mismatch`), `Channel -> DecisionSurface` mapping (`""/web/native -> native`, `gchat/google_chat/chat-app -> google_chat`, `slack`, `discord`, `telegram`, `teams`, else `other`), and SSE publishing (`choice-response` + per-row `*.chat.choice.updated` with zero agent deliveries).
   - Updated `ChoiceResponseSetView.PromptMessageID` to `omitempty` (omitted when an interpreted batch spans multiple prompts) and added per-answer `PromptMessageID` on `ChoiceResponseSetAnswer` in `pkg/hub/choices_view.go`.
   - Updated route classification (`RouteAgentToken`), authz catalog/manifest, and request-path logging allowlist tests.

3. **Hub Client & CLI (`pkg/hubclient/`, `cmd/`)**:
   - Added `HubClient.Decisions()` (`List`, `Show`, `RecordInterpreted`) in `pkg/hubclient/decisions.go` using `apiclient.DecodeRequired`.
   - Added `scion decisions list [--pending|--decided|--all] [--json]`, `scion decisions show <key> [--json]`, and `scion decisions record` (supporting both `<key> <choice-id> --from <msg>` and `--from <msg> <key>=<choice-id>...`, rejecting mixed forms, generating a UUID idempotency key, printing friendly `web.chat_choices` guidance on `404 experiment_disabled`, and rendering hub per-key conflict reasons verbatim).
   - Enabled `agentAllowed["decisions"] = true` (`cmd/cli_mode.go`) and `windowsAllowed["decisions"] = true` (`cmd/platform_gate.go`).

## Files Changed

| File | Change |
|------|--------|
| `pkg/store/testdata/decision_actionable_cases.json` | Shared golden test table for Go `DecisionActionableReason` and Phase 2B TS `actionableReason` |
| `pkg/store/decision_actionable_test.go` | Golden-table unit test for `DecisionActionableReason` |
| `pkg/store/decision.go` | Store types, sentinel errors, and `DecisionStore` methods (`ListAgentDecisions`, `GetAgentDecisionVersions`, `RecordInterpretedAnswers`) |
| `pkg/store/entadapter/decision_store.go` | Ent adapter implementations with cursor pagination, full answer loading, and locked atomic interpreted recording |
| `pkg/store/entadapter/decision_store_test.go` | Store tests including concurrent `RecordInterpretedAnswers` vs `RecordAnswer` race test |
| `pkg/hub/choices_agent.go` | Agent decision handlers, request/response views, validation steps 1–8, surface mapper, and SSE publishing |
| `pkg/hub/choices_agent_test.go` | Hub integration tests covering authz, isolation, pagination, validation rules, concurrency/rollback, mixed user sets, and surface mapping |
| `pkg/hub/choices_view.go` | Optional envelope `PromptMessageID` and per-item `ChoiceResponseSetAnswer.PromptMessageID` |
| `pkg/hub/choices_batch.go` | Populate per-item `PromptMessageID` on user set responses |
| `pkg/hub/server.go`, `route_metadata.go`, `route_authz_manifest.go`, `route_classification_test.go`, `pkg/hub/authzop/catalog.go`, `pkg/util/logging/request_path_sites_test.go` | Route registration and guardrail classifications |
| `pkg/hubclient/client.go`, `decisions.go`, `decisions_test.go`, `pkg/templatecache/hydrator_test.go` | Typed `DecisionsService` client methods and unit tests |
| `cmd/decisions.go`, `decisions_list.go`, `decisions_show.go`, `decisions_record.go`, `decisions_test.go`, `cli_mode.go`, `platform_gate.go` | `scion decisions` CLI commands, agent-mode/Windows gates, and tests |

## Verification

- `go test -buildvcs=false ./pkg/store/...`
- `make test-hub-shard SHARD=6`, `SHARD=1`, `SHARD=4`, `SHARD=5`
- `go test -buildvcs=false ./pkg/util/logging/... ./pkg/hub/authzop/... ./pkg/hubclient/... ./pkg/templatecache/... ./cmd/...`
- `GOOS=windows GOARCH=amd64 go build -buildvcs=false -o /dev/null ./cmd/scion` and `bash hack/winsec-guard.sh`
- Gate G2A live workstation Hub + Web + Playwright end-to-end verification (`/scion-volumes/scratchpad/projects/chat-choices/evidence/g2a/README.md`)
- Scoped `golangci-lint` and `env -u SCION_MODEL make ci`
