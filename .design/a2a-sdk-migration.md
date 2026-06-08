# A2A Go SDK Migration

## Status: In Progress
## Date: 2026-06-08

## Summary

Migrate the scion-a2a-bridge from a hand-rolled A2A protocol implementation to
the official `a2a-go` SDK (`github.com/a2aproject/a2a-go/v2`). This replaces
our custom JSON-RPC handling, task lifecycle management, and streaming
infrastructure with the SDK's spec-compliant implementations while preserving
our Scion Hub routing core.

## Motivation

- **Spec compliance**: The SDK tracks the A2A spec automatically. Our hand-rolled
  implementation required manual updates for each spec revision.
- **Reduced maintenance**: ~500 lines of JSON-RPC, SSE streaming, and task store
  code replaced by SDK.
- **Multi-transport**: SDK provides JSON-RPC, REST, and gRPC transports from a
  single `RequestHandler` — we get gRPC and REST nearly for free.
- **Correctness**: SDK handles edge cases (OCC, concurrent cancellation, event
  ordering) that our MVP implementation simplified or deferred.

## Architecture

### Before (hand-rolled)

```
HTTP Request → server.go (JSON-RPC dispatch) → bridge.go (task management)
    → Hub API → Broker → bridge.go (response correlation) → JSON-RPC response
```

### After (SDK-based)

```
HTTP Request → auth middleware → route extraction → SDK JSONRPC Handler
    → SDK RequestHandler → SDK task lifecycle → ScionExecutor.Execute()
    → bridge.go (Hub routing) → Broker → waiter channel → SDK events
    → SDK response serialization → HTTP response
```

### Key Components

**ScionExecutor** (`executor.go`): Implements `a2asrv.AgentExecutor`. The bridge
between the SDK's event-driven model and our Scion Hub message routing.

- `Execute()`: Translates SDK message → Scion StructuredMessage, sends to Hub,
  waits for broker response, yields SDK events.
- `Cancel()`: Sends interrupt to Scion agent, yields canceled status event.

**Server** (`server.go`): Simplified HTTP routing layer. Handles:
- Multi-project/agent URL routing (`/projects/{p}/agents/{a}/jsonrpc`)
- Agent card serving (kept custom — SDK's card handler is single-agent)
- Auth middleware, rate limiting, metrics (unchanged)
- Delegates JSON-RPC to SDK's `NewJSONRPCHandler`

**Bridge** (`bridge.go`): Core Hub routing preserved. Changes:
- Added `sdkRequestHandler` field for multi-transport access
- Task lifecycle now managed by SDK's in-memory task store
- SQLite store retained for context mapping and broker correlation

**Translate** (`translate.go`): Added SDK-compatible translation functions:
- `TranslateA2APartsToScion()`: SDK `a2a.ContentParts` → Scion message
- `TranslateScionToA2AParts()`: Scion message → SDK `a2a.Message` + `a2a.Artifact`
- `MapActivityToSDKTaskState()`: Scion activity → SDK `a2a.TaskState`
- Original functions retained for backward compatibility

## What Changed

| Component | Before | After |
|-----------|--------|-------|
| JSON-RPC parsing | `server.go` hand-rolled | SDK `a2asrv.NewJSONRPCHandler` |
| Task lifecycle | `bridge.go` + SQLite | SDK in-memory task store |
| SSE streaming | `stream.go` custom | SDK built-in |
| Push notifications | `push.go` custom | SDK `push.Sender` (future) |
| A2A types | `translate.go` custom structs | SDK `a2a` package |
| Error codes | Custom constants | SDK `a2a.Err*` sentinel errors |

## What's Preserved

- **Bridge core**: Hub client routing, broker plugin, agent lookup, context
  resolution, auto-provisioning — all unchanged.
- **Config**: Same YAML format, same fields.
- **Auth**: Same API key / Bearer middleware.
- **Metrics**: Same Prometheus metrics.
- **Rate limiting**: Same per-IP/key token bucket.
- **Broker plugin**: Same go-plugin RPC server.
- **SQLite store**: Retained for context mapping. Task state now also in SDK
  in-memory store.

## PR Structure

### PR A: SDK Adoption (`a2a/sdk-migration`)
- Add `a2a-go/v2` dependency
- New `executor.go` (AgentExecutor implementation)
- Rewritten `server.go` (SDK handler delegation)
- Updated `translate.go` (SDK type translations)
- Updated `bridge.go` (sdkRequestHandler field)
- Updated `main.go` (SDK wiring)
- Updated tests

### PR B: gRPC + REST Transports (`a2a/sdk-grpc-rest`)
- `a2agrpc.NewHandler` for gRPC transport
- `a2asrv.NewRESTHandler` for REST transport
- Config fields: `grpc_listen_address`, `rest_listen_address`
- Startup wiring in `main.go`

## Migration Risks

1. **Task store divergence**: SDK uses in-memory store; our SQLite store tracks
   context mappings separately. Tasks visible via A2A protocol come from SDK
   store; context lookups use SQLite.

2. **Broker correlation**: The SDK doesn't know about our broker. Response
   correlation happens inside `ScionExecutor.Execute()` using the same waiter
   channel pattern.

3. **Push notification gap**: SDK has `push.Sender` interface but we haven't
   wired our SSRF-safe push dispatcher yet. Push is disabled in capabilities.

## Future Work

- Wire SDK push notification support with our SSRF-safe dispatcher
- Implement SDK `taskstore.Store` interface backed by SQLite for persistence
- Add multi-turn conversation support (SDK handles it; our executor needs updates)
- Evaluate SDK's work queue for distributed deployment
