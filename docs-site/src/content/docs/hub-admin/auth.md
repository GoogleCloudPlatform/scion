---
title: Authentication & Identity
description: Configuring authentication flows for Scion.
---

Scion implements a unified authentication system designed to secure communication between all components: the CLI, the Web Dashboard, the Hub, and individual Agents.

## Identity Types

Scion recognizes four primary identity types:

1.  **Users**: Humans interacting via the CLI or Web Dashboard. Authenticated via OAuth or Development tokens.
2.  **Agents**: Running LLM instances. Authenticated via short-lived JWTs issued by the Hub during provisioning.
3.  **Runtime Brokers**: Infrastructure nodes that execute agents. Authenticated via Broker tokens.
4.  **Development User**: A special identity used for local development and zero-config testing.

## Authentication Methods

Scion supports multiple authentication methods for different use cases:

- **OAuth (Google/GitHub)**: For production web and CLI authentication.
- **Development Auth**: For local development and testing.
- **Personal Access Tokens (PATs)**: For programmatic access and CI/CD pipelines.

## OAuth Authentication

Scion supports OAuth authentication via Google and GitHub. OAuth credentials are configured separately for web and CLI clients due to different redirect URI requirements.

### Web OAuth Setup

Configure web OAuth with these environment variables:

```bash
export SCION_SERVER_OAUTH_WEB_GOOGLE_CLIENTID="your-client-id"
export SCION_SERVER_OAUTH_WEB_GOOGLE_CLIENTSECRET="your-client-secret"
export SCION_SERVER_OAUTH_WEB_GITHUB_CLIENTID="your-client-id"
export SCION_SERVER_OAUTH_WEB_GITHUB_CLIENTSECRET="your-client-secret"
```

### CLI OAuth Setup

Configure CLI OAuth with these environment variables:

```bash
export SCION_SERVER_OAUTH_CLI_GOOGLE_CLIENTID="your-client-id"
export SCION_SERVER_OAUTH_CLI_GOOGLE_CLIENTSECRET="your-client-secret"
export SCION_SERVER_OAUTH_CLI_GITHUB_CLIENTID="your-client-id"
export SCION_SERVER_OAUTH_CLI_GITHUB_CLIENTSECRET="your-client-secret"
```

## Domain Authorization

You can restrict authentication to specific email domains using the `SCION_AUTHORIZED_DOMAINS` setting. This provides an additional layer of access control beyond OAuth authentication.

### Configuration

Set the environment variable with a comma-separated list of allowed domains:

```bash
# Allow only users from these domains
export SCION_AUTHORIZED_DOMAINS="example.com,mycompany.org"
```

Or configure in `server.yaml`:

```yaml
auth:
  authorizedDomains:
    - example.com
    - mycompany.org
```

### Behavior

- **Empty list (default)**: All email domains are allowed.
- **Non-empty list**: Only emails from listed domains can authenticate.
- **Case insensitive**: `Example.COM` matches `example.com`.
- **Exact match**: Subdomains must be listed explicitly.

## Development Authentication (Dev Auth)

To minimize friction during local setup, Scion includes a "Dev Auth" mode. When enabled, the Hub auto-generates a token and creates a "Development User" identity.

### Enabling Dev Auth
Start the server with the `--dev-auth` flag or set it in your `server.yaml`:

```yaml
auth:
  devMode: true
```

Or via environment variable:
```bash
export SCION_SERVER_AUTH_DEVMODE=true
```

### Using the Dev Token
When the Hub starts with `devMode: true`, it writes the token to `~/.scion/dev-token`.
- **CLI**: The `scion` CLI automatically looks for this file.
- **Web**: The Web Dashboard automatically uses this token for the "Development User" login when `SCION_DEV_AUTH_ENABLED=true` is set.

Alternatively, you can set the token in your environment:
```bash
export SCION_DEV_TOKEN=scion_dev_...
```

## Runtime Broker Security

Runtime Brokers use a robust security model to ensure that only authorized Hubs can dispatch commands and that agents remain isolated.

### HMAC-Based Authentication

Communication between the Hub and a Runtime Broker (in both directions) is secured using **HMAC-SHA256 request signing**. This provides several security benefits:
- **Mutual Authentication**: Both parties prove they possess the shared secret.
- **Payload Integrity**: The request body is included in the signature, preventing tampering.
- **Replay Protection**: Every request includes a timestamp and a unique nonce.

A shared secret is established during the `scion broker register` flow and is stored locally in `~/.scion/broker-credentials.json`.

### Provider Authorization

The Hub enforces a "Provider" model for authorization. Even if a broker is authenticated, it will only receive agent dispatch requests for **Projects** that it has been explicitly registered to provide for. This prevents a compromised broker from accessing projects it shouldn't have access to.

### Secret Management

