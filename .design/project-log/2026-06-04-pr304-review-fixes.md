# PR #304 Review Feedback Fixes

**Date**: 2026-06-04
**Branch**: `pr/postgres-core`
**PR**: #304

## Changes Made

Addressed all three review comments from the automated code review:

### 1. Context leak in `initStore` (medium priority)
- `initStore` was using `context.Background()` for `s.Migrate()` and `s.Ping()`.
- Refactored `initStore` to accept a `context.Context` parameter.
- The server's cancellable context (from `runServerStart`) is now threaded through, allowing graceful cancellation on Ctrl+C during startup.

### 2. Goroutine/connection leak in event publisher (high priority)
- `initWebServer` was calling `newEventPublisher(context.Background(), cfg)`.
- The Postgres event publisher starts a LISTEN/NOTIFY goroutine that only stops when its context is cancelled. With `context.Background()`, this goroutine and its connection leak on shutdown.
- Refactored `initWebServer` to accept a `context.Context` parameter and pass the server's cancellable context to `newEventPublisher`.
- Note: the standalone hub path (non-web mode) at line 236 already used `ctx` correctly.

### 3. DSN parsing for `file://` prefix (medium priority)
- `parseSQLiteSourceDSN` only had a `file:` prefix handler, so `file:///var/lib/hub.db` was trimmed to `///var/lib/hub.db` (triple slash).
- Added a `file://` case before the `file:` case, so `file:///abs` correctly resolves to `/abs`.
- Added three test cases covering `file://`, `file:///`, and `file:///...?query` forms.

## Verification
- `go build ./cmd/...` passes
- `go vet ./cmd/...` passes
- All DSN parsing tests pass (including new cases)
