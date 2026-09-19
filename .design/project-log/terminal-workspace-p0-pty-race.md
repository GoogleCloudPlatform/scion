# Phase 0 PTY resize/close synchronization

Issue: ptone/scion#1666 (Phase 0 #1640, root #1639).
Date: 2026-09-19.
Integration base: `1bb405704a96c2a03f2a1b2b95719edc5d0cda94`.
Provisional regression dependency: `37c5f793a04f2355931e532d1e6f6ac0f27fd073`.

## Diagnosis and fix

The unchanged lifecycle regression reproduced two data races with
`go test -race ./pkg/hub ./pkg/runtimebroker -run TestPTYLifecycle -count=10 -v`.
During detach and detach-and-close, PTY descriptor destruction overlapped
`handleResize` calling `pty.Setsize`, whose ioctl helper calls `os.File.Fd`.
Destruction occurred both directly in deferred `Close` and when a pending read
released its last descriptor reference. Context cancellation alone did not
establish completion of the resize worker.

`StreamPTYHandler.Run` now cancels and joins the resize worker through a
completion channel before returning into deferred descriptor cleanup. Closing
that channel orders every resize descriptor access before the subsequent PTY
close. Resize behavior, PTY reads/writes, process reaping, and protocol stay the
same. No lock is held over blocking I/O, and neither I/O worker is joined before
the descriptor close that may unblock it.

Startup failures return before creating the resize worker, so they cannot wait
on an unstarted worker. Kubernetes returns through its existing separate path.
Once started, resize exits on context cancellation or stream close; any in-flight
sandbox resize command receives the same canceled context via `CommandContext`.
The join therefore does not depend on receiving another resize event or PTY
output. Existing lifecycle coverage checks detach, immediate stream close,
descriptor closure, process reaping, original pane survival, and reattachment.

## Scope and adjacent observations

Only `pkg/runtimebroker/pty_handlers.go` and this log belong to this fix. The
regression and browser fixtures remain owned by the lifecycle developer; their
provisional dependency is preserved without reauthoring its commits.

`LocalPTYSession.Run` has a similar cancellation/close pattern while its
WebSocket reader performs resize. It is a separate path and was reported to the
manager without modification or a claim of regression proof.

`StreamPTYHandler.Close` also directly closes the descriptor. A repository-wide
Go reference search found only the constructor/method definitions, the local
handler in `handlePTYStreamWithAgent` that invokes `Run`, and the lifecycle test.
The production handler is not stored, returned, or assigned to an interface;
there are no checked-in callers of `Close`. Concurrent external use of that
exported method remains an adjacent API hazard, outside this proved Run cleanup
race. Revisit its ownership contract before introducing a caller.

The regression uses a private disposable tmux socket and a local runtime-exec
adapter. It proves the bridge behavior with a kernel PTY, not Docker,
Kubernetes, Apple container, or Cloud Run sandbox integration.

## Verification and delivery

- Before the fix: unchanged lifecycle regression with `-race -count=10` failed
  with the descriptor race (both direct close and pending-read destruction).
- After the fix: `go test -race ./pkg/hub ./pkg/runtimebroker -run TestPTYLifecycle
  -count=30 -v` passed, including 120 broker attach/resize/close rounds.
- `go test -race ./pkg/runtimebroker -count=1` passed for the complete package.
- Isolated `make ci` passed: formatting, vet, custom checks, the complete
  `no_sqlite` test suite, and build. Its HOME and XDG directories were disposable;
  existing Go caches were retained. Every test invocation stripped inherited
  `SCION_*` variables. `git diff --check` passed.
- Full web/browser CI and real container-runtime integration were not run for
  this Go-only synchronization fix. No required gate was unavailable or failed
  on the fixed candidate. The newer lifecycle cleanup dependency is reserved for
  manager-coordinated combined validation.

Durable report, manifest, and logs are under
`/scion-volumes/scratchpad/projects/terminal-workspace/reports/tw-p0-pty-fix-r1*`;
the thin bundle is under the sibling `transfers/` directory. The task brief
requires verified shared-bundle delivery instead of a remote push. This is a
candidate for fresh independent review, not integration approval.
