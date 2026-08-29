# C3 — Port pkg/messaging Library Files

**Date:** 2026-08-29
**Branch:** `scion/ca-msg-em6-c3-messaging` (base: `8b09c118f` = upstream/main)
**Phase:** em9-unify re-derivation, Phase C3

## Files Ported

### M-ADD (13 files — mechanical extraction from em9, identical on both source branches)

| File | Lines |
|------|-------|
| `pkg/messaging/delivery.go` | +101 |
| `pkg/messaging/delivery_compat.go` | +90 |
| `pkg/messaging/delivery_compat_test.go` | +368 |
| `pkg/messaging/delivery_test.go` | +438 |
| `pkg/messaging/envelope.go` | +362 |
| `pkg/messaging/envelope_compat.go` | +431 |
| `pkg/messaging/envelope_compat_test.go` | +936 |
| `pkg/messaging/envelope_test.go` | +483 |
| `pkg/messaging/validate.go` | +150 |
| `pkg/messaging/validate_compat.go` | +108 |
| `pkg/messaging/validate_compat_test.go` | +424 |
| `pkg/messaging/validate_test.go` | +598 |
| `pkg/messaging/VALIDATION_EXEMPTIONS.md` | +50 |

### M-MOD (4 files — hunk-ported onto main's current copy)

| File | Added | Removed | Justification for deletions |
|------|-------|---------|---------------------------|
| `pkg/messaging/derive_key.go` | +96 | -1 | `Surface: "native"` replaced with `Surface: cfg.surface` |
| `pkg/messaging/derive_key_test.go` | +254 | -0 | Pure addition |
| `pkg/messaging/conversation.go` | +55 | -1 | Old return line replaced with topic-lookup forwarding block |
| `pkg/messaging/conversation_test.go` | +246 | -0 | Pure addition |

### Skipped (3 files)

| File | Reason |
|------|--------|
| `pkg/messaging/backfill.go` | IDENTICAL on main and em9 |
| `pkg/messaging/backfill_test.go` | em9 removes B6/B7 EnsureParticipant mock (16 lines), adds only 1 cosmetic line. Not worth the risk. |
| `pkg/messaging/resolve.go` | em9 changes only 2 whitespace characters (alignment). Not worth touching. |

## Endpoint Deletion Counts

Total deletions across all files: **2 lines** (both justified M-MOD replacements).

- `conversation.go -1`: replaced `return ResolveOrCreateConversationByKey(ctx, cs, log, extRef, kind, projID)` with topic-lookup forwarding block
- `derive_key.go -1`: replaced `Surface: "native"` with `Surface: cfg.surface` (now configurable via WithSurface option)

## Security Constraints Observed

1. **DM key IS the ACL** — golden test vectors preserved unchanged
2. **Parse failure denies** — no fallback, no repair paths added
3. **Never normalise a DM key on derivation path** — canonicality check returns original key verbatim
4. **Derivation failures non-fatal** (B10 ruling) — all resolve functions return nil on error
5. **B6/B7 security fix preserved** — EnsureParticipant, nil-pe guard, all participant interfaces retained
6. **No M-MOD files were file-copied** — all 4 were hunk-ported onto main's current copy

## B6/B7 Preservation Verification

### conversation.go (all present ✅)
- `type ParticipantAdder interface` — line 57
- `type ParticipantEnsurer interface` — line 65
- `pe ParticipantEnsurer` in DM function signature — line 87
- `if pe == nil {` guard — line 128
- `pe.EnsureParticipant(ctx, ...)` loop — line 157

### conversation_test.go (all present ✅)
- `AddParticipant` and `EnsureParticipant` methods on mock — lines 64, 72
- `participants`, `addPartErr`, `ensuredParticipants`, `ensurePartErr` fields — lines 38-44
- `TestResolveOrCreateDMConversation_RegistersBothParticipants` — line 246
- `TestResolveOrCreateDMConversation_ParticipantErrorIsNonFatal` — line 281
- `TestResolveOrCreateDMConversation_ThirdPartyGuardDocumented` — line 304
- `TestResolveOrCreateDMConversation_IdempotentEnsure` — line 336
- `TestResolveOrCreateDMConversation_NilParticipantEnsurer` — line 574

## Build/Test/Guard Results

| Check | Result |
|-------|--------|
| `go build ./...` | ✅ pass |
| `go test -tags no_sqlite ./pkg/messaging/...` | ✅ pass (0.010s) |
| `check-conversation-upsert-guard.sh` | ✅ exit 0 |
| `check-authz-guards.sh` | ✅ exit 0 |
| `check-security-marker-gates.sh` | ❌ exit 1 (pre-existing on upstream/main — unrelated to C3) |

The security-marker-gates failure is a pre-existing condition on upstream/main. The script checks for `ActionAttach` markers in handler files that are not part of C3's scope. Running the same script against a clean upstream/main checkout produces identical failures.
