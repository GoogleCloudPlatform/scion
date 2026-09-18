# gRPC Transport Authentication — Hub-to-Bridge

**Date**: 2026-09-18
**Issue**: #1619
**Branch**: `scion/dev-grpc-transport`
**Base**: `370a026` (origin/main)

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

### Review fixes (post-f95a124)

1. **Bridge test mock interface** (`followup_test.go`): Added missing
   `Messaging() hubclient.MessagingService` method to `mockHubClient`,
   fixing compilation after the `MessagingService` interface addition in
   Phase 5.

2. **Wildcard listen address fail-closed** (`tokenvalidator.go`): Added
   `isLocalListenAddress` that treats empty host (`:50051`), `0.0.0.0`,
   and `::` as NON-local wildcard binds. Updated
   `ValidateStandaloneServerConfig` to use it instead of `isLocalAddress`
   (which treats empty host as local for client dial semantics). This
   closes the fail-open vulnerability where a bridge on `:50051` with no
   auth mode would silently accept unauthenticated connections on all
   interfaces.

3. **JWKS singleflight + cooldown** (`tokenvalidator.go`): Refactored
   `getSigningKey` so network I/O (JWKS fetch) happens OUTSIDE the
   validator mutex. Concurrent refreshes are coalesced via
   `singleflight.Group`. Cache reads use a short `RLock`; cache updates
   use a short `Lock` after the fetch completes. Cached valid-key
   lookups return immediately even during a slow refresh. Added
   `minJWKSRefreshInterval` (1 minute) cooldown to prevent DoS via
   unknown-kid stampede — if the kid is not found within the cooldown,
   the error is returned without re-fetching. Also added `io.LimitReader`
   (1 MiB) on the JWKS response body to bound memory allocation.

4. **Dynamic activation auth fields** (`handlers_integrations.go`):
   Added `AuthType` and `AuthAudience` to the `PluginEntry` constructed
   in `activateInstalledIntegration`, so dynamically activated plugins
   inherit authentication settings from `settings.yaml`. The mock
   `IntegrationManager` in the hub test suite now captures the full
   `PluginEntry` in `loadOneEntries` for assertion.

5. **Mux comment correction** (`main.go`): Fixed misleading comment that
   described the mux-mode listen address as "always local" — it is a
   wildcard bind requiring auth.

### Regression tests added

- `TestIsLocalListenAddress` — 9 subtests covering localhost, loopback,
  wildcard, empty host, remote hostname, private IP
- `TestValidateStandaloneServerConfig_WildcardListen_FailsClosed` —
  reproduces the exact fail-open condition (`:50051` + empty auth)
- `TestValidateStandaloneServerConfig_WildcardListen_LocalDev_FailsClosed`
- `TestValidateStandaloneServerConfig_WildcardListen_GoogleIDToken_Accepted`
- `TestGoogleIDTokenValidator_JWKSCooldown_PreventsStampede` — verifies
  5 unknown-kid attempts trigger zero additional JWKS fetches
- `TestGoogleIDTokenValidator_CachedKey_NotBlockedBySlowRefresh` —
  proves cached valid-key verification returns immediately while a slow
  JWKS refresh is in-flight
- `TestGoogleIDTokenValidator_ConcurrentRefreshes_Coalesce` — proves 10
  concurrent refreshes result in 1 HTTP fetch
- `TestActivateInstalledIntegration_AuthFieldsPropagatedToLoadOne` —
  real `pkg/hub` regression test exercising `Server.activateInstalledIntegration`
  and asserting `LoadOne` receives `AuthType`/`AuthAudience`

## Boundaries

- Did NOT implement #1616/#1617 user credential exchange or #1618 task
  persistence.
- Did NOT create upstream PRs or deploy to production.
- Did NOT touch the taskstore construction block in bridge `main.go`.
- HMAC de-scoped from production standalone config (no client-side minting).

## Test Results

