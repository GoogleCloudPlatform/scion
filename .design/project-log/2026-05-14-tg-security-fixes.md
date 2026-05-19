# Telegram Plugin Security & Safety Fixes

**Date**: 2026-05-14
**Branch**: scion/chat-tee
**Scope**: extras/scion-telegram/internal/telegram/

## Changes

### C1. Path Traversal in resolveAttachmentPath (CRITICAL)

**File**: broker_v2.go

The `resolveAttachmentPath` function previously trimmed a `/workspace/` prefix and concatenated the remainder into a host-side path using string concatenation. An attacker-controlled attachment path like `/workspace/../../../etc/passwd` would pass the `HasPrefix` guard but produce a `relPath` of `../../../etc/passwd`, escaping the project directory.

**Fix applied:**
- `filepath.Clean` the relative path immediately after `TrimPrefix`.
- Reject any relative path that starts with `..` or is absolute — log a warning and return the original path unchanged.
- Use `filepath.Join` instead of string concatenation to build `hostPath`.
- Post-join bounds check: verify the resolved `hostPath` is still under the expected project directory prefix; reject otherwise.

### C2. Webhook Secret Not Constant-Time (CRITICAL)

**File**: webhook.go

The secret-token comparison used Go's `!=` operator, which short-circuits on the first differing byte. This is susceptible to timing side-channel attacks.

**Fix applied:**
- Replaced `token != ws.secretToken` with `subtle.ConstantTimeCompare([]byte(token), []byte(ws.secretToken)) != 1`.
- Added `"crypto/subtle"` to imports.

### I2. In-Place Slice Mutation in Observer/Commentary Filter (IMPORTANT)

**File**: broker_v2.go

Two filter loops used `chatIDs[:0]` to re-use the backing array for filtered output. This mutates the underlying slice, which can corrupt the caller's view of `chatIDs` if the slice is referenced elsewhere.

**Fix applied (2 instances):**
- Observer-mode filter (~line 582): replaced `chatIDs[:0]` with `make([]int64, 0, len(chatIDs))`.
- Commentary filter (~line 605): same replacement.

## Verification

- All existing tests pass: `cd extras/scion-telegram && go test ./...` — OK.
