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

### 0. Prerequisites

- Sign in to the Hub: `scion hub auth login --hub-url https://hub.example.com`.
- Configure the Hub endpoint in your global settings **before** starting the broker, for example with `scion -g global config set --global hub.endpoint https://hub.example.com`, or set the `SCION_HUB_ENDPOINT` environment variable. A broker started without a Hub endpoint does not connect after a later `register`; stop and start it again.
- Configure an image registry (`scion -g global config set --global image_registry <registry>`): `scion runtime-broker start` refuses to start without one.
- Outside a project directory, pass `--global` to `scion runtime-broker start` and `register`.

### 1. Start the Broker

You can start a standalone broker process in the background:

```bash
scion runtime-broker start
```

The broker listens on port 9800 by default, or on `server.broker.port` if your global settings set it. If that port is taken, pass `--port`:

```bash
scion runtime-broker start --port 19800
```

While the broker runs, `start` keeps a record of the port it used, and the other `runtime-broker` subcommands (`register`, `deregister`, `status`, `stop`, `restart`, `hubs`) use that port unless you pass their own `--port`. `restart` also keeps the `--auto-provide` and `--debug` values the daemon was started with.

*(Alternatively, `scion server start` with no flags runs a local workstation server, which includes a broker.)*

### 2. Link to the Hub

Before the broker can receive commands, it must be registered with the Hub you are connected to. This establishes a secure trust relationship.

```bash
scion runtime-broker register
```

This command checks that the local broker server is running, then exchanges credentials with the Hub, linking your machine's broker to your Hub user account. The credentials are saved to `~/.scion/hub-credentials/<name>.json`, one file per Hub connection (`--name`, derived from the Hub endpoint by default). List the connections with `scion runtime-broker hubs`.

### 3. Provide Compute for a Project

Even after registration, your broker will not accept arbitrary agents. It only executes agents for specific **Projects** (projects) that you explicitly authorize it to serve.

Navigate to the directory of a project that is connected to the Hub, and run:

```bash
scion runtime-broker provide
```

Or name the project from anywhere: `scion runtime-broker provide --project <name|id>`.

This tells the Hub: *"My local broker is now a provider for this specific Project."* When anyone on your team starts an agent in this Project and targets your broker, the agent will execute on your machine.

To verify which projects your broker is currently serving:

```bash
scion runtime-broker status
```

## Headless Registration with a Join Token

`scion runtime-broker register` needs a Hub sign-in on the broker host. For a host where you do not want to sign in, such as a build machine or a VM provisioned by a script, split registration in two: create a join token on your own machine, then redeem it on the host. The host never holds a Hub user credential; it ends up with the same broker credentials `register` would have saved.

**1. On your machine**, signed in to the Hub, create the broker and a token for it:

```bash
scion hub brokers join-token create build-host-3 --ttl 30m
```

The token is printed on stdout and the instructions, including the broker ID, on stderr. `--json` prints `brokerId`, `brokerName`, `joinToken`, `expiresAt`, `hubEndpoint` and `reissued` as one JSON object.

