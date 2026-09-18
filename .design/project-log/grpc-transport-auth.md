# gRPC Transport Authentication — Hub-to-Bridge

**Date**: 2026-09-18
**Issue**: #1619
**Branch**: `scion/dev-grpc-transport`
**Base**: `49f89d8` (main at `b2856682`)

## Summary

Implemented authenticated gRPC transport for the Hub→Bridge path, closing
the security gap where the production adapter factory and bridge gRPC server
had no authentication or TLS support.

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
- Warns when no auth is configured for remote addresses.

### Server-side auth (`pkg/plugin/grpcbroker/serverauth.go`)

- `UnaryAuthInterceptor` / `StreamAuthInterceptor`: validate bearer tokens
  on all incoming RPCs.
- `ServerOptions`: creates `grpc.ServerOption` slices from a
  `ServerAuthConfig`, combining auth interceptors and TLS credentials.
- `serverTLSConfig`: native server TLS with optional mTLS (client CA
  verification).

### Hub startup (`cmd/server_foreground.go`)

- Wires `adcsource.New` into `grpcbroker.SetADCSourceConstructor`.
- Maps new `AuthType`/`AuthAudience` fields from `V1PluginEntry` to
  `PluginEntry` in `initPluginManager`.

### Tests (`pkg/plugin/grpcbroker/auth_test.go`)

27 new test cases covering:
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

### Documentation (`extras/scion-a2a-bridge/docs/grpc-transport-auth.md`)

Comprehensive deployment guide covering:
- Hub `settings.yaml` configuration with `mode: grpc`, auth, and TLS fields
- Cloud Run deployment (audience, X-Serverless-Authorization distinction,
  IAM invoker, single-port h2c mux)
- Kubernetes deployment (native TLS, mTLS, ingress/sidecar)
- Security requirements and fail-closed scenarios
- Credential lifecycle (refresh, rotation, reconnection)
- Locally verified vs. unperformed live validation table

## Boundaries

- Did NOT modify bridge `main.go` (shared with `dev-a2a-taskstore`; requires
  coordination).
- Did NOT implement #1616/#1617 user credential exchange or #1618 task
  persistence.
- Did NOT create upstream PRs or deploy to production.
- No broad dependency upgrades; only existing project dependencies used.

## Test Results

```
go test ./pkg/plugin/grpcbroker/... -count=1
ok  github.com/GoogleCloudPlatform/scion/pkg/plugin/grpcbroker  0.102s

go test ./pkg/plugin/... -count=1
ok  github.com/GoogleCloudPlatform/scion/pkg/plugin           0.014s
ok  github.com/GoogleCloudPlatform/scion/pkg/plugin/grpcbroker 0.122s
ok  github.com/GoogleCloudPlatform/scion/pkg/plugin/refbroker  0.213s
```

## Residual Work

1. Bridge `main.go` needs server-side interceptors wired into `serveStandalone`
   (coordinate with `dev-a2a-taskstore` before editing).
2. Bridge server TLS flags (`--grpc-tls-cert`, `--grpc-tls-key`,
   `--grpc-tls-client-ca`) need to be added to the bridge CLI.
3. Live Cloud Run / Kubernetes / GCE metadata validation is deferred to #1620.
