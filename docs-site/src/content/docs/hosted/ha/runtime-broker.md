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

- Add the Hub to your hub list, make it current, and sign in:
  ```bash
  scion hub add prod https://hub.example.com --use
  ```
  ```bash
  scion hub auth login
  ```
- `scion runtime-broker register`, `join` and the other Runtime Broker administration commands (`deregister` picks the hub connection from the Runtime Broker's stored connections (with `--name` when there are several), or, with none stored, the registration in the global settings, but sends the deregistration to the Hub the CLI selects, so select that connection's Hub first) talk to the Hub the CLI selects, like any other command: `--hub` (a URL or a hub list nickname), `SCION_HUB_ENDPOINT` or `SCION_HUB_URL`, then, inside a linked project directory, its link entry, then the current Hub. `scion runtime-broker start` does not use the current Hub: the Runtime Broker process reports to the Hub named in its server settings (next item). A Runtime Broker started without a Hub endpoint does not connect after a later `register`; stop and start it again. `register` and `join` refuse a Hub URL with user info, a query or a fragment; pass a plain `http(s)` URL.
- The Runtime Broker process itself reports to the Hub named by `server.broker.hub_endpoint` (or `SCION_SERVER_RUNTIMEBROKER_HUBENDPOINT`). `register` and `join` save that setting when it is unset (except on a host that runs a workstation Hub). While it is unset, the Runtime Broker falls back to `hub.endpoint` and logs a deprecation warning at startup; set `server.broker.hub_endpoint` in `~/.scion/settings.yaml` to avoid it. See [The server's own hub endpoint](/scion/hosted/user/multiple-hubs/#the-servers-own-hub-endpoint).
- Configure an image registry (`scion config set --global image_registry <registry>`): `scion runtime-broker start` refuses to start without one.
- `scion runtime-broker start`, `register` and `join` work outside a project directory; no `--global` is needed.

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

## Headless Registration with a Join Token

`scion runtime-broker register` needs a Hub credential on the broker host: a sign-in, or a [hub token](#headless-registration-with-a-hub-token). For a host where you do not want to place any Hub user credential, such as a build machine or a VM provisioned by a script, split registration in two: create a join token on your own machine, then redeem it on the host. The host never holds a Hub user credential; it ends up with the same broker credentials `register` would have saved.

**1. On your machine**, signed in to the Hub, create the broker and a token for it:

```bash
scion hub brokers join-token create build-host-3 --ttl 30m
```

The token is printed on stdout and the instructions, including the broker ID, on stderr. `--json` prints `brokerId`, `brokerName`, `joinToken`, `expiresAt`, `hubEndpoint` and `reissued` as one JSON object.

- You need the `broker.create` permission (see [Broker Registration Permission](/scion/hosted/ha/runtime-broker/#broker-registration-permission)), and you become the broker's owner. Sign in with `scion hub auth login`, or use a hub-boundary [user access token](/scion/hosted/user/personal-access-tokens/) that carries `broker:create`; the broker then belongs to the token's user.
- `--ttl` sets how long the token is valid, from `5m` to `24h`. The default is `1h`.
- The token is single use.
- Running the command again for the same broker issues a new token, and the previous unused one stops working. Only the broker's owner, or a super-admin with a sign-in, can do this; a token does this only for a broker its user created.
- The command never changes an existing broker's settings (auto-provide, labels, GCP host identity) and does not add the broker to any project.

**2. Move the token to the host** through a channel you already trust for secrets, for example `scp` to a file readable only by the broker's user, or your secret manager.

**3. On the host**, redeem it:

```bash
export SCION_HUB_ENDPOINT=https://hub.example.com
scion runtime-broker join --broker-id <broker-id> --token-file /path/to/token
```

- The token is read from `--token-file` (`-` reads it from stdin), or else from the `SCION_BROKER_JOIN_TOKEN` environment variable. There is no flag that takes the token on the command line. If the file has any group or other permissions, `join` prints a warning and still uses it.
- `--broker-id` can also come from `SCION_BROKER_ID`.
- The broker server does not have to be running yet; `join` only warns if it is not.
- `join` saves the credentials to `~/.scion/hub-credentials/<name>.json` and records the broker ID and, when unset, `server.broker.hub_endpoint` in global settings (not on a host that runs a workstation Hub), as `register` does.
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

## In-Cluster Runtime Broker for the Substrate Runtime

The `substrate` runtime runs each agent as an [Agent Substrate](https://github.com/agent-substrate/substrate) actor on GKE. Because Substrate's control API (`ateapi`) and inbound router (`atenet-router`) have no authorization of their own, the Runtime Broker for this runtime must run **inside** the GKE cluster rather than reaching in over a LoadBalancer or Ingress. Inside each actor, `sciontool substrate-serve` runs as PID 1 and serves the Runtime Broker's bootstrap, exec and health requests with the enforced privilege drop in place. Agent egress is limited to the Hub, git, model and telemetry hosts plus any public hostnames the operator lists in the runtime's `egress_allow`. The agent image must be pinned by digest.

Configure it with a runtime entry of `type: substrate` and a `substrate:` block (`api_endpoint`, `router_endpoint`, `sandbox_class`, `worker_selector`, `snapshot_storage`, `egress_allow`, …). The cluster prerequisites, Runtime Broker manifest, example settings and day-2 operations live in [`deploy/substrate/`](https://github.com/GoogleCloudPlatform/scion/tree/main/deploy/substrate).

## Security & Isolation

When you register your machine as a broker:
*   **Isolation**: Every agent runs in its own isolated container. In worktree-per-agent projects each agent gets a dedicated git worktree; in other hub-hosted git projects agents share a single workspace checkout, but each agent's per-agent state (task prompt, resolved config) lives outside that shared mount so sibling agents cannot read it.
*   **No Source Code Sharing**: The Hub does not store your source code. The broker simply creates local branches and commits.
*   **Safe Secrets**: Sensitive API keys and environment variables managed in the Hub are injected directly into the agent container's memory at runtime. They are not saved to your local disk.
*   **Mutual Authentication**: All communication over the Control Channel uses HMAC-SHA256 signatures, ensuring that only the authorized Hub can send commands to your machine.

## Broker Registration Permission

Registering a Runtime Broker, or re-minting its join token, requires the `broker.create` permission. This covers `POST /api/v1/brokers` and the embedded Runtime Broker path of `POST /api/v1/projects/register`. `broker.create` is granted through the built-in `hub-member` role, so users with the **member** or **admin** [hub role](/scion/hosted/ha/permissions/#hub-roles) can register Runtime Brokers. Users with the **viewer** hub role cannot. A request authenticated with a [user access token](/scion/hosted/user/personal-access-tokens/) is admitted only when the token has a hub boundary and carries `broker:create`; any other user access token is denied with `403`. See [Headless registration with a hub token](#headless-registration-with-a-hub-token). Registration, re-registration, and secret rotation also refuse agent, delivery, federation, and on-behalf-of credentials: only a user credential (or, for rotation, the Runtime Broker's own credential) is accepted.

:::caution[Breaking change]
Viewer-role users could previously register brokers; they now receive a 403. The `hub-member` role is reconciled to revision 3 on Hub start to add `broker.create`, so no manual migration is needed for members.
:::

## Broker Ownership

The user who registers a Runtime Broker becomes its owner. Registration admits a CLI or Web UI sign-in, or a hub-boundary [user access token](/scion/hosted/user/personal-access-tokens/) that carries `broker:create`; a Runtime Broker request acting on behalf of a user is not admitted. A broker registered with a token belongs to the token's user.

| Operation | Who may perform it |
|---|---|
| Register a new broker | A user who holds `broker.create` (sign-in, or hub token with `broker:create`). |
| Re-register a broker (new join token) | The same credentials, and the user must be the broker's owner, or a super-admin with a sign-in. This includes the embedded broker's registration path. |
| Rotate the broker's HMAC secret | The broker itself (HMAC, and only for its own secret), or its owner or a super-admin with a sign-in. No user access token can rotate a secret. |
| Turn on auto-provide | Additionally requires `broker.auto_provide`, held by super-admins. |

Registering a broker never associates it with a project. Brokers registered before ownership was recorded get an owner assigned automatically when the Hub boots.

Register, re-register, rotate, link, unlink and update events in the audit log record the credential kind and credential ID next to the user, so you can tell whether a sign-in or a specific token was used.

With the `hub.conduit` [experiment](/scion/reference/experiments/) on, each of the Runtime Broker's Conduit sessions is tied to the secret it was admitted with. Deregistering the Runtime Broker, rotating or replacing its secret, or a secret that is no longer active closes those sessions with `4401 authz_expired`. The Runtime Broker retries with backoff (1 to 60 seconds); after a rotation it is admitted again with its new secret. A session that cannot be checked (for example during a database error) stays open until the next check.

With the experiment on, a Runtime Broker's Conduit session also carries the Hub's short requests to it: agent logs, `exec`, image status, pull and removal, and project cleanup. The session lists the requests it serves when it connects, and any Hub instance can reach it, not only the one that holds the control channel. A request that no Conduit session serves uses the control channel, and then the broker's HTTP endpoint; this path is the fallback only. Agent creation, start, stop, delete, messages, keys and attach do not change.

- An `exec` whose request reached the Runtime Broker but whose result was lost (for example, the session closed while it ran) returns `502` with code `outcome_unknown`. The command may have run. The Hub does not retry it, and `error.details` carries `operation`, `broker_id` and `request_id`. A failure the Runtime Broker reports still returns `502 runtime_error`.
- A request that is safe to repeat (logs, image status, image pull and project cleanup) is retried on another session of the same Runtime Broker when it loses its session. When no session serves it, logs return `503 unavailable`, and project cleanup, image status and image pull fail. Image removal is not retried.
- Project cleanup on the Runtime Broker removes the project's directory under `~/.scion/project-configs` even when the project directory itself is already gone, and reports success only once both are gone, so repeating it finishes any step that failed. The Hub's Conduit path reports a cleanup failure as a failure.

### Settings managed through the update API

Fields set through the update API (`PATCH /api/v1/runtime-brokers/{id}`) persist across registrations:

- **GCP host service account and project.** They are written together. Registration fills them only while they are unset; a value set or cleared through the update API stays until the next update. Send `gcpHostPinned: false` in an update to hand them back to registration.
- **Name.** Registration keeps the stored name.
- **Labels.** Labels a registration sends are added when the key is absent; an existing key keeps its value.

A registration response lists any requested value that was kept (`keptSettings`), and `scion runtime-broker register` prints one line per kept value. The register audit event records them, and changes made through the update API are audited too.

### Hub-managed labels

The Hub sets the `scion.io/plugin` and `scion.io/broker-role` labels itself; values sent in registration or update requests are ignored.

## Headless registration with a hub token

To register a broker on a machine where you cannot sign in interactively (a build host or a VM), mint a short-lived hub-boundary token on a machine where you are signed in, then use it on the host.

1. **Mint a hub token.** You must be a hub member, which grants `broker.create`. Include `broker:read` as well, so that `register` can check its existing registration:

   ```bash
   scion hub token create --boundary hub --name build-host-3 \
     --scopes broker:create,broker:read --expires 2h
   ```

   The command prints the token value (`scion_pat_...`) once. Choose an `--expires` that is shortly after you plan to register.

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
- **Withdrawing:** a provider can be removed by anyone with `project.update`, or by the broker's owner (or a super-admin) for that broker without update permission on the project: `scion runtime-broker withdraw --project <project>`, or `DELETE /api/v1/projects/{id}/providers/{brokerId}`. Removal admits a sign-in or a user access token; a Runtime Broker request acting on behalf of a user is not admitted. A withdrawn standalone Runtime Broker keeps its project directory when the project is later deleted (see [What remains after a project delete](/scion/hosted/ha/kubernetes/#what-remains-after-a-project-delete)).
- **Auto-provide:** an auto-provide broker is linked to every new project and is usable by every user. Turning it on requires `broker.auto_provide`.

## Scaling a Broker Host: inotify Limits

On a Linux host whose Runtime Broker runs agents with Docker or Podman, every agent runs as the broker's own host user, so the file watchers of all agents' harness processes count against that one user's inotify limits. Scion itself creates no inotify watchers. The kernel default of `fs.inotify.max_user_instances=128` can run out at around 150 agents; after that, watchers fail with "Too many open files", and the first errors often come from the host's own per-user services rather than from Scion.

At startup, a Runtime Broker in hosted mode with a Docker or Podman runtime (or a flat Runtime Broker host in hosted mode with a `docker` instance) reads `/proc/sys/fs/inotify/max_user_instances` and `max_user_watches`. If either is below what about 100 agents need, it logs one `WARNING` line with the values and the fix. 100 agents is the Hub's default `max_agents_per_broker`; the broker does not see a different per-broker limit at startup, so the warning assumes the default. The thresholds are 1 instance and 1024 watches per agent, plus 128 instances and 8192 watches for the user's other processes: at least 228 instances and 110592 watches. They are estimates, based on one running Claude Code agent that held 1 instance and about 1000 watches, roughly one watch per directory of its workspace; agents on workspaces with many more directories need more watches. A workstation server does not log the warning. The check only reads the limits; it never changes them, and it never stops the broker.

`scion doctor` shows the same limits on any Linux host as the `inotify-limits` check, together with the approximate number of inotify instances and watches held by processes running as your user. It judges them (pass or warn) only on a host configured as a server whose Runtime Broker runs agents with Docker or Podman; elsewhere the line is shown as skipped. On other operating systems, or when `/proc/sys/fs/inotify` cannot be read, the check is reported as not applicable.

To raise the limits, as root:

```bash
sudo sysctl -w fs.inotify.max_user_instances=8192 fs.inotify.max_user_watches=524288
```

To keep them across reboots, put them in a file under `/etc/sysctl.d/`, for example `/etc/sysctl.d/99-scion-inotify.conf`:

```text
fs.inotify.max_user_instances=8192
fs.inotify.max_user_watches=524288
```

The new limits apply to new watchers without restarting the Runtime Broker or the agents; the Runtime Broker's startup warning is re-evaluated only at its next start. A VS Code window opened with [`scion vscode`](/scion/reference/cli/#scion-vscode) runs its file watcher inside the agent container too, so it uses an instance of the same limit; when none is left, files created outside the explorer stop appearing in that window. See [Troubleshooting: new files do not appear in the explorer](/scion/reference/cli/#troubleshooting-new-files-do-not-appear-in-the-explorer).

## Broker Health Monitoring

A Runtime Broker whose default runtime failed to resolve at startup reports itself `degraded` on `/healthz` (still HTTP `200`), and its `/readyz` returns `503`, so point readiness probes at `/readyz`. Starting or restarting an existing agent whose saved profile the Runtime Broker cannot resolve returns a retryable `503 runtime_unavailable` (with `Retry-After`) before anything is stopped, instead of falling back to the default runtime. On a Runtime Broker without the Hub's stored profiles, the one exception is an agent that last ran on a plain Docker or Podman default runtime, which falls back to it. Where the Hub's stored profiles apply (a Runtime Broker co-located with a Hub that stores at least one profile), an agent whose saved profile is neither one of them nor a built-in profile kept beside them gets the `503` and never falls back, even if it last ran on the default runtime; the error names the available profiles. A dispatch that names no profile uses `active_profile` when it is one of the Hub's stored profiles, otherwise the only stored profile when there is exactly one, otherwise a built-in profile that `active_profile` names (when several are stored), and otherwise gets the same `503`.

The Hub monitors broker health via a recurring heartbeat timeout scheduler. If a broker's WebSocket control channel disconnects and the disconnect event is not received (for example, due to a Hub crash or network partition), the Hub automatically marks the broker as **offline** after approximately five minutes of missed heartbeats. This mirrors the existing agent heartbeat timeout pattern and ensures the broker selection cascade does not dispatch work to unreachable brokers. With the `hub.conduit` [experiment](/scion/reference/experiments/) on, Runtime Broker presence also reads the Conduit session registry alongside the legacy WebSocket control channel and heartbeat age, so a Runtime Broker connected through a live Conduit session on another Hub replica or a separate relay counts as connected for create, lifecycle, purge, move and tunnel checks even when the answering Hub node has no local control-channel socket for it.

On a co-located server, the broker detail shows the host identity state: whether the configured GCP host service account matches the identity the host reports. `mismatch` and `pinned_absent` (configured, but the host reports no account) mark the broker as needing attention in the health summary. The configured value is used either way.

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
