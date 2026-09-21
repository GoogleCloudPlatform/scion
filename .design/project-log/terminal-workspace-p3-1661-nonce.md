# P3-1661: Nonce-Based Container-Side Cleanup

**Date:** 2026-09-21
**Branch:** `scion/dev-p3-1661-nonce`
**Base:** `ce7ff971` (integration HEAD with all QA follow-ups)
**Bundle:** `transfers/p3-1661-nonce-ba9fc681.bundle`

## Summary

Added per-attach nonce injection and container-side PID cleanup for Docker exec
attach sessions. When a browser tab closes, the broker can now identify and
SIGTERM the exact tmux client process inside the container, rather than relying
solely on host-side PTY close propagation.

## Design Decisions

- **Nonce generation:** 16 random bytes → 32-char hex string via `crypto/rand`.
  Injected as `-e SCION_ATTACH_NONCE=<nonce>` in docker exec args.
- **PID lookup:** Shell script scans `/proc/*/environ` for the nonce, filters by
  `*tmux*attach*` cmdline. Returns empty on multiple matches (no-signal-on-ambiguity).
- **TOCTOU mitigation:** Read `/proc/<pid>/stat` start-time (field 22, safely
  parsed via `${stat##*) }` to handle comm fields with spaces like "tmux: client"),
  then verify start-time matches before sending `kill -TERM` in a single shell script.
- **Docker-only guard:** `isDockerCompatibleRuntime()` returns true only for
  `"docker"`, `"container"`, or `""`. K8s and CloudRun paths unchanged.
- **Graceful fallback:** All failures (nonce empty, PID not found, start-time
  mismatch, docker exec errors) fall back silently to existing host-side cleanup.
- **Fresh context:** Container cleanup uses `context.WithTimeout(context.Background(), 5s)`
  since the session context is already canceled at cleanup time.
- **pidfd_open rejected:** Cannot be used reliably via docker exec shell scripts.

## Files Changed

- `pkg/runtimebroker/pty_handlers.go` — Production code (+204 lines):
  - `isDockerCompatibleRuntime()`, `generateAttachNonce()`
  - `cleanupContainerAttach()`, `findContainerPIDByNonce()`,
    `readContainerPIDStartTime()`, `killContainerPID()`
  - `gracefulShutdownExec` signature extended with runtime/container/user/nonce params
  - `LocalPTYSession` and `StreamPTYHandler` nonce field + injection in `startDockerExec()`
- `pkg/runtimebroker/pty_cleanup_test.go` — Tests (+698 lines):
  - 12 new test functions covering unit, integration, topology, and lifecycle
  - Shell adapter with "docker" symlink for PATH-based integration testing
  - Helper functions: `clearSCIONEnv`, `filterSCIONEnv`, `nonceTestAdapter`

## Verification

All gates run with `-race -count=1`:
- `go vet ./pkg/runtimebroker/...` — clean
- `go test -race ./pkg/runtimebroker/... -run TestPTYCleanup` — 21/21 pass
- `go test -race ./pkg/runtimebroker/...` — all pass except 3 pre-existing
  `TestEnvGather_Secret*` failures (confirmed on base commit)
- `go test -race -tags no_sqlite ./pkg/hub/... -run TestPTYCleanup` — 5/5 pass
- Zero data races detected
