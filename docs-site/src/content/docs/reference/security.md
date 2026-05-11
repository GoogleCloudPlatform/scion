---
title: Security Architecture
description: Comprehensive overview of Scion's security model, including authentication, transport security, and permissions.
---

Scion is designed with a multi-layered security model to ensure the integrity and confidentiality of agent operations, user data, and system communications. This document outlines the authentication mechanisms, transport security protocols, and future authorization plans for the Scion platform.

## 1. Authentication Model

Scion operates in multiple contexts, each with specific security requirements. Authentication is managed centrally by the **Scion Hub**, which resolves identities for users, agents, and infrastructure components.

### 1.1 Authentication Contexts

| Context | Client Type | Auth Method | Token Storage |
|---------|-------------|-------------|---------------|
| **Web Dashboard** | Browser | OAuth 2.0 + Session Cookie | HTTP-only cookie |
| **CLI (Hub Commands)** | Terminal | OAuth 2.0 + Device Flow | `~/.scion/credentials.json` |
| **Agent (sciontool)** | Container | Hub-issued JWT | Env Var (`SCION_HUB_TOKEN`) |
| **Runtime Broker** | Compute Node | HMAC Signature | `~/.scion/broker-credentials.json` |
| **Development** | Any | Dev Token (Bearer) | `~/.scion/dev-token` |

### 1.2 User Authentication (OAuth 2.0)

For both Web and CLI access, Scion relies on standard OAuth 2.0 providers (Google and GitHub).

- **Web Flow**: Standard Authorization Code flow. The embedded Go web server handles the callback and exchanges the provider token for a session-bound Hub access token.
- **CLI Flow**: Uses a localhost callback server (defaulting to port `18271`). The CLI opens the user's browser for authentication and receives the authorization code via the local server.
- **PKCE**: The CLI uses Proof Key for Code Exchange (PKCE) to prevent authorization code injection attacks.

### 1.3 Agent Authentication (`sciontool`)

Agents running inside containers must report status back to the Hub without possessing user-level credentials.

- **Hub-Issued JWT**: During provisioning, the Hub generates a short-lived JWT scoped specifically to that agent instance.
- **Claims**: The token includes the `agent_id` (sub) and `project_id`.
- **Scopes**: Standardized scopes include:
    - `agent:status:update`: Allows reporting progress and heartbeats.
    - `agent:log:append`: Allows streaming logs back to the Hub.
    - `project:secret:read`: Allows the agent to retrieve project-scoped secrets.
- **Transmission**: The token is injected into the container via the `SCION_HUB_TOKEN` environment variable and is used by `sciontool` for all API calls.

### 1.4 Runtime Broker Authentication (HMAC)

Runtime Brokers represent high-trust infrastructure. They use HMAC-based request signing for bidirectional authentication with the Hub.

- **Shared Secret**: Established during initial registration via a short-lived `joinToken`.
- **Signing**: Every request includes headers for `X-Scion-Broker-ID`, `X-Scion-Timestamp`, `X-Scion-Nonce`, and `X-Scion-Signature`.
- **Replay Protection**: Nonce-based tracking and timestamp validation (5-minute clock skew tolerance) prevent replay attacks.
- **NAT Traversal**: Brokers establish a persistent WebSocket control channel. The initial upgrade request is HMAC-authenticated, establishing a trusted session for subsequent commands.

## 2. Transport Security

### 2.1 TLS and HTTPS Enforcement

In production mode, Scion mandates the use of TLS for all network traffic.

- **HTTPS Enforcement**: The Hub server rejects non-HTTPS requests (unless configured for local development or behind a trusted TLS-terminating proxy).
- **Security Headers**: Standard headers such as `Strict-Transport-Security` (HSTS), `X-Frame-Options`, and `Content-Security-Policy` are enforced.
- **mTLS (Future)**: Support for Mutual TLS between Hub and Runtime Brokers is planned for high-security environments.

### 2.2 WebSocket Security

- **CLI/Agents**: Use standard `Authorization` headers.
- **Browser/Web**: Since browser WebSocket APIs cannot set custom headers, Scion uses a **Ticket-Based Authentication** system. The client requests a short-lived, single-use ticket via a POST request (authenticated by cookie) and provides it in the WebSocket query string (`?ticket=...`).

## 3. Authorization and Access Control

### 3.1 Domain Authorization

Scion supports restricting authentication to specific email domains via the `SCION_AUTHORIZED_DOMAINS` configuration. This provides a first-line defense, ensuring only authorized organization members can access the Hub.

### 3.2 Permissions System (Future Plans)

A comprehensive, hierarchical RBAC (Role-Based Access Control) system is currently in the design phase. For a detailed technical specification of the policy language and agent identity claims, see the [Policy & Permissions Reference](/scion/reference/permissions-policy/).

