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

## Review round 1 fixes

Resolved every Required finding from the review of `7808067c08cbe1a1e53f93c8f4530771e184f366`:

- Credential metadata now enforces the existing server-derived credential-decoration contract at the audit boundary: 8 labels, 32-byte keys, 64-byte values, the canonical key/value character rules, reserved attribution keys, control/format rejection, and case-insensitive `scion_pat_`/`Bearer ` canaries. Failures use `CredentialValidationError` and never echo rejected names, keys, or values.
- Each catalog entry now declares the complete payload leaf type, requiredness, byte/item bounds, exact-format length, and closed-enum values. Payload validation dispatches only on those declarations; the former global field-name schema switch is gone, and the catalog snapshot covers every constraint.
- Validation rejects the nil UUID for both `event_id` and `causation_id`.
- `Render` materializes payload leaves once and validates and serializes that same snapshot. A stateful regression payload proves a changed second result cannot cross the boundary.
- The capture sink regression runs bounded concurrent emitters and readers under the race detector, checks the exact final count and record content, and proves returned byte slices are defensive copies.

Focused evidence after the fixes:

- `go test -p 2 ./pkg/hub/auditevent` — PASS.
- `go test -race -p 2 ./pkg/hub/auditevent` — PASS.
- `go vet ./pkg/hub/auditevent` — PASS.
- `go test -count=1 -cover -p 2 ./pkg/hub/auditevent` — PASS, 84.9% statement coverage.
- `test -z "$(gofmt -l pkg/hub/auditevent/*.go)"` — PASS.
- `make fmt-check` — PASS.
- `GOGC=40 golangci-lint run --concurrency=1 ./pkg/hub/auditevent/...` — PASS (`0 issues`).
- `git diff --check` — PASS.
- `GOFLAGS='-p=2' make ci` — PARTIAL / environment-limited. Formatting, no-SQLite vet, and all custom checks passed; the package under change passed. `test-fast` then reproduced unrelated ambient failures in `cmd` (missing Hub authorization), `pkg/config` (leaked `SCION_PROJECT`), and `pkg/harness` (`CODEX_HOME` policy conflict), so the dependent build target was not reached.
- `GOFLAGS='-p=2' make build` — PASS (`./build/scion`).

## #2404 / #2405 interface facts

- `BuildAccessBoundaryCreate` returns one envelope whose `EventID`, `OccurredAt`, correlation, resource, identity, and typed `AccessBoundaryPayload` can feed the purpose-specific history row. The same envelope should be dispatched to the structured sink only after the shared mutation/history transaction commits.
- A rollback or history-write failure must discard the built envelope rather than emit it as a committed event.
- The public `EnvelopeV1` fields and concrete `AccessBoundaryPayload` expose the approved history values without requiring store code to parse rendered JSON or depend on a generic map.
- `Render` and every `Sink.Emit` path validate against the catalog before serialization; #2404 should not duplicate this schema validation in the store layer.
- `DestinationHistory` is a catalog declaration, not persistence. This slice intentionally adds no store dependency or generic event store.
- #2405 can map purpose-specific history rows to its response model directly; it does not need to deserialize the structured-log envelope.

## Review round 2 remaining fixes

Resolved Required findings 2, 3, and 4 from the full review of `628e3fba510ccb56c23dd3d67d4199a931a1acea`, while preserving the approved shared credential-metadata contract at `0a5ffe7eb0d4cd9e10d2362623b1686f71565031`:

- All audit-envelope validation failures now use `ValidationError`, whose only structured fields are the stable field identifier and rule. Family, action, phase, outcome, severity, request, identity, resource, and payload failures no longer interpolate rejected values. The canonical credential contract continues to return its own typed, value-free `credentialmeta.ValidationError`. Canary coverage checks both rendered error text and structured fields for every rejected string-bearing source.
- `Validate` and `Render` now materialize one renderer-owned snapshot. Envelope pointers, credential labels, the payload leaf map, and supported nested payload slices/maps/pointers are cloned before validation; the exact validated snapshot is marshaled. Retained-map and caller-alias regressions prove later mutations cannot change the encoded record, and targeted race coverage exercises concurrent mutation of original builder inputs and retained payload backing data after snapshot capture.
- Phase/outcome coverage now checks the Cartesian product of all six phases, absent outcome, all six declared outcomes, and representative unknown values against the exact common matrix. Every catalog entry is checked across the same product so only literal `AllowedPairs` validate, with explicit assertions that the current non-diagnostic action rejects both observation outcomes.

Targeted evidence after the fixes:

- `go test -count=1 -p 2 ./pkg/hub/auditevent` — PASS.
- `go test -count=1 -race -p 2 ./pkg/hub/auditevent` — PASS.
- `go vet ./pkg/hub/auditevent` — PASS.
- `go test -count=1 -cover -p 2 ./pkg/hub/auditevent` — PASS, 88.7% statement coverage.
- `GOGC=40 golangci-lint run --new-from-rev=HEAD --concurrency=1 ./pkg/hub/auditevent/...` — PASS (`0 issues`).
- `test -z "$(gofmt -l pkg/hub/auditevent/*.go)"` — PASS.
- `git diff --check` — PASS.
- Full `make ci` and `make ci-full` were intentionally not run under the campaign broker workload rule; it permits only targeted package checks with `-p 2` and scoped linting with concurrency 1.
