# Audit update milestone 1: shared auditevent prerequisites

## Scope

This slice resolves the two shared-interface blockers recorded by the #2404
store checkpoint. It changes only `pkg/hub/auditevent`, its focused tests, and
this project log. Governance mutation paths, history persistence, API/query/UI,
logging configuration, handlers, queues, and cloud transports remain unchanged.

## Production slog sink contract

`NewSlogSink` requires an injected non-nil `*slog.Logger` and implements the
existing `auditevent.Sink` interface. `Emit` materializes one renderer-owned
snapshot, validates it once, and presents every stable rendered envelope v1
top-level field as a structured `slog.Attr`. The record message is the stable
event name `scion.audit`, its time is the envelope `occurred_at`, and its slog
level follows the validated audit severity.

The sink dispatches directly through the injected logger's configured handler.
This preserves existing logger `With` attributes/groups and handler topology
while allowing the sink to return a synchronous `Handler.Handle` error, which
the convenience `slog.Logger` logging methods otherwise discard. A returned
nil error means only that synchronous handler dispatch returned nil. It does
not claim cloud ingestion, flush completion, exactly-once delivery, or durable
acknowledgement.

Focused capture-handler tests prove exact structured equivalence with
`Render`, a single payload materialization, defensive ownership of the emitted
snapshot, value-free renderer errors, synchronous handler-error propagation,
and nil-logger rejection.

## Scope-aware access-boundary resource contract

`ResourceRef.Scope` is a validation-only discriminator and does not add a new
serialized envelope field. The literal `access_boundary/create` catalog entry
owns the complete scope/project-ID matrix through `ResourceScopes`:

- `system`: `resource.project_id` must be absent and is omitted from rendered
  output and downstream history inputs.
- `project`: `resource.project_id` is required, bounded by the existing ID
  rule, and rendered.

Missing/unknown scopes, a project ID on system scope, and a missing project ID
on project scope fail closed with the existing typed, value-free
`ValidationError`. The typed builder now requires callers to provide the
constraint scope explicitly. The catalog snapshot and exhaustive focused
scope matrix prevent requiredness from drifting into a second rule.

## Retained-author handoff

The retained #2404 author can now map the committed constraint scope into
`AccessBoundaryCreateInput.Scope`, build one envelope inside the shared
mutation/history transaction, map that same envelope into the purpose-specific
history row, discard it on rollback, and call `SlogSink.Emit` only after
`Store.WithTx` returns successfully. This slice does not perform that wiring.

## Verification

At implementation commit `47b24367fa860cda6d01a76a38ce16173b0fcacc`:

- `go test -count=1 -p 2 ./pkg/hub/auditevent` — PASS.
- `go test -count=1 -race -p 2 ./pkg/hub/auditevent` — PASS.
- `go vet -p 2 ./pkg/hub/auditevent` — PASS.
- `GOGC=40 golangci-lint run --new-from-rev=fc6954bc94bfa39cdbc9ae905e49dc5da5bb699c --concurrency=1 ./pkg/hub/auditevent/...` — PASS (`0 issues`).
- `test -z "$(gofmt -l pkg/hub/auditevent/*.go)"` — PASS.
- `git diff --check` — PASS.

Local `make ci` and `make ci-full` were not run because the campaign broker
workload rule prohibits them. No SDK queue/drop metrics or cloud delivery
behavior were tested because those facts are unavailable at this layer.

## Residual risks

- Governance integration is intentionally retained for its assigned author;
  until wired, the new production sink has no mutation-path caller.
- Handler-specific asynchronous loss, filtering, circuit-open behavior, flush
  failure, and backend retention remain properties of the existing logging
  topology and are not strengthened by this sink.
