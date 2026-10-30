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

### Who can sign in (user access mode)

:::caution[The default access mode is open]
The default user access mode is `open`: once OAuth is configured, any account that the identity provider accepts can sign in and gets an account with the default role described below (`member` unless changed). Choose a mode before the Hub is reachable by others.
:::

`server.auth.user_access_mode` controls who can sign in:

| Mode | Who can sign in |
|------|-----------------|
| `open` (default) | Any account the identity provider accepts, limited to `authorized_domains` when that list is set. |
| `domain_restricted` | Only accounts from `authorized_domains`. With an empty list, no one except `admin_emails` can sign in. |
| `invite_only` | Only users who were invited or already have an account, still limited to `authorized_domains` when set. |

Accounts in `admin_emails` can always sign in. On a single-node Hub, set the initial values in `hub.env`; an admin can change them later in **Admin > Server Config**:

```bash
SCION_SEED_SERVER_AUTH_USERACCESSMODE=invite_only
SCION_SEED_SERVER_AUTH_AUTHORIZEDDOMAINS=example.com
```

Or in `settings.yaml`:

```yaml
server:
  auth:
    user_access_mode: invite_only     # open (default) | domain_restricted | invite_only
    authorized_domains:
      - example.com
```

### Default role for new users

In a multi-user deployment, each user has a hub role: `admin`, `member` or `viewer`. Users listed in `admin_emails` are always admins. Everyone else gets the role set by `server.auth.default_user_role` (`member` by default) when their account is first created or activated. This includes the first sign-in of an invited or allow-listed user. Set it to `viewer` if new users should be able to work in projects they are added to, but not create projects of their own:

```yaml
server:
  auth:
    default_user_role: viewer   # member (default) | viewer
```

