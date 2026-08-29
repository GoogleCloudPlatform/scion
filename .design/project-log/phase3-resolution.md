# Phase 3: ResolveConversation Service & DriftState Logic

**Date:** 2026-08-27
**Branch:** dev-resolution (based on dev-store)
**Author:** dev-resolution agent

## Summary

Implemented the conversation resolution layer (`pkg/messaging/`) as Phase 3 of
the messaging refactor. This is pure logic — no HTTP handlers, no CLI wiring.

## Deliverables

| File | Purpose |
|------|---------|
| `pkg/messaging/resolve.go` | Reference parser and `Resolve()` function |
| `pkg/messaging/resolve_test.go` | 26 tests covering all resolution paths |
| `pkg/messaging/normalize.go` | `NormalizeAgentRef` shared helper |
| `pkg/messaging/normalize_test.go` | 8 tests for normalization |
| `pkg/messaging/drift.go` | `TransitionDriftState` pure function |
| `pkg/messaging/drift_test.go` | 11 tests for drift state machine |

## Key Design Decisions

### Reference Grammar (§2.6)

Four forms accepted: `conv:<uuid>`, `@<agent-slug>`, `@<email>`, `#<thread-name>`.
The parser rejects `#<space>/<thread>` per AC-31.

### Project Isolation (AC-30)

For `conv:<id>` references pointing to another project:
- Sender belongs to that project → `boundary-violation` error
- Sender does NOT belong → `not-found` error (identical to genuinely missing)
- Conversations with nil ProjectID (global DMs) → always allowed

Project membership is determined by checking if the sender participates in any
conversation within the target project.

### Resolve-or-Create

`@<agent-slug>` and `@<email>` use resolve-or-create: they find an existing
direct conversation or create a new one with both parties as participants.
`@<email>` creates global DMs (nil ProjectID).

### ResolutionStore Interface

A `ResolutionStore` interface defines the minimal subset of store methods needed,
decoupling the resolution package from the full `store.Store`.

## NormalizeAgentRef Import Path

```
github.com/GoogleCloudPlatform/scion/pkg/messaging.NormalizeAgentRef
```

This function is the single source of truth for agent slug/UUID resolution.
Phase 4 (backfill) should import it from this path.

## Verification

- `go build ./pkg/messaging/...` — clean
- `go test ./pkg/messaging/... -v` — 45 tests pass
- `go build ./...` — full project build clean
