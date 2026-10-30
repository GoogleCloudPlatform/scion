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
| **Runtime Broker** | Compute Node | HMAC Signature | `~/.scion/hub-credentials/<name>.json` |
| **Development** | Loopback Hub only | Developer Token (Bearer) | `~/.scion/dev-token` |

### 1.2 User Authentication (OAuth 2.0)

For both Web and CLI access, Scion relies on standard OAuth 2.0 providers (Google and GitHub).

- **Web Flow**: Standard Authorization Code flow. The embedded Go web server handles the callback and exchanges the provider token for a session-bound Hub access token.
- **CLI Flow**: Uses a localhost callback server (defaulting to port `18271`). The CLI opens the user's browser for authentication and receives the authorization code via the local server.
- **PKCE**: The CLI uses Proof Key for Code Exchange (PKCE) to prevent authorization code injection attacks.
- **Per-User Session Revocation**: Each user carries a `session_generation` counter. Administrators can increment this counter via the API (`POST /api/v1/users/:id/revoke-sessions`) or the Web Dashboard, immediately invalidating all of the user's active cookie-based sessions. The Hub middleware checks the counter on every request and forces re-authentication on mismatch. See [Authentication & Identity — Session Revocation](/scion/hosted/single-node/auth/#session-revocation).

### 1.3 Agent Authentication (`sciontool`)

Agents running inside containers must report status back to the Hub without possessing user-level credentials.

