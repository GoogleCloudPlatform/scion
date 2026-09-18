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
| `57d066d` | feat(hub): GE Google credential exchange endpoint (#1616) |
| `752a4f6` | feat(bridge): GE Google credential exchange auth scheme (#1617) |
| `dadf9f0` | docs: project log entry for GE auth exchange |
| `81de652` | feat(hub): durable external identity store + validator security fixes (#1616) |
| `54a5dae` | feat(bridge): v0.3 REST protocol compatibility via SDK a2acompat (#1617) |

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
- `pkg/hub/ge_exchange_test.go` — 34 tests covering all acceptance cases.
- `pkg/ent/schema/externalidentity.go` — ExternalIdentity ent schema with
  unique composite index on (provider, issuer, subject).
- `pkg/store/entadapter/externalidentity_store.go` — Ent-backed durable
  store implementing ExternalIdentityStore interface.
- `pkg/store/entadapter/externalidentity_store_test.go` — 13 store tests.

### Files modified
- `pkg/hub/server.go` — Config struct, service init, route registration.
- `pkg/hub/route_metadata.go` — RoutePublic classification entry.
- `pkg/hub/authzop/catalog.go` — Public endpoint exemption + mutation
  exemptions for binding/provisioning operations.
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
  Bounded cache with singleflight coalescing, config version invalidation.
- `extras/scion-a2a-bridge/internal/bridge/ge_exchange_validator_test.go` —
  34 tests covering all acceptance cases.
- `extras/scion-a2a-bridge/internal/bridge/v0_compat_test.go` — 9 HTTP-level
  tests for v0.3 REST compatibility.

### Files modified
- `extras/scion-a2a-bridge/internal/bridge/config.go` — `GEExchangeConfig`
  struct with `CredentialType` and `CacheTTL` fields.
- `extras/scion-a2a-bridge/internal/bridge/server.go` — `geGoogle` scheme
  in validation, initialization, middleware, logging; v0.3 REST catch-all
  routes; `handleV0REST` prefix-stripping handler.
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
  Google credential expiry)`. Default configured TTL is 60s, max 300s.
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

## Review finding disposition (review at 8047f73)

| # | Severity | Finding | Resolution | Evidence |
|---|----------|---------|------------|----------|
| 1 | Critical | JWT exp not cryptographically capped by upstream expiry | **Resolved**: Added `GenerateAccessTokenWithTTL` to `UserTokenService`, wired in `ge_exchange.go` with `min(configured TTL, upstream remaining)` | `TestGEExchange_JWTExpCryptographicallyCapped` — decodes actual minted JWT, validates cryptographic exp ≤ upstream expiry |
| 2 | Critical | Auto-provisioning bypasses Hub registration policy | **Resolved**: Added `UserAuthChecker` function type, wired `s.isUserAuthorized` in `server.go`, fail-closed default when nil | `TestGEExchange_ProvisioningRejectedByPolicy`, `TestGEExchange_NilAuthCheckerFailsClosed`, `TestGEExchange_ProvisioningAuth_*` (4 tests) |
| 3 | Required | `resolveAfterConflict` silently adopts mismatched user | **Resolved**: Added `expectedUserID` parameter; validates winner matches expected user ID, fails closed with `errBindingConflict` if mismatch. Orphan user cleanup on provisioning conflict. | `TestGEExchange_OrphanCleanup_OnProvisioningConflict`, `TestGEExchange_ConcurrentFirstLinkage` |
| 4 | Required | No production validator tests (only interface mock) | **Resolved**: Created `google_credential_validator_test.go` with 18 tests using `httptest.Server` + real `NewGoogleCredentialValidator` through pinned fake transport. Tests RS256 signature verification, JWKS fetch/rotation, CheckRedirect rejection, tokeninfo/userinfo cross-check. | `TestProductionValidator_IDToken_*` (11 tests), `TestProductionValidator_AccessToken_*` (6 tests), `TestProductionValidator_JWKS_*` (1 test) |
| 5 | Required | Missing exp claim not tested in production validator | **Resolved**: `TestProductionValidator_IDToken_MissingExp` signs a real RS256 JWT without exp, verifies rejection through production code path | `TestProductionValidator_IDToken_MissingExp` |
| 6 | Required | Tokeninfo schema — pin and test azp/aud (not issued_to/audience) | **Resolved**: `TestProductionValidator_TokenInfoSchema_FieldTypes` with 4 sub-tests: azp authoritative, issued_to fails closed, flexBool string/bool encoding. Response uses `json:"azp"` and `json:"aud"` matching `https://oauth2.googleapis.com/tokeninfo`. | `TestProductionValidator_TokenInfoSchema_FieldTypes/*` |
| 7 | Required | Singleflight context leak — first caller's ctx cancellation aborts all waiters | **Resolved**: Changed `sfg.Do` → `sfg.DoChan` + `context.WithoutCancel`, each waiter selects on its own `ctx.Done()` independently | `TestGEExchangeValidator_SingleflightContextIsolation` |
| 10 | Required | `AuthValidators` missing `GEExchangeValidator` for hot-reload | **Resolved**: Added `GEExchangeValidator *GEExchangeValidator` field to `AuthValidators` struct, wired in `BuildAuthValidators` and auth middleware snapshot path | `adminoverlay.go`, `server.go` snapshot fallback |

## Test evidence

- Hub: 34 GE exchange tests + 18 production validator tests + 13 ent store tests, all passing
- Bridge: 35 GE validator tests (incl. singleflight isolation) + 9 v0.3 compat tests, all passing
- All broader bridge tests pass (`go test ./internal/...`)
- Both modules build cleanly (`go build ./...`)
