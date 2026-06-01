# Cloud Logging Circuit Breaker (Issue #70)

**Date:** 2026-05-31
**Author:** dev-issue-70
**Issue:** #70 — Hub crashes when Cloud Logging retries exhaust resources during metadata outage

## Summary

Added a circuit breaker pattern to the Cloud Logging integration so the hub
remains operational when the GCP metadata service (or Cloud Logging API) is
unavailable.

## Changes

### New: `pkg/util/logging/resilient_cloud_handler.go`

- `ResilientCloudHandler` wraps `CloudHandler` with a three-state circuit breaker
  (closed → open → half-open → closed).
- Background goroutine runs periodic flush health checks against the Cloud
  Logging buffer.
- When consecutive failures exceed the threshold (default: 3), the circuit
  opens and `Handle()` silently drops entries from the cloud path. Local
  logging via the `multiHandler` continues unaffected.
- After `OpenDuration` (default: 60s), the circuit transitions to half-open
  and probes with a timeout-guarded flush. On success the circuit closes and
  Cloud Logging resumes automatically.
- Circuit breaker state is shared across derived handlers (`WithAttrs` /
  `WithGroup`) via a `*circuitBreaker` pointer.

### Modified: `pkg/util/logging/cloud_handler.go`

- Added `BufferedByteLimit` (default 8 MiB) to the `gcplog.Logger` to cap
  the internal write buffer and prevent unbounded memory growth.
- Added `ClientTimeout` (default 15s) to `gcplog.NewClient` creation context
  so startup doesn't hang when metadata is unreachable.

### Modified: `cmd/server_foreground.go`

- `initServerLogging` wraps the `CloudHandler` with `ResilientCloudHandler`.
- Updated type assertions for `Client()` access from `*CloudHandler` to
  `*ResilientCloudHandler`.

## Design Decisions

- **Circuit breaker over retry limiter**: A circuit breaker provides cleaner
  behavior than just capping retries — it stops all Cloud Logging traffic
  during outages rather than letting each log entry independently discover
  the backend is down.
- **Flush-based health detection**: Since `gcplog.Logger.Log()` is async
  and doesn't return errors, we use periodic `Flush()` calls with timeouts
  to detect backend failures.
- **Shared state via pointer**: The `circuitBreaker` struct is heap-allocated
  and shared by pointer, avoiding `go vet` complaints about copying
  `atomic.Int32` in `WithAttrs`/`WithGroup`.

## Testing

- 17 unit tests covering: config defaults, state transitions, Handle behavior
  in each circuit state, failure/success tracking, WithAttrs/WithGroup
  state sharing, concurrent access safety (race detector).
- All existing logging tests continue to pass.