- **Hub-Issued JWT**: During provisioning, the Hub generates a short-lived JWT scoped specifically to that agent instance. When `server.keyring_file` is configured (or `server.jwt.emit_version` is `"keyring"`), the Hub signs agent and user JWTs with the active generation of the keyring's signing ring (setting the JWT protected `kid` header when `emit` is `"versioned"` or `server.jwt.emit_version` is `"keyring"`) and verifies tokens by `kid` against the keyring while still accepting unversioned tokens from `signing.legacy_read_ids` / `server.jwt.accept_legacy` (see [`server.keyring_file`](/scion/reference/server-config/#environment-variables)).
- **Claims**: The token includes the `agent_id` (sub), `project_id`, `scopes`, and the `run_id` of the agent run it was issued for. With [`server.auth.agent_run_scope`](/scion/reference/server-config/#authentication-serverauth) set to `observe`, the Hub compares `run_id` with the agent's current run and logs mismatches, such as a token from an earlier run; it does not refuse requests on this basis yet. The agent's current run changes only when the Hub dispatches a create, start or restart, when the Runtime Broker reports the run it launched, when a failed dispatch or move is reverted, or when a Runtime Broker heartbeat reports the agent's container carrying another run the Hub issued a token for while no container holds the recorded run (logged as `agent_run_adopted_from_heartbeat`).

  :::caution[Scopes Field Plurality]
  The agent token JWT strictly uses `"scopes"` (plural, array of strings like `["project:read", "agent:status:update"]`), **NOT** `"scope"` (singular string, which is the standard OAuth 2.0 convention). 
  
  Providing the singular `"scope"` field in a token will silently fail, resulting in an empty scopes list and a locked-down agent.
  :::

- **Role-Based Scopes**: Instead of raw template scopes (which are deprecated), an agent's scopes are governed by its assigned **Tiered Agent Role** (`none`, `readonly`, `baseline`, or `full`). An empty or unspecified agent role is securely enforced to resolve to the least-privilege role (`AgentRoleNone`) across all authorization paths. Scheduled dispatch children automatically persist this explicit role, and dispatches lacking a creator are refused.
    - `project:read` (Readonly): Allows reading project state (agents, templates, etc.).
    - `project:artifact:read` (Readonly): Allows the agent to use the artifact service to read artifacts (`artifact.read`) when the `hub.artifacts` experiment is on; which artifacts it can read is decided by its project and by artifact grants. Artifact access uses dedicated agent scopes. Agents created from a credential issued before artifacts existed, and some agents created before then, do not carry it; refreshing such an agent's token is not enough. A Hub super-admin can bring it up to date with `scion reset-auth <agent> --reissue-scopes` (see [Reset Auth and Scope Re-issue](/scion/reference/reset-auth/)); an agent created from an access token issued before artifacts existed must be recreated from a current credential.
    - `project:artifact:write` (Baseline): Allows the agent to use the artifact service to publish artifacts (`artifact.create`) when the `hub.artifacts` experiment is on; where it can publish is decided by its project. Readonly agents do not get it. Like the read scope, some agents created before artifacts existed do not carry it; re-issue their scopes with `scion reset-auth <agent> --reissue-scopes`, or recreate the agent from a current credential.
    - `agent:status:update`, `agent:token:refresh`, `project:agent:notify`, `agent:port:forward` (Baseline): Standard operational scopes allowing the agent to report progress, refresh its token, and hold port tunnels.
    - `project:agent:create`, `project:agent:sa_assign`, `project:agent:lifecycle`, `project:secret:read`, `project:template:write` (Full): Complete programmatic control allowing the agent to spawn sub-agents, assign GCP service accounts to agents, manage their phases, retrieve project secrets dynamically, and create or update templates within the project.
- **Transmission**: In Runtime Broker mode, the Runtime Broker injects only the Hub-issued token (`StartOptions.HubIssuedToken`: the create request's `agentToken`, or on start/restart the resolved environment's `SCION_AUTH_TOKEN` when the Hub classified it `secret-bootstrap`) into the container (via `~/.scion/scion-token` and `SCION_AUTH_TOKEN` / `SCION_HUB_TOKEN`) and never falls back to its own `SCION_AUTH_TOKEN` or `SCION_DEV_TOKEN`. The Runtime Broker deletes `SCION_AUTH_TOKEN` from the merged start environment (`opts.Env`), and `buildAgentEnv` drops any `SCION_AUTH_TOKEN` set in the agent configuration or harness-config environment with a start warning (`SCION_AUTH_TOKEN is set in the agent's environment configuration and is ignored: the agent's hub credential is only the token the hub issues for it`). When a Hub-dispatched start or restart has no Hub-issued token, the agent manager removes any previous `~/.scion/scion-token` file from the Agent home and fails the start with `500 runtime_error` (`the hub issued no agent token for this start; check the hub's agent token service`), starting no container; on a restart, the running container is stopped first, so a restart without a Hub-issued token leaves the agent stopped.

### 1.4 Runtime Broker Authentication (HMAC)

Runtime Brokers represent high-trust infrastructure. They use HMAC-based request signing for bidirectional authentication with the Hub.

- **Shared Secret**: Established during initial registration via a short-lived, single-use `joinToken`. The token can be redeemed on the same machine (`scion runtime-broker register`) or created on one machine and redeemed on another (`scion hub brokers join-token create`, then `scion runtime-broker join`), so the broker host needs no Hub user credential.
- **Signing & Verification**: Every request includes headers for `X-Scion-Broker-ID`, `X-Scion-Timestamp`, `X-Scion-Nonce`, and `X-Scion-Signature`. The Hub strictly verifies that the authenticated caller identity matches any target broker paths to prevent cross-tenant escalation.
- **Body Coverage**: The signature covers the SHA-256 of the request body whenever the body has at least one byte, whatever `Content-Length` says. An empty body adds no hash (on HTTP/1.1, a request with neither `Content-Length` nor `Transfer-Encoding` has an empty body and is accepted), so bodyless `GET`, `DELETE` and `POST` requests are signed as before. The signer (`apiclient.HMACAuth`) reads a body of unknown length into memory before signing and sends it with a `Content-Length`. The Hub and Runtime Broker verifiers refuse a signed request whose body does not declare its length (`Transfer-Encoding: chunked`, or an HTTP/2 body sent without `Content-Length`) with `401` and the message `signed request body must declare its length`.
  - *Upgrade note:* Hub and Runtime Broker versions can be upgraded in either order; every request the in-tree signers send verifies on both old and new versions. An out-of-tree client that signs with an older `apiclient.HMACAuth` and streams a body without a `Content-Length` gets `401` from an upgraded Hub or Runtime Broker; rebuild it against the current `apiclient`. A proxy in front of the Hub or a Runtime Broker must forward signed requests with their `Content-Length` (Go's `httputil.ReverseProxy`, used by `extras/cloudrun-iap-proxy`, does). If a proxy in front of the Hub or a Runtime Broker re-chunks request bodies, report it: the length check is defence in depth and can be dropped in code without losing body coverage, because the body hash alone still makes an added or changed body fail the signature.
- **Body size limit**: The Hub and the Runtime Broker hash a signed request's body before they check the signature, so both refuse a signed request whose body is larger than 2 MiB. The response is `413` with error code `payload_too_large`, and the body is not read. No broker-signed route carries file contents (workspaces move through object storage, and attachments, templates and harness configs are downloads), so the limit does not affect normal traffic. On the Hub, the broker authentication audit event records the refusal with failure code `malformed`, and the legacy event's failure reason reads `signed request body exceeds the 2 MiB limit`.
- **Replay Protection**: Every signed request must carry an `X-Scion-Timestamp` within ±5 minutes and a non-empty `X-Scion-Nonce` header (`401 missing X-Scion-Nonce header` when absent; every in-tree signer sends a random nonce). Both the Hub and the Runtime Broker's HMAC verification middlewares (and the Conduit RPC guard) record each verified signature's nonce only after the signature check succeeds (so a request with an invalid signature or an over-limit body leaves its nonce unused), cache it until `timestamp + maxClockSkew`, and refuse any replayed request within that window with `401 request already served`. In external Conduit relay mode (`server.hub.conduit.relay_mode: external`), a Runtime Broker's `GET /api/v1/conduit` dial to the Hub's Conduit ingress is verified there without consuming its nonce (`nonce_reason: broker_nonce_deferred_to_admit` on the ingress audit event) so that the separate relay's `Admit` call records the nonce once.
- **NAT Traversal**: Brokers establish a persistent WebSocket control channel. The initial upgrade request is HMAC-authenticated, establishing a trusted session for subsequent commands.
- **Before Keys Are Loaded**: A Runtime Broker that has no HMAC keys yet (pending registration, or a credential that failed to load) serves only `/healthz` and `/readyz`. Every other request, including agent create and control, is answered with `503` and the error code `broker_auth_unavailable` until the broker loads its HMAC keys (after registration, or after a restart with a working secret or credential). A broker without keys starts only on a loopback address.
- **Browser Origins**: The Runtime Broker sends no CORS headers by default, so browsers do not expose broker responses to cross-origin pages and refuse cross-origin requests that need a preflight; the requests themselves are authorized by broker authentication. See [`server.broker.cors`](/scion/reference/server-config/#broker-cors-serverbrokercors) to allow specific origins.

### 1.5 Broker Registration and Association Credentials

User-credentialed Runtime Broker operations admit a fixed set of credential kinds. A Runtime Broker HMAC request that names a user on whose behalf it acts is **not** a user credential for registering, re-registering, rotating, associating a Runtime Broker with a project or removing an association; Runtime Broker HMAC on its own is admitted only for the Runtime Broker's self-maintenance (heartbeat, control channel, rotating its own secret, reading and writing its own env vars and secrets, and reporting launches and message-delivery failures).

| Operation | Endpoint | Admitted credentials | Additional checks |
|---|---|---|---|
| Register a new broker | `POST /api/v1/brokers` | Interactive session, dev credential, hub-boundary UAT with `broker:create` | The user must hold `broker.create` (hub members). The caller's user becomes the owner. Turning on auto-provide also requires `broker.auto_provide` (super-admins). |
| Re-register (re-mint the join token) | `POST /api/v1/brokers` matching an existing broker | Same as registration | The user must also be the broker's owner, or a super-admin with an interactive session or dev credential. |
| Rotate the HMAC secret | `POST /api/v1/brokers/{id}/rotate-secret` | Broker HMAC for its own ID; interactive session or dev credential | A user must be the broker's owner or a super-admin. No UAT is admitted. |
| Associate with a project | `POST /api/v1/projects/{id}/providers`, `brokerId` in `POST /api/v1/projects/register`, explicit broker on agent create | Interactive session, dev credential, or UAT for the project side | `project.update` on the project **and** `broker.update` on the broker (owner or super-admin). No UAT selector carries `broker.update`. |
| Remove an association | `DELETE /api/v1/projects/{id}/providers/{brokerId}` | Interactive session, dev credential, UAT | `project.update` on the project, or `broker.update` on that broker. |
| Use an associated broker | Agent creation | As for agent creation | Allowed for auto-provide brokers, for brokers associated with the project with the owner's consent, and for holders of `broker.dispatch` on the broker. |
| Read env vars and secret metadata | Runtime Broker env and secret routes (read) | Runtime Broker HMAC for its own ID; interactive session, dev credential, hub-boundary UAT with `broker:read` | `broker.read` on the Runtime Broker, plus the owner relationship or `broker.update`. Sensitive values are masked. |
| Write env vars or secrets, update, deregister | Runtime Broker env and secret routes (write), Runtime Broker update and deregistration | Runtime Broker HMAC for its own env vars and secrets (not update or deregistration); interactive session or dev credential | `broker.update` on the Runtime Broker (`broker.delete` to deregister). No UAT is admitted. |
| Send a heartbeat as a user | Runtime Broker heartbeat | Runtime Broker HMAC for its own ID; interactive session or dev credential | `broker.update` on the Runtime Broker from a role binding; the owner relationship does not apply. |
| Pull or remove an image on a named Runtime Broker | Harness-config image operations | As for the operation | A user holding `broker.dispatch` on that Runtime Broker is admitted on a session or dev credential. A user access token is always refused, even on an auto-provide Runtime Broker, because no token scope carries `broker.dispatch`. Without `broker.dispatch`, a session or dev credential is admitted only on an auto-provide Runtime Broker; association with the project does not help. |

Registration never associates a broker with a project, and a project's default runtime broker must already be one of its providers. Register, re-register, rotate, link and unlink audit events record the credential kind and credential ID (a token ID or broker ID; never a secret) alongside the user.

## 2. Transport Security

### 2.1 TLS and HTTPS Enforcement

In production mode, Scion mandates the use of TLS for all network traffic.

- **HTTPS Enforcement**: The Hub server rejects non-HTTPS requests (unless configured for local development or behind a trusted TLS-terminating proxy).
- **Conduit Internal Hop TLS**: When the Hub runs an internal Conduit listener (`server.hub.conduit.internal_listen`) or a separate Conduit relay connects to the Hub (`server.relay.hub_internal_url`), `internal_advertise`, `hub_internal_url`, and every peer Hub internal URL must use `https://` (`server.hub.conduit.internal_tls_cert_file` and `internal_tls_key_file`, plus `server.hub.conduit.internal_tls_ca_file` for a private or internal CA, on every hub and relay node) unless the host is a loopback IP literal (`127.0.0.0/8` or `::1`; `localhost` does not count as loopback). Internal hop HTTP clients also refuse non-loopback `http://` URLs and do not follow HTTP redirects. See [Separate Conduit Relays — Upgrading](/scion/hosted/ha/separate-relay/#upgrading) and [`server.hub.conduit`](/scion/reference/server-config/#conduit-serverhubconduit).
- **Security Headers**: Standard headers such as `Strict-Transport-Security` (HSTS), `X-Frame-Options`, and `Content-Security-Policy` are enforced.
- **mTLS (Future)**: Support for Mutual TLS between Hub and Runtime Brokers is planned for high-security environments.

### 2.2 WebSocket Security

- **CLI/Agents**: Use standard `Authorization` headers.
- **Browser/Web**: Since browser WebSocket APIs cannot set custom headers, Scion uses a **Ticket-Based Authentication** system. The client requests a short-lived, single-use ticket via a POST request (authenticated by cookie) and provides it in the WebSocket query string (`?ticket=...`).

### 2.3 Hub Credential Origin Binding

`apiclient` and `transportauth` attach Hub credentials (`Authorization`, `X-Scion-*` headers, and transport credentials such as Cloud Run ID tokens) only to requests matching the configured Hub origin (scheme, lowercase host, and effective port), and strip `Authorization`, `Proxy-Authorization`, `X-Serverless-Authorization` and `X-Scion-*` headers when an HTTP redirect crosses to any other origin. When `hubclient` downloads an artifact file through a `3xx` redirect to object storage (`OpenFile`), it requires an `https://` redirect URL with a non-empty host (unless the Hub itself is configured over plain `http://` for local development) and refuses any further redirect from the object store. Remote template fetches likewise copy only regular files and directories, refusing symlinks and special files.

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
- **Tiered Agent Authorization**: Agents are assigned tiered roles (`none`, `readonly`, `baseline`, `full`) that restrict their JWT scopes through project and parent-agent creation ceilings plus live delegation checks. Each delegation edge also freezes the creating credential's permission ceiling and authority provenance at creation; the delegation walk applies every hop's frozen ceiling, and edges without recorded provenance are denied for sensitive-material permissions (see [Permissions](/scion/hosted/ha/permissions/#delegation-and-revocation)).
- **Delegation Provenance**: Delegation edges record the credential that authorized them and a frozen effect ceiling. Edges written before this was recorded are handled by a bounded adoption migration; see [Delegation Provenance Adoption](/scion/reference/delegation-provenance-adoption/).

### 3.3 GCP Service Account Assignment Gates

To prevent lateral privilege escalation, Scion implements a strict two-layer delegation check when binding a GCP service account to any agent, and re-evaluates both the calling agent's `gcp_service_account.use` authority and the recorded assigner's live `gcp_service_account.assign` authority (within the recorded permission ceiling; pre-provenance agents are backfilled on upgrade by `sa_assignment_legacy_backfill`, see [Token-Request Authority](/scion/hosted/ha/permissions/#token-request-authority)) on every token mint (`POST /api/v1/agent/gcp-token`, `POST /api/v1/agent/gcp-identity-token`, and chat `gs://` object fetches):
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
- **No Fallback When Authorization Is Unavailable**: The Hub refuses to start if the route metadata for session-only routes is incomplete. A Hub-admin route that declares a permission is evaluated only through the authorization service; if none is configured, the route answers `500` "authorization unavailable" instead of falling back to a plain admin-role check.
- **Fail-Closed Dispatch Access**: The `checkBrokerDispatchAccess` guard is strictly fail-closed, ensuring that no agent execution can be triggered on a runtime broker unless dispatch permissions have been verified.
- **Role Boundary Enforcement (`addGroupMember`)**: Non-user callers (such as automated agents or system services) are strictly capped at the plain `member` role when executing `addGroupMember` operations, preventing elevation of privileges across organizational boundaries.
- **Strict Isolation Ordering (404-before-403)**: To prevent unauthorized users or agents from discovering the existence of sensitive resources via API probe responses, Scion enforces strict **resource isolation ordering**. If a caller requests a resource they are not authorized to view, the Hub performs resource existence checks and tenant bounds validation first. This ensures the Hub responds with a `404 Not Found` rather than a `403 Forbidden` if the resource does not exist or belongs to another tenant/project, preventing side-channel resource enumeration.
- **Dispatcher-Level Route Authorization**: Route families that share a resource are authorized once, in a single dispatcher, before any handler runs:
  - **Project workspace routes** (files, archive, and pull) check project access first. Any non-read method (for example `PUT`, `POST`, `DELETE`) requires update access on the project.
  - **Template file routes** check access on the specific template before any read or write, and validate file paths with the same rules as the workspace file handlers.
  - **Agent status updates**: an agent can update only its own status. Any non-agent caller needs update access on the agent.
  - **Harness config routes** grant Runtime Brokers read-only access.
  - **Project GitHub settings** require read access on the project for `GET`, and update access for any other method.
- **Scope Boundary on Resource Reads**: User- and project-scoped skills, templates, and harness configs can be read only by their owner, members of the owning project, and Hub admins. Hub-wide member and viewer grants apply only to hub- and global-scoped records; they do not open up another user's or project's resources. The same scope check applies when a template is resolved at agent creation and when a template is cloned. There is no separate `visibility` setting on these resources, so access depends only on scope and grants.
- **Project Agent Routes**: Project-scoped agent list, get, and update, and the resume and restart path, authorize each caller individually. Unless the caller holds `agent.attach` on the agent (an agent delegated credential never qualifies), agent responses leave out the environment, MCP server env and headers, service env and telemetry export headers in the agent's applied config, create inputs, `pendingConfig`, and `effectiveNextRun` (the entries themselves stay listed); `GITHUB_TOKEN` is never returned.
- **Chat Search Visibility**: Chat search returns DM threads only to their participants.
- **Project Authority From Role Bindings Only**: A project's `ownerId` field grants no access. Project authority, including project stop-all, comes only from project-scoped role bindings, so a removed creator whose only claim is `ownerId` is denied. See [Permissions & Policy](/scion/hosted/ha/permissions/#roles-and-bindings).
- **Sandboxed Port-Forward Responses**: Responses proxied from an agent's exposed port always carry the Hub's sandbox `Content-Security-Policy`, its untrusted-content `Permissions-Policy`, and `X-Content-Type-Options: nosniff`, and the agent's `Set-Cookie`, CSP, and CORS headers are never relayed onto the Hub origin. The agent's `X-Frame-Options`, `X-XSS-Protection`, `Referrer-Policy` and `Permissions-Policy` headers are not relayed either; when the web frontend serves the Hub, its own values for the first three apply, and each response carries a single `Permissions-Policy`, the untrusted-content one. See [Port Forwarding](/scion/hosted/user/port-forwarding/).
- **Project-Scoped Agent Deletion**: When a Runtime Broker deletes an agent, it resolves the agent within the requested project only, on every runtime. It never matches by bare slug across projects, so it cannot remove a same-slug agent's container, VM, or files in another project. The broker returns `404` when nothing matches and refuses the delete if the match is ambiguous.
- **Agent Name Path Validation**: Wherever an agent name resolves to on-disk agent state, it must be a single, clean path element. Empty names, `.`, `..`, and names containing `/`, `\`, or NUL are rejected, so a name cannot escape the agents directory. Broker create and start requests that fail this check return a validation error. Broker dispatch and scheduled-event lookups address agents by slug.
- **Sanitized Broker Failure Reasons**: Before a message failure reason reported by a Runtime Broker is stored or echoed into the sending agent's terminal, the Hub strips control characters and invalid UTF-8 and truncates it to 512 bytes.
- **Sanitized Failure Logs**: When the Runtime Broker logs why an agent create, asynchronous launch, start or restart failed, or why a provision failed because its template or harness config was not found, it blanks `KEY=VALUE` values, the values of credential-named keys and credential-shaped strings (authorization credentials, JWTs, URL userinfo, PEM blocks, long encoded runs), and cuts the sanitized text at 1 KiB, adding a marker with its full length. A failure caused by a required skill is the exception: the failure record carries only the skill's user-visible text and a correlation ID, and separate records carry the skill failure text with its URLs redacted, or with the resolver or install message withheld when the skill reference itself had to be redacted. The error returned to the client is unchanged.
- **Artifact Stewards**: On an [artifact](/scion/reference/artifacts/#stewards) owned by an agent, the owners of its home project (the `artifact.steward` permission, carried only by the built-in project-owner role and super-admins) and the agent's delegating user (the root of its live, recorded delegation chain, who must also read in the home project through a role) may share it. The authority is computed on every request from the artifact's current home and owner, never from grants on the artifact, and it is **non-amplifying**: a steward may give at most write, never an admin grant, and may not move the artifact (`403 steward_action_not_allowed`), so no stored grant it creates can carry more power than it holds. Artifacts owned by users have no stewards, and agents are never stewards (`artifact.steward` has no agent token scope). A user access token must carry `artifact:steward` (owner clause) or `artifact:read` (delegator clause) besides `artifact:manage`, and reach the home project; older tokens fail closed. Each steward write is checked again, under the artifact's lock, against the home and owner it was decided on, so a share that races a move or an ownership transfer answers `404` and commits nothing. Grant creation, permission changes and share-link creation are recorded in the artifact's event table with the actor and the steward clause, never the link token. Grants a steward created outlive the steward's role, like any grant; any current owner, admin grantee or steward can remove them.
- **Artifact Review Access**: On an [artifact](/scion/reference/artifacts/#grants) owned by an agent, callers who are not its [stewards](/scion/reference/artifacts/#stewards), such as an admin of its home project who is not an owner (`project.manage` there) and the agent's delegating user when they hold no role in the home project, may give a user review access without being able to manage the artifact. Review access is a **review** grant: it lets the user publish versions of kind review and nothing else, and it gives no read access of its own. It is given only to a user who can already read the artifact through the home project, or a grant to one of their projects, and it never replaces a grant the user already holds. The hub then asks again on every review request whether the user can still read it that way. When that access ends, review access ends with it, and the artifact answers `404` like a missing one. A refused grant request gets the same `403` with one fixed message, with one exception: a caller who holds this authority and asks for a review grant to a user gets `403` with code `review_subject_cannot_read` when that user cannot read the artifact, and `403` with code `review_replaces_access` when the user already holds a read, write or admin grant. Review access given before the review level existed was a write grant, and those grants were not changed.
- **Artifact Deletion**: Deleting an [artifact](/scion/reference/artifacts/#deletion) (`DELETE /api/v1/artifacts/{id}`, behind the `hub.artifacts` experiment) is allowed only for users: its owner, a user with an admin grant on it, a [steward](/scion/reference/artifacts/#stewards) of an artifact owned by an agent, or an admin of its current home project (`project.manage` there: the project owners and admins of the artifact's home project, and super-admins; hub-admins do not hold it). The caller's credential must also allow `artifact.manage` in that project, so a user access token stays within its boundary and permission ceiling. Agents can never delete artifacts. The read check runs first: a caller that cannot read the artifact gets the same `404` as for a missing one, and a reader without the right gets `403` with one fixed message, so the answer reveals neither existence nor the reason. A delete is a soft delete that also removes every grant and share link in the same transaction; it, and every expiry change, move, grant creation, change and removal, and share-link creation and revoke, is recorded in the artifact's own append-only event table with the actor and its authority. A periodic owner check marks artifacts whose owning agent or user is gone (`ownerDeleted`); the mark gives nobody any access.
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
- **Chat attachments as artifacts**: With the `hub.attachment_artifacts` experiment on, a file uploaded in web chat is stored in the artifact service as an attachment of the message it is sent in. Whether a caller may read it is decided on every request by the artifact service's single read rule: the caller's credential must allow reading messages, and the caller must be able to read the message now (a participant of its conversation, a reader of its project for a project thread, or an addressee). Before it is sent, only the uploader can read it. A user access token reads an attachment stored as an artifact only when every clause of its rule holds: it carries `inbox:read`, eligible for its boundary; its holder may read the message; the project recorded for the attachment when it was sent is known and within the token's boundary (no project needs a hub boundary); and, in a direct message with an agent, the token passes `agent:read` on every agent in the conversation, whichever participant its holder is. Its holder's unsent upload needs a hub boundary. A token uploads and names attachments only where it may post, with `inbox:write`. Agents read attachments stored as artifacts only through delivery (below): an agent in good standing that may read the message, as a participant, a reader of its project or an addressee, may receive it, and agents cannot use the download route. A caller who may not read an attachment gets the same answer as for an unknown id, on the download route and on every artifact route. Attachments cannot be shared, versioned or changed, are never listed, and are removed when their message is deleted. They are served like artifact files: `X-Content-Type-Options: nosniff`, `inline` only for safe image and text types, a sandboxing `Content-Security-Policy` when the Hub streams the bytes, and a short-lived signed URL on object storage.
- **Attachment delivery capabilities**: An attachment stored as an artifact reaches an agent as a file in its home (`~/.scion/inbox/<message-id>/`), written by the agent's Runtime Broker. For each dispatch of a message, the Hub asks the attachment read rule for the recipient agent, on a context that carries only the recipient's identity, and issues a capability only when it allows. A capability is signed with its own Hub key (HMAC-SHA256, separate from every other signing key) and names one attachment, message, agent and broker. It expires 15 minutes after it is issued and can be redeemed once. The broker redeems it at `GET /api/v1/attachment-deliveries/<token>` with its own HMAC credential. The Hub checks that the broker is the one named in the capability, verifies the signature before it reads the payload, checks the expiry, records the single use before it serves any byte, and checks that the message still exists and the attachment is still bound to it. Every refusal gets the same `404`. On object storage the file is served through a 60-second signed URL, which the broker fetches without its Hub credential. The broker checks each file's size and SHA-256 against the values the Hub sent, and writes the files as the agent user with one exec: the files go into a temporary directory with `umask 077`, a symlink on the inbox path is refused, and the directory is moved into place under an atomic claim. File names are reduced to `[A-Za-z0-9._-]` by the Hub. Old inbox directories are pruned without following symlinks.
- **Untrusted content isolation**: Workspace and shared-directory files are written by users and agents, not by the Hub. Responses that serve them, including `?view=true` previews and chat attachments, carry a `Content-Security-Policy: sandbox allow-scripts allow-popups allow-popups-to-escape-sandbox allow-downloads allow-modals; frame-ancestors 'none'` header, a `Permissions-Policy` that turns off camera, microphone, location, screen capture, motion sensors, USB/serial/HID/Bluetooth devices, payments, passkeys and clipboard reads, and `X-Content-Type-Options: nosniff`. `allow-modals` lets a report's Print button and `alert`/`confirm` dialogs work. These pages, and pages served from an agent's exposed port, can't be embedded in other pages or sites. The sandbox omits `allow-same-origin`, so a sandboxed document gets an opaque origin: `allow-scripts` lets its scripts run, so generated HTML reports keep working in the file browser's preview, but it cannot act with the viewer's Hub session.
- **Web page scripts**: Inline scripts on the web pages run only with a per-response script nonce. Each page response carries a fresh nonce for the one inline script of the app shell, and no other inline script, inline event handler or `javascript:` URL runs. Script files served from the hub's own address are also allowed. The pages also ask the browser to report, without blocking, strings passed to APIs that parse markup or script. Reports go to `POST /api/v1/csp-reports` and are written to the hub log; they are not persisted in application storage, and the log records follow your log retention. A report's sample, which can hold page content such as a chat message, is left out by default. See [Browser Policy Reports](/scion/reference/api/#browser-policy-reports-hub).
- **NFS shared directories**: With shared directories on NFS (`server.project_storage.backend: nfs`, or the deprecated `server.shared_dir_storage.backend: nfs`), shared-directory operations use an `O_NOFOLLOW` component walk anchored on the project tree's inode. See [Shared Directory Storage](/scion/reference/server-config/#shared-directory-storage-servershared_dir_storage).

### 3.6 Workspace Host-Path Validation and Agent Home Confinement

Before a Runtime Broker bind-mounts, syncs, uploads, or recursively `chown`s a workspace or agent-home host path, it resolves the path through any symlinks and validates the result. The check runs on every runtime and fails closed: a path that cannot be resolved, including when the home directory cannot be determined, is refused. The Runtime Broker then acts on the resolved path, not the original one. The following are always refused:

- The filesystem root (`/`, or a Windows volume root).
- Critical system directories.
- The user's home directory, or any ancestor of it.
- The scion home (`~/.scion`) or any ancestor of it, and anything under `~/.scion` except the subtrees that legitimately hold workspaces and agent homes, such as Hub-managed project workspaces under `~/.scion/projects/<slug>`.

Where the broker knows the project's root, the resolved path must also fall under it, so a symlink inside a project that points elsewhere is judged by where it leads.

- **Workspace mount derivation and `.scion` protection**: The Runtime Broker determines whether a workspace is the agent's own checkout (`<agent-dir>/workspace` or a verified linked worktree recorded under the agent's broker-owned state directory), derives container mounts only from broker-known paths using git probes bounded by `GIT_CEILING_DIRECTORIES`, with inherited `GIT_*` variables removed (worktree checks also read no global or system git config), only to verify them, and refuses a git common directory outside the project or a timed-out git probe. In shared workspaces, `.scion` is mounted read-only (on the Apple `container` runtime, which cannot bind-mount a single file, a `.scion` marker file that would be shared is refused at start), `.scion/agents` is created if missing and masked with an empty tmpfs on Docker and Podman whenever `.scion` is a directory (the start fails if `.scion` cannot be inspected, or if `.scion/agents` cannot be created or is not a real directory), and a hub-managed workspace root with no `.scion` entry skips the project settings and harness-config file tier rather than creating one.
- **Host-side git hardening (`pkg/util`)**: Host-side git commands that Scion runs during provisioning clones, shared-workspace pulls, and worktree or shared-clone operations run with a scrubbed `GIT_*` environment, with hooks, fsmonitor, the pager, signing, submodule recursion and the `ext::` transport disabled, with `.lfsconfig` endpoints ignored, and with HTTP(S) tokens supplied only through a host-scoped credential helper. Commands in an existing repository first check its `.git/config` and `config.worktree` against a strict allow-list. See [Git that Scion runs on the host](/scion/local/workspace/#git-that-scion-runs-on-the-host).
- **Agent home confinement (`pkg/agenthome`)**: On the Runtime Broker start path, writes and removals under an agent's home directory (`resetStagedProvisioning`, `restoreControlPlaneInputs`, `restoreSecretsRecord`, container-script staging, `writeAgentTokenFile`, and `agent-info.json` reads and writes) resolve relative to an open directory handle on the agent home, using `O_NOFOLLOW` on each directory component, atomic temporary-file renames over leaf files so symlinks at the leaf are replaced and never written through, and recursive removals that never descend through symlinks.

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

1. **Runtime Broker scope** (lowest priority): Infrastructure-level defaults for agents on that broker.
2. **Hub scope**: Global defaults.
3. **Project scope**: Project-level secrets.
4. **User scope** (highest priority): Personal secrets for the agent's owner.

This produces a merged set of secrets for each agent: for the same key, User scope wins over Project scope, Project over Hub, and Hub over Runtime Broker. Environment variables use the same scope order, and for the same key a variable from any scope takes precedence over an environment-type secret.

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
- Each token row records an explicit boundary (`boundary_kind`, default `project`). A project-boundary token must carry a `project_id`, and a database CHECK constraint enforces the pairing. At startup, the Hub logs the IDs (never the token or project) of any rows that break this rule, and such tokens are rejected when used, while valid tokens keep working. On SQLite, a hand-edited row whose `project_id` is not a UUID fails the schema migration, so correct or delete it before upgrading.
- Tokens can be minted with an explicit hub boundary (`"boundary": {"kind": "hub"}` on `POST /api/v1/auth/tokens`), which reaches hub-level resources and every project; a request naming no boundary is rejected rather than read as hub-bound. Hub-only selectors such as `broker:create` are mintable only on hub-bound tokens. Broker creation admits a UAT only when it is hub-bound and carries `broker:create`, and such a token re-registers only a broker its own user created (see [Runtime Broker](/scion/hosted/ha/runtime-broker/#broker-ownership)).
- Every bearer request passes one gate: the boundary is valid, the target's scope is inside the boundary, the permission is inside the stored ceiling, the holder still has active access to a project target, and the holder's live authority allows the action. Any evaluation error denies.
- At mint time, each requested scope is checked against the caller's live authority before the token is written; an ineligible scope is refused with `403 scope_violation`.
- Operation-specific checks: inbox, conversation and notification operations check the token's inbox selectors and boundary; project messaging policy, template and project configuration operations each require their own permission; Hub configuration operations admit only hub-bound tokens with the matching selector; and hub pre-start hook scripts are redacted for every credential other than an interactive session. See [User Access Tokens](/scion/hosted/user/personal-access-tokens/#hub-bound-tokens).
- A token's selected scopes are a ceiling, not a grant: minting a token with a scope records it as
  a restriction on what the token may do, and grants no access by itself. Every request the token
  later makes is independently authorized against the holder's *current* authority on the specific
  target, including active project access — this is re-checked on every request, not just at mint
  time.
- `agent:attach` and `agent:port_access` are resource-relative: they may be selected for a project
  before the holder has created a single agent in it, but each later request against a specific
  agent is authorized separately. An `agent:attach` request succeeds only for the holder's own
  agents and their descendants. An `agent:port_access` request also succeeds for any agent in a
  project where the holder's role grants `agent.port_access` (the built-in `project-owner` and
  `project-admin` roles do). Losing project access denies every subsequent request against that project's
  targets immediately, independent of the token's remaining validity period.

### 4.5 Credentials Propagation

Scion ensures that sensitive credentials (GCP Service Accounts, API keys for LLMs) are propagated into agent containers securely.
- **Docker / Podman**: Injected via environment variables or read-only bind mounts for file-type secrets. File secrets are written to a temporary directory and mounted into the container at the target path.
- **Kubernetes**: Propagated via Kubernetes Secrets or Secret Manager CSI drivers (e.g., GCP Secret Manager).
- **Broker Mode Isolation**: When agents are dispatched via the Hub, the credential pipeline only uses hub-resolved secrets and environment variables. The broker operator's host environment and filesystem are never scanned, preventing credential leakage into hub-dispatched agents.
- **Isolation**: Agent home directories and non-git project data are isolated on the host filesystem and externalized from the workspace to prevent cross-agent data leakage and unauthorized traversal.
- **Shadow Mounts**: Scion uses `tmpfs` shadow mounts to definitively block agents from accessing `.scion` configuration data or other agents' workspaces within the same project.
- **Token Reset Off the Command Line**: `scion reset-auth` passes the fresh Hub token to the container over stdin, not as a command argument, so it never appears in `/proc/<pid>/cmdline` on the host. This applies to every runtime.
- **Lifecycle**: Secrets exist only in the agent container's memory or transient mounts. When an agent is deleted, all projected secrets and transient volumes are purged.

### 4.6 Hub-Internal Keys

JWT signing keys used for agent and user token issuance are stored through the secret backend when GCP Secret Manager is configured. In development mode (local backend), signing keys fall back to direct database storage with a logged warning. These keys use the internal `hub` scope and are not accessible through the user-facing secrets API.

**Skill download signing key.** Hubs using local storage for the skill registry sign skill file download URLs with HMAC-SHA256. Cloud storage issues presigned object-store URLs and is unaffected. Each URL is a capability for exactly one file of one skill version (`?version=…&exp=…&sig=…`), valid for 15 minutes. The Hub signs it only after checking the creator's read access, and it lets a Runtime Broker download skill files at dispatch without a principal. The key follows the same persistence policy as the signing keys above. When stable keys are required (GCP Secret Manager backend), a failure to load it fails startup. Otherwise the Hub falls back to an in-memory key, which only invalidates URLs that are still outstanding.

### 4.7 Broker Authentication Secrets

The following broker-related secrets are stored in the Hub database and are not managed through the secrets backend:

- **Join tokens**: SHA-256 hashed before storage and single-use. Each token has its own lifetime, from 5 minutes to 24 hours (default 1 hour). A broker has at most one token: issuing a new one replaces the unused earlier one. Redeeming a token deletes it in the same transaction that stores the broker's shared secret, so only one of several concurrent redemptions succeeds, and a failed redemption leaves the token usable. Expired tokens are removed by a scheduled job. The audit log records who created each token, when it expires, and whether it replaced an earlier one; the token itself is never logged.
- **Shared secrets**: Stored as binary BLOBs in the `broker_secrets` table; used for HMAC-SHA256 request signing.

These are infrastructure-level secrets established during broker registration and are managed by the broker authentication subsystem rather than the user-facing secrets API.

### 4.8 Credential Redaction in Logs and URLs

The Hub masks the password in its database connection string (DSN) before writing it to logs or stdout. This covers Hub startup and the `scion server recover-authz` and `scion server migrate-storage` commands. Both URL-style (`postgres://user:pw@host/db`) and libpq keyword/value DSNs (`password=...`) are masked, including Cloud SQL Unix-socket hosts and credentials passed as query parameters (`?password=`, `sslpassword=`). A non-SQLite DSN that cannot be parsed is replaced with a placeholder rather than logged. SQLite paths carry no credential and are logged unchanged.

URL user-info redaction (`pkg/util/urlredact`) is shared across CLI output, API client errors, log text (`SanitizeText`), Hub endpoint strings, git clone URLs, template update URLs, artifact remote fetches, and tunnel endpoints. When a URL holds an `@` and its user info cannot be told apart from its host and path, `urlredact` fails closed and shows only the URL's scheme followed by a placeholder (`xxxxx` in CLI output and API client errors) rather than returning the input unchanged. Skills directory discovery (`POST /api/v1/skills/discover-directory`) passes through only the `?token=<SECRET_NAME>` query parameter (`[A-Z][A-Z0-9_]*`, a `token` value that is not such a name is refused with `400`) and strips all other query parameters and fragments before normalizing or logging candidate URIs.

## 5. Development Security

To facilitate local development, Scion provides a **Development Authentication** mode.
- **Developer Token**: A persistent token starting with `scion_dev_` stored in `~/.scion/dev-token` (or provided via `SCION_DEV_TOKEN` / `SCION_DEV_TOKEN_FILE`).
- **Loopback-Only Client Rule**: The CLI sends development credentials only to loopback Hub endpoints (`localhost`, `127.0.0.1`, `::1`). For any non-loopback Hub, `SCION_DEV_TOKEN` and `SCION_DEV_TOKEN_FILE` are not sent and, when no other credential applies, print a one-line notice to stderr naming the Hub, and `~/.scion/dev-token` is not sent and prints no notice.
- **Agent Containers and Dispatched Agents**: Hub-dispatched agents authenticate only with the token the Hub issues for their run (`~/.scion/scion-token` and `SCION_AUTH_TOKEN` / `SCION_HUB_TOKEN`). The Runtime Broker never passes its own `SCION_DEV_TOKEN` or `SCION_AUTH_TOKEN` values into an agent's environment (even when co-located with a loopback Hub under dev auth), strips any `SCION_AUTH_TOKEN` set in agent or harness-config environment settings with a warning, and fails any Hub-dispatched start or restart that lacks a Hub-issued token (`the hub issued no agent token for this start; check the hub's agent token service`).
- **Constraints & Safeguards**: Dev mode is disabled by default. Startup validation strictly prohibits binding the `devAuthMiddleware` to non-loopback interfaces (e.g., `0.0.0.0`). Because this middleware automatically logs in every cookieless request as an admin, refusing it on public interfaces prevents the accidental exposure of an unauthenticated admin UI.
- **Warning**: The server logs clear warnings when operating in Dev Mode.
