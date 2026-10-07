# User Access Tokens

**Status:** Approved
**Date:** 2026-03-18
**Related:** [auth-overview](.design/hosted/auth/auth-overview.md), [server-auth-setup](.design/hosted/auth/server-auth-setup.md), [sciontool-auth](.design/hosted/auth/sciontool-auth.md)

## Problem Statement

Scion needs a mechanism for headless, non-interactive authentication in CI/CD pipelines and automation scenarios. While the existing `SCION_HUB_TOKEN` environment variable already supports bearer tokens, there is no user-facing way to create scoped, expirable tokens suitable for production automation. The existing API key system (`sk_live_*`) provides a foundation but lacks grove-scoping and action-level granularity.

Users need to:
- Create tokens scoped to a specific grove with limited action permissions
- Use tokens in CI/CD pipelines (e.g., dispatch agents, check status)
- Manage token lifecycle (create, list, revoke) via CLI and web UI
- Rotate tokens without disrupting other integrations

## Current State

### Existing Token Types

| Type | Format | Lifetime | Scoping | Use Case |
|------|--------|----------|---------|----------|
| User JWT | Hub-signed JWT | 15 min (web) / 30 days (CLI) | Full user access | Interactive sessions |
| Agent JWT | Hub-signed JWT with claims | 10 hours | `grove_id` + `AgentTokenScope` list | Agent-to-Hub communication |
| API Key | `sk_live_<base64>` | Configurable expiry | `[]string` scopes (unstructured) | Programmatic access (partially implemented, **never used — to be removed**) |
| Dev Token | `scion_dev_<hex>` | None | Full dev-user access | Local development |

### Existing Infrastructure

- **`SCION_HUB_TOKEN`**: Environment variable already consumed by CLI/hubclient for bearer auth
- **`APIKeyService`** (`pkg/hub/apikey.go`): Full CRUD, SHA-256 hashed storage, revocation, expiry — **to be removed** (never used)
- **`APIKeyStore`** (`pkg/store/store.go`): SQLite-backed persistence for API keys — **to be removed** (never used)
- **`UnifiedAuthMiddleware`** (`pkg/hub/auth.go`): Already dispatches on token type/prefix
- **`AuthzService`** (`pkg/hub/authz.go`): Policy-based authorization with actions (`create`, `read`, `update`, `delete`, `list`, `start`, `stop`, `message`, `attach`, `dispatch`, etc.) and resource types (`agent`, `grove`, `policy`, `group`)
- **Hub API endpoints**: `GET/POST /api/v1/auth/api-keys`, `DELETE /api/v1/auth/api-keys/{id}` — **to be removed** with API key deprecation
- **Web profile section**: Exists at `/profile` with nav for env vars, secrets, settings — but no token management UI yet

### Agent Token Scopes (for reference)

```
agent:status:update    agent:log:append     agent:token:refresh
grove:secret:read      grove:agent:create   grove:agent:lifecycle
grove:agent:notify     grove:gcp:token:<sa-id>
```

### AuthzService Actions (for reference)

```
create  read  update  delete  list  manage
start   stop  message attach  dispatch  stop_all
register  addMember  removeMember  verify
```

## Design

### Token Model: User Access Tokens (UATs)

User Access Tokens are opaque bearer tokens that carry grove-scoped, action-limited permissions. They are stored hashed (like API keys) and validated at the middleware layer.

#### Token Format

```
scion_pat_<base64url-encoded-random-32-bytes>
```

- **Prefix `scion_pat_`**: Distinguishes from API keys (`sk_live_`), dev tokens (`scion_dev_`), and JWTs. `pat` = Personal Access Token.
- **Body**: 32 bytes of cryptographic randomness, base64url-encoded (43 chars).
- **Full length**: ~53 characters.

The prefix enables the `UnifiedAuthMiddleware` to route to the correct validation path without ambiguity.

#### Storage Model

Extend or create a new table alongside `api_keys`:

```go
// UserAccessToken represents a scoped personal access token.
type UserAccessToken struct {
    ID          string     `json:"id"`                    // UUID
    UserID      string     `json:"userId"`                // FK to User.ID
    Name        string     `json:"name"`                  // User-provided label
    Prefix      string     `json:"prefix"`                // First N chars for identification
    KeyHash     string     `json:"-"`                     // SHA-256 hash (never exposed)

    // Scoping
    GroveID     string     `json:"groveId"`               // Required: grove this token is scoped to
    Scopes      []string   `json:"scopes"`                // Action scopes (see below)

    // Lifecycle
    Revoked     bool       `json:"revoked"`
    ExpiresAt   *time.Time `json:"expiresAt,omitempty"`   // Required for UATs
    LastUsed    *time.Time `json:"lastUsed,omitempty"`
    Created     time.Time  `json:"created"`
}
```

