# Web NATS + SSE Real-Time Testing Walkthrough

> **2026-02-19 — NATS approach abandoned.** This walkthrough documents testing for the NATS-based SSE pipeline, which is being replaced by an in-process Go channel design (`ChannelEventPublisher`). The Koa BFF is being consolidated into the Go binary, eliminating NATS as a runtime dependency. See `.design/hosted/web-realtime.md` for the current design. This walkthrough remains as a historical reference for the Koa/NATS implementation while it is still in use during the migration.

This guide provides step-by-step instructions for testing the NATS-to-SSE real-time event pipeline in the Scion Web Frontend (Milestones 7 and 8).

## Architecture Overview

```
Browser (EventSource) ←── SSE stream ←── Koa /events endpoint
                                              │
                                         SSEManager
                                              │
                                         NatsClient
                                              │
                                         NATS Server
                                              │
                                    Hub / Runtime Broker
                                    (publishes events)
```

The pipeline is unidirectional: NATS messages published by the Hub or Runtime Broker flow through the web server's SSE Manager and arrive at the browser as Server-Sent Events. Subscriptions are declared via query parameters at connection time and are immutable for the connection lifetime.

## Prerequisites

- Node.js 20+ installed
- Docker (for running NATS)
- NATS CLI (`nats`) for publishing test messages (optional but recommended)
- `curl` for testing endpoints
- `jq` for JSON formatting (optional)
- A running Hub API (or `DEV_AUTH=true` for local development without one)

## 1. Start NATS Server

```bash
# Run NATS locally via Docker
docker run -d --name scion-nats -p 4222:4222 nats:latest

# Verify NATS is running
nats server check connection --server nats://localhost:4222
```

If you don't have the `nats` CLI, you can verify with:

```bash
curl -s telnet://localhost:4222 || echo "NATS listening on 4222"
```

## 2. Start the Web Server with NATS Enabled

```bash
cd web

# Minimal dev startup with NATS
SCION_NATS_URL=nats://localhost:4222 DEV_AUTH=true npm run dev
```

You should see output including:

```
║  NATS: enabled (nats://localhost:4222)                    ║
```

And shortly after:

```
[NATS] Connected to nats://localhost:4222
[NATS] Ready for SSE subscriptions
```

### Environment Variables Reference

| Variable | Default | Description |
|----------|---------|-------------|
| `SCION_NATS_URL` | (none) | NATS server URL(s), comma-separated for clusters |
| `NATS_URL` | (none) | Fallback if `SCION_NATS_URL` is not set |
| `NATS_TOKEN` | (none) | Optional auth token for NATS connection |
| `NATS_ENABLED` | `true` if URL set | Explicitly enable/disable NATS |
| `NATS_MAX_RECONNECT` | `-1` (infinite) | Max reconnect attempts before giving up |
| `DEV_AUTH` | (none) | Set to `true` to bypass OAuth for local testing |
| `PORT` | `8080` | Web server port |

---

## 3. Health Endpoint Verification

### Liveness Probe

```bash
curl -s http://localhost:8080/healthz | jq
```

Expected response (always 200 if server is running):

```json
{
  "status": "healthy",
  "timestamp": "2026-02-18T12:00:00.000Z",
  "uptime": 5.123
}
```

### Readiness Probe (with NATS status)

```bash
curl -s http://localhost:8080/readyz | jq
```

Expected when NATS is connected (HTTP 200):

```json
{
  "status": "healthy",
  "timestamp": "2026-02-18T12:00:00.000Z",
  "uptime": 5.123,
  "nats": "connected"
}
```

### Readiness When NATS Is Down

Stop the NATS container and retry:

```bash
docker stop scion-nats
curl -s -w "\nHTTP Status: %{http_code}\n" http://localhost:8080/readyz | jq
```

Expected (HTTP 503):

```json
{
  "status": "unhealthy",
  "timestamp": "2026-02-18T12:00:00.000Z",
  "uptime": 30.456,
  "nats": "reconnecting"
}
```

Restart NATS and verify recovery:

```bash
docker start scion-nats
# Wait a few seconds for reconnection
sleep 3
curl -s http://localhost:8080/readyz | jq
```

Expected: status returns to `"healthy"` with `"nats": "connected"`.

---

## 4. SSE Endpoint Tests

The SSE endpoint is `GET /events?sub=<subject>`. It requires authentication (a valid session cookie or dev-auth).

### 4.1 Obtain a Session Cookie

With `DEV_AUTH=true`, any request to a protected route auto-creates a session. Capture the cookie:

```bash
# Get the session cookie from the dev server
SESSION_COOKIE=$(curl -s -D - http://localhost:8080/ 2>&1 | grep -i 'set-cookie' | head -1 | sed 's/.*: //' | cut -d';' -f1)
echo "Cookie: $SESSION_COOKIE"
```

