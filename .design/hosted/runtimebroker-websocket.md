# Runtime Broker WebSocket Design

> **Status**: ✅ **Implemented** (2026-02-03) - Core control channel and PTY streaming functionality is complete.
>
> **Updated 2026-10-05:** sections 4.1, 4.4, 5.3, 6.2 and 9 now match the code: the CLI attach route is `/api/v1/agents/{id}/pty` on the Hub, login is `scion hub auth login`, the browser ticket flow is not implemented, and close codes and the CLI's no-reconnect behaviour are described. Attach is expected to move to the conduit relay (a new connection layer); see section 9.5.

This document consolidates the WebSocket-related design for communication between the Hub and Runtime Brokers. The WebSocket connection serves two primary purposes:

1. **Control Channel**: Hub-initiated commands to Runtime Brokers (for NAT/firewall traversal)
2. **PTY Streaming**: Bidirectional terminal access from browsers/CLI to agent containers

---

## Table of Contents

1. [Overview](#1-overview)
2. [Architecture](#2-architecture)
3. [Control Channel Protocol](#3-control-channel-protocol)
4. [PTY Streaming](#4-pty-streaming)
5. [Authentication](#5-authentication)
6. [Connection Lifecycle](#6-connection-lifecycle)
7. [Transport Selection](#7-transport-selection)
8. [Future Considerations](#8-future-considerations)

---

## 1. Overview

### 1.1 Problem Statement

Runtime Brokers often run in environments where the Hub cannot directly initiate HTTP connections:
- Developer laptops behind NAT
- On-premise servers with firewall restrictions
- Containers without public endpoints

Additionally, interactive terminal access requires low-latency bidirectional streaming that cannot be achieved with REST APIs alone.

### 1.2 Solution

The Runtime Broker initiates a persistent WebSocket connection to the Hub. This connection:
- **Traverses NAT/firewalls**: Broker-initiated outbound connections typically succeed
- **Enables bidirectional communication**: Hub can send commands; Broker can send events
- **Supports stream multiplexing**: Multiple PTY sessions over a single connection

### 1.3 Design Goals

| Goal | Description |
|------|-------------|
| NAT traversal | Brokers behind NAT/firewalls can receive Hub commands |
| Low latency | Real-time PTY streaming for interactive use |
| Simplicity | Reuse existing REST API logic where possible |
| Resilience | Graceful reconnection with state reconciliation |
| Security | Authenticated connections with TLS |

---

## 2. Architecture

### 2.1 High-Level Flow

```
┌─────────────────┐                    ┌─────────────────┐
│   Scion Hub     │                    │  Runtime Broker   │
│                 │◄───────────────────│  (behind NAT)   │
│                 │   WebSocket        │                 │
│  Control Plane  │   Control Channel  │  Broker Agent   │
│                 │(Broker-initiated)  │                 │
└─────────────────┘                    └─────────────────┘
        │                                      │
        │  HTTP Requests (Tunneled)            │
        │  ◄────────────────────               │
        │  HTTP Responses (Tunneled)           │
        │  ────────────────────►               │
        │                                      │
        │  Multiplexed Streams                 │
        │  ◄────────────────────►              │
        │  (PTY, Logs)                         │
```

### 2.2 Connection Topology

```
                    ┌──────────────────────────┐
                    │       Scion Hub          │
                    │                          │
                    │  ┌────────────────────┐  │
                    │  │ Control Channel    │  │
                    │  │ Manager            │  │
                    │  │                    │  │
Browser/CLI ──WS──► │  │ ┌──────┐ ┌──────┐  │  │ ◄──WS── Runtime Broker A
                    │  │ │Brkr A│ │Brkr B│  │  │
                    │  │ └──────┘ └──────┘  │  │ ◄──WS── Runtime Broker B
                    │  └────────────────────┘  │
                    │                          │
                    │  ┌────────────────────┐  │
                    │  │ Stream Mapper      │  │
                    │  │ (client WS →       │  │
                    │  │  broker stream ID)  │  │
                    │  └────────────────────┘  │
                    └──────────────────────────┘
```

### 2.3 WebSocket Endpoints

| Endpoint | Direction | Purpose |
|----------|-----------|---------|
| `WS /api/v1/runtime-brokers/connect` | Broker → Hub | Control channel (commands, events, streams) |
| `WS /api/v1/agents/{id}/pty` | Client → Hub | PTY access (proxied to broker) |
| `WS /api/v1/agents/{id}/events` | Client → Hub | Agent status stream |
| `WS /api/v1/groves/{id}/events` | Client → Hub | Grove-wide events |

---

## 3. Control Channel Protocol

### 3.1 Connection Establishment

**Endpoint:**
```
WS /api/v1/runtime-brokers/connect
```

**Prerequisites:**
- Broker must have registered at least one grove via REST API
- Broker must have a valid shared secret (from registration flow)

**HMAC Authentication Headers:**
```
X-Scion-Broker-ID: broker-abc123
X-Scion-Timestamp: 2025-01-24T10:00:00Z
X-Scion-Nonce: random-nonce-xyz
X-Scion-Signature: HMAC-SHA256(secret, "{brokerId}:{timestamp}:{nonce}:GET:/api/v1/runtime-brokers/connect")
```

### 3.2 Initial Handshake

**Broker → Hub (connect message):**
```json
{
  "type": "connect",
  "brokerId": "string",
  "version": "1.2.3",
  "groves": [
    {
      "groveId": "string",
      "profiles": ["docker", "k8s-dev"]
    }
  ],
  "capabilities": {
    "webPty": true,
    "sync": true,
    "attach": true
  },
  "supportedHarnesses": ["claude", "gemini"],
  "resources": {
    "cpuAvailable": "4",
    "memoryAvailable": "8Gi"
  }
}
```

**Hub → Broker (connected acknowledgment):**
```json
{
  "type": "connected",
  "brokerId": "string",
  "hubTime": "2025-01-24T10:00:00Z",
  "groves": [
    {
      "groveId": "string",
      "groveName": "string"
    }
  ]
}
```

### 3.3 HTTP Tunneling Protocol

To support a unified API surface and allow the Hub to "dial" the Runtime Broker regardless of network topology, the Control Channel acts as a tunnel for standard HTTP requests. This avoids maintaining a separate "command" schema and allows the Broker to reuse its existing REST API handlers.

**Request Envelope (Hub → Broker):**
```json
{
  "type": "request",
  "requestId": "req-uuid-123",
  "method": "POST",
  "path": "/api/v1/agents",
  "headers": {
    "Content-Type": ["application/json"],
    "X-Trace-ID": ["trace-abc"]
  },
  "body": "base64-encoded-body"
}
```

**Response Envelope (Broker → Hub):**
```json
{
  "type": "response",
  "requestId": "req-uuid-123",
  "statusCode": 201,
  "headers": {
    "Content-Type": ["application/json"]
  },
  "body": "base64-encoded-body"
}
```

### 3.4 Pros/Cons of HTTP Tunneling

| Aspect | HTTP Tunneling | Custom Command Protocol |
|--------|----------------|-------------------------|
| **API Evolution** | **Pro**: Changes to REST API (e.g., adding fields) automatically work over WS. | **Con**: Requires updating command schemas and handlers separately. |
| **Consistency** | **Pro**: Identical behavior for Direct HTTP and WS Tunnel. | **Con**: Subtle behavior differences likely to creep in. |
| **Tooling** | **Pro**: Can use standard HTTP middleware (logging, auth, tracing). | **Con**: Requires custom middleware logic. |
| **Overhead** | **Con**: Slightly higher byte count (headers, JSON wrapping). | **Pro**: Minimal payload. |

**Recommendation:** Adopt HTTP Tunneling. The operational benefits of a single API implementation outweigh the negligible bandwidth overhead.

### 3.5 Event Types (Broker → Hub)

For Broker-to-Hub events (e.g., status updates, heartbeats), the Runtime Broker **MUST** use standard, direct HTTP requests to the Hub API, authenticated via HMAC.

The WebSocket control channel is primarily for **Hub-initiated** traffic (Tunneling) and **Bidirectional Streaming** (PTY). It is not used for Broker-initiated control plane events.

**Why?**
- **Simplicity:** Keeps the WebSocket protocol focused on "dial-in" capability.
- **Reliability:** Standard HTTP retries and load balancing can be used for events.
- **Scalability:** Events can be handled by any Hub instance, not just the one holding the WebSocket connection.

---

## 4. PTY Streaming

### 4.1 Stream Multiplexing

PTY sessions are opened with a `stream_open` message on the control channel (`wsprotocol.StreamOpenMessage`), which establishes a multiplexed stream. The Hub does not tunnel an HTTP upgrade request for this; the broker's direct `/api/v1/agents/{id}/attach` endpoint is a separate path that the CLI does not use.

**Stream Open (Hub → Broker):**
```json
{
  "type": "stream_open",
  "streamId": "stream-xyz",
  "streamType": "pty",
  "slug": "agent-123",
  "projectId": "project-1",
  "cols": 120,
  "rows": 40
}
```

**Stream Data Frame:**
```json
{
  "type": "stream",
  "streamId": "stream-xyz",
  "data": "base64-encoded-bytes"
}
```

**Stream Close:**
```json
{
  "type": "stream_close",
  "streamId": "stream-xyz",
  "reason": "client disconnected"
}
```

### 4.2 Browser PTY Flow

```
┌─────────┐         ┌─────────┐         ┌─────────────┐         ┌───────────┐
│ Browser │         │   Hub   │         │ Runtime Broker│         │ Container │
└────┬────┘         └────┬────┘         └──────┬──────┘         └─────┬─────┘
     │                   │                     │                      │
     │ WS connect        │                     │                      │
     │ /agents/{id}/pty  │                     │                      │
     │──────────────────►│                     │                      │
     │                   │                     │                      │
     │                   │ Tunnel Request      │                      │
     │                   │ (Upgrade: PTY)      │                      │
     │                   │────────────────────►│                      │
     │                   │                     │                      │
     │                   │                     │ tmux attach          │
     │                   │                     │─────────────────────►│
     │                   │                     │                      │
     │                   │ 101 Switching Proto │                      │
     │                   │◄────────────────────│                      │
     │                   │                     │                      │
     │ ◄─────────────────────────────────────► │ ◄───────────────────►│
     │         bidirectional data flow         │   tmux I/O           │
     │                   │                     │                      │
```

### 4.3 PTY Message Format (Browser/CLI ↔ Hub)

**Data Message:**
```json
{
  "type": "data",
  "data": "base64-encoded-bytes"
}
```

**Resize Message:**
```json
{
  "type": "resize",
  "cols": 120,
  "rows": 40
}
```

### 4.4 CLI PTY Flow

The CLI acts similarly to a browser but uses standard user authentication.

1.  **Auth**: CLI uses the user's Hub credentials from `scion hub auth login` (or `SCION_HUB_TOKEN`), plus transport-level headers when the Hub is behind IAP.
2.  **Connect**: CLI connects to `wss://hub.example.com/api/v1/agents/{id}/pty` (`pkg/wsclient/pty.go`). Any path prefix on the Hub endpoint is kept.
    *   Header: `Authorization: Bearer <token>`
3.  **Proxying**:
    *   Hub validates the user token and checks the `attach` action on the agent.
    *   Hub locates the target Runtime Broker. This hub process must hold that broker's control channel, otherwise it returns `503`.
    *   Hub sends `stream_open` (see 4.1) over the Control Channel to the Broker.
    *   Hub pipes the CLI WebSocket frames to the Broker Stream frames.
4.  **Terminal Mode**: CLI requires a terminal on stdin and stdout and sets its local TTY to raw mode. Detach is the tmux key (`Ctrl-b d`).
5.  **End**: the close frame carries a PTY close code (`pkg/wsprotocol/pty_close.go`). `1000` is a clean detach. For any other code the CLI prints a message based on `ClassifyPTYClose` and exits non-zero. It does not reconnect.

---

## 5. Authentication

### 5.1 Control Channel Authentication

The control channel WebSocket upgrade request is authenticated using HMAC:

```
HMAC-SHA256(shared_secret, "{brokerId}:{timestamp}:{nonce}:GET:{path}")
```

**Headers:**
| Header | Description |
|--------|-------------|
| `X-Scion-Broker-ID` | Runtime Broker identifier |
| `X-Scion-Timestamp` | RFC 3339 timestamp |
| `X-Scion-Nonce` | Random nonce for replay prevention |
| `X-Scion-Signature` | HMAC signature |

### 5.2 Session-Based Trust

Once the WebSocket is established with HMAC authentication:
- **Hub → Broker commands** use session-based trust (no per-message signing)
- **Broker → Hub requests** requiring authorization must use separate HMAC-authenticated HTTP requests

**Rationale:**
- WebSocket runs over TLS, providing transport-level integrity
- Initial connection establishes broker identity
- Similar trust model to SSH after key exchange
- Avoids per-message cryptographic overhead

### 5.3 Client WebSocket Authentication

There are distinct authentication strategies for Browsers and CLI clients due to platform limitations.

**Browser:**
Browsers using the standard `WebSocket` API **cannot** add custom headers (like `Authorization`) to the initial handshake request. This is a known security limitation of the web platform.
*   **Today:** the web terminal connects to the same `/pty` route, and its browser session cookie authenticates it (`credentials: 'include'`).
*   **Proposed, not implemented:** a short-lived, single-use "ticket" passed in the URL query string. There is no `/api/v1/auth/ws-ticket` endpoint, and the Hub's ticket validation is an unimplemented placeholder.
    1. `POST /api/v1/auth/ws-ticket` -> `{ "ticket": "..." }` (Authenticated with cookie/session)
    2. `WS /api/v1/agents/{id}/pty?ticket=<ticket>`

**CLI:**
The CLI (e.g., using a library like `gorilla/websocket`) has full control over the handshake headers.
*   **Solution:** Use the standard `Authorization` header with the Bearer token.
    *   `WS /api/v1/agents/{id}/pty` with `Authorization: Bearer <token>`

---

## 6. Connection Lifecycle

### 6.1 Heartbeat

- **Interval**: Broker sends heartbeat every 30 seconds
- **Timeout**: Hub marks broker as `disconnected` after 90 seconds without heartbeat
- **Format**:
  ```json
  {
    "type": "event",
    "event": "heartbeat",
    "payload": {
      "status": "online",
      "agentCount": 3,
      "resources": { ... }
    }
  }
  ```

### 6.2 Reconnection

- **Backoff**: Exponential backoff (1s, 2s, 4s, ... max 60s)
- **Session Resumption**: On reconnect, Hub sends list of expected agents for reconciliation
- **Stream Recovery**: Active streams are terminated on disconnect (PTY clients receive close code `4503`, reason `broker_disconnected`); clients must re-attach. The web terminal re-attaches on its own. The CLI does not: it prints a message and exits, and the user runs `scion attach` again.

### 6.3 Graceful Shutdown

**Broker Shutdown:**
1. Broker sends `shutting_down` heartbeat
2. Hub marks broker as `offline`
3. Hub fails pending commands for this broker
4. Active streams are terminated

**Hub Shutdown:**
1. Hub sends disconnect to connected brokers
2. Brokers enter reconnection loop
3. Commands queue up locally (if broker supports it)

---

## 7. Transport Selection

The Hub supports two transport modes for communicating with Runtime Brokers:

| Transport | Use Case | Selection Criteria |
|-----------|----------|-------------------|
| WebSocket Control Channel | Brokers behind NAT/firewalls | Broker has active WS connection |
| Direct HTTP | Brokers with reachable endpoints | Broker has registered `endpoint` URL |

**Selection Logic:**

```
When Hub needs to send request to Broker:
1. If Broker has active control channel → use WebSocket Tunnel
2. If Broker has registered endpoint and status == "online" → attempt Direct HTTP
3. Otherwise → return 502 runtime_error
```

---

## 8. Future Considerations

### 8.1 Alternative Transport: gRPC / HTTP/2

The current WebSocket + JSON approach could be replaced with gRPC for improved efficiency:

| Aspect | Current (WS+JSON) | gRPC/HTTP/2 |
|--------|-------------------|-------------|
| Framing | Manual JSON envelopes | Native protobuf framing |
| Multiplexing | Custom `streamId` management | Native HTTP/2 streams |
| Binary data | Base64 overhead (~33%) | Native binary transport |
| Type safety | JSON schema / manual validation | Proto definitions with codegen |
| Browser support | Native WebSocket API | Requires gRPC-Web proxy |

**Trade-offs:**
- gRPC-Web requires a proxy (Envoy) for browser support
- JSON is human-readable and easier to debug
- gRPC provides stronger API contracts and better tooling

**Decision**: Start with WebSocket + JSON for simplicity and universal browser support. Consider gRPC migration when performance or type safety becomes a bottleneck.

### 8.2 Hybrid: Command Queue + On-Demand WebSocket

For horizontal scalability, a hybrid approach could decouple command delivery from streaming:

```
┌────────────────────────────────────────────────────────────────┐
│                      Hybrid Architecture                        │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│   CRUD Operations (Polling)          Interactive Streams (WS)  │
│   ─────────────────────────          ────────────────────────  │
│                                                                 │
│   Hub ──► Queue ──► Broker          Browser ◄──► Hub ◄──► Broker│
│       (DB-backed)                         (WebSocket relay)    │
│                                                                 │
│   • create_agent                     • PTY attachment          │
│   • stop_agent                       • Live log streaming      │
│   • delete_agent                                               │
│   • exec (async)                                               │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

**Benefits:**
- Hub instances are stateless (any can write commands, any can serve polls)
- Commands persist through Hub restarts
- Easier horizontal scaling

**Limitations:**
- Polling introduces latency (5-10 seconds)
- Requires on-demand WebSocket for PTY

**Decision**: Deferred. Current design uses persistent WebSocket. Revisit if horizontal scaling becomes a priority.

### 8.3 Open Questions

**1. Stream-ready WebSocket vs On-Demand**
*Question:* Should brokers maintain a persistent "stream-ready" WebSocket, or connect on-demand per stream?
*Recommendation:* **Multiplex over Control Channel**. Opening a new WebSocket connection from Broker to Hub for every PTY session introduces latency and connection management overhead. Multiplexing over the existing authenticated control channel is more efficient and firewall-friendly.

**2. Stream token expiration**
*Question:* How to handle cleanup of unused stream tokens/tickets?
*Recommendation:* **Short TTL + Single Use**. Tickets for browser WebSocket connection should expire in 60 seconds and be invalidated immediately upon use. This prevents replay attacks and accumulation of stale state in the Hub.

**3. WebRTC**
*Question:* Can browser-to-broker PTY bypass the Hub using WebRTC in some scenarios?
*Decision:* **No**. The Runtime Broker is designed to operate in environments that are unreachable from the public internet (behind NAT/firewalls). While WebRTC *can* traverse NAT, the complexity of STUN/TURN setup outweighs the benefits for text-based PTY streams. All traffic will proxy through the Hub.

---

## 9. Implementation Status

> **Last Updated:** 2026-02-03

### 9.1 Completed Components

| Component | Files | Status |
|-----------|-------|--------|
| **Protocol Types** | `pkg/wsprotocol/protocol.go`, `connection.go` | ✅ Complete |
| **Hub Control Channel** | `pkg/hub/controlchannel.go`, `controlchannel_client.go` | ✅ Complete |
| **Hub PTY Endpoint** | `pkg/hub/pty_handlers.go` | ✅ Complete |
| **Runtime Broker Control Channel** | `pkg/runtimebroker/controlchannel.go` | ✅ Complete |
| **Runtime Broker PTY Handler** | `pkg/runtimebroker/pty_handlers.go` | ✅ Complete |
| **CLI WebSocket Client** | `pkg/wsclient/pty.go` | ✅ Complete |
| **CLI Attach Command** | `cmd/attach.go` | ✅ Complete |

### 9.2 Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                           CLI                                    │
│                    (pkg/wsclient/pty.go)                        │
└─────────────────────┬───────────────────────────────────────────┘
                      │ WebSocket: /api/v1/agents/{id}/pty
                      ▼
┌─────────────────────────────────────────────────────────────────┐
│                           Hub                                    │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │ PTY Handler (pkg/hub/pty_handlers.go)                    │   │
│  │ - Authenticates client                                   │   │
│  │ - Opens stream to broker                                 │   │
│  │ - Relays data bidirectionally                           │   │
│  └─────────────────────┬───────────────────────────────────┘   │
│                        │                                        │
│  ┌─────────────────────▼───────────────────────────────────┐   │
│  │ Control Channel Manager (pkg/hub/controlchannel.go)      │   │
│  │ - Manages broker connections                             │   │
│  │ - Tunnels HTTP requests                                  │   │
│  │ - Multiplexes streams                                    │   │
│  └─────────────────────┬───────────────────────────────────┘   │
└─────────────────────────┼───────────────────────────────────────┘
                          │ WebSocket: /api/v1/runtime-brokers/connect
                          ▼
┌─────────────────────────────────────────────────────────────────┐
│                      Runtime Broker                                │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │ Control Channel Client (pkg/runtimebroker/controlchannel.go)│   │
│  │ - Connects to Hub                                        │   │
│  │ - Handles tunneled requests                              │   │
│  │ - Manages PTY streams                                    │   │
│  └─────────────────────┬───────────────────────────────────┘   │
│                        │                                        │
│  ┌─────────────────────▼───────────────────────────────────┐   │
│  │ PTY Handler (pkg/runtimebroker/pty_handlers.go)            │   │
│  │ - Attaches to tmux session via docker exec              │   │
│  │ - Pipes I/O to stream                                   │   │
│  └─────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────┘
```

### 9.3 Key Features Implemented

**Control Channel:**
- WebSocket endpoint at `/api/v1/runtime-brokers/connect`
- Broker connection management with session IDs
- HTTP request tunneling through WebSocket
- Stream multiplexing for PTY sessions
- Ping/pong keepalive
- Automatic fallback to HTTP when control channel unavailable
- Graceful shutdown
- HMAC authentication for WebSocket upgrade
- Reconnection with exponential backoff

**PTY Streaming:**
- WebSocket endpoint at `/api/v1/agents/{id}/pty`
- User authentication (Bearer token; ticket validation not implemented)
- Agent lookup and access control
- Stream proxy to runtime broker via control channel
- Bidirectional data relay
- Docker exec integration with tmux
- Terminal raw mode handling in CLI
- SIGWINCH resize event propagation

### 9.4 Configuration

**Runtime Broker:**
```yaml
runtimeBroker:
  hubEndpoint: "http://localhost:9810"
  controlChannelEnabled: true  # Enable WebSocket control channel
```

### 9.5 Remaining Enhancements

| Item | Description | Priority |
|------|-------------|----------|
| PTY Ticket Validation | Single-use tickets for browser clients. Not implemented: no ticket endpoint exists and validation is a placeholder. | Medium |
| CLI reconnect | The CLI exits on any non-clean close. Reconnect is planned as part of the move to the conduit relay. | Medium |
| Conduit migration | Attach is expected to move from this control-channel stream to the conduit relay, which also removes the requirement that the hub process serving the attach holds the broker's control channel (multi-replica hubs). No user action is expected. | Medium |
| Resize Propagation | Apply terminal resize to tmux sessions | Low |
| Integration Tests | End-to-end WebSocket tests | Medium |
| Browser Terminal | xterm.js integration with PTY endpoint | High |

---

## Related Documentation

| Document | Relevant Sections |
|----------|-------------------|
| [hub-api.md](hub-api.md) | Section 8 (WebSocket Endpoints), Section 11 (Broker Control Plane Protocol), Section 15 (Future Considerations) |
| [runtime-broker-api.md](runtime-broker-api.md) | Section 3.2 (Control Channel), Section 4.2 (Attach PTY) |
| [auth/runtime-broker-auth.md](auth/runtime-broker-auth.md) | Section 10.4 (Hub-to-Broker Communication), Section 10.5 (WebSocket Message Auth) |
| [server-implementation-design.md](server-implementation-design.md) | Section 12.5 (WebSocket Proxying) |
| [web-frontend-design.md](web-frontend-design.md) | Terminal component WebSocket integration |
