---
title: Personal Access Tokens
description: Managing and using Personal Access Tokens (PATs) in Scion.
---

Scion supports Personal Access Tokens (PATs) for programmatic access to the Hub API and for authenticating CLI operations when browser-based OAuth is not feasible.

## Overview

A Personal Access Token is a long-lived credential linked to your user account. It inherits all your permissions, allowing scripts, CI/CD pipelines, or remote tools to interact with the Scion Hub on your behalf. 

**Note on Legacy Keys:** The legacy `sk_live_*` API keys have been completely removed. All users must migrate to the new `scion_pat_*` tokens.

## Scoping and Permissions

Personal Access Tokens support granular scoping. When creating a token, you can:
- Restrict the token to specific actions (e.g., read-only, agent creation).
- Limit the token's scope to specific projects rather than global access.

## Creating a Personal Access Token

You can generate a new PAT using the Scion CLI:

```bash
scion auth tokens create "My CI/CD Token"
```

This will output the token value. **Store this token securely.** It is only displayed once and cannot be retrieved later.

## Using a Personal Access Token

To authenticate with a PAT, you must set it in your environment using the `SCION_HUB_TOKEN` variable:

```bash
export SCION_HUB_TOKEN="scion_pat_..."
scion list
```

When this environment variable is set, the CLI will bypass the browser-based OAuth flow and use the token for all communication with the Hub.

## Trust Level Separation

It is crucial to understand the distinction between how users authenticate with the Hub and how agents authenticate with the Hub. Scion uses two separate environment variables for this purpose to enforce strict privilege boundaries:

### `SCION_HUB_TOKEN` (User Level)
- **Purpose**: Authenticates a human user or a CI/CD pipeline.
- **Scope**: Grants access based on the user's permissions and the specific scopes assigned to the token.
- **Usage**: Used by the Scion CLI or external scripts calling the Hub API.

### `SCION_AUTH_TOKEN` (Agent Level)
- **Purpose**: Authenticates an agent running within a container.
- **Scope**: Carries a Hub-issued JWT scoped specifically to that agent. It is short-lived, auto-injected by the Runtime Broker, and grants only the specific permissions that agent needs to function (e.g., reporting status, reading its own secrets).
- **Usage**: Automatically used by the `sciontool` binary running inside the agent.

:::danger[Privilege Escalation Risk]
**Never inject a `SCION_HUB_TOKEN` (or a user-level PAT) into an agent container as the `SCION_AUTH_TOKEN`.** 

Injecting a user PAT into an agent means the agent will operate with your full user permissions, rather than its intended, restricted scope. This allows the agent to create other agents, access other projects, or read secrets it shouldn't have access to. The Scion runtime automatically handles agent authentication; you do not need to manually configure agent tokens.
:::

## Managing Tokens

Tokens can be managed either via the CLI or the Web UI.

### Using the Web UI
The easiest way to administer your tokens is through the **Web UI management interface** available in your user profile. This interface allows you to create, view, and revoke tokens visually, as well as configure specific action permissions and project-level scopes.

### Using the CLI
If a token is compromised or no longer needed, you can revoke it:

```bash
scion auth tokens revoke <token-id>
```

You can list all your active tokens using:

```bash
scion auth tokens list
```