#### Capability Scopes

Scopes are defined as `resource:action` pairs, derived from the existing `AuthzService` action constants and resource types. A selector is derived from its permission ID: every segment but the last joined with `_`, a colon, then the last segment (`agent.create` gives `agent:create`, `hub.config.read` gives `hub_config:read`). For the initial implementation, scopes map to what the system can currently enforce:

| Scope | Permits | Typical CI/CD Use |
|-------|---------|-------------------|
| `grove:read` | Read grove metadata | Status dashboards |
| `agent:create` | Create agents in the scoped grove | Dispatch CI agents |
| `agent:read` | Read agent status/metadata | Monitor agent progress |
| `agent:list` | List agents in the scoped grove | Enumerate active agents |
| `agent:start` | Start/restart agents | Re-run failed agents |
| `agent:stop` | Stop agents | Cancel running agents |
| `agent:delete` | Delete agents | Cleanup after CI runs |
| `agent:message` | Send messages to agents | Provide input to agents |
| `agent:attach` | Attach to agent sessions | Interactive debugging |
| `agent:dispatch` | Dispatch agents (create + start) | Primary CI/CD action |

A convenience alias `agent:manage` grants `agent:create`, `agent:read`, `agent:list`, `agent:start`, `agent:stop`, `agent:delete`, `agent:dispatch`.

Scopes are validated at token creation time against a known allowlist. Unknown scopes are rejected.

#### Authentication Flow

```
Client (CI/CD)
    │
    │  Authorization: Bearer scion_pat_<token>
    │  ─── or ───
    │  SCION_HUB_TOKEN=scion_pat_<token>
    ▼
UnifiedAuthMiddleware
    │
    ├─ Detect prefix "scion_pat_"
    ├─ SHA-256 hash the token
    ├─ Look up hash in user_access_tokens table
    ├─ Check: not revoked, not expired
    ├─ Load user record
    ├─ Build UserIdentity with grove + scope constraints
    │
    ▼
Handler / AuthzService
    │
    ├─ Check: request grove matches token's grove_id
    ├─ Check: request action is in token's scopes
    ├─ Deny if either check fails
    │
    ▼
Response
```

The key difference from API keys: UATs produce a **scoped** `UserIdentity` that carries grove and action restrictions. The `AuthzService.CheckAccess` path is augmented to enforce these constraints before evaluating policies.

### API Endpoints

Build on the existing `/api/v1/auth/` namespace:

```
POST   /api/v1/auth/tokens            Create a new user access token
GET    /api/v1/auth/tokens            List user's access tokens
GET    /api/v1/auth/tokens/{id}       Get token details
DELETE /api/v1/auth/tokens/{id}       Delete (permanently remove) a token
POST   /api/v1/auth/tokens/{id}/revoke  Revoke a token (soft-delete)
```

All endpoints require user authentication (interactive session or existing valid token — but UATs cannot create other UATs).

#### Create Token Request

```json
{
  "name": "ci-deploy-token",
  "groveId": "grove-uuid-here",
  "scopes": ["agent:dispatch", "agent:read", "agent:stop"],
  "expiresAt": "2026-06-18T00:00:00Z"
}
```

#### Create Token Response

```json
{
  "token": "scion_pat_abc123...",
  "accessToken": {
    "id": "uuid",
    "name": "ci-deploy-token",
    "prefix": "scion_pat_abc1...",
    "groveId": "grove-uuid-here",
    "scopes": ["agent:dispatch", "agent:read", "agent:stop"],
    "expiresAt": "2026-06-18T00:00:00Z",
    "created": "2026-03-18T12:00:00Z"
  }
}
```

The plaintext `token` value is shown **only once** in the create response.

### CLI Commands

```
scion hub token create --grove <grove> --name <name> --scopes <scope,...> [--expires <duration|date>]
scion hub token list [--grove <grove>]
scion hub token revoke <token-id>
scion hub token delete <token-id>
```

#### Examples

```bash
# Create a token for CI that can dispatch and monitor agents, expires in 90 days
scion hub token create \
  --grove my-project \
  --name "github-actions" \
  --scopes agent:dispatch,agent:read,agent:stop \
  --expires 90d

# List tokens
scion hub token list

# Revoke a token
scion hub token revoke tok_abc123

# Use the token in CI
export SCION_HUB_TOKEN=scion_pat_...
scion hub agent dispatch --grove my-project --template default --task "Run tests"
```

