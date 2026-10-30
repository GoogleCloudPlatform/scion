---
title: Port Forwarding & Auto-Expose
description: Learn how to access HTTP services running inside agent containers securely via the Scion Hub reverse proxy.
---

Scion's **port forwarding** feature allows users, developers, and integrations to access HTTP services running inside agent containers—such as web servers, developer APIs, or debugging interfaces—securely through the Hub's reverse proxy. 

Instead of opening container ports to the public internet or managing complex VPNs/ingress rules, Scion keeps a secure outbound WebSocket connection from the agent container to the Hub and routes incoming traffic over it on demand.

---

## How It Works

Port forwarding in Scion relies on an **outbound WebSocket connection** from the container:

1. **Connection Establishment**: When an agent container boots, `sciontool` (the in-container agent helper) opens a persistent outbound WebSocket connection to the Hub's port-forward endpoint (the route keeps its legacy name):
   ```
   GET /api/v1/agents/{agentID}/ports/tunnel
   ```
   This connection is authenticated using the agent's unique `X-Scion-Agent-Token`.
2. **Port Registration**: A port must be registered with the Hub before it can receive forwarded traffic. Ports can be registered manually via the CLI/API or automatically detected via **Auto-Expose**.
3. **Request Proxying**: When a user or system sends an HTTP request to the Hub proxy URL for a specific agent port, the Hub encapsulates the request headers, method, path, query parameters, and body into a multiplexed control message.
4. **Local Forwarding**: The Hub sends this message over the WebSocket connection to the agent's in-container port-forward manager. The manager unwraps the request and makes a standard HTTP request to the local loopback address (`127.0.0.1` or `localhost`) on the specified port.
5. **Response Delivery**: The local service's response is streamed back over the WebSocket connection, reconstructed by the Hub, and returned to the caller.

