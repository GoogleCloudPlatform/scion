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
| **Development** | Any | Developer Token (Bearer) | `~/.scion/dev-token` |

### 1.2 User Authentication (OAuth 2.0)

For both Web and CLI access, Scion relies on standard OAuth 2.0 providers (Google and GitHub).

- **Web Flow**: Standard Authorization Code flow. The embedded Go web server handles the callback and exchanges the provider token for a session-bound Hub access token.
- **CLI Flow**: Uses a localhost callback server (defaulting to port `18271`). The CLI opens the user's browser for authentication and receives the authorization code via the local server.
- **PKCE**: The CLI uses Proof Key for Code Exchange (PKCE) to prevent authorization code injection attacks.
- **Per-User Session Revocation**: Each user carries a `session_generation` counter. Administrators can increment this counter via the API (`POST /api/v1/users/:id/revoke-sessions`) or the Web Dashboard, immediately invalidating all of the user's active cookie-based sessions. The Hub middleware checks the counter on every request and forces re-authentication on mismatch. See [Authentication & Identity — Session Revocation](/scion/hosted/single-node/auth/#session-revocation).

### 1.3 Agent Authentication (`sciontool`)

Agents running inside containers must report status back to the Hub without possessing user-level credentials.

- **Hub-Issued JWT**: During provisioning, the Hub generates a short-lived JWT scoped specifically to that agent instance.
- **Claims**: The token includes the `agent_id` (sub), `project_id`, and `scopes`.

  :::caution[Scopes Field Plurality]
  The agent token JWT strictly uses `"scopes"` (plural, array of strings like `["project:read", "agent:status:update"]`), **NOT** `"scope"` (singular string, which is the standard OAuth 2.0 convention). 
  
  Providing the singular `"scope"` field in a token will silently fail, resulting in an empty scopes list and a locked-down agent.
  :::

- **Role-Based Scopes**: Instead of raw template scopes (which are deprecated), an agent's scopes are governed by its assigned **Tiered Agent Role** (`none`, `readonly`, `baseline`, or `full`). An empty or unspecified agent role is securely enforced to resolve to the least-privilege role (`AgentRoleNone`) across all authorization paths. Scheduled dispatch children automatically persist this explicit role, and dispatches lacking a creator are refused.
    - `project:read` (Readonly): Allows reading project state (agents, templates, etc.).
    - `agent:status:update`, `agent:token:refresh`, `project:agent:notify`, `agent:port:forward` (Baseline): Standard operational scopes allowing the agent to report progress, refresh its token, and hold port tunnels.
    - `project:agent:create`, `project:agent:lifecycle`, `project:secret:read`, `project:template:write` (Full): Complete programmatic control allowing the agent to spawn sub-agents, manage their phases, retrieve project secrets dynamically, and create or update templates within the project.
- **Transmission**: The token is injected into the container via the `SCION_HUB_TOKEN` environment variable and is used by `sciontool` for all API calls.

### 1.4 Runtime Broker Authentication (HMAC)

Runtime Brokers represent high-trust infrastructure. They use HMAC-based request signing for bidirectional authentication with the Hub.

- **Shared Secret**: Established during initial registration via a short-lived `joinToken`.
- **Signing & Verification**: Every request includes headers for `X-Scion-Broker-ID`, `X-Scion-Timestamp`, `X-Scion-Nonce`, and `X-Scion-Signature`. The Hub strictly verifies that the authenticated caller identity matches any target broker paths to prevent cross-tenant escalation.
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

### 3.2 Permissions and Policy Model

Scion implements a robust, hierarchical RBAC (Role-Based Access Control) and policy system. For a detailed technical specification of the policy language and agent identity claims, see the [Policy & Permissions Reference](/scion/reference/permissions-policy/) and [Permissions & Policy Guide](/scion/hosted/ha/permissions/).

- **Principal-Based**: Permissions are granted to **Users** and **Groups**.
- **Hierarchical Groups**: Groups can contain other groups, allowing for complex team structures.
- **Resource Scopes**: Policies are attached to scopes (Hub, Project, or specific Resource) and follow a containment hierarchy.
- **Override Model**: Lower-level policies (e.g., at the Agent level) override higher-level ones (e.g., at the Project level), allowing for granular delegation of authority.
- **Actions**: Standardized CRUD actions (`create`, `read`, `update`, `delete`, `list`) plus resource-specific actions (`start`, `stop`, `attach`, `message`).
- **Tiered Agent Authorization**: Agents are assigned tiered roles (`none`, `readonly`, `baseline`, `full`) that restrict their JWT scopes through project and parent-agent creation ceilings plus live delegation checks.