### 4.2 Open an SSE Connection

```bash
# Subscribe to a grove-scoped wildcard
curl -N -H "Cookie: $SESSION_COOKIE" \
  "http://localhost:8080/events?sub=grove.test-grove.>"
```

Expected: the connection stays open and you receive an initial `connected` event:

```
id: 1
event: connected
data: {"connectionId":"sse-1","subjects":["grove.test-grove.>"]}
```

The stream will then emit a named `heartbeat` event every 30 seconds to keep the connection alive. Its data is a millisecond timestamp, and it has no `id:` line.

### 4.3 Multiple Subject Subscriptions

```bash
# Two separate sub params
curl -N -H "Cookie: $SESSION_COOKIE" \
  "http://localhost:8080/events?sub=grove.abc.>&sub=agent.xyz.>"

# Comma-separated in a single param
curl -N -H "Cookie: $SESSION_COOKIE" \
  "http://localhost:8080/events?sub=grove.abc.>,agent.xyz.>"
```

Both formats are supported. The `connected` event will list all subjects.

### 4.4 Subject Validation — Error Cases

**Missing subjects (400):**

```bash
curl -s -H "Cookie: $SESSION_COOKIE" \
  "http://localhost:8080/events" | jq
```

```json
{
  "error": "Bad Request",
  "message": "At least one subject is required. Use ?sub=grove.mygrove.>"
}
```

**Bare wildcards (400):**

```bash
curl -s -H "Cookie: $SESSION_COOKIE" \
  "http://localhost:8080/events?sub=>" | jq
```

```json
{
  "error": "Bad Request",
  "message": "Invalid subjects",
  "details": [">: Bare wildcards are not allowed"]
}
```

**Invalid prefix (400):**

```bash
curl -s -H "Cookie: $SESSION_COOKIE" \
  "http://localhost:8080/events?sub=system.internal.>" | jq
```

```json
{
  "error": "Bad Request",
  "message": "Invalid subjects",
  "details": ["system.internal.>: Subject must start with one of: grove., agent., broker."]
}
```

**Single-token subject (400):**

```bash
curl -s -H "Cookie: $SESSION_COOKIE" \
  "http://localhost:8080/events?sub=grove." | jq
```

```json
{
  "error": "Bad Request",
  "message": "Invalid subjects",
  "details": ["grove.: Subject must have at least two tokens (e.g., grove.mygrove)"]
}
```

**NATS unavailable (503):**

```bash
docker stop scion-nats
sleep 3
curl -s -w "\nHTTP Status: %{http_code}\n" \
  -H "Cookie: $SESSION_COOKIE" \
  "http://localhost:8080/events?sub=grove.test.>" | jq
docker start scion-nats
```

```json
{
  "error": "Service Unavailable",
  "message": "Real-time event service is not available"
}
```

---

## 5. End-to-End NATS → SSE Message Flow

This is the core test: publish a NATS message and verify it arrives in the SSE stream.

### Terminal Layout

Open three terminals:

| Terminal | Purpose |
|----------|---------|
| T1 | Web server (`npm run dev`) |
| T2 | SSE listener (`curl -N ...`) |
| T3 | NATS publisher (`nats pub ...`) |

### 5.1 Subscribe to a Grove (T2)

```bash
curl -N -H "Cookie: $SESSION_COOKIE" \
  "http://localhost:8080/events?sub=grove.test-grove.>"
```

Wait for the `connected` event.

### 5.2 Publish Agent Status Update (T3)

```bash
nats pub grove.test-grove.agent.status \
  '{"agentId":"agent-001","status":"running","sessionStatus":"idle"}'
```

### Expected in T2

```
id: 2
event: update
data: {"subject":"grove.test-grove.agent.status","data":{"agentId":"agent-001","status":"running","sessionStatus":"idle"}}
```

### 5.3 Publish Agent Created Event (T3)

```bash
nats pub grove.test-grove.agent.created \
  '{"agentId":"agent-002","name":"test-agent","template":"claude","status":"provisioning"}'
```

### Expected in T2

```
id: 3
event: update
data: {"subject":"grove.test-grove.agent.created","data":{"agentId":"agent-002","name":"test-agent","template":"claude","status":"provisioning"}}
```

### 5.4 Publish Agent Deleted Event (T3)

```bash
nats pub grove.test-grove.agent.deleted \
  '{"agentId":"agent-001"}'
```

### 5.5 Publish Grove Summary (T3)

```bash
nats pub grove.test-grove.summary \
  '{"groveId":"test-grove","name":"Test Grove","agentCount":5,"runningCount":3}'
```

### 5.6 Agent-Scoped Heavy Event (T3)

This event should **not** appear on the grove subscription, only on an agent-specific subscription.