```
go test ./pkg/plugin/grpcbroker/... -count=1
ok  github.com/GoogleCloudPlatform/scion/pkg/plugin/grpcbroker  1.176s

go test ./pkg/plugin/... -count=1
ok  github.com/GoogleCloudPlatform/scion/pkg/plugin           0.014s
ok  github.com/GoogleCloudPlatform/scion/pkg/plugin/grpcbroker 1.213s
ok  github.com/GoogleCloudPlatform/scion/pkg/plugin/refbroker  0.213s

# Hub activation test:
go test ./pkg/hub/ -count=1 -run TestActivateInstalledIntegration
ok  github.com/GoogleCloudPlatform/scion/pkg/hub  0.115s

# Bridge test suite:
cd extras/scion-a2a-bridge && go test ./... -count=1
ok  .../internal/bridge  23.194s
ok  .../internal/state    0.262s

go vet ./pkg/plugin/... ./pkg/config/... ./pkg/hub/...
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

## PR #1743 bounded pre-merge fixes (2026-09-18)

The accepted transport tip `e9a4c8858cb4ffb88678f51d56a75043e9bf90d9`
was preserved as the first parent of merge commit
`a059d453cfae7cf74ff44f21480093b09c11c060`. The second parent is the
then-current `origin/main` tip
`21c380344b774fc09a9147f38f9b96be4ca77f34`; the merge was conflict-free.
No rebase or force-push was used, and the original
`scion/dev-grpc-transport` ref was not updated.

The upstream review finding at
`discussion_r4047071974` was confirmed with a red regression: canceling the
request that led the shared JWKS singleflight aborted the HTTP fetch and made
an uncanceled coalesced caller fail with `context canceled`. The refresh now
uses `singleflight.DoChan`; each caller selects on its own context, while the
coalesced network request uses a detached context bounded by the validator's
JWKS fetch timeout. This prevents leader-cancellation coupling without leaving
unbounded work when all callers cancel.

Regressions cover leader cancellation, all waiters canceling, fetch timeout,
cache/cooldown, unknown-kid suppression, cached-key availability during a
refresh, and concurrent coalescing. Focused tests passed normally, with the
race detector, and for 30 repetitions. The complete `grpcbroker`, plugin, and
nested A2A bridge suites passed, as did Cloud Run ingress/principal negatives
and dynamic activation auth propagation. Go 1.26.1 `gofmt`, vet, root and
bridge builds, scoped golangci-lint, and `git diff --check` were clean.

The only lint-only edits were checking `Close` in `auth_test.go` and removing
the behaviorless empty branch in `tokenvalidator.go`. No tidy command was run,
and unmerged baseline tidy PR #1744 / commit `07721e83` was not imported or
duplicated.

An additional `make ci` attempt stopped on legacy literals and conversation
guard failures already present in the merged `origin/main`, while its
`test-fast` stage exposed unrelated baseline/environment failures in command,
config, Hub, authz, runtime, and runtime-broker packages. The transport package
remained green. `make lint` and `make build` passed; the unrelated failures
were documented in the durable report and intentionally left out of this
bounded fix.

## Post-merge duplicate mock resolution (2026-09-18)

Following the upstream merge of PR #1741 into `main` (`7c0e3a26856136975a479715724db37fabde1bfa`),
the A2A bridge test suite failed to compile due to a duplicate method declaration:
`mockHubClient.Messaging()` was declared at both lines 163 and 166 in
`extras/scion-a2a-bridge/internal/bridge/followup_test.go`:
`internal/bridge/followup_test.go:166:25: method mockHubClient.Messaging already declared at internal/bridge/followup_test.go:163:25`.

A fresh fix branch `scion/dev-postmerge-mock-dedup` was created directly from upstream `main`
(`7c0e3a26856136975a479715724db37fabde1bfa`), without importing historical composition branches
(such as `9abf6c8`) or rebasing old PR branches. The redundant line 166 declaration was removed,
preserving line 163 and all test behaviors.

Validation confirmed clean compilation and test execution:
- Full A2A bridge test suite passed cleanly (`go test ./...` in 34.9s).
- Full A2A bridge race detector passed cleanly (`go test -race ./...` in 36.5s).
- Focused follow-up tests passed cleanly (`go test -run TestSendFollowUp ./internal/bridge/...`).
- Bridge vet and build passed cleanly (`go vet ./...` and `go build ./cmd/scion-a2a-bridge/`).
- Root build and vet passed cleanly (`go build ./cmd/...` and `go vet ./pkg/plugin/... ./pkg/config/... ./pkg/hub/...`).
- Root plugin tests passed cleanly (`go test ./pkg/plugin/...`).
- Hub integration activation test passed (`go test ./pkg/hub/ -run TestActivateInstalledIntegration`).
- Formatting (`gofmt`), git diff sanity (`git diff --check`), compatibility literals (`make compat-literals`), and scoped `golangci-lint` passed with zero issues.
- Baseline `authzop` catalog mismatches against new API endpoints in `7c0e3a2` were classified as pre-existing and isolated from this fix.

