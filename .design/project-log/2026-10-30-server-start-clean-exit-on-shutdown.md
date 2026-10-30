# server start: a stop during startup exits 0 (hub-update-channels N1)

**Date:** 2026-10-30
**Branch:** scion/hu-p3-n1-go (from scion/hu-p3-mf1 @ d078661f3)

A `systemctl stop` or restart during the first boot's migrations made
`scion server start` exit 1 ("failed to run migrations: context
canceled"). systemd then marked the unit failed, `OnFailure=` ran the
boot-failure guard, and the guard could swap a healthy release back (VM2
evidence T4, note N1).

## Change
- `cmd/server_startup_shutdown.go`: `startupShutdownErr(err,
  shutdownBegan, ctxErr)` returns nil (with one info line, "shutdown
  requested during startup; exiting cleanly") only when the shutdown
  sequence has begun, the server context's `Err()` is `context.Canceled`,
  and err wraps `context.Canceled` but not `context.DeadlineExceeded`.
  Everything else is returned unchanged, so it still exits non-zero.
  `(*serverShutdownSequence).startupErr(ctx, err)` feeds it the sequence's
  own begun flag (`startedAt`), set by the signal handler's `claim`.
- `cmd/server_foreground.go` `runServerStart` uses it for every startup
  step after the signals are armed that runs under the server context and
  can fail:
  `initStore` (migrations), `resolveHubEndpoints`, `initHubServerFn`
  (returns before the "failed to start" log), `newCommandBus`,
  `startConduit`, `newEventPublisher`, `initWebServer`, and
  `startRuntimeBroker`.
  Steps that take no context, or that cannot fail (`managedImageRegistryConfigured`,
  `initPluginManagerFn`, a secret backend error which is only logged), are
  unchanged.

## Tests
`cmd/server_startup_shutdown_test.go`: `TestStartupShutdownErr` (shutdown
plus Canceled → nil; Canceled with no shutdown → exit 1; another error after
the shutdown → exit 1; DeadlineExceeded, with or without a shutdown → exit
1; a still-live context; Canceled joined with DeadlineExceeded) and
`TestServerShutdownSequence_StartupErr` (a real sequence before and after
`begin(SIGTERM)`).
