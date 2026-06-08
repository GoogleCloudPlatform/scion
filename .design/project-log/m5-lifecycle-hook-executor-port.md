# M5: Lifecycle Hook Executor Port

**Date:** 2026-06-08
**Agent:** lh-port-m5
**Branch:** scion/lifecycle-hooks-port
**Commit:** a06b767c

## Summary

Ported the HTTPExecutor (lifecycle hook action executor) from the architect reference branch and wired it into the evaluator, replacing the M4 LoggingExecutor stub.

## Files Changed

- `pkg/hub/lifecycle_hook_executor.go` — NEW: HTTPExecutor implementation
- `pkg/hub/lifecycle_hook_executor_test.go` — NEW: 24 executor test cases
- `pkg/hub/server.go` — Wired NewHTTPExecutor into StartLifecycleHookEvaluator

## Key Behaviors Preserved

1. **Identity resolution:** GetGCPServiceAccount → verified email → GenerateAccessToken with `cloud-platform` scope
2. **SSRF-safe client:** DNS resolve → block loopback (127/8, ::1) + link-local (169.254/16, fe80::/10) + link-local multicast; ALLOW RFC1918 (10/8, 172.16/12, 192.168/16); dial validated IP directly (anti DNS-rebinding); block all redirects
3. **Token attachment:** Bearer token ONLY for action.Type=="http" over HTTPS; never for webhooks
4. **Retry/timeout:** per-action timeout via context deadline; on_error="retry" → max 3 attempts with exponential backoff (500ms, 1s); 4xx is non-retryable (early exit); default timeout = 10s
5. **Audit:** records status code, latency, error class ONLY; NEVER persists response bodies, rendered auth headers, or secret body fields; logs host-only (not full URL path)

## Deviations from Reference

- **Test store creation:** Changed from `sqlite.New(":memory:")` to `newTestStore(":memory:")` to match the current codebase's ent-based test store pattern (via `teststore_test.go`)
- No other deviations; the executor code is a faithful port of the reference

## Test Results

All 24 tests pass, including with `-race`:
- Success/failure paths: 2xx, 4xx, 5xx, timeout
- Retry: backoff, exhaustion, 4xx non-retryable, ctx-cancel-during-backoff
- Security: SSRF loopback blocked, RFC1918 allowed, redirect blocked, no-body-in-audit, no-auth-in-audit, webhook-no-auth, http-requires-identity
- SSRF dialer: validated-IP dial, all-blocked-refused, mixed-IPs-first-allowed
- Template rendering: trust class verification, untrusted var encoding

## Wiring

The HA-dedup block (`allOpts`/`deduperDriverForPublisher`) in `StartLifecycleHookEvaluator` was preserved unchanged. Only the executor argument was swapped from `nil` to `NewHTTPExecutor(s.store, s.gcpTokenGenerator, s.auditLogger, ...)`.
