---
title: Port Forwarding & Auto-Expose
description: Learn how to access HTTP services running inside agent containers securely via the Scion Hub reverse proxy.
---

Scion's **port forwarding** feature allows users, developers, and integrations to access HTTP services running inside agent containers—such as web servers, developer APIs, or debugging interfaces—securely through the Hub's reverse proxy. 

Instead of opening container ports to the public internet or managing complex VPNs/ingress rules, Scion establishes a secure WebSocket-based reverse tunnel between the agent container and the Hub, routing incoming traffic on-demand.

---

## How It Works

Port forwarding in Scion relies on a **reverse tunnel architecture**:

1. **Tunnel Establishment**: When an agent container boots, `sciontool` (the in-container agent helper) establishes a persistent WebSocket connection to the Hub's tunnel registration endpoint:
   ```
   GET /api/v1/agents/{agentID}/ports/tunnel
   ```
   This connection is authenticated using the agent's unique `X-Scion-Agent-Token`.
2. **Port Registration**: A port must be registered with the Hub before it can receive forwarded traffic. Ports can be registered manually via the CLI/API or automatically detected via **Auto-Expose**.
3. **Request Proxying**: When a user or system sends an HTTP request to the Hub proxy URL for a specific agent port, the Hub encapsulates the request headers, method, path, query parameters, and body into a multiplexed control message.
4. **Local Forwarding**: The Hub sends this message over the WebSocket tunnel to the agent's in-container tunnel manager. The manager unwraps the request and makes a standard HTTP request to the local loopback address (`127.0.0.1` or `localhost`) on the specified port.
5. **Response Delivery**: The local service's response is streamed back over the WebSocket tunnel, reconstructed by the Hub, and returned to the caller.

```d2
direction: lr
classes: {
  box: {
    style: {
      fill: "#f8f9fa"
      stroke: "#cbd5e1"
      stroke-width: 1
    }
  }
}

user: "User / Browser" {
  shape: person
}

hub: "Scion Hub (Reverse Proxy)" {
  class: box
}

agent: "Agent Container" {
  class: box
  sciontool: "sciontool Tunnel Manager" {
    style.fill: "#e0f2fe"
  }
  service: "Local Service\n(e.g., Python Web Server on :8000)" {
    style.fill: "#fef08a"
  }
}

user -> hub: "1. GET /api/v1/.../ports/8000/proxy/"
hub <-> agent.sciontool: "2. Secure WebSocket Reverse Tunnel"
agent.sciontool <-> agent.service: "3. Local Loopback request\n(127.0.0.1:8000)"
hub -> user: "4. HTTP Response"
```

---

## URL Patterns & Proxy Endpoints

Every exposed port on an agent is allocated a dedicated base path under the Hub's API. You can access the service using the following pattern:

```
https://<hub-url>/api/v1/agents/<agent-id>/ports/<port>/proxy/<subpath>
```

For example, if your Hub is hosted at `hub.scion.local`, the agent ID is `agent-abc-123`, and you want to access a service listening on port `8000`, the base proxy URL is:

```
https://hub.scion.local/api/v1/agents/agent-abc-123/ports/8000/proxy/
```

Any subpath or query parameters appended to the proxy path are forwarded faithfully to the container. For instance, accessing:
`https://hub.scion.local/api/v1/agents/agent-abc-123/ports/8000/proxy/api/v1/metrics?raw=true`
will forward a local request to `http://127.0.0.1:8000/api/v1/metrics?raw=true`.

---

## Manual Port Management

You can manually manage exposed ports using the `sciontool` CLI from inside the agent container, or via the Hub API.

### Exposing a Port
To expose a local port manually, run `sciontool expose` with the target port:

```bash
sciontool expose 8080 --label "my-web-app"
```

Options:
- `--label`: An optional label to identify the service.
- `--host`: The local host to forward to (defaults to `127.0.0.1`). Target hosts are strictly validated and **must resolve to a loopback address** in this revision.

### Unexposing a Port
To stop exposing a port, run:

```bash
sciontool unexpose 8080
```
This operation is idempotent; if the port is not currently exposed, the command completes successfully and silently.

### Listing Exposed Ports
To list all currently exposed ports for the agent, run:

```bash
sciontool expose --list
```
This outputs the current port number, label, and full proxy URL.

---

## Auto-Expose Ports

Scion includes an **Auto-Expose** engine that can automatically detect listening TCP services inside the agent container and register them with the Hub without manual developer intervention.

### How Auto-Detection Works
1. **Procfs Scanning**: The in-container agent helper (`sciontool`) runs a periodic reconciliation loop (by default every 3 seconds) that reads `/proc/net/tcp` and `/proc/net/tcp6` in pure Go.
2. **State Filtering**: It identifies all sockets in the `TCP_LISTEN` state (hex code `0A`), resolving their local IP addresses and port numbers.
3. **Policy Evaluation**: The scanned ports are filtered against configured minimums, allowlists, or denylists.
4. **Hub Registration**: Any eligible newly discovered ports are registered with the Hub using the label **`auto-scan`** (allowing operators and users to distinguish them from manually registered ports).
5. **Reconciliation & Unexposure**: If a port was auto-exposed but the service subsequently stops listening (is no longer found in `procfs` scans), `sciontool` automatically deregisters and unexposes the port from the Hub.
6. **System Notifications**: When a port is auto-exposed, the reconciler optionally sends a platform event message to the agent channel with category `system_category: agent:port:forward`, generating a notification. This notification suggests sharing the proxy URL with collaborating users so they can collaborate on the exposed service.