### Web UI

Add a **"Access Tokens"** page to the profile section:

- **Route**: `/profile/tokens`
- **Navigation**: Add "Access Tokens" entry to `scion-profile-nav` under the Configuration section
- **Components**:
  - Token list table: name, prefix, grove, scopes, created, last used, expires, revoke button
  - Create token dialog: name input, grove selector, scope checkboxes, expiry date picker
  - One-time token display modal after creation (with copy button)

### Implementation Plan

#### Phase 1: Backend (Store + Service + Auth) ✅

1. ✅ **Remove legacy API key system**: Deleted `APIKeyService` (`pkg/hub/apikey.go`), `APIKeyStore` interface and SQLite implementation, API key endpoints (`/api/v1/auth/api-keys`), and all related `sk_live_*` handling from `UnifiedAuthMiddleware`.
2. ✅ Add `UserAccessToken` model to `pkg/store/models.go`
3. ✅ Add `UserAccessTokenStore` interface to `pkg/store/store.go`
4. ✅ Implement SQLite storage in `pkg/store/sqlite/` (migration V34)
5. ✅ Create `UserAccessTokenService` in `pkg/hub/useraccesstoken.go`
   - `CreateToken(ctx, userID, name, groveID, scopes, expiresAt)` → (plaintext, *UserAccessToken, error)
   - `ValidateToken(ctx, token)` → (ScopedUserIdentity, error)
   - `ListTokens(ctx, userID)` → ([]UserAccessToken, error)
   - `RevokeToken(ctx, userID, tokenID)` → error
   - `DeleteToken(ctx, userID, tokenID)` → error
   - Enforce limit of **50 tokens per user**
   - Enforce maximum expiry of **1 year**, default to **90 days** if not specified
6. ✅ Extend `UnifiedAuthMiddleware` to detect `scion_pat_` prefix and validate via the new service
7. ✅ **Enforce UAT-creates-UAT prevention at the handler level**: Reject requests authenticated with `scion_pat_*` tokens on the `/api/v1/auth/tokens` creation endpoint
8. ✅ Introduce `ScopedUserIdentity` that wraps `UserIdentity` with grove/scope restrictions
9. ✅ Augment `AuthzService.CheckAccess` to enforce grove + scope constraints when identity is scoped
10. ✅ **Add auth-type to request logging**: Include the authentication method (JWT, UAT, dev-token) in standard request log entries
11. ✅ Register API handlers on the server router

#### Phase 2: CLI Commands ✅

1. ✅ Add `cmd/hub_token.go` with `scion hub token {create,list,revoke,delete}` subcommands
2. ✅ Add hubclient `TokenService` interface and implementation in `pkg/hubclient/tokens.go`
3. ✅ Register `Tokens()` on the `Client` interface in `pkg/hubclient/client.go`

#### Phase 3: Web UI ✅

1. ✅ Add profile token list page component (`web/src/components/pages/profile-tokens.ts`)
2. ✅ Add create token dialog component with grove selector, scope checkboxes, expiry picker, and one-time token reveal modal (`web/src/components/shared/token-list.ts`)
3. ✅ Add to profile navigation, routing, and shell title mapping

## Alternatives Considered

### A. Reuse Existing API Keys (`sk_live_*`)

**Approach**: Add `groveId` field to the existing `APIKey` model and enforce grove-scoping in the validation path.

**Pros**:
- Less new code; reuses existing storage, service, and endpoints.
- Already wired into `UnifiedAuthMiddleware`.

**Cons**:
- API keys were designed as general-purpose credentials without grove scoping. Retrofitting adds conditional logic throughout the validation path.
- The `sk_live_` prefix conveys a different semantic (Stripe-style API key) than a scoped access token.
- Existing API key consumers (if any) would need migration or compatibility handling.
- Scopes on API keys are currently unstructured `[]string` with no validation or enforcement.

**Verdict**: Rejected. The semantic mismatch and lack of grove-scoping in the existing model make this fragile. A clean model is worth the modest additional code. Furthermore, the existing `sk_live_*` API key system has never been used and will be fully removed as part of this work (see Phase 1).

### B. Mint Long-Lived User JWTs

**Approach**: Generate long-lived JWTs (like the CLI token type, 30-day) with grove and scope claims baked in.

