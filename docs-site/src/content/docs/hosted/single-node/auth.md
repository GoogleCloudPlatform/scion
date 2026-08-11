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
- **User Access Tokens (UATs)**: For programmatic access and CI/CD pipelines.

## Tenancy: single- vs multi-user

**Tenancy** is whether a deployment serves one identity or many. It is **orthogonal** to the
availability tier — either hosted tier ([Single-node](/scion/hosted/single-node/overview/) or
[HA](/scion/hosted/ha/overview/)) can be single- or multi-user. [Local](/scion/choosing-a-mode/)
and [Workstation](/scion/workstation/workstation-server/) modes are single-user by construction.

- **Single-user** — one principal, with simple auth: a workstation developer token, or a single
  OAuth identity. There are no other users to isolate, so Groups and access policies are not
  needed.
- **Multi-user** — many principals authenticated through an OAuth identity provider (Google or
  GitHub). Access is governed by Hub **Groups** (named collections of users) and access policies
  that decide who can see and act on what.

Deciding to run multi-user is what turns on the rest of this page's OAuth setup, domain
authorization, and the RBAC model. For the authorization model itself — Groups, roles, and
policy bindings — see [Identity & Access (RBAC)](/scion/hosted/ha/permissions/).

:::note[Terminology]
Prefer **single-user / multi-user** over "single-tenant / multi-tenant"; in Scion, "multi-tenancy"
is reserved for organizational isolation, a different concern. See the
[Glossary](/scion/glossary/).
:::

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

## OIDC Identity Provider (IdP)

When enabled, the Scion Hub can act as a minimal OpenID Connect (OIDC) Identity Provider. This allows running agents inside containers to request short-lived, cryptographically signed OIDC tokens to prove their identity to external systems.

### Configuration
Enable the OIDC IdP feature in `settings.yaml`:

```yaml
server:
  oidc:
    enabled: true
    # IssuerURL defaults to the Hub's public endpoint if empty.
    # Must be valid HTTPS in hosted mode (HTTP allowed in workstation mode).
    issuer_url: "https://hub.scion.dev"
    token_lifetime: "15m"
```

### Endpoints
When active, the Hub exposes standard OIDC discovery and key endpoints:
- `/.well-known/openid-configuration`: Returns OIDC discovery metadata.
- `/.well-known/jwks.json`: Publishes the public keys (JWKS) used to verify token signatures.
- `POST /api/v1/agent/identity-token`: Endpoint for agents to request tokens.

### Token Issuance and Rotation
- **Token Signing**: Tokens are RS256-signed using keys generated and managed by the Hub's secrets backend.
- **Key Rotation**: The Hub rotates signing keys every 24 hours automatically, maintaining a key overlap period to ensure seamless token verification during transitions.
- **Audience Scope**: Tokens are minted targeting specific external audiences.

### Agent Identity Token API
For programmatic or custom integrations where `sciontool` is not available, agents can request OIDC identity tokens directly from the Hub's token endpoint.

* **Endpoint:** `POST /api/v1/agent/identity-token`
* **Authentication:** Requires a valid Agent Token header (e.g. `X-Scion-Agent-Token: <scion JWT>`).
* **Request Body:** Must be JSON and include the mandatory `audience` parameter targeting the external service.

**Example Request:**

```http
POST /api/v1/agent/identity-token HTTP/1.1
Host: hub.scion.dev
X-Scion-Agent-Token: <agent_jwt_token>
Content-Type: application/json

{
  "audience": "https://vault.example.com"
}
```

:::caution[Audience is Mandatory]
The `audience` parameter is strictly required in the JSON body. If the `audience` field is missing, empty, or the request body is malformed, the Hub will immediately reject the call with an HTTP `400 Bad Request` status.
:::

**Example Response (HTTP 200 OK):**

```json
{
  "token": "eyJhbGciOiJSUzI1NiIsImtpZCI6Ii4uLiJ9...",
  "expires_at": "2026-08-11T12:15:00Z"
}
```

### In-Agent Retrieval
From inside any authorized agent container, retrieve an OIDC identity token using `sciontool`:

```bash
sciontool identity-token --audience="https://vault.example.com"
```

Use this token to authenticate agents to external services like HashiCorp Vault, AWS IAM Roles for Service Accounts (IRSA), or GCP Workload Identity Federation (WIF).

---

## OIDC-Based Federation

