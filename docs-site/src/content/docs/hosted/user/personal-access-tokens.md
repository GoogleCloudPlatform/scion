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
project** (or, for a [hub-bound token](#hub-bound-tokens-api-only), to the projects you can
reach) and carries a specific set of action permissions, so a token minted for CI can do only
what CI needs.

**Note on legacy keys:** the legacy `sk_live_*` API keys have been completely removed. All users
must migrate to `scion_pat_*` tokens.

## Scoping and permissions

Every token has a boundary (a single project, or the hub for a
[hub-bound token](#hub-bound-tokens-api-only)) and an explicit list of **scopes** (action
permissions). Available scopes:

| Scope | Grants |
|-------|--------|
| `project:read` | Read project metadata |
| `project:update` | Update project settings, configurations, and annotations |
| `project:manage` | Manage project administration (membership operations) |
| `agent:create` | Create agents |
| `agent:read` | Read agent status/metadata |
| `agent:list` | List agents |
| `agent:lifecycle` | Start, stop, suspend, restart, restore, and reincarnate agents |
| `agent:delete` | Delete agents |
| `agent:message` | Send messages to agents |
| `agent:attach` | Attach to agent sessions (terminal, exec, env, reset-auth) — your own agents and their descendants |
| `agent:port_access` | Access agent forwarded ports — your own agents and their descendants, plus agents in projects where your role grants `agent.port_access` (project owners and admins) |
| `agent:manage` | All agent scopes except `agent:attach` and `agent:port_access` (convenience alias) |

In addition to project and agent scopes, Scion supports UAT scopes for 7 other resource types: `skill`, `template`, `harness_config`, `group`, `user`, `broker`, and `gcp_service_account`. Each resource type provides a `*:manage` convenience alias (e.g., `skill:manage`, `template:manage`) that grants all available actions for that resource.

You can dynamically discover all available scopes and their descriptions by querying the API:
```bash
curl -H "Authorization: Bearer $SCION_HUB_TOKEN" \
     https://scion.example.com/api/v1/auth/scopes
```

When a token is created, the Hub resolves its scopes into a fixed, versioned set of
permissions (the token's permission ceiling), checks that you hold every one of them in the target
project, and stores that ceiling with the token. Every later request made with the token is
limited to that ceiling. A token whose stored ceiling is missing or has an unrecognized version is
denied. Existing tokens are converted to versioned ceilings automatically on upgrade.

### Hub-boundary tokens for broker registration

Most tokens are bound to one project. A token can instead be bound to the **hub boundary**
(`"boundary": {"kind": "hub"}` in `POST /api/v1/auth/tokens`), which is used for hub-level
resources such as Runtime Brokers. The `broker:create` scope is available only with a hub
boundary:

| Scope | Boundary | Grants |
|-------|----------|--------|
| `broker:create` | Hub only | Register a Runtime Broker, or re-register one you own (a super-admin may re-register any broker). Requires that you currently hold `broker.create`, which hub members do. |
| `broker:read` | Hub only | Read Runtime Broker records. `scion runtime-broker register` uses it to check an existing registration. |

`broker:create` covers registration only. It never associates a broker with a project, never
turns on auto-provide, and never rotates a broker's secret. Associating a broker with a project
needs a sign-in by the broker's owner; see
[Sharing a broker with a project](/scion/hosted/ha/runtime-broker/#sharing-a-broker-with-a-project).
For the full steps, see
[Headless registration with a hub token](/scion/hosted/ha/runtime-broker/#headless-registration-with-a-hub-token).

### Scopes are restrictions, not grants

Selecting a scope only **limits** what a token may ever be used for — it never by itself grants
access to anything. Whether a request actually succeeds is still checked against your current
authority on the specific target, every time the token is used.

This matters most for `agent:attach` and `agent:port_access`: you may select either scope for a
project before you have created a single agent in it. The token gains no access from selection
alone — each later attach or port request is independently checked against the specific agent.
An attach request succeeds only for your own agents and their descendants. A port request also
succeeds for any agent in a project where your role grants `agent.port_access`, which the
built-in `project-owner` and `project-admin` roles do. Losing project access (for example,
being removed from the project) makes every request against that project fail immediately, even
though the token itself is still otherwise valid.

### Checking what you can select

Before minting a token, you can ask which scopes you are currently eligible to select for a given
project, and why a particular scope is not available to you:

```bash
scion hub token scopes --project my-project
```

Or via the API:
```bash
curl -H "Authorization: Bearer $SCION_HUB_TOKEN" \
     "https://scion.example.com/api/v1/auth/scopes?projectId=<project-id>"
```

Each scope in the response reports `eligible` and, when it is not, a machine-readable
`eligibilityReason` (for an alias such as `agent:manage`, also which member scopes are
ineligible). The **Create Token** form in the web UI uses the same information: once you pick a
project, scopes you cannot select are shown with the reason.

This answers only "may I select this restriction" — it never lists which agents or other targets
the resulting token could reach. If you request eligibility for a project you cannot access, or
one that does not exist, the request is denied identically in both cases, so the response cannot
be used to discover whether a given project ID exists.

## Creating a token

Generate a new token with the Scion CLI:

```bash
scion hub token create \
  --project my-project \
  --name "github-actions" \
  --scopes agent:create,agent:read,agent:attach \
  --expires 90d
```

- `--project` (required) — the project name or ID the token is scoped to.
- `--name` (required) — a human-readable label.
- `--scopes` (required) — a comma-separated list of the scopes above.
- `--expires` — a duration (`30d`, `90d`, `1y`) or an RFC 3339 date
  (`2026-12-31T00:00:00Z`). Defaults to 90 days; maximum 1 year.
- `--purpose` — an optional description of what the token is for (up to 128 bytes).
- `--label` — an optional `key=value` label; repeat the flag for more (up to 8). Keys are
  lowercase, start with a letter and may contain digits, `_`, `.` and `-` (up to 32 bytes).
  Values are up to 64 bytes. Keys that could be confused with identity or authorization
  fields (for example `user`, `agent`, `owner`, `role` or `scope`) are rejected.

The purpose and labels are descriptive only: they grant no permissions and cannot be changed
after the token is created. The Hub records them, along with the token's identity, in request
logs, authorization decisions and audit records, so you can tell which automation made a call.

The command prints the token value **once**. Store it securely — it cannot be retrieved later.
Each requested scope is checked against your live authority in the project before the token is
written. If a requested scope is denied, the Hub returns `403` with error code
`scope_violation`, with `details.selector` and `details.reason` naming the scope and the reason;
nothing is created. Run `scion hub token scopes --project <project>` to see the full picture
before retrying.

### Hub-bound tokens (API only)

A token can instead carry a **hub boundary**, which lets one token work across every project you
can reach. The CLI always mints project tokens; mint a hub token through the API by sending
`"boundary": {"kind": "hub"}` instead of `projectId` to `POST /api/v1/auth/tokens`. A missing
boundary never means hub, and a hub boundary that also names a project is rejected with `400`.

The same per-request checks apply, with these limits:

- **Lists.** A hub token lists only projects (and their resources) you currently have access to,
  and the token needs the exact list permission, for example `agent:list`. A project token lists
  only its own project. A list cursor issued to one token does not work for a token with a
  different boundary, or for a browser session.
- **Delegation.** Grants a token creates must fall inside its boundary. A token can never create
  a system-scoped grant. Under a hub boundary, you must also currently have the matching access in
  the grant's project.
- **Messages.** A message sent to an agent with a token passes the same boundary, ceiling, and
  live project-access checks before any other rule can allow it.
- **Runtime Broker registration.** The `broker:create` scope can be selected only on a hub token.

## Using a token

Authenticate by setting the token in the `SCION_HUB_TOKEN` environment variable:

```bash
export SCION_HUB_TOKEN="scion_pat_..."
scion list --project my-project
```

When this variable is set, the CLI bypasses the browser-based OAuth flow and uses the token for
all communication with the Hub.

### What scoped tokens cannot do

Because a UAT is scoped, some operations that must be re-checked later, or that rely on
owner or administrator shortcuts, require an unscoped sign-in (CLI or Web UI login) instead:

- **Scheduled work**: creating, updating, re-targeting or resuming scheduled messages and
  scheduled `dispatch_agent` events or schedules. See
  [Scheduling](/scion/hosted/user/scheduling/#security--authorization).
- **Broker secret rotation and association**: no UAT can rotate a Runtime Broker's HMAC
  secret or carry the broker owner's consent to associate a broker with a project. A
  hub-boundary token with `broker:create` can register a broker. See
  [Runtime Broker](/scion/hosted/ha/runtime-broker/#broker-ownership).

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