**Pros**:
- Stateless validation (no DB lookup per request).
- Reuses existing `UserTokenService` and JWT infrastructure.
- Already handled by `UnifiedAuthMiddleware`.

**Cons**:
- **Irrevocable**: JWTs cannot be revoked without maintaining a blacklist, which negates the stateless advantage.
- **Size**: JWTs are ~500+ chars vs ~53 chars for opaque tokens, awkward in env vars.
- **Rotation**: Cannot update scopes on an existing token; must issue a new one.
- **Leak impact**: A leaked JWT is valid until expiry with no way to invalidate.

**Verdict**: Rejected. Revocability is a hard requirement for tokens used in CI/CD systems where secrets may be accidentally exposed in logs.

### C. Extend Agent Token System for Users

**Approach**: Use the `AgentTokenService` to mint JWTs for human users in automation contexts, reusing `AgentTokenClaims` with its scope system.

**Pros**:
- Agent tokens already have well-defined scopes and grove binding.
- Minimal new token infrastructure.

**Cons**:
- Conflates agent and user identity. Handlers that check `GetAgentFromContext()` vs `GetUserIdentityFromContext()` would need rework.
- Agent scopes (`agent:status:update`, `grove:agent:create`) are from the agent's perspective, not the user's.
- Same JWT irrevocability problem as alternative B.
- Agent tokens are designed to be provisioned by the system, not self-service by users.

**Verdict**: Rejected. Identity type confusion would create subtle authorization bugs.

### D. OAuth Client Credentials Grant

**Approach**: Implement the OAuth 2.0 Client Credentials flow. Users register a client (client_id + client_secret), then exchange credentials for short-lived access tokens.

**Pros**:
- Standards-based, well-understood by CI/CD systems.
- Short-lived tokens reduce leak exposure.
- Revocation is simple (revoke the client registration).

**Cons**:
- Two-step flow: CI must first exchange credentials, then use the token. Adds latency and complexity to pipelines.
- Requires client registration management (another CRUD surface).
- Over-engineered for the current use case where a simple bearer token suffices.

**Verdict**: Deferred. Could be a future enhancement for enterprises, but the PAT model is simpler and sufficient for the initial use case.

## Resolved Decisions

1. **Maximum token count per user**: 50 tokens per user, configurable.

2. **Maximum expiry duration**: 1 year max, with a default of 90 days if not specified.

3. **Legacy API keys (`sk_live_*`)**: Fully deprecate and remove. The existing API key system (`APIKeyService`, `APIKeyStore`, related endpoints and middleware handling) has never been used and will be deleted as part of Phase 1. UATs are the sole non-JWT programmatic auth mechanism going forward.

4. **Cross-grove tokens**: Single-grove scoping only. One token per grove; multi-grove support may be added later if needed.

5. **Audit logging**: Include the auth-type (JWT, UAT, dev-token) in standard request log entries. No separate audit trail for now; `lastUsed` timestamp is updated on each use.

6. **UAT-creates-UAT prevention**: Enforced at the middleware level. Requests authenticated with `scion_pat_*` tokens are rejected on the token creation endpoint.

7. **Scope granularity evolution**: Scopes remain coarse capability gates. The policy engine handles fine-grained decisions within those gates. Scopes can evolve as the authorization system matures.

## Credential Decoration: Bounded Purpose and Labels (E.1, ptone/scion#2126)

**Status:** Approved (part of the access-token-relationship-refactor tracker, execution area E).

Simple compute automation (CI/CD, scheduled jobs) commonly reuses one human's
UAT across several purposes. E.1 lets the issuer attach optional, bounded,
purely descriptive metadata to a token at mint time, so that reused-by-the-
same-human tokens are distinguishable in logs and audit records — without
ever treating that metadata as a verified actor, ancestry, or authorization
signal. The authenticated principal is always the human user; a label such
as `nightly-cleanup` is a hint for a log reader, never an agent identity. A
static guard test scans every production file in `pkg/hub` for the known
ways decoration can be carried or read — the type itself, a field/embed/alias
declaration naming it, or a call to one of its rendering functions — and
fails if any of those appears outside the documented carriage and rendering
points, so authorization code cannot branch on it without the guard catching
the new reference (see `pkg/hub/credential_decoration_authz_guard_test.go`).

### Fields

- `purpose` (optional, string): a free-text description of what the token is
  for, e.g. "nightly cleanup automation".
- `labels` (optional, map of string to string): issuer-supplied key/value
  annotations, e.g. `{"team": "platform", "env": "prod"}`.

