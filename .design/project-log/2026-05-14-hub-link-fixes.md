# Hub Telegram Link Fixes (C3, I3, I4)

**Date:** 2026-05-14
**File:** `pkg/hub/telegram_link.go`

## Changes

### C3: ConsumePending Never Called (Critical)

**Problem:** After `VerifyCode()` marks an entry as "confirmed", `ConsumePending()` existed but was never called. Confirmed entries persisted in the pending map until the 15-minute TTL cleanup loop removed them, meaning the status endpoint could return "confirmed" multiple times.

**Fix:** Added a `ConsumePending(telegramUserID)` call in `handleTelegramLinkStatus` immediately after `writeJSON`, gated on `status == "confirmed"`. This ensures the Telegram plugin receives the confirmation exactly once, then the entry is cleaned up.

### I3: No Rate Limiting on /verify Endpoint (Important)

**Problem:** The `/api/v1/telegram/link/verify` endpoint had no rate limiting. With a 6-char code from a 30-char alphabet (~729M combinations) and a 15-minute TTL, automated brute-force attacks were feasible.

**Fix:** Added an in-memory IP-based rate limiter to `TelegramLinkService`, following the same token-bucket pattern used by `GCPTokenRateLimiter` in `gcp_ratelimit.go`. The limiter allows 5 attempts per minute per IP (burst of 5). In `handleTelegramLinkVerify`, the client IP is extracted via `net.SplitHostPort(r.RemoteAddr)` and checked against the limiter before processing the request. Rate-limited requests receive HTTP 429. Stale limiter entries (>30 min idle) are cleaned up in the existing cleanup loop.

### I4: Close() Panics on Double Close (Important)

**Problem:** `Close()` called `close(s.done)` directly, which panics if called twice.

**Fix:** Added a `closeOnce sync.Once` field to `TelegramLinkService` and wrapped the channel close in `s.closeOnce.Do(func() { close(s.done) })`.

## Verification

- `go build ./pkg/hub/...` passes cleanly.