```bash
# This should NOT appear in T2 (grove subscription)
nats pub agent.agent-001.event \
  '{"type":"tool_use","data":"heavy harness output payload..."}'

# To receive it, open a second SSE listener with agent scope:
curl -N -H "Cookie: $SESSION_COOKIE" \
  "http://localhost:8080/events?sub=grove.test-grove.>&sub=agent.agent-001.>"
```

Now the agent-scoped event will appear on the second listener but not the first.

### 5.7 Non-JSON Payloads

The SSE manager handles non-JSON payloads gracefully:

```bash
nats pub grove.test-grove.agent.log "plain text message"
```

Expected in T2:

```
id: N
event: update
data: {"subject":"grove.test-grove.agent.log","data":"plain text message"}
```

---

## 6. Connection Lifecycle Tests

### 6.1 Heartbeat Verification

Hold an SSE connection open for 30+ seconds and observe heartbeat events:

```
event: heartbeat
data: 1708264830000
```

These are named events (data is a millisecond timestamp) rather than SSE comments, so page code can observe them with `addEventListener('heartbeat', ...)`. They carry no `id:` line, so `Last-Event-ID` is unaffected, and they keep TCP connections alive through proxies.

### 6.2 Client Disconnect Cleanup

1. Open an SSE connection in T2
2. Send Ctrl+C to close the curl client
3. Check web server logs in T1 for cleanup messages
4. Verify no lingering NATS subscriptions (the server logs subscription creation/removal)

### 6.3 NATS Reconnection

1. Open an SSE connection (T2)
2. Stop NATS: `docker stop scion-nats`
3. Observe web server logs showing `[NATS] Disconnected` and `[NATS] Reconnecting...`
4. Restart NATS: `docker start scion-nats`
5. Observe `[NATS] Reconnected` in web server logs
6. Publish a new message via T3 — it should arrive in T2's SSE stream

### 6.4 Last-Event-ID Resume

The `Last-Event-ID` header is used for resume on reconnect:

```bash
# Simulate resuming from event ID 5
curl -N -H "Cookie: $SESSION_COOKIE" \
  -H "Last-Event-ID: 5" \
  "http://localhost:8080/events?sub=grove.test-grove.>"
```

The connection should open normally. The `lastEventId` counter on the server side starts from the provided value (subsequent events are numbered 6, 7, ...). Note: missed events between the disconnect and reconnect are **not** replayed — the event ID is used for ordering continuity only.

---

## 7. NATS-Disabled Graceful Degradation

Verify the web server works correctly without NATS:

```bash
# Start without NATS URL
DEV_AUTH=true npm run dev
```

Expected startup output:

```
║  NATS: disabled                                           ║
```

### Verify Behavior

| Test | Expected |
|------|----------|
| `/healthz` | 200, no `nats` field |
| `/readyz` | 200, no `nats` field |
| `/events?sub=grove.test.>` | 503 Service Unavailable |
| Page loads (`/groves`) | Pages render normally, SSE silently not connected |

---

## 8. Browser-Side Testing

These tests verify the client-side SSE client and StateManager integration.

### 8.1 SSE Connection in DevTools

1. Navigate to `http://localhost:8080/groves` in a browser
2. Open DevTools → Network tab → filter by "EventSource" or "events"
3. Verify an `EventSource` connection is opened to `/events?sub=grove.*.summary`
4. The connection should show a `connected` event in the EventStream tab

### 8.2 Scope-Based Subscription Changes

1. Navigate to `/groves` — observe SSE URL: `/events?sub=grove.*.summary`
2. Click into a specific grove (e.g., grove ID `abc`) — observe:
   - Old SSE connection closes
   - New SSE connection opens to `/events?sub=grove.abc.>`
3. Click into an agent detail (e.g., agent ID `xyz`) — observe:
   - SSE reconnects to `/events?sub=grove.abc.>&sub=agent.xyz.>`
4. Navigate back to `/groves` — observe:
   - SSE reconnects to `/events?sub=grove.*.summary`

### 8.3 Real-Time UI Updates

1. Navigate to a grove detail page (e.g., `/groves/test-grove`)
2. In a terminal, publish an agent status change:

```bash
nats pub grove.test-grove.agent.status \
  '{"agentId":"<actual-agent-id>","status":"stopped"}'
```

3. Verify the agent's status badge updates in the browser without a page refresh

### 8.4 Agent Created / Deleted

1. On a grove detail page, publish a "created" event:

```bash
nats pub grove.test-grove.agent.created \
  '{"agentId":"new-agent-123","name":"New Agent","status":"provisioning"}'
```

2. Verify the new agent appears in the agent list
3. Publish a "deleted" event:

```bash
nats pub grove.test-grove.agent.deleted \
  '{"agentId":"new-agent-123"}'
```

4. Verify the agent is removed from the list

