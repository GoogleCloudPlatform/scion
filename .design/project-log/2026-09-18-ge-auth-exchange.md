# GE Google credential exchange — Hub endpoint + Bridge auth scheme

**Date:** 2026-09-18
**Branch:** `scion/dev-ge-auth`
**Issues:** #1616 (Hub), #1617 (Bridge)
**Base:** `b2856682fbd2ed43588c759cfb8e54e90556becf`

## Summary

Implemented the Hub-owned GE Google credential exchange endpoint and the
bridge-side authentication scheme that consumes it. This allows Google
Enterprise users to authenticate to the A2A bridge using their Google
credentials (ID tokens or access tokens), which are validated and exchanged
for short-lived Hub user access tokens.

## Commits

| SHA | Description |
|-----|-------------|
| `1973a64` | feat(hub): GE Google credential exchange endpoint (#1616) |
| `7885703` | feat(bridge): GE Google credential exchange auth scheme (#1617) |
| `db6350e` | docs: project log entry for GE auth exchange |
| `bf69c9e` | feat(hub): durable external identity store + validator security fixes (#1616) |
| `59de2dc` | feat(bridge): v0.3 REST protocol compatibility via SDK a2acompat (#1617) |
| `3354ea8` | docs: update project log with durable store, validator fixes, v0.3 compat |
| `01d3557` | fix(hub,bridge): resolve all critical/required review findings (#1616, #1617) |
| `b0dc610` | fix: authzop catalog + GE JSON-RPC wire compatibility tests (#1616, #1617) |

## Hub — `POST /api/v1/auth/integrations/google/exchange` (#1616)

### Files added
- `pkg/hub/google_credential_validator.go` — Google credential validation
  interface and production implementation. ID tokens validated via Google
  JWKS cryptographic signature verification with pinned issuer, audience,
  expiry, stable subject, verified email, and SA rejection. Access tokens
  validated via `POST https://oauth2.googleapis.com/tokeninfo` with `azp`
  as authoritative issued-client field plus userinfo cross-check.
  Security fixes applied: no-redirect HTTP client, URL-encoded form body,
  flexBool for email_verified, required exp, azp policy, unconditional
  aud/azp disagreement rejection, bounded JWKS stale + force-refresh.
- `pkg/hub/ge_exchange.go` — Exchange service, external identity binding,
  user resolution/provisioning, conflict-safe concurrent binding resolution,
  and HTTP handler.
- `pkg/hub/google_credential_validator_test.go` — 18 production validator
  tests with pinned fake transport (`googleURLRewriter` RoundTripper).
- `pkg/hub/ge_exchange_test.go` — 44+ tests covering all acceptance cases
  plus JWT exp regression, provisioning auth, conflict resolution, and
  orphan cleanup.
- `pkg/ent/schema/externalidentity.go` — ExternalIdentity ent schema with
  unique composite index on (provider, issuer, subject).
- `pkg/store/entadapter/externalidentity_store.go` — Ent-backed durable
  store implementing ExternalIdentityStore interface.
- `pkg/store/entadapter/externalidentity_store_test.go` — 13 store tests.

### Files modified
- `pkg/hub/server.go` — Config struct, service init, route registration.
- `pkg/hub/route_metadata.go` — RoutePublic classification entry.
- `pkg/hub/usertoken.go` — Added `GenerateAccessTokenWithTTL` method.
- `pkg/hub/authzop/catalog.go` — Public endpoint exemption + mutation
  exemptions: 2×UpdateUser (email + profile), DeleteUser (orphan cleanup),
  CreateUser (provisioning).
- `pkg/ent/schema/user.go` — Added `external_identities` reverse edge.
- `pkg/store/store.go` — Added `ExternalIdentityStore` interface and model.
- `pkg/store/entadapter/composite.go` — Added ExternalIdentityStore.
- `pkg/ent/*.go` — Regenerated ent code (20 files).

### Key design decisions
- **Durable external identity store:** Ent-backed with unique composite
  index on (provider, issuer, subject). Auto-migrated on startup. Bindings
  persist across Hub restarts.
- **Conflict-safe concurrent binding:** On unique constraint violation
  during creation, `resolveAfterConflict()` looks up the winning binding
  and resolves to the winner's user. Tested with 5-goroutine race.
- **Authoritative email domain guard:** Only Gmail (`gmail.com`,
  `googlemail.com`) or verified Workspace (`hd` claim) emails may
  auto-bootstrap external identity bindings. Other domains fail closed.
- **Issuer canonicalization:** Both `accounts.google.com` and
  `https://accounts.google.com` normalize to the canonical HTTPS form.
  Store does exact match; canonicalization is caller responsibility.
- **Access token metadata:** `azp` is the authoritative issued-client
  field. aud/azp disagreement unconditionally rejected.
- **Token expiry capping:** Hub token TTL is `min(configured, upstream
  remaining)`. Both `expiresAt` and `upstreamExpiresAt` mandatory.
- **No login regression:** Normal Hub web Google login does not create
  integration bindings via the weaker UserInfo-only flow.
- **flexBool type:** Handles both boolean and string `"true"`/`"false"`
  forms from Google APIs' inconsistent JSON encoding.
- **JWKS cache:** Bounded stale at 24h max. Force-refresh on unknown kid
  with 30s rate limit to prevent excessive fetches.

## Bridge — `geGoogle` auth scheme + v0.3 REST compat (#1617)

### Files added
- `extras/scion-a2a-bridge/internal/bridge/ge_exchange_validator.go` —
  Bounded cache with singleflight DoChan + `context.WithoutCancel`,
  config version invalidation.
- `extras/scion-a2a-bridge/internal/bridge/ge_exchange_validator_test.go` —
  35+ tests covering all acceptance cases including singleflight caller
  cancellation isolation.
- `extras/scion-a2a-bridge/internal/bridge/v0_compat_test.go` — 20 tests:
  9 v0.3 HTTP-level + 11 GE JSON-RPC wire compatibility (message/send,
  message/stream, tasks/get/cancel/resubscribe, discovery aliases,
  multi-turn cursor, v0.3 REST body forwarding).

### Files modified
- `extras/scion-a2a-bridge/internal/bridge/config.go` — `GEExchangeConfig`
  struct with `CredentialType` and `CacheTTL` fields.
- `extras/scion-a2a-bridge/internal/bridge/adminoverlay.go` — Added
  `geGoogle` hot-reload overlay with config version tracking.
- `extras/scion-a2a-bridge/internal/bridge/server.go` — `geGoogle` scheme
  in validation, initialization, middleware, logging; v0.3 REST catch-all
  routes; `handleV0REST` prefix-stripping handler; `SetSDKHandler` for
  test-only SDK handler override.
- `extras/scion-a2a-bridge/internal/bridge/bridge.go` — Dual-format agent
  card with v1.0 JSONRPC + v0.3 REST interfaces and legacy flat fields.
- `extras/scion-a2a-bridge/cmd/scion-a2a-bridge/main.go` — v0.3 REST
  handler creation via `a2av0.NewRESTHandler` in both code paths.
- `extras/scion-a2a-bridge/go.mod` — Added `a2acompat/a2av0` transitive
  dependency (`github.com/a2aproject/a2a-go v0.3.15` indirect).
- `extras/scion-a2a-bridge/go.sum` — Updated checksums.

### Key design decisions
- **v0.3 REST via SDK:** Uses official `a2acompat/a2av0.NewRESTHandler`
  from the SDK. Catch-all wildcard routes strip per-agent prefix; Go 1.22
  mux ensures specific agent-card/jsonrpc routes take precedence.
- **Dual-format agent card:** `supportedInterfaces` includes both v1.0
  JSONRPC and v0.3 REST. Legacy flat fields (`url`, `protocolVersion`,
  `preferredTransport`) included for v0.3 client compatibility.
- **Cache key:** `SHA-256(credential + configVersion)` — raw tokens never
  stored as map keys or logged.
- **Cache TTL:** `min(configured, Hub expiry, upstream expiry)`.
- **Config version invalidation:** `InvalidateCache()` increments version
  and clears cache.
- **Bounded size:** 10,000 entries with oldest-expiry eviction.
- **Singleflight:** `golang.org/x/sync/singleflight` coalesces concurrent
  cache misses for the same credential.
- **No shared-client fallback:** Hub rejection propagated as-is.

## Revocation/cache window and stream-establishment

The bridge cache creates a bounded revocation window:

- **Maximum window:** `min(configured TTL, Hub token expiry, upstream
  Google credential expiry)`. Default configured TTL is 60s; Hub default
  token TTL is 60s; max configurable TTL is 5min.
- **Revocation propagation:** When a Google credential is revoked upstream,
  the bridge continues to accept the cached identity until the cache entry
  expires. Deliberate trade-off for performance.
- **Stream establishment:** SSE streaming connections authenticated at
  establishment time. A stream established during the cache window will
  remain open even if the credential is subsequently revoked. Bounded by
  SSE keepalive timeout and bridge task inactivity timeout.
- **Cold replicas:** New bridge replicas have empty caches; first request
  validates via Hub. No cross-replica cache sharing.
- **Config invalidation:** Trust configuration changes immediately clear
  all cached entries via `InvalidateCache()`.

## Module changes

Bridge `go.mod`:
- `github.com/a2aproject/a2a-go v0.3.15` — **new indirect** (required by
  SDK `a2acompat/a2av0` package)
- Minor indirect version bumps from `go mod tidy` (grpc-gateway, otel)

Hub `go.mod`: No changes.

## Review finding disposition (review-auth-1 at 8047f73, fixed in 01d3557 + b0dc610)

### Critical findings

| # | Finding | Resolution | Evidence |
|---|---------|------------|----------|
| C1 | JWT exp not capped by min(configured, upstream remaining) | **Resolved**: `GenerateAccessTokenWithTTL` caps at `min(DefaultGETokenTTL, upstream remaining)`; `DefaultGETokenTTL = 60s`, `MaxGETokenTTL = 5min` | `TestGEExchange_JWTExpCryptographicRegression`, `TestGEExchange_JWTExpRegression_ConfiguredTTLWins`, `TestGEExchange_TokenExpiryCappedByUpstream` |
| C2 | Auto-provisioning bypasses Hub registration policy | **Resolved**: `provisionNewUser` calls `s.authChecker(ctx, email)` before `CreateUser`; `server.go` wires `srv.isUserAuthorized` | `TestGEExchange_ProvisioningAuth_DomainRestricted`, `_DomainAllowed`, `_InviteOnly_Rejected`, `_AdminBypass`, `_NilAuthChecker_FailsClosed` |

### Required findings

| # | Finding | Resolution | Evidence |
|---|---------|------------|----------|
| R3 | `resolveAfterConflict` silently adopts mismatched user; no orphan cleanup | **Resolved**: `expectedUserID` validation + `DeleteUser` orphan cleanup; authzop catalog updated with 2×UpdateUser + DeleteUser | `TestGEExchange_ConflictResolution_ExpectedUserMismatch`, `TestGEExchange_OrphanCleanup_OnProvisioningConflict` |
| R4 | No production validator tests | **Resolved**: 18 tests with `googleURLRewriter` RoundTripper intercepting production Google URLs → httptest servers | `TestProductionValidator_IDToken_*` (7), `TestProductionValidator_AccessToken_*` (6), `TestProductionValidator_IDToken_JWKSForceRefresh`, etc. |
| R5 | Missing exp regression test | **Resolved**: Cryptographic JWT validation — parses minted JWT with go-jose, asserts `jwt_exp == response.expires_at ≤ upstream_expiry` | `TestGEExchange_JWTExpCryptographicRegression`, `TestGEExchange_JWTExpRegression_ConfiguredTTLWins` |
| R6 | Tokeninfo schema — `issued_to`/`audience` vs `azp`/`aud` | **Resolved**: `googleTokenInfoResponse` uses `json:"azp"` and `json:"aud"` matching `https://oauth2.googleapis.com/tokeninfo`; production validator tests pin exact schema | `TestProductionValidator_AccessToken_TokenInfoEndpoint` |
| R7 | Singleflight context leak — `Do` → `DoChan` | **Resolved**: `DoChan` + `context.WithoutCancel`; each waiter selects on its own ctx | `TestGEExchangeValidator_Singleflight_CallerCancellation` |
| R8 | No GE JSON-RPC wire compatibility tests | **Resolved**: 11 tests covering message/send, message/stream, tasks/get/cancel/resubscribe, discovery aliases, multi-turn cursor, v0.3 REST | `TestJSONRPC_WireFormat_*` (5), `TestJSONRPC_DiscoveryAlias_*` (2), `TestJSONRPC_MultiTurnCursor_*` (1), `TestV0REST_WireFormat_*` (3) |

### Additional

| Finding | Resolution | Evidence |
|---------|------------|----------|
| geGoogle hot-reload overlay | **Resolved**: `adminoverlay.go` geGoogle overlay with config version tracking | `01d3557` |
| Cache TTL reconciliation (~60s contract) | **Resolved**: `DefaultGETokenTTL = 60s`, `MaxGETokenTTL = 5min`, bridge `defaultGECacheTTL = 60s` | `01d3557`, `b0dc610` |

## Test evidence

- Hub: 44+ GE exchange tests + 18 production validator tests + 13 ent store tests, all passing
- Bridge: 35+ GE validator tests (incl. singleflight isolation) + 20 v0.3/wire compat tests, all passing
- authzop `TestMutationClassificationBidirectional` passes with updated catalog entries
- All broader bridge tests pass (`go test ./internal/bridge/`)
- Both modules build cleanly (`go build ./...`)