Both are set only at **issuance** (`POST /api/v1/auth/tokens`, `scion hub
token create --purpose/--label`) and are **immutable afterward** — there is
no update endpoint or CLI command. To change them, create a replacement
token and revoke the old one. This is enforced at both the service layer
(`ValidateCredentialMetadata` runs once, at mint) and the store layer (the
`purpose`/`labels` ent fields are `Immutable()`, so no generated update
setter exists for them).

### Bounded schema

| Item | Rule |
|---|---|
| `purpose` | optional; ≤ 128 bytes UTF-8 after trim; must be valid UTF-8; reject Unicode categories Cc (control), Cf (format, including bidi overrides and zero-width characters), Zl (line separator, U+2028) and Zp (paragraph separator, U+2029) — i.e. genuinely a single line |
| label count | ≤ 8 labels per token |
| label key | must be valid UTF-8; must match `^[a-z][a-z0-9_.-]{0,31}$` (≤ 32 bytes, lowercase ASCII, digits, `_`, `.`, `-`) |
| label value | must be valid UTF-8; ≤ 64 bytes; charset `[A-Za-z0-9 _.:/@+=,-]`; no leading/trailing whitespace; empty value allowed |
| token `name` (existing field) | **new tokens:** ≤ 128 bytes, must be valid UTF-8, reject Cc/Cf/Zl/Zp, reject text resembling a pasted secret. **Existing rows (minted before E.1):** accepted unchanged; sanitized at render time instead |
| secret-looking text | any of `name`/`purpose`/a label key or value containing `scion_pat_` or `Bearer ` (case-insensitive) is rejected — a cheap, deliberately non-exhaustive check against accidentally pasting a token or an `Authorization` header value |

**Reserved label keys** — rejected exactly or as a dotted prefix (e.g. `user`
also rejects `user.name`), case-insensitively, with `-` normalized to `_`
first (so `actor-binding` and `actor_binding` are both caught):

```
agent  agent_id  actor  actor_binding  principal  principal_id  principal_kind
user  user_id  email  on_behalf_of  delegate  delegator  delegation  ancestry
creator  created_by  owner  project_id  broker_id  credential  credential_id
token  token_id  role  scope  scopes  permission  permissions  verified
system  executor  initiator
actor_agent_id  authorizing_user_id  source_grant_id  delegation_edge_id
parent_grant_id  exchange_agent_credential_id  actor_kind
```

`actor_binding` and the seven `actor_agent_id`…`actor_kind` names are reserved
for a future verified-agent-actor extension (execution area G); they are not
themselves implemented by E.1. The seven are exact matches only — no
`actor_`/`source_` prefix is reserved — and are the single canonical list a
same-package test pins against G's structured audit-record field names.

In addition, any label key starting with `scion.`, `hub.`, or `x-` (again
case-insensitively) is rejected outright, reserving those namespaces for the
platform and for future header-derived metadata.

### Validation errors never echo the offending value