To pre-register a person before their first sign-in (for example, under `invite_only`), use **Invite User** on **Admin > Users**, `scion hub users invite` (`POST /api/v1/admin/users/invite/bulk`, which can invite multiple users and optionally grant them project roles), `scion hub users provision <email>`, or `POST /api/v1/users`. All of them create `invited` records and require the `user.invite` permission, which hub admins hold. The CLI and API accept an interactive sign-in or a hub-boundary user access token carrying `user:invite`; a token whose user lacks `user.invite` is refused. Granting project roles with `scion hub users invite` also needs `user.read` and the authority to grant those roles. `scion hub users provision`, `POST /api/v1/users`, and the dialog when a display name is given are not available while the Hub runs with dev auth (see [Development Authentication](#development-authentication-dev-auth)). An optional display name is stored with `provision`; at first sign-in, a name supplied by the sign-in provider replaces it. The hub role is assigned at first sign-in as described above. You can also add an `invited` user to groups and grant them role bindings ahead of time; those bindings grant authority only once the user signs in and their account becomes `active`. See the [API reference](/scion/reference/api/) and the [CLI reference](/scion/reference/cli/).

You can also set it from **Admin > Server Config**, or seed it with `SCION_SEED_SERVER_AUTH_DEFAULTUSERROLE=viewer`. Changing it does not affect existing users; change an individual user's role on **Admin > Users**. See [Hub roles](/scion/hosted/ha/permissions/#hub-roles) for what each role allows and how demotion from `admin_emails` works, and the [server configuration reference](/scion/reference/server-config/#authentication-serverauth) for the setting.

`server.auth.default_user_role` is a different setting from the deprecated federation `default_role` described under [OIDC-Based Federation](#oidc-based-federation) below, which is accepted for compatibility and has no effect.

### Deleting users

Deleting a user on **Admin > Users** (`DELETE /api/v1/users/{id}`) fails with `409 last_owner` if the user is the only usable owner of any project (an owner is usable when their owner binding is in effect and their account exists and is active; suspended, invited and deleted owners do not count), or holds the last owner binding of any project, even an expired one. The error's `details.projects` lists those projects. Transfer ownership or add another owner on each one, then delete the user again. If someone grants the user a role or changes one of their roles while the delete runs (for example, transfers a project to them) and that change commits first, the delete is aborted with `409 conflict` and nothing is changed; retry it. A concurrent revoke of one of the user's roles does not abort the delete. A grant that commits in the last moments before the delete itself commits is not detected and can leave a stale binding on the deleted user ([ptone/scion#2769](https://github.com/ptone/scion/issues/2769)). It also fails with `409 user_delete_blocked` while any of the user's agents has not been deleted, including a stopped one (unless the `hub.user_delete_force_cascade` experiment is on and you use [Force-deleting a user who has agents](#force-deleting-a-user-who-has-agents) below). The guard counts three kinds of agent as the user's: the agents the user owns, the agents whose ancestry root is the user (agents started by the user's agents, or by those agents' child agents), and the agents started by the user's own schedules. It does not count an agent started by a schedule that an agent created. The error's `details.agents` lists each such agent's `id`, `slug`, `projectId` and `deletionState` (empty, or `deleting`, `finalizing` or `failed` when a delete of that agent is under way or failed), so delete those agents first, including agents started by the user's agents. The same error lists in `details.edges` any active delegation from the user to an agent that exists and is not one of those agents (for example an agent another user owns); delete those agents too. Each list holds at most 50 entries, and `details.truncated` is `true` when there are more. A delegation to an agent that is gone, or to another user, does not block, and a successful delete deactivates every active delegation edge with the user as the delegator. The admin page lists these blockers when you open the delete dialog, and `GET /api/v1/users/{id}/delete-blockers` returns them without deleting. Deleting an agent does not delete the agents it started. An agent created for the user while the delete runs either finishes first, and the delete then fails with this error, or fails (`409 conflict` on the create API; a scheduled start fails and is logged) because the user or agent it belongs to no longer exists. A soft-deleted agent does not block the delete, but once the user is deleted, a soft-deleted agent of any of these three kinds cannot be restored: `POST /api/v1/agents/{id}/restore` fails with `409 conflict` because the user or agent it belongs to no longer exists. The deprecated allow-list delete (`DELETE /api/v1/admin/allow-list/{email}`) applies the same rules.

When the deletion succeeds, Scion also removes all of the user's role bindings (project, hub and system). It then removes the user's user-scope secrets and environment variables; this step is best effort, and anything it cannot remove (for example while an external secret backend is unavailable) is removed by a startup sweep once the backend is reachable. The sweep runs in the background after the Hub starts, within a time limit, and does not delay startup. If no secret backend is configured (for example because it failed to start), secrets whose value is stored in the Hub database are still removed; a secret that only references an external backend (GCP Secret Manager) is kept until a sweep can also remove its external value. Bindings left behind by deletions made before this change are not cleaned up. To clear such a binding when it is a project's only owner, add a real owner first, then remove the old binding from the project's members.

Schedules do not block a user delete. Once it succeeds, every active recurring schedule whose recorded authority is the user (the user last created, changed or resumed it; it may have been created by someone else), in any project, is paused with status reason `creator_deleted`, because none of its fires could be authorized. Anyone allowed to resume such a schedule who resumes it records their own authority, and it then runs as them (see [Schedules paused by the Hub](/scion/hosted/user/scheduling/#schedules-paused-by-the-hub)). This step is best effort: a schedule it misses, or one whose user was removed through the deprecated allow-list delete, is paused the next time it comes due. A schedule whose recorded authority is an agent is not affected by the user delete; it is paused when that agent is deleted.

After a user is deleted, that user's tokens stop working immediately. Requests that present a web or CLI sign-in token get `401` with the error code `user_not_found`. Requests that present one of the user's access tokens (`scion_pat_`) are also refused, with `401` and the error code `unauthorized`.

### Force-deleting a user who has agents

Behind the `hub.user_delete_force_cascade` [experiment](/scion/reference/experiments/) (off by default), an admin can delete a user whom agents or delegations depend on, instead of deleting those agents one by one. On **Admin > Users**, the delete dialog of such a user offers **Force delete**, which asks for confirmation naming how many agents it stops; the API is `DELETE /api/v1/users/{id}?force=true`. While the experiment is off, `force=true` is ignored and the delete is refused as described above.

A forced delete needs `user.delete` on the user and, for every one of the user's agents, the same permission an agent delete needs (`agent.delete` on that agent). If you may not delete one or more of them, the request is refused with `403 user_delete_force_forbidden`, `details.agents` lists those agents (`id`, `projectId`), and nothing changes. It needs a sign-in session, like a plain delete. The last super-admin, admin lockout and last-owner guards apply as for a plain delete. If nothing blocks the delete, a forced delete is a plain delete (`204`).

Otherwise the delete runs in two steps:

1. **Revoke, at once.** In one transaction, the user's status becomes `deleting`, the user's role bindings are removed, the user's access tokens are revoked, and every active delegation from the user, together with the delegations and service-account assignments of each of the user's agents, is deactivated. The user cannot sign in or use any credential from then on, and the agents keep only the read access every agent has in its project. The request answers `202` with the deletion resource. Before it answers, the user's schedules are paused with `creator_deleted`, as after a plain delete. If an agent of the user appeared after the permission checks, the request is refused with `409 user_delete_blockers_changed` and nothing changes; retry it.
2. **Stop, then remove.** The Hub deletes each of the user's agents through the normal agent delete (deepest child agents first), retrying failures with a growing delay, and removes the user once every agent is **stopped** (the agent delete confirmed its runtime was torn down) or **removed** (its record is gone without that confirmation). The user's user-scope secrets and environment variables are then removed as after a plain delete.

While the deletion is in progress:

- `GET /api/v1/users/{id}/deletion`, and **Deletion status** on the user's row, show the state (`stopping`, then `completed`; the panel reloads every few seconds while the deletion is stopping), each agent's outcome (`pending`, `stopping`, `failed`, `stopped` or `removed`) with its latest error, and counts per outcome. The resource answers after the user is removed too.
- A plain delete, and any change to the user, is refused with `409 user_deletion_in_progress` (with `details.deletionId`). Another forced delete joins the deletion and retries its agents at once (**Retry** in the panel).
- Creating or restoring an agent for the user (an agent owned by the user, or started by one of the user's agents) is refused with `409 conflict`.

An agent whose Runtime Broker cannot be reached stays `failed`, and the deletion stays in progress, with no timeout: the Hub never reports an agent as stopped unless its teardown was confirmed. To finish, delete that agent from its page with force (`DELETE /api/v1/agents/{id}?force=true`); it is then reported as `removed`, and the completion records how many agents were stopped and how many were removed. Turning the experiment off stops new forced deletes only: a deletion already accepted runs to completion, because the user's authority is already revoked and what remains is stopping the agents the requester was allowed to delete. While the experiment is off, the **Deletion status** panel and `GET /api/v1/users/{id}/deletion` are not available.

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

### External OIDC Login Provider Support

For enterprise SSO setups, Scion supports authenticating Web UI users via an external OpenID Connect (OIDC) identity provider (such as Okta, Keycloak, or Ping Identity) as an alternative to standard Google/GitHub OAuth.

#### How It Works
- **OIDC Discovery**: The Hub automatically discovers authorization and token endpoints by fetching the provider's standard metadata configuration from `{issuer_url}/.well-known/openid-configuration` with a **1-hour TTL discovery cache** to ensure high performance and zero-downtime key rotation.
- **Dynamic Login Button**: When enabled, the Web Dashboard dynamically renders a custom login button using your configured `display_name` alongside any active Google or GitHub OAuth buttons.
- **Public Client Support**: For OIDC public clients (like Keycloak public clients), the Hub allows the `client_secret` to be left empty or omitted, removing client secret validation from the token exchange workflow.

#### Redirect URI

When registering Scion as a client in your identity provider, set the **redirect URI** (sometimes called "callback URL") to:

```
<hub-base-url>/auth/callback/oidc
```

Replace `<hub-base-url>` with the web server's base URL, including the scheme, for example `https://hub.example.com`. The web base URL is resolved once at startup from operator inputs only (in order: `--base-url`, `SCION_SERVER_BASE_URL`, `SCION_SERVER_HUB_ENDPOINT`, `server.hub.public_url` in the global `settings.yaml`, or a URL derived from a Cloud Run IAP audience; never from the database; see [Web Base URL](/scion/reference/server-config/#web-base-url)), and defaults to `http://localhost:<web port>`. This is the endpoint the IdP redirects users to after authentication.

The Hub builds the redirect URI from its web base URL, and marks the session cookie `Secure` when the base URL starts with `https://`. On any non-loopback listener, the server requires an `https://` web base URL and refuses to start with `http://`; plain `http://` is accepted only on a loopback listener (`localhost`, `127.0.0.0/8`, or `::1`). Whether an `http://` loopback redirect URI is accepted is up to your identity provider. The `issuer_url` must also use `https://` unless its host is `localhost` or `127.0.0.1`, and the Hub does not start otherwise.

#### Configuration

To enable the external OIDC login provider, add the `oidc_login` section under `server` in your Hub's static `settings.yaml` bootstrap file:

```yaml
server:
  oidc_login:
    enabled: true
    display_name: "Corporate SSO"                         # Text shown on the login button
    issuer_url: "https://sso.example.com/auth/realms/main" # Base OIDC issuer URL
    client_id: "scion-client"                              # Client ID registered with the provider
    client_secret: "secret-value"                          # Client secret (can be empty for public clients)
    scopes: ["openid", "email", "profile"]                 # Custom scopes (defaults to openid, email, profile)
```

Prefer `settings.yaml` for these keys. Underscored environment variables such as `SCION_SERVER_OIDC_LOGIN_ENABLED` are ignored, and the Hub logs a warning at startup for each one. The collapsed names (`SCION_SERVER_OIDCLOGIN_ENABLED`, `SCION_SERVER_OIDCLOGIN_ISSUERURL`, `SCION_SERVER_OIDCLOGIN_CLIENTID`, and so on) take effect only when `settings.yaml` has a `server:` section, as above. They are ignored on the legacy `server.yaml` path ([ptone/scion#3038](https://github.com/ptone/scion/issues/3038)).

:::tip[Troubleshooting: `invalid redirect_uri`]
If your identity provider returns an `invalid redirect_uri` error during login, verify that the redirect URI registered in your IdP matches `<hub-base-url>/auth/callback/oidc` exactly, including the scheme, hostname, port, and path. The value must be the web base URL (resolved from `--base-url`, `SCION_SERVER_BASE_URL`, `SCION_SERVER_HUB_ENDPOINT`, `server.hub.public_url` in the global `settings.yaml`, or the Cloud Run IAP audience), plus `/auth/callback/oidc`.
:::

### Verified Email Requirement

Google, GitHub and external OIDC sign-in accept only an email address that the provider marks as verified. The Hub never falls back to an unverified address.

- **Google and OIDC**: the provider's verified-email claim must be true.
- **GitHub**: the Hub always reads the account's full email list (the `user:email` scope) rather than trusting the public profile email. It uses the profile email if the list marks it verified, otherwise the primary verified email, otherwise any verified email.

:::caution[Breaking change]
Users whose only email address is unverified at their provider can no longer sign in. They must verify an address with the provider (for GitHub, under **Settings > Emails**) and sign in again.
:::

## Domain Authorization

You can restrict authentication to specific email domains using the `server.auth.authorized_domains` setting. This provides an additional layer of access control beyond OAuth authentication. See also [Who can sign in](#who-can-sign-in-user-access-mode).

### Configuration

Set the environment variable with a comma-separated list of allowed domains:

```bash
# Allow only users from these domains
export SCION_SEED_SERVER_AUTH_AUTHORIZEDDOMAINS="example.com,mycompany.org"
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
* **Project visitors**: When the `federation.project_visitors` experiment is on, the dialog for a `hub` or `user` issuer has the switch **Allow project owners to grant visitor access to users from this issuer**, bound to the issuer's `allow_visitor_grants` setting (off by default). While it is on, project owners and admins can add users of that issuer as visitors on the project's **Visitors** tab (see [the Web Dashboard](/scion/workstation/dashboard/#projects)). Turning it off refuses that issuer's visitors on their next request and keeps their grants, so turning it back on restores access.
* **Default Role (deprecated)**: The **Default Role** field is no longer shown for `user` issuers. The `default_role` key is still accepted in the configuration and ignored, and editing an issuer keeps a stored value unchanged.

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
        # Optional (hub and user issuers): let project owners grant visitor
        # access to users of this issuer (federation.project_visitors)
        allow_visitor_grants: false
```

### How It Works
- **Multi-Issuer Support**: The federation authenticator handles multiple external trust domains simultaneously.
- **JWKS Caching**: Public verification keys are cached locally with RS256-signature pinning and automatic refresh to eliminate per-request latency.
- **Federation Access Middleware**: Requests carrying external OIDC tokens pass through the `RequireFederationAccess` scope-gated middleware, validating token authenticity, issuer rules, and matching scopes.
- **Identity Types**:
  - `hub`: Identifies requests originating from federated partner Hubs. A partner Hub marks each credential it signs with `scion_token_kind`: `agent` for agent identity tokens, `user` for user access credentials. Agent identity tokens, and credentials without the claim from Hubs that predate it, are read as agents, as before. A user access credential is accepted only when the issuer has `accept_user_tokens: true` and the `federation.project_visitors` experiment is on; it must carry a verified email and no agent claims, and must pass `allowed_emails` and `allowed_domains`. Any other `scion_token_kind` value is refused. A refused credential gets the same `401` as any other credential that fails verification.
  - `service_account`: Authenticates automated workloads via GCP Service Accounts.
  - `user`: Maps OIDC tokens to standard user identities, with domain restrictions using wildcards (e.g. `allowed_emails: ["*@example.com"]`). The `default_role` key is deprecated: it is accepted and has no effect on what a federated user can do (it is also separate from `server.auth.default_user_role`, which applies to users who sign in through OAuth, OIDC login or an auth proxy).
- **Write limits for federated callers**: A federated caller keeps visitor rights on the resources it has been granted, but the Hub refuses its writes to profile and other non-agent resources: user-scope templates, skills and harness configs, imports, env vars and secrets, chat preferences, presence, and attachments outside a project. Federated callers also cannot redeem invites.

### External Bearer Tokens (Google credential pass-through)

An integration that sits in front of the Hub — for example the Gemini Enterprise A2A bridge — does not mint a Hub credential on the end user's behalf. It forwards the caller's **own** Google credential, verbatim, as the request's `Authorization: Bearer` header, on every request. There is no exchange step and no separate Hub-issued token to cache, refresh, or revoke.

This path activates once a `user`-type trusted issuer for `https://accounts.google.com` is configured with a non-empty `expected_audience`. Two credential shapes and two principal kinds are accepted:

| Principal | Google OIDC ID token (JWT) | Google OAuth2 access token (opaque) |
|---|---|---|
| **User** | Accepted: `aud` in `expected_audience`, verified email, `allowed_domains` (if set), then the Hub sign-in policy | Accepted: `azp` in `expected_audience`, verified email, `allowed_domains` (if set), then the Hub sign-in policy |
| **Service account** | Accepted: `aud` in `expected_audience`, verified email, GCP project (parsed from the email) in `allowed_gcp_projects` — unset admits none | **Always rejected** |

```yaml
server:
  federation:
    enabled: true
    trusted_issuers:
      - issuer_url: "https://accounts.google.com"
        jwks_url: "https://www.googleapis.com/oauth2/v3/certs"
        issuer_type: "user"
        # REQUIRED: the OAuth client ID whose tokens are accepted. Without
        # this, the path is disabled for this issuer, and a warning is
        # logged at startup and on reload.
        expected_audience: "1234567890-abc.apps.googleusercontent.com"
        # Optional: restrict USER principals to these email domains (exact,
        # case-insensitive, no wildcards, no subdomain matching). Each entry
        # must be a bare domain: no "@" or "*" (this is not the allowed_emails
        # pattern syntax), no "/" or ":" (not a URL), no whitespace, no
        # leading or trailing dot, and no ".." — config validation rejects
        # any entry that could never match. Omit to accept any verified
        # Google account that passes the Hub sign-in policy below.
        allowed_domains: ["example.com"]
        # Optional: admit SERVICE-ACCOUNT principals whose GCP project ID
        # (parsed from the service account's email) is listed (exact,
        # case-insensitive). Omitting this admits NO service accounts.
        allowed_gcp_projects: ["my-a2a-project"]
```

A verified **user** identity is still resolved through the same sign-in policy as interactive login (`admin_emails`, `authorized_domains`, `user_access_mode`) after the `allowed_domains` check passes — a listed domain narrows *which* users reach that policy, it does not replace it. A verified **service account** identity skips the sign-in policy entirely: membership in `allowed_gcp_projects` **is** the authorization decision for first-time admission. A service account is never subject to `allowed_domains`, and a user is never subject to `allowed_gcp_projects`. Bypassing the sign-in policy does not extend to suspension: a service account that was already provisioned and is later suspended is refused on its next request exactly like a suspended user.

The A2A bridge forwards Google credentials under the `hubBearer` auth scheme (`auth.scheme: hubBearer` in `scion-a2a-bridge.yaml`): it admits a caller by presenting the same token to the Hub's `GET /api/v1/auth/me`, then forwards that token verbatim on every downstream call. The Hub re-verifies the token on each request: a suspended user is refused on the next call. A revoked Google credential stops being accepted once the Hub's verification cache entry expires (at most 5 minutes, and never beyond the credential's own expiry).

**Response codes:**

| Condition | Status | Code |
|---|---|---|
| No Google trust configured, or the token isn't a shape this path recognizes | Falls through unchanged to whichever other authentication check applies to that credential | — |
| Per-client-IP rate limit exceeded (checked only on a credential-cache miss) | 429, with `Retry-After` | `rate_limited` |
| Verification failed, unverified email, a service account presented as an access token, or its project/domain isn't listed | 401 | `unauthorized` (message `invalid external bearer token`; the specific reason is logged, never returned) |
| Google's token-verification endpoints are unreachable | 503 | `upstream_unavailable` |
| An internal fault while resolving the verified identity to a Hub user (store error) | 503 | `store_error` |
| The resolved user is suspended | 403 | `user_suspended` |
| The Hub sign-in policy denies the user, or the identity fails auto-link checks | 403 | `forbidden` |

:::note[Every unrecognized opaque token is sent to Google]
Once a Google issuer is trusted, the Hub does not sniff a bearer token's prefix (e.g. `ya29.`) to decide whether it might be a Google access token — that prefix isn't part of Google's contract. Any bearer credential that reaches this path without being a JWT is treated as a candidate Google access token and introspected against Google's `tokeninfo` endpoint. A non-JWT credential intended for some other purpose will still just fail verification (401), but it does cost a Google round trip.
:::

:::note[Domain-scoped GCP projects]
A domain-scoped GCP project ID such as `example.com:proj` produces service-account emails whose domain is `proj.example.com.iam.gserviceaccount.com`, which parses to `proj.example.com`. List that parsed form — `proj.example.com`, not `example.com:proj` — in `allowed_gcp_projects`.
:::

`allowed_domains` and `allowed_gcp_projects` are matched case-insensitively against the verified email; the configured lists themselves are kept exactly as written (not lower-cased or otherwise rewritten when the config loads).

---

## Development Authentication (Dev Auth)

To minimize friction during local setup, Scion includes a "Dev Auth" mode. When enabled, the Hub auto-generates a token and creates a "Development User" identity.

Dev auth is for a single-user workstation Hub. Pre-registering other users (`POST /api/v1/users`, `scion hub users provision`) is not available while the Hub runs with dev auth, for any caller, and returns `403` with `details.reason: dev_auth_not_supported`.

### Enabling Dev Auth
Start the server with the `--dev-auth` flag or set it in your `settings.yaml`:

```yaml
server:
  hub:
    host: 127.0.0.1
  auth:
    dev_mode: true
```

Or via environment variable:
```bash
export SCION_SERVER_AUTH_DEVMODE=true
```

Dev auth requires a loopback listen host (`127.0.0.1`, `::1`, or `localhost`). The server refuses to start when dev auth (`--dev-auth`, `server.auth.dev_mode`, or the workstation-mode default) is combined with a non-loopback host such as `0.0.0.0` (including an empty host, which normalizes to `0.0.0.0`), both when the web server is enabled and when the Hub API runs standalone with the web server disabled.

### Using the Developer Token
When the Hub starts with `dev_mode: true`, it writes the token to `~/.scion/dev-token`.
- **CLI**: The `scion` CLI automatically uses `~/.scion/dev-token` (or `SCION_DEV_TOKEN` / `SCION_DEV_TOKEN_FILE`) only when the target Hub endpoint is on loopback (`localhost`, `127.0.0.1`, or `::1`). For any non-loopback Hub, `SCION_DEV_TOKEN` and `SCION_DEV_TOKEN_FILE` are not sent and, when no other credential applies, print a one-line notice to stderr naming the Hub, and `~/.scion/dev-token` is not sent and prints no notice.
- **Web**: Browser sign-in in dev auth mode uses a single-use launch link rather than logging in automatically on page load. `scion server start` and `scion server open` (or `scion server open --print`) exchange the local dev token via `POST /auth/dev-launch` (`Authorization: Bearer <dev-token>`) for a short-lived, single-use code (valid for about two minutes, up to 32 pending codes) and open `GET /auth/dev-signin?code=<code...>`, which consumes the code and sets the browser session cookie. Visiting `/login` directly without a launch link does not sign in automatically (`POST /auth/dev-launch` and `GET /auth/dev-signin` return `404` when dev auth is off). When a signed-out browser opens a protected page, `/login?returnTo=<path>` (and `/auth/login`, including on a dev sign-in server when a web OAuth provider is configured) carries the requested local path through the OAuth flow so the callback redirects back to that page instead of `/`; non-local, absolute, or sign-in paths fall back to `/`.
- **Agent containers**: Hub-dispatched agents authenticate with the token the Hub issues for their run (`~/.scion/scion-token` or `SCION_AUTH_TOKEN`). The Runtime Broker passes an agent only its Hub-issued token (`StartOptions.ScionToken`) and never passes its own `SCION_DEV_TOKEN` or `SCION_AUTH_TOKEN` values into an agent's environment. If a Hub-dispatched agent starts without a Hub-issued token, the Runtime Broker removes any stale `~/.scion/scion-token` file from the agent home (failing the start if removal fails) and relays a warning to the Hub that the agent started without a Hub token and cannot reach the Hub. When a Hub and Runtime Broker run co-located with dev auth enabled, the Runtime Broker offers its dev token only to an agent that has no Hub-issued token, whose dispatch comes from the co-located Hub on a loopback endpoint, and whose Hub endpoint comes from the server's own configuration rather than from project settings or the agent's environment.

Alternatively, you can set the token in your environment for a loopback Hub:
```bash
export SCION_DEV_TOKEN=scion_dev_...
```

## Runtime Broker Security

Runtime Brokers use a robust security model to ensure that only authorized Hubs can dispatch commands and that agents remain isolated.

### HMAC-Based Authentication

Communication between the Hub and a Runtime Broker (in both directions) is secured using **HMAC-SHA256 request signing**. This provides several security benefits:
- **Mutual Authentication**: Both parties prove they possess the shared secret.
- **Payload Integrity**: The request body is included in the signature, preventing tampering.
- **Replay Protection**: Every signed request must include both an `X-Scion-Timestamp` header (within ±5 minutes) and a non-empty `X-Scion-Nonce` header (refused with `401 missing X-Scion-Nonce header` otherwise; every in-tree signer sends a random nonce). Both the Hub and the Runtime Broker's HMAC verification middlewares (and the Conduit RPC guard) record each verified signature for the clock-skew window and refuse any repeat within that window with `401 request already served`.

A shared secret is established during the `scion runtime-broker register` flow and is stored locally in `~/.scion/hub-credentials/<name>.json`, one file per Hub connection.

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

- **Block (Default on every runtime except Kubernetes)**: All requests to the metadata server are intercepted and return a 403 Forbidden. This ensures agents cannot expose the host's identity (e.g., when running on a GCE instance). On the Kubernetes runtime, Block is accepted but works differently: the agent pod runs as the runtime profile's block Kubernetes ServiceAccount, or the namespace's default ServiceAccount when none is configured, with no GCP credentials mounted. On Workload Identity nodes that ServiceAccount still receives a federated token, so the agent's GCP access is whatever IAM grants that ServiceAccount holds. An agent with no GCP identity mode configured gets Passthrough on Kubernetes. See the Kubernetes runtime note in [Permissions](/scion/hosted/ha/permissions/#hub-default-gcp-identity).
- **Assign**: Assigns a specific Google Service Account to the agent.
  - The agent's `sciontool` sidecar intercepts requests to the metadata server.
  - Token requests are proxied to the Scion Hub, which checks both the calling agent's live `gcp_service_account.use` authority and the recorded assigner's live `gcp_service_account.assign` authority on that Service Account (within the recorded permission ceiling; see [Token-Request Authority](/scion/hosted/ha/permissions/#token-request-authority)) before using its own permissions to generate a short-lived access or identity token (via `iam.serviceAccounts.getAccessToken` or `generateIdToken`).
  - The token is then returned to the agent, allowing it to use standard GCP SDKs (Application Default Credentials) as that specific Service Account.
- **Passthrough**: Requests are allowed to reach the actual host metadata server. Use with caution as this allows the agent to assume the identity of the underlying node. Security is tightened by restricting GCP identity passthrough to broker owners and admin-role users on embedded (co-located) brokers.

:::caution[Passthrough does not work in Cloud Run Sandboxes]
GCP identity passthrough mode does not work inside Cloud Run Sandbox (gVisor) runtimes because the real GCE metadata server is unreachable.

While the Hub automatically translates passthrough to assign mode at agent creation and PATCH time (using the broker's host service account), explicitly configuring assign mode is recommended for clarity and consistency.
:::

#### Host networking on Docker and Podman

On the Docker runtime, an agent container runs with `--network=host` when its Hub endpoint is on loopback (`localhost`, `127.0.0.1`, `::1`, or `host.docker.internal`). On Docker and Podman, it also runs with `--network=host` whenever `SCION_FORCE_HOST_NETWORK` is set on the Runtime Broker. Because the container shares the host network namespace, `sciontool` cannot install its `iptables` rule for `169.254.169.254` without altering the host's own networking:

- **`assign`** is always refused under host networking.
- **`block`** is governed by the Runtime Broker's [`SCION_HOST_NETWORK_BLOCK_POLICY`](/scion/reference/server-config/#runtime-broker-environment-variables) environment variable:
  - `auto` (default when unset): refuses agent start when the Runtime Broker runs on a detected Linux cloud host (GCE, AWS EC2, Azure, Oracle Cloud, Alibaba Cloud, OpenStack, DigitalOcean, or Hetzner, identified from local DMI files under `/sys/class/dmi/id/`, `/run/cloud-init/instance-data.json`, or GCE guest-agent files with no network calls). On other hosts (such as a local workstation), the agent starts and the Runtime Broker logs and returns a warning that `169.254.169.254` is not blocked.
  - `strict`: always refuses agent start when `block` combines with host networking, even off-cloud.
  - `allow`: allows agent start under host networking even on a detected cloud host, logging a warning and appending an acknowledgement to the start warning.
  - Any unrecognized value is logged at `ERROR` and treated as `strict`.

To run agents with `block` or `assign` on a cloud VM, configure a non-loopback, bridge-reachable Hub URL (`SCION_SERVER_BASE_URL` or `server.hub.public_url`) so containers run on bridge networking, and leave `SCION_FORCE_HOST_NETWORK` unset on the Runtime Broker.

### Management UI & Hub-Minted Service Accounts

Administrators can manage available Service Accounts through the **Service Accounts** section in the Admin dashboard. 
- **Registration**: Register existing GCP Service Accounts by email. The system
  accepts service accounts with `@<project>.iam.gserviceaccount.com`,
  `@developer.gserviceaccount.com`, and `@appspot.gserviceaccount.com` suffixes.
- **Hub-Minted Accounts**: The Hub can directly manage and provision (mint) GCP service accounts based on your quota dashboard and capability controls.
- **Validation**: Scion auto-verifies that the Hub has the necessary permissions to act as the registered Service Account upon registration.
- **Assignment & Defaults**: Service Accounts can be assigned to agents during the creation flow. Projects also support default GCP identities that are automatically applied in the agent creation form.

### Security & Auditing

- **Iptables Interception**: Scion uses `iptables` (when `NET_ADMIN` capability is available) to redirect traffic from `169.254.169.254:80` to the local sidecar.
- **Authorization Checks**: Administrative actions for GCP Service Account management require `project-owner` (`ActionManage`) permissions to enforce strict authorization boundaries. Every agent token request (`POST /api/v1/agent/gcp-token` and `POST /api/v1/agent/gcp-identity-token`, as well as `gs://` link fetches in chat) also verifies both the agent's live `gcp_service_account.use` authority and the recorded assigner's live `gcp_service_account.assign` authority on the assigned Service Account (see [Token-Request Authority](/scion/hosted/ha/permissions/#token-request-authority)).
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

### Webhook Secret
GitHub App webhook deliveries must be signed. Set the webhook secret in the "GitHub App" tab of the Admin Server Config UI; the Hub stores it as a hub-scoped secret. Use the same value as the webhook secret in your GitHub App's settings. If no webhook secret is configured, the Hub rejects every webhook event with `503 Service Unavailable` instead of processing it unsigned.


## Session Revocation

Administrators can force any user to re-authenticate by revoking all of their active web sessions. This is useful for incident response, credential compromise, or when offboarding a user who is still logged in.

- **Web Dashboard**: On the Admin Users page, open the actions menu for a user and select **Revoke Sessions**.
- **API**: `POST /api/v1/users/:id/revoke-sessions` (requires admin privileges).

Revocation takes effect immediately — the user is redirected to the login page on their next request. Agent tokens and User Access Tokens (UATs) are unaffected; revoke those through their own mechanisms ([UATs](/scion/hosted/user/personal-access-tokens/), agent credential reset via [`scion reset-auth`](/scion/reference/cli/#scion-reset-auth)).

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
- A Hub-dispatched create, start, or restart requires this Hub-issued token: if the Hub issues none, the Runtime Broker removes any previous `~/.scion/scion-token` file and fails the start with `500 runtime_error` (`the hub issued no agent token for this start; check the hub's agent token service`), starting no container (and leaving the agent stopped on a restart). The Runtime Broker never substitutes its own `SCION_AUTH_TOKEN` or `SCION_DEV_TOKEN`, and any `SCION_AUTH_TOKEN` configured in an agent's or harness-config's environment settings is stripped and ignored with a start warning.
- When `server.keyring_file` is configured (or `server.jwt.emit_version` is `"keyring"`), the Hub signs agent and user JWTs with the active generation of the keyring's signing ring (writing that generation's key ID in the JWT protected `kid` header when `emit` is `"versioned"` or `server.jwt.emit_version` is `"keyring"`) and verifies tokens by `kid` against the keyring while still accepting unversioned tokens during rotation (`signing.legacy_read_ids` / `server.jwt.accept_legacy`; see [`server.keyring_file`](/scion/reference/server-config/#environment-variables)).

## User Access Tokens

For programmatic access (e.g., CI/CD pipelines), the Hub supports **user access tokens (UATs)**.
- Tokens can be generated via the Web Dashboard or CLI (`scion hub token create`).
- Tokens are prefixed with `scion_pat_` (a legacy artifact of the older "personal access token" name).
- Use the `Authorization: Bearer <token>` header in your requests.

See [User Access Tokens](/scion/hosted/user/personal-access-tokens/) for the full user-facing guide.