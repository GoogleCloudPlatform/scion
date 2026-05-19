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
| `auth` | Client authentication — API key or bearer token |
| `projects` | Which projects and agents to expose, with optional auto-provisioning |
| `state` | SQLite database path |
| `timeouts` | Send message timeout, SSE keepalive interval, push retry limit |
| `rate_limit` | Per-key token-bucket rate limiting |
| `logging` | Log level and format (text or JSON) |

Environment variables can be referenced as `${VAR_NAME}` in the YAML file. **Note:** `os.Expand` has no escape mechanism — a literal `$` in config values (e.g. in an API key like `Pa$$w0rd`) will be interpreted as the start of an environment variable reference. Avoid literal `$` in non-variable config values, or set such values via environment variables instead.

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
| `/projects/{project}/agents/{agent}/.well-known/agent-card.json` | GET | Per-agent A2A card |
| `/projects/{project}/agents/{agent}/jsonrpc` | POST | A2A JSON-RPC endpoint |
| `/healthz` | GET | Liveness check |
| `/readyz` | GET | Readiness check (database, broker) |
| `/metrics` | GET | Prometheus metrics (restrict access — see Security) |

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

## TLS

The A2A server listens on plain HTTP. **TLS must be terminated at a reverse proxy** (e.g. Caddy, nginx, or a cloud load balancer) in front of the bridge. The bridge logs a `WARN` at startup as a reminder. Do not expose the bridge port directly to the internet without a TLS-terminating proxy.

## Ports

| Port | Purpose |
|------|---------|
| 8443 | A2A HTTP server (JSON-RPC, agent cards, health/metrics) |
| 9090 | Broker plugin RPC (Hub connects here to push agent messages) |

## Setup and onboarding (agent instructions)

Step-by-step instructions for installing, configuring, and running the A2A bridge from scratch. Every command is copy-paste ready.

### 1. Prerequisites

1. Install Go 1.25 or later. Confirm with `go version`.
2. Have network access to a running Scion Hub instance. Note its HTTP API URL (e.g., `https://hub.example.com`).
3. Obtain the Hub's HS256 signing key, base64-encoded. This is either:
   - A file on disk containing the raw base64 string, or
   - A GCP Secret Manager resource name (e.g., `projects/my-project/secrets/hub-signing-key`).
4. Have a Hub admin user email that the bridge will authenticate as (e.g., `a2a-bridge@example.com`).
5. Know the project slug(s) you want to expose over A2A.

### 2. Build the binary

From the repository root:

```sh
cd extras/scion-a2a-bridge
go build -o scion-a2a-bridge ./cmd/scion-a2a-bridge/
```

Verify the binary exists:

```sh
ls -l scion-a2a-bridge
```

### 3. Create and edit the config file

Copy the sample config:

```sh
cp scion-a2a-bridge.yaml.sample scion-a2a-bridge.yaml
```

Edit `scion-a2a-bridge.yaml`. The required fields are:

| Field | Description | Example |
|-------|-------------|---------|
| `hub.endpoint` | Hub HTTP API URL | `https://hub.example.com` |
| `hub.user` | Admin identity the bridge uses for Hub API calls | `a2a-bridge@example.com` |
| `hub.signing_key` | Path to a file containing the Hub's base64-encoded HS256 signing key. Mutually exclusive with `hub.signing_key_secret`. | `/path/to/signing-key.b64` |
| `hub.signing_key_secret` | GCP Secret Manager resource name for the signing key. Mutually exclusive with `hub.signing_key`. | `projects/my-project/secrets/hub-signing-key` |
| `bridge.listen_address` | Address for the A2A HTTP server | `:8443` |
| `bridge.external_url` | Public URL where A2A clients reach the bridge | `https://a2a.example.com` |
| `auth.api_key` | Static API key clients pass in the `X-API-Key` header. Supports env var expansion. | `${A2A_API_KEY}` |
| `projects[].slug` | Grove slug to expose. Add one entry per project. | `my-project` |
| `plugin.listen_address` | Broker plugin RPC listen address | `localhost:9090` |