Scion supports inbound OIDC-based federation authentication. This allows external identities (such as other Scion Hubs, Google Cloud Service Accounts, or Firebase/Google users) to authenticate against the Hub API using OIDC ID tokens from trusted issuers.

### Configuration and Runtime Management

Federation authentication is feature-gated. It can be configured initially at bootstrap via the `server.federation` block in `settings.yaml`, or managed dynamically at runtime via the **Admin UI** (using the `opsettings` pattern).

#### Runtime Administration (Admin UI)
When running in database mode, administrators can manage OIDC federation configuration directly in the Admin UI without restarting the Hub:
* **Issuer CRUD**: Create, read, update, and delete trusted OIDC issuers dynamically. The interface provides conditional input fields based on the selected identity/issuer type.
* **Hot-Reloading**: Changes saved in the UI are immediately applied cluster-wide. The backend utilizes an `atomic.Pointer` to hot-reload the `FederationAuthenticator`, ensuring a zero-downtime, lock-free path for inbound federated requests.
* **Semantic Validation**: The Admin API performs strict semantic validation on save, preventing malformed issuer rules or invalid URLs from reaching the live runtime.

#### Static Bootstrap Configuration (`settings.yaml`)
Alternatively, or for initial bootstrapping, you can configure federation statically in your configuration file.

:::note[OIDC Wiring Guarantee]
Federation and OIDC configurations are fully wired end-to-end into the server's config schemas (`config.GlobalConfig` and `V1ServerConfig`), ensuring no federation fields are silently dropped on file load. In combo-server setups, standard `/.well-known/` discovery endpoints are routed correctly and are not intercepted by the SPA catch-all routing.
:::

```yaml
server:
  federation:
    enabled: true
    trusted_issuers:
      - issuer_url: "https://hub.other-org.com"
        # Optional: Explicit JWKS URL (retrieved via discovery if omitted)
        jwks_url: "https://hub.other-org.com/.well-known/jwks.json"
        expected_audience: "https://hub.scion.dev"
        # Optional restriction lists
        allowed_projects: ["project-uuid-1", "project-uuid-2"]
        allowed_root_users: ["user@other-org.com"]
        # Default scopes/roles for identities from this issuer
        default_scopes: ["project:read", "agent:status:update"]
        issuer_type: "hub" # hub, service_account, or user
```

### How It Works
- **Multi-Issuer Support**: The federation authenticator handles multiple external trust domains simultaneously.
- **JWKS Caching**: Public verification keys are cached locally with RS256-signature pinning and automatic refresh to eliminate per-request latency.
- **Federation Access Middleware**: Requests carrying external OIDC tokens pass through the `RequireFederationAccess` scope-gated middleware, validating token authenticity, issuer rules, and matching scopes.
- **Identity Types**:
  - `hub`: Identifies requests originating from federated partner Hubs.
  - `service_account`: Authenticates automated workloads via GCP Service Accounts.
  - `user`: Maps OIDC tokens to standard user identities, with configurable `default_role` (defaults to `viewer`) and domain restrictions using wildcards (e.g. `allowed_emails: ["*@example.com"]`).

---

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

### Using the Developer Token
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

For details on configuring and managing secrets, see [Secret Management](/scion/hosted/user/secrets/).

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

1.  **Login**: `scion hub auth login` opens a browser to the dashboard login page.
2.  **Exchange**: After successful login, the dashboard provides a token (or the CLI exchanges a code).
3.  **Storage**: The token is stored in `~/.scion/config.json`.

## Agent Authentication

Agents are automatically authenticated. When the Hub dispatches an agent to a Runtime Broker, it includes a one-time-use **Agent Token**.
- The agent uses this token for all calls back to the Hub (e.g., updating status, streaming logs).
- Tokens are scoped to the specific agent and its project.
- Tokens have a default expiration (typically 24 hours), but Scion implements an automated token refresh mechanism to ensure long-running agents maintain valid authorization throughout extended tasks.

## User Access Tokens

For programmatic access (e.g., CI/CD pipelines), the Hub supports **user access tokens (UATs)**.
- Tokens can be generated via the Web Dashboard or CLI (`scion hub token create`).
- Tokens are prefixed with `scion_pat_` (a legacy artifact of the older "personal access token" name).
- Use the `Authorization: Bearer <token>` header in your requests.

See [User Access Tokens](/scion/hosted/user/personal-access-tokens/) for the full user-facing guide.