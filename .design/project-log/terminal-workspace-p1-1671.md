# Terminal workspace P1: SSE subscription ordering (#1671)

Base: `3a7b0393044bfdd0437ff2f2bc80243487bbd03e`.

The SSE handler previously flushed response headers before subscribing to its
subjects. A browser's EventSource open callback could start a metadata snapshot
while events still had no subscriber, leaving a lost-update interval.

Move subscription creation and deferred cleanup before the initial flush, after
all existing validation and subject authorization. Preserve headers, wire format,
heartbeat, connection max age, cancellation, and authorization policy. The
agent-subject authorization issue (#1672) remains a separate change.

`pkg/hub/sse_ordering_test.go` publishes one event synchronously at the first
flush boundary. `testing/synctest.Wait` deterministically lets the handler drain
its work before the test checks the exact update frame. The original handler
fails with an empty body; the repaired handler delivers the event. No polling,
retry publication, timing sleeps, live Hub, or active agents are used.

Additional tests verify subscriber cleanup on cancellation, publisher closure,
write failure accompanied by cancellation, and a first-flush abort panic, plus
rejection of invalid or unauthorized requests without flushing or retaining a
subscription. Write errors alone are still ignored by the handler; the test
models cancellation accompanying a disconnected client, not new error handling.

Verification: the regression failed on the original production handler, then
`go test ./pkg/hub -run SSE -count=1` and the corresponding `-race` run passed.
`make ci` passed (format, vet, custom checks, repository no-SQLite tests, build).
Scoped `golangci-lint` against the pinned base passed with zero issues; focused
`gofmt` and `git diff --check` passed. No baseline failures were observed.
Every inherited `SCION_*` variable was removed from test subprocesses. Checks
used `GOFLAGS=-buildvcs=false` where applicable. Cumulative `make ci-full` is
reserved for the phase manager/writer and was not run for this backend leaf.

Detailed results are recorded in the developer handoff report at
`/scion-volumes/scratchpad/projects/terminal-workspace/reports/p1-1671-developer.md`.
The shared handoff bundle and its manifest preserve the commit for independent
review and sole-writer integration. No remote push or integration is performed
by this developer, as required by the phase brief.
