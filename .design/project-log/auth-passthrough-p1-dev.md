# auth-passthrough — Phase 1 (vertical slice: Google user ID token)

**Agent:** ap-p1-dev · **Branch:** `scion/auth-passthrough` · **Date:** 2026-09-23

## Summary

Implemented Phase 1 of `impl-design.md` §5: the Google user **ID token** end-to-end
path through `UnifiedAuthMiddleware`, sharing one identity-resolution code path
with the existing GE exchange endpoint. No new config keys, no access tokens, no
service-account admission, no caching decorator, no rate limiter — those are
later phases.

## What changed

- **`pkg/hub/federation_auth.go`** — added `(*FederationAuthenticator).IssuerConfig(url) (config.TrustedIssuerConfig, bool)`,
  cherry-picked from upstream PR `GoogleCloudPlatform/scion#1847` (closed; content
  preserved in `ptone/scion` at `/scion-volumes/scratchpad/projects/auth-passthrough/ref/1847.diff`
  and branch `ref/upstream-pr-1847`, read-only). Used by the external-bearer path
  to read Google trust config through the existing hot-reloadable pointer.
- **`pkg/hub/google_credential_validator.go`** (§4.2(i)) — `ValidateIDToken` and
  `ValidateAccessToken` no longer reject service accounts themselves; they set
  `ValidatedGoogleIdentity.IsServiceAccount` (classified from the verified email)
  and let the caller decide. The azp/aud disagreement rule for SA ID tokens is
  **unchanged** (Phase 3 work, §4.2(ii)) — SA ID tokens still fail that check
  today, which is fine because no caller admits SAs yet.
- **`pkg/hub/ge_exchange.go`** — added an explicit SA rejection in `Exchange()`
  immediately after validation (`identity.IsServiceAccount` → 403), preserving
  the exchange's pre-existing behavior byte-for-byte (S7). Removed
  `resolveLocalUser`/`resolveAfterConflict`/`provisionNewUser` (moved, see
  below). `GEExchangeService` now holds a `*GoogleIdentityResolver` built
  internally by `NewGEExchangeService` (constructor signature **unchanged** —
  all ~30 existing call sites in `*_test.go` compile and pass unmodified) and
  exposes `SetResolver` so `server.go` can swap in the shared, production
  resolver.
- **`pkg/hub/google_identity_resolver.go`** (new) — `GoogleIdentityResolver` +
  `ResolvePolicy`, extracted from `ge_exchange.go`'s former
  `resolveLocalUser`/`resolveAfterConflict`/`provisionNewUser`. Behaviour
  deltas per design: `roleFor(ctx, email)` replaces the hard-coded `"member"`
  for newly-provisioned users; `ResolvePolicy.PreAuthorized` bypasses the
  `authorize` check in provisioning only (never suspension); Google
  service-account emails now count as an authoritative bootstrap domain
  alongside Gmail/matching-`hd` (`isGoogleServiceAccount(identity.Email) ||
  isAuthoritativeEmailDomain(...)`). `NewGoogleIdentityResolver`'s `roleFor`
  and `authorize` both default to safe values (`"member"`, fail-closed) when
  nil, so `GEExchangeService`'s internally-built default resolver reproduces
  pre-refactor behavior exactly (see "Deviation" below).
- **`pkg/hub/identity.go`** — added `AuthTypeExternalBearer = "external-bearer"`.
- **`pkg/hub/auth.go`** — added `AuthConfig.GoogleValidator` /
  `AuthConfig.GoogleResolver`; added the two `serveExternalBearer` hook call
  sites in `UnifiedAuthMiddleware` (after `ValidateUserToken` fails, and in the
  `default:` unrecognized-format arm), cherry-picked from #1847's `auth.go`
  hunks. Did **not** add `ExternalUserProvisioner` or `GoogleTokenInfoURL`
  (dropped per §7 reuse map).
- **`pkg/hub/auth_external_bearer.go`** (new) — `serveExternalBearer` (response
  skeleton + `errExternalBearerNotApplicable` sentinel, cherry-picked from
  #1847), `authenticateExternalBearer`, `classifyExternalBearer` (JWT +
  unverified Google `iss` → ID token; anything else → not applicable in this
  phase — no prefix sniff, no access-token branch yet), `googleTrust` (reads
  `IssuerConfig("https://accounts.google.com")`, requires `issuer_type: user`
  and non-empty `expected_audience`), and `hasGoogleUserTrust` (the equivalent
  check server.go's `New` uses against the raw config at startup, before the
  `FederationAuthenticator` exists). An SA identity that reaches this path is
  rejected explicitly (not silently admitted), per the brief. Status mapping
  implemented for the codes reachable in this phase: not-applicable → fall
  through unchanged; `ErrUserSuspended` → 403 `user_suspended`;
  `ErrAccessDenied`/non-authoritative-email/binding-conflict → 403 `forbidden`;
  `ErrGoogleUpstreamError` (JWKS fetch failure) → 503 `upstream_unavailable`;
  everything else (bad signature, wrong audience, unverified email, SA in this
  phase) → 401 `unauthorized`, message `invalid external bearer token`.
  Dropped everything §7 says to drop: no `tokenInfoCache`, no `ya29.` sniff, no
  `GoogleTokenInfoURL`, no `ExternalUserProvisioner`. No package-level mutable
  state (I4).