### 8.5 Reconnection Behavior

1. Open a grove page in the browser
2. Stop the NATS container: `docker stop scion-nats`
3. Open browser console — observe `[SSE] Reconnecting in Xms (attempt N)` messages
4. Verify exponential backoff: 1s, 2s, 4s, 8s, 16s, 30s (capped)
5. Restart NATS: `docker start scion-nats`
6. Verify `[SSE] Connected` appears and events resume

### 8.6 Page Unload Cleanup

1. Open browser DevTools Network tab
2. Navigate to a grove page (SSE connection opens)
3. Close the tab or navigate to an external URL
4. Verify the SSE connection is closed (check server logs for cleanup)

---

## 9. Subscription Scope Verification Matrix

| Page Route | Expected SSE URL | Scope Type |
|------------|-----------------|------------|
| `/groves` | `/events?sub=grove.*.summary` | `dashboard` |
| `/groves/:groveId` | `/events?sub=grove.{groveId}.>` | `grove` |
| `/agents/:agentId` | `/events?sub=grove.{groveId}.>&sub=agent.{agentId}.>` | `agent-detail` |

The agent-detail scope includes both grove-level and agent-level subscriptions. The grove-level subscription keeps sidebar/breadcrumb state fresh; the agent subscription adds heavy events (harness output) for the detail view.

---

## 10. Allowed Subject Prefixes

| Prefix | Typical Use | Example |
|--------|-------------|---------|
| `grove.` | Grove-scoped events (agent status, broker health) | `grove.abc.agent.status` |
| `agent.` | Agent-scoped heavy events (harness output) | `agent.xyz.event` |
| `broker.` | Broker-scoped events | `broker.brk1.health` |

Subjects outside these prefixes are rejected with 400. Bare wildcards (`>`, `*`) are rejected. All subjects must have at least two tokens (e.g., `grove.mygrove` minimum).

---

## 11. Server-Side Log Messages Reference

| Log Entry | Meaning |
|-----------|---------|
| `[NATS] Connected to ...` | Successfully connected to NATS server |
| `[NATS] Disconnected: ...` | Lost connection, will attempt reconnect |
| `[NATS] Reconnecting...` | Actively attempting to reconnect |
| `[NATS] Reconnected to ...` | Successfully reconnected |
| `[NATS] Draining connection...` | Graceful shutdown in progress |
| `[NATS] Connection drained and closed` | Shutdown complete |
| `[NATS] Ready for SSE subscriptions` | Initial connection established |
| `[SSE] Failed to subscribe to ...` | Subscription error during connection setup |
| `[SSE] Closing all connections...` | Server shutdown closing SSE streams |

---

## 12. Graceful Shutdown Sequence

When the server receives SIGTERM or SIGINT:

1. SSE Manager closes all active SSE connections (unsubscribes NATS, ends streams)
2. NATS client drains (finishes in-flight messages) then closes
3. HTTP server closes
4. Force shutdown after 10 seconds if steps don't complete

Test this:

```bash
# Start server, open an SSE connection, then:
kill -SIGTERM $(pgrep -f "tsx.*index.ts")
```

Observe the shutdown sequence in server logs:

```
SIGTERM received. Shutting down gracefully...
[SSE] Closing all connections...
[NATS] Draining connection...
[NATS] Connection drained and closed
Server closed successfully
```

---

## 13. Quick Reference: Full Test Sequence

```bash
# 1. Start NATS
docker run -d --name scion-nats -p 4222:4222 nats:latest

# 2. Start web server
cd web
SCION_NATS_URL=nats://localhost:4222 DEV_AUTH=true npm run dev

# 3. Get session cookie
SESSION_COOKIE=$(curl -s -D - http://localhost:8080/ 2>&1 \
  | grep -i 'set-cookie' | head -1 | sed 's/.*: //' | cut -d';' -f1)

# 4. Health checks
curl -s http://localhost:8080/healthz | jq
curl -s http://localhost:8080/readyz | jq

# 5. Open SSE listener (leave running)
curl -N -H "Cookie: $SESSION_COOKIE" \
  "http://localhost:8080/events?sub=grove.test.>" &
SSE_PID=$!

# 6. Publish test events
nats pub grove.test.agent.status '{"agentId":"a1","status":"running"}'
nats pub grove.test.agent.created '{"agentId":"a2","name":"new","status":"starting"}'
nats pub grove.test.agent.deleted '{"agentId":"a2"}'
nats pub grove.test.summary '{"groveId":"test","agentCount":1}'

# 7. Error cases
curl -s "http://localhost:8080/events" | jq                          # 400
curl -s -H "Cookie: $SESSION_COOKIE" "http://localhost:8080/events?sub=>" | jq  # 400

# 8. Cleanup
kill $SSE_PID
docker stop scion-nats && docker rm scion-nats
```
