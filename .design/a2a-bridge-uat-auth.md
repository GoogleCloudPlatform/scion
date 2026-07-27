# Design: Per-User Access Token Auth for the A2A Bridge

**Status:** Draft  
**Date:** 2026-07-24  
**Author:** a2a-fed-arch  
**Related issue:** #184 — A2A federation for Claude Desktop / Codex Desktop  

---

## Problem & Goals

The Scion A2A bridge (`extras/scion-a2a-bridge/`) exposes Scion agents as A2A protocol endpoints. Today it authenticates all callers with a single shared static API key. From the Hub's perspective, every external A2A caller shares one admin identity — the bridge's configured `hub.user`. This makes it impossible to:

1. Audit which end-user sent which message to an agent
2. Enforce per-user scope or project restrictions at the Hub layer
3. Allow individual Claude Desktop or Codex Desktop users to use their own Scion credentials

**Goals:**
- Add `hubUAT` auth scheme to the bridge: accept Scion user access tokens (`scion_pat_*`) from A2A callers, validate them via the Hub, and propagate the caller's identity to Hub API calls.
- Add `hubJWT` auth scheme as a secondary option: accept Scion user JWTs (CLI or web login tokens) validated locally using the Hub's signing key.
- Per-user task isolation at the bridge layer: callers can only read or cancel their own tasks.
- Full identity propagation to the Hub: Hub audit logs and authz decisions reflect the real calling user, not the bridge admin.
- Document the desktop app onboarding flow.
- Preserve backward compatibility: existing `apiKey`, `bearer`, and `none` schemes continue to work unchanged.

**Success criteria:**
- A Claude Desktop user with a `scion_pat_*` token scoped to `agent:message` can successfully call `message/send` via the bridge, and the Hub's message delivery log shows the correct user identity.
- A different user's task is not visible to the first user via `tasks/get`.
- An invalid or revoked UAT is rejected at the bridge with HTTP 401.
- A UAT without `agent:message` scope is rejected by the Hub with a 403 that the bridge maps to an A2A error.

---

## Non-Goals