### 3.3 GCP Service Account Assignment Gates

To prevent lateral privilege escalation, Scion implements a strict two-layer delegation check when binding a GCP service account to any agent:
- **Layer 1: Scion Hub Policy**: The Hub's policy engine checks if the caller holds the `ActionAssign` permission on the target GCP service account resource within Scion.
- **Layer 2: GCP IAM (`actAs`)**: When `gcp_iam_check_mode` is set to `"enforce"`, the Hub performs an out-of-band call via Google's **Policy Troubleshooter v3 API** to verify that the caller's GCP principal possesses `iam.serviceAccounts.actAs` permission on the target service account.

Key security attributes of the GCP IAM check include:
- **Fail-Closed Design**: If the Policy Troubleshooter returns an indeterminate result (due to conditional bindings, or due to insufficient Hub reviewer permissions when configured to fail-closed), the check fails closed and assignment is blocked. There is no fallback to insecure alternatives like getIamPolicy.
- **Asymmetric Caching**: Approved assignments are cached for **60 seconds**, and denials are cached for **10 seconds**. Indeterminate or error states are never cached.
- **Auditing**: Every service account assignment check—both allowed and denied—generates a permanent audit log entry detailing the principal, target service account, and Policy Troubleshooter decision.
- **Hub-Scoped SAs**: Real hub-scoped service accounts can be assigned across projects. However, to prevent privilege bypasses, hub-scoped assignments are immediately rejected if `gcp_iam_check_mode` is not set to `"enforce"`.

### 3.4 Fail-Closed API Authorization and Resource Isolation

To guarantee that no API endpoints or handlers can be accessed without explicit authorization, the Scion Hub enforces a strict **fail-closed** authorization design:

- **Explicit Fail-Closed Handlers (`s.authorize()`)**: All API handlers route through fail-closed authorization checks (`s.authorize()`). This eliminates legacy fail-open bypass vectors (such as functions relying on `GetUserIdentityFromContext` which could return `nil` for agent or broker callers and silently bypass authorization). Under the fail-closed model, any context lacking a valid user identity, agent token, or broker credentials is automatically denied.
- **Fail-Closed Dispatch Access**: The `checkBrokerDispatchAccess` guard is strictly fail-closed, ensuring that no agent execution can be triggered on a runtime broker unless dispatch permissions have been verified.
- **Role Boundary Enforcement (`addGroupMember`)**: Non-user callers (such as automated agents or system services) are strictly capped at the plain `member` role when executing `addGroupMember` operations, preventing elevation of privileges across organizational boundaries.
- **Strict Isolation Ordering (404-before-403)**: To prevent unauthorized users or agents from discovering the existence of sensitive resources via API probe responses, Scion enforces strict **resource isolation ordering**. If a caller requests a resource they are not authorized to view, the Hub performs resource existence checks and tenant bounds validation first. This ensures the Hub responds with a `404 Not Found` rather than a `403 Forbidden` if the resource does not exist or belongs to another tenant/project, preventing side-channel resource enumeration.
- **Dispatcher-Level Route Authorization**: Route families that share a resource are authorized once, in a single dispatcher, before any handler runs:
  - **Project workspace routes** (files, archive, pull, cache, sync status, WebDAV, and the legacy `groves` alias) check project access first. Any non-read method (for example `PUT`, `POST`, `DELETE`) requires update access on the project.
  - **Template file routes** check access on the specific template before any read or write, and validate file paths with the same rules as the workspace file handlers.
  - **Agent status updates**: an agent can update only its own status. Any non-agent caller needs update access on the agent.
  - **Harness config routes** grant Runtime Brokers read-only access.
  - **Project GitHub settings** require read access on the project for `GET`, and update access for any other method.
- **Chat Search Visibility**: Chat search returns DM threads only to their participants.
- **Project-Scoped Agent Deletion**: When a Runtime Broker deletes an agent, it resolves the agent within the requested project only, on every runtime. It never matches by bare slug across projects, so it cannot remove a same-slug agent's container, VM, or files in another project. The broker returns `404` when nothing matches and refuses the delete if the match is ambiguous.
- **Sanitized Broker Failure Reasons**: Before a message failure reason reported by a Runtime Broker is stored or echoed into the sending agent's terminal, the Hub strips control characters and invalid UTF-8 and truncates it to 512 bytes.
- **Regression Checks in CI**: To prevent future authorization regressions, an automated `authz-guard` check is wired into the CI pipeline (via a dedicated Makefile target and GitHub Actions step) that statically analyzes and validates that all API handlers are protected by appropriate authorization helpers.

### 3.5 Project File Access Containment

