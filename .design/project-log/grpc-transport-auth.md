# gRPC Transport Authentication — Hub-to-Bridge

**Date**: 2026-09-18
**Issue**: #1619
**Branch**: `scion/dev-grpc-transport`
**Base**: `49f89d8` (main at `b2856682`)

## Summary

Implemented authenticated gRPC transport for the Hub→Bridge path, closing
the security gap where the production adapter factory and bridge gRPC server
had no authentication or TLS support. All EM security findings addressed:
fail-closed remote defaults, concrete JWKS-based token validation,
principal authorization, startup config validation, TLS/mTLS, and
bridge `main.go` production wiring.

## Changes

### Config schema (`pkg/plugin/config.go`, `pkg/config/settings_v1.go`)

Added `auth_type` and `auth_audience` fields to `PluginEntry` and
`V1PluginEntry`. These flow through `initPluginManager` in
`cmd/server_foreground.go` to the adapter factory.

### Client-side auth (`pkg/plugin/grpcbroker/auth.go`)

- `TokenSourceCredentials`: adapts `transportauth.TokenSource` to gRPC
  `PerRPCCredentials`. Each RPC gets a fresh-or-cached token; the
  underlying source handles refresh.
- `extractBearerToken`: server-side helper to extract bearer tokens from
  gRPC metadata.
- `TokenValidator` / `TokenValidatorFunc`: interface for server-side token
  validation, used by interceptors.

### Factory wiring (`pkg/plugin/grpcbroker/factory.go`)

- `NewAdapterFromEntry` now reads `AuthType` and `AuthAudience` from the
  `PluginEntry` and resolves a `PerCallAuthenticator`.
- For `google_id_token`: uses GCE metadata server on GCE, falls back to
  ADC off-GCE. Requires `auth_audience`.
- ADC constructor is injected via `SetADCSourceConstructor` to avoid
  linking `google.golang.org/api/idtoken` into lean binaries.
- **Fails closed** for remote addresses without auth (explicit `none` or
  missing `auth_type` both rejected).

### Concrete token validators (`pkg/plugin/grpcbroker/tokenvalidator.go`)

- `GoogleIDTokenValidator`: JWKS-based Google OIDC ID token validation
  with issuer, audience, expiry, email, email_verified, and principal
  authorization via `AuthorizedSubjects` allowlist.
- `HMACTokenValidator`: symmetric-key JWT validation with audience,
  expiry, and optional subject authorization.
- `StandaloneServerConfig` + `ValidateStandaloneServerConfig`: fail-closed
  startup validation for the bridge's gRPC server auth config.
- `BuildStandaloneServerOptions`: creates server options from validated config.
- `validateTLSFields`: checks cert/key/CA consistency.

### Server-side auth (`pkg/plugin/grpcbroker/serverauth.go`)

- `UnaryAuthInterceptor` / `StreamAuthInterceptor`: validate bearer tokens
  on all incoming RPCs (both unary and streaming).
- `ServerOptions`: creates `grpc.ServerOption` slices from a
  `ServerAuthConfig`, combining auth interceptors and TLS credentials.
- `serverTLSConfig`: native server TLS with optional mTLS (client CA
  verification with `RequireAndVerifyClientCert`).

### Bridge production wiring (`extras/scion-a2a-bridge/cmd/scion-a2a-bridge/main.go`)

- `resolveGRPCServerAuth`: reads auth config from environment variables
  (`GRPC_AUTH_MODE`, `GRPC_AUTH_AUDIENCE`, `GRPC_AUTH_SUBJECTS`,
  `GRPC_AUTH_HMAC_KEY`, `GRPC_TLS_CERT/KEY/CLIENT_CA`).
- Calls `ValidateStandaloneServerConfig` at startup — fails closed on
  invalid config.
- Calls `BuildStandaloneServerOptions` to get server options.
- **TLS/mux separation**: TLS fields are stripped when `muxPorts=true`
  (Cloud Run h2c) because native `grpc.Creds` is incompatible with the
  h2c `grpcServer.ServeHTTP` path. Auth interceptors apply in both modes.

### Hub startup (`cmd/server_foreground.go`)

