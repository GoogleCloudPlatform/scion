# B2+B1+B14: DM Migration Atomicity, D-1 Guard Routing, Empty-Ref Ruling

**Date:** 2026-08-28
**Branch:** `scion/ca-msg-em6-b2b1b14`
**Base:** `ea35abfdd` (origin/main)

## Summary

Four commits addressing three bugs in the DM migration system:

### Commit 1: Shared predicate extraction

Extracted `messages.CheckDMParticipantKey(convKind, externalRef, principalKind, principalID)` into `pkg/messages/dm_key.go` — the canonical D-1 immutability guard implementation. Refactored `checkDMParticipantKey` in `pkg/store/entadapter/conversation_store.go` to delegate to it, wrapping errors with `store.ErrInvalidInput` to preserve existing `ErrorIs` behaviour.

### Commit 2: B2 — Atomicity fix in mergeConversation

**Bug:** The re-stamp loop in `mergeConversation` recorded errors in `result.Errors` but continued to soft-delete the old row. If re-stamping failed, messages were left pointing at a soft-deleted row — data loss.

**Fix:** Track re-stamp failures; abort before participant copy or soft-delete. Under-migrating is recoverable; deleting the source is not.

### Commit 3: B1 — D-1 guard routing in mergeConversation

**Bug:** `mergeConversation` blindly copied ALL old-row participants to the target via `AddParticipant`. A stranger in the old row's participant table would be copied. The D-1 guard in `AddParticipant` would reject the stranger, but the error was swallowed.

**Fix:** Before the participant copy loop, look up the target conversation's kind and external_ref. Filter each participant through `messages.CheckDMParticipantKey` before calling `AddParticipant`. Strangers are logged and skipped.

### Commit 4: B14 — Empty-ref ruling

**Bug:** `stepMergeOrRekeyEmptyRef` read participants from an empty-ref row and synthesized a key from them, fabricating an ACL from the listing index.

**Ruling:** An empty-ref direct row must be left keyless for operator review. Deriving a key from the participant index inverts the direction of authority. This does NOT contradict re-keying malformed-but-parseable keys (step 3b), where principals are already named in the data — that is normalization, not fabrication.

**Fix:** `stepMergeOrRekeyEmptyRef` now unconditionally skips, incrementing a new `EmptyRefSkipped` counter on `DMMigrationResult`.

## Mutation verification

| Fix | Test | Mutation (revert fix alone) | Result |
|-----|------|---------------------------|--------|
| B2 | `TestB2_MergeAbortsOnRestampFailure` | Remove `restampFailed` abort → old row soft-deleted | ✅ FAIL as expected |
| B1 | `TestB1_MergeRejectsStrangerParticipant`, `TestB1_SharedPredicate_MergeConversationDirectly` | Remove `CheckDMParticipantKey` filter → stranger copied | ✅ FAIL as expected |
| B14 | `TestB14_EmptyRefRowLeftKeyless` | Restore old fabrication logic → row re-keyed | ✅ FAIL as expected |

All three mutations confirmed: each test fails only when its specific fix is reverted, with positive controls confirming the selector selects.

## Gate results

- `gofmt -l .` — clean
- `make check-authz-guards` — no violations
- `make compat-literals` — clean
- `./hack/check-conversation-upsert-guard.sh` — no violations
- `make test-fast` — all pass
- `make build` — success
- `go test ./pkg/messaging/... -count=1` — all pass
- `go test ./pkg/messages/... -count=1` — all pass
- `go test -tags '!no_sqlite' ./pkg/store/entadapter/... -count=1` — all pass

## Files changed

- `pkg/messages/dm_key.go` — added `CheckDMParticipantKey`
- `pkg/messages/dm_key_test.go` — tests for shared predicate
- `pkg/store/entadapter/conversation_store.go` — refactored `checkDMParticipantKey` to delegate
- `pkg/messaging/dm_migration.go` — B2 atomicity, B1 guard routing, B14 skip, `EmptyRefSkipped` counter
- `pkg/messaging/dm_migration_test.go` — tests for B2, B1, B14 with mutation contracts