The Hub's project file handlers serve project workspaces and shared directories. These cover list, download, archive, upload, inline write, and delete. Each handler is confined to the directory it serves. The contents of these directories are agent-writable by design: a workspace is a git checkout, and a shared directory is mounted read-write into every agent in the project. A symlink planted inside one must therefore not expose files elsewhere on the host.

- **Kernel-enforced containment**: File operations go through Go's `os.Root`, which checks containment at every path component. A path that resolves outside the served directory, including through a symlink stored inside it, is refused with `400 File not accessible` and a warning is logged. A symlink whose target is outside returns the same response whether or not the target exists, so it cannot be used to probe the host.
- **In-tree symlinks**: A symlink that resolves to another location inside the same directory is followed normally.
- **Symlinked base directories**: If the served directory itself is a symlink, the request is refused.
- **Archives**: Directory archive downloads skip symlinks entirely.
- **Deletes**: Deleting a symlink removes the link only, never its target.
- **No implicit creation**: Read and delete requests on a missing workspace or shared directory no longer create it. Only uploads and writes do.
- **Attachment ingest and staging**: Attachments are resolved through an `os.Root` anchored on the project scratchpad, so a symlink at any intermediate directory component cannot redirect them. A shared directory that is itself a symlink is refused.
- **NFS shared directories**: With `server.shared_dir_storage.backend: nfs`, shared-directory operations use an `O_NOFOLLOW` component walk anchored on the project tree's inode. See [Shared Directory Storage](/scion/reference/server-config/#shared-directory-storage-servershared_dir_storage).

## 4. Secret Management

Scion provides a typed, scope-aware secret management system. Secret values are never stored in plaintext in the Hub database. For a user-facing guide, see [Secret Management](/scion/hosted/user/secrets/).

### 4.1 Secrets Backend Architecture

The Hub uses a pluggable **SecretBackend** interface for secret storage:

| Backend | Value Storage | Write Operations | Read Operations |
|---------|--------------|-----------------|-----------------|
| **`gcpsm`** (GCP Secret Manager) | Encrypted in GCP SM | Supported | Supported |
| **`local`** (default) | Encrypted at rest (AES-256-GCM) in Hub DB | Supported | Supported |

When `gcpsm` is configured, a hybrid model is used:
- **Metadata** (name, type, scope, version) is stored in the Hub database.
- **Secret values** are stored in GCP Secret Manager with automatic versioning.
- GCP SM secret names follow the pattern: `scion-{scope}-{sha256(scopeID)[:12]}-{name}`.

The `local` backend now encrypts secret values at rest using AES-256-GCM, with domain-separated key derivation from the hub signing secret. Legacy plaintext values are transparently readable and will be automatically re-encrypted upon the next write operation.

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

### 4.4 User Access Tokens (UATs)

For headless environments (CI/CD, automation), Scion supports **user access tokens (UATs)**.
- Tokens are prefixed with `scion_pat_` (a legacy artifact of the older "personal access token" name).
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

**Skill download signing key.** Hubs using local storage for the skill registry sign skill file download URLs with HMAC-SHA256. Cloud storage issues presigned object-store URLs and is unaffected. Each URL is a capability for exactly one file of one skill version (`?version=…&exp=…&sig=…`), valid for 15 minutes. The Hub signs it only after checking the creator's read access, and it lets a Runtime Broker download skill files at dispatch without a principal. The key follows the same persistence policy as the signing keys above. When stable keys are required (GCP Secret Manager backend), a failure to load it fails startup. Otherwise the Hub falls back to an in-memory key, which only invalidates URLs that are still outstanding.

### 4.7 Broker Authentication Secrets

The following broker-related secrets are stored in the Hub database and are not managed through the secrets backend:

- **Join tokens**: SHA-256 hashed before storage; single-use with 1-hour expiry.
- **Shared secrets**: Stored as binary BLOBs in the `broker_secrets` table; used for HMAC-SHA256 request signing.

These are infrastructure-level secrets established during broker registration and are managed by the broker authentication subsystem rather than the user-facing secrets API.

## 5. Development Security

To facilitate local development, Scion provides a **Development Authentication** mode.
- **Developer Token**: A persistent token starting with `scion_dev_` stored in `~/.scion/dev-token`.
- **Constraints & Safeguards**: Dev mode is disabled by default. Startup validation strictly prohibits binding the `devAuthMiddleware` to non-loopback interfaces (e.g., `0.0.0.0`). Because this middleware automatically logs in every cookieless request as an admin, refusing it on public interfaces prevents the accidental exposure of an unauthenticated admin UI.
- **Warning**: The server logs clear warnings when operating in Dev Mode.