- OAuth / PKCE token exchange at the bridge. Desktop apps obtain their UAT directly from the Hub UI (or Hub CLI) before calling the bridge.
- New token types. This design uses the existing `scion_pat_*` UAT system without modification.
- Issuing tokens on behalf of callers. The bridge validates tokens; it does not issue them.
- Modifying the Hub's token issuance flow.
- Per-agent UATs (a different scoping model). This design uses project-scoped UATs.
- Agent-principal federation (mapping external A2A identities to Scion's agent principal type — tracked separately in `agent-authz-arch`).

---

## Proposed Design

### 3.1 Overview

```
A2A Desktop Client
    │  Authorization: Bearer scion_pat_... (hubUAT)
    │  or Authorization: Bearer <HS256 JWT> (hubJWT)
    ▼
scion-a2a-bridge — authMiddleware
    │  1. Detect token type (UAT prefix vs JWT shape)
    │  2a. hubUAT: call Hub GET /api/v1/auth/me with the UAT (cached 60s)
    │  2b. hubJWT: validate JWT locally using Hub signing key
    │  3. Populate CallerIdentity in request context
    ▼
bridge.SendMessage (per-request Hub client)
    │  hubUAT: hubclient.New(endpoint, WithBearerToken(callerUAT))
    │  hubJWT: hubclient.New(endpoint, WithAuthenticator(mintingAuth(callerIdentity)))
    │  Sender: "user:<callerEmail>"
    │  Broker subscription: AllUserTopic(projectID)
    ▼
Scion Hub — UnifiedAuthMiddleware
    │  Validates UAT (DB lookup) or JWT (local verify)
    │  enforceUATConstraints: project-pinned, scope-checked
    ▼
Scion Agent
    │  Replies to scion.project.<projectID>.user.<callerEmail>.messages
    ▼
scion-a2a-bridge — AllUserTopic wildcard subscription catches reply
    │  a2aTaskId in metadata routes to correct waiter
    ▼
A2A Desktop Client
```

The bridge maintains two Hub client types:
- **Admin client** (existing, unchanged): used for read-only Hub operations (agent listing, context resolution, auto-provisioning). Created at startup with the bridge's own identity.
- **Per-request caller client** (new): created per-request in `hubUAT` / `hubJWT` mode. Used only for Hub write operations (SendStructuredMessage, task interrupt on cancel).

### 3.2 Identity-Propagation Decision

The core question from the brief: does the bridge pass the caller's UAT through to the Hub, or mint a short-lived JWT after validating the UAT once?

**Recommendation: UAT passthrough for `hubUAT`, JWT re-mint for `hubJWT`.**

Rationale:

For `hubUAT`:
- The Hub already validates UATs natively via `UATSvc.ValidateToken` (DB lookup). Presenting the UAT on each Hub API call causes Hub-side authz (`enforceUATConstraints`) to fire on every write — which is exactly what we want. The UAT's project binding and scope constraints are enforced at the Hub layer without any bridge-side re-implementation.
- Immediate revocation: if the UAT is revoked, the next Hub API call fails within one cache TTL (60 seconds). A re-minted JWT alternative would keep working until the short-lived JWT expires.
- No new Hub code required.

For `hubJWT`:
- User JWTs are validated locally in the bridge using the Hub signing key (the bridge already has it). The bridge then re-mints a fresh 5-minute JWT for the same user identity using `identity.TokenMinter`. This is already how the bridge authenticates itself to the Hub — it's the same infrastructure, just for a different user identity.
- Avoids a network round-trip for JWT validation.
- Limitation: no immediate revocation (JWTs are not checked against the DB). Acceptable for a secondary scheme.

For both schemes, **the bridge does not act as a token exchange endpoint**. It receives a credential, validates it, and uses it (or a derived credential) for Hub calls.

### 3.3 UAT Introspection: Why No New Hub Endpoint Is Needed

The brief asked whether the Hub needs a new UAT introspection endpoint for the bridge to call.

Answer: **No.** The existing `GET /api/v1/auth/me` endpoint already serves as a UAT introspection endpoint for the bridge's purposes. When the bridge calls it with a UAT bearer token, the Hub's `UnifiedAuthMiddleware` validates the UAT via `UATSvc.ValidateToken` (DB lookup, same path as all other Hub API requests) and returns the user's ID, email, display name, and role.

`/api/v1/auth/validate` was ruled out because it only validates JWTs (`UserTokenService.ValidateUserToken`), not UATs — confirmed in `pkg/hub/handlers_auth.go:handleAuthValidate`.

The bridge caches `/api/v1/auth/me` responses to avoid a DB round-trip on every A2A message, with a 60-second TTL keyed by SHA-256(token). This means a revoked UAT stops working within 60 seconds of revocation.

The `UserResponse` from `/api/v1/auth/me` includes `{ID, Email, DisplayName, Role}`. The bridge does not receive UAT scopes from this call — scope enforcement is left to the Hub's `enforceUATConstraints` which runs on each Hub API call the bridge makes with the caller's UAT. If the UAT lacks `agent:message` scope, the Hub returns a 403 which the bridge maps to an A2A error response.

### 3.4 New Auth Schemes: `hubUAT` and `hubJWT`

**`AuthConfig` struct changes** (`internal/bridge/config.go`):

```go
type AuthConfig struct {
    // Scheme selects the auth mode.
    // Existing: "apiKey" | "bearer" | "none"
    // New:      "hubUAT" | "hubJWT"
    Scheme string `yaml:"scheme"`

    // APIKey is the shared static key for "apiKey" and "bearer" schemes.
    // Not used for hubUAT or hubJWT.
    APIKey string `yaml:"api_key"`

    // UATCacheTTL is the UAT introspection cache TTL for hubUAT mode.
    // Default: 60s. Maximum: 300s.
    UATCacheTTL time.Duration `yaml:"uat_cache_ttl"`
}
```

`ValidateConfig` changes:
- Accept `"hubUAT"` and `"hubJWT"` as valid scheme values.
- `api_key` is NOT required for `hubUAT` or `hubJWT`.
- For `hubJWT`, the Hub signing key must be present (`cfg.Hub.SigningKey` or `cfg.Hub.SigningKeySecret`) — already validated for the existing minter path, so no new validation needed.
- For `hubUAT`, no additional config fields are required beyond the existing `hub.endpoint`.

**Sample config** (`scion-a2a-bridge.yaml.sample`) additions:

```yaml
auth:
  # Per-user UAT auth. Callers present "Authorization: Bearer scion_pat_..."
  # The bridge validates each token via Hub /api/v1/auth/me (60s cache).
  scheme: "hubUAT"
  # uat_cache_ttl: 60s   # optional: default 60s, max 300s

  # --- OR: per-user JWT auth (for CLI/scripted access) ---
  # scheme: "hubJWT"
  # The hub.signing_key is used for local JWT validation. No api_key needed.

  # --- Backward-compatible shared key (existing deployments) ---
  # scheme: "apiKey"
  # api_key: "${A2A_API_KEY}"
```

### 3.5 `CallerIdentity`: Context Propagation

A new `CallerIdentity` struct carries the validated caller identity from `authMiddleware` through to `Bridge.SendMessage`:

```go
// internal/bridge/caller.go (new file)
package bridge

// CallerIdentity holds the per-request caller identity extracted from a
// validated hub credential. Absent for legacy apiKey/bearer/none modes.
type CallerIdentity struct {
    UserID    string
    Email     string
    Role      string
    RawToken  string // The original bearer token for UAT passthrough
    TokenType string // "uat" or "jwt"
}

type callerContextKey struct{}

func withCallerIdentity(ctx context.Context, id *CallerIdentity) context.Context {
    return context.WithValue(ctx, callerContextKey{}, id)
}

func callerIdentityFromContext(ctx context.Context) *CallerIdentity {
    v, _ := ctx.Value(callerContextKey{}).(*CallerIdentity)
    return v
}
```

**Context flow:**
1. `authMiddleware` (in `server.go`) validates the token, constructs `CallerIdentity`, injects into `r.Context()`.
2. `handleJSONRPC` already propagates `r.Context()` to the SDK handler via `r.WithContext(ctx)`.
3. The SDK calls `executor.OnSendMessage` / `executor.OnMessageStream` which receives `ctx`.
4. `ScionExecutor` passes `ctx` to `Bridge.SendMessage`.
5. `Bridge.SendMessage` reads `CallerIdentity` from context to determine Hub client and Sender identity.

### 3.6 `authMiddleware` Changes

The revised `authMiddleware` in `server.go` adds two branches before the existing static-key comparison:

```
switch cfg.Auth.Scheme:

case "hubUAT":
    token = extractBearerOrAPIKey(r)
    if !strings.HasPrefix(token, "scion_pat_") { → 401 }
    callerID = s.uatValidator.Validate(ctx, token)  // cached /auth/me call
    if err { → 401 }
    inject CallerIdentity{UserID, Email, Role, RawToken: token, TokenType: "uat"}
    next()

case "hubJWT":
    token = extractBearerToken(r)
    claims = s.jwtValidator.ValidateUserToken(token) // local, no network
    if err { → 401 }
    inject CallerIdentity{UserID: claims.UserID, Email: claims.Email,
                          Role: claims.Role, RawToken: token, TokenType: "jwt"}
    next()

case "apiKey", "bearer", "":
    // Existing constant-time SHA-256 compare; no CallerIdentity injected.

case "none":
    next() // pass-through, no CallerIdentity
```

**`UATValidator`** (new, `internal/bridge/uatvalidator.go`):
- Wraps the Hub `/api/v1/auth/me` HTTP call using the admin HTTP transport.
- Caches results in a `sync.Map` keyed by `sha256.Sum256([]byte(rawToken))` (avoids storing plaintext tokens in cache keys).
- Cache entries have a configurable TTL (default 60s, max 300s). A background goroutine or lazy eviction on access cleans expired entries.
- Returns `CallerIdentity` or an error.

```go
type UATValidator struct {
    hubEndpoint string
    httpClient  *http.Client
    ttl         time.Duration
    mu          sync.Mutex
    cache       map[[32]byte]*uatCacheEntry
}

type uatCacheEntry struct {
    identity  *CallerIdentity
    expiresAt time.Time
}

func (v *UATValidator) Validate(ctx context.Context, token string) (*CallerIdentity, error) {
    key := sha256.Sum256([]byte(token))
    // check cache; if hit and not expired, return
    // else: GET hub.endpoint + "/api/v1/auth/me"
    //       Header: Authorization: Bearer <token>
    // parse UserResponse → CallerIdentity{RawToken: token, TokenType: "uat"}
    // store in cache with expiresAt = now + ttl
}
```

**`JWTValidator`** (thin wrapper around the existing `UserTokenService`):
- In `hubJWT` mode, the bridge constructs a `hub.UserTokenService` from the same signing key it already uses for minting. No new key material needed.
- `ValidateUserToken` is already implemented (`pkg/hub/usertoken.go`). The bridge imports and calls it directly.
- The bridge's `go.mod` already imports `pkg/hub` (confirmed via import of `pkg/hubclient`). If it doesn't import `pkg/hub` directly, a thin copy of the JWT validation logic into `internal/bridge/jwtvalidator.go` avoids the import cycle (the bridge is in `extras/`, not `pkg/`, so there is no cycle — confirm before implementation).

### 3.7 Hub API Call Routing

**`Bridge.SendMessage` changes** (`internal/bridge/bridge.go`):

```go
func (b *Bridge) SendMessage(ctx context.Context, projectSlug, agentSlug, contextID,
    existingTaskID string, parts []Part, blocking bool) (*TaskResult, error) {

    caller := callerIdentityFromContext(ctx) // nil in legacy mode

    // writeClient: used for Hub write operations (send message, cancel interrupt).
    // In per-user mode, this is a short-lived client authenticated as the caller.
    // In legacy mode, it's the bridge admin client.
    writeClient := b.hubClient // legacy default
    senderUser := b.config.Hub.User

    if caller != nil {
        senderUser = caller.Email
        var err error
        writeClient, err = b.callerHubClient(caller)
        if err != nil {
            return nil, fmt.Errorf("creating per-user hub client: %w", err)
        }
        // Note: writeClient is created here per-message. The hubclient.New
        // call is cheap (no connection made until first request).
    }

    // ... existing context resolution uses b.hubClient (admin, unchanged) ...

    scionMsg.Sender = fmt.Sprintf("user:%s", senderUser)
    scionMsg.Recipient = fmt.Sprintf("agent:%s", agentCtx.AgentSlug)

    // Broker subscription: in per-user mode, subscribe to wildcard
    // AllUserTopic so replies to any user's messages are received.
    if b.broker != nil {
        if caller != nil {
            b.subscribeAllUserTopics(agentCtx.ProjectID)
        } else {
            b.subscribeAdminUserTopics(agentCtx.ProjectID)
        }
    }

    // ... send via writeClient.Agents().SendStructuredMessage ...
}

// callerHubClient creates a per-request Hub client authenticated as the caller.
func (b *Bridge) callerHubClient(caller *CallerIdentity) (hubclient.Client, error) {
    switch caller.TokenType {
    case "uat":
        return hubclient.New(b.config.Hub.Endpoint,
            hubclient.WithBearerToken(caller.RawToken))
    case "jwt":
        // Re-mint a 5-minute JWT for the caller's identity using the bridge's signing key.
        mintAuth := identity.NewMintingAuth(b.minter,
            caller.UserID, caller.Email, caller.Role, 5*time.Minute)
        return hubclient.New(b.config.Hub.Endpoint,
            hubclient.WithAuthenticator(mintAuth))
    default:
        return nil, fmt.Errorf("unknown token type: %s", caller.TokenType)
    }
}
```

**Context resolution and agent lookup** (`resolveContext`, `lookupAgent`) continue to use `b.hubClient` (the admin client). These are read-only operations on the bridge's configured projects, and the admin client has full access. Per-user callers don't need to own the context resolution path — it's a bridge-internal concern.

### 3.8 Broker Subscription: Wildcard Per-Project

**Problem:** Today the bridge subscribes to `UserTopic(projectID, b.config.Hub.User)` — the bridge admin's user topic. If we change `scionMsg.Sender` to the caller's email, agents reply to the caller's user topic, which the bridge's existing subscription doesn't cover.

**Solution:** In `hubUAT`/`hubJWT` mode, subscribe to `AllUserTopic(projectID)` — the wildcard `scion.project.<projectID>.user.*.messages`. This single subscription captures replies from all users, regardless of which user sent the original message. The existing `a2aTaskId` metadata in message replies ensures each reply is routed to the correct waiter.

```go
func (b *Bridge) subscribeAllUserTopics(projectID string) {
    patterns := []string{
        projectcompat.AllUserTopic(projectID),
        projectcompat.LegacyAllUserTopic(projectID), // if it exists; else skip
    }
    for _, pattern := range patterns {
        if err := b.broker.RequestSubscription(pattern); err != nil {
            b.log.Warn("failed to request wildcard subscription", "pattern", pattern, "error", err)
        }
    }
}
```

The wildcard subscription is idempotent (the broker deduplicates repeated subscription requests). It is requested once per project per message, but the underlying subscription persists until the broker connection resets.

**Concern: wildcard subscription traffic.** In a multi-tenant environment, subscribing to `scion.project.<projectID>.user.*.messages` causes the bridge to receive ALL user messages in that project, not just A2A-bridged ones. The bridge already has `a2aTaskId`-based filtering: if a message's `a2aTaskId` is unknown or absent, `dispatchBrokerMessage` drops it with a warning. Net effect: some extra broker messages are received and discarded. This is acceptable given the bridge is a dedicated service for a specific set of configured projects.

**Legacy mode** (`apiKey`/`bearer`/`none`): continues to subscribe to `UserTopic(projectID, b.config.Hub.User)` as today, no change.

**Migration:** If an operator switches from `apiKey` mode to `hubUAT` mode mid-operation, in-flight tasks created under the old mode (with Sender = bridge admin user) will have their agent replies routed to the bridge admin's user topic. The bridge will no longer subscribe to that topic in per-user mode. **Recommendation:** Drain in-flight tasks before switching auth modes, or subscribe to BOTH the admin user topic and the AllUserTopic during a transition window.

### 3.9 Per-User Task Isolation

**State schema change** (`internal/state/state.go`):

```go
type Task struct {
    ID          string
    ContextID   string
    ProjectID   string
    AgentSlug   string
    AgentID     string
    State       string
    CallerUserID string // NEW: empty for legacy-mode tasks
    CreatedAt   time.Time
    UpdatedAt   time.Time
    Metadata    string
}
```

SQLite migration (at startup, existing migration pattern):

```sql
ALTER TABLE tasks ADD COLUMN caller_user_id TEXT NOT NULL DEFAULT '';
```

This is backward-compatible (existing rows get `''`).

**Isolation enforcement:**

- `Bridge.GetTask`: if `CallerIdentity` is in context AND task's `CallerUserID` is non-empty, return 404 if caller's UserID ≠ task's CallerUserID.
- `Bridge.CancelTask`: same check.
- `Bridge.ListTasks`: filter by `CallerUserID` if caller is identified.
- Legacy mode (no CallerIdentity, CallerUserID is `''`): no isolation enforced — behavior unchanged.

Tasks created in legacy mode (`CallerUserID = ''`) are visible to all legacy-mode callers but NOT to per-user callers (the 404 guard only fires if BOTH the context has a caller identity AND the task has a non-empty CallerUserID). This means mixed-mode deployments work safely.

### 3.10 Task Record at CreateTask

In `Bridge.SendMessage`, when creating the task:

```go
task := &state.Task{
    ID:           taskID,
    ContextID:    agentCtx.ContextID,
    ProjectID:    agentCtx.ProjectID,
    AgentSlug:    agentCtx.AgentSlug,
    AgentID:      agentCtx.AgentID,
    State:        TaskStateSubmitted,
    CallerUserID: "", // default
    CreatedAt:    now,
    UpdatedAt:    now,
    Metadata:     "{}",
}
if caller != nil {
    task.CallerUserID = caller.UserID
}
```

### 3.11 ScopedTaskStore and SDK-Level Task Isolation

The bridge uses a `ScopedTaskStore` wrapper around the SDK's in-memory task store (`a2asrv/taskstore`). This store already enforces project/agent routing isolation (task ownership by route key). The `CallerUserID` isolation described in §3.9 is enforced at the `Bridge` layer on `GetTask`/`CancelTask`/`ListTasks` — which wrap the SQLite state store, not the SDK task store. The two isolation layers are complementary.

### 3.12 Documentation Deliverable

Add a new section to `docs-site/src/content/docs/hosted/user/external-channels.md`:

**"Desktop App Federation (Claude Desktop, Codex Desktop)":**

1. Create a Scion UAT with `agent:message` and `agent:read` scopes for your project:
   ```
   scion token create --name "claude-desktop" --project <project-slug> \
     --scope agent:message,agent:read --expires 365d
   ```
2. In Claude Desktop's A2A provider settings, set:
   - Endpoint: `https://<bridge-host>/projects/<project-slug>/agents/<agent-slug>`
   - Auth: Bearer token → paste your `scion_pat_...` token
3. Bridge operator: configure `auth.scheme: hubUAT` (no api_key needed).

The documentation should cover the agent card URL for discovery, the required scopes, and how to test the connection with `curl`.

---

## Alternatives Considered

### Alternative A: New Hub UAT Introspection Endpoint (`POST /api/v1/auth/introspect`)

Create a new endpoint on the Hub (RFC 7662-style) that accepts a token and returns `{valid, user_id, email, role, scopes, project_id}`. The bridge calls this once per request (cached) to get full token metadata including scopes, allowing bridge-level scope pre-validation.

**Rejected because:**
- Requires new Hub code, adding to the scope and creating a Hub dependency.
- The existing `GET /api/v1/auth/me` already provides the user identity the bridge needs (ID, email, role). Scope enforcement at the Hub layer is sufficient — the bridge does not need to pre-validate scopes.
- RFC 7662 is valuable in a multi-service architecture where many services share a token validation endpoint; here the bridge-to-Hub topology is point-to-point and `/auth/me` is simpler.

This alternative is not entirely off the table for a future iteration (e.g., if the bridge needs to make scope-based routing decisions), but it is unnecessary for the current requirement.

### Alternative B: Service Account Model (Bridge Admin Always, User ID in Metadata)

The bridge validates the caller's UAT via `/auth/me` to get identity, but continues to make all Hub API calls as the bridge admin user (`b.hubClient`). The caller's user ID is passed as message metadata (`scionMsg.Metadata["callerUserID"] = callerUserID`).

**Rejected because:**
- Does not close the actual gap. Hub audit logs still show the bridge admin as the message sender, not the real user. Hub-side authz (`enforceUATConstraints`) doesn't run on the caller's scopes — it runs on the bridge admin's permissions. A misconfigured bridge admin could send messages beyond what the UAT's scopes allow.
- The caller's UAT scopes (e.g., `agent:message` only, not `agent:create`) are not enforced at the Hub layer — only at the bridge layer, which is a weaker guarantee.
- Auth-lead explicitly identified this as the gap: "All A2A callers share one undifferentiated Hub identity."

This model is significantly simpler to implement (no per-request Hub clients, no broker subscription changes) and could be a valid "Phase 0" or "escape hatch" if full identity propagation proves operationally difficult. It is preserved as an option but is not the recommended path.

### Alternative C: JWT-Only Auth (No UAT Support)

Add only `hubJWT` mode: validate user JWTs locally using the Hub signing key, re-mint for Hub calls. No `hubUAT` support.

**Rejected because:**
- User JWTs (15 min) and CLI JWTs (30 days) require more frequent rotation than UATs (up to 1 year). Desktop apps benefit from long-lived credentials.
- No immediate revocation for JWTs. If a user's access is revoked in the Hub, their JWT remains valid until expiry.
- UATs are the idiomatic Scion programmatic credential (`scion_pat_*`) and the natural choice for desktop app onboarding. Supporting only JWTs would require users to manage token rotation themselves.
- Both schemes can be supported with modest additional code.

`hubJWT` is kept as a secondary scheme for scripting/CI use cases where JWT rotation is already managed by the tool (e.g., the Scion CLI).

### Alternative D: UAT Validated Entirely by Hub Per-Request (No Bridge Cache)

Skip the `/auth/me` introspection call and just pass the UAT directly to the Hub on every Hub API call (first call is list-agents or send-message). The Hub validates it there. The bridge gets the user identity only implicitly (it doesn't know who the caller is until the Hub rejects or accepts the call).

**Rejected because:**
- The bridge needs the caller's identity BEFORE the Hub call to: (a) set the correct `Sender` field, (b) record `CallerUserID` on the task, (c) enforce per-user task isolation. Without a dedicated introspection step, the bridge is blind to the caller's identity.
- This model would require the bridge to parse the Hub's response to infer user identity, which is fragile.

---

## Migration / Rollout

### Forward-Compat
- Existing deployments using `apiKey` or `bearer` schemes continue to work with zero config changes. The new schemes are opt-in.
- The `CallerUserID` column has a `DEFAULT ''` — existing rows are unaffected.
- Legacy tasks (CallerUserID = '') are not subject to per-user isolation checks. Mixed-mode operation is safe.

### Backward-Compat
- The `hubUAT` scheme does not remove or change the `apiKey` or `bearer` schemes.
- The API key validation code path is unchanged.
- The broker subscription pattern change (`AllUserTopic` vs specific user topic) is scoped to `hubUAT`/`hubJWT` mode. Legacy mode retains the existing subscription behavior.

### Migration Steps for Operators

1. **Bridge operator:** Update bridge config to `auth.scheme: hubUAT`. Remove `auth.api_key`. Ensure `hub.signing_key` is still set (unchanged).
2. **Drain in-flight tasks** before the config change to avoid subscription routing issues (see §3.8).
3. **End users:** Create a UAT in the Scion Hub UI with `agent:message` and `agent:read` scopes, scoped to the target project.
4. **Desktop app:** Set `Authorization: Bearer scion_pat_...` in the A2A provider config.
5. **Verify:** Use the `/healthz` endpoint to confirm the bridge restarted cleanly. Send a test message via `curl` to verify auth succeeds.

### State Database Migration

The SQLite `ALTER TABLE` migration runs at startup if the `caller_user_id` column doesn't exist. No downtime is required — SQLite `ALTER TABLE ADD COLUMN` is instantaneous.

---

## Open Questions

1. **`AllUserTopic` wildcard and legacy broker compatibility:** `projectcompat.AllUserTopic()` exists in the codebase. Does the broker support NATS-style wildcard subscriptions with `*`? Verify with the broker team before implementation. If wildcards are not supported, fall back to Alternative B (service account model) for Phase 1 and add a GitHub issue for wildcard broker support.

2. **`agent-authz-arch` overlap:** Auth-lead flagged an active project designing a role-tier system for agent identities. If that project changes how external identities are represented, this design may need a revision pass. The developer should check `agent-authz-arch`'s current state before implementation and file a follow-up issue if integration work is needed.

3. **Hub client creation cost at scale:** Per-request `hubclient.New()` is cheap (creates a struct, no connection until first HTTP call). At high request rates, connection pool behavior may matter. If profiling shows this is a bottleneck, the bridge can maintain a short-lived per-user client LRU cache (keyed by caller token hash, TTL matching the auth cache TTL). This is a follow-on optimization.

4. **Hosted vs. self-hosted:** The documentation should call out whether the bridge is deployed by the Scion operator (self-hosted) or is a shared hosted service. For the hosted case, the bridge operator's `hub.user` admin identity has access to all projects, which may raise privilege questions. The bridge's `exposed_agents` allowlist and project config already limit which agents are reachable — this is the existing defense-in-depth.

---

## Implementation Phases

### Phase 1: Bridge-Side Auth (No Hub Changes)

**Commits/PRs expected: 3–5**

1. **`config.go`**: Add `hubUAT` and `hubJWT` to `ValidateConfig`. Add `UATCacheTTL` to `AuthConfig`. Remove the `api_key` requirement for the new schemes.

2. **`caller.go`** (new file): Define `CallerIdentity`, context key helpers `withCallerIdentity` / `callerIdentityFromContext`.

3. **`uatvalidator.go`** (new file): Implement `UATValidator` with the `/api/v1/auth/me` introspection call and 60-second TTL cache.

4. **`jwtvalidator.go`** (new file or thin wrapper): Wire up `hub.UserTokenService.ValidateUserToken` for local JWT validation in `hubJWT` mode. If `pkg/hub` cannot be imported from `extras/` (check for import cycles), inline the JWT validation logic using `go-jose` (already a dependency of the bridge via `identity.go`).

5. **`server.go:authMiddleware`**: Add `hubUAT` and `hubJWT` branches. Inject `CallerIdentity` into context. Update `WarnOnOpenAuth` to not warn for the new schemes.

6. **`state.go`**: Add `CallerUserID string` to `Task`. Add the `ALTER TABLE` migration in `state.New`. Add `ListTasksByCallerUser` store method.

7. **`bridge.go`**: 
   - `callerHubClient()`: per-request Hub client factory.
   - `SendMessage`: read `CallerIdentity` from context; set `scionMsg.Sender`; use `writeClient`; record `CallerUserID` on task.
   - `GetTask` / `CancelTask` / `ListTasks`: enforce per-user isolation when `CallerIdentity` is present.
   - `subscribeAllUserTopics()` / `subscribeAdminUserTopics()`: broker subscription helpers.

8. **`scion-a2a-bridge.yaml.sample`**: Add `hubUAT`/`hubJWT` examples.

9. **`docs-site/.../external-channels.md`**: Add desktop app federation guide.

10. **Tests**: 
    - `uatvalidator_test.go`: mock Hub `/auth/me` call, test cache TTL, test revoked UAT rejection.
    - `jwtvalidator_test.go`: test expired JWT rejection, wrong audience rejection.
    - `server_test.go`: table-driven middleware tests for all schemes.
    - `bridge_test.go` (integration): per-user task isolation (user A cannot access user B's tasks).

### Phase 2: Full Identity Propagation (Follow-On)

If Phase 1 uses the service account fallback (Alternative B) for initial safety:

- Switch `SendMessage` to per-request caller Hub clients.
- Switch broker subscription to `AllUserTopic`.
- Update `scionMsg.Sender` to caller's email.

If Phase 1 already implements full identity propagation (recommended), Phase 2 is the optimization pass:
- Per-user Hub client LRU cache (if needed for performance).
- Scope pre-validation at bridge layer (if Hub introspection endpoint is added later).

---

## Acceptance Criteria

The QA tester should verify the following before considering this done:

**Functional correctness**

1. `hubUAT` mode: A valid `scion_pat_*` UAT with `agent:message` scope successfully authenticates a `message/send` call.
2. `hubUAT` mode: An invalid or malformed UAT returns HTTP 401.
3. `hubUAT` mode: A revoked UAT is rejected within 60 seconds of revocation (the cache TTL).
4. `hubUAT` mode: A UAT without `agent:message` scope causes a Hub-side 403 that the bridge maps to an A2A error response (not a bridge crash or silent success).
5. `hubJWT` mode: A valid Scion CLI JWT (HS256, `type: cli`) authenticates successfully.
6. `hubJWT` mode: An expired or tampered JWT is rejected with HTTP 401.
7. Legacy `apiKey` mode: Unaffected by this change — existing shared-key deployments continue to work.
8. Legacy `none` mode: Unaffected.

**Per-user isolation**

9. User A creates a task. User B (different UAT) calls `tasks/get` with User A's task ID and receives a "task not found" response (not A's task data).
10. User A can cancel their own task.
11. User A cannot cancel User B's task.
12. A legacy task (created before this change, with empty CallerUserID) is accessible by legacy callers but not by per-user callers with a populated CallerUserID.

**Identity propagation**

13. After a `message/send` via `hubUAT`, the Scion Hub's message delivery log (or Hub API response) shows the calling user's identity as the message sender, not the bridge admin identity.
14. The task record in the bridge SQLite DB has `caller_user_id` set to the calling user's Hub user ID.

**State migration**

15. Starting the new bridge binary against an existing SQLite database (without the `caller_user_id` column) succeeds without error. Existing tasks are preserved.

**Documentation**

16. The desktop app federation guide in `external-channels.md` is accurate end-to-end: a user can follow the guide from UAT creation through to a successful `message/send` call.

---

*Design doc path: `.design/a2a-bridge-uat-auth.md`*  
*Scratchpad: `/scion-volumes/scratchpad/projects/a2a-federation/`*
