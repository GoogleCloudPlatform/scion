# Auth Proxy Mode (IAP-style header auth)

## Status: Approach approved by @ptone (2026-06-05) — ready for implementation

All open design decisions are resolved (see "Resolved Decisions"). Scope: add an
exclusive **proxy** human-auth mode (Google IAP first) with verified-assertion
provisioning, plus a hub-minted **transport-auth** layer that lets agents traverse
the IAP / Cloud Run-invoker front door (generalizing PR #307).

## Problem Statement

The hub supports two human auth modes today:

1. **Developer / local-workstation auth** — single-user; auth is short-circuited
   through a locally-minted dev token (`scion_dev_*`). See `pkg/hub/devauth.go`,
   `pkg/hub/auth.go:163`.
2. **OAuth login** — full browser/CLI/device flows against Google and GitHub
   (plus a partial custom OIDC provider). The hub exchanges an authorization code
   for provider userinfo, provisions the user, and mints its own session JWT.
   See `pkg/hub/oauth.go`, `pkg/hub/handlers_auth.go`.

We want to add a **third mode: authenticating-proxy mode**, where the hub sits
behind a trusted proxy that has already authenticated the user, and the hub
derives the current user from proxy-supplied request headers. The first concrete
target is **Google IAP** with its signed-header (JWT assertion) format:
https://docs.cloud.google.com/iap/docs/signed-headers-howto

Unlike OAuth, proxy mode has **no login step and no hub-minted session** — the
proxy re-asserts the identity on *every* request. The design must reconcile this
with the hub's existing login-time provisioning/authorization logic.

## Goals

- Verify Google IAP signed headers (`X-Goog-IAP-JWT-Assertion`) cryptographically
  on each request and derive the current user from the verified assertion.
- Provision a user on first sight if they don't exist, subject to the **existing**
  access controls: `admin_emails`, `authorized_domains`, and
  `user_access_mode` (`open` / `domain_restricted` / `invite_only`).
- Make the proxy layer pluggable so non-IAP proxies (Cloudflare Access, ALB OIDC,
  a self-managed sidecar) can be added later without touching the middleware.
- Reuse — not duplicate — the OAuth path's provisioning and authorization logic.

## Non-Goals

- Replacing OAuth or dev auth. Proxy mode is an additional, independently
  selectable mode.
- Implementing the agent/CLI ingress story behind IAP beyond documenting it
  (see Open Question 1). Initial scope is **human web users**.
- A generic SAML/arbitrary-IdP integration.

## Background: what already exists (and its limits)

There is already a shallow proxy path in `UnifiedAuthMiddleware`
(`pkg/hub/auth.go:139-159`):

```go
// Step 3: if no bearer token AND the request came from a trusted-proxy IP,
// build an identity directly from X-Forwarded-User-* headers.
if len(trustedNets) > 0 && isTrustedProxy(r, trustedNets) {
    if user := extractProxyUser(r); user != nil { ... }
}
```

`extractProxyUser` (`pkg/hub/auth.go:379`) reads `X-Forwarded-User-Id/Email/Name/Role`
and synthesizes an `AuthenticatedUser`. The plumbing is useful but **insufficient
for IAP**:

- **No signature verification.** Trust is based solely on the source IP CIDR
  (`TrustedProxies`). IAP instead hands us a *signed* JWT we should verify; IP
  trust alone is brittle (NAT, mesh, misconfig) and is not what IAP expects.
- **No provisioning.** It fabricates an identity from headers and never consults
  the user store — no canonical user UUID, role, or `status` (suspended?) lookup,
  and no create-if-not-exists.
- **No access control.** `domain_restricted` / `invite_only` / `admin_emails` are
  never applied on this path.
- **Header trust mismatch.** IAP's *unsigned* convenience headers
  (`X-Goog-Authenticated-User-Email` / `-Id`) must **not** be trusted; only the
  signed assertion is safe.

Good news — the pieces we need to reuse already exist and are factored:

- Authorization: `checkUserAuthorized(ctx, email, authorizedDomains, adminEmails, accessMode, store)`
  (`pkg/hub/handlers_auth.go:1268`) — admin bypass, domain match, allow-list.
- Role assignment: `determineUserRole(email, adminEmails)` via
  `Server.getUserRole` (`handlers_auth.go`).
- Identity model already reserves a proxy slot: `AuthTypeProxy = "proxy"`
  (`pkg/hub/identity.go:202`).

The find-or-create user block is currently **duplicated** in the OAuth handlers
(`handlers_auth.go:257-292` and `401-436`). This design extracts it so the proxy
path and both OAuth call sites share one implementation.

## Google IAP signed-header primer

- **Header:** `X-Goog-IAP-JWT-Assertion`, value is a compact JWT.
- **Algorithm:** `ES256` (ECDSA P-256).
- **Public keys:** JWKS at `https://www.gstatic.com/iap/verify/public_key-jwk`,
  selected by the JWT `kid`. Must be cached and periodically refreshed (keys rotate).
- **`iss`:** `https://cloud.google.com/iap`.
- **`aud`:** deployment-specific and **must** be validated:
  - GCE/GKE backend service: `/projects/PROJECT_NUMBER/global/backendServices/BACKEND_SERVICE_ID`
  - App Engine: `/projects/PROJECT_NUMBER/apps/PROJECT_ID`
- **Claims:** `sub` = `accounts.google.com:<numeric-id>`, `email` =
  `accounts.google.com:<address>` (note the IdP prefix — must be stripped),
  optional `hd` (Workspace hosted domain), `iat`/`exp` (validate with small skew).
- The unsigned `X-Goog-Authenticated-User-{Email,Id}` headers are spoofable if a
  request ever reaches the hub without traversing IAP; we ignore them entirely.

## Proposed Design

### 1. A `ProxyAuthenticator` abstraction

Introduce a small interface so the middleware is provider-agnostic:

```go
// ProxyUserInfo is the verified identity extracted from proxy headers.
type ProxyUserInfo struct {
    Subject     string // stable provider subject (IdP prefix stripped)
    Email       string // verified email (IdP prefix stripped, lowercased)
    DisplayName string // best-effort; may be empty for IAP
    Domain      string // hd claim, if present
}

// ProxyAuthenticator verifies proxy-supplied auth on a request and returns the
// verified user. (nil, nil) = "no proxy assertion present" (fall through);
// (nil, err) = assertion present but invalid (reject).
type ProxyAuthenticator interface {
    Authenticate(r *http.Request) (*ProxyUserInfo, error)
    Name() string // for logging/metrics, e.g. "iap"
}
```

Implementations:

- **`IAPAuthenticator`** (new) — verifies `X-Goog-IAP-JWT-Assertion`:
  parse JWT → look up `kid` in cached JWKS → verify ES256 signature → check
  `iss`, `aud` (against configured audience), `exp`/`iat` (±skew) → strip
  `accounts.google.com:` prefixes → return `ProxyUserInfo`.
  JWKS is fetched lazily and cached with periodic refresh + on-miss refresh for
  rotated `kid`s.
- **`HeaderProxyAuthenticator`** (refactor of today's `extractProxyUser`) — keeps
  the `X-Forwarded-User-*` + IP-trust behavior for self-managed proxies, now
  routed through the same provisioning path. Not the initial focus but preserved.

Selecting an external JWT library: prefer an already-vendored one. Decision point
— see Open Question 3.

### 2. User provisioning service (shared)

Extract the duplicated find-or-create block into one method on `Server`:

```go
// provisionUser resolves a verified external identity to a stored user,
// applying access controls and creating the user on first sight.
// Returns (nil, errAccessDenied) when the user is not authorized.
func (s *Server) provisionUser(ctx context.Context, info ExternalUserInfo) (*store.User, error)
```

Behavior (identical to today's OAuth path, just centralized):

1. `checkUserAuthorized(...)` → deny if not permitted.
2. `GetUserByEmail`; if missing, `CreateUser` with `Role = getUserRole(email)`,
   `Status = "active"`.
3. If found: refresh `LastLogin`, backfill display/avatar, promote to admin if
   newly listed, reject if `Status == "suspended"`.
4. `ensureHubMembership(ctx, store, user.ID)`.

Both OAuth call sites (`handlers_auth.go:257`, `:401`) are refactored to call this,
removing the duplication. The proxy middleware calls the same method.

### 3. Middleware integration & precedence

Add a proxy step to `UnifiedAuthMiddleware` (`pkg/hub/auth.go`). Precedence,
highest first:

1. Agent token (`X-Scion-Agent-Token` / agent JWT) — unchanged.
2. Broker HMAC (`X-Scion-Broker-ID`) — unchanged.
3. Bearer token (dev / UAT / user JWT) — unchanged.
4. **Proxy authenticator** (new) — runs when configured and no higher-priority
   credential matched. Replaces the current IP-only `extractProxyUser` branch.

Keeping bearer/agent ahead of proxy means internal/non-IAP ingress (agents, CLI,
service tokens) still works even when the proxy front-end is enabled — important
for the agent-ingress question below.

On a verified proxy assertion the middleware calls `provisionUser`, then sets the
identity (canonical stored user — real UUID/role, not header-derived) and
`AuthTypeProxy`. To avoid a DB round-trip on every request, wrap the resolution
in a short-TTL cache keyed by verified email (e.g. 30–60s); the signature check
still runs every request, only the store lookup is cached. Cache TTL is a tuning
knob — Open Question 4.

### 4. Configuration

New `Auth.Proxy` section in `pkg/config/hub_config.go`, surfaced through
`ServerConfig`/`AuthConfig` and wired in `cmd/server_foreground.go` (alongside the
existing `DevAuthToken`/`UserAccessMode` wiring at `:868`, `:1132`):

```yaml
auth:
  mode: proxy              # oauth | proxy | dev  — exclusive human auth mode
  proxy:
    # consulted only when mode == proxy
    provider: iap            # iap | header
    iap:
      audience: "/projects/123456789/global/backendServices/987654321"
      # issuer + JWKS URL default to Google's; overridable for testing
    requireTrustedProxyIP: false   # optional defense-in-depth IP allowlist
  # transport (outer/platform) auth the hub instructs agents to carry.
  # Drives which entries the refresh endpoint returns (see "Generalized token refresh").
  transport:
    mode: iap                # none | cloudrun_invoker | iap
    oidcAudience: ""         # IAP client ID (iap) or hub URL (invoker); empty = derive
    platformAuthSA: "scion-transport-auth@PROJECT.iam.gserviceaccount.com"
  # reuses existing knobs for provisioning:
  userAccessMode: domain_restricted
  authorizedDomains: ["example.com"]
  adminEmails: ["admin@example.com"]
```

`auth.mode` is an **exclusive** selector — `proxy` and `oauth` are never both
active (Decision 4). In `proxy` mode the OAuth handlers and `/auth/providers` are
disabled. `user_access_mode`, `authorized_domains`, `admin_emails` are **reused
as-is** for proxy provisioning — no new access-control concepts.
`auth.transport.mode` (distinct from `auth.mode`) is the server-side source of
truth for which transport tokens the refresh endpoint hands back to agents.

### 5. Logout / session semantics

In proxy mode the hub does not own the session, so hub `/logout` cannot end it.
`/api/v1/auth/logout` should become a no-op (or redirect to IAP's
`/_gcp_iap/clear_login_cookie`). Because mode is exclusive, the login UI renders a
proxy-mode view with **no** OAuth provider buttons (extend the existing
`devAuthEnabled` gate at `web.go:1549` with the active `auth.mode`); `/auth/providers`
returns empty/unavailable in proxy mode.

## Security Considerations

- **Verify, don't trust headers.** Only the signed assertion is authoritative;
  the unsigned `X-Goog-Authenticated-User-*` headers are ignored.
- **Audience binding** is mandatory — without it, a JWT minted for a different
  IAP-protected service would be accepted.
- **Bypass risk.** The hub must be reachable *only* through IAP for the human
  surface; any path that reaches the hub directly could spoof headers — except
  the verified-JWT path is safe regardless, since forged assertions fail the
  signature check. The optional `requireTrustedProxyIP` adds belt-and-suspenders.
- **Key rotation / availability.** Cache JWKS; refresh on unknown `kid`; tolerate
  transient JWKS-endpoint failures by serving the last good key set.
- **Clock skew.** Allow a small leeway on `exp`/`iat`.
- **Suspended users.** `provisionUser` must reject `status == "suspended"` even
  though IAP would still authenticate them upstream.

## Dual-layer auth: agent/service ingress (resolved — generalize PR #307)

Agents do **not** need a separate non-proxied ingress. They traverse the same
front door using a two-layer credential, generalizing the Cloud Run pattern from
[PR #307](https://github.com/GoogleCloudPlatform/scion/pull/307):

- **Outer (platform) layer** — `Authorization: Bearer <Google OIDC identity token>`,
  fetched from the GCE metadata server by `pkg/sciontool/hub/oidc.go`. This
  satisfies the platform guard (Cloud Run invoker IAM, or IAP programmatic access).
- **App layer** — `X-Scion-Agent-Token: <scion JWT>`, the existing hub agent auth.
  Because it's a custom header, it never collides with the outer `Authorization`.

The two scenarios differ **only in the OIDC audience**:

- **Cloud Run invoker:** `aud` = the hub URL (current default in `oidc.go`).
- **IAP:** `aud` = the **IAP OAuth client ID**. IAP validates the token, then
  injects `X-Goog-IAP-JWT-Assertion` asserting the *service account's* identity.

`oidc.go` already supports this via `SCION_HUB_OIDC_AUDIENCE`. Generalization work:
formalize audience selection so an IAP deployment sets the IAP client ID (config /
env), rather than defaulting to the hub URL. No three-layer case to handle — per
the deployment owner, when IAP and invoker guards are both present the IAP service
agent carries the invoker role, so the agent still sends a single outer token.

**Hub-side consequence (important precedence rule):** an agent request arriving
through IAP carries *both* `X-Goog-IAP-JWT-Assertion` (the service account) *and*
`X-Scion-Agent-Token`. The middleware checks the agent token **first** (Step 1),
so the request is identified as the agent. When any app-layer credential
(agent/broker/bearer) is present, the proxy assertion is treated as **transport
only** and is **not** used to provision a user — we never create user records for
service-account identities. The proxy authenticator runs only when no app-layer
credential matched (i.e. genuine human IAP traffic). This is already the ordering
in §3.

**One residual nuance to be aware of:** `Authorization`-based *scion* credentials
(user JWT, `scion_pat_` UAT) cannot coexist with an outer Google OIDC token behind
a Cloud Run invoker, because there is only one `Authorization` header. This only
affects a human CLI hitting an invoker-guarded hub directly; it does not affect
agents (custom header) or IAP human traffic (assertion header). Out of initial
scope, but noted — a future option is to also accept UATs via a custom header.

## Agent OIDC identity & bootstrap (resolved)

### How PR #307 makes first contact today

PR #307 has **no** chicken-egg because it uses the agent's **ambient** GCP
identity: `oidc.go` calls the local metadata server
(`instance/service-accounts/default/identity?audience=<hub URL>`). This works on
the very first request because the GKE pod already has a workload-identity SA
attached and that SA was pre-granted the Cloud Run invoker role. The cost is
exactly what we want to avoid:

- **Policy sprawl:** every agent's compute SA, across every project, must hold the
  invoker (or IAP-access) role — or we lean on a broad
  `principalSet://…/type/ServiceAccount` grant per project.
- **Coupling to agent GCP identity:** it only works in `passthrough` metadata
  mode. It is wrong in `assign` mode (grants platform-auth to the agent's
  app-purposed SA) and impossible in `block` mode (`GCPMetadataMode`,
  `pkg/api/types.go:489`).

### Goal: a hub-managed SA, decoupled from agent GCP identity

Treat the outer OIDC layer as **strictly a hub-auth concern**: one hub-managed
service account used for the platform layer by all agents in all projects,
independent of whether the agent's own GCP identity is `block`/`passthrough`/
`assign`. Avoid distributing a keyfile (more sensitive than the telemetry key,
and we don't want another keyfile to manage).

### Bootstrap vs. steady-state refresh (corrected — only first contact is cold)

An earlier draft of this section over-claimed that "you can't refresh the
front-door key through the front door." That is wrong for steady state, and the
agent's own scion JWT already proves it: sciontool refreshes the scion JWT by
calling the **hub directly** (not the broker), and it works because the refresh
happens while the *current* credential is still valid — a sliding window. The
same applies to the outer OIDC token:

> As long as the agent refreshes **before** the current OIDC expires, the request
> reaches the hub on the old (still-valid) token; the hub mints a fresh OIDC (it
> manages the SA) and returns it in the response body. The platform validated the
> inbound request; the response is just data carrying the next token.

So the side channel is required **only for the genuinely cold case** — the very
first token, before the agent has ever connected. Everything after that rides the
front door, exactly like the scion JWT.

Two distinct phases:

1. **First contact (cold — side channel required).** The agent has no OIDC yet, so
   it cannot call the hub. The hub mints the initial OIDC token at dispatch
   (impersonating the hub-managed SA) and includes it in the **dispatch payload**,
   which already flows hub → broker → agent env injection alongside
   `SCION_AUTH_TOKEN` (`cmd/hub.go:449`). That path is hub-originated and not behind
   IAP. One-time, no chicken-egg.
2. **Steady-state refresh (warm — through the hub).** The agent maintains a rolling
   OIDC via a background ticker that refreshes well before the ~1h expiry. Simplest
   surface: **piggyback on the existing scion-JWT refresh** — the refresh response
   returns both a new scion access token *and* a fresh OIDC token, sliding both
   layers in one call. The refresh is authenticated by the agent's scion identity,
   so only legitimately-connected agents get fresh platform tokens. No broker
   involvement; matches how the scion JWT already works.

Google ID tokens are fixed ~1h with no refresh-token concept, so the background
ticker (sub-1h cadence) is what keeps a long-running *idle* agent from ever
letting the OIDC lapse. A stopped/restarted agent simply re-bootstraps via dispatch
(phase 1) — the same way it re-acquires its scion token.

### Options for who holds the minting capability

- **A — Keyfile for the hub-managed SA.** Inject the SA JSON key; agent self-mints
  ID tokens. Trivial, but a long-lived auth-grade secret in every agent container.
  **Rejected** (per deployment owner).
- **B — Impersonate via the agent's own GCP identity.** Agent's ambient SA gets
  `serviceAccountTokenCreator` on the hub SA. Re-introduces per-SA IAM and
  re-couples to GCP identity — breaks in `block` mode. **Not recommended.**
- **C — Hub mints (recommended).** The **hub** impersonates the single hub-managed
  SA for both phase 1 (dispatch) and phase 2 (refresh response). The auth-grade
  minting capability lives **only in the hub**; agents hold no SA credential and
  need no GCP identity (works even in `block` mode). The **broker is just the
  dispatch conduit** it already is — it needs **no** token-minting IAM grant. Only
  the hub's runtime SA needs `serviceAccountTokenCreator` on the managed SA.

This is simpler than the earlier "broker mints / broker relays" variants, which the
corrected refresh model makes unnecessary: the broker never mints. (The broker's
*own* control channel to the hub still authenticates with the broker's ambient infra
SA — one invoker grant on that SA — but that is a small, fixed, infra-managed set,
not the per-agent sprawl we're avoiding.)

**Decoupling:** the agent needs no GCP identity for hub auth; the sensitive
credential never leaves the hub. Strictly better than the telemetry-key model.

**Generalizing `oidc.go`:** make its token source pluggable —
`metadataTokenSource` (PR #307 / passthrough) vs an `injectedTokenSource` (phase 1)
that is then refreshed via the hub (phase 2). Audience is set per scenario (IAP
client ID, or hub URL for invoker).

**Sub-decisions (resolved):**

- **Dedicated platform/transport-auth SA — confirmed.** A dedicated service account
  used *only* for the invoker/IAP transport layer, **owned and managed by the hub
  SA** (the hub SA holds `serviceAccountTokenCreator`/`getOpenIdToken` on it and
  impersonates it to mint agent ID tokens). It is never used for anything but the
  platform guard; its asserted identity is ignored as transport at the app layer
  (per §"Dual-layer auth").
- **Piggyback on the refresh endpoint — confirmed, generalized to an array.** Refactor
  the refresh response to return an **array of updated tokens** rather than a single
  token pair. The set of tokens returned is driven by a **server config setting**
  that declares the system's overall auth/transport configuration, so the client is
  config-light and learns what to maintain from the server. See §"Generalized token
  refresh" below.

### Generalized token refresh (array payload)

The agent refresh endpoint is refactored from "return a scion access/refresh token"
to "return the set of credentials this deployment requires the client to maintain."

```jsonc
// Response from the agent token refresh endpoint
{
  "tokens": [
    { "layer": "app",       "type": "scion_access",   "value": "...", "expiresIn": 900 },
    { "layer": "app",       "type": "scion_refresh",  "value": "...", "expiresIn": 604800 },
    // present only when transport auth is configured (IAP / Cloud Run invoker):
    { "layer": "transport", "type": "google_oidc",    "value": "...",
      "audience": "<iap-client-id|hub-url>", "expiresIn": 3600 }
  ]
}
```

- Which entries appear is decided **server-side** from a config setting describing
  the deployment's transport mode (e.g. `none` / `cloudrun_invoker` / `iap`), so the
  same client binary works across deployments without per-mode flags.
- The client applies each entry to the right place by `layer`/`type`: app tokens to
  the scion-token store; `transport: google_oidc` to the OIDC transport's token
  source (`oidc.go`), which sets `Authorization: Bearer` on outbound hub requests.
- A `transport` token is minted by the hub via the dedicated SA with the configured
  audience. The background ticker drives refresh on the shortest-lived entry.
- First contact (dispatch payload) uses the **same** token-array shape, so phase 1
  and phase 2 share one schema.

## Resolved Decisions

1. **Provisioning trigger — lazy, allow-list-gated.** On the first verified human
   IAP request, `provisionUser` runs `checkUserAuthorized` and **auto-creates** the
   user iff the email is already permitted (admin / authorized domain / for
   `invite_only`, already allow-listed). No separate redeem/claim step; if not
   permitted, return 403. (`invite_only` allow-listing is populated as today, via
   admin/invite-code redemption — that just isn't part of the request-time path.)

2. **JWT/JWKS — reuse `go-jose/go-jose/v4` (no new dep).** It is already vendored
   (`go.mod`) with its `/jwt` subpackage and natively supports **ES256** and JWKS
   (`jose.JSONWebKeySet`). `IAPAuthenticator` verifies the assertion with go-jose;
   only a thin JWKS fetch+cache wrapper around the gstatic endpoint is new.

3. **Resolution cache TTL — 60s.** Acceptable staleness for role/suspension under
   proxy mode (deemed near-inconsequential).

4. **Mode is exclusive — proxy XOR OAuth, never both.** A deployment selects *one*
   human auth mode. Implication: a single `auth.mode` selector (e.g.
   `oauth` | `proxy` | `dev`) gates which login surface is active; in `proxy` mode
   the OAuth handlers and `/auth/providers` are disabled and the login UI shows no
   provider buttons (the front door is the proxy). This is cleaner than the
   "coexist + hide buttons" idea and removes the headless-CLI-via-device-flow
   ambiguity — headless/agent access in proxy deployments uses the transport-token
   path (§"Agent OIDC identity"), not OAuth device flow.

## Implementation Plan (phased)

**Phase 0 — refactor (no behavior change, lands independently):**
1. Extract `provisionUser` from the two OAuth call sites
   (`handlers_auth.go:257`, `:401`) and refactor both onto it.

**Phase 1 — inbound proxy (human IAP auth):**
2. `pkg/hub/proxyauth.go`: `ProxyAuthenticator` interface + `IAPAuthenticator`
   (verify with `go-jose/v4`, ES256, gstatic JWKS fetch+cache) + unit tests using a
   test key pair with overridable issuer/JWKS URL.
3. `auth.mode` + `auth.proxy` config in `hub_config.go`/`settings_v1.go`; wire into
   `ServerConfig`/`AuthConfig` in `cmd/server_foreground.go`.
4. Replace the IP-only proxy branch in `UnifiedAuthMiddleware` with the
   authenticator → `provisionUser` (allow-list-gated) → 60s resolution cache; set
   `AuthTypeProxy`.
5. Web login-UI: exclusive-mode gating (proxy view, no OAuth buttons),
   `/auth/providers` disabled, logout no-op/redirect.

**Phase 2 — outbound transport auth (agents through the front door):**
6. Hub-side issuance: dedicated transport-auth SA (owned/impersonated by hub SA);
   `auth.transport` config; mint the initial token into the dispatch payload.
7. Refactor the agent token refresh response to the `tokens[]` array shape, driven
   by `auth.transport.mode`; same shape reused for the dispatch payload.
8. Agent-side (`pkg/sciontool/hub/oidc.go`): consume the `transport` token from the
   refresh/dispatch array (pluggable source vs the PR #307 metadata source);
   background ticker refreshes on the shortest-lived entry.

**Phase 3 — docs:**
9. Deployment guide for the IAP + Cloud Run-invoker topology.

Phase 1 (inbound IAP) and Phase 2 (outbound transport) are independent and can be
built in parallel once Phase 0 lands; Phase 2 builds on PR #307.

## Files in scope

- `pkg/hub/auth.go` — middleware integration, retire IP-only `extractProxyUser`.
- `pkg/hub/proxyauth.go` *(new)* — `ProxyAuthenticator`, `IAPAuthenticator`, JWKS.
- `pkg/hub/handlers_auth.go` — extract `provisionUser`, dedupe OAuth call sites.
- `pkg/hub/identity.go` — reuse `AuthTypeProxy`; proxy identity wrapper if needed.
- `pkg/config/hub_config.go`, `pkg/config/settings_v1.go` — `Auth.Proxy` config.
- `cmd/server_foreground.go` — wiring into `ServerConfig`/`AuthConfig`.
- `pkg/hub/web.go` — proxy-mode login-UI flag; logout semantics.
- `pkg/sciontool/hub/oidc.go` — agent-side dual-layer transport; audience
  selection for IAP (builds on PR #307).
