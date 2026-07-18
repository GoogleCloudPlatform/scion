---
title: Runtime Broker
description: How a Hub user can register their local machine as a compute resource for your team's Scion Hub.
---

A **Runtime Broker** is the component of Scion that actually runs agents (containers or VMs). While a centralized **Scion Hub** manages metadata and agent configurations, you can register your own machine as a Runtime Broker to execute agents locally while still participating in your team's Hub environment.

This is especially useful if you need agents to access local resources (like an intranet database, local files, or specialized hardware) or if you want to contribute compute power to your team's projects.

## Architecture

When you run a Runtime Broker connected to a Hub, your machine establishes a persistent WebSocket connection (a "Control Channel") to the Hub.

```d2
direction: right
You -> Hub: "Start Agent on My Machine"
Hub -> Your Machine (Broker): "CreateAgent (via WS Tunnel)"
Your Machine (Broker) -> Docker: "Run Container"
Agent -> Hub: "Status: RUNNING"
```

The Hub acts as the control plane, but the actual execution (and the git worktrees) stay on your machine.

## Registering Your Machine

To allow the Hub to dispatch agents to your machine, you must start a Runtime Broker and register it.

### 1. Start the Broker

You can start a standalone broker process in the background:

```bash
scion broker start
```

*(Alternatively, if you run `scion server start --workstation`, a broker is automatically started alongside a local workstation server.)*

### 2. Link to the Hub

Before the broker can receive commands, it must be registered with the Hub you are connected to. This establishes a secure trust relationship.

```bash
scion broker register
```

This command will securely exchange credentials with the Hub, linking your machine's broker to your Hub user account.

### 3. Provide Compute for a Project

Even after registration, your broker will not accept arbitrary agents. It only executes agents for specific **Projects** (projects) that you explicitly authorize it to serve.

Navigate to the directory of a project that is connected to the Hub, and run:

```bash
scion broker provide
```

This tells the Hub: *"My local broker is now a provider for this specific Project."* When anyone on your team starts an agent in this Project and targets your broker, the agent will execute on your machine.

To verify which projects your broker is currently serving:

```bash
scion broker status
```

## Transport Auth for IAP-Protected Hubs

When the Hub is behind [Google IAP](/scion/hosted/ha/auth-proxy-iap/), the broker must carry a transport-layer OIDC token on every request. Transport auth is configured either during registration or via environment variables.

### Configuration at registration time

The `scion hub brokers register` command accepts transport flags that are persisted to the credentials file:

```bash
scion hub brokers register \
  --name my-broker \
  --transport-mode iap \
  --transport-audience "1234567890-abc.apps.googleusercontent.com" \
  https://hub.example.com
```

| Flag | Description |
|---|---|
| `--transport-mode` | Transport mode: `iap` or `cloudrun_invoker` |
| `--transport-audience` | OIDC audience — the custom OAuth 2.0 Client ID (for `iap`) or Hub URL (for `cloudrun_invoker`) |

These values are written to the credentials file as `transportMode` and `transportAudience`, so the broker daemon automatically uses them on startup.

### Environment variable overrides

For containerized brokers, set `SCION_TRANSPORT_MODE` and `SCION_TRANSPORT_AUDIENCE` as environment variables in the Deployment manifest. Environment variables override credentials-file values.

See [Brokers behind IAP](/scion/hosted/ha/auth-proxy-iap/#brokers-behind-iap) for the full deployment guide, including Workload Identity setup and the registration Job manifest.

## Security & Isolation

When you register your machine as a broker:
*   **Isolation**: Every agent runs in its own isolated container. In local mode each agent gets a dedicated git worktree (`.scion_worktrees/`); in hub-hosted git projects agents share a single workspace checkout, but each agent's per-agent state (task prompt, resolved config) lives outside that shared mount so sibling agents cannot read it.
*   **No Source Code Sharing**: The Hub does not store your source code. The broker simply creates local branches and commits.
*   **Safe Secrets**: Sensitive API keys and environment variables managed in the Hub are injected directly into the agent container's memory at runtime. They are not saved to your local disk.
*   **Mutual Authentication**: All communication over the Control Channel uses HMAC-SHA256 signatures, ensuring that only the authorized Hub can send commands to your machine.

## Stopping the Broker

If you want to stop accepting agent workloads from the Hub, you can simply stop the broker daemon:

```bash
scion broker stop
```

Agents that are currently running on your machine may be interrupted or left orphaned depending on their state.
