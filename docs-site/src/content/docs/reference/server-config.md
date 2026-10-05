---
title: Server Configuration (Hub & Runtime Broker)
description: Configuration reference for Scion Hub and Runtime Broker services.
---

This document describes the configuration for the Scion Hub (State Server) and the Scion Runtime Broker.

## Configuration Location

Server configuration is defined in the `server` section of your `settings.yaml` file.

- **Primary**: `~/.scion/settings.yaml` (Global settings)
- **Legacy**: `server.yaml` in `~/.scion`, in the `--config` path, or in the working directory (Deprecated, but supported as a fallback when `settings.yaml` has no `server` key)

:::tip[Migration]
To move a `server.yaml` into `settings.yaml`, copy its contents under a top-level `server:` key in `~/.scion/settings.yaml`, then remove `server.yaml`. `scion config migrate` merges a `server.yaml` only while it converts a legacy (unversioned) `settings.yaml`; it skips a file that already has `schema_version`. There is no `--server` flag yet (ptone/scion#3116).
:::

## Structure

```yaml
schema_version: "1"
server:
  env: prod
  log_level: info
  
  hub:
    port: 9810
    host: "0.0.0.0"
    public_url: "https://hub.scion.dev"
    
  broker:
    enabled: true
    port: 9800
    broker_id: "generated-uuid"
    
  database:
    driver: sqlite
    url: "hub.db"
    
  auth:
    dev_mode: false
```

## Section Reference

### Hub Settings (`server.hub`)

Controls the central Hub API server.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `port` | int | `9810` | HTTP port to listen on (standalone mode). In combined mode (`--enable-web`), the Hub API is served on the web port instead and this setting is ignored. |
| `host` | string | `"0.0.0.0"` | Network interface to bind to. |
| `public_url` | string | | The externally accessible URL of the Hub (used for callbacks). |
| `agent_endpoint` | string | | Optional override of `public_url` used **only** for the Hub URL injected into agents (`SCION_HUB_ENDPOINT`). Use when agents reach the Hub on a different address than users — e.g. an internal VPC URL — while invite links, chat-bridge links, the OIDC issuer default, and the `cloudrun_invoker` audience default keep using `public_url`. Must be `scheme://host[:port]` only: `http` or `https`, an IP literal or a hostname of letters, digits, `_`, `-`, and `.`, no path, query, fragment, or credentials (a trailing `/` is stripped); the Hub fails to start otherwise. When unset, agents receive the Hub's regular endpoint (`public_url`, or the endpoint the Hub resolves when `public_url` is unset). **Scope:** injected into agents on every broker attached to this Hub, including remote brokers — see [Splitting the agent endpoint from the public URL](#splitting-the-agent-endpoint-from-the-public-url). **Security:** an `http://` value sends agent bearer tokens and fetched secrets unencrypted; prefer `https://` unless the network is trusted and isolated. |
| `gcp_project_id` | string | | GCP project ID used for minting GCP Service Accounts. Auto-detected if running on GCE/Cloud Run. |
| `gcp_iam_check_mode` | string | `"off"` | Controls whether IAM `actAs` permission is checked when binding a GCP service account to an agent. Supported values: `"off"` (no check; default) or `"enforce"` (uses Policy Troubleshooter to enforce `iam.serviceAccounts.actAs`). `"enforce"` is strongly recommended for any Hub where agents receive GCP identities; see the caution under [GCP IAM Check Mode](#gcp-iam-check-mode) for what `"off"` permits. See the security/permissions reference for details on roles and caches. |
| `gcp_iam_deny_unknown_policy` | string | `"fail-open"` | Behavior when Policy Troubleshooter cannot evaluate deny policies (e.g. if the Hub lacks org-level reviewer roles). Supported values: `"fail-open"` (allow if no explicit deny is found; default) or `"fail-closed"` (treat as indeterminate and deny). |
| `read_timeout` | duration | `"30s"` | HTTP read timeout. |
| `write_timeout` | duration | `"60s"` | HTTP write timeout. |
| `admin_emails` | list | `[]` | List of emails granted super-admin access. Listed users are always admins: they are promoted on sign-in. When the list is non-empty, an admin whose email is removed from it is demoted to [`default_user_role`](#authentication-serverauth) at the next hub restart or their next sign-in, whichever comes first. At restart, both `admin_emails` and the default role come from `settings.yaml` or the environment, so a change made only in the Admin UI (Postgres mode) takes effect at the user's next sign-in. If the default role was set only in the Admin UI, a user demoted at restart becomes Member. Two exceptions: admins promoted from **Admin > Users** (or the users API) stay admins, and nobody is demoted if the startup safety check failed (for example, no existing user matched the list at startup and there were no UI-promoted admins); demotions resume only after the configuration is fixed and the hub is restarted. Roles set from the admin UI for users who were never config admins (`member`, `viewer`) are not changed by this list. |
| `soft_delete_retention` | duration | | Duration to retain soft-deleted agents (e.g., `"72h"`). |
| `soft_delete_retain_files` | bool | `false` | Preserve workspace files during the soft-delete period. |
| `async_agent_launch` | bool | `false` | Turns on asynchronous agent create. A create is asynchronous only when this is on **and** the request opts in (`acceptAsyncLaunch`); `scion start`, `scion resume`, and scheduled agent creates opt in, other clients stay synchronous. Provision-only creates and reprovisioning are always synchronous. See [Asynchronous agent create](#asynchronous-agent-create). Startup-only: restart required to change. Env: `SCION_SERVER_HUB_ASYNCAGENTLAUNCH`. |
| `launch_timeout` | duration | `"5m"` | Whole-launch budget for an asynchronous create, from the moment the Hub begins the launch until the agent reaches a terminal Hub state. A launch that has not reached one by this deadline is ended and the agent is set to `error` (`launch_timeout`). A Go duration string such as `"15m"`. There is no upper limit. A non-zero value below `30s` is replaced by the default (`5m`) with a warning in the Hub log. In `settings.yaml`, a value that is not a valid duration is ignored and the default applies. Raise it for clusters with slow pod starts. Startup-only: restart required to change. Env: `SCION_SERVER_HUB_LAUNCHTIMEOUT`. |
| `launch_keepalive_seconds` | int | `15` | Keepalive interval, in seconds, that the Hub sends to the Runtime Broker with each asynchronous create. A launch whose broker sends no report for 8x this interval (120s at the default) is ended and the agent is set to `error` (`broker_lost`). Values of `0` or less use the default. Startup-only: restart required to change. Env: `SCION_SERVER_HUB_LAUNCHKEEPALIVESECONDS`. |
| `missing_agent_grace` | duration | `"3m"` | How long a `running` agent may be absent from its Runtime Broker's heartbeat before the Hub marks it `error` with exit reason `container_missing` (an existing `preempted` or `evicted` exit reason and its message are kept). Applies only when the broker is online, reported a complete runtime inventory, and sent a recent previous heartbeat; agents with a lifecycle operation in progress are skipped. Values below `"1m"` fall back to the default. Env: `SCION_SERVER_HUB_MISSINGAGENTGRACE`. |
| `start_claim_lease_ttl` | duration | `"90s"` | Lease of the claim the Hub takes before dispatching any agent start; the Hub process running the start renews it every third of this. Allowed `30s` to `5m`; other values fall back to the default. Hot-reloaded. Env: `SCION_SERVER_HUB_STARTCLAIMLEASETTL`. |
| `start_max_duration` | duration | `"12m"` | Hard deadline on any agent start, including a wait for another Hub node to dispatch it. Minimum `11m` (the broker's pod-ready bound plus a minute). Hot-reloaded. Env: `SCION_SERVER_HUB_STARTMAXDURATION`. |
| `start_unconfirmed_hold` | duration | `"13m"` | Longest time a start whose outcome is unknown (for example a dispatch timeout) keeps other starts of the agent waiting, until the runtime shows whether it created anything. Minimum `12m40s` (the broker's whole start budget plus a minute). Hot-reloaded. Env: `SCION_SERVER_HUB_STARTUNCONFIRMEDHOLD`. |
| `start_create_unconfirmed_hold` | duration | `"5m"` | `start_unconfirmed_hold` for a new agent's create-and-start. Allowed `3m` up to `start_unconfirmed_hold`. Hot-reloaded. Env: `SCION_SERVER_HUB_STARTCREATEUNCONFIRMEDHOLD`. |
| `cors` | object | | CORS configuration (see below). |
| `conduit` | object | | Conduit relay settings (see [Conduit](#conduit-serverhubconduit)). |

#### CORS (`server.hub.cors`)

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `enabled` | bool | `true` | Enable CORS. |
| `allowed_origins` | list | `["*"]` | Allowed origins. |

#### Conduit (`server.hub.conduit`)

Settings for the in-process conduit relay and its stream grants. They take effect only when the `hub.conduit` [experiment](/scion/reference/experiments/) is on. All of them are read at startup, so a change needs a restart. An invalid value is a startup error, not silently ignored.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `grant_key_activation` | duration | `"15m"` | Delay between publishing a new grant signing key and signing with it. Minimum `"1m"`. Targets must refresh their keys at least this often. Flag: `--conduit-grant-key-activation`. Env: `SCION_SERVER_HUB_CONDUIT_GRANTKEYACTIVATION`. |
| `tcp_allowed_ports` | list of int | `[]` | Additional agent-local ports a TCP stream grant may target, on every agent, besides that agent's exposed ports. The reserved ports (9810, 18380) are always refused. Empty means exposed ports only; this setting never narrows access to exposed ports. Flag: `--conduit-tcp-allowed-ports`. Env: `SCION_SERVER_HUB_CONDUIT_TCPALLOWEDPORTS` (comma-separated). |
| `internal_listen` | string | | `host:port` of the internal relay API listener, used by multi-node deployments. It serves only the internal relay API and must be reachable only inside the cluster or VPC, never publicly. Flag: `--internal-listen`. Env: `SCION_SERVER_HUB_CONDUIT_INTERNALLISTEN`. |
| `internal_advertise` | string | | Base URL (`http(s)://host:port`) other hub nodes use to reach this node's internal listener. Default: `POD_IP` with the listen port, else the listen host if it is not a wildcard. Flag: `--internal-advertise`. Env: `SCION_SERVER_HUB_CONDUIT_INTERNALADVERTISE`. |
| `peer_auth` | string | `"auto"` | Relay-peer authentication: `auto`, `oidc` or `hmac`. Requests between relays are always signed with a key derived from the hub's signing secret, which is required in every mode. `oidc` also requires a Google OIDC ID token, `auto` adds the ID token on GCP, and `hmac` uses the signature alone. Env: `SCION_SERVER_HUB_CONDUIT_PEERAUTH`. |
| `peer_service_accounts` | list | own service account | With OIDC peer auth, the service-account emails allowed to call the internal relay API. The hub logs a warning at startup when the default resolves to a Compute Engine default service account. Env: `SCION_SERVER_HUB_CONDUIT_PEERSERVICEACCOUNTS` (comma-separated). |
| `peer_audience` | string | `"scion-conduit-relay-peer"` | With OIDC peer auth, the ID token audience. It must be identical on every hub node. Env: `SCION_SERVER_HUB_CONDUIT_PEERAUDIENCE`. |
| `reconnect_window` | duration | `"5s"` | Jitter window sent with a planned close: targets redial after a random delay within it. Between `"0s"` and `"5m"`. Flag: `--conduit-reconnect-window`. Env: `SCION_SERVER_HUB_CONDUIT_RECONNECTWINDOW`. |
| `instance_id` | string | see description | This node's relay instance id. It must be unique among live hub processes: a relay that starts with an id already in use takes it over from the other process. Up to 128 printable ASCII characters, no spaces. Default: `POD_NAME` when set, else the host name plus a random per-process suffix. Env: `SCION_SERVER_HUB_CONDUIT_INSTANCEID`. |

**TLS on the internal hop.** Use TLS for the internal relay endpoint (for example a service mesh or a TLS-terminating proxy) and advertise it as `https://`. Plain `http://` is accepted.

**Hosted HA.** In an HA deployment each hub node runs a relay that other nodes must reach directly, so the hub refuses to start when:

- the grant key ring is not stored with the shared at-rest key;
- it runs on Cloud Run (`K_SERVICE` is set), whose instances are not individually addressable;
- `internal_advertise` uses the public hub host or a `*.run.app` host;
- the relay is not addressable at its internal endpoint, or answers its self-check as another instance.

Outside HA, a relay that cannot start is logged and the hub serves without it.

#### Asynchronous agent create

By default, a Hub agent create is synchronous: the Hub holds the create request open while the Runtime Broker provisions the workspace and starts the agent. That request is bounded by the CLI's HTTP client timeout (30s) and by the Hub's `write_timeout` (`60s` by default), so the synchronous path is not suited to agents that take several minutes to start.

With `async_agent_launch: true`, the Hub instead answers as soon as the broker accepts the create. The agent is returned in a pre-running phase with an active launch, and the broker finishes the start in the background:

- **Opt-in per request.** `scion start` and `scion resume` opt in on every Hub create and then poll the agent until it is `running`, `error`, or `stopped`. By default they wait for the Hub's remaining launch budget plus 30 seconds (5 minutes when the Hub does not report a budget); `--wait-timeout` overrides this and `--no-wait` returns once the Hub has accepted. See [`scion start`](/scion/reference/cli/#scion-start-or-run). Scheduled agent creates opt in server-side. Requests that do not opt in are synchronous.
- **The wait is not the launch.** If the CLI stops waiting (the wait budget runs out, or Ctrl-C), the launch continues on the Hub and broker, and the agent still comes up. Run `scion start <agent>` again to resume waiting.
- **Bounded by `launch_timeout`.** A launch that has not reached a terminal Hub state by its deadline is ended: the broker stops it shortly before the deadline, and the Hub sets the agent to `error` with launch error `launch_timeout` shortly after. Starting such an agent is refused with `agent_create_incomplete`; delete it and create it again.
- **Starts during a launch.** While a launch is in progress and before its deadline, a start or restart of that agent returns the launching agent with HTTP 200 instead of starting it again (a restart adds the warning `agent is launching; restart not performed`), and `scion start` keeps waiting. Restore, reincarnate, and wake are refused with `agent_launching`.

**Requirements.** The Hub, the Runtime Broker, and the CLI must all run a version that includes asynchronous create. A broker that reports no async launch support, or that answers the create synchronously, gets the synchronous create, so mixing versions is safe but slow starts on an older broker keep the synchronous limits. A broker runtime that cannot serve an asynchronous launch also falls back to the synchronous create.

**Slow pod starts.** On clusters with slow node provisioning, for example GKE Autopilot cold starts where a new node is added and the agent image takes several minutes to pull, raise `launch_timeout` so the launch is not ended first:

```yaml
server:
  hub:
    async_agent_launch: true
    launch_timeout: "15m"
```

Both keys are read only at Hub startup; restart the Hub after changing them. They cannot be set through the admin server-config API (see [Layer 0](#layer-0--bootstrap-file--env-only)).

### Broker Settings (`server.broker`)

Controls the Runtime Broker service.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `enabled` | bool | `false` | Whether to start the broker service. |
| `port` | int | `9800` | HTTP port to listen on. |
| `broker_id` | string | | Unique UUID for this broker. |
| `broker_name` | string | | Human-readable name. |
| `broker_nickname` | string | | Short display name. |
| `hub_endpoint` | string | | The Hub URL this broker connects to. |
| `container_hub_endpoint` | string | | Overrides `hub_endpoint` when injecting the Hub URL into agent containers. Use when containers cannot reach the Hub at the broker's address (e.g. `http://host.containers.internal:8080` for local development). |
| `broker_token` | string | | Authentication token for the Hub. |
| `auto_provide` | bool | `false` | Automatically add as provider for new projects. |

### Database (`server.database`)

Persistence settings for the Hub.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `driver` | string | `"sqlite"` | Database driver: `sqlite` or `postgres`. |
| `url` | string | `"hub.db"` | Connection string or file path. |

### Authentication (`server.auth`)

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `mode` | string | `"oauth"` | Selects the exclusive human auth mode: `"oauth"` (default), `"proxy"`, or `"dev"`. |
| `dev_mode` | bool | `false` | Enable insecure development authentication (used in `"dev"` mode). |
| `dev_token` | string | | Static token for dev mode. |
| `authorized_domains` | list | `[]` | Limit access to specific email domains. |
| `user_access_mode` | string | `"open"` | Who may sign in: `"open"` (any verified email, subject to `authorized_domains` if set), `"domain_restricted"` (email domain must be in `authorized_domains`), or `"invite_only"` (the email must belong to an invited, allow-listed or existing user). Users in `admin_emails` are always allowed. |
| `default_user_role` | string | `"member"` | Hub role given to a user when their account is first created or activated: first sign-in, including the first sign-in of an invited or allow-listed user. Values: `"member"` or `"viewer"` (`"admin"` is rejected; use `admin_emails`). Users in `admin_emails` are always admins. Changing it does not affect existing users. It is also the role given to an admin who is removed from `admin_emails`. See [Hub roles](/scion/hosted/ha/permissions/#hub-roles). Environment: `SCION_SEED_SERVER_AUTH_DEFAULTUSERROLE` (recommended) or `SCION_SERVER_AUTH_DEFAULTUSERROLE` (per-node, deprecated for Layer-1 keys). |

### Proxy Auth (`server.auth.proxy`)

Proxy authentication configuration (consulted when `server.auth.mode` is set to `"proxy"`). See [Proxy Auth (Google IAP)](/scion/hosted/ha/auth-proxy-iap/) for the full deployment guide, and [Generic JWT proxy provider](/scion/hosted/ha/auth-proxy-iap/#generic-jwt-proxy-provider) for bespoke auth proxies.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `provider` | string | | Selects the proxy auth provider: `"iap"`, `"jwt"`, or `"header"`. |
| `require_trusted_proxy_ip` | bool | `false` | Enables defense-in-depth IP allowlisting. Uses the trusted_proxies CIDR list. |

#### Google IAP Settings (`server.auth.proxy.iap`)

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `audience` | string | | **MANDATORY for IAP.** The expected audience claim (`aud`) in the IAP-signed JWT assertion. Supported formats are Cloud Run native path or GCE/GKE GCLB backend service path. |
| `issuer` | string | `"https://cloud.google.com/iap"` | The expected JWT issuer. Override only for mock/testing setups. |
| `jwks_url` | string | `"https://www.gstatic.com/iap/verify/public_key-jwk"` | The URL to retrieve public keys for signature verification. Override only for testing. |

#### Generic JWT Settings (`server.auth.proxy.jwt`)

Used when `provider` is `"jwt"`. Exactly one key source — `public_key_file`, `jwks_url`, or `jwks_file` — must be set; the Hub refuses to start otherwise.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `header` | string | `"X-Auth-Proxy-JWT"` | HTTP header carrying the signed JWT assertion. |
| `algorithm` | string | | **Required.** The single accepted signing algorithm. Must be asymmetric: `RS256`, `RS384`, `RS512`, `ES256`, `ES384`, `ES512`, `PS256`, `PS384`, or `PS512`. |
| `issuer` | string | | If set, must match the JWT `iss` claim. Empty skips issuer validation. |
| `audience` | string | | If set, must be contained in the JWT `aud` claim. Empty skips audience validation. |
| `public_key_file` | string | | Key source: path to a PEM-encoded public key (PKIX, PKCS1, or an X.509 certificate). Loaded once at startup. |
| `jwks_url` | string | | Key source: JWKS endpoint. Keys are cached and refreshed hourly and on an unknown `kid`, with the last-good key set served if the endpoint fails. |
| `jwks_file` | string | | Key source: path to a local JWKS JSON document. Loaded once at startup; keys are matched by `kid`. Changes require a restart. |
| `claims.email` | string | `"email"` | Claim holding the user's email. The claim is required on every token. |
| `claims.subject` | string | `"sub"` | Claim holding the stable subject ID. Falls back to the email when absent. |
| `claims.display_name` | string | `"name"` | Claim holding the display name. |
| `claims.domain` | string | `"hd"` | Claim holding the hosted domain. |

### Transport Auth (`server.auth.transport`)

Transport auth configuration for the platform guard (IAP or Cloud Run invoker). See [Proxy Auth (Google IAP)](/scion/hosted/ha/auth-proxy-iap/) for the full deployment guide.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `mode` | string | `"none"` | Transport auth mode: `none`, `iap`, or `cloudrun_invoker`. |
| `oidc_audience` | string | | OIDC audience for transport tokens. For `iap`: the IAP OAuth client ID. For `cloudrun_invoker`: the Hub URL (auto-derived from `hub.public_url` if empty). |
| `platform_auth_sa` | string | | Dedicated service account the Hub impersonates to mint OIDC ID tokens for agents. This account is never provisioned as a Hub user and is never issued user credentials, even if it signs in. |

#### Agent transport environment variables

When transport auth is configured, the Hub injects these environment variables into agent containers at dispatch time:

| Variable | Description |
| :--- | :--- |
| `SCION_TRANSPORT_TOKEN` | Initial Google OIDC ID token for the transport layer. Bootstrap only: `sciontool init` moves it to `~/.scion/transport-token` and removes it from the child environment. If the file cannot be written, the value stays in the environment and is used until it expires. |
| `SCION_TRANSPORT_TOKEN_FILE` | Set by `sciontool init` for child processes. Path of the transport token file, which every refresh rewrites. |
| `SCION_TRANSPORT_AUDIENCE` | Audience the transport token was minted for. |
| `SCION_TRANSPORT_TOKEN_EXPIRY` | Expiry of the initial token, in RFC 3339 format. Bootstrap only: removed by `sciontool init` together with `SCION_TRANSPORT_TOKEN`. |
| `SCION_TRANSPORT_MODE` | Transport mode (`iap` or `cloudrun_invoker`). Injected alongside the other three transport vars so that in-agent clients can select the correct header placement. |

#### Broker transport configuration

Brokers are long-lived originators that mint their own OIDC tokens (via GKE Workload Identity or ambient GCE SA). Transport settings are configured via environment variables or per-connection credentials-file fields.

**Environment variables** (for containerized brokers):

| Variable | Description |
| :--- | :--- |
| `SCION_TRANSPORT_MODE` | Transport mode: `iap` or `cloudrun_invoker`. |
| `SCION_TRANSPORT_AUDIENCE` | OIDC audience — the custom OAuth 2.0 Client ID (for `iap`) or Hub URL (for `cloudrun_invoker`). |

**Credentials-file fields** (per hub connection, persisted by `scion hub brokers register`):

| Field | Type | Description |
| :--- | :--- | :--- |
| `transportMode` | string | Transport mode: `iap` or `cloudrun_invoker`. |
| `transportAudience` | string | OIDC audience for the transport token. |

Environment variables override credentials-file values. Per-connection credentials-file fields support the multi-hub scenario where each hub has a different IAP OAuth client ID.

### OAuth (`server.oauth`)

OAuth provider credentials.

```yaml
server:
  oauth:
    web:
      google: { client_id: "...", client_secret: "..." }
      github: { client_id: "...", client_secret: "..." }
    cli:
      google: { client_id: "...", client_secret: "..." }
```

### Storage (`server.storage`)

Backend for storing templates and artifacts.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `provider` | string | `"local"` | Storage provider: `local` or `gcs`. |
| `bucket` | string | | GCS bucket name. |
| `local_path` | string | | Local path for storage. |

### Secrets (`server.secrets`)

Backend for managing encrypted secrets. The `local` backend is read-only and rejects secret write operations. Configure `gcpsm` to enable full secret management.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `backend` | string | `"local"` | Secrets backend: `local` or `gcpsm`. The `local` backend rejects writes; use `gcpsm` for production. |
| `gcp_project_id` | string | | GCP Project ID for Secret Manager. Required when `backend` is `gcpsm`. |
| `gcp_credentials` | string | | Path to GCP service account JSON or the JSON content itself. Optional if using Application Default Credentials. |
| `gcp_replication_locations` | list of strings | `[]` | GCP regions for user-managed secret replication (e.g. `["us-east1", "europe-west1"]`). When non-empty, secrets are created with user-managed replication restricted to these regions instead of automatic global replication. Required for GCP orgs enforcing `constraints/gcp.resourceLocations`. See [User-Managed Replication Locations](/scion/hosted/user/secrets/#user-managed-replication-locations). |

### Workspace Storage (`server.workspace_storage`)

Configures the backend and mount settings for storing and managing agent workspaces. This is a critical setting for high-availability deployments where multiple Hub and Broker replicas need shared, durable access to project workspaces.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `backend` | string | `"local"` | Storage backend pivot: `"local"` (node-local directories), `"nfs"` (Network File System mounts), `"cloudrun-volume"` (Cloud Run platform-managed volume mounts), or `"gke-shared-volume"` (GKE shared CSI-backed PVC mounts). Names are case-sensitive; any other value stops the Hub at startup. |
| `nfs.mount_root` | string | | The host base directory under which NFS exports are mounted. |
| `nfs.mount_options` | string | `"vers=3,hard,nconnect=4,_netdev"` | Standard mount options passed to the `mount.nfs` utility. |
| `nfs.auto_mount` | boolean | `false` | Whether the Runtime Broker mounts the shares itself. See [NFS Mounts on the Runtime Broker](#nfs-mounts-on-the-runtime-broker). Requires the broker to run as root. |
| `nfs.uid` | integer | `1000` | Node-independent owner UID for NFS-backed workspace trees to ensure consistent container write permissions (not yet applied on Kubernetes; ptone/scion#2608). |
| `nfs.gid` | integer | `1000` | Node-independent owner GID for NFS-backed workspace trees. |
| `nfs.storage_class` | string | | The Kubernetes StorageClass name used to dynamically allocate volumes on GKE. |
| `nfs.subpath_root` | string | `"projects"` | The base folder within the share for project workspaces. See [subpath_root](#subpath_root). |
| `nfs.shares` | list of objects | `[]` | List of NFS share objects. Each share requires: `id` (stable ID), `server` (IP address or hostname), `export` (exported path, e.g., `/scion-workspaces`), and optional `pv_name` (for GKE). |
| `cloudrun_volume.volume_name` | string | | **Required** when `backend` is `"cloudrun-volume"` (the settings schema checks this only for the selected backend). The name of the platform volume declared in the Cloud Run service specification. The Hub resolves workspaces under `/mnt/<volume_name>`, which is where Cloud Run mounts a declared volume. If it is missing or empty, the Hub refuses to start. |
| `cloudrun_volume.subpath_root` | string | `"projects"` | Sub-directory prefix within the Cloud Run volume. See [subpath_root](#subpath_root). |
| `gke_shared_volume.volume_name` | string | | **Required** when `backend` is `"gke-shared-volume"`; if it is missing or empty, the Hub refuses to start. The K8s volume name referencing the persistent volume claim (PVC). **The pod spec must mount that volume at `/mnt/<volume_name>`**: the Hub derives every workspace path from it, and a pod that mounts the PVC elsewhere fails readiness (`GET /readyz` returns `503`) rather than writing workspaces to ephemeral container storage. |
| `gke_shared_volume.pv_claim_name` | string | | The name of the GKE-managed PVC bound to the shared storage backend (e.g. Filestore). |
| `gke_shared_volume.subpath_root` | string | `"projects"` | Sub-directory prefix within the GKE volume. See [subpath_root](#subpath_root). |

#### Startup validation

The Hub checks `workspace_storage` when it starts, and refuses to start with an error naming the bad field when:

- `backend` is not one of the four names above;
- `backend` is `"nfs"` and `nfs.shares` is empty;
- `backend` is `"cloudrun-volume"` or `"gke-shared-volume"` and the matching `volume_name` is missing or empty;
- the selected backend's `subpath_root` is invalid (see below).

A Runtime Broker that runs without the Hub does not refuse to start. It only logs warnings:

- If the `nfs` block is incomplete, the broker warns and skips its NFS mount checks (see [NFS Mounts on the Runtime Broker](#nfs-mounts-on-the-runtime-broker)).
- If the selected backend's `subpath_root` is invalid, the broker warns at startup. Its NFS mount checks still run, because they do not use `subpath_root`. However, every agent start that uses that backend fails with a `subpath_root` error until the value is fixed. This includes values such as `projects/` or `./projects`, which earlier versions accepted and normalized.

The Hub's readiness check (`GET /readyz`) and its Cloud Run write guard still apply as further safeguards. A volume backend without a mount point fails readiness, and blocks workspace writes on Cloud Run.

#### subpath_root

`subpath_root` is the directory inside the share or volume that holds project trees, at `<subpath_root>/<project-id>/...`. It defaults to `"projects"` for every backend, and for `shared_dir_storage.nfs`. It must be a clean relative path: no leading `/`, no `.` or `..` components, no empty components and no trailing `/`. If a value is not clean, the error names the clean value to use instead (for example `projects` for `projects/`). It may have several components, for example `team/projects`. The Cloud Run runtime's NFS export paths use the same `nfs.subpath_root`.

#### NFS Mounts on the Runtime Broker

When `backend` is `nfs`, the Runtime Broker reads this block from its global `settings.yaml` (never from project settings) and checks each share at `<mount_root>/<share id>`. The block must have an absolute `mount_root`, and every share needs a unique single-segment `id`, a `server` (a hostname or IPv4 address, without whitespace or a leading `-`; IPv6 literals are not supported), and an absolute `export`. If the block is incomplete, the broker logs a warning and skips NFS handling. With any other backend, the broker does no NFS handling at all.

- **`auto_mount: false` (default)**: The broker only checks. It reads the host mount table (`/proc/mounts`) to confirm each share is mounted from the expected `server:export`, at startup and then every minute. It never mounts or unmounts anything and never refuses agent creation. Mount each export on the host yourself, for example with `/etc/fstab`.
- **`auto_mount: true`**: The broker mounts missing shares in the background, and remounts a share that is mounted from the wrong source. The broker must run as root: if it does not, it logs a warning at startup and only checks, without running `mount`. If the broker's default runtime is Kubernetes or Cloud Run, it also only checks, because the platform mounts the export into the agent. Each mount command times out after 90 seconds, or sooner if the agent-create request ends.

With `auto_mount: true`, agent creation for a project on the `nfs` backend is decided by the runtime the agent is dispatched to. This check runs before anything is mounted. Only agent creation is checked; starting or restarting an existing agent is not.

- **Docker, Podman, or Apple**: The broker first makes sure the first share is mounted, because that share holds the workspaces. If it is not mounted, the broker returns `503` with error code `nfs_unavailable`.
- **Kubernetes or Cloud Run**: The broker never mounts and never refuses the request. If the share's last check found a problem, it logs a warning and continues.

In both modes, NFS problems are logged and reported per share in the `nfs_mounts` check of `GET /healthz`. The overall status becomes `degraded` only if all of these hold: `auto_mount` is on, the default runtime is not Kubernetes or Cloud Run, the first check has finished, and a share is unhealthy. NFS problems never affect `GET /readyz`, and the broker keeps serving projects that do not use NFS. `scion doctor` runs the same check locally, using the same mount-table lookup. It reports whether NFS is configured, whether each share is mounted from the expected source, and whether each server is reachable on TCP port 2049. A share that is not mounted is a warning when `auto_mount` is off, the share has a `pv_name`, or the default runtime is Kubernetes or Cloud Run (so the broker does not mount it, as in `/healthz`), and a failure otherwise.

#### NFS Workspaces on Kubernetes

With the `nfs` backend and a bound PV claim (`nfs.shares[].pv_name`), each Kubernetes agent pod gets a `workspace-provision` init container. It runs for both git and non-git agents. It creates the per-project subPath (or, if another pod is already provisioning it, waits for that pod to finish) and chowns it to the agent runtime uid (`1000`) and `nfs.gid` so the agent can write `/workspace`; `nfs.uid` is not yet applied on Kubernetes (ptone/scion#2608). For git agents, it also clones the repository. The init container runs as root with only the `CHOWN`, `FOWNER`, and `DAC_OVERRIDE` capabilities and does not follow symlinks. If the chown fails, the agent start fails and the error names the failed init container, so the agent never runs with an unwritable workspace.

Pods get `fsGroup` from `nfs.gid` (default `1000`). With `server.shared_dir_storage.backend: nfs`, Scion also adds each shared directory's own group to the pod's supplementary groups (see [Agent groups](#agent-groups-on-nfs-shared-directories)), so `nfs.gid` does not need to match it. Shared directories served from the workspace export (`workspace_storage` set to `nfs` without `shared_dir_storage`) do not get that group: there, set `nfs.gid` to the shared-directory leaf group, otherwise pods lose group access to the leaf.

#### Ephemeral Storage & 503 Safety Gate

To protect deployments from silent data loss, the Hub implements a strict **503 Safety Gate**:
* If the Hub is deployed on serverless environments like Google Cloud Run with the `local` backend selected, its local workspace paths map to ephemeral, non-durable container storage.
* The Hub detects this non-durable state and automatically intercepts all file write and modification endpoints (including WebDAV, inline file editing, and git cloning).
* Affected endpoints will return `503 Service Unavailable` with a descriptive message rather than allowing writes to persist ephemerally on the container's scratch space, enforcing the transition to a durable backend (`nfs`, `cloudrun-volume`, or `gke-shared-volume`) for production.

### Shared Directory Storage (`server.shared_dir_storage`)

Selects where project [shared directories](/scion/local/workspace/#5-project-shared-directories) are stored, independently of `server.workspace_storage`. With the `nfs` backend, every Runtime Broker resolves a project's shared directories to the same path on one NFS export, so agents on different brokers — Docker or Kubernetes — see the same files.

This setting is **global-only**: each broker process reads it from its own global `settings.yaml`, never from project settings. It is a [Layer-0](#layer-0--bootstrap-file--env-only) setting and requires a restart to take effect.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `backend` | string | `"local"` | `"local"` (shared directories live next to the project's local config) or `"nfs"`. Any other value, including a different case, is rejected. |
| `nfs.mount_root` | string | | **Required for `nfs`.** Host directory under which the share is mounted, at `<mount_root>/<shares[0].id>`. Docker, Podman, and Apple runtimes bind-mount from here. |
| `nfs.shares` | list of objects | `[]` | **Required for `nfs`.** Only the first entry is used. `id` is required. `pv_name` names the static PersistentVolumeClaim that Kubernetes pods mount by `subPath`, and is required for Kubernetes brokers. |
| `nfs.subpath_root` | string | `"projects"` | Directory within the share that holds per-project trees. Must be a relative path. |
| `nfs.gid` | integer | | Optional. The only shared-directory group that agents may be given as a supplementary group. When set, a leaf owned by any other group is skipped with a warning. See [Agent groups](#agent-groups-on-nfs-shared-directories). |

Shared directories resolve to `<mount_root>/<share id>/<subpath_root>/<project id>/shared-dirs/<name>`. On Kubernetes, pods mount the `pv_name` claim with the matching `subPath` instead of creating a per-directory PVC.

The `nfs` backend fails closed. Agent start is refused when the block is incomplete, the host base directory does not exist, the runtime is not a local-container or Kubernetes runtime (for example, Cloud Run), or a shared-directory path resolves through a symlink. The NFS export itself must be provisioned and mounted before agents start. The `uid`, `mount_options`, `storage_class`, and `auto_mount` fields of the `nfs` block are ignored here.

With the `nfs` backend, the Hub and brokers also apply the following:

- **Symlink-safe access**: Every Hub operation on an NFS shared directory goes through the same confined resolver. This covers the web file browser, archive downloads, attachment staging, and shared-dir deletion. The resolver walks each path component with `O_NOFOLLOW`, anchored on the inode of the project's tree, and refuses any symlink in the path. A missing or incomplete `nfs` block, or an unusable host base directory, fails closed on the Hub as well as on agent start.
- **Leaf modes and ACLs**: A newly created shared directory gets mode `2775` (setgid, group-writable) and a minimal default POSIX ACL, so files agents create inside it inherit group write access regardless of umask. If the export does not support POSIX ACLs, a warning is logged once and the directory stays plain `2775` with no ACL. Files created inside such a directory follow each writer's umask (usually `022`), so they are not group-writable. Directories that already existed are never modified. See the [hybrid tier guide](https://github.com/GoogleCloudPlatform/scion/blob/main/docs/deploy/hybrid-tier.md) for the manual fix-up recipe.
- **Ownership on an export that does not squash ids**: the broker creates the project chain as its own user and never changes ownership. Upper directories get `2755` and the leaf `2775`, and each inherits the group of a setgid parent. Pods create nothing on this export; they mount the existing leaf by `subPath`. When agents with different uids share a directory, for example Docker agents and Kubernetes pods, give the share directory (`<mount_root>/<share id>`) a shared group with the setgid bit (for example `chgrp <gid>` and `chmod 2775`) so every leaf inherits it. Scion then adds that group to each agent that mounts the leaf; see [Agent groups](#agent-groups-on-nfs-shared-directories).
- **Cleanup on delete**: Deleting a project removes its `<subpath_root>/<project id>/shared-dirs` tree from the export. Removing a single shared directory removes that directory's contents. Both are best-effort: failures are logged and never block or roll back the database change.
- **Startup summary**: At startup the server logs one `server.shared_dir_storage resolved layout: …` line, plus a warning if any ignored `nfs` fields are set.

#### Agent groups on NFS shared directories

Different kinds of agents can write to the same NFS shared directory (leaf): Docker or rootful Podman agents on brokers, and Kubernetes pods. They usually run with different uids, so each one can modify the others' files only through the leaf's group. For that to work:

- **The export must support POSIX ACLs.** The leaf's default ACL makes new files group-writable whatever the writer's umask. Without ACL support, files follow each writer's umask (usually `022`) and other writers cannot modify them. Scion logs a warning once when it cannot set the ACL. ACL-capable storage is required for shared directories with mixed writers.
- **Every writer must be in the leaf's group.** At each agent start, the broker reads the group of every NFS shared directory the agent mounts, from the leaf itself, and adds it to the agent:
  - Kubernetes: added to the pod's `supplementalGroups`. `fsGroup` is not changed, and a group equal to `fsGroup` is not repeated.
  - Docker and rootful Podman: added with `--group-add`. The agent image's `sciontool` must keep these groups when it switches from root to the agent user; older images drop them and keep the previous behaviour.
  - Rootless Podman and Apple containers: not supported; the broker logs a warning and starts the agent without the group.

  For safety, the broker skips a group (with a warning) when it is below `1000`, when it is an overflow id (`65534` or `4294967294`, which NFSv4 id mapping reports for unmapped groups), or when `nfs.gid` is set and does not match. If the group cannot be read, the agent starts without it. Agents without an NFS shared directory are unchanged. On an `all_squash` export the server maps every client to one identity, so the added group has no effect there.

Some files are not upgraded:

- Files created by a writer that passes an explicit restrictive mode (for example `open(..., 0644)`) stay non-group-writable; an ACL cannot add permissions the creator did not request.
- Leaves created outside Scion, or before Scion added the leaf ACL, keep their existing mode and ACL. Scion only sets modes and ACLs on leaves it creates. Fix them by hand (see the hybrid tier guide).

The `local` backend (or an unset `shared_dir_storage`) behaves as before.

```yaml
server:
  shared_dir_storage:
    backend: nfs
    nfs:
      mount_root: /mnt/scion-nfs
      subpath_root: projects
      shares:
        - id: shared
          server: 10.0.0.2
          export: /scion-shared
          pv_name: scion-shared-pvc
```

#### Per-profile backend

A runtime entry or a profile can override the backend with `shared_dir_storage_backend` (`local` or `nfs`). This lets one broker keep its Docker profile on `local` while a Kubernetes profile uses `nfs`. The backend for an agent is resolved when it starts, in this order:

1. `profiles.<name>.shared_dir_storage_backend` for the agent's profile.
2. `runtimes.<name>.shared_dir_storage_backend` for that profile's runtime entry.
3. `server.shared_dir_storage.backend`.

The `nfs` details always come from `server.shared_dir_storage.nfs`, so an `nfs` override needs a complete `nfs` block there. Settings validation rejects an `nfs` override without one. Validation checks configuration only and never looks at the mount. On a Hub that stores runtimes and profiles in the database, `server.shared_dir_storage.nfs` is edited only in `settings.yaml`, and such an edit is not checked against the overrides stored in the database; an `nfs` override left without a complete block fails at agent start with an error that names the key.

```yaml
runtimes:
  docker:
    type: docker
  gke:
    type: kubernetes
    shared_dir_storage_backend: nfs
profiles:
  local:
    runtime: docker
  gke:
    runtime: gke
server:
  shared_dir_storage:
    backend: local
    nfs:
      mount_root: /mnt/scion-nfs
      shares:
        - id: shared
          pv_name: scion-shared-pvc
```

- **Chosen from global settings**: like `server.shared_dir_storage`, the overrides are read from the broker's global settings, never from project settings. On a co-located Hub and broker whose runtimes and profiles are stored in the database, the stored values apply. This picks the backend for an agent's first start; after that, the agent's recorded backend applies (see below).
- **No restart**: overrides are read again at every agent start, so a change made in `settings.yaml` or through the Hub settings API applies to the next agent start.
- **Recorded per agent**: an agent records the backend its shared directories were set up with and keeps it on later starts, even if the settings change.
  - The record is `shared-dir-storage.json` in the agent's directory on the broker, next to `scion-agent.json`. It is outside the agent's home. In the default layouts the agent's container does not mount it. In two older layouts the agent's directory sits inside the workspace mount, so the container sees the record there, as it sees `scion-agent.json`: a non-git project whose `.scion` directory is inside the project, and a shared-workspace git project without an external agents directory. Reincarnating the agent keeps the record, and moving a shared-workspace agent's state out of the project moves the record with it.
  - If an agent recorded `nfs` and the `nfs` block was later removed, its start fails with an error that says so, before any host path is touched.
  - An override that later fails validation does not block an agent that recorded a backend, because the agent does not use it. It still fails the first start of a new agent.
  - A start that could not load the global settings records nothing, so the agent picks up its configured backend once the settings load again.
  - Agents created before the backend was recorded use the current resolution.
- **Host mount**: a broker that starts an `nfs`-resolved agent needs the export mounted at `<mount_root>/<share id>`, as with the global `nfs` backend. A missing mount fails only agents that resolve to `nfs`. Agents on the `local` backend, server startup, and health checks are not affected. The startup log has one line per profile whose backend comes from an override.
- **Hub file browser and attachments**: the Hub's file browser, archive downloads and attachment staging use `server.shared_dir_storage.backend` only, not the per-profile override.
- **Cleanup on delete**: deleting a project removes its tree from the export whenever `server.shared_dir_storage.nfs` is complete, whatever the backend settings select. An agent can still be on `nfs` by its record after every setting has moved to `local`, and the Hub cannot read records kept on brokers. If the global backend is not `nfs` and the export is not mounted on the Hub's host, cleanup logs a warning and the delete still succeeds.

### Agent Home Storage (`server.home_storage`)

Selects where the home directory of Kubernetes agents lives. With the default `local` backend the home is inside the pod and is filled from the broker's copy at every start. With `nfs`, each agent's home is a directory on the NFS export of its profile's [shared-dir storage](#shared-directory-storage-servershared_dir_storage), kept across stops, restarts and pod replacements.

The `nfs` backend is in development. It takes effect only when the hub's `hub.k8s_nfs_home` [experiment](/scion/reference/experiments/) is on and `allow_incomplete_phases` is set. The export's group must be the pod group (gid 1000). See [Persistent Agent Home](/scion/hosted/ha/kubernetes/#persistent-agent-home-nfs) for how the home is created and used.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `backend` | string | `local` | `local` or `nfs`. A runtime entry or profile can override it with `home_storage_backend`. |
| `leaf` | string | `pod` | How an agent's home directory is created on the export: `pod` (an init container in the agent's pod) or `broker` (the broker, through its own mount of the export at the shared-dir storage `mount_root`). A runtime entry or profile can override it with `home_storage_leaf`. |
| `stop_grace_seconds` | int | `30` | Termination grace period of pods with an NFS home. |
| `termination_wait_seconds` | int | `15` | How long a start waits, beyond the grace period, for the agent's previous pod to stop. Keep `stop_grace_seconds` plus this below 90 (the hub's start window; the broker request timeout is 120); the server warns at startup and `scion config validate` warns when it is not. |
| `skeleton_max_bytes` | int | `268435456` | Largest image home skeleton copied into a new home. |
| `allow_incomplete_phases` | bool | `false` | Development only. Allows the `nfs` backend while the feature is incomplete. |

The backend and leaf mode for an agent are resolved when it first starts, each in this order:

1. `profiles.<name>.home_storage_backend` (or `home_storage_leaf`) for the agent's profile.
2. `runtimes.<name>.home_storage_backend` (or `home_storage_leaf`) for that profile's runtime entry.
3. `server.home_storage.backend` (or `leaf`).
4. `local` (or `pod`).

```yaml
server:
  shared_dir_storage:
    backend: local
    nfs:
      mount_root: /mnt/scion-nfs
      shares:
        - id: shared
          pv_name: scion-shared-pvc
  home_storage:
    allow_incomplete_phases: true
runtimes:
  gke:
    type: kubernetes
profiles:
  gke:
    runtime: gke
    shared_dir_storage_backend: nfs
    home_storage_backend: nfs
    home_storage_leaf: pod
  docker:
    runtime: docker
```

- **Kubernetes only**: agents on any other runtime always get a local home, whatever the settings say. An `nfs` value on a non-Kubernetes runtime entry or profile is accepted with a warning.
- **Share**: the home uses the first share and claim of the profile's resolved `shared_dir_storage` `nfs` block. A profile that selects `home_storage_backend: nfs` without `shared_dir_storage` `nfs` fails to start agents, with an error naming the profile.
- **Hub agents only**: an NFS home is named after the agent's hub ID, at `<subpath_root>/<project id>/agents/<agent slug>/home-<agent id>` on the export. A start without a hub agent ID fails.
- **Chosen from global settings**: like `server.shared_dir_storage`, the per-profile and per-runtime keys are read from the broker's global settings, never from project settings. On a co-located Hub and broker whose runtimes and profiles are stored in the database, the stored values apply, and an edit takes effect at the next agent start with no restart. `server.home_storage` itself is read from `settings.yaml` only.
- **Recorded per agent**: a new agent's home storage is decided at its first start and recorded in `home-storage.json` in the agent's directory on the broker, next to `shared-dir-storage.json`. Later starts, restarts and reincarnations use the record, so a settings change never moves an existing home. A recorded `nfs` home whose share is no longer configured, or whose agent is started with the experiment off, fails to start rather than getting a new, empty home. Agents created before the record existed keep a local home.
- **Startup summary**: the startup log has a warning for each invalid value and one line per profile that resolves to `nfs`.

### Scheduler (`server.scheduler`)

Controls the background task scheduler in the Hub. This regulates the tick interval and concurrency of recurring maintenance tasks (such as telemetry aggregation, session cleanups, and heartbeats) to match database capacity.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `interval_seconds` | integer | `60` | The root ticker interval in seconds. All recurring background tasks fire at multiples of this interval. Increasing this value reduces database connection pressure on smaller deployments. |
| `max_concurrency` | integer | `2` | Limits the number of recurring maintenance tasks that can execute concurrently in a single tick. By default, this is capped at `2` to avoid database connection pool saturation. Set to `0` for unlimited concurrency (legacy behavior) or a higher value for larger deployments. |

:::note[Database Stability]
Configuring a modest concurrency limit (such as the default `2`) is highly recommended for small or single-node database instances to prevent sudden spikes in database connection usage.
:::

### Maintenance (`server.maintenance`)

Controls how the Hub checks for and applies its own updates. The Hub dispatches update checks by **deployment tier**: a `binary` Hub reads the `LATEST.json` release manifest and the GitHub Releases API, while a `source` Hub checks its git checkout for new commits.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `deployment_tier` | string | `binary` if no repository path is configured, otherwise `source` | Update strategy. `binary` updates from GitHub Releases; `source` updates from a git checkout. The single-node VM deploy script sets `binary`. |
| `release_channel` | string | Detected from the running version | Release channel to track: `stable`, `preview`, or `nightly`. When empty, the channel is derived from the version string (`v0.5.0` → `stable`, `v0.5.0-rc1` → `preview`, `nightly-*` → `nightly`). Development builds have no channel and skip scheduled checks. |
| `update_policy` | string | `auto` for `binary`, `disabled` for `source` | `auto` checks on a schedule and installs updates automatically. `notify` checks on a schedule and shows an update-available banner in the admin UI, where an admin applies or dismisses it. `disabled` turns off scheduled checks; manual checks from the admin UI still work. |
| `check_interval_hours` | integer | `6` | How often the scheduled release check runs. Minimum `1`. Each run is jittered by up to ±30 minutes. |
| `github_repo` | string | `GoogleCloudPlatform/scion` | GitHub repository used for release and manifest lookups. |

Scheduled checks run only when `deployment_tier` is `binary` and `update_policy` is not `disabled`. A binary update downloads the release tarball, verifies the new binary's version, backs up the current binary, installs the new one, and restarts the `scion-hub` systemd service. If the install fails, the backup is restored.

The related admin endpoints are `POST /api/v1/admin/maintenance/check-updates` (manual check) and `GET` / `DELETE` on `/api/v1/admin/maintenance/update-available` (read or dismiss a pending update notification). Both require the `hub.maintenance.execute` permission.

```yaml
server:
  maintenance:
    deployment_tier: "binary"
    release_channel: "stable"
    update_policy: "notify"
    check_interval_hours: 12
```

### OIDC Identity Provider (`server.oidc`)

Configuration for the Hub's built-in OIDC Identity Provider feature.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `enabled` | bool | `false` | Enable the OIDC Identity Provider endpoints. |
| `issuer_url` | string | | The public issuer URL of this Hub. If empty, the hub public URL is used. |
| `token_lifetime` | duration | `"15m"` | Validity duration for minted OIDC identity tokens (e.g. `"15m"`, `"1h"`). |

### OIDC Federation (`server.federation`)

Configuration for inbound OIDC-based federation authentication.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `enabled` | bool | `false` | Enable OIDC federation authentication. |
| `trusted_issuers` | list of objects | `[]` | List of trusted OIDC issuers (see below). |
| `algorithms` | list of strings | `["RS256"]` | Supported cryptographic signing algorithms. |
| `cache.refresh_interval` | duration | `"1h"` | How often to refresh cached issuer public keys (JWKS). |
| `cache.debounce_interval` | duration | `"1s"` | Min interval between JWKS reload attempts to prevent DDOS. |

#### Trusted Issuer Settings (`server.federation.trusted_issuers[]`)

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `issuer_url` | string | | **MANDATORY.** The exact OIDC issuer URL (matching token `iss` claim). |
| `jwks_url` | string | | The URL to fetch signing public keys. Discovered via OIDC discovery if empty. |
| `expected_audience` | string | | The expected audience `aud` claim in tokens. Required (non-empty) for a `user`-type Google issuer to enable external bearer tokens. |
| `allowed_projects` | list of strings | | If set, restricts tokens to specific project UUIDs. |
| `allowed_root_users` | list of strings | | If set, restricts tokens to specific root user emails. |
| `default_scopes` | list of strings | | Default JWT scopes granted to federated agents. |
| `issuer_type` | string | `"hub"` | Type of issuer: `"hub"`, `"service_account"`, or `"user"`. |
| `default_role` | string | `"viewer"` | Default role for federated users (`issuer_type: user`). |
| `allowed_emails` | list of strings | | Restrict user tokens to specific email claims (supports wildcards e.g. `*@example.com`). |
| `allowed_domains` | list of strings | | Google issuer only. Restrict **user** principals presenting an [external bearer token](/scion/hosted/single-node/auth/#external-bearer-tokens-google-credential-pass-through) to these email domains (exact, case-insensitive, no wildcards or subdomain matching). The Hub sign-in policy still applies. Never consulted for service accounts. |
| `allowed_gcp_projects` | list of strings | | Google issuer only. Admit **service-account** principals whose GCP project ID (parsed from the service account email) is listed. Empty admits no service accounts. Distinct from `allowed_projects`, which matches Scion project IDs. |

### OIDC Login (`server.oidc_login`)

Configuration for an external OIDC provider used for Web UI user login.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `enabled` | bool | `false` | Enable the external OIDC login provider. |
| `display_name` | string | | The human-readable label shown on the login button (e.g. `"Corporate SSO"`). |
| `issuer_url` | string | | The exact OIDC issuer URL. Used to perform discovery via `{issuer_url}/.well-known/openid-configuration`. |
| `client_id` | string | | The OAuth2/OIDC Client ID. |
| `client_secret` | string | | The OAuth2/OIDC Client Secret (can be empty for public OIDC clients). |
| `scopes` | list of strings | `["openid", "email", "profile"]` | Overrides the default scopes requested during login. |

### Project Defaults (`project_defaults`)

Configuration for project-level default behaviors across the Hub. Unlike most other server configurations, `project_defaults` is declared as a **top-level section** in `settings.yaml` (outside of the `server:` block).

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `default_scratchpad` | bool | `true` | If enabled, automatically provisions a default `scratchpad` shared directory when a new project is created. |

**Example:**
```yaml
# Declared at the top level of settings.yaml
project_defaults:
  default_scratchpad: true
```

## Environment Variables

:::tip[Database Mode]
When running with a postgres database, operational settings (Layer-1) can be configured via `SCION_SEED_*` environment variables and managed in the admin UI. See the [Admin Settings Model](/scion/reference/admin-settings/) for details on the seeded/managed lifecycle and the `SCION_SEED_*` namespace.
:::

Most server settings can be overridden via environment variables using the `SCION_SERVER_` prefix. Write each path segment in upper case and drop the underscores inside a multi-word field name: `read_timeout` becomes `READTIMEOUT`, not `READ_TIMEOUT`. A name that the Hub does not recognise is ignored and logged as a warning at startup, with a suggested spelling where one exists. Each key's working variable is listed as `x-env-var` in the [settings schema](https://github.com/GoogleCloudPlatform/scion/blob/main/pkg/config/schemas/settings-v1.schema.json).

There are two exceptions to the pattern:

- The broker's listener settings under `server.broker` use the `RUNTIMEBROKER` segment, for example `server.broker.port` -> `SCION_SERVER_RUNTIMEBROKER_PORT`.
- The broker identity keys keep their underscores: `server.broker.broker_id` -> `SCION_SERVER_BROKER_BROKER_ID`, and likewise `BROKER_BROKER_NAME`, `BROKER_BROKER_NICKNAME`, `BROKER_BROKER_TOKEN` and `BROKER_AUTO_PROVIDE`.

`server.log_format` and `server.env` have no environment variable. There is no boot-time override for `server.log_level`. `SCION_SERVER_LOGLEVEL` only affects the level applied when a file-mode admin server-config save or reload re-reads the config. At startup, use `--debug` or `SCION_LOG_LEVEL=debug`.

**Examples:**
- `server.hub.port` -> `SCION_SERVER_HUB_PORT`
- `server.hub.gcp_project_id` -> `SCION_SERVER_HUB_GCPPROJECTID`
- `server.hub.gcp_iam_check_mode` -> `SCION_SERVER_HUB_GCPIAMCHECKMODE`
- `server.hub.gcp_iam_deny_unknown_policy` -> `SCION_SERVER_HUB_GCPIAMDENYUNKNOWNPOLICY`
- `server.hub.admin_emails` -> `SCION_SERVER_HUB_ADMINEMAILS`
- `server.hub.stalled_threshold` -> `SCION_SERVER_HUB_STALLEDTHRESHOLD`
- `server.auth.user_access_mode` -> `SCION_SERVER_AUTH_USERACCESSMODE`
- `server.broker.enabled` -> `SCION_SERVER_RUNTIMEBROKER_ENABLED`
- `server.broker.container_hub_endpoint` -> `SCION_SERVER_RUNTIMEBROKER_CONTAINERHUBENDPOINT`
- `server.broker.broker_id` -> `SCION_SERVER_BROKER_BROKER_ID`
- `server.database.url` -> `SCION_SERVER_DATABASE_URL`
- `server.auth.dev_mode` -> `SCION_SERVER_AUTH_DEVMODE`
- `server.secrets.backend` -> `SCION_SERVER_SECRETS_BACKEND`
- `server.secrets.gcp_project_id` -> `SCION_SERVER_SECRETS_GCPPROJECTID`
- `server.secrets.gcp_credentials` -> `SCION_SERVER_SECRETS_GCPCREDENTIALS`
- `server.secrets.gcp_replication_locations` -> `SCION_SERVER_SECRETS_GCPREPLICATIONLOCATIONS`
- `server.scheduler.interval_seconds` -> `SCION_SERVER_SCHEDULER_INTERVALSECONDS`
- `server.scheduler.max_concurrency` -> `SCION_SERVER_SCHEDULER_MAXCONCURRENCY`

### Logging Environment Variables

These environment variables control server-side logging behavior. They are not part of the `settings.yaml` structure.

| Variable | Description | Default |
| :--- | :--- | :--- |
| `SCION_LOG_GCP` | Enable GCP Cloud Logging JSON format on stdout | `false` |
| `SCION_LOG_LEVEL` | Set to `debug` to log at DEBUG level from startup. Any other value leaves the level at `info`. | `info` |
| `SCION_CLOUD_LOGGING` | Send logs directly to Cloud Logging via client library | `false` |
| `SCION_CLOUD_LOGGING_LOG_ID` | Log name in Cloud Logging for application logs | `scion` |
| `SCION_GCP_PROJECT_ID` | GCP project ID for Cloud Logging (priority 1) | auto-detect |
| `GOOGLE_CLOUD_PROJECT` | GCP project ID for Cloud Logging (priority 2) | - |
| `SCION_SERVER_REQUEST_LOG_PATH` | Write HTTP request logs to a file at this path. Each line is a JSON object in `HttpRequest` format. When not set, request logs follow the default routing (stdout in background mode, suppressed in foreground mode, Cloud Logging when enabled). | (disabled) |

See the [Local Development Logging guide](/scion/contributing/logging/) for details on log formats, request log fields, and Cloud Logging integration.

### Boolean Environment Variable Parsing (`parseBoolEnv`)

For server and infrastructure configurations, Scion parses several boolean environment variables using a robust, operator-friendly `parseBoolEnv` parser:

- **Supported Truthy Values:** `true`, `1`, `t`, `yes`, `y`, `on` (case-insensitive, whitespace-trimmed).
- **Supported Falsy Values:** `false`, `0`, `f`, `no`, `n`, `off` (case-insensitive, whitespace-trimmed).
- **Safety Warnings:** Unset, empty, or unparseable values default to `false`. However, to prevent configuration typos from silently disabling critical features, any **unrecognized non-empty value** (e.g., `SCION_LOG_GCP=trur`) will trigger an explicit **warning at startup** and default to `false`.

#### Tracked Boolean Variables:

| Variable | Description | Default |
| :--- | :--- | :--- |
| `SCION_SERVER_ADMIN_MODE` | Forces the server into emergency maintenance mode (break-glass removal). | `false` |
| `SCION_TRACING_ENABLED` | Enables OpenTelemetry tracing when a GCP Project ID is configured. | `false` |
| `SCION_LOG_GCP` | Enables Google Cloud Logging JSON format on standard output. | `false` |
| `SCION_REQUIRE_STABLE_SIGNING_KEY` | Demands a persistent session/JWT signing key. When enabled, startup aborts (fail-closed) if no stable key/secret can be resolved in hosted mode, preventing JWT signature mismatches across replica restarts. | `false` |

### Hub Endpoint Resolution

When `server.hub.public_url` is not explicitly set, the Hub endpoint injected into agents is resolved in this order:

1. `SCION_SERVER_HUB_ENDPOINT` or `server.hub.public_url` — explicit Hub public URL.
2. Project-level `hub.endpoint` setting.
3. `SCION_SERVER_BASE_URL` — the server's public base URL (also used for OAuth redirects).
4. **IAP Audience Derivation** (in Hosted HA mode with IAP authentication):
   - For **Cloud Run** IAP audiences (`/projects/<number>/locations/<region>/services/<service>`), Scion can auto-derive the Hub's URL using the legacy Cloud Run URL format (`https://<service>-<number>.<region>.run.app`). Newer Cloud Run services use a different URL format (`https://<service>-<hash>-<region>.a.run.app`) where the hash cannot be derived from the project number — for those services, set `SCION_SERVER_BASE_URL` explicitly instead of relying on auto-derivation.
   - For **GKE/GCLB** backend-service IAP audiences (`/projects/<number>/global/backendServices/<id>`), a URL cannot be derived from the ID. If `SCION_SERVER_BASE_URL` (or other explicit URL settings) is not set, Scion will log a warning at startup and fall back to `localhost`, which is likely unreachable from dispatched agents.
5. Auto-computed `http://localhost:{port}` (last resort).

For local development where the Hub runs on `localhost` but agents are in containers, set `server.broker.container_hub_endpoint` to a container-accessible address like `http://host.containers.internal:8080`.

The endpoint resolved above (or `server.hub.agent_endpoint`, if set — see below) is what the co-located broker then forwards to the container, applying its own bridging rules (host-gateway mapping for a hostname on co-located Docker, no rewrite for an already-reachable IP, and the `cloudrun-sandbox` runtime's own link-local logic).

#### Splitting the agent endpoint from the public URL

`server.hub.agent_endpoint` overrides the endpoint above for agents only — invite links, chat-bridge links, the OIDC issuer default, and the `cloudrun_invoker` audience default keep reading `public_url`. Use it when agents must reach the Hub on an address that would be wrong for a human clicking a link, such as an internal VPC IP in a topology where a proxy fronts the Hub for users. When unset, agents receive the Hub's regular endpoint — `public_url`, or the endpoint the Hub resolves above when `public_url` is itself unset.

The value must be `scheme://host[:port]` only: `http` or `https`, an IP literal or a hostname of letters, digits, `_`, `-`, and `.` (Docker Compose-style service names such as `scion_hub` are accepted), and an optional port — no path, query, fragment, or credentials (a trailing `/` is stripped from an otherwise-bare URL). The Hub rejects anything else at startup with an error naming `server.hub.agent_endpoint`.

**Scope: this value is injected into agents on every runtime broker attached to this Hub, including remote brokers.** Only set it if every broker's agents can reach the address and it is this Hub on each of those networks — otherwise agents dispatched from a remote broker will send their Hub credentials to whatever answers at that address on their own network.

**Security:** with an `http://` value, agent bearer tokens, and the secrets and tokens agents fetch from the Hub, travel **unencrypted** on the network between agents and the Hub. Use `https://` unless that network is trusted and isolated (for example, a private VPC subnet with no untrusted tenants).

## Notification channels

Notification channels deliver agent messages to external systems. Configure them
under `server.hub.notification_channels` as a list of channel objects. Each object
has a `type`, a `params` map, and optional filters.

```yaml
server:
  hub:
    notification_channels:
      - type: <channel-type>
        params:
          # channel-specific key/value pairs
        filter_urgent_only: false   # if true, only deliver urgent messages
        filter_types:               # if set, only deliver these message types
          - input-needed
          - state-change
```

### Slack channel

Delivers notifications via a Slack incoming webhook using Slack's `text` payload
format.

**Type:** `slack`

**Parameters:**

| Param              | Required | Description |
|--------------------|----------|-------------|
| `webhook_url`      | yes      | Slack incoming webhook URL (must use `https://`). |
| `channel`          | no       | Override the webhook's default channel. |
| `mention_on_urgent`| no       | Mention string added when `msg.Urgent == true` (e.g. `@here`, `@channel`). |

**Example:**

```yaml
notification_channels:
  - type: slack
    params:
      webhook_url: https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXX
      mention_on_urgent: "@here"
```

### Webhook channel

Delivers notifications as a raw HTTP POST to an arbitrary URL. Use this when you
need the full structured payload without truncation or when integrating with a
custom receiver.

**Type:** `webhook`

**Parameters:**

| Param         | Required | Description |
|---------------|----------|-------------|
| `webhook_url` | yes      | Destination URL (must use `https://`). |

**Example:**

```yaml
notification_channels:
  - type: webhook
    params:
      webhook_url: https://example.com/scion-notifications
```

### Email channel

Delivers notifications by email.

**Type:** `email`

**Parameters:**

| Param    | Required | Description |
|----------|----------|-------------|
| `to`     | yes      | Recipient email address. |
| `from`   | no       | Sender address override. |
| `smtp`   | no       | SMTP server host:port. |

**Example:**

```yaml
notification_channels:
  - type: email
    params:
      to: oncall@example.com
```

### Discord channel

Delivers notifications via a Discord incoming webhook using Discord's native
webhook format (rich embeds, colour-coded severity, allowed-mentions-controlled
role/user pings). Unlike the Slack channel, the Discord channel targets the
Discord-native endpoint — the `/slack`-compatibility suffix is explicitly
rejected because it dilutes what each channel type means and silently hides
the user's real intent.

**Type:** `discord`

**Parameters:**

| Param               | Required | Description |
|---------------------|----------|-------------|
| `webhook_url`       | yes      | Discord incoming webhook URL. Must use `https://` and one of the allowed Discord hosts: `discord.com`, `discordapp.com`, `ptb.discord.com`, `canary.discord.com`. Path must begin with `/api/webhooks/` and must not end with `/slack`. |
| `mention_on_urgent` | no       | Mention string applied when `msg.Urgent == true`. Use Discord mention syntax: `<@&ROLE_ID>` for a role, `<@USER_ID>` for a user. `@here` and `@everyone` are intentionally **not** supported — the channel sets `allowed_mentions.parse: []` so Discord will not resolve them even if present. |
| `username`          | no       | Override the webhook's default username for delivered messages. |
| `avatar_url`        | no       | Override the webhook's default avatar for delivered messages. |

**Embed colours by message type:**

| Type                  | Colour | Hex       |
|-----------------------|--------|-----------|
| `state-change`        | blue   | `#3498db` |
| `input-needed`        | yellow | `#f1c40f` |
| `instruction`         | grey   | `#95a5a6` |
| *(urgent — any type)* | red    | `#e74c3c` (overrides the type colour) |

**Truncation:** Discord caps embed descriptions at 2048 characters. Messages
longer than that are truncated with a `…(truncated)` marker — use the webhook
channel type if you need the full structured payload without truncation.

**Example:**

```yaml
notification_channels:
  - type: discord
    params:
      webhook_url: https://discord.com/api/webhooks/123456789012345678/abcDEFghiJKLmnoPQR_stu
      mention_on_urgent: "<@&987654321098765432>"
      username: Scion Hub
    filter_urgent_only: false
    filter_types:
      - input-needed
      - state-change
```

:::note[Migrating from a Slack-compat Discord webhook]
Earlier scion releases had no Discord channel type — operators could route
notifications to a Discord webhook by using `type: slack` with a webhook URL
ending in `/slack` (Discord's Slack-compatibility endpoint). That approach
produces plain-text messages with no embeds, colours, or mentions.

To migrate:

1. Remove the `/slack` suffix from the webhook URL.
2. Change `type: slack` to `type: discord`.
3. If you previously used `mention_on_urgent: "@here"`, replace it with a
   Discord role mention (`"<@&ROLE_ID>"`) — `@here` is not supported via the
   native Discord webhook format (the channel sets `allowed_mentions.parse: []`
   which prevents Discord from resolving `@here` and `@everyone`).
4. Reload the hub config. Validation will reject the old `/slack`-suffixed
   URL so a misconfiguration will surface on startup rather than silently
   falling back.
:::

## Two-Tier Settings Architecture (HA Deployments)

In HA deployments where multiple Hub replicas share a Postgres database, settings are split into two tiers to prevent node drift while keeping bootstrap settings file-managed.

### Layer 0 — Bootstrap (file + env only)

Settings required before the database connection exists, or that are restart-bound. Managed exclusively via `settings.yaml` and `SCION_SERVER_*` environment variables. **Cannot be written via the admin API** — `PUT /api/v1/admin/server-config` returns `422` if any Layer-0 key is present.

| Group | Keys (`server.` prefix unless noted) |
| :--- | :--- |
| Database | `database.*` |
| Listeners | `hub.port`, `hub.host`, `hub.read_timeout`, `hub.write_timeout`, `broker.*` |
| Auth stack | `auth.mode`, `auth.dev_mode`, `auth.dev_token`, `auth.dev_token_file`, `auth.proxy.*`, `auth.transport.*`, `oauth.*`, `oidc_login.*` |
| Secrets/storage | `secrets.*`, `storage.*`, `workspace_storage.*`, `shared_dir_storage.*` |
| Identity/mode | `mode`, `env`, `hub.hub_id`, `hub.gcp_project_id` |
| Logging | `log_level`, `log_format` |
| CORS | `hub.cors.*`, `broker.cors` |
| Messaging/plugins | `message_broker.*`, `plugins.*` |
| Async agent create | `hub.async_agent_launch`, `hub.launch_timeout`, `hub.launch_keepalive_seconds` |
| Heartbeat reconcile | `hub.missing_agent_grace` |
| Conduit relay | `hub.conduit.*` |

### Layer 1 — Operational (Postgres `hub_settings` table)

Settings that can be changed at runtime and are shared across all replicas. Stored as section-per-row in the `hub_settings` table. In SQLite/workstation mode, these fall back to `settings.yaml` (unchanged behavior), except for the `maintenance` section which is runtime/API-only and has no `settings.yaml` representation (ephemeral in file/SQLite mode).

| Section | Contents |
| :--- | :--- |
| `access` | `admin_emails`, `user_access_mode`, `authorized_domains`, `default_user_role` |
| `lifecycle` | `auto_suspend_stalled`, `soft_delete_retention`, `soft_delete_retain_files`, `start_claim_lease_ttl`, `start_max_duration`, `start_unconfirmed_hold`, `start_create_unconfirmed_hold` |
| `maintenance` | `admin_mode`, `maintenance_message` (durable + cluster-wide) |
| `telemetry` | Full `telemetry.*` subtree (enabled, cloud, hub, local, filter, resource) |
| `agent_defaults` | `default_template`, `default_harness_config`, `default_max_turns`, `default_max_model_calls`, `default_max_duration`, `default_resources`, `default_model`, `default_thinking_level`, `default_max_agent_role`, `default_agent_role`, `default_runtime_broker`, `default_timezone`, `default_gcp_identity_mode`, `default_gcp_identity_service_account_id` |
| `federation` | `enabled`, `trusted_issuers[]`, `algorithms`, `refresh_interval`, `debounce_interval` |
| `endpoints` | `hub.public_url`, `image_registry` |
| `github_app` | `app_id`, `api_base_url`, `webhooks_enabled`, `installation_url`, `private_key_path` |
| `notifications` | `notification_channels[]` |
| `project_defaults` | `default_scratchpad` |
| *(reserved)* `global_defaults` | Reserved for future hub-resource design — not implemented |

`agent_defaults.default_timezone` is the Hub default `TZ` for agent containers: an IANA zone name, used only when the agent has no pin and no `TZ` environment variable applies. Empty means no default (the image default, UTC). An invalid name or `Local` is rejected with `422`. In `settings.yaml`, and in the `PUT /api/v1/admin/server-config` request body, it is the top-level `default_timezone` field. It does not change how times are stored or displayed. See [Times and Timezones](/scion/reference/times-and-timezones/#hub-default-timezone).

### Precedence

In Postgres mode, the effective value for any Layer-1 key is resolved in this order (highest priority first):

1. **`SCION_SERVER_*` environment variable** — node-local escape hatch
2. **`hub_settings` DB row** — cluster-shared, set via admin API
3. **`settings.yaml` Layer-1 fields** — fallback when key absent in DB
4. **Compiled defaults**

### Seeding and Migration

- **First startup**: the first replica to start seeds `hub_settings` from its local `settings.yaml` (Layer-1 keys only) under an advisory lock. Subsequent replicas see the seed marker and skip.
- **Seeding reads file values only** — environment overrides are not baked into shared state.
- **DB wins**: once a section is seeded/written to DB, the DB row fully owns that section. Omitted fields within the section fall to compiled defaults, not to the file.
- **Rollback safety**: older builds ignore the `hub_settings` table entirely and read files — rolling back reverts to pre-change behavior.

### Environment Override Warnings

Because env overrides on Layer-1 keys reintroduce per-node drift, the system warns administrators:

- `GET /api/v1/admin/server-config` includes an `env_overrides` array listing which Layer-1 keys are overridden by env vars on the serving node.
- A startup `WARN` log lists any overridden Layer-1 keys.
- The admin UI renders a visible warning banner when env overrides are detected.

### Admin API Behavior Notes

**PUT partitioning**: The request body is partitioned by the section registry. Layer-1 fields (including `runtimes`, `profiles`, and `harness_configs`) are written to DB sections in the `hub_settings` table as whole-map JSONB documents. Layer-0 fields trigger a `422` rejection. Unclassified fields (non-registered settings) are ignored and reported in `ignored_keys`.

**Revision CAS**: The request body may include `expected_revisions` — a map of section name to expected revision number. On mismatch, the response is `409 Conflict` with the conflicting sections and their current revisions. Omitted sections use last-writer-wins semantics. The `access` section is the exception: it is merged onto the current row, and a concurrent change to that row between read and write returns 409 even without `expected_revisions`. Sections are written in alphabetical order for deterministic partial-apply behavior.

**Presence-aware clearing**: The PUT handler distinguishes **omitted** fields (preserve current DB value) from **explicitly-sent empty values** (`""`, `[]`, `null`) which **clear** the field. This enables clearing admin_emails, user_access_mode, authorized_domains, default_user_role, notification_channels, and public_url without sending every field.

**Masked secrets**: `GET /api/v1/admin/server-config` masks secrets (OAuth client secrets, GitHub App keys, notification channel parameters, and other credentials). A PUT may send a masked placeholder back only inside a block that exactly matches the stored block once masked; the Hub then keeps the stored secret. The block is the structure the secret sits in (for example one OAuth provider, the GitHub App, or one notification channel). To change any field of such a block, send every secret in that block in clear. Any other placeholder is rejected with `400`, so it is never stored over a real value. The admin web UI leaves unedited masked blocks out of its saves.

**Maintenance durability**: `PUT /api/v1/admin/maintenance` writes to the `maintenance` section in DB, making admin/maintenance mode durable across restarts and propagated to all replicas. `SCION_SERVER_ADMINMODE` env var still force-enables per node for break-glass access. In file/SQLite mode, maintenance changes are ephemeral (in-memory only, lost on restart). Use `SCION_SERVER_ADMINMODE=true` env var for persistent control.

**Schema endpoint**: `GET /api/v1/admin/server-config/schema` returns JSON-schema fragments and koanf key paths per section for UI form generation and CLI validation.

:::caution[Go Zero-Value Limitations]
Due to Go's `omitempty` JSON behavior, boolean `false` is indistinguishable from an omitted field in some contexts. This affects:

- `auto_suspend_stalled` (Layer 1, lifecycle section) — `false` may be treated as omitted
- `github_app.webhooks_enabled` (Layer 1, github_app section) — `false` may be treated as omitted

When these fields are explicitly set to `false` in the DB, they are correctly applied via the snapshot. However, the raw JSON representation may omit them. The admin API handles this correctly via the presence-aware clearing mechanism.
:::

## GCP IAM Check Mode

The `gcp_iam_check_mode` setting controls whether the Hub verifies that a caller holds the `iam.serviceAccounts.actAs` IAM permission on a GCP service account before allowing it to be assigned to an agent. This uses the [GCP Policy Troubleshooter v3 API](https://cloud.google.com/policy-intelligence/docs/troubleshoot-access).

:::caution[Use enforce when agents receive GCP identities]
Set `gcp_iam_check_mode: enforce` on any Hub where agents receive GCP identities. In the default `"off"` mode, Scion roles alone decide which project-scoped service accounts a user can assign: the `project-owner`, `project-admin` and `project-member` roles hold `gcp_service_account.assign`, so every owner, admin and member of a project can assign any verified project-scoped service account in that project to an agent, with no GCP IAM `iam.serviceAccounts.actAs` check on the caller. In `"enforce"` mode the Hub also requires the caller's own `actAs` grant on the target service account.
:::

### Values

| Value | Behaviour |
| :--- | :--- |
| `"off"` (default) | No GCP IAM check. Project owners, admins and members can assign any **verified project-scoped** service account in their project to an agent on Scion role authority alone, with no GCP IAM `iam.serviceAccounts.actAs` check on the caller. Hub-scoped service accounts remain unassignable in `"off"`. See the caution above. |
| `"enforce"` | The Hub calls Policy Troubleshooter to verify the caller has `actAs`. Denials are enforced. |

### Configuration

```yaml
# settings.yaml
server:
  hub:
    gcp_iam_check_mode: "off"   # or "enforce"
```

Or via environment variable:

```bash
export SCION_SERVER_HUB_GCPIAMCHECKMODE=enforce
```

### Enablement Checklist

Before setting `gcp_iam_check_mode: enforce`:

1. **Enable the Policy Troubleshooter API** on the Hub's GCP project:
   ```bash
   gcloud services enable policytroubleshooter.googleapis.com
   ```

2. **Grant the Hub SA `roles/iam.securityReviewer`**:
   - On the Hub's own GCP project (minimum; covers Hub-minted SAs).
   - On each org or project containing BYOSA service accounts, if applicable.

3. **(Recommended)** Grant `roles/iam.denyReviewer` and `roles/browser` at the org level for full deny-policy and resource hierarchy evaluation.

4. **(Optional)** Configure domain-wide delegation with Workspace `groups.read` for group-binding resolution. Without this, group-granted `serviceAccountUser` bindings produce an indeterminate result (denied by default).

5. **Test with a known-good SA assignment** before enabling in production.

### Group-Binding Limitation

When `roles/iam.serviceAccountUser` is granted to a Google Workspace **group**, the Hub SA must have domain-wide delegation with the `groups.read` privilege to resolve the membership. Without it, Policy Troubleshooter returns `MEMBERSHIP_UNKNOWN_INFO`, which under fail-closed rules is treated as a denial.

This denies legitimately authorized users whose `actAs` grant arrives via a group binding, even when the PT API is fully available and functioning correctly.

| Mitigation | Cost | Resolves groups? |
| :--- | :--- | :--- |
| Grant Hub SA domain-wide delegation + `groups.read` | High (org-admin consent per Workspace) | Yes |
| Grant `actAs` directly to users, not via groups | Low (per-SA IAM binding) | Avoided |
| Leave `gcp_iam_check_mode: "off"` | Zero | N/A (check disabled) |

### BYOSA Cross-Org Access

For BYOSA service accounts (accounts in a customer's org, not the Hub's), the Hub SA needs `roles/iam.securityReviewer` in the customer's organisation or at minimum on the project containing the SA. Some customers may refuse this grant. Their options are:

1. Leave `gcp_iam_check_mode: "off"` (the default).
2. Grant `securityReviewer` on the specific project (not org-wide).
3. Accept that BYOSA assignments will fail closed until the grant is made.

### Required IAM Permissions Summary

| Role / Permission | Scope | Purpose | Required? |
| :--- | :--- | :--- | :--- |
| `roles/iam.securityReviewer` | Org or project containing the target SA | Read allow policies, role bindings, and role definitions | **Yes** |
| `roles/iam.denyReviewer` | Org or folder | Read IAM Deny policies | Recommended |
| `roles/browser` | Org | Read project/folder hierarchy for policy inheritance | Recommended |
| Workspace Admin `groups.read` (via domain-wide delegation) | Google Workspace domain | Resolve group memberships in IAM bindings | Only if group-granted actAs must be resolved |
| PT API enabled on Hub's GCP project | Hub's GCP project | `policytroubleshooter.googleapis.com` | **Yes** |
