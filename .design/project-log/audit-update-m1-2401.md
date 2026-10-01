# Audit update milestone 1: #2401 envelope foundation

## Scope

Added the first approved #2378 audit-contract slice in `pkg/hub/auditevent` without changing existing audit emitters, store schemas, persistence methods, endpoints, or UI code.

## Behavior

- Defines the version 1 `scion.audit` envelope, bounded identity/request/credential/resource references, and all six phases with their exact outcome matrix.
- Adds operation contexts that either validate a trusted correlation ID or create one UUID at operation ingress. Event builders require this context and never synthesize a correlation ID.
- Adds the literal milestone-1 catalog entry for `access_boundary/create`, including its `commit/succeeded` pair, `access_constraint` resource, exact payload leaves, and structured-log plus history destinations.
- Validates common bounds, UUIDs, UTC timestamps, identity kinds, severity, resource kind, catalog phase/outcome, payload leaf allowlists, payload types, and boundary-specific enums/cardinality.
- Renders stable JSON using explicit serialized fields. Unknown optional identities are omitted, and arbitrary payload maps cannot cross the public boundary.
- Provides a concurrency-safe capture sink that stores the exact validated JSON record.
- Provides a typed `BuildAccessBoundaryCreate` builder with the approved identity/resource and revision, classification, preview, draft hash, impact-count, and changed-field leaves.

## Files

- `pkg/hub/auditevent/types.go`: envelope and typed payload contracts.
- `pkg/hub/auditevent/context.go`: operation-context lifecycle.
- `pkg/hub/auditevent/catalog.go`: literal action schema.
- `pkg/hub/auditevent/validate.go`: common and catalog validation.
- `pkg/hub/auditevent/render.go`: stable allowlisted JSON rendering.
- `pkg/hub/auditevent/sink.go`: sink contract and capture sink.
- `pkg/hub/auditevent/builder.go`: typed boundary-create builder.
- `pkg/hub/auditevent/auditevent_test.go`: focused contract coverage.

## Verification

- `go test -p 2 ./pkg/hub/auditevent`
- `go vet ./pkg/hub/auditevent`

Both passed before the initial durable push.

## #2404 / #2405 interface facts

- `BuildAccessBoundaryCreate` returns one envelope whose `EventID`, `OccurredAt`, correlation, resource, identity, and typed `AccessBoundaryPayload` can feed the purpose-specific history row. The same envelope should be dispatched to the structured sink only after the shared mutation/history transaction commits.
- A rollback or history-write failure must discard the built envelope rather than emit it as a committed event.
- The public `EnvelopeV1` fields and concrete `AccessBoundaryPayload` expose the approved history values without requiring store code to parse rendered JSON or depend on a generic map.
- `Render` and every `Sink.Emit` path validate against the catalog before serialization; #2404 should not duplicate this schema validation in the store layer.
- `DestinationHistory` is a catalog declaration, not persistence. This slice intentionally adds no store dependency or generic event store.
- #2405 can map purpose-specific history rows to its response model directly; it does not need to deserialize the structured-log envelope.
