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
To move a `server.yaml` into `settings.yaml`, copy its contents under a top-level `server:` key in `~/.scion/settings.yaml`, then remove `server.yaml`. `scion config migrate` merges a `server.yaml` only while it converts a legacy (unversioned) `settings.yaml`; it skips a file that already has `schema_version`. There is no `--server` flag yet.
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
| `public_url` | string | | The externally accessible URL of the Hub (used for callbacks). Also editable in the admin UI; resolved at startup as described in [Hub Endpoint Resolution](#hub-endpoint-resolution): `--base-url`, then `SCION_SERVER_HUB_ENDPOINT`, then `SCION_SERVER_BASE_URL`, then a value saved in the database, then this file (the web server's base URL uses only operator inputs and never reads the database; see [Web Base URL](#web-base-url)). See [`server.hub.public_url`: environment first](/scion/reference/admin-settings/#serverhubpublic_url-environment-first). |
| `agent_endpoint` | string | | Optional override of `public_url` used **only** for the Hub URL injected into agents (`SCION_HUB_ENDPOINT`). Use when agents reach the Hub on a different address than users — e.g. an internal VPC URL — while invite links, chat-bridge links, the OIDC issuer default, and the `cloudrun_invoker` audience default keep using `public_url`. Must be `scheme://host[:port]` only: `http` or `https`, an IP literal or a hostname of letters, digits, `_`, `-`, and `.`, no path, query, fragment, or credentials (a trailing `/` is stripped); the Hub fails to start otherwise. When unset, agents receive the Hub's regular endpoint (`public_url`, or the endpoint the Hub resolves when `public_url` is unset). **Scope:** injected into agents on every broker attached to this Hub, including remote brokers — see [Splitting the agent endpoint from the public URL](#splitting-the-agent-endpoint-from-the-public-url). **Security:** an `http://` value sends agent bearer tokens and fetched secrets unencrypted; prefer `https://` unless the network is trusted and isolated. |
| `monitoring_dashboard_url` | string | | Optional link to an external monitoring dashboard for this Hub, such as a Cloud Monitoring or Grafana dashboard. Must be an absolute `http` or `https` URL with a host, at most 2048 characters, with no user credentials, a port (if any) from 1 to 65535, and no whitespace (including Unicode spaces such as U+00A0), control characters (C0, DEL or C1), bidirectional formatting characters, invisible format characters (U+00AD, U+180E, U+200B to U+200D, U+2060, U+FEFF) or U+FFFD; a path, query and fragment are allowed. Any other value is rejected with `422` when saved through the admin API. When set, the Health page header shows **Open monitoring dashboard**, which opens the URL in a new tab; when unset, no link is shown. Editable in **Server Config** and applied without a restart. Only callers with `hub.health.read` receive it (as `links.monitoring_dashboard` in the health summary); it is not part of `/api/v1/settings/public`. Env: `SCION_SERVER_HUB_MONITORINGDASHBOARDURL` (seed: `SCION_SEED_SERVER_HUB_MONITORINGDASHBOARDURL`). |
| `gcp_project_id` | string | | The Hub's GCP project. Layer-0, read once at startup: set it in `settings.yaml` (`server.hub.gcp_project_id`) or with `SCION_SERVER_HUB_GCPPROJECTID`, which wins over the file. A legacy `server.yaml` `hub.gcpProjectId` is read only when `settings.yaml` has no `server` key: a `settings.yaml` with a `server` block replaces `server.yaml` as a whole (see [Which GCP project the Hub uses](#which-gcp-project-the-hub-uses)). Used as the project for service account minting and as the Policy Troubleshooter quota project; when it is unset, both use the project of the GCP metadata server on GCE, GKE or Cloud Run. It is also the export project of the Hub's own metrics and traces, and turns metrics export on when [`observability.metrics_enabled`](#observability-export-serverhubobservability) is unset. Not editable in the admin UI, and refused by `PUT /api/v1/admin/server-config` in every mode, workstation included (`GET` leaves it out), because it selects the service account minting project. See [Which GCP project the Hub uses](#which-gcp-project-the-hub-uses). |
| `observability` | object | | Switches for the Hub's own metrics and trace export. See [Observability export](#observability-export-serverhubobservability). |
| `gcp_iam_check_mode` | string | `"off"` | Controls whether IAM `actAs` permission is checked when binding a GCP service account to an agent. Supported values: `"off"` (no check; default) or `"enforce"` (uses Policy Troubleshooter to enforce `iam.serviceAccounts.actAs`). `"enforce"` is strongly recommended for any Hub where agents receive GCP identities; see the caution under [GCP IAM Check Mode](#gcp-iam-check-mode) for what `"off"` permits. See the security/permissions reference for details on roles and caches. |
| `gcp_iam_deny_unknown_policy` | string | `"fail-open"` | Behavior when Policy Troubleshooter cannot evaluate deny policies (e.g. if the Hub lacks org-level reviewer roles). Supported values: `"fail-open"` (allow if no explicit deny is found; default) or `"fail-closed"` (treat as indeterminate and deny). |
| `read_timeout` | duration | `"30s"` | HTTP read timeout. |
| `write_timeout` | duration | `"60s"` | HTTP write timeout. |
| `admin_emails` | list | `[]` | List of emails granted super-admin access. Listed users are always admins: they are promoted on sign-in. When the list is non-empty, an admin whose email is removed from it is demoted to [`default_user_role`](#authentication-serverauth) at the next hub restart or their next sign-in, whichever comes first. At restart, both `admin_emails` and the default role come from `settings.yaml` or the environment, so a change made only in the Admin UI (Postgres mode) takes effect at the user's next sign-in. If the default role was set only in the Admin UI, a user demoted at restart becomes Member. Two exceptions: admins promoted from **Admin > Users** (or the users API) stay admins, and nobody is demoted if the startup safety check failed (for example, no existing user matched the list at startup and there were no UI-promoted admins); demotions resume only after the configuration is fixed and the hub is restarted. Roles set from the admin UI for users who were never config admins (`member`, `viewer`) are not changed by this list. |
| `auto_suspend_stalled` | bool | `false` | Suspend an agent when the Hub marks it `stalled`, unless its harness does not support session resume. See [Auto-Suspend of Stalled Agents](/scion/local/agent-lifecycle/#auto-suspend-of-stalled-agents). Editable in **Server Config**. Env: `SCION_SERVER_HUB_AUTOSUSPENDSTALLED` (seed: `SCION_SEED_SERVER_HUB_AUTOSUSPENDSTALLED`). |
| `stalled_threshold` | duration | `"5m"` | How long a `running` agent with a recent heartbeat may go without activity events before the Hub marks it `stalled`. A value under `2m` falls back to the default. Editable in **Server Config**. Applied without a restart when settings are stored in the database; in file mode, at the next restart. Env: `SCION_SERVER_HUB_STALLEDTHRESHOLD` (seed: `SCION_SEED_SERVER_HUB_STALLEDTHRESHOLD`). |
| `soft_delete_retention` | duration | | Duration to retain soft-deleted agents (e.g., `"72h"`). The same window applies to agent directories that stale-directory cleanup keeps because their workspace may hold work: once it has passed, the next agent create or start that uses the same agents directory removes them, and the removal is logged. That check reads this value only from `~/.scion/settings.yaml` or `SCION_SERVER_HUB_SOFTDELETERETENTION` on the machine that holds the directory (the CLI machine, or the Runtime Broker). A value set in a `scion server start --config` file or in the Hub's admin settings is not seen. Unset or `0` keeps such directories until they are deleted. |
| `soft_delete_retain_files` | bool | `false` | Preserve workspace files during the soft-delete period. When the retention window passes, the Hub asks the agent's Runtime Broker to remove the kept files before it purges the record. While that removal runs, the Runtime Broker refuses a start of the agent with a retryable `409 retained_purge_in_progress`. A Runtime Broker that cannot be reached, or whose removal keeps failing, is retried on every purge run for up to 30 days past the window; after that the record is purged and the files are reported as left on that Runtime Broker. |
| `launch_timeout` | duration | `"5m"` | Whole-launch budget for an asynchronous create or start, from the moment the Hub begins the launch until the agent reaches a terminal Hub state. A launch that has not reached one by this deadline is ended and the agent is set to `error` (`launch_timeout`). A Go duration string such as `"15m"`. There is no upper limit. A non-zero value below `30s` is replaced by the default (`5m`) with a warning in the Hub log. In `settings.yaml`, a value that is not a valid duration is ignored and the default applies. Raise it for clusters with slow pod starts. Startup-only: restart required to change. Env: `SCION_SERVER_HUB_LAUNCHTIMEOUT`. |
| `launch_keepalive_seconds` | int | `15` | Keepalive interval, in seconds, that the Hub sends to the Runtime Broker with each asynchronous create or start. A launch whose broker sends no report for 8x this interval (120s at the default) is ended and the agent is set to `error` (`broker_lost`). Values of `0` or less use the default. Startup-only: restart required to change. Env: `SCION_SERVER_HUB_LAUNCHKEEPALIVESECONDS`. |
| `missing_agent_grace` | duration | `"3m"` | How long a `running` (or `stopping`) agent may be absent from its Runtime Broker's heartbeat before the Hub marks it `error` with exit reason `container_missing` (an existing `preempted` or `evicted` exit reason and its message are kept). Applies only when the broker is online, reported a complete runtime inventory, and sent a recent previous heartbeat; agents with a lifecycle operation in progress are skipped. Values below `"1m"` fall back to the default. Env: `SCION_SERVER_HUB_MISSINGAGENTGRACE`. |
| `start_claim_lease_ttl` | duration | `"90s"` | Lease of the claim the Hub takes before dispatching any agent start; the Hub process running the start renews it every third of this. Allowed `30s` to `5m`; other values fall back to the default. Hot-reloaded. Env: `SCION_SERVER_HUB_STARTCLAIMLEASETTL`. |
| `start_max_duration` | duration | `"12m"` | Hard deadline on any agent start, including a wait for another Hub node to dispatch it. Minimum `11m` (the broker's pod-ready bound plus a minute). Hot-reloaded. Env: `SCION_SERVER_HUB_STARTMAXDURATION`. |
| `start_unconfirmed_hold` | duration | `"17m"` | Longest time a start whose outcome is unknown (for example a dispatch timeout) keeps other starts of the agent waiting, until the runtime shows whether it created anything. Minimum `16m` (the Runtime Broker's 15-minute bound on a synchronous start, plus a minute). Hot-reloaded. Env: `SCION_SERVER_HUB_STARTUNCONFIRMEDHOLD`. |
| `start_create_unconfirmed_hold` | duration | `"16m"` | `start_unconfirmed_hold` for a new agent's create-and-start. The Runtime Broker may go on starting the agent for up to 15 minutes after the Hub stops waiting for it, so the allowed range is `16m` up to `start_unconfirmed_hold`. A value from an earlier release below the minimum (for example `5m`) is raised to `16m` with a warning, and a lower `start_unconfirmed_hold` (for example `13m`) to `16m`; such values still save. Hot-reloaded. Env: `SCION_SERVER_HUB_STARTCREATEUNCONFIRMEDHOLD`. |
| `perf_trace` | bool | `false` | Turns on per-request performance tracing for diagnosis. Observe only. See [Request performance tracing](#request-performance-tracing) and the [developer guide](/scion/contributing/perf-tracing/). Startup-only: restart required to change. Env: `SCION_SERVER_HUB_PERFTRACE`. |
| `csp_report_raw_samples` | bool | `false` | Adds each browser policy report's `sample` to its `csp_violation` log record. Off by default: a sample can hold the start of page content (for a Trusted Types report, the text passed to the API, such as a chat message), and log records follow your log retention. Turn it on only for a short diagnosis, then off. See [Browser Policy Reports](/scion/reference/api/#browser-policy-reports-hub). Startup-only: restart required to change. Env: `SCION_SERVER_HUB_CSPREPORTRAWSAMPLES`. |
| `membership_sweep_report_only` | bool | `false` | Puts the membership-standing sweep in report-only mode: it logs and audits (`agent_hold_would_set`) each agent it would hold and stop, and holds and stops none. Off by default (the sweep enforces). Holds from membership changes made while it is on still apply. Set it on every replica: the sweep runs on whichever replica takes its lock, and a replica left enforcing holds the listed agents at its next sweep. For the first boot after an upgrade; see [Upgrading: report-only first boot](/scion/reference/agent-suspension/#upgrading-report-only-first-boot). Startup-only: restart required to change. Env: `SCION_SERVER_HUB_MEMBERSHIPSWEEPREPORTONLY`. |
| `cors` | object | | CORS configuration (see below). |
| `conduit` | object | | Conduit relay settings (see [Conduit](#conduit-serverhubconduit)). |
| `port_proxy` | object | | Agent port proxy settings (see [Port proxy](#port-proxy-serverhubport_proxy)). |

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
| `internal_listen` | string | | `host:port` of the internal relay API listener, used by multi-node deployments. It serves only the internal relay API and must be reachable only inside the cluster or VPC, never publicly. Without `internal_tls_cert_file` and `internal_tls_key_file` it must be a loopback address (a loopback IP literal: 127.0.0.0/8 or `::1`, not `localhost`); see **TLS on the internal hop** below. Setting `internal_advertise` without it is a setting error: an HA hub refuses to start, and a single-node hub logs it and runs without the relay. Flag: `--internal-listen`. Env: `SCION_SERVER_HUB_CONDUIT_INTERNALLISTEN`. |
| `internal_advertise` | string | | Base URL (`http(s)://host:port`) other hub nodes use to reach this node's internal listener. It must be `https://` unless its host is loopback (a loopback IP literal: 127.0.0.0/8 or `::1`, not `localhost`). Default: `POD_IP` with the listen port, else the listen host if it is not a wildcard, with `https://` when the listener serves TLS. Flag: `--internal-advertise`. Env: `SCION_SERVER_HUB_CONDUIT_INTERNALADVERTISE`. |
| `internal_tls_cert_file` | string | | Path of the PEM certificate chain the internal listener serves TLS with. Set together with `internal_tls_key_file`. Re-read when the file changes (at most once a minute), so a rotated certificate needs no restart. Flag: `--internal-tls-cert-file`. Env: `SCION_SERVER_HUB_CONDUIT_INTERNALTLSCERTFILE`. |
| `internal_tls_key_file` | string | | Path of the PEM private key of `internal_tls_cert_file`. Flag: `--internal-tls-key-file`. Env: `SCION_SERVER_HUB_CONDUIT_INTERNALTLSKEYFILE`. |
| `internal_tls_ca_file` | string | | Path of the PEM CA bundle every internal hop client verifies peers with. When set it replaces the system roots. A private or internal CA **requires** it: the system roots do not validate such a certificate, and the hop then fails. Read at startup. Flag: `--internal-tls-ca-file`. Env: `SCION_SERVER_HUB_CONDUIT_INTERNALTLSCAFILE`. |
| `internal_tls_server_name` | string | | Name internal hop clients verify in the peer certificate instead of the dialed host, so one certificate (for example with the name `scion-relay.internal`) can serve every node dialed by its pod IP. Set the same value on every node. Default: the dialed host. Flag: `--internal-tls-server-name`. Env: `SCION_SERVER_HUB_CONDUIT_INTERNALTLSSERVERNAME`. |
| `peer_auth` | string | `"auto"` | Relay-peer authentication: `auto`, `oidc` or `hmac`. Requests between relays are always signed with a key derived from the hub's signing secret, which is required in every mode. `oidc` also requires a Google OIDC ID token, `auto` adds the ID token on GCP, and `hmac` uses the signature alone. Used only when this node serves the internal relay API (`internal_listen` set, or any HA hub): a single-node hub without `internal_listen` starts its relay with no relay-peer authentication and refuses requests to and from other relays. Env: `SCION_SERVER_HUB_CONDUIT_PEERAUTH`. |
| `peer_service_accounts` | list | own service account | With OIDC peer auth, the service-account emails allowed to call the internal relay API. The hub logs a warning at startup when the default resolves to a Compute Engine default service account. Env: `SCION_SERVER_HUB_CONDUIT_PEERSERVICEACCOUNTS` (comma-separated). |
| `peer_audience` | string | `"scion-conduit-relay-peer"` | With OIDC peer auth, the ID token audience. It must be identical on every hub node. Env: `SCION_SERVER_HUB_CONDUIT_PEERAUDIENCE`. |
| `reconnect_window` | duration | `"5s"` | Jitter window sent with a planned close: targets redial after a random delay within it. Between `"0s"` and `"5m"`. Flag: `--conduit-reconnect-window`. Env: `SCION_SERVER_HUB_CONDUIT_RECONNECTWINDOW`. |
| `authz_recheck_interval` | duration | `"60s"` | Period of the authorization re-check sweep of open user streams: port proxy streams (including user requests on the legacy port-forward tunnel), terminal attach streams on the agent's Conduit session, and `scion tunnel` streams. User streams are also re-checked when the user's access changes and when the hub's event connection is re-established; Runtime Broker placement lookups are shared across streams evaluated in the same sweep. A stream whose user lost the permission is closed with `4401 authz_expired` within one interval even if the revocation event is missed; revocations normally close it at once. Between `"1s"` and `"10m"`. Flag: `--conduit-authz-recheck-interval`. Env: `SCION_SERVER_HUB_CONDUIT_AUTHZRECHECKINTERVAL`. |
| `lifetime_cap` | duration | `"3500s"` | Lifetime cap of a conduit session. Set it below the platform's connection limit (Cloud Run request timeout, load balancer or proxy timeout). 60 seconds before the cap the relay sends `GoAway`: the session takes no new streams, the agent reconnects, and streams still open are closed with `4503 relay_restart` within 30 seconds and never later than the cap. A PTY client sees that close as `4503` and reconnects. Server-sent events are not affected. Between `"90s"` and `"24h"`. Flag: `--conduit-lifetime-cap`. Env: `SCION_SERVER_HUB_CONDUIT_LIFETIMECAP`. |
| `shutdown_grace` | duration | | Time the platform allows between `SIGTERM` and killing the hub process. No default; the Cloud Run deployment profiles set `"10s"`. While the conduit relay runs, the hub handles `SIGTERM` (and `SIGINT`) by sending `GoAway` to every conduit session before its listeners close, with a drain deadline of at most `shutdown_grace` minus 2 seconds when this key is set (a value of 2 seconds or less gives a 1 ms deadline); when it is unset there is no cap and the drain deadline stays 30 seconds. Agents open a replacement session and streams still open at the deadline are closed with `4503 relay_restart`. In external relay mode with the ingress proxy serving there is no relay in the hub: the hub stops proxying conduit dials, asks the relays to drain the sessions it proxied, and closes the proxied connections still open when that drain step ends (see [Separate Conduit Relays](/scion/hosted/ha/separate-relay/#hub-ingress)). When set, it is also the whole hub process's shutdown budget: the co-located Runtime Broker's shutdown, the HTTP drain and the background teardown each get a share of `shutdown_grace` minus 2 seconds (half of `shutdown_grace` when it is 4 seconds or less), and the process exits within `shutdown_grace` even if a step has not finished (exit status 1 in that case). When it is unset, the drain stays 30 seconds and each shutdown step keeps its own bound, so one stop can take a minute or more (up to 35 seconds for the co-located broker, then up to 30 seconds of HTTP drain): on a hub run by systemd or Kubernetes, set it below the stop timeout (for example `"20s"` for a 30-second stop timeout). From the first `SIGTERM` or `SIGINT`, `/readyz` answers `503` and new requests other than health checks and Runtime Brokers' own requests get `503` with `Retry-After`; a second signal forces the shutdown (exit status 1). Must be a duration greater than 0. Read at startup; a change needs a restart. Env: `SCION_SERVER_HUB_CONDUIT_SHUTDOWNGRACE`. |
| `proxy_session_max_age` | duration | `"1h"` | How long a user's Conduit session admitted through trusted-proxy authentication (no bearer credential, so no expiry of its own) may open new tunnels, counted from admission. After it, a new tunnel request on that session is refused with `401`; tunnels already open are unaffected. Between `"5m"` and `"1h"`. Env: `SCION_SERVER_HUB_CONDUIT_PROXYSESSIONMAXAGE`. |
| `stream_authz_max` | object | see description | Authorization interval of open streams, per originating principal kind (`user`, `broker`, `agent`). When a stream reaches its interval, the hub re-checks the principal: if the permission still holds, the stream is renewed for one more interval; if not, it is closed with `4401 authz_expired` (`4404 target_not_found` if the agent is gone). If the check cannot be evaluated (for example a store outage), the stream is never renewed or closed early; it is closed with `4401 authz_expired` at the end of its current interval. Revocations and the `authz_recheck_interval` sweep close a revoked stream sooner, but never extend it. Defaults: `user` `"8h"`, `broker` `"24h"`, `agent` `"24h"`, each between `"1m"` and `"168h"`. Only `user` is enforced today (port proxy streams, including user requests on the legacy port-forward tunnel, terminal attach streams on the agent's Conduit session, and `scion tunnel` streams); the `broker` and `agent` values are validated but not yet applied. Flag: `--conduit-stream-authz-max-user`. Env: `SCION_SERVER_HUB_CONDUIT_STREAMAUTHZMAX_USER`, `SCION_SERVER_HUB_CONDUIT_STREAMAUTHZMAX_BROKER`, `SCION_SERVER_HUB_CONDUIT_STREAMAUTHZMAX_AGENT`. |
| `instance_id` | string | see description | This node's relay instance id. It must be unique among live hub processes: a relay that starts with an id already in use takes it over from the other process. Up to 128 printable ASCII characters, no spaces. Default: `POD_NAME` when set, else the host name plus a random per-process suffix. Env: `SCION_SERVER_HUB_CONDUIT_INSTANCEID`. |
| `relay_mode` | string | `"in_process"` | Where conduit sessions are held. `in_process` runs a relay in every hub process. `external` runs none: separate relay processes (`scion server --role relay`, see [Relay Settings](#relay-settings-serverrelay)) hold the sessions, and the hub serves no conduit sessions itself. External mode requires `peer_secret`, `internal_listen` (the relays call the hub internal API on it) and the `hub.conduit` experiment; startup fails without them. Layer 0 (bootstrap); read at startup, restart required. Env: `SCION_SERVER_HUB_CONDUIT_RELAYMODE` (or `SCION_SERVER_HUB_CONDUIT_RELAY_MODE`). |
| `ingress` | string | `"proxy"` | How a hub in external relay mode serves a conduit dial. `proxy` reverse-proxies the dial to the relay chosen by selection, over a `relay-peer` leg; it requires HMAC peer authentication: under any other resolved `peer_auth` (OIDC, including `auto` on GCP) the hub starts with the proxy off and conduit dials get `503` `conduit_not_serving` (cause `ingress_requires_hmac`). `redirect` answers with the chosen relay's public URL. Ignored with `relay_mode: in_process`. Layer 0 (bootstrap); read at startup, restart required. Env: `SCION_SERVER_HUB_CONDUIT_INGRESS`. |
| `target_stream_deadlines` | string | `"off"` | Whether targets enforce user streams' authorization deadlines locally. `off` leaves enforcement with the hub or relay node that holds the stream. With `enforce`, a target closes a user stream at its authorization deadline unless it is renewed. The hub publishes the value to targets with the grant keys (`GET /api/v1/conduit/grant-keys`). User streams to targets that support renewal carry their deadline in the open grant, and the node holding the stream renews it at about 80% of the interval, in both modes (in external relay mode, hub-held streams renew over the bridge to the separate relay, and tunnels held by a separate relay are re-checked and renewed by that relay's stream scheduler when the forwarding relay advertises `tunnel_recheck`, carrying route `relay_tunnel` in refusals and unstamped counts). With `enforce`, an open stream that the hub or relay cannot keep renewed is refused with `503 unavailable` (reason `stream_renewal_unavailable`) on tunnels, the port proxy and PTY attach. Today that covers a relay-held tunnel forwarded by an older relay that does not support tunnel re-check, and users whose identity has no renewal path (federated users, whose grants carry no deadline under `off`). The hub logs a warning at startup when `enforce` is set with `relay_mode: external`. Also with `enforce` and `relay_mode: external`, a user stream to an agent whose sciontool does not support stream renewal is refused before it opens: `409 agent_upgrade_required` (reason `stream_renewal_unsupported`) on tunnels and the port proxy, and `503 runtime_attach_unsupported` with the same reason on PTY attach. Restart such an agent on an image whose sciontool supports stream renewal. In that mode a user PTY attach never uses the runtime broker's terminal path, and goes over the agent's own session instead. In that mode user requests never fall back to the older routes: when the agent has no conduit session, or conduit is not available on the hub, the port proxy and PTY attach answer with a retryable 503 (`unavailable`), and exposed ports that only the port-forward tunnel can serve (a non-loopback host) are unavailable. Because streams are renewed in both modes, streams opened under `off` keep being renewed after a switch to `enforce`. A stream that stops being renewed, for example one held by a node that is no longer running, ends at its deadline (at most the configured user `stream_authz_max`). Requires the `hub.conduit` experiment. Set it in the settings file or the environment; `scion server start` has no flag for it. Env: `SCION_SERVER_HUB_CONDUIT_TARGETSTREAMDEADLINES` (or `SCION_SERVER_HUB_CONDUIT_TARGET_STREAM_DEADLINES`). |
| `peer_secret` | string (secret) | | Dedicated relay-peer secret: the HMAC key of every call between the hub and separate relays, and between relays, is derived from it. Raw octets, used as given: no decoding. Exactly one trailing newline is removed, then the value must hold at least 32 octets, at least 16 of them distinct. Generate one with `openssl rand -base64 32` and use the output as it is. Required in external relay mode and on `--role relay`, where startup fails without it; in `in_process` mode it is validated but not used; the hub also refuses to start if it equals the session secret. It is independent of the session secret: a relay never holds the session secret. Never stored in the database or logged, and never returned by the server-config API; a non-empty value in a server-config `PUT` is refused, and an empty or null value is ignored. It is never passed into agent or workload environments. Prefer a file-mounted `settings.yaml` over the environment. Layer 0 (bootstrap); read at startup, restart required. Env: `SCION_SERVER_HUB_CONDUIT_PEERSECRET` (or `SCION_SERVER_HUB_CONDUIT_PEER_SECRET`). |

**Rotating grant keys.** `POST /api/v1/admin/conduit/grant-keys/rotate` (unscoped Hub administrators only, `hub.conduit_grant_keys.execute`) rotates in a new grant signing key with an overlap window: grants signed with the outgoing key stay valid until the earlier of their own expiry and the outgoing key's `NotAfter`. Once the new key activates, the outgoing key can no longer sign; the Hub keeps only its public half. After `NotAfter` the Hub stops publishing the key, but keeps its public half for the stream renewal check for one `stream_authz_max.user` interval plus 30 seconds. The response lists only key IDs and timestamps.

**Revoking a grant key.** To withdraw a grant key at once, rotate first, wait until the new key is active (`activate_at` in the rotate response), then call `POST /api/v1/admin/conduit/grant-keys/revoke` with `{"kid": "<old key id>"}` (same permission as rotate). The key stops verifying anything at once. Streams whose latest grant it signed are not renewed again and end at their current authorization deadline. The key that signs now cannot be revoked.

**Upgrading hub replicas.** Upgrade all hub replicas to this version before the next grant key rotation, and not within one overlap window of a rotation (the activation delay plus the overlap, about 1 hour 15 minutes with the defaults). An upgraded hub does not rewrite a stored key ring that has one active key and no successor.

**TLS on the internal hop.** The internal hop (between hub nodes, and between hubs and separate relays) carries user and agent credentials and session content, so it is never plaintext off the host. With `internal_listen` set, the hub or relay refuses to start when:

- `internal_listen` is not a loopback address (a loopback IP literal: 127.0.0.0/8 or `::1`, not `localhost`) and `internal_tls_cert_file` and `internal_tls_key_file` are not set;
- `internal_advertise`, or on a separate relay `server.relay.hub_internal_url`, is `http://` to a host that is not loopback.

Hub and relay clients also refuse to send anything to a peer endpoint that is `http://` to a host that is not loopback. Only loopback IP literals count; a host name, `localhost` included, is never resolved for this decision and is refused. There is no override. To fix a refusal, give every node a certificate and set `internal_tls_cert_file` and `internal_tls_key_file`. If that certificate comes from a private or internal CA, also set `internal_tls_ca_file` on every node. Alternatively, bind `internal_listen` to `127.0.0.1` behind a TLS terminator on the same host, and advertise that terminator's `https://` address. When upgrading multi-node hubs or separate relays, set these keys on every node in the settings file or the environment (not as `--internal-tls-*` flags, which older binaries refuse) before upgrading, then upgrade all hubs and relays together; see [Upgrading](/scion/hosted/ha/separate-relay/#upgrading).

**Hosted HA.** In an HA deployment each hub node runs a relay that other nodes must reach directly, so the hub refuses to start when:

- the grant key ring is not stored with the shared at-rest key;
- it runs on Cloud Run (`K_SERVICE` is set), whose instances are not individually addressable;
- `internal_advertise` uses the public hub host or a `*.run.app` host;
- the relay is not addressable at its internal endpoint, or answers its self-check as another instance.

Outside HA, a relay that cannot start is logged and the hub serves without it.

#### Port proxy (`server.hub.port_proxy`)

Settings for the agent port proxy (`/api/v1/agents/{id}/ports/{port}/...`) when it runs over the agent's Conduit session (the `hub.conduit` [experiment](/scion/reference/experiments/)). Read at startup, so a change needs a restart. An invalid value is a startup error.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `response_header_timeout` | duration | `"60s"` | How long the proxy waits for the service on the agent port to send its response headers, counted from when the request has been sent. This includes the WebSocket handshake. If the headers do not arrive in time, the proxy cancels the upstream request, answers `504` (code `runtime_error`; a browser gets the proxy error page) and counts it in `scion.hub.port_proxy.upstream_timeout`. The response body has no time limit: server-sent events, downloads, chunked responses and upgraded WebSockets keep flowing once the headers have arrived. Between `"5s"` and `"10m"`; `0` is not accepted. Env: `SCION_SERVER_HUB_PORTPROXY_RESPONSEHEADERTIMEOUT`. |

#### Observability export (`server.hub.observability`)

Switches for the Hub's own metrics export (Cloud Monitoring) and trace export (Cloud Trace). These switches are separate from the agent `telemetry.*` settings. Both are Layer-1 settings in the `hub_observability` section. You can set them on the **Server Config** page (Hub Server tab), through `PUT /api/v1/admin/server-config` as a Hub admin, in `settings.yaml`, or with the environment variables below. The Hub creates its exporters once at startup, so a change applies after a restart. A save lists both keys under `pending_restart` ("applies after restart").

| Field | Type | Default | Environment variable | Layer | Restart | Description |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| `metrics_enabled` | bool | unset | `SCION_SERVER_HUB_OBSERVABILITY_METRICSENABLED` | 1 (`hub_observability`) | required | `true`: the Hub exports its metrics. `false`: it does not. Unset: metrics export runs when `server.hub.gcp_project_id` is set, as before this switch existed. |
| `tracing_enabled` | bool | unset | `SCION_SERVER_HUB_OBSERVABILITY_TRACINGENABLED` | 1 (`hub_observability`) | required | `true`: the Hub exports traces. `false`: it does not. Unset: trace export runs when `SCION_TRACING_ENABLED` and `server.hub.gcp_project_id` are both set, as before this switch existed. |

**Precedence.** The general rule for Layer-1 keys applies (see [Precedence](#precedence)). A database row an admin has saved wins over `settings.yaml` and the environment, and the environment and `settings.yaml` only seed the row and act as the fallback. A key set by its environment variable gets the "Overridden by environment on this node" badge on the page.

**Export project.** When a switch is `true`, the exporter sends data to `server.hub.gcp_project_id` if the operator set it (environment, else the server settings file: `settings.yaml`, or a legacy `server.yaml` when `settings.yaml` has no `server` key). Otherwise it uses the project of the GCP metadata server, the same fallback service account minting uses; off GCP the metadata server is not queried. If no project resolves, the Hub logs a warning that names `SCION_SERVER_HUB_GCPPROJECTID` and starts without that exporter. Startup never fails for this. An unset switch keeps the earlier rule, which only considers an operator-set project ID. The export project, endpoint and credentials cannot be edited in the admin UI. The page shows the export project and its source (`env`, `file` or `metadata`) read-only, together with each exporter's state at startup and the reason for it.

#### Which GCP project the Hub uses

The Hub reads every one of these sources once, at startup. A change applies after a restart.

| Use | Project, first match wins |
| :--- | :--- |
| Hub metrics export (Cloud Monitoring) and trace export (Cloud Trace) | 1. `server.hub.gcp_project_id`: `SCION_SERVER_HUB_GCPPROJECTID`, else the server settings file (see below). 2. The project of the GCP metadata server (on GCE, GKE or Cloud Run only). The page shows the source as `env`, `file` or `metadata`. |
| Metrics dashboard (the Hub queries Cloud Monitoring for agent usage) | 1. `telemetry.cloud.gcp_project_id` from `settings.yaml` or `SCION_SERVER_TELEMETRY_CLOUD_GCPPROJECTID`. 2. The `project_id` of the `scion-telemetry-gcp-credentials` Hub secret. 3. `server.hub.gcp_project_id` as configured above. The metadata server is not asked on this path. |
| Service account minting and the Policy Troubleshooter quota project | 1. `server.hub.gcp_project_id` as configured above. 2. The project of the GCP metadata server. |

**Server settings file.** The Hub reads its server settings from one file, not key by key: when `settings.yaml` has a `server` key, that block is used as a whole and a legacy `server.yaml` is ignored (the Hub logs a deprecation warning when both exist). So a `server.yaml` `hub.gcpProjectId` is not read when `settings.yaml` has a `server` block, even one without `gcp_project_id`; move it to `server.hub.gcp_project_id`. Only when `settings.yaml` has no `server` key is `server.yaml` read, and then its `hub.gcpProjectId` applies.

`server.hub.gcp_project_id` is settable only in the settings file or the environment: the server-config API refuses it in every mode.

**Upgrade note:** earlier releases ignored `server.hub.gcp_project_id` in `settings.yaml`. A Hub whose `settings.yaml` already sets it now uses that value at the next start: Hub metrics export turns on while `server.hub.observability.metrics_enabled` is unset, metrics and traces go to that project, and service account minting and the Policy Troubleshooter use it instead of the metadata server project. `SCION_SERVER_HUB_GCPPROJECTID` still wins. Remove the key, or set it to the project you want, before upgrading. A legacy `server.yaml` `hub.gcpProjectId` behaves as before: it applies only when `settings.yaml` has no `server` key.

**Startup log.** The Hub logs one line for each exporter. The line says whether the exporter is on or off and why, and names the export project when it is on. For example:

```text
Hub trace export: off (server.hub.observability.tracing_enabled unset; off because SCION_TRACING_ENABLED is not set)
Hub metrics export: on (project: my-project (source: metadata); server.hub.observability.metrics_enabled=true)
WARNING: Hub metrics export: off (server.hub.observability.metrics_enabled=true but no export project resolved (...); starting without the exporter. Set SCION_SERVER_HUB_GCPPROJECTID or run on Google Cloud)
```

Metrics are exported every 60 seconds and every trace is sampled. Neither can be configured.

#### Asynchronous agent create

A synchronous Hub agent create holds the create request open while the Runtime Broker provisions the workspace and starts the agent. That request is bounded by the CLI's HTTP client timeout (30s) and by the Hub's `write_timeout` (`60s` by default), so the synchronous path is not suited to agents that take several minutes to start.

Asynchronous create is the `hub.async_agent_launch` [experiment](/scion/reference/experiments/), on by default. An admin can turn it off, or back on, in **Admin → Server Config → Experiments**; no restart is needed, and the Hub checks it each time it sends a create to a broker. With it on, the Hub answers as soon as the broker accepts the create. The agent is returned in a pre-running phase with an active launch, and the broker finishes the start in the background:

- **Opt-in per request.** A create is asynchronous only when the experiment is on **and** the request opts in (`acceptAsyncLaunch`). Provision-only creates and reprovisioning are always synchronous. `scion start` and `scion resume` opt in on every Hub create and then poll the agent until it is `running`, `error`, or `stopped`. By default they wait for the Hub's remaining launch budget plus 30 seconds (5 minutes when the Hub does not report a budget); `--wait-timeout` overrides this and `--no-wait` returns once the Hub has accepted. See [`scion start`](/scion/reference/cli/#scion-start-or-run). The web UI's create form also opts in: the agent page shows the current launch step while the agent provisions, and if the create does not complete, its failure code and reason with **Delete** and **Create again**. Scheduled agent creates opt in server-side. The Discord integration's `/scion thread` opts in unless its `async_create` setting is `false`; it reports each launch step in the thread and binds the thread once the agent is running. Requests that do not opt in are synchronous.
- **The wait is not the launch.** If the CLI stops waiting (the wait budget runs out, or Ctrl-C), the launch continues on the Hub and broker, and the agent still comes up. Run `scion start <agent>` again to resume waiting.
- **Bounded by `launch_timeout`.** A launch that has not reached a terminal Hub state by its deadline is ended: the broker stops it shortly before the deadline, and the Hub sets the agent to `error` with launch error `launch_timeout` shortly after. Starting such an agent is refused with `agent_create_incomplete`; delete it and create it again.
- **Broker did not take the launch.** If the broker refuses the create, cannot be reached, or acknowledges a different launch, the Hub ends the launch with launch error `dispatch_failed` ("the runtime broker did not accept the launch"), so `scion start` and other clients following the launch stop waiting and report the failure. A create usually removes the agent in that case. An agent kept after a failed env submission or workspace finalize stays in `provisioning`: submit the env again once the Hub has released the failed start's claim, after `start_create_unconfirmed_hold` (16 minutes by default). If such an agent later stops or fails, starting it is refused with `agent_create_incomplete`; delete it and create it again.
- **Turning it off.** Creates sent after the experiment is turned off are synchronous. Launches the broker has already accepted are not cancelled: they finish, or are ended by `launch_timeout` or broker loss, as before.
- **Starts during a launch.** While a launch is in progress and before its deadline, a start or restart of that agent returns the launching agent with HTTP 200 instead of starting it again (a restart adds the warning `agent is launching; restart not performed`), and `scion start` keeps waiting. Restore, reincarnate, and wake are refused with `agent_launching`.

**Requirements.** The Hub, the Runtime Broker, and the CLI must all run a version that includes asynchronous create. A broker that reports no async launch support, or that answers the create synchronously, gets the synchronous create, so mixing versions is safe but slow starts on an older broker keep the synchronous limits. A broker runtime that cannot serve an asynchronous launch (currently `substrate`) also falls back to the synchronous create.

**Broker restarts during a launch.** The Runtime Broker keeps each asynchronous launch it has accepted in a launch journal on disk, so a launch is not left waiting for the Hub's `broker_lost` or `launch_timeout` when the broker stops or crashes:

- **Graceful stop (SIGTERM).** The broker first refuses new creates, starts and restarts with HTTP 503, error code `broker_draining` and `Retry-After: 5`. It stops each launch in progress and reports it failed with launch error `broker_restarted` ("the runtime broker shut down during the launch"). When the Hub confirms, the broker removes what the launch created. It waits up to 10 seconds for this, out of its shutdown budget (30 seconds, or its share of `shutdown_grace` when that is set). A report that is not delivered in that time stays in the journal for the next start. The Hub treats `broker_draining` as a start the broker did not act on: it releases the agent's start claim at once, so a retry after the drain is not blocked, and the start and restart actions (`POST /api/v1/agents/{id}/start` and `/restart`) answer with a retryable HTTP 503, error code `broker_draining`, and the broker's `Retry-After`. Stops, deletes and launch reports are still served while the broker drains. When the Hub and the broker run in one server process (for example the workstation server, or a combined Hub and broker deployment), the drain runs while the in-process Hub still accepts the brokers' launch reports: the Hub refuses new user requests once the shutdown begins, but keeps its listener open until the co-located broker and every flat broker instance in the process have stopped, and only then begins its own HTTP drain. A report that is not delivered in time stays in the journal and is sent when the server starts again.
- **Crash or kill.** When the broker starts again with the same state directory, it reports each unfinished launch failed with `broker_restarted` ("the runtime broker restarted during the launch"). The report goes out under the old broker process's identity. When the Hub confirms, the broker removes the launch's runtime resources (pods, secrets, containers). It also removes the agent's files if the launch was a create and still owns them. A launch whose success was decided but not yet delivered is reported as succeeded, and nothing is removed. If the Hub gives no definitive answer in time (from a running Runtime Broker or from recovery), the Runtime Broker keeps the journal entry, as `terminal_pending`, and the launch marker, so the next Runtime Broker start resends the report; after 5 unanswered attempts or 24 hours since the first, recovery logs a warning and removes the record with no cleanup, leaving the rest to `scion delete`.
- **Failed cleanup.** If removing what a launch created fails, the journal keeps the launch as `cleanup_pending`, and the next start of the Runtime Broker retries the cleanup it recorded (only the launch's own resources, when that was the scope). After 5 failed attempts, or 24 hours after the first failure, the Runtime Broker logs it, drops the record, and leaves the rest to `scion delete`.
- **What the journal holds.** It lives under `<state-dir>/launches/`, where the state directory is `<server.broker.state_dir>/<broker-id>` (by default `~/.scion/runtime-broker-state/<broker-id>`). Directories are mode 0700 and files 0600. Entries hold only identifiers: launch, agent, run and project IDs, the agent name and project path, the runtime name, the latest step and the created resource handles. They never hold environment variables, secrets, tokens or the request.
- **Local filesystem only.** Each broker process locks its own journal with `flock`. Broker replicas that share one broker ID must each use a local filesystem or a distinct state directory, because `flock` is not reliable on NFS or other shared filesystems.
- **Ephemeral homes.** Crash recovery needs the state directory to survive the restart. Where the broker home is an `emptyDir` (the Hub chart's default), a container restart in the same pod keeps it, but a new pod does not. A graceful stop still reports in-flight launches.
- **Choosing a persistent state directory.** To recover launches after a crash or a new pod, set `server.broker.state_dir` (or `SCION_SERVER_RUNTIMEBROKER_STATEDIR`) to a directory that survives the restart: on Kubernetes, a volume of your choice mounted into the broker container, for example a `PersistentVolumeClaim` per replica. The chart does not add one; that is an operator decision. The directory must be on a local filesystem, not NFS or another shared filesystem (see above), and each replica that shares a broker ID needs its own. Keep the path the same across restarts. Changing it starts the broker with an empty state directory: launches journalled under the old path are not recovered, and `scion runtime-broker deregister --purge-local` removes the directory under the current setting only. It reads `server.broker.state_dir` from the global settings and environment of the shell that runs it, so run it where the broker's setting applies (on Kubernetes, inside the broker container). If it cannot resolve the setting, it removes nothing and exits non-zero.
- **Grace period.** On Kubernetes, give broker and combined Hub and broker pods `terminationGracePeriodSeconds: 45` so the drain and the rest of the shutdown fit before the pod is killed. The 30-second default leaves little margin.

**Slow pod starts.** On clusters with slow node provisioning, for example GKE Autopilot cold starts where a new node is added and the agent image takes several minutes to pull, raise `launch_timeout` so the launch is not ended first:

```yaml
server:
  hub:
    launch_timeout: "15m"
```

`launch_timeout` and `launch_keepalive_seconds` are read only at Hub startup; restart the Hub after changing them. They cannot be set through the admin server-config API (see [Layer 0](#layer-0--bootstrap-file--env-only)).

**Retired setting.** The `server.hub.async_agent_launch` setting (env `SCION_SERVER_HUB_ASYNCAGENTLAUNCH`) has been retired and is ignored. A Hub that still has it set logs this warning at startup: "server.hub.async_agent_launch is retired; enable the hub.async_agent_launch experiment instead". Remove it. The experiment is on by default, so asynchronous create stays on unless an admin turns the experiment off. An admin server-config save (`PUT /api/v1/admin/server-config`) that still carries the key is refused with 422 and the same message.

**Brokers on a Cloud Run service.** An asynchronous launch keeps running in the Runtime Broker after the create response. A broker hosted on a Cloud Run service therefore needs CPU allocated outside requests: deploy the service with `--no-cpu-throttling`, and keep its minimum instances above zero.

- **Symptoms.** If the service allocates CPU only during requests (the Cloud Run default), the launch stalls after the create response. The broker stops reporting, so the Hub ends the launch and sets the agent to `error` with launch error `broker_lost` (or `launch_timeout`). With minimum instances at zero, idle scale-in can also end a broker mid-launch, with the same result.
- **Startup warning.** The server cannot see the service's CPU setting. When the Runtime Broker is enabled and the process runs on a Cloud Run service (`K_SERVICE` set, `CLOUD_RUN_INSTANCE` unset), it logs this warning once at startup: "If this Cloud Run service allocates CPU only during requests, async agent launches will stall after the create response and be reaped. Deploy with --no-cpu-throttling and set SCION_CLOUDRUN_CPU_ALWAYS_ALLOCATED=true to silence this."
- **Silencing it.** Set the environment variable `SCION_CLOUDRUN_CPU_ALWAYS_ALLOCATED=true` on a service deployed with `--no-cpu-throttling`. It is an assertion, not a check: remove it if you redeploy with CPU throttling. The Cloud Run deploy script (`scripts/cloudrun/deploy.sh`) sets both.

**Brokers using the `cloudrun-sandbox` runtime.** On the single-node [Cloud Run Instance tier](/scion/hosted/single-node/hub-setup-cloudrun/), the Runtime Broker runs on the Cloud Run Instance and starts each agent as a Cloud Run Sandbox. With the `hub.async_agent_launch` experiment on, eligible creates that opt in are asynchronous:

- **Launch timeout.** Set `launch_timeout` to at least `"60s"`. A sandbox start may not finish within a shorter budget, and the launch is then ended with `launch_timeout`.
- **CPU after the create response (not verified).** Async on this runtime assumes the Cloud Run Instance keeps CPU allocated while the broker finishes the launch in the background after the create response. This has not been verified on a live Instance. If the Instance does not keep CPU allocated, launches stall after the create response and the Hub ends them with `broker_lost` or `launch_timeout`; the synchronous create (the experiment off) is not affected. To return to it, turn the experiment off; no restart is needed. The startup warning above covers Cloud Run services only and is not logged on an Instance.

**Runtime Brokers using the `cloudrun` runtime.** The `cloudrun` runtime starts each agent as a Cloud Run Instance. Each Instance declares container port 18381, where `sciontool init` opens a startup listener, and its startup probe checks that port (every 10 seconds, up to 24 failures) instead of the platform default port 8080. With the `hub.async_agent_launch` experiment on, eligible creates that opt in are asynchronous, as on other runtimes. If a launch fails or is ended, the Runtime Broker deletes the Instance the launch created, including one whose create request was cut off but which the platform created anyway.

- **Restarts.** The runtime does not set a restart policy on the Instance, so the platform's default restart behavior applies: an agent that exits with code 0 may not be restarted. Start it again with `scion start`.
- **Launch timeout.** Set `launch_timeout` to at least `"120s"`; the default (`"5m"`) is above this. The figure comes from measurements on the live Instances API: an Instance whose image was already cached was ready about 8 seconds after the create was accepted (worst of the cached runs), and the single start measured with an image not yet pulled took about 46 seconds. The broker waits for the create to finish and stops the launch 20 seconds before its deadline, so an uncached start needs a deadline of about 66 seconds. Allowing about 54 seconds more, close to double, for the work the broker does before the create and for larger images and slower pulls than the single uncached start measured, gives 120 seconds. Raise it further for large agent images.

#### Asynchronous agent start

Asynchronous start, resume and restart of an existing agent is the `hub.async_agent_start` [experiment](/scion/reference/experiments/), off by default. When it is on, the Hub answers a start that opted in as soon as the Runtime Broker accepts it, as it does for an asynchronous create. The answer shows the agent in phase `starting` with an active `launch` (kind `start`, or `restart` for a restart). The broker then reports the outcome, and the agent moves to `running`, or to its failure phase with `launch.error` set. With the experiment off, every start, resume and restart is synchronous, as before.

- **Needs both experiments.** It applies only while `hub.async_agent_launch` is on too.
- **Who opts in.** The `scion start` and `scion resume` commands already ask for it, so they become asynchronous as soon as the experiment is on, and they wait for the launch to finish. Lifecycle API callers opt in with the body `{"acceptAsyncLaunch":true}` on `POST /api/v1/agents/{id}/start` or `/restart`. Messages that wake an agent (`--wake`), `scion reincarnate` and starts the Hub schedules itself stay synchronous.
- **Which starts.** A start of an agent at rest (`stopped`, `suspended`, `halted` or `error`, including an `error` agent that remembers an earlier phase; see resume memory below), and the start leg of a restart. The restart's stop leg stays synchronous. The first start of a `created` or `provisioning` agent stays synchronous, and so does a start of an agent that is already running. The Runtime Broker must support it too (it advertises `asyncStart`). An older broker answers synchronously, and the start then completes as before. `launch_timeout` must be more than 20 seconds.
- **Turn it on only after every Hub node runs a version that supports it.** Each such node advertises `async_start_v1` in its hub-instance registry row (the one the health summary lists). The Hub also checks this: while any live Hub instance lacks `async_start_v1`, starts stay synchronous, and the Hub logs `async start disabled: hub instance <label> (version <version>) lacks async_start_v1`, once per change of the answer or its reason. The check reads the registry at most every 30 seconds and never delays a start. A freshly started Hub process runs starts synchronously until its first read, and so does a Hub whose last read is more than a minute old, until its next read. A node whose registry row has not been written for 45 seconds (stale) or that stopped cleanly is not counted, so a node whose registry writes fail stops blocking after 45 seconds.
- **Failure phase.** A failed asynchronous restart ends `stopped`. A failed start from `stopped` or `error` returns to that phase. A failed start from `suspended` or `halted` ends `error` with resume memory (below), so a client that waits for a phase still sees the failure. A failed asynchronous restart never revokes the agent's credential.
- **Rolling back.** Turn the experiment off, then wait at least `launch_timeout` plus 15 seconds before you downgrade any Hub node, so start launches already in flight end under the newer code. The all-nodes check does not cover launches that were already in flight when a node was downgraded.
- **Resume memory.** When an asynchronous start from `suspended` or `halted` fails, the agent shows `error` and remembers the phase it started from (`launch.resumePhase` in the API). A later plain `scion start` or `scion resume`, the web Start button, or a message with `--wake` then behaves as it would have for that phase. Turning the experiment off does not clear this memory; any later phase other than `starting` or `error` does.
- **Runtime Broker side.** A Runtime Broker that can run a start in the background reports `asyncStart` in its capabilities (registration, heartbeat and `/api/v1/info`). The Hub's start must carry the launch fields and the agent's Hub ID; otherwise, or on a runtime that cannot launch in the background (currently `substrate`), the broker starts the agent synchronously as before. The broker keeps a background start in the launch journal like a create (a failed launch report also records the launch step it failed at), and a stop, delete or newer start of the agent ends it (if one arrives during admission or before handoff, the start answers `409 start_cancelled`). A failed or abandoned start removes the pods, secrets or containers it created but never the agent's files; this holds for a restart too, which the broker records as a start. The Runtime Broker also includes the Hub-minted `runId` on its launch reports: if a launch fails on the Runtime Broker after the agent was already observed `running` (`running_observed`), the Hub still applies the failed launch report (moving the agent to `error` or its start-kind failure phase, recording `launch.error` and `launch.step`, settling the start claim, releasing quota, and answering `applied` so the Runtime Broker cleans up) when `runId` matches the agent's current run, the agent is in `running` or `error` with no live activity, no other start, delete or reincarnation claim holds it, and the error code is not `broker_restarted`.

#### Request performance tracing

`perf_trace: true` (env `SCION_SERVER_HUB_PERFTRACE=true`) records where Hub API requests and SSE connections spend their time, and how many authorization store reads and decision-audit records each request causes. It is for diagnosing slow agent lists, mainly on a test or staging Hub. On a production Hub, turn it on only for a short diagnosis window: the log volume grows by one line per request. It is off by default. For a how-to (reading the log lines, running the `perf/bench` harness, and using the counts as regression budgets), see [Hub Performance Tracing and Benchmarks](/scion/contributing/perf-tracing/).

```yaml
server:
  hub:
    perf_trace: true
```

**Observe only.** Tracing never changes an authorization decision, a decision-audit record (content, count or delivery; records are delivered asynchronously and can be lost, see [Log delivery guarantees](#log-delivery-guarantees)), a response body, filtering, sorting, redaction or lineage. The only change a client can see is the extra response headers described below, and only an unscoped local platform admin who asks for them can see it.

**What it records.** Each Hub API request writes one `perf_trace` log line (subsystem `hub.perf-trace`); an SSE connection writes one at connect and one at close (`sse_stage`). A line holds:

- `endpoint`: a fixed endpoint class, for example `agents.global.legacy`, `agents.global.sorted`, `agents.project.legacy`, `agents.project.sorted`, `agents.project.sorted_agent`, `sse.events`, or `other`.
- `phase_<name>_us` and `phase_<name>_n`: time (microseconds) and count per phase. Phases are `list_scope_authz` (list-level authorization before rows are read), `list_db_read` (agent row and member reads; database time only), `list_read_authz` (per-row read decisions), `enrich`, `capabilities` (per-item capabilities and the env-view decision), `messageability`, `scope_capabilities`, `serialize` (encoding and writing the body), and for SSE `sse_expand`, `sse_authorize` and `sse_write`.
- `store_<method>_n` and `store_<method>_us`, `authz_store_calls`, `authz_store_us`: reads the authorization service makes to prepare its inputs (groups, role bindings, role definitions, access constraints, delegation edges, and user, agent, project and membership rows), counted after request-local reuse.
- `audit_records`, `audit_allow`, `audit_deny`, `audit_other`, `audit_emit_us`: decision records counted in memory. With the default sampling setting, each authorization decision emits one record, so `audit_records` is the request's decision count. `audit_emit_us` measures the emitter call. This path performs no database write.
- `db_wait_count`, `db_wait_us`, `db_in_use`, `db_open`: connection-pool waits during the request and pool use when the line is written (the end of the request), when the database driver reports them. In the response headers they are taken when the response starts. The pool is shared, so waits include concurrent requests.
- `elapsed_us`, `method`, `request_id` (for correlation only), and for SSE `sse_events`.

**Counted scope.** The store counts cover the authorization service's request-path reads of the methods above. Relationship progeny lookups and reads inside a store transaction are not counted, and work handed to a detached background context after the request ends records into a trace that is no longer logged. `capabilities` also includes the rare re-list read decision for a sorted-list row whose authorization inputs changed between reads.

**Response headers.** Only an unscoped local platform admin (a local user with the admin role, not using a scoped access token, not federated) who sends `X-Scion-Perf-Trace: 1` also gets `X-Scion-Perf-Endpoint`, `X-Scion-Perf-Phases`, `X-Scion-Perf-Phase-Counts`, `X-Scion-Perf-Store-Calls`, `X-Scion-Perf-Store-Us`, `X-Scion-Perf-Decisions` and, when available, `X-Scion-Perf-DB`. Values are `name=integer` pairs; durations are microseconds. The headers are set when the response starts, so they omit `serialize`. No other caller gets them: not unauthenticated endpoints, agents, brokers, scoped tokens, federated identities or non-admin users. Their requests are still traced and logged. The `perf/bench` API benchmark records the headers with `--want-perf-trace`, and for its non-admin caller joins the hub's `perf_trace` log lines by request ID with `--hub-perf-log`. Counts (store calls, decisions, phase counts) do not depend on machine speed, so they suit CI budgets; durations do not.

**What the counts reveal.** The counts are not just timing. A deny count is the number of candidate rows hidden from the caller (on a filtered list, whether one specific agent exists). Store-call counts hint at owner, delegator and group structure behind the rows. DB pool figures describe process-wide load. That is why the headers go only to unscoped local platform admins, and why the log line belongs with other operator logs.

**Cardinality and privacy.** Phase names, store methods, endpoint classes and audit outcomes are fixed sets. No path, query, ID, name, email, token, secret or configuration value is logged or used as a label.

**Overhead.** With tracing off, no middleware or decorator is installed, and each recording point in the list handlers is one context lookup that finds nothing. With tracing on, each recorded phase or store read adds two clock reads and an uncontended lock, plus a few hundred bytes and one log line per request.

### Relay Settings (`server.relay`)

Settings of a separate relay process, `scion server --role relay` (external relay mode, `server.hub.conduit.relay_mode: external`). Settings the relay shares with the hub's relay, such as `internal_listen`, `peer_auth`, `peer_secret` and `lifetime_cap`, stay under [`server.hub.conduit`](#conduit-serverhubconduit). All of them are Layer 0 (bootstrap) and read at startup, so a change needs a restart.

A separate relay requires targets (agents and Runtime Brokers) on version X or later. Until then, and until its other production gates are met, the separate relay runs on development and test hubs only.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `hub_internal_url` | string | | Base URL (`http(s)://host:port`) of the hub internal API the relay calls for admission, RPCs and re-checks. Any hub replica serves it, so it may be a load-balanced internal address; it must be reachable only inside the cluster or VPC. It must be `https://` unless its host is loopback (a loopback IP literal: 127.0.0.0/8 or `::1`, not `localhost`; see **TLS on the internal hop** in the conduit settings above). Layer 0; restart required. Env: `SCION_SERVER_RELAY_HUBINTERNALURL` (or `SCION_SERVER_RELAY_HUB_INTERNAL_URL`). |
| `public_url` | string | | The relay's own public URL, recorded as its `relay_instances.public_endpoint`. A user authenticated by Google IAP is admitted through a relay only with the hub's own IAP configuration (`auth.proxy.iap` with its audience, Google's issuer and key endpoint), so clients must reach the hub's IAP backend; trusted-proxy identity headers are never accepted from a relay. Layer 0; restart required. Env: `SCION_SERVER_RELAY_PUBLICURL` (or `SCION_SERVER_RELAY_PUBLIC_URL`). |
| `singleton` | bool | `false` | Experimental. Asserts a non-HA singleton relay. Required on Cloud Run (`K_SERVICE` set), where the relay refuses to start without it; set it together with `max_instances=1`, `min_instances=1` and `cpu_idle=false`. Allowed elsewhere, where it labels the relay non-HA. The relay lists the feature `singleton` in its `relay_instances` row, retries its startup self-check for up to 60s, and reports other live relays sharing its internal endpoint in the gauge `scion.relay.singleton_endpoint_peers`. Upgrade the hubs, which migrate the database, before the relays. Layer 0; restart required. Env: `SCION_SERVER_RELAY_SINGLETON`. |

**Relay database access.** A relay reads its own configuration file, never the hub's, and connects with the `database.url` in it. Give the relay a relay-scoped, least-privilege database role with exactly these rights: read and write on `relay_instances` and `conduit_sessions`; read, insert and update on `conduit_principal_epochs` (no delete); and read on the hub settings table, of which it reads only the `experiments` row to follow `hub.conduit`. On PostgreSQL the relay LISTENs on the hub's dedicated `scion_ev_conduit` channel, never on the hub's other event channels. The hub notifies that channel, in the same transaction as its own notice, of each revocation and settings change a relay acts on, with the subject and identifiers only. Grant the role table rights only (PostgreSQL does not restrict LISTEN by role). It needs nothing else, and never reads token, credential or secret tables. At startup the relay logs the configuration files it loaded and its database driver, never the connection string or its credentials.

### Broker Settings (`server.broker`)

Controls the Runtime Broker service.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `enabled` | bool | `false` | Whether to start the broker service. |
| `port` | int | `9800` | HTTP port to listen on. |
| `broker_id` | string | | Unique UUID for this Runtime Broker. It names the Runtime Broker's state directory (`<state_dir>/<broker-id>`), so it must be a single path element: an empty ID, `.`, or an ID containing `..`, `/`, `\` or NUL is refused on join and on credential save. A value written by hand (for example with `scion config set`) is caught when the server starts, which refuses to start with it. |
| `broker_name` | string | | Name the broker registers under on the Hub. Default: the hostname (`Hosted Broker` for a broker run with a co-located Hub). |
| `broker_nickname` | string | | Same as `broker_name`, and takes precedence over it. Set by `scion runtime-broker register --broker-name`. |
| `hub_endpoint` | string | | The Hub URL this broker connects to. |
| `container_hub_endpoint` | string | | Overrides `hub_endpoint` when injecting the Hub URL into agent containers. Use when containers cannot reach the Hub at the broker's address (e.g. `http://host.containers.internal:8080` for local development). |
| `state_dir` | string | `~/.scion/runtime-broker-state` | Base directory of the broker's runtime state: the [launch journal](#asynchronous-agent-create), dispatch attempts and pending env-gather. The broker uses `<state_dir>/<broker-id>`. Must be an absolute path or start with `~/`; a relative path refuses startup. Read at startup. Flat Runtime Broker instances (`instances`) do not use it and keep their default directories; setting it on a process that hosts them logs a warning. |
| `broker_token` | string | | Authentication token for the Hub. |
| `auto_provide` | bool | `false` | Automatically add as provider for new projects. |
| `cors` | object | | Broker CORS configuration; off by default (see [Broker CORS](#broker-cors-serverbrokercors)). |
| `instances` | list | | Experimental. Flat Runtime Broker instances this process hosts; see [Flat Runtime Brokers](/scion/hosted/ha/multi-broker/#flat-runtime-brokers-experimental). Empty or absent means the regular Runtime Broker. Several entries are accepted, each with a unique `key`; the Hub may run in the same process or remotely (see the link). Read only from the global `settings.yaml` (not `server.yaml` or project settings). Each entry has `key` (immutable local key: lower-case letters, digits and `-`, starting and ending with a letter or digit, up to 63 characters), `name` (the name registered with the Hub), and `runtime_target` (see [Flat instance runtime target](#flat-instance-runtime-target-serverbrokerinstancesruntime_target)). An entry may also set `agent_defaults`, the workload defaults of the agents it runs (see below). |

#### Broker CORS (`server.broker.cors`)

Nothing in Scion calls the Runtime Broker from a browser: the Hub reaches it with signed server-side requests or over the control channel. CORS on the broker is therefore off by default, and the broker sends no CORS headers.

CORS headers are added only to requests that pass broker authentication. Broker authentication runs first, and preflight (`OPTIONS`) requests are not exempt from it, so a broker with HMAC keys refuses an unsigned preflight before CORS applies. Allowing an origin therefore helps only a caller that can sign its requests.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `enabled` | bool | `false` | Enable CORS on the broker API. A missing `server.broker.cors` block means off. |
| `allowed_origins` | list | `[]` | Origins that get CORS headers on authenticated broker requests. Enabling CORS without listing origins allows none. `"*"` allows every origin; the broker logs a warning at startup when it is set. |
| `allowed_methods` | list | `GET, POST, PUT, PATCH, DELETE, OPTIONS` | Methods sent in `Access-Control-Allow-Methods` for an allowed origin. |
| `allowed_headers` | list | `Authorization, Content-Type, X-Scion-Broker-Token, X-API-Key` | Headers sent in `Access-Control-Allow-Headers` for an allowed origin. |
| `max_age` | int | `3600` | Preflight cache lifetime in seconds. |

Env: `SCION_SERVER_RUNTIMEBROKER_CORS_ENABLED`, `SCION_SERVER_RUNTIMEBROKER_CORS_ALLOWEDORIGINS` (comma-separated), and the matching names for methods, headers and max age.

:::note[Upgrade note]
Earlier releases enabled broker CORS by default for every origin. A deployment that relied on that must now set `server.broker.cors.enabled: true` and list its origins in `allowed_origins`.
:::

#### Flat instance runtime target (`server.broker.instances[].runtime_target`)

The runtime target of one flat Runtime Broker instance, and the instance's own infrastructure defaults. A flat instance never uses Runtime Broker Profiles, so these fields take the place that a profile and its runtime entry hold for a regular Runtime Broker. Each default keeps its usual precedence: an agent's own value still wins over it, and an unset field falls back to the same global setting as before. All fields are local operator policy. They are read only from the global `settings.yaml` and are never sent to the Hub. Unknown keys and invalid values refuse startup with an error that names the settings path. A Kubernetes-only field on a `docker` instance is refused with `field not valid for runtime target type docker`.

| Field | Type | Applies to | Description |
| :--- | :--- | :--- | :--- |
| `type` | string | both | `docker` or `kubernetes`. Required. |
| `display_name` | string | both | Non-sensitive name shown in the UI. Default: the type. |
| `context` | string | kubernetes | Context in the loaded kubeconfig. Default: its current context. |
| `namespace` | string | kubernetes | Namespace for the instance's agents. Default: `SCION_K8S_NAMESPACE`, `POD_NAMESPACE`, the service account's namespace, else `default`. |
| `kubeconfig` | string | kubernetes | Absolute path of the instance's own kubeconfig file, then its only configuration and credential source. Default: the process's normal kubeconfig loading. |
| `kubernetes_block_service_account` | string | kubernetes | Kubernetes ServiceAccount for pods whose GCP identity mode resolves to `block`. |
| `kubernetes_service_account_mappings` | map | kubernetes | GCP service account email to the Kubernetes ServiceAccount bound to it through Workload Identity, for GCP identity mode `assign`. |
| `shared_dir_storage_class` | string | kubernetes | Default storage class for shared directory PVCs (must support ReadWriteMany). An agent's own `kubernetes.shared_dir_storage_class` wins. |
| `shared_dir_size` | string | kubernetes | Default size of each shared directory PVC, a positive Kubernetes quantity such as `40Gi`. Default: `10Gi`. An agent's own `kubernetes.shared_dir_size` wins. |
| `safe_to_evict` | bool | kubernetes | `false` adds the `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"` annotation to agent pods (on GKE Autopilot this requests extended run duration). `true` or unset adds nothing. An agent's own `kubernetes.safeToEvict`, `true` or `false`, wins. |
| `priority_class_name` | string | kubernetes | Default PriorityClass of agent pods (DNS-1123 subdomain). The PriorityClass must already exist on the cluster. An agent's own `kubernetes.priorityClassName` wins. |
| `gke` | bool | kubernetes | `true` turns on GKE mode for the instance. `false` or unset detects a GKE cluster automatically. |
| `node_selector` | map | kubernetes | Node labels (key to value) every agent pod of the instance must match. See [Flat instance placement](#flat-instance-placement). |
| `tolerations` | list | kubernetes | Tolerations of every agent pod of the instance, each with `key`, `operator` (`Equal` or `Exists`; empty means `Equal`), `value` and `effect` (`NoSchedule`, `PreferNoSchedule`, `NoExecute`, or empty for any). An empty key requires `Exists`, and `Exists` requires an empty value. |
| `runtime_class_name` | string | kubernetes | RuntimeClass of every agent pod of the instance (DNS-1123 subdomain). The RuntimeClass must already exist on the cluster. |

A runtime target does not choose where its agents' storage lives: [`server.project_storage`](#project-storage-serverproject_storage) does, for every instance of the process. The former `shared_dir_storage_backend`, `shared_dir_storage_backends`, `home_storage_backend` and `home_storage_leaf` fields are removed. A flat instance that sets them refuses startup with an error that names the field, `server.project_storage` and `volumes:`. On runtimes and profiles, startup reports them and agent start refuses them; see [Removed storage overrides](#removed-storage-overrides). For a node-local directory, use a `volumes:` entry.

#### Flat instance placement

`node_selector`, `tolerations` and `runtime_class_name` place the instance's agent pods on the nodes the operator chooses, whatever template the agent uses, including `default`. Unlike the other defaults in the table, the operator owns them, so an agent's own value does not win:

- An agent's `kubernetes.nodeSelector` may add keys the runtime target does not set. It cannot change the value of a key the runtime target sets, including the pool key `scion.dev/pool`. A differing value refuses the start.
- Each of an agent's `kubernetes.tolerations` must match one of the runtime target's tolerations exactly: the same key, operator (empty counts as `Equal`), value and effect. A broader `Exists` toleration does not cover a different entry. When the runtime target sets no tolerations, any agent toleration is refused, including one with an empty key and `Exists`. While placement is set, an agent toleration with an empty key and `Exists` is refused even when the runtime target has the same entry.
- An agent cannot choose or change the runtime class. An agent `kubernetes.runtimeClassName` must be empty or equal to the runtime target's value.

A refused start is reported as a validation error that names the field and whether it comes from the request or the agent's stored configuration. The check runs before the start creates, changes or stops anything. Node selector entries combine: an agent-added key narrows placement further, and a key with no matching nodes leaves the pod pending. An agent-added key can also steer a pod onto any pool whose nodes carry no taint, because only the runtime target's keys are fixed. Tolerations stay limited to the runtime target's. A node selector and a toleration are independent: Scion never derives one from the other, so a tainted pool needs both.

The values are read from the instance's settings when it starts, so a change takes effect when the Runtime Broker restarts. Existing pods keep their placement. The Brokers page shows the instance's reported placement read-only, as defaults for new agents. In GCP identity `block` mode the Workload Identity node label is still added after this placement.

```yaml
server:
  broker:
    instances:
      - key: agents
        name: agents
        runtime_target:
          type: kubernetes
          namespace: agents
          node_selector:
            scion.dev/pool: agents
          tolerations:
            - key: scion.dev/pool
              operator: Equal
              value: agents
              effect: NoSchedule
          runtime_class_name: gvisor
```

`gvisor` is an example: Scion does not create RuntimeClasses or check that one exists.

This release checks and enforces these rules for flat instances only. Runtime entries and profiles of a regular Runtime Broker keep their current behaviour and follow in a later release.

##### Before upgrading

An agent that sets its own tolerations or runtime class while the instance's runtime target sets none is refused at its next start or restart. Running agents keep their pods until then. Before upgrading a host with flat instances:

1. Run `scion doctor` with the new release. The `flat-placement-preflight` check scans the stored configuration of every agent each flat instance owns, without changing anything, and reports counts per instance: agents with tolerations while the runtime target sets none (`agent_tolerations_without_operator_policy`), agents with a runtime class while the runtime target sets none (`agent_runtime_class_without_operator_policy`), and agents it could not check (`incomplete`). It shows no agent names or configuration values. An agent can be counted in both categories.
2. Treat `incomplete` as unfinished: fix unreadable agent records or configurations, then run the check again.
3. For agent tolerations, decide whether each belongs to the instance. If it does, add it to the runtime target's `tolerations`; otherwise remove it from the agent's configuration. Tolerations match by key, operator (empty counts as `Equal`), value and effect; a broad `Exists` toleration on the runtime target does not cover a different agent entry. A toleration added to the runtime target applies to every agent of the instance.
4. For an agent runtime class, either set `runtime_class_name` on the runtime target or remove the agent's class. An agent cannot choose a class on its own; repeating the runtime target's class is accepted.
5. Restart the Runtime Broker to activate runtime target changes, then run `scion doctor` again until every count and `incomplete` are zero.

The check always shows these two counts. It also counts agents the runtime target would refuse for another reason: a toleration that is not among the target's (`agent_toleration_not_in_policy`), a runtime class that differs from the target's (`agent_runtime_class_differs`), a node selector key the target sets to another value (`agent_pool_selector_conflict` for `scion.dev/pool`, `agent_selector_conflicts_operator` otherwise), a toleration that matches all taints (`agent_match_all_toleration`) and an invalid placement field (`agent_invalid_placement_field`). These appear only when counted.

#### Legacy host placement check

On a host without flat instances, `scion doctor` reports a `legacy-placement-preflight` line. It changes nothing and shows counts only, never names or values. It covers this host's global settings file and the agents in the global project, the hub-managed projects and the project you run it in.

- **Settings:** runtime entries and profiles whose placement the Hub refuses when they are saved: `settings_malformed_placement` (a misspelt key such as `nodeSelector`, or a wrong shape), `settings_invalid_placement_field`, `settings_placement_on_non_kubernetes`, `settings_profile_runtime_missing` (a profile with placement whose runtime entry does not exist), `settings_sibling_placement_disagreement` (entries with the same context and namespace but different placement) and `settings_pool_selector_conflict` (a profile that changes `scion.dev/pool`). The line names the sections to repair.
- **Agents:** the same agent categories as the flat check, against the runtime entry and profile each agent uses. With no Kubernetes placement configured, agent tolerations and runtime classes count as `agent_tolerations_without_operator_policy` and `agent_runtime_class_without_operator_policy`.
- **Incomplete coverage:** `incomplete` counts agents whose record, configuration, profile or runtime cannot be read or resolved here. `not enumerated` names sources the check cannot list: linked projects other than the current one (run `scion doctor` inside each). Neither is a clean result.

Settings stored in the Hub are reported by `placement_preflight` in the server config view (see [admin settings](/scion/reference/admin-settings/)). Repair the named settings first, then the counted agents, and run the check again.

#### Flat instance agent defaults (`server.broker.instances[].agent_defaults`)

An optional block of workload defaults for the agents a flat Runtime Broker instance runs. A flat instance does not use Runtime Broker profiles, so these take the place of the matching profile settings. Each value sits exactly where the profile value sat in its own precedence order: a template, the agent's own configuration or the request still wins wherever it won over a profile, and an unset value falls through to the same lower settings. The block is read from `settings.yaml` only and is never sent to the Hub. Unknown keys are refused at startup, naming the settings path.

| Key | Type | Precedence |
| :--- | :--- | :--- |
| `default_harness_config` | string | Below the template's and the agent's stored harness config; above the top-level `default_harness_config`. |
| `image_registry` | string | Above the top-level `image_registry` of the settings file; below a saved Hub top-level value and the node's `SCION_IMAGE_REGISTRY` (see [Image registry order](/scion/reference/settings-precedence/#image-registry-order)). |
| `volumes` | list of volume mounts | Appended after the harness config's volumes. |
| `resources` | resource spec | The agent's own resources win over it; it wins over the Hub `agent_defaults.resources` and `default_resources`. |
| `harness_overrides.<harness-config>` | `image`, `image_pull_policy`, `user`, `auth_selected_type`, `env`, `volumes`, `resources` | Applied over `harness_configs.<harness-config>`, as a profile's harness override is. An explicit `image` outranks the template and inline images (only a request image beats it). Its `resources` rank above the agent's own. An empty `env` value in any override marks a key the user must supply. Validated like a profile's harness override. |
| `secrets` | list of secret declarations (no values) | Required secrets, between the harness config's secrets and the template's. |
| `clone_depth` | `"full"` or an integer from 1 | Below the template's or agent's `clone_depth`; unset keeps the default shallow clone. |

```yaml
server:
  broker:
    instances:
      - key: docker-1
        name: docker-1
        runtime_target:
          type: docker
        agent_defaults:
          image_registry: registry.example.com/team
          resources:
            requests:
              memory: 2Gi
          harness_overrides:
            claude:
              auth_selected_type: vertex-ai
```

#### Runtime Broker environment variables

These environment variables are read by the Runtime Broker process when launching Docker or Podman agent containers:

| Variable | Default | Description |
| :--- | :--- | :--- |
| `SCION_FORCE_HOST_NETWORK` | unset | When set to any non-empty value, forces agent containers to run with `--network=host` whenever a Hub endpoint is configured. Without it, only Docker uses host networking, and only for a loopback Hub endpoint (`localhost`, `127.0.0.1`, `::1`, or `host.docker.internal`). |
| `SCION_HOST_NETWORK_BLOCK_POLICY` | `auto` | Controls how the Runtime Broker handles an agent whose GCP identity metadata mode is `block` when the container runs with `--network=host` (where in-container `iptables` interception of `169.254.169.254` cannot be installed). `auto` (or unset) refuses agent start on a detected Linux cloud host (GCE, AWS EC2, Azure, Oracle Cloud, Alibaba Cloud, OpenStack, DigitalOcean, or Hetzner, detected from local DMI, `cloud-init`, or GCE guest-agent files) and allows it with a warning on other hosts; `strict` always refuses `block` under host networking; `allow` allows agent start even on a detected cloud host and logs a warning. Any unrecognized value is logged at `ERROR` and treated as `strict`. See [Host networking on Docker and Podman](/scion/hosted/single-node/auth/#host-networking-on-docker-and-podman). |

### Database (`server.database`)

Persistence settings for the Hub.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `driver` | string | `"sqlite"` | Database driver: `sqlite` or `postgres`. |
| `url` | string | `"hub.db"` | Connection string or file path. |
| `max_open_conns` | integer | sqlite: `1`; postgres: `5` | Maximum number of open connections. For `sqlite` this is always `1`, whatever is configured, to serialize writes. For `postgres` a value of `1` or less takes the default. Settings file only: no environment variable sets it. |
| `max_idle_conns` | integer | sqlite: `1`; postgres: `2` | Maximum number of idle connections in the pool. For `postgres` a value of `1` or less takes the default; for `sqlite`, a value of `0` or less. Settings file only: no environment variable sets it. |
| `conn_max_lifetime` | duration | sqlite: unlimited; postgres: `"30m"` | Maximum time a connection may be reused (Go duration, for example `"30m"`). Empty takes the default; `"0"` means unlimited. Settings file only: no environment variable sets it. |
| `conn_max_idle_time` | duration | sqlite: no limit; postgres: `"5m"` | Maximum time a connection may sit idle in the pool before it is closed (Go duration). Set it shorter than the server-side or proxy idle timeout so idle connections are recycled first. Empty takes the default; `"0"` means no idle limit. Settings file only: no environment variable sets it. |
| `allow_shadowed_schema` | bool | `false` | PostgreSQL only. Lets the Hub start, and the maintenance commands that migrate its database run, when the schema migration would create a new table set that hides existing Hub tables in a later schema on the `search_path`. See [PostgreSQL `search_path` check](#postgresql-search_path-check). Env: `SCION_SERVER_DATABASE_ALLOWSHADOWEDSCHEMA`. |
| `tx_diagnostics` | object | see below | Transaction-hold and lock-wait diagnostics. See [Transaction diagnostics](#transaction-diagnostics). Settings file only. |

#### Transaction diagnostics

The Hub measures how long each database transaction and connection checkout holds its connection, and how long each request waits to get one. SQLite runs one writer at a time, and the Hub's SQLite pool has a single connection, so one slow holder makes every other database request wait behind it. These diagnostics name the holder and the waiters.

| Field (`server.database.tx_diagnostics.*`) | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `enabled` | bool | `true` | Install the diagnostics. `false` opens the database exactly as without them. |
| `hold_warn_threshold` | duration | `"2s"` | Log a transaction or connection checkout held at least this long. `"0"` turns these lines off; metrics are still recorded. |
| `lock_wait_warn_threshold` | duration | `"1s"` | Log a request that waited at least this long for a connection. `"0"` turns these lines off. |
| `warn_interval` | duration | `"1m"` | At most one line per call site and kind per interval. Must be positive; `"0"` or a negative value fails startup. Lines dropped by this limit are counted in the next line's `suppressed`. |

The lines are WARN records from the `hub.db` subsystem:

- `db_tx_long_hold`: `kind` (`tx`, or on SQLite `conn` for a held connection with no transaction line), `op` (the calling function), `site` (the first three calling frames), `hold_ms`, `begin_ms`, `stmts`, `stmt_ms` (time in SQL), `commit_ms` (time in the transaction's COMMIT; on SQLite that includes writing and syncing the WAL, so a slow disk shows up here; on a `kind=conn` line, `begin_ms` and `commit_ms` add up the transactions run during that checkout), `idle_ms` (time the connection was held outside BEGIN, SQL and COMMIT, such as Go work inside a transaction), `max_stmt_ms`, `rows_affected`, `outcome`, and on SQLite `waiters_max` (requests from the ent client that waited during the hold) and `busy`.
- `db_lock_wait`: `op`, `site`, `wait_ms`, `req` (`tx`, `exec` or `query`) and `outcome` (`acquired`, `canceled`, `deadline` or `error`). On SQLite it also names who held the connection during the wait: `holders` (how many connection checkouts overlapped the wait; many short ones mean a queue rather than one slow holder), `top_holder_op` and `top_holder_ms` (the holder with the most time in the wait), and, when the waiter gave up, `holder_open_op` and `holder_open_ms` (the checkout still holding the connection at that moment).

A write made outside an explicit transaction commits inside its own statement, so its commit time is part of that statement's `stmt_ms` and is not reported as `commit_ms`. Both carry `suppressed` and `dropped` (lines lost because the log queue was full). Per-waiter lines cover requests made through the ent client. Waits of the raw-SQL stores appear in aggregate, in the `scion.db.pool.connections.wait_duration` metric. Startup schema migrations run through the same path, so a long migration can log one or more lines at boot; they are expected and are not a stall.

When metrics export is on, the Hub also records the histograms `scion.db.tx.hold.duration` (labels `op`, `kind`, `outcome`, `pool`), `scion.db.tx.lock_wait.duration` and `scion.db.tx.commit.duration` (labels `op`, `outcome`, `pool`), the counter `scion.db.pool.connections.wait_duration` (ms), and `scion.db.diag.warn.dropped`. The `op` label is capped at 256 values; further call sites are reported as `other`.

#### PostgreSQL `search_path` check

On PostgreSQL, the Hub's schema migration looks up and creates its tables in `current_schema()`, the first existing schema on the connection's `search_path`. If that schema has no Hub tables but a later schema on the `search_path` does, the migration would create a new, empty table set in the first schema. That set would hide the existing data, and the Hub would start up looking like a new install. This can happen with:

- a custom `search_path`, such as `custom,public`, where `custom` has no Hub tables and `public` holds them;
- the default `"$user", public`, when a schema named after the database user exists and is empty.

So before migrating, the Hub server and every maintenance command that migrates the Hub database check the schemas on the `search_path`. A schema counts as holding Hub tables when it has both the `agents` and `runtime_brokers` tables. If the current schema has none and a later schema does, the Hub refuses to start (and the command refuses to migrate), and the error names both schemas. To fix it, change the connection's `search_path` so the schema with the Hub tables comes first, or drop the current schema if nothing else uses it.

If you do want a new table set in the current schema, set `server.database.allow_shadowed_schema: true` (or `SCION_SERVER_DATABASE_ALLOWSHADOWEDSCHEMA=true`). The Hub, or the command, then logs a warning that names both schemas and continues.

A fresh database (no Hub tables in any schema on the `search_path`) and an existing install whose current schema holds the Hub tables start as before. SQLite is not affected. The same check and override apply to the maintenance commands that migrate the database: `server migrate` (on its PostgreSQL destination), `server backfill` and `server migrate-dm-keys` (including their default dry-run mode), `server recover-authz`, `server test-infra mark` and `hub secret migrate-names` (except with `--dry-run`, which does not migrate). These commands read the override from the server settings or the environment, as the Hub does. `hub secret migrate` opens only SQLite, so the check does not apply to it.

:::caution[Permanent decision-audit data removal]
Any schema-migration entry point may permanently drop `decision_audits` and its data through `entc.AutoMigrate`, directly or through `CompositeStore.Migrate`. Export first if preservation is required. During Hub schema migration on PostgreSQL, the drop takes an `ACCESS EXCLUSIVE` table lock while the existing advisory schema lock is held.

Direct maintenance callers include:

- `server recover-authz`: SQLite/PostgreSQL through `CompositeStore.Migrate`.
- `hub secret migrate-names`: SQLite/PostgreSQL through `CompositeStore.Migrate`, except with `--dry-run`.
- `hub secret migrate`: SQLite through `CompositeStore.Migrate`, including with `--dry-run`.
- `server backfill` and `server migrate-dm-keys`: SQLite/PostgreSQL through `entc.AutoMigrate`, including their default dry-run mode.
- `server migrate`: `entc.AutoMigrate` on the PostgreSQL destination only; the SQLite source is read-only and unaffected. Source decision-audit rows are not copied, as in the existing migration behavior.

These direct calls run outside the Hub's advisory schema lock. Mixed old replicas may report degraded legacy health as well as write failures after the drop. Rolling back to an old binary can recreate an empty table but cannot restore the deleted data.
:::

:::caution[Duplicate rows removed before schema migration]
Three tables get a unique index that older Hubs did not enforce, so an existing database can hold rows the index rejects. Before the schema migration, every migrate entry point runs the same pre-migration steps (`entadapter.PreMigrate`), in this order:

1. `access_policies`: a NULL `scope_id` becomes an empty string, so the next step also finds those duplicates.
2. `access_policies`: one row is kept per (`name`, `scope_type`, `scope_id`), the oldest by `created`.
3. `delegation_edges`: one active edge is kept per (`delegate_type`, `delegate_id`, `scope_type`, `scope_id`), the oldest by `created`. Inactive edges are kept.
4. `agent_session_metrics`: one row is kept per agent, session ID and segment start (`started_at`), the earliest stored. Older Hubs stored every report, so a database can hold repeated rows for one segment. Rows for separate segments of a resumed session are kept.

The steps are idempotent and do nothing on a fresh database. They run on Hub start (`CompositeStore.Migrate`, which `server recover-authz`, `hub secret migrate-names` and `hub secret migrate` also use), and in `server backfill`, `server migrate-dm-keys` and `server migrate` (on the PostgreSQL destination) before those commands call `entc.AutoMigrate`. `server backfill` and `server migrate-dm-keys` run them only with `--execute`, so their default dry run removes no rows (it still applies the schema migration); on a database holding such duplicates the dry run therefore fails with a unique-constraint error. Rerun with `--execute`, or start the Hub once on the new version, to remove them. `server migrate` opens its SQLite source read-only and does not de-duplicate it, so duplicate `access_policies` rows in an older source still fail the copy; start the Hub on the source database with the new version first (or run `server backfill --execute` against it), then copy. The read-only `server attribution-report` runs no migration and no pre-migration steps.
:::

:::caution[Postgres: `broker_dispatch` index on upgrade]
On Postgres, auto-migrate creates the `brokerdispatch_state_updated_at` index on `broker_dispatch (state, updated_at)` with a plain `CREATE INDEX`, which blocks writes to the table while it builds. On a large deployment, create the index before upgrading so auto-migrate finds it already in place:

```sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS brokerdispatch_state_updated_at ON broker_dispatch (state, updated_at);
```
:::

:::caution[`messages` indexes on upgrade]
On Postgres, auto-migrate creates these `messages` indexes with a plain `CREATE INDEX`:

- `message_project_id_created_id` on `(project_id, created, id)`, for project message pages.
- `message_sender_id_created_id` on `(sender_id, created, id)` and `message_recipient_id_created_id` on `(recipient_id, created, id)`, for participant reads such as the agent DM inter-agent view.
- `message_project_id_sender_created_id` on `(project_id, sender, created, id)`, for the inter-agent view's lookup by sender name.

Each build blocks writes to messages until the startup migration commits; during a rolling deploy, writes from the other Hub instances wait too. On a large deployment, create the indexes before upgrading so auto-migrate finds them already in place:

```sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS message_project_id_created_id ON messages (project_id, created, id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS message_sender_id_created_id ON messages (sender_id, created, id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS message_recipient_id_created_id ON messages (recipient_id, created, id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS message_project_id_sender_created_id ON messages (project_id, sender, created, id);
```

On SQLite, the first start after upgrading builds the same indexes before the Hub serves requests, holding the database write lock: allow a few seconds per million messages.

The three participant and sender indexes take disk space too: about 370 MiB per million messages on SQLite. The cost is per message, whatever the message length.
:::

### Authentication (`server.auth`)

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `mode` | string | | Human auth mode. `"proxy"` is the only value the server checks: it then uses the proxy authenticator configured under `proxy` and offers no OAuth providers. Any other value, including unset (the default), `"oauth"` and `"dev"`, leaves the hub handling authentication itself. |
| `dev_mode` | bool | `false` | Enable insecure development authentication. This (or the `--dev-auth` flag) is the dev auth switch; `mode` does not enable it. Requires a loopback listen host (`127.0.0.1`, `::1`, or `localhost`), both when the web server is enabled and when the Hub API runs standalone with the web server disabled. Browser sign-in uses single-use launch links minted by `POST /auth/dev-launch` and consumed at `GET /auth/dev-signin` (both return `404` when `dev_mode` is `false`). |
| `dev_token` | string | | Static token for dev mode. |
| `dev_token_file` | string | `~/.scion/dev-token` | File the dev mode token is read from, or written to if it does not exist. A relative path is resolved against the global config directory (`~/.scion`). |
| `authorized_domains` | list | `[]` | Limit access to specific email domains. |
| `user_access_mode` | string | `"open"` | Who may sign in: `"open"` (any verified email, subject to `authorized_domains` if set), `"domain_restricted"` (email domain must be in `authorized_domains`), or `"invite_only"` (the email must belong to an invited, allow-listed or existing user). Users in `admin_emails` are always allowed. |
| `default_user_role` | string | `"member"` | Hub role given to a user when their account is first created or activated: first sign-in, including the first sign-in of an invited or allow-listed user. Values: `"member"` or `"viewer"` (`"admin"` is rejected; use `admin_emails`). Users in `admin_emails` are always admins. Changing it does not affect existing users. It is also the role given to an admin who is removed from `admin_emails`. See [Hub roles](/scion/hosted/ha/permissions/#hub-roles). Environment: `SCION_SEED_SERVER_AUTH_DEFAULTUSERROLE` (recommended) or `SCION_SERVER_AUTH_DEFAULTUSERROLE` (per-node, deprecated for Layer-1 keys). |
| `agent_run_scope` | string | `"off"` | How the Hub treats the run an agent token was issued for. Agent tokens carry a `run_id` claim. `"off"` ignores it; `"observe"` compares it with the agent's current run and logs and counts mismatches (metric `scion.hub.agent_token.run_scope`) without refusing any request. Enforcement cannot be selected yet. Read at startup, so a change needs a Hub restart. Environment: `SCION_SERVER_AUTH_AGENTRUNSCOPE`. |
| `agent_run_scope_legacy_until` | string | | RFC 3339 time after which agent tokens issued without a `run_id` claim are no longer accepted by the run check (in `observe` mode, counted as mismatches). Empty means no cut-off. Environment: `SCION_SERVER_AUTH_AGENTRUNSCOPELEGACYUNTIL`. |

### Proxy Auth (`server.auth.proxy`)

Proxy authentication configuration (consulted when `server.auth.mode` is set to `"proxy"`). See [Proxy Auth (Google IAP)](/scion/hosted/ha/auth-proxy-iap/) for the full deployment guide, and [Generic JWT proxy provider](/scion/hosted/ha/auth-proxy-iap/#generic-jwt-proxy-provider) for bespoke auth proxies.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `provider` | string | | Selects the proxy auth provider: `"iap"`, `"jwt"`, or `"header"`. |
| `require_trusted_proxy_ip` | bool | `false` | Enables defense-in-depth IP allowlisting. Uses the trusted_proxies CIDR list. |
| `driver_principals` | list | `[]` | Exact emails (no wildcards) of the browser drivers, such as a test-UI service account, that may carry test identity browser sessions through the proxy. A driver principal never gets a hub account: without a test identity session, page navigations and assertion-only API requests get `403` (`driver_principal_has_no_session`), and API requests with its ID token never act as it (`401` or `403`). Startup only; never writable through the settings API and not shown by it. A hub whose `mode` is not `"proxy"` refuses to start with a non-empty list. See [Browser Sessions as Test Identities](/scion/contributing/test-identity-browser-sessions/#iap-fronted-hubs). |
| `driver_token_audience` | string | | The IAP OAuth client ID. On `/api/v1/` requests, a driver's Google ID token with exactly this audience is replaced by the test identity session it carries (only when every condition in the how-to holds). Unset: never replaced. Startup only; never writable through the settings API and not shown by it. |

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

**Credentials-file fields** (per hub connection, in `~/.scion/hub-credentials/<name>.json`, persisted by `scion runtime-broker register` or `scion runtime-broker join`):

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

### Project Storage (`server.project_storage`)

The one storage block of a Hub and its Runtime Brokers. `backend` places the workspace **and** the shared directories of every project, with one value for both. `homes` places agent home directories, and is the only per-data-class choice. The block replaces [`server.workspace_storage`](#workspace-storage-serverworkspace_storage), [`server.shared_dir_storage`](#shared-directory-storage-servershared_dir_storage) and [`server.home_storage`](#agent-home-storage-serverhome_storage), which are deprecated and will be removed in a later release (see [Migrating from the deprecated blocks](#migrating-from-the-deprecated-blocks)).

```yaml
server:
  project_storage:
    backend: nfs        # local | nfs | cloudrun-volume | gke-shared-volume
    homes: local        # local | nfs (nfs needs backend nfs)
    nfs:
      mount_root: /mnt/scion-nfs
      subpath_root: projects
      shares:
        - id: shared
          server: 10.0.0.2
          export: /scion-projects
          pv_name: scion-projects-pvc
```

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `backend` | string | `"local"` | Where workspaces and shared directories live: `"local"` (node-local directories), `"nfs"` (an NFS export), `"cloudrun-volume"` (a Cloud Run platform-managed volume) or `"gke-shared-volume"` (a GKE shared CSI-backed PVC). Names are case-sensitive. |
| `homes` | string | `"local"` | Where agent home directories live: `"local"` (in the pod, or on the broker host) or `"nfs"` (on the `nfs` share of this block). `"nfs"` needs `backend: nfs`. See [Agent home storage](#agent-home-storage-serverhome_storage). |
| `nfs.*` | object | | The NFS block, with the same fields and defaults as [`server.workspace_storage.nfs`](#workspace-storage-serverworkspace_storage): `mount_root`, `mount_options`, `auto_mount`, `uid`, `gid`, `storage_class`, `subpath_root` and `shares` (`id`, `server`, `export`, `pv_name`). Used when `backend` is `"nfs"`. |
| `cloudrun_volume.*` | object | | `volume_name` and `subpath_root`, as in [`server.workspace_storage.cloudrun_volume`](#workspace-storage-serverworkspace_storage). Used when `backend` is `"cloudrun-volume"`. |
| `gke_shared_volume.*` | object | | `volume_name`, `pv_claim_name` and `subpath_root`, as in [`server.workspace_storage.gke_shared_volume`](#workspace-storage-serverworkspace_storage). Used when `backend` is `"gke-shared-volume"`. |
| `homes_options.*` | object | | The operational knobs of agent homes, with the meaning and defaults they had under `server.home_storage`: `leaf` (`pod`), `stop_grace_seconds` (`30`), `termination_wait_seconds` (`15`), `skeleton_max_bytes` (`268435456`) and `allow_incomplete_phases` (`false`). See [Agent home storage](#agent-home-storage-serverhome_storage). |

The operational content of the deprecated sections still applies to the same fields under `server.project_storage`: [startup validation](#startup-validation) of the backend blocks, [subpath_root](#subpath_root), [NFS mounts on the Runtime Broker](#nfs-mounts-on-the-runtime-broker), [NFS workspaces on Kubernetes](#nfs-workspaces-on-kubernetes), [cleanup on project delete](#cleanup-on-project-delete), the [503 safety gate](#ephemeral-storage--503-safety-gate), [agent groups on NFS shared directories](#agent-groups-on-nfs-shared-directories) and the `cloudrun` runtime's need for the `nfs` backend.

#### Where each data class lives

| `backend` | Workspaces | Shared directories |
| :--- | :--- | :--- |
| `local` | Node-local | Node-local, next to the project's local config |
| `nfs` | `<mount_root>/<share id>/<subpath_root>/<project id>/...` on the export | `<subpath_root>/<project id>/shared-dirs/<name>` on the same export, on every runtime, including Docker and Podman |
| `cloudrun-volume`, `gke-shared-volume` | `<subpath_root>/<project id>/...` on the volume | `<subpath_root>/<project id>/shared-dirs/<name>` on the same volume |

- **Docker and Podman follow the backend.** With `backend: nfs`, shared directories are bind-mounted from the export on Docker and Podman too. Before this block existed, a broker with `server.workspace_storage.backend: nfs` and no `server.shared_dir_storage` kept Docker shared directories node-local; that is no longer the case. For shared directories already on node-local disk: the Hub's `project-storage-migrate` maintenance migration (Admin → Maintenance, or `POST /api/v1/admin/maintenance/migrations/project-storage-migrate/run`; run it with `dryRun` first) moves the Hub's own trees (and moves stopped (`stopped` or `halted`, not running or `error`) Cloud Run agents' legacy `agents/<agent id>/{home,secrets}` directories on the workspace share into the unified `<subpath_root>/<project id>/agents/<slug>/{home-,secrets-}<agent id>` layout under the project's provisioning locks; running agents are deferred and a Cloud Run agent whose legacy home has not yet been moved is refused at start); each Runtime Broker moves its own node-local shared dirs (`~/.scion/project-configs/<slug>__<id8>/shared-dirs/<name>`) into `<ProjectRoot>/shared-dirs/<name>` on the share on its first start after upgrade with its shared-dir layout on the share (copy-only, never overwriting; the source is kept as `<name>.migrated-<ts>`).
- **Hub-seeded `.scion` on the share, broker-local agent state.** On a shared backend (`nfs`, `cloudrun-volume`, `gke-shared-volume`), the Hub seeds `<ProjectRoot>/.scion` (`project-id`, `project-slug`, and where applicable `settings.yaml`) at project create, clone, and register for per-agent git projects (`clone-per-agent` and `empty-per-agent`) as well as Hub-managed and shared-workspace projects (and provisions `<ProjectRoot>/workspace` at Hub-managed project create). The Runtime Broker runs Hub-managed projects from `<ProjectRoot>` (`<subpath_root>/<project id>`) and reads `<ProjectRoot>/.scion`, while keeping every agent's state directory (`agent-info.json`, local Agent home copy, worktree state) broker-local under `~/.scion/project-configs/<slug>__<id8>/.scion/agents` (falling back to `~/.scion/projects/<slug>/.scion/agents` for pre-upgrade agents). On Kubernetes with `backend: nfs`, `clone-per-agent` and `empty-per-agent` workspaces live on the share at `<subpath_root>/<project id>/agents/<slug>/workspace` (while non-Kubernetes runtimes such as Docker and Podman continue to refuse `clone-per-agent` on a shared layout at start). Agent homes on the shared export use one path shape across Kubernetes and Cloud Run: `<subpath_root>/<project id>/agents/<slug>/home-<agent id>` (and `secrets-<agent id>` on Cloud Run).
- **Volume backends open the shared-directory write gate.** Because `cloudrun-volume` and `gke-shared-volume` put shared directories on the volume, a Cloud Run Hub accepts shared-directory writes on those backends. A Cloud Run Hub without a shared backend refuses them with `503` and error code `shared_dir_writes_unavailable`, whose message names `server.project_storage`.
- **Removing a shared directory deletes it.** Removing a shared directory declaration deletes its directory on the shared backends, as it does on local disk.
- **Node-local directories are volumes.** A directory that must stay on the node, such as a build cache, is an ordinary `volumes:` entry on a profile, harness config or template, not a storage backend.

#### Configuration source (Layer-0)

`server.project_storage` is a [Layer-0](#layer-0--bootstrap-file--env-only) setting. The Hub and every Runtime Broker read it from their own global `settings.yaml` at startup; the Hub also reads `SCION_SERVER_PROJECT_STORAGE_*` environment variables. It is never read from the database or from a project's settings. On a workstation Hub, a `PUT /api/v1/admin/server-config` body that sets `server.project_storage` writes it into `settings.yaml` and lists the key in the response's `reload.requires_restart`; a hosted Hub refuses it with `422` (see [Layer-0 edits by deployment mode](/scion/reference/admin-settings/#layer-0-edits-by-deployment-mode)). The admin UI has no field for it. Either way, a change needs a restart. Configure the same block on the Hub and on every Runtime Broker that serves its hub-native projects (see [Layout identity check](#layout-identity-check)).

#### Validation

The Hub refuses to start, and `scion config validate` reports an error, when the conditions below hold. A Runtime Broker does not refuse to start: it logs the error and reports the local layout, and the Hub then refuses shared-mode dispatches to it (see [Layout identity](#layout-identity-check)).

- `server.project_storage` is set together with any of `server.workspace_storage`, `server.shared_dir_storage` or `server.home_storage`. Exactly one storage block configures a deployment; the error names the blocks to remove.
- the backend block is invalid, by the same rules as [`server.workspace_storage`](#startup-validation), reported under `server.project_storage`;
- `homes` is not `local` or `nfs`, or is `nfs` while `backend` is not `nfs`;
- a `homes_options` value is invalid;
- (Hub) the Hub cannot build its storage layout: for example, the share's host base is not an absolute path, because `nfs.mount_root` is empty or relative. The Hub serves files from its own mount, so it refuses to start rather than refuse every shared-directory request later;
- (Hub) the global settings file, in any format, has a storage block key (`project_storage`, `workspace_storage`, `shared_dir_storage` or `home_storage`) as a parsed mapping key, but the block was not loaded: for example a block not under `server:`, a missing `schema_version: "1"`, or a server section that fails to decode. The Hub refuses to fall back to the local backend. A comment, or a key such as `shared_dir_storage_class` that only contains the name, never triggers this.

#### Migrating from the deprecated blocks

Without `server.project_storage`, the Hub and the Runtime Brokers synthesize it from the deprecated blocks at startup:

- `server.workspace_storage` gives `backend` and the backend blocks.
- `server.home_storage.backend` gives `homes`; its other fields give `homes_options`.
- `server.shared_dir_storage` decides where shared directories go:
  - `nfs` on the same tree as the workspace (same NFS server, export and `subpath_root`): nothing changes.
  - `nfs` while the workspace is not on `nfs`: shared directories (and `nfs` homes) stay on the `shared_dir_storage` export and workspaces stay where they are. This transitional split exists only for synthesized configurations; it cannot be written as a `server.project_storage` block.
  - `local` while the workspace is on `nfs`: shared directories are now placed on the export with the workspace, and a warning says so.
  - unset while the workspace is on `nfs`: shared directories follow the workspace onto the export on every runtime (Docker, Podman and Apple agents kept them node-local before), and a warning says so. For existing data: the Hub's `project-storage-migrate` maintenance migration (Admin → Maintenance, or `POST /api/v1/admin/maintenance/migrations/project-storage-migrate/run`; run it with `dryRun` first) moves the Hub's own trees; each Runtime Broker moves its own node-local shared dirs into `<ProjectRoot>/shared-dirs/<name>` on the share on its first start after upgrade with its shared-dir layout on the share (copy-only, never overwriting; the source is kept as `<name>.migrated-<ts>`).
  - unset, or `local`, while the workspace is on `cloudrun-volume` or `gke-shared-volume`: shared directories stay where they are today (their own claims on Kubernetes) until `server.project_storage` is written. The Hub then treats them as non-durable, so a Cloud Run Hub refuses shared-directory writes with `503`. Writing the block moves shared directories onto the volume, and **the contents of the existing per-directory claims are not moved automatically**. While such a claim still exists, every agent start that now places that directory on the volume returns a warning naming the claim. Scion never deletes those claims.

Every synthesis logs a startup warning that names the deprecated blocks in use and prints the exact `server.project_storage` block to write, with defaults filled in. The Hub's `project_storage_config` health check reports `degraded` until the block is written. For the transitional split, the printed block is `backend: nfs` on the shared-directory export: writing it moves workspaces onto the export too, because one backend places both.

Synthesis is refused, and startup fails, only when the deprecated blocks are ambiguous:

- `workspace_storage` and `shared_dir_storage` are both `nfs` but name different trees (a different NFS server or export, or a different `subpath_root`; the share id and `mount_root` are host-local naming and may differ). One storage block places both, so choosing either would hide the other's data. Move one tree onto the other's share, then write `server.project_storage` naming it.
- `home_storage.backend` is `nfs` but neither `workspace_storage` nor `shared_dir_storage` is `nfs`, so there is no share for the homes.

For example, a deployment with workspaces and shared directories on the same export and the broker home leaf:

```yaml
# Before (deprecated)
server:
  workspace_storage:
    backend: nfs
    nfs:
      mount_root: /mnt/scion-nfs
      shares:
        - id: shared
          server: 10.0.0.2
          export: /scion-projects
          pv_name: scion-projects-pvc
  shared_dir_storage:
    backend: nfs
    nfs:
      mount_root: /mnt/scion-nfs
      shares:
        - id: shared
          server: 10.0.0.2
          export: /scion-projects
          pv_name: scion-projects-pvc
  home_storage:
    leaf: broker
```

```yaml
# After
server:
  project_storage:
    backend: nfs
    nfs:
      mount_root: /mnt/scion-nfs
      shares:
        - id: shared
          server: 10.0.0.2
          export: /scion-projects
          pv_name: scion-projects-pvc
    homes_options:
      leaf: broker
```

Remove all three deprecated blocks in the same edit, on the Hub and on every Runtime Broker, and restart. Check the edited file with `scion config validate`. For data that the new block places elsewhere, such as hub-local workspaces and node-local shared directories: the Hub's `project-storage-migrate` maintenance migration (Admin → Maintenance, or `POST /api/v1/admin/maintenance/migrations/project-storage-migrate/run`; run it with `dryRun` first) moves the Hub's own trees; each Runtime Broker moves its own node-local shared dirs into `<ProjectRoot>/shared-dirs/<name>` on the share on its first start after upgrade with its shared-dir layout on the share (copy-only, never overwriting; the source is kept as `<name>.migrated-<ts>`). No `scion` CLI command runs this migration; see [Maintenance Panel Operations](/scion/hosted/single-node/hub-server/).

#### Removing a shared directory deletes its data

Removing a shared-directory declaration through the Hub deletes that directory and its contents on a shared backend (`nfs`, `cloudrun-volume`, `gke-shared-volume`, and the shared-directory export of the transitional split). The deletion uses the same confined, symlink-refusing walk as project deletion. The local backend is unchanged: the local directory is deleted, as before. The Required `scratchpad` cannot be removed while `project_defaults.default_scratchpad` is on (`409`).

#### Removed storage overrides

Storage placement is no longer chosen per runtime, per profile, per instance, per shared directory or per agent. These keys are removed:

- `shared_dir_storage_backend` and `shared_dir_storage_backends` on `runtimes.<name>`, `profiles.<name>` and `server.broker.instances[].runtime_target`;
- `home_storage_backend` and `home_storage_leaf` on the same three places.

Settings validation (`scion config validate`), the Hub admin settings API (`PUT /api/v1/admin/server-config`) and flat Runtime Broker instance loading reject them with an error that names the key, `server.project_storage` and `volumes:`.

Startup reports them and agent start refuses them:

- **Startup reports.** When the settings file, or the runtimes and profiles stored in the Hub, still carry one of these keys on `runtimes.<name>` or `profiles.<name>`, the Hub, the Runtime Broker and a flat host log an error at startup that names each key, where it was found and the migration below. The process keeps running. The Hub's `storage_overrides` health check reports `degraded`, a Runtime Broker's `/healthz` reports `storage_overrides`, and `GET /api/v1/admin/server-config` lists the messages in `storage_override_errors`. A fix to the Hub's stored settings clears the Hub's entry when it is saved; the Hub reads the settings file at startup, so a fix there clears its entry when the Hub restarts. A Runtime Broker re-reads its settings within 30 seconds.
- **Agent start refuses.** The Runtime Broker refuses to start an agent whose profile, or that profile's runtime entry, carries one of these keys. The refusal comes before anything is created, with `422` and error code `storage_migration_required`. Agents of other profiles start normally. An agent whose shared-directory backend record from an earlier version (`shared-dir-storage.json`, or the `.retired` copy of it) pins shared directories to `nfs` is refused the same way while the shared-directory layout is not shared. The record is kept. If that data was copied elsewhere or is deliberately left behind, removing the record lets the agent start on the current layout.

To migrate a `shared_dir_storage_backend: nfs` override, edit the settings and remove the key:

- set `server.shared_dir_storage.backend: nfs` (transitional: shared directories of every runtime then use that export, and workspaces stay where they are), or
- write `server.project_storage` with `backend: nfs` (this also moves workspaces onto the export).

Check `shares[0].pv_name` and `subpath_root` against the live export. Kubernetes agents mounted that claim at `<subpath_root>/<project id>/shared-dirs/<name>`, so these two decide where the existing shared-directory data is. Keep `shares[0].server` and `export` accurate as well. For a `home_storage_backend: nfs` override, set `server.home_storage.backend: nfs` (or `homes: nfs` in `server.project_storage`). Then run `scion config validate`. Stored runtimes and profiles are edited in the Hub admin settings; a save drops the removed keys.

Agents no longer record their own shared-directory backend, and the operation that changed an agent's recorded shared-directory backend is removed. For a node-local directory, use a `volumes:` entry instead.

#### Layout identity check

Every Runtime Broker reports its storage layout in its heartbeat descriptor: the backend, for `nfs` the share server, export, `subpath_root` and the export identity marker, and `homes`. When the Hub's own backend is shared (not `local`), it compares a broker's report with its own layout:

- **Dispatch refusal**: the Hub refuses to dispatch an agent of a hub-native project in the shared-plain or worktree-per-agent sharing mode to a broker whose layout disagrees, with `409` and a message listing each difference (for example `nfs export: hub "/scion-projects", broker "/other"`) and asking for the same `server.project_storage` on the broker. A broker that reports no layout (an older version) is refused the same way; upgrade it. Linked projects, which the broker provides from its own path, and per-agent workspace modes are not checked.
- **Health**: the admin health summary shows the differences as `layout_mismatch` in the broker's storage entry and counts the broker as a problem.

### Workspace Storage (`server.workspace_storage`)

:::caution[Deprecated]
`server.workspace_storage` is deprecated and will be removed in a later release. Write [`server.project_storage`](#project-storage-serverproject_storage) instead: its `backend`, `nfs`, `cloudrun_volume` and `gke_shared_volume` fields are the fields below, and they place shared directories as well as workspaces. Until you write it, this block is [synthesized](#migrating-from-the-deprecated-blocks) into it; setting both refuses startup. The field descriptions and the operational notes in this section apply to both blocks.
:::

Configures the backend and mount settings for storing and managing agent workspaces. This is a critical setting for high-availability deployments where multiple Hub and Broker replicas need shared, durable access to project workspaces.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `backend` | string | `"local"` | Storage backend pivot: `"local"` (node-local directories), `"nfs"` (Network File System mounts), `"cloudrun-volume"` (Cloud Run platform-managed volume mounts), or `"gke-shared-volume"` (GKE shared CSI-backed PVC mounts). Names are case-sensitive; any other value stops the Hub at startup. |
| `nfs.mount_root` | string | | The host base directory under which NFS exports are mounted. |
| `nfs.mount_options` | string | `"vers=3,hard,nconnect=4,_netdev"` | Standard mount options passed to the `mount.nfs` utility. |
| `nfs.auto_mount` | boolean | `false` | Whether the Runtime Broker mounts the shares itself. See [NFS Mounts on the Runtime Broker](#nfs-mounts-on-the-runtime-broker). Requires the broker to run as root. |
| `nfs.uid` | integer | `1000` | Node-independent owner UID for NFS-backed workspace trees to ensure consistent container write permissions. Ignored by the Kubernetes runtime, where agents always run as uid `1000`; when it is set to another value, the broker logs a warning the first time an agent starts on Kubernetes with that value in each broker process, so it appears again after a broker restart. Must be between 0 and 4294967294; 0 or unset means `1000`. |
| `nfs.gid` | integer | `1000` | Node-independent owner GID for NFS-backed workspace trees. Must be between 0 and 4294967294; 0 or unset means `1000`. |
| `nfs.storage_class` | string | | The Kubernetes StorageClass name used to dynamically allocate volumes on GKE. |
| `nfs.subpath_root` | string | `"projects"` | The base folder within the share for project workspaces. See [subpath_root](#subpath_root). |
| `nfs.shares` | list of objects | `[]` | List of NFS share objects. Each share requires: `id` (stable ID), `server` (IP address or hostname), `export` (exported path, e.g., `/scion-workspaces`), and optional `pv_name` (for GKE). |
| `cloudrun_volume.volume_name` | string | | **Required** when `backend` is `"cloudrun-volume"` (the settings schema checks this only for the selected backend). The name of the platform volume declared in the Cloud Run service specification. The Hub resolves workspaces under `/mnt/<volume_name>`, so the service must mount the volume at exactly that path; see the [Cloud Run volume example](/scion/hosted/ha/setup-gcp/#3d-deploy-the-hub-to-cloud-run) in the GCP HA setup guide. If it is missing or empty, the Hub refuses to start. |
| `cloudrun_volume.subpath_root` | string | `"projects"` | Sub-directory prefix within the Cloud Run volume. See [subpath_root](#subpath_root). |
| `gke_shared_volume.volume_name` | string | | **Required** when `backend` is `"gke-shared-volume"`; if it is missing or empty, the Hub refuses to start. The K8s volume name referencing the persistent volume claim (PVC). **The pod spec must mount that volume at `/mnt/<volume_name>`**: the Hub derives every workspace path from it, and a pod that mounts the PVC elsewhere fails readiness (`GET /readyz` returns `503`) rather than writing workspaces to ephemeral container storage. |
| `gke_shared_volume.pv_claim_name` | string | | The name of the GKE-managed PVC bound to the shared storage backend (e.g. Filestore). |
| `gke_shared_volume.subpath_root` | string | `"projects"` | Sub-directory prefix within the GKE volume. See [subpath_root](#subpath_root). |

:::note[Kubernetes runtime and `gke-shared-volume`]
The Hub keeps project files on a `gke-shared-volume` backend, but the `kubernetes` runtime does not mount it into agent pods yet: agent workspaces get an `emptyDir` volume and are lost when the Pod is deleted. The agent runtime logs a warning the first time an agent starts this way in each of its processes. Use `backend: nfs` for persistent agent workspaces on Kubernetes. See [Stopping, Starting and Restarting Agents](/scion/hosted/ha/kubernetes/#stopping-starting-and-restarting-agents) and [ptone/scion#1075](https://github.com/ptone/scion/issues/1075).
:::

:::note[Cloud Run runtime needs `nfs`]
The `cloudrun` runtime (Cloud Run Instances) only works with the `nfs` backend, backed by a Filestore share. It mounts the agent's workspace, home and staged files from that share, and has no other way to provide them. Set `backend` to `"nfs"`, put the Filestore share's `server` and `export` in `nfs.shares[0]`, and mount the export on the Runtime Broker under `nfs.mount_root`, because the broker provisions workspaces on the share. With any other backend, or none, agent start on the `cloudrun` runtime fails right away with an error that says this, before any instance is created. The runtime's own `cloudrun.nfs_server` and `cloudrun.nfs_export` settings are optional. If you set them, they must match `nfs.shares[0]`. This does not apply to the `cloudrun-sandbox` runtime.
:::

#### Startup validation

The Hub checks `workspace_storage` (or the backend fields of `project_storage`) when it starts, and refuses to start with an error naming the bad field when:

- `backend` is not one of the four names above;
- `backend` is `"nfs"` and `nfs.shares` is empty;
- `backend` is `"cloudrun-volume"` or `"gke-shared-volume"` and the matching `volume_name` is missing or empty;
- the selected backend's `subpath_root` is invalid (see below).

A Runtime Broker that runs without the Hub does not refuse to start. It only logs warnings:

- If the `nfs` block is incomplete, the broker warns and skips its NFS mount checks (see [NFS Mounts on the Runtime Broker](#nfs-mounts-on-the-runtime-broker)).
- If the selected backend's `subpath_root` is invalid, the broker warns at startup. Its NFS mount checks still run, because they do not use `subpath_root`. However, every agent start that uses that backend fails with a `subpath_root` error until the value is fixed. This includes values such as `projects/` or `./projects`, which earlier versions accepted and normalized.

The Hub's readiness check (`GET /readyz`) and its Cloud Run write guard still apply as further safeguards. A volume backend without a mount point fails readiness, and blocks workspace writes on Cloud Run.

#### subpath_root

`subpath_root` is the directory inside the share or volume that holds project trees, at `<subpath_root>/<project-id>/...`. It defaults to `"projects"` for every backend, and for the deprecated `shared_dir_storage.nfs`. It must be a clean relative path: no leading `/`, no `.` or `..` components, no empty components and no trailing `/`. If a value is not clean, the error names the clean value to use instead (for example `projects` for `projects/`). It may have several components, for example `team/projects`. The Cloud Run runtime's NFS export paths use the same `nfs.subpath_root`.

#### NFS Mounts on the Runtime Broker

When `backend` is `nfs`, the Runtime Broker reads this block from its global `settings.yaml` (never from project settings) and checks each share at `<mount_root>/<share id>`. The block must have an absolute `mount_root`, and every share needs a unique single-segment `id`, a `server` (a hostname or IPv4 address, without whitespace or a leading `-`; IPv6 literals are not supported), and an absolute `export`. If the block is incomplete, the broker logs a warning and skips NFS handling. With any other backend, the broker does no NFS handling at all.

- **`auto_mount: false` (default)**: The Runtime Broker only checks. It reads the host mount table (`/proc/mounts`) to confirm each share is mounted from the expected `server:export`, at startup and then every minute. It never mounts or unmounts anything and never refuses agent creation on the `/proc/mounts` check alone. Mount each export on the host yourself, for example with `/etc/fstab`.
- **`auto_mount: true`**: The Runtime Broker mounts missing shares in the background, and remounts a share that is mounted from the wrong source. The Runtime Broker must run as root: if it does not, it logs a warning at startup and only checks, without running `mount`. If the Runtime Broker's default runtime is Kubernetes or Cloud Run, it also only checks, because the platform mounts the export into the agent. Each mount command times out after 90 seconds, or sooner if the agent-create request ends.

With `auto_mount: true`, agent creation for a project on the `nfs` backend is decided by the runtime the agent is dispatched to. This check runs before anything is mounted. Only agent creation is checked by the mount gate; starting or restarting an existing agent is not.

- **Docker, Podman, or Apple**: The Runtime Broker first makes sure the first share is mounted, because that share holds the workspaces. If it is not mounted, the Runtime Broker returns `503` with error code `nfs_unavailable`.
- **Kubernetes or Cloud Run (verify-only)**: The Runtime Broker never mounts and does not refuse the request on the `/proc/mounts` `server:export` string match (which is advisory and logged as a warning when the container's mount source differs in form, such as hostname vs IP or a trailing slash). Instead, on a shared `project_storage` layout, dispatch verifies that `<mount_root>/<share id>/<subpath_root>` is a visible directory (`503 project_storage_unavailable` when absent) and that `<ProjectRoot>/.scion/project-id` matches the dispatched project (`409` on mismatch).

In both modes, NFS problems are logged and reported per share in the `nfs_mounts` check of `GET /healthz`. The overall status becomes `degraded` only if all of these hold: `auto_mount` is on, the default runtime is not Kubernetes or Cloud Run, the first check has finished, and a share is unhealthy. NFS problems never affect `GET /readyz`, and the Runtime Broker keeps serving projects that do not use NFS. The Runtime Broker also sends its health to the Hub on every heartbeat, and the admin Health page shows it in the **Health** column of the **Runtime brokers** card. That copy holds only `healthy` or `unhealthy` for `nfs_mounts`. To see which share is failing and the mount error, query `GET /healthz` on the Runtime Broker itself. `scion doctor` runs the same check locally, using the same mount-table lookup. It reports whether NFS is configured, whether each share is mounted from the expected source, and whether each server is reachable on TCP port 2049. A share that is not mounted is a warning when `auto_mount` is off, the share has a `pv_name`, or the default runtime is Kubernetes or Cloud Run (so the Runtime Broker does not mount it, as in `/healthz`), and a failure otherwise. When the Hub or Runtime Broker runs as root and creates a new workspace or shared-directory leaf directory on the shared export, it `fchown`s that newly created leaf to `nfs.uid` and `nfs.gid` before applying `2775` and the default ACL, while keeping intermediate directories, `<ProjectRoot>/.scion`, and `<ProjectRoot>/provision` owned by the creator.

#### NFS Workspaces on Kubernetes

With the `nfs` backend and a bound PV claim (`nfs.shares[].pv_name`), each Kubernetes agent pod gets a `workspace-provision` init container. It runs for both git and non-git agents. It creates the per-project subPath (or, if another pod is already provisioning it, waits for that pod to finish) and chowns it to the agent runtime uid (`1000`) and `nfs.gid` so the agent can write `/workspace`; `nfs.uid` is ignored on Kubernetes (see the `nfs.uid` row above). For git agents, it also clones the repository. The init container runs as root with only the `CHOWN`, `FOWNER`, and `DAC_OVERRIDE` capabilities and does not follow symlinks. If the chown fails, the agent start fails and the error names the failed init container, so the agent never runs with an unwritable workspace.

Pods get `fsGroup` from `nfs.gid` (default `1000`). For shared directories on the NFS export, Scion also adds each shared directory's own group to the pod's supplementary groups (see [Agent groups](#agent-groups-on-nfs-shared-directories)), so `nfs.gid` does not need to match it.

#### Cleanup on project delete

With the `nfs` backend, deleting a project also removes its `<subpath_root>/<project-id>` tree (workspace and shared directories) from the export. The Hub removes it in the background from its own mount of the export, and provider Runtime Brokers remove it as part of their project cleanup. If the project exists again by the time cleanup runs, the tree is kept.

#### Ephemeral Storage & 503 Safety Gate

To protect deployments from silent data loss, the Hub implements a strict **503 Safety Gate**:
* If the Hub is deployed on serverless environments like Google Cloud Run with the `local` backend selected, its local workspace paths map to ephemeral, non-durable container storage.
* The Hub detects this non-durable state and refuses the project workspace file endpoints that change files (upload, inline file editing, and delete) and the shared-workspace `git pull` endpoint.
* These endpoints return `503 Service Unavailable` with error code `workspace_writes_unavailable` and a descriptive message rather than allowing writes to persist ephemerally on the container's scratch space, enforcing the transition to a durable backend (`nfs`, `cloudrun-volume`, or `gke-shared-volume`) for production.
* Shared-directory writes follow the same rule: on a Cloud Run Hub whose shared directories are not on a shared backend, they return `503` with error code `shared_dir_writes_unavailable`, and the message names `server.project_storage`.

### Shared Directory Storage (`server.shared_dir_storage`)

:::caution[Deprecated]
`server.shared_dir_storage` is deprecated and will be removed in a later release. Shared directories now follow [`server.project_storage.backend`](#project-storage-serverproject_storage), with the workspace. Until you write that block, this one is [synthesized](#migrating-from-the-deprecated-blocks) into it. Setting both refuses startup. The behaviour below for the `nfs` backend still applies to shared directories on a `server.project_storage` `nfs` backend.
:::

Selects where project [shared directories](/scion/local/workspace/#5-project-shared-directories) are stored. With the `nfs` backend, every Runtime Broker resolves a project's shared directories to the same path on one NFS export, so agents on different brokers — Docker or Kubernetes — see the same files.

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

- **Symlink-safe access**: Every Hub operation on an NFS shared directory goes through the same confined resolver. This covers the web file browser, archive downloads, and shared-dir deletion. The resolver walks each path component with `O_NOFOLLOW`, anchored on the inode of the project's tree, and refuses any symlink in the path. A missing or incomplete `nfs` block, or an unusable host base directory, fails closed on the Hub as well as on agent start.
- **Leaf modes and ACLs**: A newly created shared directory gets mode `2775` (setgid, group-writable) and a minimal default POSIX ACL, so files agents create inside it inherit group write access regardless of umask. If the export does not support POSIX ACLs, a warning is logged once and the directory stays plain `2775` with no ACL. Files created inside such a directory follow each writer's umask (usually `022`), so they are not group-writable. Directories that already existed are never modified. See the [hybrid tier guide](https://github.com/GoogleCloudPlatform/scion/blob/main/docs/deploy/hybrid-tier.md) for the manual fix-up recipe.
- **Chat attachments**: Attachments are delivered into each recipient agent's inbox (`~/.scion/inbox/<message-id>/`) when `hub.attachment_artifacts` is on. It is on by default wherever the artifact service is ready, including the workstation server and other Hubs with local storage (no object store is needed), and on Hubs whose resource storage is an object store; a stored override still applies. For a message in a project topic thread, whose readers are the whole project, the Hub also copies each attachment into the project's `scratchpad` shared directory at `.attachments/_webchat/<id>/<name>`, and the message lists that inbox path first. It resolves the directory the same way as the file browser, so on NFS the copy lands where agents mount the scratchpad, and every write goes through the same confined resolver. The message tags the copy's container path with its shared directory (`attachment_mounts` in the delivered envelope). Attachments in direct conversations and participant-list group conversations are delivered only into the recipients' inboxes, with no shared copy, on every storage backend. An attachment that is not stored as an artifact (for example while `hub.attachment_artifacts` is turned off) has no inbox delivery. On shared storage (NFS) its scratchpad copy is likewise made only for project topic threads; a direct or group conversation's attachment is not copied there. On the workstation server (local storage) the copy is how an agent reads the file, so it is made for every message, direct conversations included. The other agents of the project can then read a direct message's attachment. This is a known limitation of that configuration, and it goes away once the Hub stores attachments as artifacts. Files an agent attaches with `scion message --attach` are read from the same directory. Scratchpad copies older than `messaging.attachment_inbox_retention_days` (default 7) are removed by the Hub's hourly maintenance pass, on every backend.
- **Ownership on an export that does not squash ids**: the broker creates the project chain as its own user and never changes ownership. Upper directories get `2755` and the leaf `2775`, and each inherits the group of a setgid parent. Pods create nothing on this export; they mount the existing leaf by `subPath`. When agents with different uids share a directory, for example Docker agents and Kubernetes pods, give the share directory (`<mount_root>/<share id>`) a shared group with the setgid bit (for example `chgrp <gid>` and `chmod 2775`) so every leaf inherits it. Scion then adds that group to each agent that mounts the leaf; see [Agent groups](#agent-groups-on-nfs-shared-directories).
- **Cleanup on delete**: Deleting a project removes its `<subpath_root>/<project id>/shared-dirs` tree from the export. Removing a single shared directory declaration deletes that directory. Both are best-effort: failures are logged and never block or roll back the database change.
- **Startup summary**: At startup the server logs one `server.shared_dir_storage resolved layout: …` line, plus a warning if any ignored `nfs` fields are set.

#### Agent groups on NFS shared directories

Different kinds of agents can write to the same NFS shared directory (leaf): Docker or rootful Podman agents on brokers, and Kubernetes pods. They usually run with different uids, so each one can modify the others' files only through the leaf's group. For that to work:

- **New files must be group-writable.** Where the export supports POSIX ACLs, the leaf's default ACL makes new files group-writable whatever the writer's umask. Where it does not (for example NFSv4.1 exports, where Linux clients cannot use POSIX ACLs), Scion logs a warning once, and new files follow the writer's umask. Agents that were given NFS shared-directory groups (below) run with the group bits of their umask cleared (`022` becomes `002`; a stricter `077` becomes `007`), so files they create are group-writable without an ACL. Other writers, and agents on images without this support, use their own umask (usually `022`), and their files cannot be modified by the other kinds of agents.
- **Every writer must be in the leaf's group.** At each agent start, the broker reads the group of every NFS shared directory the agent mounts, from the leaf itself, and adds it to the agent:
  - Kubernetes: added to the pod's `supplementalGroups`. `fsGroup` is not changed, and a group equal to `fsGroup` is not repeated (the pod already holds it).
  - Docker and rootful Podman: added with `--group-add`. The agent image's `sciontool` must keep these groups when it switches from root to the agent user (for the harness, services, lifecycle hooks and commands run through the substrate exec endpoint); older images drop them and keep the previous behaviour.
  - Rootless Podman and Apple containers: not supported; the broker logs a warning and starts the agent without the group. Docker with `userns-remap` or a rootless `dockerd` also gets no effect from the group, because the leaf gid is not mapped into the container's user namespace; the broker does not detect this case.

  For safety, the broker skips a group (with a warning) when it is below `1000`, when it is an overflow id (`65534` or `4294967294`, which NFSv4 id mapping reports for unmapped groups), or when `nfs.gid` is set and does not match. If the group cannot be read, the agent starts without it. Agents without an NFS shared directory are unchanged. On an `all_squash` export the server maps every client to one identity, so the added group has no effect there.

With umask `002`, other files the agent creates later (for example in its home directory) are group-writable for its primary group too. OpenSSH refuses a group-writable ssh config or key file (such as `~/.ssh/config`). Under umask `002`, a file created without an explicit mode is group-writable; secrets and ssh tools create such files with mode `0600`.

Some files are not upgraded:

- Files created by a writer that passes an explicit restrictive mode (for example `open(..., 0644)`) stay non-group-writable; neither an ACL nor the umask can add permissions the creator did not request.
- Files copied or moved in with their modes preserved (for example `cp -p`, `rsync -a`, `tar -x` as the owner, or `mv` within the export) keep those modes.
- Leaves created outside Scion, or before Scion added the leaf ACL, keep their existing mode and ACL. Scion only sets modes and ACLs on leaves it creates. Fix them by hand (see the hybrid tier guide).

**Known limit, mixed writers**: when agents with different uids write to the same `nfs` directory and neither POSIX ACLs nor the cleared umask group bits apply, subdirectories and files they create follow each writer's umask, usually `022`, and one kind of agent cannot write into subdirectories the other created. Use the shared-group setup described above.

The `local` backend (or an unset `shared_dir_storage`) behaves as before.

```yaml
# Deprecated form; write server.project_storage instead
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

### Agent Home Storage (`server.home_storage`)

:::caution[Deprecated]
`server.home_storage` is deprecated and will be removed in a later release. Its `backend` is now [`server.project_storage.homes`](#project-storage-serverproject_storage) and its other fields are `server.project_storage.homes_options`, with unchanged meaning. Until you write that block, this one is [synthesized](#migrating-from-the-deprecated-blocks) into it. Setting both refuses startup. The behaviour below applies to `homes` and `homes_options`.
:::

Selects where the home directory of Kubernetes agents lives. With the default `local` value the home is inside the pod and is filled from the broker's copy at every start. With `nfs`, each agent's home is a directory on the NFS share of `server.project_storage` (which must have `backend: nfs`), kept across stops, restarts and pod replacements.

The `nfs` value is in development. It takes effect only when the hub's `hub.k8s_nfs_home` [experiment](/scion/reference/experiments/) is on and `allow_incomplete_phases` is set. The export's group must be the pod group (gid 1000). See [Persistent Agent Home](/scion/hosted/ha/kubernetes/#persistent-agent-home-nfs) for how the home is created and used.

| `server.project_storage` field | Former `server.home_storage` field | Type | Default | Description |
|-------|------|------|---------|-------------|
| `homes` | `backend` | string | `local` | `local` or `nfs`. `nfs` needs `backend: nfs`. |
| `homes_options.leaf` | `leaf` | string | `pod` | How an agent's home directory is created on the export: `pod` (an init container in the agent's pod) or `broker` (the broker, through its own mount of the export at `nfs.mount_root`). |
| `homes_options.stop_grace_seconds` | `stop_grace_seconds` | int | `30` | Termination grace period of pods with an NFS home. |
| `homes_options.termination_wait_seconds` | `termination_wait_seconds` | int | `15` | How long a start waits, beyond the grace period, for the agent's previous pod to stop. Keep `stop_grace_seconds` plus this below 90 (the hub's start window; the broker request timeout is 120); the server warns at startup and `scion config validate` warns when it is not. |
| `homes_options.skeleton_max_bytes` | `skeleton_max_bytes` | int | `268435456` | Largest image home skeleton copied into a new home. |
| `homes_options.allow_incomplete_phases` | `allow_incomplete_phases` | bool | `false` | Development only. Allows `nfs` homes while the feature is incomplete. |

```yaml
server:
  project_storage:
    backend: nfs
    homes: nfs
    nfs:
      mount_root: /mnt/scion-nfs
      shares:
        - id: shared
          server: 10.0.0.2
          export: /scion-projects
          pv_name: scion-projects-pvc
    homes_options:
      leaf: pod
      allow_incomplete_phases: true
```

- **One choice for the deployment**: homes are placed by `server.project_storage.homes` alone. The former per-runtime, per-profile and per-instance `home_storage_backend` and `home_storage_leaf` keys are [removed](#removed-storage-overrides).
- **Kubernetes only**: agents on any other runtime always get a local home, whatever the settings say.
- **Share**: the home uses the first share and claim of the `server.project_storage` `nfs` block.
- **Hub agents only**: an NFS home is named after the agent's hub ID, at `<subpath_root>/<project id>/agents/<agent slug>/home-<agent id>` on the export. A start without a hub agent ID fails.
- **Read from global settings**: like the rest of `server.project_storage`, `homes` and `homes_options` are read from the global `settings.yaml` only, and a change needs a restart.
- **Startup summary**: the startup log has a warning for each invalid value.

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

Controls how the Hub checks for and applies its own updates. The **deployment tier** sets the update mode: a `binary` Hub updates from release binaries (it checks the effective channel's latest installable release in the Hub's release catalog, read from the GitHub Releases API), while a `source` Hub rebuilds from its git checkout.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `deployment_tier` | string | `binary` if no repository path is configured, otherwise `source` | Update mode. `binary` updates from GitHub Releases; `source` updates from a git checkout. The single-node VM deploy script sets `binary`. |
| `release_channel` | string | Detected from the running version | Release channel to track: `nightly`, `preview`, or `stable`. When empty, the channel is derived from the version string (`v0.5.0` → `stable`, `v0.5.0-preview.1` or `v0.5.0-rc1` → `preview`, `nightly-*` → `nightly`). Development builds have no channel and skip scheduled checks. A Hub configured for another channel than the one it runs is offered that channel's latest release once it was published after the running one (for example, a nightly Hub set to `preview`). |
| `update_policy` | string | `notify` for `binary`, `disabled` for `source` | `notify` checks on a schedule and shows an update-available banner in the admin UI, where an admin applies or dismisses it. `auto` checks on a schedule and installs the channel's latest release automatically, through the same installer as the admin UI. `disabled` turns off scheduled checks; manual checks from the admin UI still work. |
| `check_interval_hours` | integer | `6` | How often the scheduled release check runs. Minimum `1`. Each run is jittered by up to ±30 minutes. |
| `github_repo` | string | `GoogleCloudPlatform/scion` | GitHub repository whose releases the Hub lists and installs. |
| `install_root` | string | `/opt/scion` | Versioned install root (`releases/<tag>/scion`, `current`, `previous`) used by release self-update. `SCION_MAINTENANCE_INSTALL_ROOT` overrides it. The single-node VM deploy script writes it. |

Scheduled checks run whenever `update_policy` is not `disabled`; a Hub in `source` mode skips them. Before `update_policy` defaulted to `notify`, a `binary` Hub defaulted to `auto`; set `auto` explicitly to keep unattended updates.

**Release selection.** An update that an operator starts from the Maintenance page or the API (an explicit target release, channel and pin) records that choice once the Hub has restarted on the new release and verified it. Auto-updates (`update_policy: auto`) never record a selection, so they never change which channel the Hub follows. update-status reports where the **effective channel** came from in `channel.source`:

- `selection`: the recorded operator choice. It takes precedence over `release_channel`, so a later edit of `release_channel` has no effect until the operator picks a channel again on the Maintenance page.
- `config`: `release_channel` is set and no selection was recorded.
- `detected`: neither is set, so the channel is derived from the running version.

The pin (update-status `pin`) is `latest` when the operator picked the channel's latest release, or the release tag otherwise; without a selection it is `latest`. Under `auto`, a Hub pinned to a release is not updated automatically; it only shows the update-available banner.

**How a release is installed.** At startup the Hub detects an installer strategy for its host, shown on the Maintenance page:

- `symlink`: the running binary lives under `<install_root>/releases/` and `<install_root>/current` is a symlink owned by the Hub's user (the single-node VM layout). The Hub verifies the release, places it in `releases/<tag>/`, switches `current` atomically (the old release becomes `previous`), then exits so that systemd restarts it on the new binary. **Restart Hub** also restarts this way. Restart Hub is refused (`409`) while an update or rebuild is running, and holds back new ones until the Hub is back.
- `sudo-install`: a repository path is configured and the Hub user has the starter hub's exact sudoers rules. The verified binary is staged at `<repo>/scion.rebuild` and installed with `sudo -n install`, then the service is restarted with `sudo -n systemctl restart`. No other sudo rights are used.
- `unsupported`: anything else. The update and the automatic policy stop before anything is downloaded, and the Maintenance page shows the reason and how to fix it.

Every installed release is downloaded only from the configured repository's GitHub release, and checked against its `SHA256SUMS` and its own `version` output. The Hub records the update before it restarts and verifies, on the next start, that it came back on the target version. See [Binary layout and updates](https://github.com/GoogleCloudPlatform/scion/blob/main/docs/deploy/single-node-vm.md#binary-layout-and-updates) for the layout and for converting an existing VM.

:::caution[Behaviour change for single-node VMs]
Hubs with no repository path used to update through whatever `sudo` the `scion` user happened to have (for example a GCE `google-sudoers` grant). They now report `unsupported` until the VM is converted to the `/opt/scion` layout with `deploy.sh migrate-layout`.
:::

On the `preview` channel, a weekly workflow (Fridays at 14:00 UTC) publishes the next `vX.Y.Z-preview.N` build. It fast-forwards the newest `release/vX.Y` branch to the newest `nightly-YYYYMMDD` tag that the branch does not yet contain and whose `Build & Test` check succeeded, tags it, and builds the release, which updates `LATEST.json` and the `preview` branch. A week with no such nightly publishes no preview. If the release branch has commits that are not on `main` (for example, fixes cherry-picked during a freeze before stable), the weekly run fails instead of force-pushing. Maintainers then tag previews by hand until the line is promoted to `vX.Y.Z` on `stable` or the branch is realigned.

The related admin endpoints are `GET /api/v1/admin/maintenance/update-status` (running version, update mode, effective channel with its `channel.source` (`selection`, `config` or `detected`, see Release selection above) and `pin`, installer, and the pending and last update), `GET /api/v1/admin/maintenance/releases?refresh=0|1` (the release catalog per channel, each release classified against the running version; refresh is limited to one GitHub fetch a minute and the list is otherwise cached for 15 minutes), `POST /api/v1/admin/maintenance/check-updates` (manual check) and `GET` / `DELETE` on `/api/v1/admin/maintenance/update-available` (read or dismiss a pending update notification). All require the `hub.maintenance.execute` permission.

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
| `user_token_audiences` | list | `[]` | Peer hub issuer URLs for which this hub issues user access credentials to its own users (`POST /api/v1/auth/federation-token`, `scion hub auth federation-token`). Empty means none are issued. Experimental: also needs the `federation.project_visitors` experiment. Read at startup; a change needs a restart. |
| `user_token_lifetime` | duration | `"15m"` | Validity of those user access credentials; a whole number of seconds, at most `"1h"`. Read at startup. |

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
| `default_role` | string | `"viewer"` | Deprecated. Accepted for compatibility and ignored; the Admin > Federation page no longer shows it. Project access for federated users comes from visitor grants (`allow_visitor_grants`). |
| `allowed_emails` | list of strings | | Restrict user tokens to specific email claims (supports wildcards e.g. `*@example.com`). |
| `allowed_domains` | list of strings | | Google issuer: restrict **user** principals presenting an [external bearer token](/scion/hosted/single-node/auth/#external-bearer-tokens-google-credential-pass-through) to these email domains (exact, case-insensitive, no wildcards or subdomain matching). The Hub sign-in policy still applies. Never consulted for service accounts. Hub issuer with `accept_user_tokens`: restrict that hub's user access credentials to these email domains, with the same matching. Not allowed on any other issuer. |
| `accept_user_tokens` | boolean | `false` | `issuer_type: hub` only. Accept user access credentials (`scion_token_kind: user`) issued by this peer Hub. Such a credential must carry `email` with `email_verified: true` and no agent claims (`project_id`, `agent_name`, `ancestry`, `root_user`), and must pass `allowed_emails` and `allowed_domains`. Read on every request: a change applies to the next request. Requires the `federation.project_visitors` experiment; while it is off, these credentials are refused. Agent identity tokens from the issuer are not affected. |
| `allowed_gcp_projects` | list of strings | | Google issuer only. Admit **service-account** principals whose GCP project ID (parsed from the service account email) is listed. Empty admits no service accounts. Distinct from `allowed_projects`, which matches Scion project IDs. |
| `allow_visitor_grants` | bool | `false` | `issuer_type` `user` or `hub` only. Lets holders of `project.manage_visitors` (project owners and admins, hub admins) grant visitor access to users of this issuer, by email, while the `federation.project_visitors` experiment is on; granting also needs every permission of the `project-visitor` role on the project. The email must also pass `allowed_emails` and `allowed_domains`. A granted user's first request from this issuer with that email (present, and verified where the token carries `email_verified`) links the grant to the token's subject. When `false`, the Hub refuses visitor grants for the issuer, links no new visitor, and refuses the issuer's linked visitors on their next request (`403`). Their grants are kept, so setting it back to `true` restores access with no grant change. In the web UI it is the switch **Allow project owners to grant visitor access to users from this issuer** on **Admin > Federation**. |

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

Most server settings can be overridden via environment variables using the `SCION_SERVER_` prefix. Write each path segment in upper case and drop the underscores inside a multi-word field name: `read_timeout` becomes `READTIMEOUT`, not `READ_TIMEOUT`. A name that the Hub does not recognise is ignored and logged as a warning at startup, with a suggested spelling where one exists. An exported but empty `SCION_SERVER_*` or `SCION_SEED_*` variable is treated as unset, so it never overrides a configured value. Each key's working variable is listed as `x-env-var` in the [settings schema](https://github.com/GoogleCloudPlatform/scion/blob/main/pkg/config/schemas/settings-v1.schema.json).

There are two exceptions to the pattern:

- Every setting under `server.broker` uses the `RUNTIMEBROKER` segment, for example `server.broker.port` -> `SCION_SERVER_RUNTIMEBROKER_PORT` and `server.broker.broker_id` -> `SCION_SERVER_RUNTIMEBROKER_BROKERID`.
- `server.hub.public_url` is set by `SCION_SERVER_HUB_ENDPOINT`. `SCION_SERVER_HUB_PUBLICURL` and `SCION_SERVER_HUB_PUBLIC_URL` set nothing.

Other spellings of a key are accepted as aliases and set the same key: the spelling that keeps a field's underscores (`SCION_SERVER_HUB_ADMIN_EMAILS`), the broker names under the `BROKER` segment (`SCION_SERVER_BROKER_PORT`, `SCION_SERVER_BROKER_BROKER_ID`) and the flattened CORS names (`SCION_SERVER_HUB_CORSENABLED`). Prefer the names above. When a key's name and one of its aliases are both set, the name above wins; when only aliases are set, the alias that sorts first wins. At startup the server logs a warning for each alias that is set, naming the variable to use instead, and says so when another spelling of the same key overrides it. Aliases will be removed after the next release.

When it starts, the server checks every `SCION_SERVER_*` and `SCION_SEED_*` variable it reads for a boolean, integer or number setting. It does not check a `SCION_SEED_*` variable of a setting that seeding does not seed, or an alias that another spelling of the same key overrides, since nothing reads those values. List values, such as the port list `SCION_SERVER_HUB_CONDUIT_TCPALLOWEDPORTS`, are not checked at this step. A boolean takes `true`/`false`, `1`/`0` or `t`/`f`, so `SCION_SERVER_HUB_AUTOSUSPENDSTALLED=false` turns the setting off. A value that does not parse, for example `SCION_SERVER_HUB_AUTOSUSPENDSTALLED=maybe`, stops the server with an error that names the variable and its setting (never the value). `SCION_SERVER_ADMIN_MODE` is checked with the spellings listed under [Tracked Boolean Variables](#tracked-boolean-variables). An empty value is unset and is not checked.

:::caution[Upgrade note]
Many alias spellings, for example `SCION_SERVER_HUB_ADMIN_EMAILS`, `SCION_SERVER_BROKER_PORT`, `SCION_SERVER_HUB_CORS_ENABLED` and `SCION_SERVER_LOG_LEVEL`, used to be ignored by the Hub and reported at startup as matching no setting. They now take effect. Before upgrading, remove any such variable that you do not want applied.

The env also now overrides the settings that live at the top level of `settings.yaml` (`telemetry`, `default_timezone`, `default_gcp_identity_mode`, `default_gcp_identity_service_account_id`, `default_harness_config`, `quotas`, `agent_secrets`, `project_defaults`) and `server.project_storage` (and the deprecated `server.workspace_storage`) and `server.native_chat`, for example `SCION_SERVER_DEFAULTTIMEZONE`. A value that does not parse for one of these keys (for example `SCION_SERVER_QUOTAS_ENFORCEBROKERQUOTAS=maybe`) makes the server fail to load its configuration at startup, as it already did for the other `SCION_SERVER_*` keys.
:::

`server.log_format` and `server.env` have no environment variable. Neither is read by the Hub: both are accepted so existing settings files still load. The log output format is chosen at startup; set `SCION_LOG_GCP=true` for Cloud Logging JSON. `server.log_level` (or `SCION_SERVER_LOGLEVEL`) is applied when the server starts, and again when a file-mode admin server-config save or reload re-reads the config; clearing it reverts to `info`. `--debug` and `SCION_LOG_LEVEL` take precedence over it (see [Precedence](/scion/hosted/single-node/observability/#precedence)). Upgrade note: a Hub whose settings still contain `server.log_level: debug` from an earlier change now starts at `debug`, because earlier releases ignored the setting at startup.

`server.keyring_file` (`--keyring-file`, `SCION_SERVER_KEYRING_FILE`) is an experimental signing-key pilot. It is the path of a version-1 JSON keyring file holding versioned signing and encryption keys. Mount the file through your secret delivery mechanism; only the path belongs in settings. The hub and web server process reads it once at startup, and `--role relay` never reads it. Startup is refused when:

- the `server.secret_rotation` experiment is off;
- `--session-secret`, `SCION_SERVER_SESSION_SECRET` or `SESSION_SECRET` is also set;
- the file is invalid (unknown fields, duplicate key IDs, more than three entries per ring, inconsistent deadlines or key lengths);
- the file is writable or executable by others, or readable by others without `server.keyring_file_allow_world_readable`;
- `signing.emit` is `legacy` and the active signing key is not listed in `signing.legacy_read_ids` (it would issue values without a key ID that no reader accepts);
- `encryption.write_format` is `v1` and the active encryption key is not listed in `encryption.legacy_v1_read_ids`;
- the local cipher cannot satisfy the database encryption policy (when a policy row is present in the database, the Hub must run with `server.keyring_file`, write `enc:v2:` under an allowed writer key ID, and hold every required reader key ID; legacy-mode startup without a keyring file is refused).

In this release web session cookies, signed skill file download URLs, artifact view URLs, list pagination cursors and attachment delivery URLs use the keyring's signing ring. The active generation signs new cookies, and cookies signed by any generation in the file are accepted until that generation's `verify_until`. Download URLs, view URLs, cursors and delivery URLs are signed by the active generation. One that carries no key ID (the only form this release writes) is accepted when a generation listed in `signing.legacy_read_ids` signed it; one that carries a key ID is accepted only from that generation. Either way, acceptance ends at that generation's `verify_until`. An unknown key ID is refused. Once the active generation reaches its `sign_until`, the server stops issuing them: skill file URLs are handed out unsigned, so brokers cannot download local-storage skill files, new artifact views and share-link views answer `503`, a list page that needs a continuation cursor answers `500`, and attachment deliveries get no download URL. `/readyz` also reports `signing_issuance_closed`. A cursor has no lifetime of its own: it stops working when its generation reaches `verify_until`, and the client must restart the listing from the first page. The `local` secret backend and all Hub-stored infrastructure backups in the database (signing keys for agent and user tokens, skill downloads, artifact views, attachment delivery, and list cursors; the OIDC signing key and `oidc_signing_keyset`; and the Conduit grant key ring) share the Hub's versioned value cipher (`enc:v1:` or `enc:v2:<kid>:`), keyed from the keyring's `encryption` ring when `server.keyring_file` is set (or from the legacy key otherwise). At startup, before any key synchronization or backup write, a startup encryption audit scans and authenticates all locally stored secret payloads (skipping reference-only rows) and validates the decrypted structure of Hub-scoped infrastructure backups. Unreadable records are preserved and never overwritten: in keyring mode, any unreadable infrastructure backup keeps the Hub not ready (`/readyz` returns `503` with `reason: "backup_unreadable"`), while in legacy mode the Hub logs an `UNHEALTHY` error naming the affected record IDs and continues serving without overwriting them. The admin API never returns or accepts this setting.

The Hub's agent and user JWTs (access, refresh, CLI and test-login tokens) also use the keyring's signing ring. The active generation signs new JWTs (setting the JWT protected `kid` header to the active generation's key ID when `signing.emit` is `"versioned"` or `server.jwt.emit_version` is `"keyring"`, and omitting `kid` when `emit` is `"legacy"`). A JWT is accepted only by the generation its key ID names, or, for a JWT without a key ID, by a generation listed in `legacy_read_ids` (while `server.jwt.accept_legacy` is true), each until its `verify_until`. An unknown key ID never falls back to another key. Once the active generation reaches its `sign_until`, the server stops issuing JWTs. Setting `server.jwt.emit_version: "keyring"` refuses startup when `server.keyring_file` is not configured or no usable file keyring was loaded, when the signing ring has no active generation or no `legacy` entry, or when an explicit agent or user JWT signing key is set. With a keyring file the Hub does not sign or verify JWTs with any stored `agent_signing_key` or `user_signing_key` record, and neither rewrites, republishes nor deletes those records. The startup encryption audit described above still decrypts and checks them, so an unreadable one keeps the Hub not ready (`backup_unreadable`) even though no JWT uses it. If the file is missing, unreadable or incomplete, startup is refused rather than falling back to those records. The `legacy` entry is required, and its JWT keys equal the keys earlier releases derived from the same session secret, so tokens issued before the keyring file keep validating while `legacy` is a legacy reader.

File permissions: mount the keyring file with mode 0400 (0440 when a group must read it), for example Kubernetes `defaultMode: 0400`. A world-readable keyring file (such as the Kubernetes Secret volume default 0644, or 0444) refuses startup unless the operator sets `server.keyring_file_allow_world_readable` (`--keyring-file-allow-world-readable`, `SCION_SERVER_KEYRING_FILE_ALLOW_WORLD_READABLE`; default false), which loads it with a warning. A file writable or executable by others is always refused, and a group-readable file loads with a warning. The override is operator-only: the admin API never returns or accepts it.

**Examples:**
- `server.hub.port` -> `SCION_SERVER_HUB_PORT`
- `server.hub.gcp_project_id` -> `SCION_SERVER_HUB_GCPPROJECTID`
- `server.hub.gcp_iam_check_mode` -> `SCION_SERVER_HUB_GCPIAMCHECKMODE`
- `server.hub.gcp_iam_deny_unknown_policy` -> `SCION_SERVER_HUB_GCPIAMDENYUNKNOWNPOLICY`
- `server.hub.admin_emails` -> `SCION_SERVER_HUB_ADMINEMAILS`
- `server.hub.stalled_threshold` -> `SCION_SERVER_HUB_STALLEDTHRESHOLD`
- `server.hub.monitoring_dashboard_url` -> `SCION_SERVER_HUB_MONITORINGDASHBOARDURL`
- `server.hub.observability.metrics_enabled` -> `SCION_SERVER_HUB_OBSERVABILITY_METRICSENABLED`
- `server.hub.observability.tracing_enabled` -> `SCION_SERVER_HUB_OBSERVABILITY_TRACINGENABLED`
- `server.auth.user_access_mode` -> `SCION_SERVER_AUTH_USERACCESSMODE`
- `server.broker.enabled` -> `SCION_SERVER_RUNTIMEBROKER_ENABLED`
- `server.broker.container_hub_endpoint` -> `SCION_SERVER_RUNTIMEBROKER_CONTAINERHUBENDPOINT`
- `server.broker.state_dir` -> `SCION_SERVER_RUNTIMEBROKER_STATEDIR`
- `server.broker.broker_id` -> `SCION_SERVER_RUNTIMEBROKER_BROKERID`
- `server.database.url` -> `SCION_SERVER_DATABASE_URL`
- `server.database.allow_shadowed_schema` -> `SCION_SERVER_DATABASE_ALLOWSHADOWEDSCHEMA`
- `server.auth.dev_mode` -> `SCION_SERVER_AUTH_DEVMODE`
- `server.secrets.backend` -> `SCION_SERVER_SECRETS_BACKEND`
- `server.secrets.gcp_project_id` -> `SCION_SERVER_SECRETS_GCPPROJECTID`
- `server.secrets.gcp_credentials` -> `SCION_SERVER_SECRETS_GCPCREDENTIALS`
- `server.secrets.gcp_replication_locations` -> `SCION_SERVER_SECRETS_GCPREPLICATIONLOCATIONS`
- `server.scheduler.interval_seconds` -> `SCION_SERVER_SCHEDULER_INTERVALSECONDS`
- `server.scheduler.max_concurrency` -> `SCION_SERVER_SCHEDULER_MAXCONCURRENCY`

A value in `settings.yaml`, or in a `SCION_*` environment variable other than the `SCION_SERVER_*` and `SCION_SEED_*` variables the server checks at startup (see above), that does not parse for its key (for example a string where a number is expected) is ignored for that key only. The key falls back to its default, or stays unset, even when the bad value came from the environment and overrode a valid file value. A warning naming the key (never the value) is logged once. Every other key still applies, so one bad value no longer turns off unrelated Hub features. The server's own configuration is not covered: a value in the `server` section of `settings.yaml`, or a `SCION_SERVER_*` variable, that does not parse can make the server ignore the whole `server` section, at startup or when it reloads settings.

### Logging Environment Variables

These environment variables control server-side logging behavior. They are not part of the `settings.yaml` structure.

| Variable | Description | Default |
| :--- | :--- | :--- |
| `SCION_LOG_GCP` | Enable GCP Cloud Logging JSON format on stdout | `false` |
| `SCION_LOG_LEVEL` | Log level: `debug`, `info`, `warn` or `error`, optionally followed by per-component levels such as `info,hub.auth=debug`. See [Controlling the Log Level](/scion/hosted/single-node/observability/#controlling-the-log-level). | `info` |
| `SCION_DEBUG` | Deprecated alias for `SCION_LOG_LEVEL=debug`. Any non-empty value enables it, and a warning is printed to stderr once. `SCION_LOG_LEVEL` wins if both are set. Ignored (no warning) by `scion` commands in agent CLI mode; see [Debugging an agent](/scion/hosted/single-node/observability/#debugging-an-agent). | - |
| `SCION_CLOUD_LOGGING` | Send logs directly to Cloud Logging via client library | `false` |
| `SCION_CLOUD_LOGGING_LOG_ID` | Log name in Cloud Logging for application logs | `scion-server` |
| `SCION_GCP_PROJECT_ID` | GCP project ID for Cloud Logging (priority 1) | auto-detect |
| `GOOGLE_CLOUD_PROJECT` | GCP project ID for Cloud Logging (priority 2) | - |
| `SCION_SERVER_REQUEST_LOG_PATH` | Write HTTP request logs to a file at this path. Each line is a JSON object in `HttpRequest` format. When not set, request logs follow the default routing (stdout in background mode, suppressed in foreground mode, Cloud Logging when enabled). | (disabled) |

See the [Local Development Logging guide](/scion/contributing/logging/) for details on log formats, request log fields, and Cloud Logging integration.

### Shutdown and Signals

Once its startup has installed signal handling, `scion server start` (the Hub, Web and co-located broker process) shuts down gracefully on either `SIGINT` (Ctrl+C) or `SIGTERM` (`systemctl stop`, Kubernetes, Cloud Run, container stop): from the first signal, `/readyz` returns `503` (`status: not_ready`, `reason: shutting down`), new requests other than health checks and Runtime Brokers' own requests are refused with a retryable `503` (`Retry-After`), SSE streams end, the Conduit relay (or ingress proxy) drains, the co-located Runtime Broker and any flat Runtime Broker instances drain their in-flight launches while the in-process Hub still accepts their launch reports, and the Hub then closes its listener, drains in-flight HTTP requests, closes the audit writer, and flushes and closes its logs (see [`shutdown_grace`](#conduit-serverhubconduit) and **Shutdown** under [Log delivery guarantees](#log-delivery-guarantees) for the full order and budgets). A second `SIGINT` or `SIGTERM` forces the shutdown: the server stops waiting for the drain, closes the audit writer, flushes its logs within a short bound and exits with status 1. An orchestrator that escalates to `SIGKILL` after its grace period also terminates the process immediately. To stop the server cleanly, send `SIGINT` or `SIGTERM`, or use `scion server stop`.

- **Early startup.** A `SIGINT` or `SIGTERM` that arrives during early startup, before signal handling is installed, normally ends the process without a drain, as before. If the server installs its signal handling while such a signal is still being handled, it may instead run the graceful shutdown.
- **Foreground runtime broker** (`scion broker start --foreground`). When the broker owns its port record (the normal case), it saves the record and then installs its own `SIGTERM` handler. Once that handler is installed, a `SIGTERM` removes the port record; the process then either ends (if the server has not installed its signal handling yet) or runs the single graceful shutdown. The record removal and the start of the graceful shutdown run concurrently, with no order between them. A `SIGTERM` that arrives after the record is saved but before the broker's handler is installed ends the process by default and can leave the record behind. On a normal exit the broker also removes its record.
- **Other signals.** Signals the server does not handle keep their default behavior. For other termination signals, for example `SIGHUP` or `SIGQUIT`, that ends the process without a drain.

If the Hub fails to start (for example, when operational settings cannot be loaded), the server logs `Hub server failed to start: <error>`, flushes its logs, and exits with status 1.

### Log delivery guarantees

Server logging is best effort. Losses in the modes below are counted, never retried, and there is no exactly-once delivery: a record can be lost, and on some platforms it can appear twice (see **Duplicates** below). This section describes what the code guarantees; nothing here is a measured ingestion rate.

**Local acceptance is not remote ingestion.** A record counts as written when the local handler accepts it: the stdout write returned, or the Cloud Logging client put the entry in its in-memory buffer (8 MiB for application logs). The client sends buffered entries to Cloud Logging later, in the background. Nothing in the Hub confirms that an entry reached Cloud Logging, so no metric reports a delivery ratio for the Cloud path.

**Paths.**

- **stdout** (plain JSON, or Cloud Logging JSON with `SCION_LOG_GCP=true`): written synchronously by the logging call. On Cloud Run and GKE the platform ships stdout to Cloud Logging; on other hosts that depends on the host's log agent (for example the Ops Agent on Compute Engine). That shipping is outside the Hub.
- **Direct Cloud Logging** (`SCION_CLOUD_LOGGING=true`): the client library buffers and sends entries asynchronously. A circuit breaker probes the client by flushing it every 30 seconds; after 3 consecutive failed flushes it opens and the Hub stops feeding the Cloud path (stdout logging continues where a stdout handler is configured; see the `circuit_open` row). It probes again at the first 30-second check at least 60 seconds after opening (about 90 seconds with the defaults, and about every 90 seconds while probes keep failing) and closes when a probe succeeds. The request log (`scion_request_log`), message log (`scion-messages`) and artifact decision log (`scion-artifacts`) share the same client and circuit.
- **Decision-log audit records** (`scion.audit`, only when the `hub.authorization_decision_audit_v2` experiment is on): queued on a bounded asynchronous writer (2,048 records, 2 MiB) with one worker, a 2-second write budget per record and a 5-second drain at shutdown. The worker writes to the same handler chain as other logs, so the stdout and Cloud rules above apply after it.

**Loss modes.** Each loss mode below increments `scion.logging.write.failures{writer, reason}`. Losses outside these modes are not counted, for example: a process ended by a termination signal it does not handle (for example `SIGINT` or `SIGTERM` before `scion server start` installs its signal handling, or `SIGHUP` or `SIGQUIT` at any time; see **Shutdown**), a process killed by `SIGKILL` or the OOM killer, an unrecovered panic, a failed stdout write outside the audit writer, an OTel log export failure, and a request-log file write failure.

| `writer` | `reason` | Meaning |
| :--- | :--- | :--- |
| `audit` | `queue_full` | The writer's queue was full (record count or bytes); the new record was dropped. |
| `audit` | `oversize` | The record exceeded the per-record limits; it was rejected, not truncated. |
| `audit` | `unsupported` | The record held a value kind the writer does not copy; it was rejected. |
| `audit` | `closed` | The record arrived after the writer closed (shutdown). |
| `audit` | `error` | The inner handler returned an error or panicked. |
| `audit` | `timeout` | The write exceeded its 2-second budget. The record may still be written later; it is counted once, as a timeout. |
| `audit` | `shutdown` | The record was still queued when the 5-second shutdown drain ended. |
| `cloud` | `error` | The Cloud Logging client reported an error (a failed batch send, an invalid or oversized entry). One count per reported error, not per record. Under an error storm the client skips some reports, so this undercounts. |
| `cloud` | `queue_full` | The client's buffer was full and it dropped the entry. |
| `cloud` | `circuit_open` | The record was dropped from the Cloud path because the circuit breaker was open or probing. Application and message logs still go to stdout, except on Cloud Run, where the Hub leaves out their stdout handler (see **Duplicates**). The request log has no stdout copy while Cloud Logging is on; it keeps a file copy only if `SCION_SERVER_REQUEST_LOG_PATH` is set. Otherwise the dropped record is lost from Cloud Logging and stdout (an OTel log copy, when OTel logging is enabled, is unaffected). |
| `cloud` | `flush_error` | A periodic, probe or shutdown flush failed, timed out, or was skipped because an earlier flush was still running. A failed flush usually reports errors the client already counted as `error`, so the two can count the same incident. |

`scion.logging.write.records{writer="audit"}` counts records the audit writer's handler accepted. There is no `records` series for `writer="cloud"`, because a client buffer accept is not ingestion. Cloud Logging client errors are counted in `scion.logging.write.failures{writer=cloud,reason=error|queue_full}` and printed once to stderr as `logging client: <error>`. They are deliberately not sent through the structured logger (stdout JSON, OTel or Cloud Logging), to avoid feeding errors back into a failing Cloud Logging pipeline. Under an error storm the client drops some errors before the hook, so the count may be low.

**State signals.**

- `scion.logging.writer.stalled{writer}` is 1 while an audit write has been in flight longer than its budget. Records keep queuing, then drop as `queue_full`.
- `scion.logging.writer.circuit_open{writer="cloud"}` is 1 while the circuit breaker is open or probing.
- `scion.logging.queue.depth{writer}` and `scion.logging.write.late_returns{writer}` (writes that finished after being counted as timeouts; not failures).
- `/healthz` key `audit_log_writer`: `healthy`, `degraded: recent write failures` (a failure in the last 5 minutes), `degraded: writer stalled` or `degraded: writer closed`.
- `/healthz` key `cloud_logging`, present only when direct Cloud Logging is configured: `healthy`, `degraded: circuit open` or `degraded: recent write failures` (any `writer="cloud"` failure in the last 5 minutes).

Both health keys are non-critical: they can make `/healthz` report `degraded`, never `unhealthy`, and they do not affect `/readyz`. The metrics are exported only when Hub metrics export is on ([`server.hub.observability.metrics_enabled`](#observability-export-serverhubobservability), or `server.hub.gcp_project_id` while that switch is unset; metric group `SCION_METRICS_LOGGING`); the counters and health keys work without it. `deploy/monitoring/alert-policies.yaml` defines WARNING policies for write failures, a stalled writer and an open circuit. They take effect only when applied to Cloud Monitoring.

**Shutdown.** On a graceful shutdown (which signals trigger it, and when, is described in [Shutdown and Signals](#shutdown-and-signals)) the Hub drains its servers, closes the audit writer (up to 5 seconds), flushes and shuts down metrics and traces, then flushes the message and request logs, stops the circuit breaker, and flushes and closes the Cloud Logging client. Because the metric exporter has already shut down, failures counted during these final log flushes are never exported; a failed final flush of the main Cloud log still prints `error flushing Cloud Logging: ...` to stderr. A process that ends without a graceful shutdown skips these steps, for example: a `SIGINT` or `SIGTERM` during early startup, another termination signal the server does not handle (for example `SIGHUP` or `SIGQUIT`), or `SIGKILL`. Queued audit records and buffered Cloud Logging entries are then lost and not counted. A Hub startup failure (`Hub server failed to start`) returns an error instead of exiting immediately, so the deferred log flush runs before the process exits with status 1. Known third-party hazard (pre-existing, not observed): in the Cloud Logging client (`cloud.google.com/go/logging` v1.13.2), a record logged after the client is closed whose send then fails reports the error on a closed channel, which panics; whether this can happen during Hub shutdown has not been determined.

**Duplicates.** When direct Cloud Logging is on and the `K_SERVICE` environment variable is set (Cloud Run), the Hub leaves out the stdout handler for application logs and the message log, so it sends each of those records on one path only (the direct client) and does not itself create a duplicate. Without `K_SERVICE` (GKE, Compute Engine, other hosts) both paths stay on. If that platform also ships stdout to Cloud Logging, each record can appear twice, once per path. This follows from the code; it has not been observed on a deployment. To avoid it there, turn off one of the two paths: direct Cloud Logging, or the platform's stdout collection.

### Boolean Environment Variable Parsing (`parseBoolEnv`)

The server reads the variables listed under [Tracked Boolean Variables](#tracked-boolean-variables) directly, with the `parseBoolEnv` parser. These rules apply only to them; a `SCION_SERVER_*` or `SCION_SEED_*` variable for a boolean setting takes only `true`/`false`, `1`/`0` or `t`/`f`, and a value that does not parse stops the server at startup (see [Environment Variables](#environment-variables)).

- **Supported Truthy Values:** `true`, `1`, `t`, `yes`, `y`, `on` (case-insensitive, whitespace-trimmed).
- **Supported Falsy Values:** `false`, `0`, `f`, `no`, `n`, `off` (case-insensitive, whitespace-trimmed).
- **Safety Warnings:** Unset or empty values are `false`, except for `SCION_SERVER_ADMIN_MODE`: when it is unset or empty, the server keeps the admin mode from its settings (a value of only whitespace turns admin mode off). An **unrecognized non-empty value** (e.g., `SCION_LOG_GCP=trur`) triggers a **warning at startup** and is treated as `false`, except for `SCION_SERVER_ADMIN_MODE`, where it stops the server at startup.

#### Tracked Boolean Variables:

| Variable | Description | Default |
| :--- | :--- | :--- |
| `SCION_SERVER_ADMIN_MODE` | Forces the server into emergency maintenance mode (break-glass removal). An unrecognized non-empty value stops the server at startup instead of defaulting to `false`. | `false` |
| `SCION_TRACING_ENABLED` | Enables Hub trace export when `server.hub.gcp_project_id` is set and [`server.hub.observability.tracing_enabled`](#observability-export-serverhubobservability) is unset. Ignored when that switch is set. | `false` |
| `SCION_LOG_GCP` | Enables Google Cloud Logging JSON format on standard output. | `false` |
| `SCION_REQUIRE_STABLE_SIGNING_KEY` | Demands a persistent session/JWT signing key. When enabled, startup aborts (fail-closed) if no stable key/secret can be resolved in hosted mode, preventing JWT signature mismatches across replica restarts. | `false` |

### Hub Endpoint Resolution

The operator-configured Hub endpoint is resolved from flags, the environment and `settings.yaml`, in that order. When no operator setting pins it (`--base-url`, `SCION_SERVER_HUB_ENDPOINT` or `SCION_SERVER_BASE_URL`), a `server.hub.public_url` saved in the database through the admin API replaces it at startup; see [`server.hub.public_url`: environment first](/scion/reference/admin-settings/#serverhubpublic_url-environment-first). A `server.hub.public_url` in `settings.yaml` does not pin the endpoint.

On a server that runs the Hub, the operator-configured endpoint is resolved in this order:

1. The `--base-url` flag — the server's public base URL (also used for OAuth redirects).
2. `SCION_SERVER_HUB_ENDPOINT` — explicit Hub public URL.
3. `SCION_SERVER_BASE_URL` — the server's public base URL (also used for OAuth redirects).
4. `server.hub.public_url` in `settings.yaml`.
5. `hub.endpoint` (or `SCION_HUB_ENDPOINT`) — deprecated, see below.
6. **IAP Audience Derivation** (in Hosted HA mode with IAP authentication):
   - For **Cloud Run** IAP audiences (`/projects/<number>/locations/<region>/services/<service>`), Scion can auto-derive the Hub's URL using the legacy Cloud Run URL format (`https://<service>-<number>.<region>.run.app`). Newer Cloud Run services use a different URL format (`https://<service>-<hash>-<region>.a.run.app`) where the hash cannot be derived from the project number — for those services, set `SCION_SERVER_BASE_URL` explicitly instead of relying on auto-derivation.
     The derived URL is the IAP front end, which the Hub's own host does not serve. On a co-located broker (for example, the single-node VM deployment), agents dispatched to Docker therefore receive `http://scion-hub.internal:<hub listen port>` instead. That hostname is mapped to the Docker host gateway, so the agents reach the Hub directly while staying on bridge networking. Agents dispatched to Podman receive `http://host.containers.internal:<port>`, which Podman resolves itself. On rootless Podman 5.0 to 5.2 with pasta networking, that name can be missing or unreachable (fixed in Podman 5.3). A host-gateway mapping has the same gap there, so upgrade Podman if agents cannot reach the Hub. This applies whatever the broker's default runtime is, including Docker or Podman runtime profiles on a Kubernetes-default broker. Agents dispatched to Kubernetes runtime profiles, Cloud Run, or Apple container still receive the derived URL.
   - For **GKE/GCLB** backend-service IAP audiences (`/projects/<number>/global/backendServices/<id>`), a URL cannot be derived from the ID. If `SCION_SERVER_BASE_URL` (or other explicit URL settings) is not set, Scion will log a warning at startup and fall back to `localhost`, which is likely unreachable from dispatched agents.
7. Auto-computed `http://localhost:{port}` (last resort).

A server that runs only the Runtime Broker takes `server.hub.public_url` (or `SCION_SERVER_HUB_ENDPOINT`), then `hub.endpoint`; `--base-url` and `SCION_SERVER_BASE_URL` do not apply to it.

**Upgrade note:** earlier releases ranked `server.hub.public_url` from `settings.yaml` (and `SCION_SERVER_HUB_ENDPOINT`) above `--base-url` and `SCION_SERVER_BASE_URL`. A server that sets `--base-url` or `SCION_SERVER_BASE_URL` to a different URL than its `settings.yaml` value now uses the flag or environment value, and a server that sets `--base-url` to a different URL than its `SCION_SERVER_HUB_ENDPOINT` value now uses the flag value. The values derived from the Hub endpoint follow it, including the OIDC issuer default and the `cloudrun_invoker` audience default. Hosts set up with the starter-hub scripts (`gce-start-hub.sh`) always set `SCION_SERVER_BASE_URL` in the systemd unit, so on those hosts that value now applies even when `settings.yaml` sets a different `server.hub.public_url`. Remove the setting you do not want, or make the two agree.

The server reads its startup settings from the global settings directory (`~/.scion`) only: `hub.endpoint`, `active_profile` and profiles, the co-located broker's default runtime, the `image_registry` check, the broker identity and `auto_provide`, the message broker and plugins, notification channels, cloud logging and maintenance settings. The Hub's system runtime endpoints (`/api/v1/system/runtime` and the image pull) resolve the active profile and runtime from the global settings directory too. Starting the server from inside a project directory does not merge that project's settings into these, and `--global` makes no difference to them. The settings reads made while the server runs (the profiles a Runtime Broker advertises and the Hub's reincarnate image settings) use the global settings directory as well.

Taking the server's endpoint from `hub.endpoint` (or `SCION_HUB_ENDPOINT`) is deprecated: the server logs a warning at startup, `scion doctor` reports it, and the fallback is removed in the next release. Set `server.hub.public_url` or `server.broker.hub_endpoint` instead. A workstation whose `hub.endpoint` is its own loopback URL is not affected.

For local development where the Hub runs on `localhost` but agents are in containers, set `server.broker.container_hub_endpoint` to a container-accessible address like `http://host.containers.internal:8080`.

The endpoint resolved above (or `server.hub.agent_endpoint`, if set — see below) is what the co-located broker then forwards to the container, applying its own bridging rules (host-gateway mapping for a hostname on co-located Docker, no rewrite for an already-reachable IP, and the `cloudrun-sandbox` runtime's own link-local logic).

#### The hub a Runtime Broker reports to

A Runtime Broker sends its heartbeat and control channel to `server.broker.hub_endpoint` (`SCION_SERVER_RUNTIMEBROKER_HUBENDPOINT`). On a combo server whose Runtime Broker runs next to the Hub, the Runtime Broker uses the Hub's localhost address when that setting is unset. A server that runs only a Runtime Broker falls back to the CLI's `hub.endpoint` (deprecated, see below). `scion runtime-broker register` and `join` save `server.broker.hub_endpoint` when it is unset, except on a host that runs a local workstation Hub, and never write `hub.endpoint`.

#### Deprecated fallback to the CLI's hub endpoint

`hub.endpoint` and `SCION_HUB_ENDPOINT` are CLI settings. The CLI now keeps its Hubs in a per-user hub list (see [Working with Several Hubs](/scion/hosted/user/multiple-hubs/)), and the server's own endpoints belong in `server.hub.public_url` and `server.broker.hub_endpoint`. The CLI takes its Hub from the root `--hub` flag, then `SCION_HUB_ENDPOINT` (then `SCION_HUB_URL`), then the Hub the current project directory is linked to, then the current Hub (`scion use`); it reads no `hub.endpoint` settings key. When the server's endpoint actually comes from the legacy fallback, the server logs this warning once at startup:

```text
deprecated: the server's hub endpoint came from hub.endpoint/SCION_HUB_ENDPOINT; set server.hub.public_url (SCION_SERVER_HUB_ENDPOINT) or server.broker.hub_endpoint (SCION_SERVER_RUNTIMEBROKER_HUBENDPOINT).
```

There is no warning when removing the fallback would not change the result, for example on a workstation whose `hub.endpoint` is its own loopback address. On a host configured as a server, `scion doctor` reports the same text as its `server-hub-endpoint` check; it cannot see the server's flags, so it warns only when every way the server could run would be affected. Selecting a Hub with `scion use` never changes the endpoint a server advertises.

#### Splitting the agent endpoint from the public URL

`server.hub.agent_endpoint` overrides the endpoint above for agents only — invite links, chat-bridge links, the OIDC issuer default, and the `cloudrun_invoker` audience default keep reading `public_url`. Use it when agents must reach the Hub on an address that would be wrong for a human clicking a link, such as an internal VPC IP in a topology where a proxy fronts the Hub for users. When unset, agents receive the Hub's regular endpoint — `public_url`, or the endpoint the Hub resolves above when `public_url` is itself unset.

The value must be `scheme://host[:port]` only: `http` or `https`, an IP literal or a hostname of letters, digits, `_`, `-`, and `.` (Docker Compose-style service names such as `scion_hub` are accepted), and an optional port — no path, query, fragment, or credentials (a trailing `/` is stripped from an otherwise-bare URL). The Hub rejects anything else at startup with an error naming `server.hub.agent_endpoint`.

**Scope: this value is injected into agents on every runtime broker attached to this Hub, including remote brokers.** Only set it if every broker's agents can reach the address and it is this Hub on each of those networks — otherwise agents dispatched from a remote broker will send their Hub credentials to whatever answers at that address on their own network.

**Security:** with an `http://` value, agent bearer tokens, and the secrets and tokens agents fetch from the Hub, travel **unencrypted** on the network between agents and the Hub. Use `https://` unless that network is trusted and isolated (for example, a private VPC subnet with no untrusted tenants).

### Web Base URL

The web server's base URL is the base of the OAuth redirect URI (`<base URL>/auth/callback/<provider>`), decides whether the session cookie is marked `Secure`, and is sent to the browser client. It is resolved once at startup, from operator inputs only:

1. The `--base-url` flag.
2. `SCION_SERVER_BASE_URL`.
3. `SCION_SERVER_HUB_ENDPOINT`.
4. `server.hub.public_url` in the global `settings.yaml`.
5. In hosted mode with a Cloud Run IAP audience (`server.auth.proxy.iap.audience` in the global `settings.yaml`, or its `SCION_SERVER_AUTH_PROXY_IAP_AUDIENCE` environment override), the Cloud Run URL derived from it (see [IAP Audience Derivation](#hub-endpoint-resolution)). It is always `https`.
6. `http://localhost:{port}` (the default).

Note that `SCION_SERVER_BASE_URL` comes before `SCION_SERVER_HUB_ENDPOINT` here, the reverse of the Hub endpoint order above: the two inputs the web server always read keep their order, and the Hub endpoint values only fill in when neither is set. To give agents a different Hub URL than users, set `server.hub.agent_endpoint` (see [Splitting the agent endpoint from the public URL](#splitting-the-agent-endpoint-from-the-public-url)), not `SCION_SERVER_HUB_ENDPOINT`. On a server that runs the web frontend without the Hub, only `--base-url` and `SCION_SERVER_BASE_URL` apply: there `server.hub.public_url` and `SCION_SERVER_HUB_ENDPOINT` name the Hub the Runtime Broker reports to.

A trailing `/` is removed and the scheme is lower-cased. The base URL is never taken from the settings database or from a `server.hub.public_url` saved through the admin settings API, and not from `hub.endpoint` (`SCION_HUB_ENDPOINT`). The IAP-derived URL is built from the audience in the global settings file or its `SCION_SERVER_AUTH_PROXY_IAP_AUDIENCE` environment override: operator configuration, never the database. Changing it needs access to the server's flags, environment or global settings file, and a restart, so a write through the admin API cannot redirect OAuth logins or downgrade the session cookie. The server logs the URL and its source at startup, for example `Web base URL: https://scion.example.com (from server.hub.public_url in /home/scion/.scion/settings.yaml)`.

The OAuth providers check the redirect URI against the one registered for the OAuth client. Google requires an exact match, and GitHub requires the same host and port as the registered callback URL (a path below it is accepted). If the base URL changes, logins fail until the registered redirect URI is updated to match. Neither provider accepts a different host.

**Listener check:** on every listener, loopback included, the base URL must parse, use `http` or `https`, and have a host; otherwise the server does not start, and the error names the URL and where it came from. In addition, the server refuses to start when the web server would listen on an address that is not loopback and its base URL is not `https` (including the default `http://localhost:{port}`), because OAuth callbacks and the session cookie would then cross the network unencrypted. The check runs early in startup, before the database and the Hub are set up, right after the dev-auth bind check. Loopback means the listen host is literally `localhost`, an address in `127.0.0.0/8`, or `::1`. Names are never resolved, so a hostname that resolves to `127.0.0.1` (for example through `/etc/hosts`) does not count as loopback. The error names the base URL, where it came from, and the listen address. To fix it, set an `https` base URL (`--base-url`, `SCION_SERVER_BASE_URL`, `SCION_SERVER_HUB_ENDPOINT`, or `server.hub.public_url`) or listen on a loopback host (`--host 127.0.0.1`). A workstation server listens on `127.0.0.1` by default and is not affected.

:::caution[Upgrade note: a deliberate availability change]
Earlier releases started the web server with an `http` base URL on any listener, including the `http://localhost:{port}` default on `0.0.0.0`. This release refuses to start in that case, by design: a server whose web frontend listens on a non-loopback address must now have an `https` base URL. Check before upgrading:

- A hosted server that listens on `0.0.0.0` must set `--base-url`, `SCION_SERVER_BASE_URL`, `SCION_SERVER_HUB_ENDPOINT` or `server.hub.public_url` to its `https` URL. The Helm chart sets `SCION_SERVER_BASE_URL`, and the Cloud Run Terraform module sets `server.hub.public_url`.
- The starter-hub `gce-start-hub.sh --no-tls` (or `SKIP_TLS=true`) no longer deploys a Hub reached over `http://` on port 8080: it now requires `HUB_BASE_URL` to be the `https://` URL of an upstream TLS terminator and stops otherwise.
- The web base URL now comes from `SCION_SERVER_HUB_ENDPOINT` or `server.hub.public_url` only when `SCION_SERVER_BASE_URL` and `--base-url` are both unset. A server in that position (for example a Cloud Run deployment that sets only `server.hub.public_url`) now uses that URL for OAuth redirects and the session cookie instead of `http://localhost:{port}`, so its OAuth clients must list `<URL>/auth/callback/<provider>`.
- A Cloud Run IAP deployment (hosted mode, a Cloud Run `server.auth.proxy.iap.audience`) that sets none of those now starts with the `https` URL derived from the IAP audience as its web base URL, instead of `http://localhost:{port}`. Its OAuth clients must list `<derived URL>/auth/callback/<provider>`. A GKE/GCLB IAP audience gives no URL, so such a deployment must set its base URL explicitly.
- A malformed base URL (one that does not parse, is not `http` or `https`, or has no host) now refuses to start on every listener, a loopback listener included. Earlier releases started with it and produced broken redirect URIs.
- A base URL with a trailing `/` (for example `--base-url https://hub.example.com/`) used to produce a redirect URI with a double slash (`https://hub.example.com//auth/callback/<provider>`). The trailing slash is now removed, so an OAuth client registered with the double-slash URI must be updated.
:::

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
| Secrets/storage | `secrets.*`, `storage.*`, `project_storage.*`, and the deprecated `workspace_storage.*`, `shared_dir_storage.*`, `home_storage.*` |
| Identity/mode | `mode`, `env`, `hub.hub_id`, `hub.gcp_project_id` |
| Logging | `log_level`, `log_format` (`log_format` is accepted but ignored) |
| Server keyring | `keyring_file`, `keyring_file_allow_world_readable` (experimental; never returned or accepted by the admin API) |
| CORS | `hub.cors.*`, `broker.cors` |
| Messaging/plugins | `message_broker.*`, `plugins.*` |
| Async agent create | `hub.launch_timeout`, `hub.launch_keepalive_seconds` |
| Diagnostics | `hub.perf_trace` |
| Membership standing | `hub.membership_sweep_report_only` |
| Heartbeat reconcile | `hub.missing_agent_grace` |
| Conduit relay | `hub.conduit.*` |
| Separate relay | `relay.*` |
| Port proxy | `hub.port_proxy.*` |

### Layer 1 — Operational (`hub_settings` table)

Settings that can be changed at runtime and are shared across all replicas. Stored as section-per-row in the `hub_settings` table of the Hub database on every driver, SQLite included. `settings.yaml` only seeds them and is the fallback for a section with no database row (see the [Admin Settings Model](/scion/reference/admin-settings/)). The `maintenance` section is runtime/API-only: it has no `settings.yaml` representation and starts from `admin_mode` until a row exists.

| Section | Contents |
| :--- | :--- |
| `access` | `admin_emails`, `user_access_mode`, `authorized_domains`, `default_user_role` |
| `lifecycle` | `auto_suspend_stalled`, `stalled_threshold`, `soft_delete_retention`, `soft_delete_retain_files`, `start_claim_lease_ttl`, `start_max_duration`, `start_unconfirmed_hold`, `start_create_unconfirmed_hold` |
| `maintenance` | `admin_mode`, `maintenance_message` (durable + cluster-wide) |
| `telemetry` | Full `telemetry.*` subtree (enabled, cloud, hub, local, filter, resource) |
| `agent_defaults` | `default_template`, `default_harness_config`, `default_max_turns`, `default_max_model_calls`, `default_max_duration`, `default_resources`, `default_model`, `default_thinking_level`, `default_max_agent_role`, `default_agent_role`, `default_runtime_broker`, `default_timezone`, `default_gcp_identity_mode`, `default_gcp_identity_service_account_id` |
| `federation` | `enabled`, `trusted_issuers[]`, `algorithms`, `refresh_interval`, `debounce_interval` |
| `endpoints` | `hub.public_url`, `hub.hub_name`, `hub.monitoring_dashboard_url`, `image_registry` |
| `hub_observability` | `hub.observability.metrics_enabled`, `hub.observability.tracing_enabled` (restart required) |
| `github_app` | `app_id`, `api_base_url`, `webhooks_enabled`, `installation_url`, `private_key_path` |
| `notifications` | `notification_channels[]` |
| `project_defaults` | `default_scratchpad` |
| `auto_expose_ports` | `enabled` |
| `quotas` | `enforce_broker_quotas` |
| `agent_secrets` | `user_scope_only` |
| `agent_delegation_policy` | `remove` map per boundary kind (`project`, `hub`) listing permissions removed from what may be delegated to agents; it can only narrow (`hub.agent_delegation` experiment; runtime/DB-only; refused to user access tokens) |
| `messaging` | `conversation_envelope_switch`, `cross_project_messaging_enabled`, `offload_threshold_runes`, `attachment_delivery_max_bytes`, `attachment_inbox_retention_days` (managed via `/api/v1/admin/messaging`) |
| `runtimes` | The whole `runtimes` map |
| `profiles` | The whole `profiles` map |
| `harness_configs` | The whole `harness_configs` map |
| *(reserved)* `global_defaults` | Reserved for future hub-resource design — not implemented |

`agent_defaults.default_timezone` is the Hub default `TZ` for agent containers: an IANA zone name, used only when the agent has no pin and no `TZ` environment variable applies. Empty means no default (the image default, UTC). An invalid name or `Local` is rejected with `422`. In `settings.yaml`, and in the `PUT /api/v1/admin/server-config` request body, it is the top-level `default_timezone` field. It does not change how times are stored or displayed. See [Times and Timezones](/scion/reference/times-and-timezones/#hub-default-timezone).

A saved change to `agent_delegation_policy` can briefly not be in effect on the Hub that saved it: when that Hub cannot re-read the settings right after the write, it applies the change on its next settings refresh (the settings event or the 60-second poll), as the other replicas do.

### Precedence

On every driver, the effective value for any Layer-1 key is resolved in this order (highest priority first):

1. **`hub_settings` DB row** — cluster-shared, set via admin API. A row fully owns its section.
2. **Bootstrap merge** — used only when the section has no row: `SCION_SERVER_*`, then `settings.yaml`, then `SCION_SEED_*`, then compiled defaults. `settings.yaml` is the global `~/.scion/settings.yaml`; see [`--config` and the bootstrap merge](/scion/reference/admin-settings/#configuration-precedence) for how a `--config` location takes part.

A `SCION_SERVER_*` variable on a Layer-1 key therefore does not override a row an admin has saved. It only changes the seed (re-synced into seeded rows at restart) and the fallback, and it is deprecated for Layer-1 keys in favour of `SCION_SEED_*`.

### Seeding and Migration

- **Every start**: the replica that takes the seed advisory lock syncs `hub_settings` (Layer-1 sections only) from its bootstrap merge: `SCION_SEED_*`, `settings.yaml`, then `SCION_SERVER_*`. A missing section is created, a section no admin has edited (seeded) is re-synced when its content differs, and an edited (managed) section is not touched. A replica that finds the lock held skips the sync.
- **DB wins**: once a section is seeded/written to DB, the DB row fully owns that section. Omitted fields within the section fall to compiled defaults, not to the file.
- **Rollback safety**: older builds ignore the `hub_settings` table entirely and read files — rolling back reverts to pre-change behavior. This covers operational settings only: other stored data is rewritten one way, so a binary rollback after a newer build has started is unsupported. See [Hub Upgrade and Binary Rollback](/scion/reference/hub-upgrade-rollback/).

### Environment Override Warnings

Because env overrides on Layer-1 keys reintroduce per-node drift, the system warns administrators:

- `GET /api/v1/admin/server-config` includes an `env_overrides` array listing every key, Layer-0 or Layer-1, that a `SCION_SERVER_*` variable sets on the serving node. Keys that no settings write path accepts (for example the hub class, the test identity settings, the relay-peer secret and the server keyring file) are left out, so the response does not reveal whether they are set.
- The same response includes `env_values`, a map with exactly the `env_overrides` keys, each `{ "value": "********", "masked": true }` for a masked key, or `{ "value": ..., "in_effect": true|false, "saved_differs": true }` otherwise (`masked` and `saved_differs` are left out when false):
  - `value` is the environment value as the Hub parsed it (string, number, boolean or list).
  - `masked` is `true` when `value` is the placeholder `********`: the key is a secret (masked as in the rest of the response), it sits under `profiles`, `runtimes` or `harness_configs`, or its value cannot be shown safely.
  - `in_effect` is `true` when the Hub on the serving node runs with the environment value:
    - In `settings.yaml` mode the environment layer wins over the file, so it is `true`, except for the keys below that start-up sets afterwards.
    - On database-backed Hubs a Layer-1 key is in effect only when the environment value equals the value the Hub applies (a saved database value, or a seeded row, wins). Other Layer-0 keys are read at startup with the environment on top and are in effect, except for the keys below.
    - `scion server start` sets some keys after loading the settings, whatever the environment says: on a workstation Hub the hub host, the broker switch, dev auth, the storage provider and the secrets backend; and in any mode the keys of `--port`, `--host`, `--db`, `--enable-runtime-broker`, `--runtime-broker-port`, `--dev-auth`, `--storage-bucket` and `--storage-dir`. For these keys `in_effect` compares the environment value with the value start-up resolved. The `--conduit-*` flags are not compared (a known limitation): for their keys `in_effect` follows the settings precedence.
  - `saved_differs` is `true` when a stored value exists (in `settings.yaml`, or in an admin-saved database row for a Layer-1 key) and differs from the environment value. A default, a seeded row, or a stored value equal to the environment value is not reported.
  - A masked entry carries only `value` (`********`) and `masked`: neither `in_effect` nor `saved_differs` is computed or returned for it. Both compare the environment value with a stored or running value, so for a secret they would let a caller who can write settings confirm a guessed value.

  `env_values` is additive; `env_overrides` is unchanged.
- A startup `WARN` log lists any overridden Layer-1 keys.
- The admin UI renders a visible warning banner when env overrides are detected, and a badge on each affected field, or on the section for the `runtimes` and `profiles` maps: "Overridden by environment on this node" with the value in effect, "Environment value superseded by the saved setting" when a saved value wins, or "Environment value not in effect on this node" when a start-up default or flag wins.

### Admin API Behavior Notes

**PUT partitioning**: The request body is partitioned by the section registry. Layer-1 fields (including `runtimes`, `profiles`, and `harness_configs`) are written to DB sections in the `hub_settings` table as JSONB documents, while Layer-0 fields trigger a `422` rejection on hosted Hubs (on workstation Hubs they are edited in place in `settings.yaml`; see [Layer-0 edits by deployment mode](/scion/reference/admin-settings/#layer-0-edits-by-deployment-mode)). A key the Hub would not persist (an unknown key at any depth, a flat dotted key such as `"server.hub.auto_suspend_stalled"` at the top level, or a field with no storage) is also rejected: the response is `422` with `error: unpersisted_keys_rejected` and the offending paths in `keys`, and nothing is saved. Fields whose value equals what `GET` returns are treated as echoes and accepted, so sending the `GET` body back still succeeds. In both DB mode (every non-map section) and `settings.yaml` mode (`server`, `telemetry`, and `federation` sections), a partial `PUT` merges onto the current settings and keeps every omitted key, so the admin UI can send only the fields you changed. `runtimes` is merged per entry in both modes. The map you send decides which entries exist: an entry you leave out, or send as `null`, is deleted. Within an entry you send, a key you leave out keeps its stored value, a key sent as `null` (or as an empty value: `""`, `false`, `{}`, `[]`) is cleared, and any other key you send replaces the stored value. A stored key that is not a runtime setting, for example `runtimes.<name>.resources` written into `settings.yaml` by hand, is kept as stored and named in the response `warnings`; nothing reads it. A stored key that matches a runtime setting only when case is ignored (for example `Namespace`) is kept as stored and named in a warning that suggests the canonical spelling. A body that sends such a key is refused as an unknown key. `profiles` and `harness_configs` still replace the stored map whole. See [Resource quantities](#resource-quantities) for the checks on `default_resources` and profile resources. `default_thinking_level` must be between `1` and `100` (`0` or any other out-of-range integer is refused with `422 validation_error`; `null` clears it), and `{"federation": null}` is refused with `422 validation_error` (on a database-backed Hub, use `DELETE /api/v1/admin/server-config/sections/federation` to restore the bootstrap federation settings).

**Revision CAS**: The request body may include `expected_revisions` — a map of section name to expected revision number. On mismatch, the response is `409 Conflict` (`revision_conflict`) with the conflicting sections and their current revisions. In DB mode, a section the request sends uses the revision the Hub read while building it as an implicit CAS when `expected_revisions` omits it, so a concurrent change to that row between read and write returns `409` even without `expected_revisions`. This covers every merged section, the per-entry `runtimes` merge, and the whole-map `profiles` section, which replaces the stored map. Only `harness_configs` uses last-writer-wins semantics when omitted from `expected_revisions`. Sections are written in alphabetical order for deterministic partial-apply behavior, and live snapshot applies are ordered by a store-wide revision so an older snapshot never overwrites a newer one.

**Presence-aware clearing**: The PUT handler distinguishes **omitted** fields, which keep the current stored value, from fields sent as an empty value (`""`, `[]`) or JSON `null`, which **clear** the stored key. In DB mode, a cleared key is unset; `settings.yaml` and environment bootstrap values are not re-applied, except for the keys listed under [Hot reload vs restart](/scion/reference/admin-settings/#hot-reload-vs-restart). This allows clearing `admin_emails`, `user_access_mode`, `authorized_domains`, `default_user_role`, `notification_channels`, `public_url`, `monitoring_dashboard_url`, and other operational settings without sending every field. In `PUT /api/v1/admin/integrations/{name}/config`, `settings` values are `string | null`, and a key sent as `null` is deleted so the plugin default applies again.

**Masked secrets**: `GET /api/v1/admin/server-config` masks secrets (OAuth client secrets, GitHub App keys, notification channel parameters, and other credentials). A PUT may send a masked placeholder back only inside a block that exactly matches the stored block once masked; the Hub then keeps the stored secret. The block is the structure the secret sits in (for example one OAuth provider, the GitHub App, or one notification channel). To change any field of such a block, send every secret in that block in clear. Any other placeholder is rejected with `400`, so it is never stored over a real value. The admin web UI leaves unedited masked blocks out of its saves.

**Maintenance durability**: `PUT /api/v1/admin/maintenance` writes to the `maintenance` section in DB, making admin/maintenance mode durable across restarts and propagated to all replicas. `SCION_SERVER_ADMINMODE` env var still force-enables per node for break-glass access. In file/SQLite mode, maintenance changes are ephemeral (in-memory only, lost on restart). Use `SCION_SERVER_ADMINMODE=true` env var for persistent control.

**Schema endpoint**: `GET /api/v1/admin/server-config/schema` returns JSON-schema fragments and koanf key paths per section for UI form generation and CLI validation.

#### Resource quantities

`default_resources`, `profiles.<name>.resources` and `profiles.<name>.harness_overrides.<harness>.resources` take Kubernetes quantities: `cpu` in cores or millicores (`2`, `500m`), `memory` and `disk` in bytes with a unit (`512Mi`, `16Gi`, `20Gi`; `Ki`, `Mi`, `Gi` and `Ti` are powers of 1024, `k`, `M`, `G` and `T` powers of 1000). A memory or disk value without a unit is a byte count, so `memory: "16"` is 16 bytes, not 16 GiB. A pod with such a limit starts but fails: its in-memory volumes are sized from the memory limit, so writes fail with `ENOSPC`.

A save (`PUT /api/v1/admin/server-config`, both modes) checks every value it sends:

| Value | Result |
| :--- | :--- |
| Not a Kubernetes quantity, zero or negative | Refused (`422` in DB mode, `400` in `settings.yaml` mode) |
| `memory` or `disk` below `1Mi`, with or without a unit (catches `"16"`) | Refused |
| A `cpu` or `memory` request above its limit | Refused |
| `memory` or `disk` of `1Mi` or more without a unit (`"2147483648"`) | Saved, with a warning that suggests a unit (`2Gi`) |
| `memory` below `64Mi` | Saved, with a warning |

A refusal names the field, for example `profiles.gke.resources.limits.memory: "16" has no unit, so it means 16 bytes, below the minimum of 1Mi; use a quantity with a unit, such as 16Gi or 512Mi`; `details.errors` lists every refused value. Warnings come back in `warnings` on the `200` response. The admin UI applies the same rules as you type: an error blocks **Save & Reload**, a warning is shown under the field. Settings already stored are not refused at startup; `scion config validate` reports these values as warnings. A save re-checks every profile it sends, and the page sends every profile on each save, so a stored invalid value has to be fixed before the next save of the profiles goes through.

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

An unset value takes the default shown above. A value that is set but not recognised (for example a typo) takes the stricter value instead: `"enforce"` for `gcp_iam_check_mode` and `"fail-closed"` for `gcp_iam_deny_unknown_policy`, with a warning in the Hub log.

### Changing the Settings from the Admin UI

On a Hub with database-backed settings (every driver), `gcp_iam_check_mode` and `gcp_iam_deny_unknown_policy` are the `gcp_iam` operational settings section. A hub admin edits them on the **GCP Identity** tab of **Admin > Server Config** (or with `PUT /api/v1/admin/server-config`), and the change applies on every replica without a restart. The value in `settings.yaml` or the environment is the deploy-time value the Hub starts from; a saved value overrides it in either direction.

- Only a hub admin signed in with an interactive session can change these settings or reset the section. User access tokens, federated users, agents and brokers are refused with `403`.
- A saved value must be one of the values listed above; an empty or unrecognised value is rejected with `422` and nothing is saved.
- A change that moves either setting to its less strict value is refused with `409` while hub-scoped service account assignment is configured: the hub default GCP identity mode is **Assign**, or any hub-scoped service account is registered. Resetting the section to its deploy-time value follows the same rule.
- Every change is recorded in the mutation audit log (`hub_setting_update`) with the caller, time, old and new value, and the surface (`server-config` or `section-reset`) before it is written. If the record cannot be written, the change is not made.
- Each other replica applies the change on reload only if the stored values are valid and the change passes the same rule. A change that moves either setting to its less strict value must also be named by the latest audit record for each changed key, and that write must not be recorded as not made. The replica then records `hub_setting_apply` (surface `reload`, with the original caller, or the hub itself when no audited write names the change). Otherwise the replica keeps the values it applied, logs an error and records `hub_setting_apply_refused` once. This covers a stored value that cannot be used and a less strict value that no audited write names, including one left by a row that changes or disappears. If the settings store cannot be read, the Hub keeps the values it last applied.
- The page shows the values the Hub applies, and marks a key that an environment variable sets on this node.

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