:::note[Conduit transport (experimental)]
When the `hub.conduit` [experiment](/scion/reference/experiments/) is on and the Hub runs the conduit relay (see [Conduit settings](/scion/reference/server-config/#conduit-serverhubconduit)), `sciontool` also connects to the relay at `GET /api/v1/conduit`, reconnecting with backoff. The Hub then proxies requests for loopback ports over the agent's conduit session, and admits only a session from the agent's current run. An agent without a conduit session, such as one running an older `sciontool`, still uses the WebSocket connection described above. If the agent has neither, the request fails with `503` and error code `agent_offline`. A TCP stream over the conduit session connects to `127.0.0.1:<port>` inside the container and, only if that connection is refused, tries `[::1]:<port>` once, so a service listening only on the IPv6 loopback address is reachable too. With the experiment off, nothing changes.
:::

:::note[Proxied responses are sandboxed]
Agent port responses are served from the Hub's own origin, so the Hub treats them as untrusted content. Every proxied response carries the Hub's sandbox `Content-Security-Policy` (`sandbox allow-scripts allow-popups allow-popups-to-escape-sandbox allow-downloads allow-modals allow-forms allow-pointer-lock allow-orientation-lock allow-presentation; frame-ancestors 'none'`), a `Permissions-Policy` that turns off camera, microphone, location, screen capture, motion sensors, USB/serial/HID/Bluetooth devices, payments, passkeys and clipboard reads, and `X-Content-Type-Options: nosniff`, none of which the agent's service can override. Exposed agent pages can't be embedded in other pages or sites. The Hub never relays the service's `Set-Cookie`, `Content-Security-Policy`, `X-Content-Type-Options`, `Clear-Site-Data`, or CORS response headers. The service's `X-Frame-Options`, `X-XSS-Protection`, `Referrer-Policy` and `Permissions-Policy` headers are not relayed either; when the web frontend serves the Hub, the Hub's own values for the first three apply, and the sandbox `Permissions-Policy` above replaces the Hub's page one. A forwarded web app therefore runs in an opaque origin: it cannot set cookies or use storage on the Hub origin, and apps that rely on cookies, `localStorage`, or same-origin requests may not work through the proxy. Forms, `print()` and `alert`/`confirm`/`prompt` dialogs, pointer lock, screen-orientation lock and the Presentation API work, and copying to the clipboard is allowed. None of them reaches past the app's opaque origin. A `GET` form works like following a link, so search and filter boxes work. A `POST` form the app submits, to the Hub or back to the app through the proxy, arrives without the Hub session, so the Hub asks the viewer to sign in instead of passing it on.
:::

```d2
direction: right
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
  sciontool: "sciontool Port-Forward Manager" {
    style.fill: "#e0f2fe"
  }
  service: "Local Service\n(e.g., Python Web Server on :8000)" {
    style.fill: "#fef08a"
  }
}

user -> hub: "1. GET /api/v1/.../ports/8000/proxy/"
hub <-> agent.sciontool: "2. Outbound WebSocket Connection"
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

Open the base URL with its trailing slash. A `GET` or `HEAD` of the URL without it (`.../ports/8000/proxy`) is redirected with `308 Permanent Redirect` to `.../ports/8000/proxy/`, query string included, so that the page's relative links resolve under the proxy path. Other methods on the URL without the slash are forwarded as before. WebSocket upgrades to the URL without the slash are not redirected.

---

## Multi-file apps

A proxy URL opened in a browser serves **one self-contained page** well. The page itself loads, but the requests that page makes back to the app are not served: separate scripts (including module scripts), stylesheets, images, `fetch()` and XHR calls, and WebSockets. Each of those requests gets `401 Unauthorized` with a short plain-text explanation, which you can see in the browser's developer tools (Network tab):

```
HTTP/1.1 401 Unauthorized
Content-Type: text/plain; charset=utf-8
X-Scion-Proxy-Error: no-credentials
X-Scion-Proxy-Hint: sandboxed-subresource
```

`X-Scion-Proxy-Hint: sandboxed-subresource` is present when the request came from a page opened through the proxy. Other requests to the link that are not page loads get the same plain-text answer. Opening a proxy URL in a browser tab without being signed in still takes you to the sign-in page.

A request made with your own credential, for example with `curl` and an agent token or a [personal access token](/scion/hosted/user/personal-access-tokens/), is served for any path. That shows the app answers, not that the page works for a person who opens the link; check the page in a browser.

### What works today

| App | Through the proxy URL in a browser |
| :--- | :--- |
| One HTML file with its CSS and JavaScript inline and images as `data:` URLs | Works |
| An HTML page that loads separate `.js`, `.css` or image files | The page loads; those files do not |
| A page that calls back to its app with `fetch()`, XHR, `EventSource` or a WebSocket | Those calls fail |
| Dev servers with live reload (`vite dev`, `next dev`) | Do not work |
| Path-based client-side routing (for example React Router's `BrowserRouter`, Vue Router's `createWebHistory`) | Does not work; use hash routing (`HashRouter`, `createWebHashHistory`) or no router |

### Single-file fallback

To share a working page through the proxy URL, build it as one self-contained HTML file:

- Put the styles in a `<style>` element and the scripts in `<script>` elements, and turn images into `data:` URLs.
- If the app has a client-side router, use hash routing (URLs like `#/settings`), not path-based routing.
- With Vite, [`vite-plugin-singlefile`](https://github.com/richardtallent/vite-plugin-singlefile) produces such a file from a normal build.
- Serve the file from any static server on the agent's loopback address, for example `python3 -m http.server 8000 --bind 127.0.0.1`, and expose the port.

The page cannot load data from the app after it opens, so include everything it needs at build time.

### Full fidelity: `scion tunnel`

For a multi-file app, an app that talks to its own backend, or a dev server with live reload, use [`scion tunnel`](#tunnels-from-your-machine-scion-tunnel) from your machine. It needs the `scion` CLI and the `hub.conduit` experiment:

```bash
scion tunnel my-agent --port 5173 --local-port 5173
```

Then open `http://127.0.0.1:5173/` in your browser. The app behaves as it does on its own machine.

### Behind an authenticating front proxy

When the Hub runs behind an authenticating front proxy, for example Google IAP or another [proxy-auth](/scion/hosted/ha/auth-proxy-iap/) deployment (`auth.mode: proxy`), the front proxy sits in front of every request, including the requests a page opened through the proxy URL makes back to its app. Depending on how the front proxy is configured, some of those requests may be answered by the front proxy's own sign-in flow instead of by the app, so a multi-file page can behave unpredictably: the page opens, and some of its parts load while others do not.

For anything beyond a single self-contained page in such a deployment, use [`scion tunnel`](#tunnels-from-your-machine-scion-tunnel), which does not go through the browser and is not affected.

With the IAP provider, apps served through an agent's port do not receive the IAP assertion or the IAP user email and ID headers. With the JWT provider, they do not receive the configured assertion header. The Hub reads only that header, so configure the front proxy not to add other user identity headers, such as email or ID headers.

---

## Manual Port Management

You can manually manage exposed ports using the `scion` CLI from inside the agent container, or via the Hub API. The commands act as the calling agent, using its own Hub identity, so they only work inside an agent container with a Hub connection.

:::note
These commands were previously `sciontool expose` and `sciontool unexpose`. The `sciontool` forms now print a message pointing to `scion expose` and `scion unexpose` and exit with an error.
:::

### Exposing a Port
To expose a local port manually, run `scion expose` with the target port:

```bash
scion expose 8080 --label "my-web-app"
```

Options:
- `--label`: An optional label to identify the service.
- `--host`: The local host to forward to (defaults to `127.0.0.1`). Target hosts are strictly validated and **must resolve to a loopback address** in this revision.

### Unexposing a Port
To stop exposing a port, run:

```bash
scion unexpose 8080
```
This operation is idempotent; if the port is not currently exposed, the command completes successfully.

### Listing Exposed Ports
To list all currently exposed ports for the agent, run:

```bash
scion expose --list
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

## Tunnels from your machine (`scion tunnel`)

With the `hub.conduit` [experiment](/scion/reference/experiments/) on, `scion tunnel` connects a local port, or stdin and stdout, to an agent through the Hub. Unlike the HTTP proxy URLs above, a tunnel carries raw TCP, so it works for any protocol (WebSockets, database clients, debuggers) and for SSH.

```bash
# Forward a local port (random, printed on stderr) to port 5173 inside the agent
scion tunnel my-agent --port 5173

# Pick the local port
scion tunnel my-agent --port 5173 --local-port 8080

# Show the tunnels your running scion tunnel processes hold
scion tunnel status
```

The local listener binds to `127.0.0.1` only. Each accepted connection gets its own tunnel. The listener runs until you stop the command (Ctrl-C, exit code 130) or the session to the Hub is lost (exit code 6). It also stops when the Hub refuses a new tunnel in a way no later connection can fix: an expired or refused credential (3), an agent that was deleted (4), access that was withdrawn (5), or a Hub without tunnels (11). After such a refusal the listener closes its port, so new connections are refused. Connections that are already open continue until they end, and the command then exits with that code. If the agent is stopped, the listener keeps listening. Each new connection is closed, with the line `tunnel request failed: the agent is not running (agent_not_running)` on stderr, and connections are served again once the agent is started.

**How it works.** The CLI opens one Conduit session to the Hub (`GET /api/v1/conduit`, with your usual Hub credential) and asks for each tunnel over it. The Hub checks each tunnel on its own and connects it to the agent's own Conduit session, so the agent's ports are never opened to the network. A `scion tunnel` process targets one agent; its session carries one tunnel per accepted local connection, up to the session's limit. Closing a tunnel closes only that tunnel.

**Who may open a tunnel.** A TCP tunnel needs `agent.port_access` on the agent, and the target must be an agent-local port you may reach: an exposed port or one the operator allow-listed, never [the reserved ports](#2-infrastructure-reserved-ports-denylist). An SSH tunnel needs `agent.attach` and `agent.port_access`. The agent must be running and have a Conduit session; otherwise the command exits with a specific code (see [`scion tunnel`](/scion/reference/cli/#scion-tunnel)). Open tunnels are re-checked like other Conduit user streams (see [`authz_recheck_interval` and `stream_authz_max`](/scion/reference/server-config/#conduit-serverhubconduit)) and close when your access is withdrawn.

### SSH into an agent

`sciontool` in the agent runs an embedded SSH server that is reachable only through a Conduit tunnel; it never listens on a network port. It offers a shell, `exec`, `sftp` and forwarding to the agent's loopback addresses (except [the reserved ports](#2-infrastructure-reserved-ports-denylist)) and Unix sockets. Unlike a TCP tunnel, SSH forwarding does not require the port to be exposed or allow-listed. Remote forwarding, agent forwarding and X11 are refused. Every session runs as the container user, whatever user name you give. The Hub authorizes the tunnel, so the SSH connection itself needs no key.

The simplest way in is `scion ssh`, which runs your system `ssh` through the tunnel with the agent's host key taken from the Hub:

```bash
scion ssh my-agent
```

To use plain `ssh`, `scp` or an editor's Remote-SSH, install Host blocks for your agents with `scion ssh-config --write`; each agent is then reachable as `scion.<hub-label>.<project>.<agent>`, where `<hub-label>` is derived from the Hub's host name (not its hub-list nickname). See [`scion ssh`](/scion/reference/cli/#scion-ssh) and [`scion ssh-config`](/scion/reference/cli/#scion-ssh-config).

You can also use `scion tunnel --ssh --stdio` as an ssh `ProxyCommand` directly:

```bash
ssh -o ProxyCommand="scion tunnel --stdio --ssh --hub https://hub.example.com --project my-project my-agent" scion@my-agent
```

Each agent run generates its own SSH host key. The agent reports it to the Hub, which returns it as `sshHostKey` in the agent's details (`GET /api/v1/agents/:id`), so you can pin it in `known_hosts`.

---

## Settings & Administration

Administrators can control whether port forwarding and auto-expose are permitted at both the global Hub level and individual Project level.

### Which Setting Wins
`SCION_AUTO_EXPOSE_PORTS` can come from four places. Highest first:

1. **The agent itself**: the agent-create request's `config.env`, or the auto-expose control on the agent's Edit page. This is recorded as the agent's explicit choice and survives reincarnate.
2. **The project annotation** `scion.io/auto-expose-ports-enabled` (see below).
3. **Harness-config env.** A template cannot set `SCION_*` keys.
4. **The Hub default** `auto_expose_ports.enabled` (see below).

A lower source applies only when no higher one sets the variable. The Hub default is not stored on the agent: it is sent on every start, so changing it affects every agent that inherits it at that agent's next start. A value set with `scion hub env set` outranks harness-config env but loses to the agent and project settings. An agent started from the `scion` CLI directly on a Runtime Broker host, rather than through the Hub, does not receive the Hub default. See [Settings Precedence](/scion/reference/settings-precedence/#a6-scion_auto_expose_ports-has-its-own-four-tier-order) for details.

### Global Server Configuration
The Hub-wide default, the lowest tier above, is configured via `settings.yaml` under the operational (Layer-1) settings hierarchy:

```yaml
# settings.yaml
auto_expose_ports:
  enabled: true
```

* **File Mode**: Editable directly in the global config file.
* **Database Mode**: Managed via the Hub Admin Settings API (`PUT /api/v1/admin/server-config`) or UI.
* **Seeds**: Seeded initially from `auto_expose_ports.enabled` in `settings.yaml`, or from `SCION_SEED_AUTOEXPOSEPORTS_ENABLED` when the file does not set it.

### Project-Level Overrides
Project owners and admins can control the auto-expose feature for all agents within a specific project using project annotations:

```yaml
# Project Settings / Metadata Annotations
scion.io/auto-expose-ports-enabled: "true"
```

When set, the Hub gives every agent in that Project `SCION_AUTO_EXPOSE_PORTS` with the annotation's value. It overrides harness-config and Hub-default values, but not a value set on the agent itself. The value is applied when the agent is created and re-read when it is reincarnated.

### Precedence

The `SCION_AUTO_EXPOSE_*` variables resolve the same way when an agent is created and when its configuration is changed later (`PATCH`): a value set explicitly on the agent wins, then the project value, then harness-config env, then the Hub default. A configuration change that does not mention the auto-expose keys leaves the explicit value alone and re-derives the inherited one.

In the web UI, the agent **Edit** page shows the effective auto-expose value and where it comes from (set on the agent, project, or Hub default; the page's source labels still mention a template, which can no longer set this key). A value set on the agent is shown as the control's value; an inherited one is shown as grey placeholder text, so picking it sets it on the agent. The page saves the auto-expose keys only when you change the control. On the create page, the control starts at the Hub default; if you leave it untouched, the agent inherits the project and Hub default values.

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

*Note: Port `8080` is intentionally permitted. Because traffic travels over the container's established outbound WebSocket connection, the agent-side port never collides with the Hub's HTTP listeners, making it safe to expose user services on `8080`.*

### 3. Authentication & Authorization
Access to the proxy and port registration APIs requires authentication, verified under the unified permission policy:

* **Managing Ports (Register/Delete/Clear)**:
  * The agent container itself (authenticating with its token containing `ScopeAgentPortForward`) may register or delete its own exposed ports.
  * A user may register, delete or clear an agent's exposed ports when they hold `agent.port_access` on that agent (the same check as accessing its proxied ports) and `agent.port_access` at hub level (the super-admin role, or a custom system-scope role that grants it). The `agent.port_access` that the project owner and admin roles carry within a project opens ports but does not manage them.
  * A [user access token](/scion/hosted/user/personal-access-tokens/) needs the `agent:port_access` scope and a boundary that contains the agent's project, and its holder needs `agent.port_access` at hub level. All three are required.
  * Only the agent itself opens its port tunnel (`/api/v1/agents/{id}/ports/tunnel`); every user is refused.
* **Accessing Proxied Ports**:
  * An agent can only access its own port registrations.
  * A user must be authenticated and must hold the **`ActionPortAccess`** (or `ActionRead`) permission for that specific agent. Unauthorized users are blocked with an HTTP `403 Forbidden` response.
  * When authenticating with a [user access token](/scion/hosted/user/personal-access-tokens/), the
    token must have the `agent:port_access` scope selected, and the holder must currently have
    access to that specific agent — your own agents and their descendants, or any agent in a
    project where your role grants `agent.port_access` (the built-in `project-owner` and
    `project-admin` roles do). Selecting the scope at
    mint time is not by itself access to any agent; it is re-checked on every request.

---

## Browser vs. Non-Browser Error Handling

Requests to a proxy URL that carry no credential and are not a page navigation (a page's scripts, stylesheets, images and `fetch()` calls) get the plain-text `401` described in [Multi-file apps](#multi-file-apps), instead of the HTML page or the JSON error described below.

When an unexposed port is accessed, or when the agent container's WebSocket connection is temporarily offline, the Hub reverse proxy performs smart **content negotiation** based on the request's `Accept` header:

### Browser Requests (`Accept: text/html`)
If the request is initiated from a web browser, the proxy returns a friendly, self-contained **HTML error page** containing troubleshooting guidance:

* **Port Not Exposed**: Returns an `HTTP 404 Not Found` page stating that the requested port has not been registered on this agent.
* **Connection Offline**: Returns an `HTTP 503 Service Unavailable` page indicating that the agent's WebSocket connection is disconnected, and the agent may not be running or is starting up.

### API / Programmatic Requests (Other headers)
For CLI, script, or webhook requests, the proxy bypasses the HTML template and returns standard, machine-readable **JSON error responses**:

```json
{
  "code": "runtime_error",
  "message": "No active port-forward tunnel for this agent"
}
```
This ensures that API integrations are easy to write and parse, while humans accessing the endpoint in a browser get clear visual diagnostics.