Minimal config example:

```yaml
bridge:
  listen_address: ":8443"
  external_url: "https://a2a.example.com"
  provider:
    organization: "My Org"
    url: "https://example.com"

hub:
  endpoint: "https://hub.example.com"
  user: "a2a-bridge@example.com"
  signing_key: "/path/to/signing-key.b64"

plugin:
  listen_address: "localhost:9090"

auth:
  scheme: "apiKey"
  api_key: "${A2A_API_KEY}"

projects:
  - slug: "my-project"
    auto_provision: false

state:
  database: "/var/lib/scion-a2a-bridge/state.db"

logging:
  level: "info"
  format: "json"
```

### 4. Configure the Hub

Edit the Hub's `settings.yaml` to enable the message broker and register the bridge as a plugin.

Add or update these sections:

```yaml
message_broker:
  enabled: true

plugins:
  a2a-bridge:
    type: self_managed
    address: "localhost:9090"
```

The `address` must match `plugin.listen_address` in the bridge config. Restart the Hub after making these changes.

### 5. Start the bridge

Set the API key environment variable (if using `${A2A_API_KEY}` in config):

```sh
export A2A_API_KEY="your-api-key-here"
```

Start the bridge:

```sh
./scion-a2a-bridge --config scion-a2a-bridge.yaml
```

### 6. Verify it is running

Run these checks against the bridge (adjust host/port if `bridge.listen_address` differs):

```sh
# Liveness check — expect HTTP 200
curl -s -o /dev/null -w '%{http_code}' http://localhost:8443/healthz

# Readiness check — expect HTTP 200 (database and broker connected)
curl -s -o /dev/null -w '%{http_code}' http://localhost:8443/readyz

# Bridge registry agent card — expect JSON with A2A agent card
curl -s http://localhost:8443/.well-known/agent-card.json | head -20

# Per-agent card (replace PROJECT and AGENT with actual values)
curl -s http://localhost:8443/projects/PROJECT/agents/AGENT/.well-known/agent-card.json
```

### 7. Test with a sample JSON-RPC request

Send a `message/send` request to an agent's JSON-RPC endpoint:

```sh
curl -s -X POST \
  -H "Content-Type: application/json" \
  -H "X-API-Key: ${A2A_API_KEY}" \
  http://localhost:8443/projects/PROJECT/agents/AGENT/jsonrpc \
  -d '{
    "jsonrpc": "2.0",
    "id": "test-1",
    "method": "message/send",
    "params": {
      "message": {
        "role": "user",
        "parts": [
          {"kind": "text", "text": "Hello, agent."}
        ]
      }
    }
  }'
```

Replace `PROJECT` and `AGENT` with the target project slug and agent name. A successful response contains a `result` object with `id`, `status`, and `artifacts` fields.

### 8. Docker deployment

The Docker build requires `CGO_ENABLED=1` (for `go-sqlite3`). The default runtime image is `debian:bookworm-slim` (glibc). Alpine users must use a musl-compatible SQLite driver or switch to a glibc-based image.

Build the image from the repository root (the Dockerfile expects the full repo context):

```sh
docker build -t scion-a2a-bridge -f extras/scion-a2a-bridge/Dockerfile .
```

Run the container, mounting your config file:

```sh
docker run -p 8443:8443 -p 9090:9090 \
  -e A2A_API_KEY="your-api-key-here" \
  -v /path/to/scion-a2a-bridge.yaml:/etc/scion-a2a-bridge/config.yaml \
  -v /path/to/signing-key.b64:/etc/scion-a2a-bridge/signing-key.b64 \
  scion-a2a-bridge
```

The container runs as non-root user `bridge` (UID 1000). The state database directory `/var/lib/scion-a2a-bridge/` is writable by this user inside the container (mode `0700`). To persist state across restarts, mount a volume at that path.

