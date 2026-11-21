---
title: Sizing for Long-Lived Connections
description: Request-slot accounting and recommended concurrency, instance count, memory, and request-timeout settings for HA hosted Hubs.
---

On request-counted platforms such as Google Cloud Run, every open HTTP stream or upgraded WebSocket holds **one in-flight request slot** (`containerConcurrency`) for its entire lifetime. Long-lived connections — browser events streams (`/events`), interactive terminal sessions, agent port-forwarding tunnels, Conduit sessions, and standalone Runtime Broker control channels — share the same per-instance concurrency budget as short-lived REST API calls, static UI assets, and agent heartbeats.

When all slots on the live Hub replicas are occupied, the platform frontend rejects new requests with HTTP `429 Too Many Requests` before they reach Scion. Sizing an [HA hosted](/scion/hosted/ha/overview/) Hub therefore starts from **concurrent open connections**, not requests per second.

## The request-slot model

At steady state, the concurrent request slots held across all Hub replicas follow:

```text
request_slots ≈ (open_tabs × streams_per_tab)
              + pty_sessions
              + tunnels
              + broker_control_channels
              + inflight_requests
```

| Component | Endpoints | Slots held | Notes |
| :--- | :--- | :--- | :--- |
| **Browser events streams** | `GET /events` (Server-Sent Events) | `open_tabs × streams_per_tab` | Primary page stream (`1` per tab) plus view-specific auxiliary streams (see below). |
| **Terminal sessions (PTY)** | `GET /api/v1/agents/{id}/pty` (WebSocket) | `1` per attached terminal | One slot per open browser terminal pane or active `scion attach` CLI session. |
| **Agent & user tunnels** | `GET /api/v1/agents/{id}/ports/tunnel` or `GET /api/v1/conduit` (WebSocket) | `1` per running agent + `1` per CLI tunnel | Each running agent maintains one outbound connection for [Agent Port Forwarding](/scion/hosted/user/port-forwarding/) or [Conduit](/scion/reference/server-config/#conduit-serverhubconduit). Even with [Separate Conduit Relays](/scion/hosted/ha/separate-relay/) (`relay_mode: "external"`, `ingress: "proxy"`), the Hub proxies the upgraded WebSocket leg and holds a slot. Active `scion tunnel` / `scion ssh` sessions add `1` slot each. |
| **Runtime Broker control channels** | Runtime Broker WebSocket control channel | `1` per standalone Runtime Broker | Held by each connected standalone [Node-Bound Broker](/scion/glossary/) or [Proxy Broker](/scion/hosted/ha/runtime-broker/). Embedded Runtime Brokers (`--enable-runtime-broker`) run in-process unless configured to dial the external Hub URL. |
| **Short-lived in-flight requests** | `/api/v1/*`, static assets | Transient burst margin | Agent status heartbeats (`POST /api/v1/agents/{id}/status` every `30s`), Runtime Broker heartbeats (every `30s`), and user API calls hold a slot only while executing — except during cold-cluster agent launches or post-restart client resync bursts. |

### Streams per browser tab

How many `/events` streams an open web UI tab holds depends on the active view:

- **Dashboard, project, and Runtime Brokers views:** **1 stream** (the primary view stream).
- **Chat views (`/chat` and conversation threads):** **2 streams** while active — the primary view stream plus an `agent-feed` stream (`project.*.agent.>`), which closes after `60s` of inactivity. Legacy per-agent message streams (`messages/stream`) or open log/diagnostic viewers (`cloud-logs/stream`, `diagnostics/logs/stream`) add **1 stream** each while open.
- **Agent detail with the terminal workspace open:** **2 `/events` streams** (primary view stream plus a batched `terminal-metadata` stream, grouped up to an 1800-byte URL query budget) in addition to **1 PTY WebSocket** per attached terminal (**3 slots total** for one terminal tab).

**Navigation trails behind buffering proxies.** Subscriptions on `/events` are fixed in query parameters for the life of a connection, so SPA route navigation closes the current `EventSource` in the browser and opens a new one. Behind reverse proxies or auth proxies that do not immediately propagate client disconnects while periodic `30s` server heartbeats keep succeeding (observed behind Cloud Run's frontend with IAP), an abandoned stream can stay open on the Hub until it reaches its jittered [`max_connection_age`](/scion/reference/server-config/#events-streams-serverhubsse). Budget headroom for transient navigation trails when sizing concurrency and `max-instances`.

**Hidden tabs (`web.sse_hidden_tab_close`).** By default, background tabs keep their `/events` streams open. Enabling the [`web.sse_hidden_tab_close`](/scion/reference/experiments/) experiment (alpha, web-only, default `false`) closes a tab's view, agent-feed, and terminal-metadata `/events` streams after the tab has been hidden for **60 seconds**, dropping its `/events` slot usage to `0` until the user returns to the tab (at which point it reconnects and resyncs). Attached terminal PTY WebSockets stay open while hidden.

**Database pool independence.** Open `/events` streams do **not** consume database connection-pool slots from `server.database.max_open_conns`. On Postgres, each Hub replica uses a single dedicated `LISTEN` connection to fan out events in memory across all local subscribers (`64`-event channel buffer per stream). Database queries for an `/events` stream run only at connect time (and on membership changes) to expand wildcards and authorize subjects.

---

## Recommended settings pairing (Cloud Run)

Cloud Run's defaults (`--concurrency=80`, `--memory=512Mi`, `--timeout=300`) are tuned for short stateless RPCs, not long-lived SSE and WebSocket workloads. Pair the platform service flags and Scion's Hub settings together:

| Layer | Setting / Flag | Recommended | Default | Rationale |
| :--- | :--- | :--- | :--- | :--- |
| **Cloud Run** | `--concurrency` (`containerConcurrency`) | **`>= 250`** (up to `1000`) | `80` | Idle Go SSE handlers use ~`25`–`50 KiB` of heap and goroutine stack per stream (`128 KiB` test ceiling). Raising concurrency keeps idle streams and tunnels from exhausting slots while CPU is nearly idle. |
| **Cloud Run** | `--min-instances` / `--max-instances` | **`min >= 2`**, **`max` sized with headroom** (e.g. `>= 4`) | `0` / `100` | `min >= 2` maintains HA failover. Cloud Run targets ~`60%` concurrency utilization for scaling, and unsticky load balancing spreads long-lived connections unevenly across replicas. Keep `max_instances × database.max_open_conns` (plus event-pool headroom) within your Cloud SQL `max_connections` budget. |
| **Cloud Run** | `--memory` | **`>= 1Gi`** (`2Gi` at `concurrency >= 500`) | `512Mi` | Steady-state stream buffers are small, but higher concurrency concentrates connect-time wildcard expansion (`ListProjects` + batch authorization checks) and simultaneous REST list refetches when a replica restarts. |
| **Cloud Run** | `--timeout` (`timeoutSeconds`) | **`900`** (`15m`) to **`3600`** (`60m`) | `300` (`5m`) | Must cover cold GKE Autopilot node provisioning on first agent dispatch (`5`–`10m`) and sets the hard platform ceiling on every open stream and tunnel. |
| **`settings.yaml`** | [`server.hub.platform_request_timeout`](/scion/reference/server-config/#platform-request-timeout) | **Equal to `--timeout`** (e.g. `"900s"`) | Unset (`""`); valid `"150s"`–`"24h"` | Clamps both `/events` maximum age and Conduit `lifetime_cap` to `0.8 × platform_request_timeout` before ±20% jitter so the Hub closes connections cleanly before Cloud Run cuts them. |
| **`settings.yaml`** | [`server.hub.sse.max_connection_age`](/scion/reference/server-config/#events-streams-serverhubsse) | `"10m"` | `"10m"` (valid `"1m"`–`"24h"`) | Base age is `min(max_connection_age, 0.8 × platform_request_timeout)`, jittered **±20%** (`80%`–`120%`) per stream. With `"10m"` and `platform_request_timeout: "900s"`, streams send `event: reconnect` and close after **8 to 12 minutes** (`480s`–`720s`). |
| **`settings.yaml`** | [`server.hub.sse.heartbeat_interval`](/scion/reference/server-config/#events-streams-serverhubsse) | `"30s"` | `"30s"` (valid `"5s"`–`"1m"`) | Keeps proxies warm while staying below the web client's `75s` stale-stream threshold. Heartbeat-only agent and Runtime Broker status updates do not emit `/events` frames. |
| **`settings.yaml`** | [`server.hub.conduit.lifetime_cap`](/scion/reference/server-config/#conduit-serverhubconduit) | `"3500s"` (auto-clamped when `platform_request_timeout` is set) | `"3500s"` (valid `"90s"`–`"24h"`) | Lowered to `max(min(lifetime_cap, 0.8 × platform_request_timeout), 90s)` when `platform_request_timeout` is set, then jittered **±20%**. Sends `GoAway` `60s` before the cap and drains open streams within `30s`. |

### Example configuration

In `settings.yaml` (or via `SCION_SERVER_HUB_PLATFORMREQUESTTIMEOUT=900s`):

```yaml
server:
  hub:
    platform_request_timeout: "900s"   # match the Cloud Run --timeout value
    sse:
      max_connection_age: "10m"        # default; 8m-12m after +/-20% jitter
      heartbeat_interval: "30s"        # default; max "1m"
```

Paired with the Cloud Run service configuration (see [Deploy on GCP](/scion/hosted/ha/setup-gcp/#3d-deploy-the-hub-to-cloud-run) for full flags):

```bash
gcloud run services update scion-hub \
  --region=$REGION \
  --project=$PROJECT_ID \
  --concurrency=250 \
  --min-instances=2 \
  --max-instances=4 \
  --memory=1Gi \
  --timeout=900
```

### How `platform_request_timeout` prevents synchronized reconnect storms

When a long-lived connection runs into Cloud Run's hard `timeoutSeconds`, the platform cuts the HTTP response abruptly (`Truncated response body`). Connections opened together — for example when a fleet of agents starts or after a Hub replica rolls — then get cut at the same wall-clock second and reconnect in a single burst, triggering simultaneous `/events` authorization queries and UI list refetches (even with the browser client's `0`–`2s` first-reconnect jitter).

Setting `server.hub.platform_request_timeout` to match `--timeout` lets the Hub close connections gracefully before the platform limit:

1. **80% clamp (`0.8 × platform_request_timeout`):**
   - Effective `/events` base age = `min(sse.max_connection_age, 0.8 × platform_request_timeout)`.
   - Effective Conduit base cap = `max(min(conduit.lifetime_cap, 0.8 × platform_request_timeout), 90s)`.
2. **Per-connection ±20% jitter (`0.8×` to `1.2×` of the effective base):**
   - Because `0.8 × 1.2 = 0.96`, even the top of the jitter range never exceeds **96%** of `platform_request_timeout`.
   - With `--timeout=900` and `platform_request_timeout: "900s"`:
     - `0.8 × 900s = 720s` (`12m`).
     - `/events` streams use `min(10m, 12m) = 10m`, jittered across **`8m`–`12m` (`480s`–`720s`)**, ending cleanly with `event: reconnect`.
     - Conduit sessions (whose default `3500s` would otherwise exceed `900s`) are clamped to `720s` (`12m`) and jittered across **`9.6m`–`14.4m` (`576s`–`864s`)**, sending `GoAway` `60s` before each session's cap.
   - With Cloud Run's default `--timeout=300` and `platform_request_timeout: "300s"`:
     - `0.8 × 300s = 240s` (`4m`). Both `/events` streams (`10m` default) and Conduit sessions (`3500s` default) are clamped to `4m` and jittered across **`3.2m`–`4.8m` (`192s`–`288s`)**, staying below `300s`.

Both `server.hub.platform_request_timeout` and `server.hub.sse.*` are **Layer-0 startup settings**: changes require a Hub restart, invalid values fail startup preflight (`platform_request_timeout` must be between `"150s"` and `"24h"`), and the hosted server-config API refuses runtime edits to them. Separate relay processes (`scion server --role relay`) validate `server.hub.platform_request_timeout` with the same rules.

### Cloud Run startup warning

When the Hub starts on a Cloud Run service (`K_SERVICE` is set in the environment) and `server.hub.platform_request_timeout` (or `SCION_SERVER_HUB_PLATFORMREQUESTTIMEOUT`) is unset, preflight logs one `WARN` entry and continues startup:

```text
Running on Cloud Run without server.hub.platform_request_timeout: set it to the service's request timeout so the hub ends events streams and conduit sessions before Cloud Run cuts them. Each open events stream, terminal and tunnel holds one request slot (containerConcurrency) until it ends, so streams that run into the platform timeout hold slots and reconnect in bursts
```

The structured log record includes `setting="server.hub.platform_request_timeout"`, `env="SCION_SERVER_HUB_PLATFORMREQUESTTIMEOUT"`, `events_stream_max_age`, and `conduit_lifetime_cap` showing the unclamped durations in effect.

---

## Worked examples

### Example 1: 14 users (3 tabs each) and 20 agents with tunnels

Consider a team of **14 active web UI users**, each keeping **3 browser tabs** open (`42` tabs total), alongside **20 running agents** with port-forwarding tunnels or Conduit sessions, **4 attached terminal sessions**, and **1 embedded Runtime Broker**:

1. **Browser `/events` streams:**
   - Suppose across the 42 tabs, 14 are on project/dashboard pages (`14 × 1 = 14`), 14 are on active chat views (`14 × 2 = 28`), and 14 are on agent detail pages (`14 × 1 = 14`), with 4 of those tabs holding the terminal workspace open (`4 × 1 = 4` `terminal-metadata` streams): **`60` stream slots** (or `42` slots if every tab is on a single-stream view).
2. **Terminal PTY sessions:** **`4` slots** (`4 × 1` WebSocket).
3. **Agent tunnels:** **`20` slots** (`20 × 1` outbound tunnel/Conduit WebSocket).
4. **Short-lived API & heartbeat burst margin:** **`~12` slots**.

- **Steady-state slots:** `60 + 4 + 20 + 12 =` **`96` concurrent slots** (`~78` slots on single-stream views).
- **With navigation trails behind a buffering proxy:** if active navigation leaves `2`–`3` prior `/events` streams per active user lingering until `max_connection_age` (`8`–`12m`), transient demand climbs to **`125`–`140` concurrent slots**.

**Why Cloud Run defaults (`--concurrency=80 --max-instances=2 --memory=512Mi`) fail:**
The nominal ceiling is `2 × 80 = 160` slots, but without sticky affinity long-lived streams land unevenly across the two instances (for example `80` on replica A and `55` on replica B). Replica A hits its `80`-slot cap and Cloud Run begins returning HTTP `429` even though total in-flight requests are below `160`. Meanwhile, if `--timeout=900` is used without `server.hub.platform_request_timeout: "900s"`, all 20 agent Conduit sessions (`3500s` default) hit the `900s` hard cut.

**Recommended pairing for this workload:**
- `--concurrency=250`, `--min-instances=2`, `--max-instances=4`, `--memory=1Gi`, `--timeout=900`
- `server.hub.platform_request_timeout: "900s"`
- **Result:** The 2 minimum replicas alone provide **`500` slots** (`~300` slots at Cloud Run's ~`60%` autoscaling target before a third replica even starts, and **`1000` slots** across all 4 replicas). Losing or rolling one replica leaves `250`+ slots immediately available on the peer with zero `429`s. Enabling [`web.sse_hidden_tab_close`](/scion/reference/experiments/) further drops steady-state `/events` slots from background tabs after `60s`.

### Example 2: 50 users (4 tabs each), 120 agents, 15 terminals, and 3 standalone Runtime Brokers

Consider a larger multi-project Hub with **50 UI users** (`200` open tabs total), **120 running agents**, **15 attached terminals**, and **3 standalone Proxy/Node-Bound Runtime Brokers**:

1. **Browser `/events` streams:**
   - Without `web.sse_hidden_tab_close` (~`1.4` streams/tab across mixed views): `200 × 1.4 =` **`280` slots**, plus **`~70` slots** of navigation/reconnect headroom = **`350` slots**.
   - With `web.sse_hidden_tab_close` enabled (assuming ~`1.5` tabs per user visible within the last `60s`): `75 × 1.4 ≈` **`105` slots**, plus **`~35` slots** headroom = **`140` slots**.
2. **Terminal PTY sessions:** **`15` slots**.
3. **Agent tunnels / Conduit sessions:** **`120` slots**.
4. **Standalone Runtime Broker control channels:** **`3` slots**.
5. **Short-lived API & resync burst margin:** **`~35` slots**.

- **Peak slots:** **`~523` slots** (default hidden-tab behavior) or **`~313` slots** (with `web.sse_hidden_tab_close`).

**Recommended pairing for this workload:**
- `--concurrency=500`, `--min-instances=2`, `--max-instances=6`, `--cpu=2`, `--memory=2Gi`, `--timeout=900`
- `server.hub.platform_request_timeout: "900s"`
- **Result:** At `--concurrency=500`, Cloud Run's ~`60%` utilization target is `300` slots per replica, so **`2`–`3` active replicas** serve the peak load while `max-instances=6` (`3000` slots ceiling) absorbs rolling deploys and instance replacements.
- **Database connection check:** Verify that `max_instances × server.database.max_open_conns` (default `5` on Postgres; `10` in the [Terraform HA module](/scion/hosted/ha/terraform/)), plus each replica's Postgres event pool and any co-deployed services, stays comfortably below your Cloud SQL `max_connections` (for example `200`).

---

## Verifying capacity in production

Import the [Hub Monitoring Dashboard](/scion/hosted/single-node/hub-monitoring-dashboard/) and compare Scion's per-replica SSE metrics with Cloud Run's container metrics:

- **`scion.hub.sse.streams` and `scion.hub.sse.users`** (summed across `service_instance_id`, or inspected per replica): `streams / users` measures live `/events` slots per active user.
- **`scion.hub.sse.closes` and `scion.hub.sse.stream_duration` by `reason`:** healthy bounded streams close primarily with `reason="client"` and `reason="max_age"` spread across `8`–`12` minutes (`max_age_ms` in the `events stream closed` Info log line), rather than clustering right at `--timeout`.
- **`run.googleapis.com/container/max_request_concurrencies` and Cloud Run request logs (`status=429`):** confirm peak per-instance concurrency stays well below `--concurrency` and `429` responses remain zero during peak navigation and deploys.
