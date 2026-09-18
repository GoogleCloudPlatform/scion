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

## Hub — `POST /api/v1/auth/integrations/google/exchange` (#1616)

### Files added
- `pkg/hub/google_credential_validator.go` — Google credential validation
  interface and production implementation. ID tokens validated via Google
  JWKS cryptographic signature verification with pinned issuer, audience,
  expiry, stable subject, verified email, and SA rejection. Access tokens
  validated via `POST https://oauth2.googleapis.com/tokeninfo` with `azp`
  as authoritative issued-client field plus userinfo cross-check.
- `pkg/hub/ge_exchange.go` — Exchange service, external identity binding
  store, user resolution/provisioning, and HTTP handler.
- `pkg/hub/ge_exchange_test.go` — 27 tests covering all acceptance cases.

### Files modified
- `pkg/hub/server.go` — Config struct, service init, route registration.
- `pkg/hub/route_metadata.go` — RoutePublic classification entry.
- `pkg/hub/authzop/catalog.go` — Public endpoint exemption + mutation
  exemptions for binding/provisioning operations.

### Key design decisions
- **Authoritative email domain guard:** Only Gmail (`gmail.com`,
  `googlemail.com`) or verified Workspace (`hd` claim) emails may
  auto-bootstrap external identity bindings. Other domains fail closed.
- **Issuer canonicalization:** Both `accounts.google.com` and
  `https://accounts.google.com` normalize to the canonical HTTPS form.
- **Access token metadata:** `azp` is the authoritative issued-client
  field. Disagreement among any populated client-identifying fields
  (`aud`, `azp`, `issued_to`) causes rejection.
- **Token expiry capping:** Hub token TTL is `min(configured, upstream
  remaining)`. Both `expiresAt` and `upstreamExpiresAt` are mandatory
  in the response.
- **In-memory external identity store:** Used `MemoryExternalIdentityStore`
  to avoid ent schema codegen diff. Migration path to ent documented.
- **No login regression:** Normal Hub web Google login does not create
  integration bindings via the weaker UserInfo-only flow.

## Bridge — `geGoogle` auth scheme (#1617)

### Files added
- `extras/scion-a2a-bridge/internal/bridge/ge_exchange_validator.go` —
  Bounded cache with singleflight coalescing, config version invalidation.
- `extras/scion-a2a-bridge/internal/bridge/ge_exchange_validator_test.go` —
  30 tests covering all acceptance cases.

### Files modified
- `extras/scion-a2a-bridge/internal/bridge/config.go` — `GEExchangeConfig`
  struct with `CredentialType` and `CacheTTL` fields.
- `extras/scion-a2a-bridge/internal/bridge/server.go` — `geGoogle` scheme
  in validation, initialization, middleware, and logging.
- `extras/scion-a2a-bridge/go.mod` / `go.sum` — transitive indirect bumps
  from `go mod tidy` (no new direct dependencies).

### Key design decisions
- **Cache key:** `SHA-256(credential + configVersion)` — raw tokens never
  stored as map keys or logged.
- **Cache TTL:** `min(configured, Hub expiry, upstream expiry)`.
- **Config version invalidation:** `InvalidateCache()` increments version
  and clears cache. Trust/config changes (allowed client IDs, credential
  mode, Hub endpoint) are handled atomically.
- **Bounded size:** 10,000 entries with oldest-expiry eviction.
- **Singleflight:** `golang.org/x/sync/singleflight` coalesces concurrent
  cache misses for the same credential.
- **No shared-client fallback:** Hub rejection is propagated as-is.
- **Hub token passthrough:** Hub-issued access token stored in
  `CallerIdentity.RawToken` for direct use in downstream Hub API calls.

## Revocation/cache window and stream-establishment

The bridge cache creates a bounded revocation window:

- **Maximum window:** `min(configured TTL, Hub token expiry, upstream
  Google credential expiry)`. Default configured TTL is 60s, max 300s.
- **Revocation propagation:** When a Google credential is revoked upstream,
  the bridge continues to accept the cached identity until the cache entry
  expires. This is a deliberate trade-off for performance: full revalidation
  on every request would require a Hub round-trip per A2A call.
- **Stream establishment:** SSE streaming connections are authenticated at
  establishment time. A stream established during the cache window will
  remain open even if the credential is subsequently revoked. The stream
  lifetime is bounded by SSE keepalive timeout and bridge restart.
- **Cold replicas:** New bridge replicas have empty caches and must exchange
  every credential on first access. In a multi-replica deployment, there is
  no cross-replica cache sharing — each replica independently exchanges
  with the Hub.
- **Config invalidation:** When the bridge operator changes trust
  configuration (allowed client IDs, credential type, Hub endpoint), calling
  `InvalidateCache()` immediately clears all cached entries and forces
  re-exchange on next access.

## OAuth client setup

- **Dedicated client:** Create a dedicated Google OAuth client ID for GE
  Bridge authentication. Add it to the Hub's `AllowedClientIDs` list. This
  provides the strongest security boundary — tokens issued to other clients
  are rejected.
- **Shared client:** If a shared OAuth client must be used, add all client
  IDs to the `AllowedClientIDs` list. The Hub validates `azp` against this
  list for both ID tokens and access tokens.
- **No default client:** The Hub does not provide a default/builtin client
  ID. The `AllowedClientIDs` list must be non-empty when the exchange is
  enabled.

## SDK a2av0 compatibility evaluation

The `a2a-go/v2` SDK provides `a2acompat/a2av0` (package
`github.com/a2aproject/a2a-go/v2/a2acompat/a2av0`) with v0.3-compatible
`NewRESTHandler` and `NewJSONRPCHandler` functions. This is the official
path for Go v1/v0.3 client compatibility. The bridge currently does not
use a bespoke `v0compat.go`; if v0.3 support is needed, the SDK package
should be preferred.

## Test evidence

- Hub: 27 tests, all passing (commit 57d066d)
- Bridge: 30 tests, all passing (commit 752a4f6)
- `make ci` not runnable from bridge subtree (requires root workspace
  toolchain); `go build -buildvcs=false ./...` and `go test -count=1
  -run TestGEExchange ./pkg/hub/` both pass at final tip.
