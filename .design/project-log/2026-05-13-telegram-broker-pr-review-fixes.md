# Telegram Broker PR #66 Review Fixes

**Date**: 2026-05-13
**Branch**: scion/fix-issue-43
**Agent**: fix-fb-pr66

## Summary

Addressed 2 critical and 4 medium severity issues from the code review on PR #66 (Telegram Bot Message Broker Plugin).

## Changes

### Critical Fixes
1. **C1 - UTF-8 truncation bug** (`format.go`): `truncateMessage` was slicing on byte boundaries, which splits multi-byte UTF-8 characters (emoji, CJK). Fixed by walking backward to a valid rune boundary using `unicode/utf8.RuneStart`.

2. **C2 - Data race on `hubReceived`** (`plugin_integration_test.go`): Plain `int` was accessed from multiple goroutines without synchronization. Changed to `int32` with `sync/atomic` operations.

### Medium Fixes
3. **M1 - InboundHandler without lock** (`telegram.go`): `b.InboundHandler` was read in `deliverInbound` without holding `b.mu`. Restructured to read the handler under `b.mu.RLock()` along with other fields.

4. **M2 - Only last error returned from Publish** (`telegram.go`): When sending to multiple chats, earlier failures were lost. Changed to aggregate all errors using `errors.Join`.

5. **M4 - Response body not drained** (`telegram.go`): Added `io.Copy(io.Discard, resp.Body)` before `resp.Body.Close()` to enable HTTP connection reuse.

6. **M5 - Bot token in errors** (`api.go`): Added `redactToken` helper that replaces the bot token with `[REDACTED]` in error strings from HTTP client calls.

## Verification
- `go build ./...` - passes
- `go test ./pkg/plugin/telegram/... -race -count=1` - all tests pass
- `go vet ./pkg/plugin/telegram/... ./cmd/scion-plugin-telegram/...` - clean
