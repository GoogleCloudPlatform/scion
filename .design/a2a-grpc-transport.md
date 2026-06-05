# A2A Bridge: gRPC Transport

**Status:** Implementing
**Created:** 2026-06-05
**Related:** [a2a-bridge-design.md](./a2a-bridge-design.md), [a2a-multi-turn-lifecycle.md](./a2a-multi-turn-lifecycle.md)

---

## 1. Problem

The A2A bridge currently only supports JSON-RPC 2.0 over HTTP. The A2A protocol
specification defines three transport bindings: JSON-RPC, gRPC, and HTTP+JSON/REST.
The ITK compatibility tests exercise gRPC transport in their multi-hop traversal.
Without gRPC, the bridge cannot participate in cross-SDK interop scenarios that
use gRPC transport.

## 2. Design

### Approach: gRPC adapter over existing Bridge logic

The Bridge struct already implements all A2A operations (SendMessage, GetTask,
ListTasks, CancelTask, push notifications, streaming). The gRPC server is a thin
adapter that:

1. Receives gRPC requests
2. Translates protobuf messages to the bridge's internal types
3. Delegates to the same Bridge methods the JSON-RPC server uses
4. Translates responses back to protobuf

This avoids duplicating any business logic.

### Proto compilation

Use the official A2A proto from `a2aproject/A2A/specification/a2a.proto` as the
source. Compile with `protoc` + `protoc-gen-go` + `protoc-gen-go-grpc` to generate
Go code. Generated code goes in `internal/a2apb/`.

The proto has dependencies on `google/api/annotations.proto` etc. — use buf or
vendored google API protos.

### Server structure

```
extras/scion-a2a-bridge/
  internal/
    a2apb/           # generated protobuf Go code
    bridge/
      grpc_server.go # gRPC adapter implementing A2AServiceServer
```

### gRPC service methods → Bridge method mapping

| gRPC RPC | Bridge method |
|---|---|
| SendMessage | Bridge.SendMessage (blocking or non-blocking based on return_immediately) |
| SendStreamingMessage | Bridge.SendStreamingMessage |
| GetTask | Bridge.GetTask |
| ListTasks | Bridge.ListTasks |
| CancelTask | Bridge.CancelTask |
| SubscribeToTask | Bridge.SubscribeToTask |
| CreateTaskPushNotificationConfig | Bridge.SetPushNotificationConfig |
| GetTaskPushNotificationConfig | Bridge.GetPushNotificationConfig |
| ListTaskPushNotificationConfigs | Bridge.GetPushNotificationConfig |
| DeleteTaskPushNotificationConfig | Bridge.DeletePushNotificationConfig |
| GetExtendedAgentCard | Bridge.GenerateAgentCard |

### Configuration

Add to config:
```yaml
bridge:
  grpc_listen_address: ":9443"  # separate port for gRPC
```

### Startup

The main() function starts both HTTP and gRPC servers. gRPC is optional — only
started if grpc_listen_address is configured.

## 3. Scope

- In: gRPC server adapter, proto compilation, config, startup wiring
- Out: gRPC client (for making outbound A2A calls), TLS on gRPC (use reverse proxy)

## 4. Testing

- Test: each gRPC RPC maps correctly to Bridge method
- Test: protobuf ↔ internal type translation
- Test: streaming RPCs deliver events correctly
- Test: gRPC server starts and accepts connections