A validation failure returns `400 validation_error` with a message naming
the **field** and the **rule** that failed (e.g. "invalid labels: label key
is reserved") — never the value that violated it. This keeps a rejected
purpose or label out of error responses and logs, since that text is
untrusted and unbounded until it passes validation.

### Rendering (logs)

Decoration renders as a `slog` group (`credential.*`): `kind`, `id`, `name`,
`boundary.kind`, `boundary.project_id`, `purpose`, and labels nested under
`credential.labels.*` with a constant `credential.labels_source="issuer"`
marker — never promoted to top-level log attributes, so a label can never
collide with (or be mistaken for) a real `agent_id`/`user_id`/`principal_kind`
field. Rendering always re-sanitizes text (replacing Cc/Cf/Zl/Zp runes with
U+FFFD and truncating with a `…` marker), because a row created before this
validation existed cannot be assumed to already satisfy the bounded schema.
The token's plaintext value, its SHA-256 hash, and its visible prefix are
never part of the decoration and never logged this way.

### Non-goals

Purpose and labels never grant, narrow, or verify anything: they carry no
permissions, do not affect `Decide`, `CanDelegate`, capabilities, or
delegation-edge attribution, and are not a substitute for the agent-bound
delegation extension (area G) that will add a separately verified actor
binding. See `pkg/hub/credential_decoration_authz_guard_test.go` for the
mechanical enforcement of "never read by authorization code."

## Credential Attribution in Logs and Audit (E.2a, ptone/scion#2127)

E.2a threads E.1's decoration through request logs, decision audit, and
mutation audit, so the same human using two different tokens is
distinguishable end to end — including in denied and rejected requests.

### Request log

- `RequestLogMiddleware` now wraps `UnifiedAuthMiddleware` instead of being
  wrapped by it, so a request auth rejects outright (invalid, revoked, or
  expired UAT; suspended user) is still request-logged, not silently dropped.
  Auth records the outcome via `logging.SetRequestAuth` on the shared,
  mutable `*RequestMeta` this middleware installs; `auth_type` and any
  `user_id`/`principal_kind`/`credential` attributes are read back after the
  handler chain returns, not before.
- A rejected UAT produces one `"credential rejected"` line with `auth_type=uat`
  and `reason` ∈ `{invalid, revoked, expired, user_suspended,
  reserved_identity, lookup_error}`, at Warn level except `lookup_error`,
  which logs at Error level: it means the token store or database call itself
  failed, not that a client presented a bad credential, and it never carries
  `credential.id`. `credential.id` is otherwise included only when the
  presented value matched a stored token row (revoked/expired/suspended) — an
  unknown or malformed bearer value never yields an asserted identity, not
  even a rejected one. The presented token string is never logged, in either
  case.

### Decision and mutation audit

- `AuthzService.Decide` is a thin wrapper around the kernel evaluation
  (`decide`) that emits exactly one decision audit record per call, whichever
  internal return path produced the result — including the UAT
  project/credential-scope pre-kernel gate.
- Decision audit records carry a `permission_id`: exactly the caller-supplied
  `AuthzRequest.Permission`, and only when it is a canonical ID present in
  the permissions registry — never derived from resource/action, and never
  an unregistered string. Both decision and mutation audit records carry: a
  bounded, sanitized snapshot of the credential's name/boundary/labels at
  decision time (audit rows outlive tokens, which can be deleted); and a
  `correlation_id` equal to the request's ID, so a decision audit row and the
  mutation audit row(s) it authorized can be joined.
- `executor_kind`/`executor_id` distinguish what is currently executing from
  the initiating principal/credential above. They are empty for an ordinary
  live request; deferred-execution entry points (scheduler, schedule
  evaluator, broker dispatch — tracker E.2b) set them via
  `hub.ContextWithExecutor`.
- `AuthzRequest.AlwaysAudit` and its counterpart `Decision.AlwaysAudit`
  (settable from inside `decide`'s body, for a branch that only learns
  partway through evaluation that this decision must not be sampled away)
  force an allow decision to be audited regardless of the configured
  allow-sampling rate.
- `hub.BuildDecisionAuditRecord(ctx, request, decision)` is exported so a
  non-`Decide` decision-audit path can build a schema-consistent record
  through the same field mapping, instead of hand-assembling one.

### Retention

Decision and mutation audit records have **no retention sweep** as of E.2a.
`CleanupAuditRecords` exists but has no production caller, and
`ServerConfig.AuditRetentionDays` is read nowhere. E.2a does not change this;
wiring a sweep is tracked as a separate follow-up issue (rulings Q6).

### How this differs from a future verified agent actor (G)

E's decoration and audit snapshot are an unverified, issuer-supplied label on
an ordinary human bearer token — never a verified actor identity. A later
delegated-agent extension (area G) records a *separately verified* actor
through its own columns (`actor_agent_id`, `authorizing_user_id`,
`source_grant_id`, `delegation_edge_id`) and its own `CredentialKind`
(`delegated_agent`), added to the same `store.DecisionAuditRecord` /
`store.MutationAuditRecord` types in G's own migration, in G's own field
block. G's fields are written only by G's code paths; E's writers leave them
zero — `pkg/hub/e2a_no_g_column_test.go` runs E's decision- and
mutation-audit writers with a decoration carrying every name in
`gVerifiedActorFieldNames` (E.1's reserved label-key set, above) and asserts
those fields stay zero (once they exist) and that the label values surface
only inside the bounded `credential_labels` snapshot, never in a principal or
credential ID field. A separate test pins the two lists together so they
cannot silently drift apart. A decoration label such as
`"nightly-cleanup-agent"` therefore always stays under `credential.labels.*`
with `labels_source=issuer`, and cannot occupy where G's verified actor
fields will live.

`Decision.DeniedBy` (a typed denial-source string, owned by a parallel
authorization-kernel change) is recorded verbatim in a `denied_by` audit
column when set. G's aggregated per-list capability-filter record leaves
`denied_by` empty by agreement, recording its own fine-grained code in its
own audit block instead.