Brokers never store agent secrets (like API keys) on disk.
1. The Hub resolves secrets from all applicable scopes (user, project, broker) via the configured secrets backend (e.g., GCP Secret Manager).
2. The Hub includes the resolved secrets in the `CreateAgent` command sent to the Broker over the TLS-secured control channel.
3. The Broker projects secrets into the agent container based on their type (environment variable, JSON file, or filesystem path).
4. When the agent is deleted, the secrets are purged from the host.

For details on configuring and managing secrets, see [Secret Management](/scion/hub-user/secrets/).

## GCP Identity & Metadata Emulation

Scion provides a native mechanism to assign Google Cloud Platform (GCP) identities to agents, even when running on non-GCP infrastructure. This is achieved through an in-process metadata server emulator within `sciontool` that intercepts requests to the standard GCE metadata IP (`169.254.169.254`).

### Metadata Modes

When creating an agent, you can configure its **GCP Identity Mode**:

- **Block (Default)**: All requests to the metadata server are intercepted and return a 403 Forbidden. This ensures agents cannot "leak" the host's identity (e.g., when running on a GCE instance or GKE node).
- **Assign**: Assigns a specific Google Service Account to the agent.
  - The agent's `sciontool` sidecar intercepts requests to the metadata server.
  - Token requests are proxied to the Scion Hub, which uses its own broad permissions to generate a short-lived access token for the requested Service Account (via the `iam.serviceAccounts.getAccessToken` permission).
  - The token is then returned to the agent, allowing it to use standard GCP SDKs (Application Default Credentials) as that specific Service Account.
- **Passthrough**: Requests are allowed to reach the actual host metadata server. Use with caution as this allows the agent to assume the identity of the underlying node. Security is tightened by restricting GCP identity passthrough to broker owners only.

### Management UI & Hub-Minted Service Accounts

Administrators can manage available Service Accounts through the **Service Accounts** section in the Admin dashboard. 
- **Registration**: Register existing GCP Service Accounts by email.
- **Hub-Minted Accounts**: The Hub can directly manage and provision (mint) GCP service accounts based on your quota dashboard and capability controls.
- **Validation**: Scion auto-verifies that the Hub has the necessary permissions to act as the registered Service Account upon registration.
- **Assignment & Defaults**: Service Accounts can be assigned to agents during the creation flow. Projects also support default GCP identities that are automatically applied in the agent creation form.

### Security & Auditing

- **Iptables Interception**: Scion uses `iptables` (when `NET_ADMIN` capability is available) to redirect traffic from `169.254.169.254:80` to the local sidecar.
- **Authorization Checks**: Administrative actions for GCP Service Account management require `project-owner` (`ActionManage`) permissions to enforce strict authorization boundaries.
- **Rate Limiting**: Token requests are rate-limited per-agent to prevent abuse.
- **Audit Logging**: All token issuance events are logged at the Hub level with the requesting `agent_id` and `user_id`.

## GitHub App Integration

Scion supports native GitHub App integration for secure, automated agent authentication with GitHub repositories. This provides a robust alternative to static personal access tokens.

### Features
- **Native Auth**: Uses JWT-based authentication and automated installation token minting.
- **Automated Token Refresh**: A background refresh loop ensures long-running agents always have valid git credentials.
- **Git Credential Helper**: The `sciontool` injects a credential helper into the agent environment, providing fresh tokens to `git` on-demand.
- **Commit Attribution**: Supports per-project git identity configuration to ensure commits are authored correctly.
- **Admin Management**: Global monitoring of installations, rate limits, and status via the "GitHub App" tab in the Admin Server Config UI.

### Project Association
Projects can be linked to specific GitHub App installations. The system automatically associates GitHub App installations at project creation time, streamlining the authentication flow for private repositories. Project settings provide visual indicators and permission badges for real-time feedback on integration health.


## CLI Authentication

Users can authenticate the CLI against a Scion Hub using the following flow:

1.  **Login**: `scion auth login` opens a browser to the dashboard login page.
2.  **Exchange**: After successful login, the dashboard provides a token (or the CLI exchanges a code).
3.  **Storage**: The token is stored in `~/.scion/config.json`.

## Agent Authentication

Agents are automatically authenticated. When the Hub dispatches an agent to a Runtime Broker, it includes a one-time-use **Agent Token**.
- The agent uses this token for all calls back to the Hub (e.g., updating status, streaming logs).
- Tokens are scoped to the specific agent and its project.
- Tokens have a default expiration (typically 24 hours), but Scion implements an automated token refresh mechanism to ensure long-running agents maintain valid authorization throughout extended tasks.

## Personal Access Tokens

For programmatic access (e.g., CI/CD pipelines), the Hub supports Personal Access Tokens (PATs).
- Tokens can be generated via the Web Dashboard or CLI.
- Tokens are prefixed with `scion_pat_`.
- Use the `Authorization: Bearer <token>` header in your requests.