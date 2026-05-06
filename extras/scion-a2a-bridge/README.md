# scion-a2a-bridge

A protocol bridge that exposes Scion agents as [A2A (Agent-to-Agent)](https://google.github.io/A2A/) endpoints, allowing any A2A-compatible client to discover and interact with agents managed by a Scion Hub.

## What it does

- Translates A2A JSON-RPC requests into Scion Hub API calls and vice versa.
- Generates A2A Agent Cards for each exposed agent, enriched with metadata from the Hub.
- Supports blocking request/response, SSE streaming, and push notification (webhook) delivery modes.
- Manages task lifecycle state in a local SQLite database.
- Connects to the Hub via a broker plugin (go-plugin RPC) to receive agent messages in real time.

## Configuration

Copy and edit the sample configuration file:

```sh
cp scion-a2a-bridge.yaml.sample scion-a2a-bridge.yaml
```

Key sections:

| Section | Purpose |
|---------|---------|
| `bridge` | A2A HTTP server address, external URL, provider metadata |
| `hub` | Scion Hub endpoint, admin user, signing key (file path or GCP Secret Manager) |
| `plugin` | Broker plugin RPC listen address (default `localhost:9090`) |
| `auth` | Client authentication — API key, bearer token, or OAuth2 |
| `groves` | Which groves and agents to expose, with optional auto-provisioning |
| `state` | SQLite database path |
| `timeouts` | Send message timeout, SSE keepalive interval, push retry limit |
| `rate_limit` | Per-key token-bucket rate limiting |
| `logging` | Log level and format (text or JSON) |

Environment variables can be referenced as `${VAR_NAME}` in the YAML file.

## Running

### Locally

```sh
go build -o scion-a2a-bridge ./cmd/scion-a2a-bridge/
./scion-a2a-bridge --config scion-a2a-bridge.yaml
```

### Docker

```sh
docker build -t scion-a2a-bridge -f Dockerfile ../..
docker run -p 8443:8443 -p 9090:9090 \
  -v /path/to/config.yaml:/etc/scion-a2a-bridge/config.yaml \
  scion-a2a-bridge
```

## Key endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/.well-known/agent-card.json` | GET | Bridge registry card |
| `/groves/{grove}/agents/{agent}/.well-known/agent-card.json` | GET | Per-agent A2A card |
| `/groves/{grove}/agents/{agent}/jsonrpc` | POST | A2A JSON-RPC endpoint |
| `/healthz` | GET | Liveness check |
| `/readyz` | GET | Readiness check (database, broker) |
| `/metrics` | GET | Prometheus metrics |

### Supported JSON-RPC methods

- `message/send` — send a message (blocking or non-blocking)
- `message/stream` — send a message with SSE streaming response
- `tasks/get` — retrieve task status by ID
- `tasks/list` — list tasks by context ID
- `tasks/cancel` — cancel an in-progress task
- `tasks/resubscribe` — re-attach an SSE stream to an active task
- `tasks/pushNotification/set` — register a webhook for task updates
- `tasks/pushNotification/get` — list webhooks for a task
- `tasks/pushNotification/delete` — remove a webhook

## Ports

| Port | Purpose |
|------|---------|
| 8443 | A2A HTTP server (JSON-RPC, agent cards, health/metrics) |
| 9090 | Broker plugin RPC (Hub connects here to push agent messages) |