:::note[Manual Exclude]
Auto-expose will never unexpose or overwrite a port that was manually registered (without the `auto-scan` label). It only manages and cleans up the ports that it registered itself.
:::

### Configuring Auto-Expose on the Agent
Auto-expose behavior inside the agent container is governed by several environment variables:

| Environment Variable | Default Value | Description |
| :--- | :--- | :--- |
| `SCION_AUTO_EXPOSE_PORTS` | `false` | Set to `true` or `1` to enable the auto-expose scanner loop. |
| `SCION_AUTO_EXPOSE_INTERVAL` | `3s` | The scanning and reconciliation cycle interval (minimum `1s`). |
| `SCION_AUTO_EXPOSE_MODE` | `allowlist` | The filtering strategy. Allowed values: `allowlist` or `denylist`. |
| `SCION_AUTO_EXPOSE_PORTS_LIST` | *empty* | A comma-separated list of ports. In `allowlist` mode, only these ports are exposed. In `denylist` mode, these ports are excluded. |
| `SCION_AUTO_EXPOSE_MIN_PORT` | `1024` | The minimum port number eligible for auto-exposure (prevents exposing privileged system ports). |

**Example Allowlist Configuration:**
```bash
SCION_AUTO_EXPOSE_PORTS=true
SCION_AUTO_EXPOSE_MODE=allowlist
SCION_AUTO_EXPOSE_PORTS_LIST=8000,8080,3000
```

---

## Settings & Administration

Administrators can control whether port forwarding and auto-expose are permitted at both the global Hub level and individual Project level.

### Global Server Configuration
The global default for new agents is configured via `settings.yaml` under the operational (Layer-1) settings hierarchy:

```yaml
# settings.yaml
auto_expose_ports:
  enabled: true
```

* **File Mode**: Editable directly in the global config file.
* **Database Mode**: Managed via the Hub Admin Settings API (`PUT /api/v1/admin/server-config`) or UI.
* **Seeds**: Can be seeded initially using the `SCION_SEED_AUTO_EXPOSE_PORTS_ENABLED` environment variable.

### Project-Level Overrides
Project owners and admins can control the auto-expose feature for all agents within a specific project using project annotations:

```yaml
# Project Settings / Metadata Annotations
scion.io/auto-expose-ports-enabled: "true"
```

If set to `true`, the Hub automatically injects `SCION_AUTO_EXPOSE_PORTS=true` into the environment of any new agent container started under that Project, unless the agent configuration explicitly defines it otherwise (agent-level settings take precedence).

---

## Security & Network Isolation

Port forwarding is designed with multi-tenant security in mind, utilizing strict host validation, access control lists, and infrastructure port protections.

### 1. Target Validation & Host Isolation
- **Loopback Enforcement**: Forwarding is strictly limited to services bound to the container's **loopback interface** (`127.0.0.1`, `localhost`, `::1`). Requests targeting external or network-routable IPs are rejected with an error.
- **Max Port Limit**: Each agent container is permitted a maximum of **10** simultaneously exposed ports. Requests to expose additional ports are rejected with a validation error.

### 2. Infrastructure Reserved Ports (Denylist)
Certain infrastructure and internal control-plane ports are reserved for system services and are permanently banned from manual exposure or auto-exposure:

| Port | Reserved Service | Reason |
| :--- | :--- | :--- |
| `9810` | Scion Hub API | Protects the Hub's local control API from external exposure. |
| `18380` | Scion Metadata Server | Protects the agent's internal instance identity metadata server. |

*Note: Port `8080` is intentionally permitted. Because the reverse tunnel operates over an established connection, the agent-side port never collides with the Hub's HTTP listeners, making it safe to expose user services on `8080`.*

### 3. Authentication & Authorization
Access to the proxy and port registration APIs requires authentication, verified under the unified permission policy:

* **Managing Ports (Register/Delete)**:
  * Only the agent container itself (authenticating with its token containing `ScopeAgentPortForward`) or a global **Hub Admin** user is authorized to register or delete exposed ports.
* **Accessing Proxied Ports**:
  * An agent can only access its own port registrations.
  * A user must be authenticated and must hold the **`ActionPortAccess`** (or `ActionRead`) permission for that specific agent. Unauthorized users are blocked with an HTTP `403 Forbidden` response.

---

## Browser vs. Non-Browser Error Handling

When an unexposed port is accessed, or when the agent container's reverse tunnel is temporarily offline, the Hub reverse proxy performs smart **content negotiation** based on the request's `Accept` header:

### Browser Requests (`Accept: text/html`)
If the request is initiated from a web browser, the proxy returns a friendly, self-contained **HTML error page** containing troubleshooting guidance:

* **Port Not Exposed**: Returns an `HTTP 404 Not Found` page stating that the requested port has not been registered on this agent.
* **Tunnel Offline**: Returns an `HTTP 503 Service Unavailable` page indicating that the reverse tunnel is disconnected, and the agent may not be running or is starting up.

### API / Programmatic Requests (Other headers)
For CLI, script, or webhook requests, the proxy bypasses the HTML template and returns standard, machine-readable **JSON error responses**:

```json
{
  "code": "runtime_error",
  "message": "No active port-forward tunnel for this agent"
}
```
This ensures that API integrations are easy to write and parse, while humans accessing the endpoint in a browser get clear visual diagnostics.