- **`pkg/hub/server.go`** (`New`) — constructs the base `GoogleCredentialValidator`
  (no caching decorator — Phase 1) and one shared `GoogleIdentityResolver`
  (backed by `srv.isUserAuthorized` and `srv.getUserRole(ctx, email, "", "")`)
  whenever Google trust is configured (`hasGoogleUserTrust(cfg.Federation)`) **or**
  `cfg.GEGoogleExchange.IsValid()`. When the exchange service exists, its
  resolver is swapped for the shared instance via `SetResolver`, so both
  mechanisms make identical decisions during the soak.
- **`pkg/hub/authzop/catalog.go`** — updated the 4 mutation-classification
  entries that referenced the old `ge_exchange.go:resolveLocalUser` /
  `provisionNewUser` call sites to their new location
  (`google_identity_resolver.go:Resolve` / `provisionNewUser`), keeping
  `TestMutationClassificationBidirectional` green. Read the file immediately
  before editing per the shared-registry rule; only touched the 4 stale
  entries.
- **`pkg/hub/google_credential_validator_test.go`** — updated
  `TestProductionValidator_IDToken_ServiceAccount` and
  `TestProductionValidator_AccessToken_ServiceAccount` to assert
  `IsServiceAccount == true` with `err == nil`, instead of an error, matching
  the §4.2(i) behavior change.
- **`pkg/hub/auth_external_bearer_test.go`** (new) — see acceptance-criteria
  table below.

## Deviation from the design doc (flagged to ap-em, not yet blocking)

§4.3 says "`GEExchangeService` then holds a `*GoogleIdentityResolver` and calls
it. Its tests must pass unchanged except for the role delta." The existing
`ge_exchange_test.go` (~30 tests) constructs `GEExchangeService` via
`NewGEExchangeService(config, validator, tokenSvc, extIDStore, userStore,
authChecker, logger)` — a fixed 7-argument constructor with no room for a
`roleFor` parameter, and it turns out **no existing test asserts the
new-user role default** (I grepped for it — nothing checks `.Role` on a
freshly-provisioned user). Given that constraint, I kept the constructor
signature exactly as-is and had it build its own default resolver internally
(`roleFor` defaults to `"member"`, reproducing the pre-refactor hardcoded
value exactly), and added `(*GEExchangeService).SetResolver` for `server.go`
to inject the shared, `admin_emails`-aware resolver in production. Net effect:
zero test changes needed beyond the two SA-classification assertions above:
the "role-default assertion" the brief anticipated doesn't exist in the
current suite, so there was nothing to update. Flagging this as a design
call rather than assuming it's uncontroversial — happy to change the
constructor shape (e.g., add a `roleFor` parameter and update all call sites)
if ap-em/the reviewer would rather have that instead of the `SetResolver` seam.

## Source material

Upstream PR `GoogleCloudPlatform/scion#1847` is now closed. Its content was
originally fetched from `bobbymatthews/scion:feat/ge-external-bearer-gating`
(`4df5374`) for the accessor/hook-site cherry-picks (before ap-em's note that
the durable copies are `/scion-volumes/scratchpad/projects/auth-passthrough/ref/1847.diff`
and `ref/upstream-pr-1847` on `ptone/scion`, read-only — same content, so no
rework was needed). Commits carrying his hunks (the `IssuerConfig` accessor,
the two `UnifiedAuthMiddleware` hook sites, `AuthTypeExternalBearer`, and the
`serveExternalBearer` skeleton/sentinel) carry the
`Co-authored-by: Bobby Matthews <bobbymatthews@google.com>` trailer.

## Verification

- `gofmt -l` clean on all touched files.
- `go build -buildvcs=false ./pkg/hub/...` — clean.
- `go vet -buildvcs=false ./pkg/hub/...` — clean.
- `go test -buildvcs=false ./pkg/hub/...` — full package + subpackages (see
  report to ap-em for the final run's pass/fail breakdown).
- Targeted run of every test added/touched in this phase
  (`TestExternalBearer_*`, `TestHasGoogleUserTrust`,
  `TestNoTokenInfoOutsideGoogleCredentialValidator`, all `TestGEExchange_*`,
  all `TestProductionValidator_*`) — all green.