- Wires `adcsource.New` into `grpcbroker.SetADCSourceConstructor`.
- Maps new `AuthType`/`AuthAudience` fields from `V1PluginEntry` to
  `PluginEntry` in `initPluginManager`.

### Tests (`pkg/plugin/grpcbroker/auth_test.go`)

27+ test cases covering:
- TokenSourceCredentials (metadata, TLS requirement, errors, rotation)
- Auth interceptor (authorized, unauthorized/no-token, wrong-token, rotation)
- Factory path (no-auth, google_id_token with mock, missing audience,
  unsupported type, no credential source)
- Server options (with/without validator)
- TLS (valid client, wrong CA, wrong client cert, missing client cert)
- Combined TLS + auth end-to-end
- Reconnect preserves auth
- Bearer token extraction (valid, missing metadata/header, wrong format, empty)
- All 6 control methods protected when auth is enabled

### Tests (`pkg/plugin/grpcbroker/tokenvalidator_test.go`)

30+ test cases covering:
- GoogleIDTokenValidator: valid token, wrong audience, expired, wrong
  issuer, wrong signing key, missing audience, both Google issuers,
  no email claim, unverified email
- **GE invoker negative test**: proves a valid Google token from GE
  Discovery Engine SA is rejected on all 6 control methods
- HMACTokenValidator: valid, wrong key, unauthorized subject
- Config validation: all auth modes × valid/invalid combinations,
  local_dev remote fail-closed, no-auth remote fail-closed
- TLS validation: cert-without-key, key-without-cert, client-CA-without-cert
- BuildStandaloneServerOptions: local_dev, hmac, google_id_token
- Factory fail-closed: remote-no-auth, remote-explicit-none, local-allowed

### Documentation (`extras/scion-a2a-bridge/docs/grpc-transport-auth.md`)

Comprehensive deployment guide covering:
- Hub `settings.yaml` configuration with `mode: grpc`, auth, and TLS fields
- Bridge server-side environment variable configuration
- Cloud Run deployment (audience, principal authorization,
  X-Serverless-Authorization distinction, IAM invoker, single-port h2c mux)
- Kubernetes deployment (native TLS, mTLS, ingress/sidecar)
- Principal authorization (Hub vs. GE invoker separation)
- TLS/mux-port separation
- Security requirements and fail-closed scenarios
- Credential lifecycle (refresh, rotation, reconnection)
- Locally verified vs. unperformed live validation table

## Boundaries

- Did NOT implement #1616/#1617 user credential exchange or #1618 task
  persistence.
- Did NOT create upstream PRs or deploy to production.
- Did NOT touch the taskstore construction block in bridge `main.go`.
- No broad dependency upgrades; only existing project dependencies used.
- Bridge `go.mod/go.sum` updated via `go mod tidy` for transitive deps.

## Test Results

```
go test ./pkg/plugin/grpcbroker/... -count=1
ok  github.com/GoogleCloudPlatform/scion/pkg/plugin/grpcbroker  0.118s

go test ./pkg/plugin/... -count=1
ok  github.com/GoogleCloudPlatform/scion/pkg/plugin           0.014s
ok  github.com/GoogleCloudPlatform/scion/pkg/plugin/grpcbroker 0.140s
ok  github.com/GoogleCloudPlatform/scion/pkg/plugin/refbroker  0.212s

go vet ./pkg/plugin/... ./pkg/config/...
(clean)

go build -buildvcs=false ./cmd/...
(clean)

# Bridge module (separate Go module):
cd extras/scion-a2a-bridge && go build -buildvcs=false ./cmd/scion-a2a-bridge/
(clean)
```

**Gates not run (environment limit)**:
- `make ci` — requires full toolchain alignment; scoped checks above are
  the narrowest passing subset.
- Live Cloud Run / Kubernetes / GCE metadata / ADC — requires deployed
  infrastructure. Tracked by #1620.

## Residual Work

1. Live Cloud Run / Kubernetes / GCE metadata validation is deferred to #1620.
2. `HealthCheck` swallows auth errors (returns degraded status) — existing
   design, documented and tested.
