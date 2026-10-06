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

This tells the Hub: *"My local broker is a provider for this specific Project."* Members of the Project can then start agents on your broker, and those agents execute on your machine. Providing a broker requires update permission on the Project and ownership of the broker; see [Sharing a broker with a project](#sharing-a-broker-with-a-project).

To verify which projects your broker is currently serving:

```bash
scion runtime-broker status
```

## Transport Auth for IAP-Protected Hubs

When the Hub is behind [Google IAP](/scion/hosted/ha/auth-proxy-iap/), the broker must carry a transport-layer OIDC token on every request. Transport auth is configured either during registration or via environment variables.

### Configuration at registration time

The `scion runtime-broker register` command accepts transport flags that are persisted to the credentials file:

```bash
scion runtime-broker register \
  --hub https://hub.example.com \
  --name my-hub \
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

## Security & Isolation

When you register your machine as a broker:
*   **Isolation**: Every agent runs in its own isolated container. In local mode each agent gets a dedicated git worktree (`.scion_worktrees/`); in hub-hosted git projects agents share a single workspace checkout, but each agent's per-agent state (task prompt, resolved config) lives outside that shared mount so sibling agents cannot read it.
*   **No Source Code Sharing**: The Hub does not store your source code. The broker simply creates local branches and commits.
*   **Safe Secrets**: Sensitive API keys and environment variables managed in the Hub are injected directly into the agent container's memory at runtime. They are not saved to your local disk.
*   **Mutual Authentication**: All communication over the Control Channel uses HMAC-SHA256 signatures, ensuring that only the authorized Hub can send commands to your machine.

## Broker Registration Permission

Registering a broker, or re-minting its join token, requires the `broker.create` permission. This covers `POST /api/v1/brokers` and the embedded-broker path of `POST /api/v1/projects/register`. `broker.create` is granted through the built-in `hub-member` role, so users with the **member** or **admin** [hub role](/scion/hosted/ha/permissions/#hub-roles) can register brokers. Users with the **viewer** hub role cannot.

:::caution[Breaking change]
Viewer-role users could previously register brokers; they now receive a 403. The `hub-member` role is reconciled to revision 3 on Hub start to add `broker.create`, so no manual migration is needed for members.
:::

## Broker Ownership

The user who registers a broker becomes its owner. Registration admits a CLI or Web UI sign-in, or a hub-boundary [user access token](/scion/hosted/user/personal-access-tokens/) that carries `broker:create`; a Runtime Broker request acting on behalf of a user is not admitted. A broker registered with a token belongs to the token's user.

| Operation | Who may perform it |
|---|---|
| Register a new broker | A user who holds `broker.create` (sign-in, or hub token with `broker:create`). |
| Re-register a broker (new join token) | The same credentials, and the user must be the broker's owner, or a super-admin with a sign-in. This includes the embedded broker's registration path. |
| Rotate the broker's HMAC secret | The broker itself (HMAC), or its owner or a super-admin with a sign-in. No user access token can rotate a secret. |
| Turn on auto-provide | Additionally requires `broker.auto_provide`, held by super-admins. |

Registering a broker never associates it with a project. Brokers registered before ownership was recorded get an owner assigned automatically when the Hub boots.

Register, re-register, rotate, link and unlink events in the audit log record the credential kind and credential ID next to the user, so you can tell whether a sign-in or a specific token was used.

## Headless registration with a hub token

To register a broker on a machine where you cannot sign in interactively (a build host or a VM), mint a short-lived hub-boundary token on a machine where you are signed in, then use it on the host.

1. **Mint a hub token.** You must be a hub member, which grants `broker.create`. Include `broker:read` as well, so that `register` can check its existing registration. `scion hub token create` does not yet offer a hub boundary, so create the token through the API with your CLI session:

   ```bash
   HUB=https://hub.example.com
   SESSION=$(jq -r --arg hub "$HUB" '.hubs[$hub].accessToken' ~/.scion/credentials.json)
   curl -sS -X POST "$HUB/api/v1/auth/tokens" \
     -H "Authorization: Bearer $SESSION" \
     -H "Content-Type: application/json" \
     -d '{"name":"build-host-3","boundary":{"kind":"hub"},"scopes":["broker:create","broker:read"],"expiresAt":"2026-12-01T00:00:00Z"}'
   ```

   The response contains the token value (`scion_pat_...`) once. Choose an `expiresAt` that is shortly after you plan to register.

2. **Register on the host** with the broker server running (`scion runtime-broker start`):

   ```bash
   SCION_HUB_ENDPOINT=https://hub.example.com SCION_HUB_TOKEN=scion_pat_... scion runtime-broker register -y
   ```

   The broker belongs to the token's user. Do not pass `--auto-provide` unless you hold `broker.auto_provide`.

3. **Share the broker with a project.** As the broker's owner, run `scion runtime-broker provide --project <project>` from a signed-in machine. See [Sharing a broker with a project](#sharing-a-broker-with-a-project).

4. **Revoke the token** when registration is done:

   ```bash
   scion hub token revoke <token-id>
   ```

:::caution
The CLI prefers credentials from `scion hub auth login`, an agent token file, and `SCION_AUTH_TOKEN` over `SCION_HUB_TOKEN`. On a host with a stale sign-in, run `scion hub auth logout` first so the token is used.
:::

## Sharing a broker with a project

A broker runs agents for a project only after it is **associated** with the project (added as a provider). Association needs consent from both sides:

- **Project side:** update permission on the project (`project.update`, held by project owners and admins).
- **Broker side:** `broker.update` on the broker, which its owner and super-admins hold. Holding `project.update` alone, or having registered some other broker, is never enough to associate someone else's broker.

Every path that associates a broker applies both checks: `scion runtime-broker provide` (`POST /api/v1/projects/{id}/providers`), the `brokerId` field of `POST /api/v1/projects/register`, and naming a broker that is not yet a provider when creating an agent. A user access token can carry the project side (`project:update`) but not the broker side, so associating a broker needs a sign-in.

Once a broker is associated by its owner (or a super-admin), **members of the project can create agents on it**, including with a project token that can create agents in the project. Members can use a provider broker when its association records the owner's consent (linked by the owner, by auto-provide, or by a user who is an active super-admin); otherwise only holders of `broker.dispatch` on the broker (its owner and super-admins) can use it. Providing the broker as its owner records consent. Brokers with no recorded owner (operator-provisioned) are usable by members of every project they serve.

Related rules:

- **Default broker:** a project's default runtime broker must already be a provider of the project. Provide the broker first, then set it as the default (`scion runtime-broker provide --make-default`).
- **Withdrawing:** a provider can be removed by anyone with `project.update`, or by the broker's owner (or a super-admin) for that broker without update permission on the project: `scion runtime-broker withdraw --project <project>`, or `DELETE /api/v1/projects/{id}/providers/{brokerId}`. Removal admits a sign-in or a user access token; a Runtime Broker request acting on behalf of a user is not admitted.
- **Auto-provide:** an auto-provide broker is linked to every new project and is usable by every user. Turning it on requires `broker.auto_provide`.

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
