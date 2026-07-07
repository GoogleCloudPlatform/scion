---
title: User Access Tokens
description: Managing and using user access tokens (UATs) in Scion.
---

Scion supports **user access tokens (UATs)** for programmatic access to the Hub API and for
authenticating CLI operations when browser-based OAuth is not feasible — for example in CI/CD
pipelines or automation scripts.

:::note[Naming]
The canonical term is **user access token (UAT)**, per the root
[`GLOSSARY.md`](https://github.com/GoogleCloudPlatform/scion/blob/main/GLOSSARY.md). You may
still see the older name *personal access token (PAT)* in some places, and the on-the-wire token
prefix is `scion_pat_` — a legacy artifact of that earlier name. The two terms refer to the same
credential.
:::

## Overview

A user access token is a scoped, revocable bearer token linked to your user account, used for
non-interactive authentication. Unlike a full OAuth session, a UAT is **scoped to a single
project** and carries a specific set of action permissions, so a token minted for CI can do only
what CI needs.

**Note on legacy keys:** the legacy `sk_live_*` API keys have been completely removed. All users
must migrate to `scion_pat_*` tokens.

## Scoping and permissions

Every token is scoped to a single project and to an explicit list of **scopes** (action
permissions). Available scopes:

| Scope | Grants |
|-------|--------|
| `project:read` | Read project metadata |
| `agent:create` | Create agents |
| `agent:read` | Read agent status/metadata |
| `agent:list` | List agents |
| `agent:start` | Start/restart agents |
| `agent:stop` | Stop agents |
| `agent:delete` | Delete agents |
| `agent:message` | Send messages to agents |
| `agent:attach` | Attach to agent sessions |
| `agent:dispatch` | Dispatch agents (create + start) |
| `agent:manage` | All agent scopes (convenience alias) |

## Creating a token

Generate a new token with the Scion CLI:

```bash
scion hub token create \
  --project my-project \
  --name "github-actions" \
  --scopes agent:dispatch,agent:read,agent:stop \
  --expires 90d
```

- `--project` (required) — the project name or ID the token is scoped to.
- `--name` (required) — a human-readable label.
- `--scopes` (required) — a comma-separated list of the scopes above.
- `--expires` — a duration (`30d`, `90d`, `1y`) or an RFC 3339 date
  (`2026-12-31T00:00:00Z`). Defaults to 90 days; maximum 1 year.

The command prints the token value **once**. Store it securely — it cannot be retrieved later.

## Using a token

Authenticate by setting the token in the `SCION_HUB_TOKEN` environment variable:

```bash
export SCION_HUB_TOKEN="scion_pat_..."
scion list --project my-project
```

When this variable is set, the CLI bypasses the browser-based OAuth flow and uses the token for
all communication with the Hub.

## Trust level separation

It is crucial to distinguish how **users** authenticate with the Hub from how **agents**
authenticate with the Hub. Scion uses two separate environment variables to enforce strict
privilege boundaries:

### `SCION_HUB_TOKEN` (user level)
- **Purpose**: Authenticates a human user or a CI/CD pipeline.
- **Scope**: Grants access based on the user's permissions and the specific scopes assigned to
  the token.
- **Usage**: Used by the Scion CLI or external scripts calling the Hub API.

### `SCION_AUTH_TOKEN` (agent level)
- **Purpose**: Authenticates an agent running within a container.
- **Scope**: Carries a Hub-issued JWT scoped specifically to that agent. It is short-lived,
  auto-injected by the Runtime Broker, and grants only the specific permissions that agent needs
  to function (e.g., reporting status, reading its own secrets).
- **Usage**: Automatically used by the `sciontool` binary running inside the agent.

:::danger[Privilege escalation risk]
**Never inject a `SCION_HUB_TOKEN` (or a user-level UAT) into an agent container as the
`SCION_AUTH_TOKEN`.**

Injecting a user token into an agent means the agent will operate with your full user
permissions, rather than its intended, restricted scope. This allows the agent to create other
agents, access other projects, or read secrets it shouldn't have access to. The Scion runtime
automatically handles agent authentication; you do not need to manually configure agent tokens.
:::

## Managing tokens

Tokens can be managed either via the CLI or the Web UI.

### Using the Web UI
The easiest way to administer your tokens is through the **Web UI management interface**
available in your user profile. It lets you create, view, and revoke tokens visually, and
configure action permissions and project-level scopes.

### Using the CLI

List your tokens (name, ID, visible prefix, status, expiry, and scopes):

```bash
scion hub token list
```

Revoke a token — it stops working for authentication but remains visible in listings as
revoked:

```bash
scion hub token revoke <token-id>
```

Delete a token entirely (rather than leaving it in listings as revoked):

```bash
scion hub token delete <token-id>
```