- **Principal-Based**: Permissions are granted to **Users** and **Groups**.
- **Hierarchical Groups**: Groups can contain other groups, allowing for complex team structures.
- **Resource Scopes**: Policies are attached to scopes (Hub, Project, or specific Resource) and follow a containment hierarchy.
- **Override Model**: Lower-level policies (e.g., at the Agent level) override higher-level ones (e.g., at the Project level), allowing for granular delegation of authority.
- **Actions**: Standardized CRUD actions (`create`, `read`, `update`, `delete`, `list`) plus resource-specific actions (`start`, `stop`, `attach`, `message`).

## 4. Secret Management

Scion provides a typed, scope-aware secret management system. Secret values are never stored in plaintext in the Hub database. For a user-facing guide, see [Secret Management](/scion/hub-user/secrets/).

### 4.1 Secrets Backend Architecture

The Hub uses a pluggable **SecretBackend** interface for secret storage:

| Backend | Value Storage | Write Operations | Read Operations |
|---------|--------------|-----------------|-----------------|
| **`gcpsm`** (GCP Secret Manager) | Encrypted in GCP SM | Supported | Supported |
| **`local`** (default) | N/A | **Rejected** (returns 501) | Read-only (existing data) |

When `gcpsm` is configured, a hybrid model is used:
- **Metadata** (name, type, scope, version) is stored in the Hub database.
- **Secret values** are stored in GCP Secret Manager with automatic versioning.
- GCP SM secret names follow the pattern: `scion-{scope}-{sha256(scopeID)[:12]}-{name}`.

The `local` backend rejects all write operations to prevent plaintext secret storage. It supports read and delete operations only for migration of pre-existing data.

### 4.2 Secret Scopes and Resolution

Secrets are scoped and resolved hierarchically when an agent starts. The following scopes are merged in order (last one wins for the same key):

1. **Hub scope** (lowest priority): Global defaults.
2. **User scope**: Personal secrets for the agent's owner.
3. **Project scope**: Project-level secrets.
4. **Runtime Broker scope** (highest priority): Infrastructure-level overrides.

This produces a merged set of secrets for each agent, where more specific scopes override broader ones.

### 4.3 Secret Types and Projection

Secrets are typed to control how they reach the agent container:

- **`environment`**: Injected as environment variables (default).
- **`variable`**: Written to `~/.scion/secrets.json` inside the container.
- **`file`**: Written to a specified filesystem path (max 64 KiB).

### 4.4 Personal Access Tokens (PATs)

For headless environments (CI/CD, automation), Scion supports **Personal Access Tokens**.
- Tokens are prefixed with `scion_pat_`.
- Only the SHA-256 hash of the token is stored in the database; the original value is never persisted.
- Tokens can be scoped to specific permissions and projects, and revoked instantly via the dashboard or CLI.

### 4.5 Credentials Propagation

Scion ensures that sensitive credentials (GCP Service Accounts, API keys for LLMs) are propagated into agent containers securely.
- **Docker / Podman**: Injected via environment variables or read-only bind mounts for file-type secrets. File secrets are written to a temporary directory and mounted into the container at the target path.
- **Kubernetes**: Propagated via Kubernetes Secrets or Secret Manager CSI drivers (e.g., GCP Secret Manager).
- **Broker Mode Isolation**: When agents are dispatched via the Hub, the credential pipeline only uses hub-resolved secrets and environment variables. The broker operator's host environment and filesystem are never scanned, preventing credential leakage into hub-dispatched agents.
- **Isolation**: Agent home directories and non-git project data are isolated on the host filesystem and externalized from the workspace to prevent cross-agent data leakage and unauthorized traversal.
- **Shadow Mounts**: Scion uses `tmpfs` shadow mounts to definitively block agents from accessing `.scion` configuration data or other agents' workspaces within the same project.
- **Lifecycle**: Secrets exist only in the agent container's memory or transient mounts. When an agent is deleted, all projected secrets and transient volumes are purged.

### 4.6 Hub-Internal Keys

JWT signing keys used for agent and user token issuance are stored through the secret backend when GCP Secret Manager is configured. In development mode (local backend), signing keys fall back to direct database storage with a logged warning. These keys use the internal `hub` scope and are not accessible through the user-facing secrets API.

### 4.7 Broker Authentication Secrets

The following broker-related secrets are stored in the Hub database and are not managed through the secrets backend:

- **Join tokens**: SHA-256 hashed before storage; single-use with 1-hour expiry.
- **Shared secrets**: Stored as binary BLOBs in the `broker_secrets` table; used for HMAC-SHA256 request signing.

These are infrastructure-level secrets established during broker registration and are managed by the broker authentication subsystem rather than the user-facing secrets API.

## 5. Development Security

To facilitate local development, Scion provides a **Development Authentication** mode.
- **Dev Token**: A persistent token starting with `scion_dev_` stored in `~/.scion/dev-token`.
- **Constraints**: Dev mode is disabled by default and requires `localhost` binding if TLS is not used.
- **Warning**: The server logs clear warnings when operating in Dev Mode.
