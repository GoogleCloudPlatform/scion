# Hub Agent-Endpoint Override

**Date:** 2026-09-25
**Branch:** scion/hub-agent-endpoint
**Fork PR:** ptone/scion#1898

## Problem

The Hub injects one endpoint value into every dispatched agent as `SCION_HUB_ENDPOINT`. That same value also feeds admin-generated invite links, chat-bridge registration links, the OIDC issuer default, and the `cloudrun_invoker` transport-audience default. A deployment where agents reach the Hub on a different address than users — for example an internal VPC URL, while a proxy fronts the Hub for the public — had no way to separate the two without breaking the user-facing links.

## Solution

Added an optional setting, `server.hub.agent_endpoint`, that overrides the Hub URL injected into agents only. Every other consumer keeps reading the regular hub endpoint.

- New field on `V1ServerHubConfig` (`agent_endpoint`, settings.yaml) and `HubServerConfig`/`GlobalConfig.Hub` (`AgentEndpoint`), following the same conversion conventions already used by `public_url`. The functional override is `SCION_SERVER_HUB_AGENTENDPOINT` (the env var that actually reaches a running Hub).
- Validated and normalized at startup, only when this process runs the Hub: the value must be `scheme://host[:port]` — `http` or `https`, an IP literal or a hostname of letters, digits, `_`, `-`, and `.` (Docker Compose-style names such as `scion_hub` are accepted), and an optional port in `1..65535` — with no userinfo, query, fragment, path beyond `/`, or IPv6 zone. The normalized form (lowercase scheme, authority rebuilt from the parsed host and port so an empty port is dropped, a port with leading zeros is rewritten to its canonical decimal form, and an IPv6 host is bracketed) is what gets stamped everywhere and always re-parses to the same scheme and host. No error message echoes any part of the configured value — not even the scheme or the host, either of which could itself be a leaked secret or username: a rejected value is, by definition, one this function has not finished validating, so no substring of it — including the path or query, which can carry credentials even in a value with no `//` authority — is assumed safe to print. The port-range message does not name the host either, since the host itself could be the leaked credential (e.g. `http://admin:123456`). This means a mistyped credential is never written back into the startup log or an admin API response.
- `HTTPAgentDispatcher` gained `SetAgentEndpoint` and an internal `effectiveAgentHubEndpoint()` helper, applied at every site that stamps `SCION_HUB_ENDPOINT`: the create request, the start path, and the restart path. When unset, agents receive the Hub's regular endpoint — `public_url`, or the endpoint the Hub resolves when `public_url` is itself unset. When both the override and the regular endpoint are empty, the start and restart paths omit the `SCION_HUB_ENDPOINT` key entirely rather than injecting an empty string.
- `hub.ServerConfig` gained a matching `AgentEndpoint` field, copied from `cfg.Hub.AgentEndpoint` by a small pure helper (`buildHubServerConfig`) that a unit test pins directly, so a dropped field shows up without needing a running Hub.
- The setting applies to agents on every runtime broker attached to the Hub, including remote brokers. The Hub logs an Info line naming both `agent_endpoint` and `hub_endpoint` (the regular endpoint it replaces for agents) when the override is configured, and the settings reference documents the scope, the accepted URL format, and the cleartext-transport risk of an `http://` value.
- `server.hub.agent_endpoint` is documented in `settings.yaml.example` as a Layer 0 (file/env only, restart required) setting, alongside `hub_id` and `gcp_project_id`, rather than next to the Admin-Settings-API-managed `public_url`. In file mode, the same generic admin server-config write endpoint that can set `public_url` can also set this key; that write path now validates it with the same rules the Hub applies at startup and persists the normalized form, so a malformed value is rejected immediately instead of only failing at the next restart.

## Files Changed

| File | Change |
|------|--------|
| `pkg/config/hub_config.go` | Added `HubServerConfig.AgentEndpoint` and `ValidateAgentEndpoint` (validates per-label hostname syntax or an IP literal, rejects an IPv6 zone, normalizes, and never echoes the configured value in an error) |
| `pkg/config/settings_v1.go` | Added `V1ServerHubConfig.AgentEndpoint`, conversion both ways, and a `knownCompoundFields` entry |
| `pkg/hub/server.go` | Added `ServerConfig.AgentEndpoint`; wires the dispatcher's `SetAgentEndpoint` when configured and logs the all-brokers-scope notice |
| `pkg/hub/httpdispatcher.go` | Added `SetAgentEndpoint` and `effectiveAgentHubEndpoint()`; used at the create, start, and restart injection sites |
| `pkg/hub/admin_settings.go` | The file-mode admin server-config write handler validates and normalizes `server.hub.agent_endpoint` when present, returning 400 on an invalid value instead of persisting it |
| `cmd/server_foreground.go` | `buildHubServerConfig` (pure config assembly), `validateServerPreflight` (startup validation, hub-enabled only), and `resolveTransportAudience` (extracted `cloudrun_invoker` audience derivation, tested to never read the override) |
| `pkg/config/schemas/settings-v1.schema.json`, `settings.yaml.example`, `docs-site/.../reference/server-config.md` | Documented the new setting (next to `public_url` in the reference table and schema; with the Layer 0 keys in settings.yaml.example): its accepted URL format and charset, its all-brokers scope, the cleartext-transport warning for `http://` values, and the admin-API write path |

## What it does not affect

Admin invite links, chat-bridge registration links and bootstrap config (the `hub_url` credential and the bridge config template), the OIDC issuer default, the `cloudrun_invoker` audience default, the IAP-audience/public-endpoint derivation, and the co-located broker's own hub connection all keep reading the Hub's regular endpoint — none of that code was changed. Tested directly for invite links, the OIDC issuer default, the chat-bridge `hub_url` credential and bootstrap config template, and the public-endpoint resolution function.

## Broker-side pass-through

No changes were needed in `pkg/runtimebroker`. The override only changes the value the Hub stamps before it reaches the broker; from there it is passed through exactly like `public_url` always was. Confirmed by reading `start_context.go`/`hubenv.go`: an IP-literal endpoint passes through unchanged on Docker and Kubernetes, a hostname endpoint gets the existing host-gateway mapping on co-located Docker, and the `cloudrun-sandbox` runtime keeps its own link-local endpoint logic regardless of this setting (agents on that runtime are unaffected by the override).

## Notes

- `server.hub.agent_endpoint` is not part of the operational-settings (Postgres) model, unlike `public_url` — in postgres mode it can only be set through settings.yaml or the `SCION_SERVER_HUB_AGENTENDPOINT` env var on each node, and a postgres-mode admin API write of it returns 422 (unclassified key). In file mode, it is writable through the generic admin server-config API like any other `server.hub` key, is validated and normalized on write. Like `public_url`, it has no live-reload path and takes effect only at the Hub's next restart.
- The setting is hub-wide: it is injected into agents dispatched on every broker attached to the Hub, including remote brokers on other networks. Operators should only set it when every such broker's agents can reach the address and it is the same Hub on each of those networks.
- Out of scope: per-profile/runtime endpoint overrides, and any `scripts/single-node-vm/deploy.sh` change.