- You need the `broker.create` permission (see [Broker Registration Permission](/scion/hosted/ha/runtime-broker/#broker-registration-permission)), and you become the broker's owner. Creating a token with a [user access token](/scion/hosted/user/personal-access-tokens/) is not supported; use an interactive sign-in (`scion hub auth login`).
- `--ttl` sets how long the token is valid, from `5m` to `24h`. The default is `1h`.
- The token is single use.
- Running the command again for the same broker issues a new token, and the previous unused one stops working. Only the broker's owner or a super-admin can do this.
- The command never changes an existing broker's settings (auto-provide, labels, GCP host identity) and does not add the broker to any project.

**2. Move the token to the host** through a channel you already trust for secrets, for example `scp` to a file readable only by the broker's user, or your secret manager.

**3. On the host**, redeem it:

```bash
export SCION_HUB_ENDPOINT=https://hub.example.com
scion runtime-broker join --broker-id <broker-id> --token-file /path/to/token
```

- The token is read from `--token-file` (`-` reads it from stdin), or else from the `SCION_BROKER_JOIN_TOKEN` environment variable. There is no flag that takes the token on the command line. If the file can be read by group or other users, `join` prints a warning and still uses it.
- `--broker-id` can also come from `SCION_BROKER_ID`.
- The broker server does not have to be running yet; `join` only warns if it is not.
- `join` saves the credentials to `~/.scion/hub-credentials/<name>.json` and records the Hub endpoint and broker ID in global settings, as `register` does.
- If the host already has credentials for this Hub connection, or its settings name a different broker ID, `join` stops. Pass `--force` to replace them.
- `--name`, `--transport-mode` and `--transport-audience` work as for `register`.

Redeeming a token replaces the broker's credentials. If another host has already joined as this broker, it is disconnected.

**4. Start the broker** with `scion runtime-broker start`, then [provide it to a project](/scion/hosted/ha/runtime-broker/#3-provide-compute-for-a-project). Joining does not provide the broker to any project.

## Transport Auth for IAP-Protected Hubs

When the Hub is behind [Google IAP](/scion/hosted/ha/auth-proxy-iap/), the broker must carry a transport-layer OIDC token on every request. Transport auth is configured either during registration or via environment variables.

### Configuration at registration time

The `scion runtime-broker register` command accepts transport flags that are persisted to the credentials file:

```bash
scion runtime-broker register \
  --hub https://hub.example.com \
  --name my-broker \
  --transport-mode iap \
  --transport-audience "1234567890-abc.apps.googleusercontent.com"
```

| Flag | Description |
|---|---|
| `--transport-mode` | Transport mode: `iap` or `cloudrun_invoker` |
| `--transport-audience` | OIDC audience — the custom OAuth 2.0 Client ID (for `iap`) or Hub URL (for `cloudrun_invoker`) |

These values are written to the credentials file as `transportMode` and `transportAudience`, so the broker daemon automatically uses them on startup.

### Environment variable overrides

For containerized brokers, set `SCION_TRANSPORT_MODE` and `SCION_TRANSPORT_AUDIENCE` as environment variables in the Deployment manifest. Environment variables override credentials-file values.

See [Brokers behind IAP](/scion/hosted/ha/auth-proxy-iap/#brokers-behind-iap) for the full deployment guide, including Workload Identity setup and the registration Job manifest.

## In-Cluster Runtime Broker for the Substrate Runtime

The `substrate` runtime runs each agent as an [Agent Substrate](https://github.com/agent-substrate/substrate) actor on GKE. Because Substrate's control API (`ateapi`) and inbound router (`atenet-router`) have no authorization of their own, the Runtime Broker for this runtime must run **inside** the GKE cluster rather than reaching in over a LoadBalancer or Ingress. Inside each actor, `sciontool substrate-serve` runs as PID 1 and serves the Runtime Broker's bootstrap, exec and health requests with the enforced privilege drop in place. Agent egress is limited to the Hub, git, model and telemetry hosts plus any public hostnames the operator lists in the runtime's `egress_allow`. The agent image must be pinned by digest.

Configure it with a runtime entry of `type: substrate` and a `substrate:` block (`api_endpoint`, `router_endpoint`, `sandbox_class`, `worker_selector`, `snapshot_storage`, `egress_allow`, …). The cluster prerequisites, Runtime Broker manifest, example settings and day-2 operations live in [`deploy/substrate/`](https://github.com/GoogleCloudPlatform/scion/tree/main/deploy/substrate).

## Security & Isolation

When you register your machine as a broker:
*   **Isolation**: Every agent runs in its own isolated container. In local mode each agent gets a dedicated git worktree (`.scion_worktrees/`); in hub-hosted git projects agents share a single workspace checkout, but each agent's per-agent state (task prompt, resolved config) lives outside that shared mount so sibling agents cannot read it.
*   **No Source Code Sharing**: The Hub does not store your source code. The broker simply creates local branches and commits.
*   **Safe Secrets**: Sensitive API keys and environment variables managed in the Hub are injected directly into the agent container's memory at runtime. They are not saved to your local disk.
*   **Mutual Authentication**: All communication over the Control Channel uses HMAC-SHA256 signatures, ensuring that only the authorized Hub can send commands to your machine.

## Broker Registration Permission

Registering a Runtime Broker, or re-minting its join token, requires the `broker.create` permission. This covers `POST /api/v1/brokers` and the embedded Runtime Broker path of `POST /api/v1/projects/register`. `broker.create` is granted through the built-in `hub-member` role, so users with the **member** or **admin** [hub role](/scion/hosted/ha/permissions/#hub-roles) can register Runtime Brokers. Users with the **viewer** hub role cannot. Runtime Broker creation requires a signed-in session: a request authenticated with a [user access token](/scion/hosted/user/personal-access-tokens/) is denied with `403`, whatever the token's boundary or scopes.

:::caution[Breaking change]
Viewer-role users could previously register brokers; they now receive a 403. The `hub-member` role is reconciled to revision 3 on Hub start to add `broker.create`, so no manual migration is needed for members.
:::

## Broker Ownership

The user who registers a broker becomes its owner. Re-registering an existing broker and rotating its HMAC secret are ownership-gated actions. This includes the embedded broker's registration path. These actions are allowed only for the broker's owner, the broker itself (authenticated via HMAC), or a system super-admin. The owner and super-admin shortcuts apply only to an unscoped sign-in: a scoped [user access token](/scion/hosted/user/personal-access-tokens/) never satisfies them, even if it belongs to the owner or a super-admin. Brokers registered before ownership was recorded get an owner assigned automatically when the Hub boots.

## Broker Health Monitoring

The Hub monitors broker health via a recurring heartbeat timeout scheduler. If a broker's WebSocket control channel disconnects and the disconnect event is not received (for example, due to a Hub crash or network partition), the Hub automatically marks the broker as **offline** after approximately five minutes of missed heartbeats. This mirrors the existing agent heartbeat timeout pattern and ensures the broker selection cascade does not dispatch work to unreachable brokers.

## Unregistering a Broker

To permanently remove a broker from the Hub, use the **Unregister** button on the broker detail page in the Web Dashboard. Unregistering a broker:

- Removes the broker's registration from the Hub.
- Cleans up associated HMAC secrets and join tokens.

Only the broker's owner or a Hub admin can unregister it, and unregistering requires confirmation. After unregistering, the broker can no longer receive agent dispatch commands. Re-registration via `scion runtime-broker register` is required to reconnect.

From the broker machine itself, run:

```bash
scion runtime-broker deregister
```

This deletes the broker from the Hub (removing it from every project it provides for), and deletes the local credentials file for that Hub connection. Once no Hub connection remains, it also clears this broker's ID and token from your global settings. Other settings, such as `image_registry`, are left as they are. With more than one Hub connection, choose one with `--name` (see `scion runtime-broker hubs`).

## Stopping the Broker

If you want to stop accepting agent workloads from the Hub temporarily, you can stop the broker daemon:

```bash
scion runtime-broker stop
```

Agents that are currently running on your machine may be interrupted or left orphaned depending on their state.
