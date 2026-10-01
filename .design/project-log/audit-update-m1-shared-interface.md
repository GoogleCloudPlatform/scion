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

## Review round 1 fixes

Resolved the required cross-handler schema finding and explicitly addressed
the optional source-location finding from the review of
`7cf04310333361fe6357dcd3a0a874ce9e06eebe`.

### Handler-portable nested schema

The sink no longer passes request, identity, credential, resource, payload, or
`impact_counts` structs through `slog.Any`. Every nested object is constructed
as an explicit `slog.GroupValue` using the exact rendered v1 field names and
scalar types. Optional leaves are appended only when the renderer would include
them. Credential labels are a string-valued group, `impact_counts` is a
uint-valued group, and `changed_fields` uses the standard `[]string` list value
that the configured OTel bridge converts to an OTel string slice. The
validation-only `ResourceRef.Scope` is never included in the resource group.

The sink still creates one immutable render snapshot and validates it once.
All slog attributes are derived from that owned snapshot; caller aliases are
not read again. Unsupported future payload value types fail closed rather than
falling back to a struct-valued `KindAny` representation.

New regressions inspect the raw `slog.Record` and require every nested v1
object to be `KindGroup`, with the only `KindAny` leaf restricted to the
supported `[]string` list. A second regression sends a complete event through
the repository's real `logging.NewOTelHandler` path and reconstructs the OTel
attribute maps/slices. Both raw and OTel objects are exactly JSON-equivalent to
`Render`, retain scalar types and nesting, and exclude `Scope`.

### Source PC

Direct handler dispatch remains necessary to surface synchronous
`Handler.Handle` errors. The sink now captures the caller of `SlogSink.Emit`
with `runtime.Callers` and supplies that PC to `slog.NewRecord`. This makes the
event's integration call site available to existing source-aware GCP handlers,
matching normal logger behavior instead of silently omitting source metadata.
A focused test resolves the PC and pins the caller file/function rather than a
sink-internal frame. OTel's configured handler currently drops PC unless its
own source option is enabled; that existing handler policy is unchanged.

### Verification after fixes

At implementation commit `4c9ce00636dc078bfbbee7d9b05fc50870b25c67`:

- `go test -count=1 -p 2 ./pkg/hub/auditevent` — PASS.
- `go test -count=1 -race -p 2 ./pkg/hub/auditevent` — PASS.
- `go test -count=1 -p 2 ./pkg/util/logging -run 'Test(NewOTelHandler|SetupWithOTel)'` — PASS.
- `go vet -p 2 ./pkg/hub/auditevent` — PASS.
- `GOGC=40 golangci-lint run --new-from-rev=7cf04310333361fe6357dcd3a0a874ce9e06eebe --concurrency=1 ./pkg/hub/auditevent/...` — PASS (`0 issues`).
- `test -z "$(gofmt -l pkg/hub/auditevent/*.go)"` — PASS.
- `git diff --check` — PASS.

Local `make ci` and `make ci-full` remain intentionally unrun under the
campaign broker workload rule. The approved scope/project-ID catalog matrix
is unchanged, and governance integration remains with the retained author.

## Review round 2 fix

Resolved the empty-request parity finding from the review of
`e414f8dc8e5e4470d5201d61698f22a6c5d2ee9f`. A non-nil, all-empty
`RequestRef` is now canonicalized to absence when the immutable render
snapshot is created. Both the validation event and serialized envelope refer
to that canonical value, so `Render`, raw slog records, JSON handlers, and the
repository's real `logging.NewOTelHandler` path all omit `request` rather than
disagreeing between `{}` and absence.

The correction is intentionally confined to the shared snapshot boundary.
Populated request references continue to be defensively cloned and rendered
unchanged. No sibling optional-pointer policy, validation rule, catalog-owned
scope/project-ID matrix, nested slog schema, or source-PC behavior changed.
Governance integration remains outside this slice.

The focused regression first failed against the reviewed checkpoint by
showing non-nil snapshot request pointers and a rendered `"request":{}` while
the raw slog and OTel paths omitted the group. After the correction, it proves
snapshot, Render, raw slog/JSON, and real OTel parity for the all-empty case;
the existing complete-event parity regressions continue to cover populated
request preservation.

### Verification after round 2

At implementation commit `d1b5fd4ff`:

- `go test -count=1 -p 2 ./pkg/hub/auditevent` — PASS.
- `go test -count=1 -race -p 2 ./pkg/hub/auditevent` — PASS.
- `go test -count=1 -p 2 ./pkg/util/logging -run 'Test(NewOTelHandler|SetupWithOTel)'` — PASS.
- `go vet -p 2 ./pkg/hub/auditevent` — PASS.
- `GOGC=40 golangci-lint run --new-from-rev=e414f8dc8e5e4470d5201d61698f22a6c5d2ee9f --concurrency=1 ./pkg/hub/auditevent/...` — PASS (`0 issues`).
- `test -z "$(gofmt -l pkg/hub/auditevent/*.go)"` — PASS.
- `git diff --check` — PASS.

Local `make ci` and `make ci-full` were not run because the campaign broker
workload rule prohibits them.
