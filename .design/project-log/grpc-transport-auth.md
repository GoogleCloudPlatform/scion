# gRPC Transport Authentication — Hub-to-Bridge

**Date**: 2026-09-18
**Issue**: #1619
**Branch**: `scion/dev-grpc-transport`
**Base**: `49f89d8` (main at `b2856682fbd2ed43588c759cfb8e54e90556becf`)

## Summary

Implemented authenticated gRPC transport for the Hub→Bridge path, closing
the security gap where the production adapter factory and bridge gRPC server
had no authentication or TLS support. All EM security findings addressed
including: fail-closed remote defaults, concrete JWKS-based token validation,
principal authorization, exp requirement, RS256 algorithm pinning, Cloud Run
dual-header semantics, HMAC de-scoping, startup config validation, TLS/mTLS,
bridge `main.go` production wiring, and factory-to-server integration tests.

## Changes

### Config schema (`pkg/plugin/config.go`, `pkg/config/settings_v1.go`)

Added `auth_type` and `auth_audience` fields to `PluginEntry` and
`V1PluginEntry`. These flow through `initPluginManager` in
`cmd/server_foreground.go` to the adapter factory.

### Client-side auth (`pkg/plugin/grpcbroker/auth.go`)

- `TokenSourceCredentials`: adapts `transportauth.TokenSource` to gRPC
  `PerRPCCredentials`. Supports `WithCloudRunHeader()` option to send
  the same token in both `authorization` and `x-serverless-authorization`
  metadata for Cloud Run platform compatibility.
- `extractBearerToken`: server-side helper, reads only from `authorization`
  metadata — never from `x-serverless-authorization`.
- `TokenValidator` / `TokenValidatorFunc`: interface for server-side token
  validation, used by interceptors.

### Factory wiring (`pkg/plugin/grpcbroker/factory.go`)

- `NewAdapterFromEntry` reads `AuthType` and `AuthAudience` from
  `PluginEntry` and resolves a `PerCallAuthenticator`.
- For `google_id_token`: uses GCE metadata → ADC fallback. Cloud Run
  dual-header automatically enabled via `WithCloudRunHeader()`.
- **Fails closed** for remote addresses without auth (explicit `none` or
  missing `auth_type` both rejected).

### Concrete token validators (`pkg/plugin/grpcbroker/tokenvalidator.go`)

- `GoogleIDTokenValidator`: JWKS-based Google OIDC ID token validation.
  Algorithm pinned to RS256 only (Google's documented algorithm). Validates
  issuer, audience, mandatory exp, email (stable SA identifier, not
  opaque numeric sub), email_verified, and `AuthorizedSubjects` allowlist.
- `HMACTokenValidator`: symmetric-key JWT validation with mandatory issuer,
  audience, exp, and subject authorization. De-scoped from production
  standalone config (no interoperable client-side HMAC minting in factory);
  retained for testing.
- `StandaloneServerConfig` + `ValidateStandaloneServerConfig`: fail-closed
  startup validation. Supported modes: `google_id_token`, `local_dev`.
- `BuildStandaloneServerOptions`: creates server options from validated config.

### Server-side auth (`pkg/plugin/grpcbroker/serverauth.go`)

- `UnaryAuthInterceptor` / `StreamAuthInterceptor`: validate bearer tokens
  from `authorization` metadata on all incoming RPCs.
- `ServerOptions`: creates `grpc.ServerOption` slices combining interceptors
  and TLS credentials.
- `serverTLSConfig`: native server TLS with optional mTLS.

### Bridge production wiring (`extras/scion-a2a-bridge/cmd/scion-a2a-bridge/main.go`)

- `resolveGRPCServerAuth`: reads env vars (`GRPC_AUTH_MODE`, `GRPC_AUTH_AUDIENCE`,
  `GRPC_AUTH_SUBJECTS`, `GRPC_TLS_CERT/KEY/CLIENT_CA`). HMAC env vars removed.
- Calls `ValidateStandaloneServerConfig` at startup — fails closed.
- TLS fields stripped when `muxPorts=true` (Cloud Run h2c).

### Hub startup (`cmd/server_foreground.go`)

- Wires `adcsource.New` into `grpcbroker.SetADCSourceConstructor`.
- Maps new `AuthType`/`AuthAudience` fields.

### Tests

**`auth_test.go`** — 27+ test cases covering auth interceptors, TLS,
factory paths, reconnection, and bearer token extraction.

**`tokenvalidator_test.go`** — 40+ test cases covering:
- Google validator: valid token, wrong audience, expired, missing exp,
  wrong issuer, wrong signing key, missing audience, both issuers,
  no email, unverified email
- GE invoker negative test (all 6 control methods rejected)
- HMAC validator: valid, wrong key, unauthorized subject, missing exp,
  wrong issuer, missing issuer, missing subjects
- Config validation: all modes × valid/invalid, HMAC rejected in standalone
- Cloud Run ingress simulation: GE invoker with x-serverless only (rejected),
  no headers (rejected), GE in authorization (wrong principal, rejected),
  Hub with dual headers (accepted), Hub with authorization only (accepted)
- Cloud Run metadata passthrough: x-serverless cannot substitute for
  authorization; authorization authenticates
- Production factory-to-server end-to-end: `NewAdapterFromEntry` to
  `BuildStandaloneServerOptions`, successful RPC + unauthorized rejection
- Dual-header option: sends both headers when enabled, only authorization
  when disabled

### Documentation

- `extras/scion-a2a-bridge/docs/grpc-transport-auth.md`: deployment guide
  with Cloud Run dual-header semantics, env var reference, principal
  authorization, verification matrix
- `.design/project-log/grpc-transport-auth.md`: this log

## Boundaries

- Did NOT implement #1616/#1617 user credential exchange or #1618 task
  persistence.
- Did NOT create upstream PRs or deploy to production.
- Did NOT touch the taskstore construction block in bridge `main.go`.
- HMAC de-scoped from production standalone config (no client-side minting).

## Test Results

```
go test ./pkg/plugin/grpcbroker/... -count=1
ok  github.com/GoogleCloudPlatform/scion/pkg/plugin/grpcbroker  0.831s

go test ./pkg/plugin/... -count=1
ok  github.com/GoogleCloudPlatform/scion/pkg/plugin           0.013s
ok  github.com/GoogleCloudPlatform/scion/pkg/plugin/grpcbroker 0.758s
ok  github.com/GoogleCloudPlatform/scion/pkg/plugin/refbroker  0.213s

# Bridge test suite:
cd extras/scion-a2a-bridge && go test ./... -count=1
ok  .../internal/bridge  23.086s
ok  .../internal/state    0.226s

go vet ./pkg/plugin/... ./pkg/config/...
(clean)

go build -buildvcs=false ./cmd/...
(clean)

cd extras/scion-a2a-bridge && go build -buildvcs=false ./cmd/scion-a2a-bridge/
(clean)
```

## Residual Work

1. Live Cloud Run / Kubernetes / GCE metadata validation deferred to #1620.
2. `HealthCheck` swallows auth errors (returns degraded status) — existing
   design, documented and tested.
