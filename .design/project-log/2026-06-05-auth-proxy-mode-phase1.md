# Auth Proxy Mode — Phase 1 Implementation

**Date:** 2026-06-05  
**Branch:** scion/auth-proxy-mode  
**Author:** Scion Agent (auth-proxy-phase1)

## Summary

Implemented Phase 1 (inbound human IAP auth) of the auth-proxy-mode feature,
delivering items 2–5 of the design plan in `.design/auth-proxy-mode.md`.

## Files Added/Changed

### New
- `pkg/hub/proxyauth.go` — ProxyAuthenticator interface, ProxyUserInfo struct,
  IAPAuthenticator (ES256 JWT verification via go-jose/v4), JWKS cache
- `pkg/hub/proxyauth_test.go` — 13 unit tests

### Modified
- `pkg/config/hub_config.go` — Added `Mode`, `Proxy` (ProxyAuthConfig/IAPAuthConfig)
  to DevAuthConfig
- `pkg/config/settings_v1.go` — Added `Mode`, `Proxy` (V1ProxyConfig/V1IAPConfig)
  to V1AuthConfig; updated both conversion functions; extended compound fields
  and section names for env var mapping
- `pkg/hub/auth.go` — Replaced IP-only extractProxyUser branch with
  ProxyAuthenticator path; added ProxyUserCache (60s TTL);
  MakeProxyUserProvisioner; added ProxyAuthenticator/ProxyUserProvisioner
  fields to AuthConfig
- `pkg/hub/handlers_auth.go` — Added ErrUserSuspended; suspended-user gate in
  provisionUser; updated all 4 provisionUser callers to handle ErrUserSuspended
- `pkg/hub/server.go` — Added AuthMode/ProxyAuth to ServerConfig; wired into
  authConfig with MakeProxyUserProvisioner
- `pkg/hub/web.go` — Added AuthMode to WebServerConfig; handleAuthProviders
  returns empty in proxy mode; handleLogout redirects to IAP clear_login_cookie
  in proxy mode
- `cmd/server_foreground.go` — Construct IAPAuthenticator when mode==proxy &&
  provider==iap; wire AuthMode into hub and web configs

## Design Decisions

### Audience/Issuer/Exp Validation
- **Audience**: mandatory binding — IAPAuthenticator.Audience must be set,
  validated against JWT aud claim
- **Issuer**: defaults to `https://cloud.google.com/iap`, overridable via
  struct field for testing
- **Clock skew**: ±30s leeway on exp/iat
- **JWKS URL**: defaults to gstatic, overridable for testing

### JWKS Cache Design
- Lazy fetch on first request
- Proactive background refresh when cache > 1 hour old
- On-miss refresh for unknown kid (key rotation)
- Transient failure tolerance: serves last-good keys if fetch fails
- 5s debounce to prevent stampede

### Resolution Cache
- 60s TTL keyed by verified email (per design Decision 3)
- JWT signature verification runs every request — only the provisionUser
  store lookup is cached
- Implemented as ProxyUserCache (sync.RWMutex + map)

### Suspended User Gate
- Added to provisionUser — rejects Status=="suspended" with ErrUserSuspended
- Intentional behavior change closing the pre-existing OAuth suspended-login gap
  documented in Phase 0's NOTE comment
- All 4 provisionUser callers updated to surface 403 "user_suspended"

### Proxy Precedence
- Proxy authenticator runs AFTER agent token (step 1), broker HMAC (step 2),
  and bearer token (step 3) — ensuring app-layer credentials take priority
- When no ProxyAuthenticator is configured, legacy extractProxyUser (IP-trust)
  is preserved for backward compatibility

## Test Results

### New Tests (all passing)
13 tests in proxyauth_test.go:
- Valid assertion → correct ProxyUserInfo
- Missing header → (nil, nil) fall-through
- Bad signature → error
- Wrong audience → error
- Wrong issuer → error
- Expired token → error
- Custom issuer override
- Unknown kid triggers JWKS refresh
- Strip prefix
- Email lowercasing
- HD claim
- Name() returns "iap"
- JWKS transient failure tolerance

### Pre-existing Failures (unchanged)
~15 pre-existing "invalid UUID" failures in other hub tests (unrelated to auth):
TestCreateAgent_ResumeFromStoppedStatus, TestPopulateAgentConfig_*, etc.
~5 pre-existing config test failures from leaked SCION_ env vars in sandbox.

## Flags / Notes

- **HeaderProxyAuthenticator** (refactoring extractProxyUser behind the
  interface) was left as a TODO — the legacy path is preserved but not
  refactored behind the interface. Lower priority per design doc.
- **No new dependencies** — uses already-vendored go-jose/go-jose/v4.