## Known Limitations (MVP)

- **Single-turn only.** The bridge treats the first non-state-change message from an agent as the final response and closes the task. Multi-turn agents that emit interim content (clarifying questions, progress updates) will have their task closed prematurely. Agents using `input-required` → `completed` flows are not supported yet. Agent cards advertise `streaming: false` and `pushNotifications: false` to reflect this constraint. Streaming requests (`message/stream`) are accepted but emit a runtime warning because multi-turn dispatch is not implemented.
- **Blocking-mode `input-required` flows never resolve.** State-change messages are intentionally skipped for blocking waiters so the actual content reply is delivered. This means a blocking `message/send` call against an agent that transitions to `input-required` will time out (default 120s) because the state change is suppressed and no content reply follows.

## Security considerations

### SQLite state database

The SQLite database stores webhook bearer credentials (`token`, `auth_credentials`) in cleartext. To protect these secrets at rest:

- **File permissions**: the database file must be readable only by the bridge process (`chmod 0600`). The Dockerfile enforces `0700` on the data directory.
- **Encrypted storage**: deploy the database on an encrypted volume (e.g., LUKS, dm-crypt, cloud-provider encrypted disks) so that a disk image or backup leak does not expose webhook tokens.
- **Short-lived tokens**: use short-lived or rotatable webhook tokens where possible. Avoid long-lived static bearer tokens for push notification webhooks, since a database leak exposes every client's webhook credentials.
- **Future**: envelope encryption (AES-GCM with a config-supplied KEK) for credential columns is planned. HMAC body-signing (`X-A2A-Signature`) will replace static bearer tokens for webhook authentication, eliminating the need to store shared secrets. See `push.go` for the tracking TODO.

### Signing key

The bridge mints HS256 admin JWTs using the Hub's signing key. Anyone who reads this key (from the config file, state database, or process memory) can forge admin tokens for `hub.user`. Use a dedicated, minimally-privileged Hub user for the bridge — not a full admin account. Store the signing key via GCP Secret Manager (`hub.signing_key_secret`) rather than a plaintext file where possible.

### Broker plugin RPC (`plugin.allow_remote`)

By default the broker plugin RPC binds to loopback only. Setting `plugin.allow_remote: true` (e.g. to run the Hub and bridge in separate containers) opens the socket to the network with **no transport authentication** — anything that can dial the port can publish arbitrary messages as if they came from real agents. When using `allow_remote: true`, deploy the bridge behind a network-level mTLS boundary or firewall that restricts access to the Hub's IP only.

### `/metrics` endpoint

The `/metrics` endpoint requires the same API key authentication as other non-public endpoints. Prometheus scrapers must include the API key in their scrape configuration (e.g., via `authorization` or `headers` in the Prometheus job config). If this is undesirable, consider running a dedicated metrics listener on a separate port restricted to internal networks.

### Rate limiting and `trust_proxy`

When `rate_limit.trust_proxy: true` is set, the bridge takes the client IP from the leftmost `X-Forwarded-For` header value, which is fully attacker-controlled. Any client can rotate XFF values to bypass per-IP rate limits. Only enable `trust_proxy` when the bridge sits behind a trusted reverse proxy, and restrict network access to the bridge port so that only the proxy can reach it. A `trusted_proxies` CIDR allowlist is planned for a future release.

### Push notifications (webhooks)

Push notification URLs are validated against SSRF before use. The bridge:

- Rejects URLs that resolve to private, loopback, link-local, CGNAT, or reserved IP ranges (see `ValidatePushURL`).
- Applies a connect-time `Dialer.Control` callback (`ssrfSafeDialer`) that re-checks resolved IPs, defeating DNS rebinding attacks.
- Blocks HTTP redirects to prevent leaking `Authorization` headers to redirect targets.

Operators should additionally restrict outbound network access at the infrastructure level (e.g., firewall egress rules) as defense-in-depth.
