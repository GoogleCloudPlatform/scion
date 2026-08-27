# Fix: Compute Divergence Match + ResolveOrCreateDMConversation Unit Tests

**Date:** 2026-08-27
**Branch:** `scion/dev-fix-divergence`
**Agent:** dev-fix-divergence

## Problem

All 6 dual-write call sites in Phase 5 hardcoded `Match: true` in their divergence logging. This meant `DivergenceMetrics.Mismatches()` always returned 0, making it impossible to detect routing disagreements between the old model (thread/sender-recipient pairs) and the new model (conversation resolution). The divergence logging is the gate for the S4 read-switch — a clean board that cannot be trusted is worse than no board at all.

Additionally, `ResolveOrCreateDMConversation` — the single function both backfill and dual-write depend on — had no dedicated unit tests.

## Changes

### `pkg/messaging/divergence.go`
- Added `ComputeDivergenceMatch(senderID, recipientID, threadID, convID)` — returns `(match bool, reason string)` based on:
  1. `convID == ""` → `false, "no-new-routing"`
  2. `senderID == "" && recipientID == ""` → `false, "unknown/no-old-routing"`
  3. `threadID != ""` → `false, "old-model-thread vs new-model-dm"`
  4. Normal DM agreement → `true, "both-models-dm-agreement"`
- Added `NewRoutingStr(convID)` helper for consistent `"conv:{id}"` / `"none"` formatting.

### `pkg/hub/handlers_agent_messaging.go` (4 sites)
- Replaced hardcoded `Match: true` with computed match via `ComputeDivergenceMatch`.
- Moved `LogDivergence` outside the `if convID != ""` guard so failed resolutions are logged as divergence signals.

### `pkg/hub/messagebroker.go` (2 sites)
- Same pattern as above, keeping the `!msg.Broadcasted` guard but moving divergence logging outside the `convID != ""` check.

### `pkg/messaging/conversation_test.go` (new)
- 7 unit tests for `ResolveOrCreateDMConversation`: happy path, empty sender, empty recipient, upsert error, external_ref format, ProjectID set, ProjectID nil.

### `pkg/messaging/divergence_test.go` (appended)
- 4 unit tests for `ComputeDivergenceMatch` covering all branches.
- 1 test for `NewRoutingStr`.

## Verification

- `go build -buildvcs=false ./...` — passes
- `go test ./pkg/messaging/...` — all 62 tests pass (0.006s)
- No remaining hardcoded `Match: true` in any call site (verified via grep)
