# GE Google credential exchange — Hub endpoint + Bridge auth scheme

**Date:** 2026-09-18
**Branch:** `scion/dev-ge-auth`
**Issues:** #1616 (Hub), #1617 (Bridge)
**Base:** `346b1f74ceaf541b2a8838c7b14b6a68ec73baa3` (rebased onto origin/main — conflict-free, zero file overlap)

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
| `d1340ac` | docs: update project log with review-auth-1 finding dispositions (#1616, #1617) |
| `b86fe45` | fix(hub,bridge): resolve review-auth-2 findings (#1616, #1617) |
| `0695f75` | fix(bridge): wire transport auth to snapshot validator + harden dispatch tests (#1616, #1617) |
| `58df399` | docs: update project log with round 3 rebase onto origin/main 346b1f7 (#1616, #1617) |
| `e099d85` | fix(hub): concurrent first-linkage provisioning collision (H3-R1) (#1616) |

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
- **Bounded size:** 10,000 entries with LRU eviction (O(1) via
  `container/list` doubly-linked list + map).
- **Singleflight:** `golang.org/x/sync/singleflight` coalesces concurrent
  cache misses for the same credential.
- **No shared-client fallback:** Hub rejection propagated as-is.
- **Transport auth:** Functional options `WithGETransportAuth` composes
  Cloud Run/IAP auth headers on outgoing Hub requests.
- **Fail closed on expiry:** Expired Hub tokens (remaining ≤ 0) are
  rejected, never cached.

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

## Review finding disposition (review-auth-bridge-2 — REQUEST CHANGES)

| # | Finding | Resolution | Evidence |
|---|---------|------------|----------|
| B2-C1 | Missing `ge_exchange` in `callerHubClient` | **Resolved**: added `case "ge_exchange":` — creates per-caller Hub client with bearer token + transport auth | `TestCallerHubClient_GEExchangeTokenType`, `TestGEExchange_ExecutorPath_Regression` (hard-asserts Hub received message + correct bearer token) |
| B2-R2 | Synthetic JSON-RPC tests | **Resolved**: complete rewrite with `newIntegrationTestServer` → real SDK handler + executor + mock Hub; SDK v2 wire format (messageId, ROLE_USER, text parts); hard dispatch assertions (t.Fatal) | `TestJSONRPC_RealHandler_*` (6 tests), `TestGEExchange_ExecutorPath_Regression` |
| B2-R3 | Missing discovery aliases + direct POST | **Resolved**: `/.well-known/agent.json` routes, direct POST routes, auth exemption | `TestDiscovery_*` (6 tests) |
| B2-R4 | Transport auth not wired to GE validator / snapshot validator | **Resolved**: `BuildSnapshot` accepts `...GEValidatorOption`; `broker.go` stores+forwards geOpts; `main.go` all 4 call sites forward geOpts | `TestGEExchangeValidator_TransportAuth_*` (2 tests) + `TestSnapshotMiddleware_TransportAuth_InitialComposition`, `TestSnapshotMiddleware_TransportAuth_AfterSnapshotReplacement` |
| B2-R5 | O(N) eviction → LRU | **Resolved**: `container/list` + map for O(1) eviction | `TestGEExchangeValidator_EvictLRU`, `TestGEExchangeValidator_LRUEviction_ConcurrentAccess` |
| B2-R6 | Expired response caching | **Resolved**: fail closed when remaining ≤ 0 | `TestGEExchangeValidator_Expired*_FailsClosed`, `TestGEExchangeValidator_ZeroExpiry_FailsClosed` |
| B2-N1 | `SetSDKHandler` on Server | **Resolved**: moved to `export_test.go` | — |
| B2-N2 | REST v0.3 advertised unconditionally | **Resolved**: `v0RESTEnabled` flag, conditional advertising | `TestV0REST_AgentCardNoRESTWhenHandlerNil` |

## Review finding disposition (review-auth-hub-3 at `58df399` — REQUEST CHANGES)

| # | Finding | Resolution | Evidence |
|---|---------|------------|----------|
| H3-R1 | Concurrent first-linkage provisioning collision — two simultaneous exchanges for unseen identity/email cause "bound user not found" | **Resolved**: `provisionNewUser` returns `(user, provisioned bool, err)`; on `store.ErrAlreadyExists` re-queries by email to find winner; orphan cleanup only when `provisioned && winner.ID != user.ID`; `fakeUserStore` mutex + unique email constraint | `TestGEExchange_ConcurrentFirstLinkage` (5 goroutines, hard assertions), `TestGEExchange_ConcurrentFirstLinkage_PersistentStore` (SQLite, two service instances), `TestGEExchange_ProvisionNewUser_CreateError_FailsClosed` (HTTP 500 when no winner) |

## Review finding disposition (review-auth-hub-2 — APPROVE, 3 observations)

| # | Observation | Resolution | Evidence |
|---|-------------|------------|----------|
| H2-O1 | `flexInt64` for `expires_in` | **Resolved**: custom JSON type handling number/string forms | `TestFlexInt64_*` (4 tests), `TestProductionValidator_AccessToken_ExpiresInAsString` |
| H2-O2 | Dead remaining-lifetime branch | **Resolved**: strict `remaining <= 0` check | `TestProductionValidator_IDToken_ExpiredWithinSkew`, `_PositiveRemaining`, `_LongRemaining` |
| H2-O3 | User deletion cascade | **Resolved**: `entsql.OnDelete(entsql.Cascade)` annotation | `TestExternalIdentityStore_UserDeleteCascade` |

## Test evidence (round 4 H3-R1 fix: `e099d85`)

- **H3-R1 concurrent first-linkage provisioning collision:** Fixed multi-layered race — (1) `fakeUserStore` lacked mutex/unique email constraint, (2) `provisionNewUser` didn't handle email-collision race, (3) orphan cleanup deleted winning user when provisioner lost binding race
- **Production code:** `provisionNewUser` returns `(user, provisioned bool, err)` — on `store.ErrAlreadyExists`, re-queries by email to find collision winner; orphan cleanup gated by `provisioned && winner.ID != user.ID`
- **Test fakes:** `fakeUserStore` gains `sync.Mutex` on all map operations + unique email constraint enforcement via `usersByEmail` index
- **New tests:** `TestGEExchange_ConcurrentFirstLinkage` (5 goroutines, hard assertions on convergence), `TestGEExchange_ConcurrentFirstLinkage_PersistentStore` (SQLite via `entc.OpenSQLite` + `entadapter.NewCompositeStore`, two independent service instances), `TestGEExchange_ProvisionNewUser_CreateError_FailsClosed` (HTTP 500 when re-query finds no winner)
- **50× race-clean:** `go test -count=50 -race -run 'TestGEExchange_ConcurrentFirstLinkage$' ./pkg/hub/` — all 50 pass, 0 data races
- All existing Hub GE tests continue to pass (74 tests total)
- Bridge suite unmodified, continues to pass

## Test evidence (round 3 rebase: `deee990`)

- Freshness rebase onto `origin/main` `346b1f7` — conflict-free, zero file overlap, no semantic conflict resolution
- Hub: 44+ GE exchange tests + 22 production validator tests (incl. flexInt64) + 14 ent store tests (incl. cascade), all passing post-rebase
- Bridge: full suite passes (34.8s), race detector clean (35.9s)
- Snapshot middleware: 2 tests prove transport auth flows through `BuildSnapshot → BuildAuthValidators → GEExchangeValidator` via `Server.Handler()` with non-nil snapshot (initial + hot-reload)
- Dispatch hard assertions: `TestGEExchange_ExecutorPath_Regression` and `TestJSONRPC_RealHandler_MessageSend` use `t.Fatal` to verify Hub received dispatched messages (not timeout-as-success)
- SDK v2 wire format: all payloads use `messageId`, `ROLE_USER`, `{"text": "..."}` part format
- Build/vet/diff-check all clean
- All broader bridge tests pass (`go test ./internal/bridge/`)
- Both modules build cleanly (`go build ./...`, `go vet ./...`)
