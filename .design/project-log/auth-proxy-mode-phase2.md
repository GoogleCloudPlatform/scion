# Auth Proxy Mode — Phase 2 Implementation

**Date:** 2026-06-05
**Branch:** scion/auth-proxy-mode
**Pushed SHA:** e9776f09

## Summary

Implemented Phase 2 (outbound transport auth) of the auth-proxy-mode feature.
This enables agents to traverse IAP / Cloud Run invoker front doors using
hub-minted Google OIDC ID tokens alongside their existing scion agent tokens.

## Commits (5 logical chunks)

1. **a34bf6e** — config: add auth.transport config types
2. **a488b96** — hub: add TransportTokenMinter interface and implementations
3. **b617c84** — hub: wire transport token minter into ServerConfig and dispatch
4. **e6a11f6** — hub: extend token refresh response with generalized tokens[] array
5. **e9776f0** — sciontool: add pluggable OIDC transport for agent outbound auth

## Key Design Decisions

### TransportTokenMinter interface
- `MintIDToken(ctx, audience) (token, expiry, error)` — clean interface
- `gcpTransportMinter`: uses IAM Credentials API (`generateIdToken`) via
  already-vendored `google.golang.org/api/iamcredentials/v1`
- `noopTransportMinter`: returns error when transport mode == "none"
- `FakeTransportMinter`: exported test double
- When mode == "none" or unset: minter is nil everywhere → zero impact

### tokens[] backward compatibility
- Response keeps existing `token` + `expires_at` fields alongside `tokens[]`
- Old clients ignore `tokens[]`; new clients use both
- No breaking change for existing RefreshToken parsers

### Dispatch vs refresh schema
- Dispatch uses individual env vars: `SCION_TRANSPORT_TOKEN`,
  `SCION_TRANSPORT_AUDIENCE`, `SCION_TRANSPORT_TOKEN_EXPIRY`
- Refresh uses the `tokens[]` JSON array in the response
- Pragmatic deviation from "same schema" in the design doc — env vars match
  existing dispatch conventions (SCION_AUTH_TOKEN, GITHUB_TOKEN, etc.)

### Agent-side pluggable token source
- `injectedTokenSource`: hub-provided token from dispatch env var, refreshed
  via tokens[] array on subsequent refreshes
- `metadataTokenSource`: GCE metadata server (PR #307 pattern, passthrough mode)
- Selection: SCION_TRANSPORT_TOKEN env → injected; on GCE → metadata; else → disabled
- Background ticker: uses shortest-lived token to drive refresh (5-min margin
  for transport tokens vs 2h for scion tokens)

## Files Changed

| File | Action |
|------|--------|
| `pkg/config/hub_config.go` | Edit — add TransportAuthConfig |
| `pkg/config/settings_v1.go` | Edit — add V1TransportConfig, conversion, env mapping |
| `pkg/hub/transport_token.go` | **New** — minter interface, implementations, RefreshTokenEntry |
| `pkg/hub/transport_token_test.go` | **New** — 11 tests |
| `pkg/hub/server.go` | Edit — add transport fields to ServerConfig + Server |
| `pkg/hub/httpdispatcher.go` | Edit — add minter field, setter, inject in 3 dispatch paths |
| `pkg/hub/handlers.go` | Edit — extend handleAgentTokenRefresh with tokens[] |
| `cmd/server_foreground.go` | Edit — construct minter from config |
| `pkg/sciontool/hub/oidc.go` | **New** — pluggable OIDC sources + transport |
| `pkg/sciontool/hub/oidc_test.go` | **New** — 23 tests |
| `pkg/sciontool/hub/client.go` | Edit — oidcSource, RefreshTokenEntry, applyRefreshTokens |

## Test Results

- `go build ./...` — clean
- `go vet ./pkg/hub/... ./pkg/config/... ./pkg/sciontool/...` — clean
- `go test ./pkg/sciontool/hub/...` — PASS (all tests including 23 new)
- `go test ./pkg/hub/... -run Transport|JWT|Refresh` — PASS (11 new tests)
- `go test ./pkg/config/...` — 5 pre-existing failures (TestIsInsideProject etc.)
- `go test ./pkg/hub/...` — 15 pre-existing 'invalid UUID' failures
- No new failures introduced
