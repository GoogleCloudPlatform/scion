# auth-passthrough — Phase 1 (vertical slice: Google user ID token)

**Agent:** ap-p1-dev · **Branch:** `scion/auth-passthrough` · **Date:** 2026-09-23

## Summary

Implemented Phase 1 of `impl-design.md` §5: the Google user **ID token** end-to-end
path through `UnifiedAuthMiddleware`, sharing one identity-resolution code path
with the existing GE exchange endpoint. No new config keys, no access tokens, no
service-account admission, no caching decorator, no rate limiter — those are
later phases.

## What changed

> This section describes the state reviewed in r1 (commit `6a7ba5f`). See
> **Fix round 1** below for what changed since then — several things
> mentioned here (`hasGoogleUserTrust`, `SetResolver`, the exchange's
> internally-built default resolver) were removed during the fix round.


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
  `default:` unrecognized-format arm), cherry-picked from `GoogleCloudPlatform/scion#1847`'s
  `auth.go` hunks. Did **not** add `ExternalUserProvisioner` or `GoogleTokenInfoURL`
  (dropped per §7 reuse map).
- **`pkg/hub/auth_external_bearer.go`** (new) — `serveExternalBearer` (response
  skeleton + `errExternalBearerNotApplicable` sentinel, cherry-picked from
  `GoogleCloudPlatform/scion#1847`), `authenticateExternalBearer`, `classifyExternalBearer` (JWT +
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
- `go build -buildvcs=false ./...` (whole repo, not just `pkg/hub`) — clean.
- `go vet -buildvcs=false ./pkg/hub/...` — clean.
- `go test -buildvcs=false -timeout 15m ./pkg/hub/` — 557.98s, 4 failures:
  `TestDEF164_AtAgentSlug_DeliversToAgent`,
  `TestDEF164_AtAgentSlug_DMConversationCreated`,
  `TestDEF152_AgentToAgentDM_DeliversViaOutbound`,
  `TestCreateTemplateV2_ScopeIDInjectionBlocked`. Confirmed all 4 pre-exist on
  `ca486fa` via a detached baseline worktree (`git worktree add --detach
  /tmp/baseline-check ca486fa`) — unrelated areas (agent-to-agent DM delivery,
  template scope injection), not touched by this change.
- `go test -buildvcs=false ./pkg/hub/authzop/... ./pkg/hub/auth/...
  ./pkg/hub/githubapp/... ./pkg/hub/imagecheck/...` — all green (authzop
  needed the `catalog.go` mutation-classification update above).
- Targeted run of every test added/touched in this phase
  (`TestExternalBearer_*`, `TestHasGoogleUserTrust`,
  `TestNoTokenInfoOutsideGoogleCredentialValidator`, all `TestGEExchange_*`,
  all `TestProductionValidator_*`) — all green, verbose, individually confirmed.
- `make ci`: `fmt-check`/`lint`/`check-custom` all green. `test-fast` (`go test
  -tags no_sqlite ./...`, whole repo) has widespread pre-existing failures
  outside `pkg/hub` (`pkg/config`, `pkg/agent`, `pkg/runtime`,
  `pkg/runtimebroker`, `cmd`, ...); spot-checked `pkg/config` against the
  `ca486fa` baseline and confirmed the same failures there — a config-decoding
  issue (`'auto_expose_ports' expected a map or struct, got "string"`)
  unrelated to auth-passthrough. Did not touch those packages.
- `make build` — clean.

## Fix round 1 (review r1, verdict REQUEST CHANGES: 0 Critical, 8 Required, 4 Optional, 2 Nit, 3 FYI)

Reviewer: `ap-p1-rev`. Report: `/scion-volumes/scratchpad/projects/auth-passthrough/reviews/p1-r1-ap-p1-rev.md`.
EM dispositions: `/scion-volumes/scratchpad/projects/auth-passthrough/briefs/ap-p1-dev-fix-r1.md`.
Every finding resolved per the EM's disposition table; details below.

### Required

- **R1 (empty `expected_audience` guard ineffective).** `NewFederationAuthenticator`
  was replacing an empty `ExpectedAudience` with the Hub's OIDC issuer URL and
  storing only the resolved copy, so `IssuerConfig`/`googleTrust` could never see
  "not configured." Fixed by giving `issuerEntry` a second field (`rawConfig`,
  the as-configured copy with no fallback applied); `IssuerConfig` now returns
  `rawConfig`, while `Authenticate()` still uses the resolved `config` for
  federation-token validation. Tests: `TestGoogleTrust_EmptyExpectedAudience_NotOK`
  (unit), `TestExternalBearer_EmptyExpectedAudience_NotApplicable` (integration:
  a validly-signed Google token whose `aud` equals the fallback Hub URL is
  rejected as not-applicable, validator never called).
- **R2 (validator not shared with the exchange).** `server.go` built two
  independent `NewGoogleCredentialValidator(nil)` instances (two JWKS caches,
  two HTTP clients). Fixed: one validator + one resolver built once in `New`,
  both passed into `NewGEExchangeService`. Test:
  `TestGEExchange_Route_SharesValidatorAndResolverWithExternalBearer` (asserts
  pointer identity between `srv.geExchangeService.validator`/`.resolver` and
  `srv.authConfig.GoogleValidator`/`.GoogleResolver` after a real `New()`).
- **R3 (exchange role delta untested; dead `SetResolver` seam).** Took the
  preferred fix: `NewGEExchangeService` now takes a `*GoogleIdentityResolver`
  directly (replacing `extIDStore, userStore, authChecker`); `SetResolver` and
  the constructor's internally-built default resolver are gone (also closes
  D2). All ~35 test call sites across `ge_exchange_test.go` and
  `google_credential_validator_test.go` updated via a new `newTestResolver`
  helper. Added `TestGEExchange_AdminEmails_ProvisionsAdminRole` (exercises the
  role delta through the same resolver production actually uses) and the R2
  wiring test above (also covers R3's "minimum acceptable" ask, done via the
  preferred route instead).
- **R4 (no test for SA rejection on the external-bearer path).** Added
  `TestExternalBearer_ServiceAccountIDToken_Rejected`: SA ID token with
  `aud=azp=`expected audience → 401, exact body `invalid external bearer
  token`, handler not reached, no user or binding created.
- **R5 (S7 test drove the dead arm).** `TestGEExchange_ServiceAccount` now has
  the fake validator return `IsServiceAccount: true, nil` (matching what the
  real validator actually does post-§4.2(i)) instead of the old
  `ErrGoogleServiceAccount` sentinel, and asserts the exact rejection message
  plus that no user/binding was created (resolver never reached).
- **R6 (I1 not golden; configured-trust invariant untested).**
  `TestExternalBearer_ConfiguredTrustInvariant_Golden` is a table-driven test
  with 4 cases — (a) no trust + non-Hub JWT, (b) trust configured + wrong-
  signature Hub JWT, (c) trust configured + non-Google-`iss` JWT, (d) trust
  configured + opaque token in the `default:` arm — each asserting
  `w.Body.Bytes()` byte-equal to an independently-reconstructed expected body
  (`wantErrorBody`, built from the same `ErrorResponse`/`APIError` shape
  `writeError` uses, fed with the real `ValidateUserToken` error text rather
  than a hand-typed literal) and validator-call-count 0. Reconstructing
  independently rather than hardcoding go-jose's exact error string still
  catches the mutation class the reviewer found (M7: a stray `details` map;
  M8: leaking `err.Error()` into a message that must stay fixed), because the
  expected bytes are computed via a separate code path from the one under
  test.
- **R7 (401/403 bodies unasserted).** Added exact-body assertions (via
  `wantErrorBody`) to `TestExternalBearer_UnverifiedEmail_Unauthorized` (U2),
  the new R4 SA test, a new `TestExternalBearer_WrongAudience_Unauthorized`,
  and `TestExternalBearer_NonAuthoritativeEmail_Forbidden` (U3).
- **R8 (bare issue-number reference).** Reworded to `GoogleCloudPlatform/scion#1847` in this
  project log (the two remaining bare mentions) and rewrote the `d5717da37`
  commit message (history rewrite, approved by ap-em — see the new head SHA
  reported to ap-em).

### Optional

- **O1.** Added `TestExternalBearer_PATShapedToken_NeverTouchesGoogleValidator`
  and `TestExternalBearer_ValidAgentToken_NeverTouchesGoogleValidator`. Both
  test comments say explicitly that they cannot pin hook *ordering* — PATs and
  agent tokens are routed away before either hook site is even reached
  (`detectTokenType`'s prefix check for PATs; Step 1's unconditional early
  return for agent tokens) — matching the reviewer's judgement 1.
- **O2.** Added `TestExternalBearer_JWKSUpstreamFailure_ServiceUnavailable`:
  JWKS endpoint returns 500 with no cached keys → 503 `upstream_unavailable`.
- **O3.** Deleted `hasGoogleUserTrust` entirely. `server.go`'s `New` now always
  constructs the base validator and resolver (the validator does no network
  I/O until first use), so `googleTrust`, reading through
  `cfg.FederationAuth` on every request, is the single source of truth for
  whether the path is reachable — Google trust added later via hot reload
  takes effect without a restart. Tests:
  `TestExternalBearer_HotReload_TrustAddedWithoutRestart` (unit, via
  `UnifiedAuthMiddleware` directly: same `AuthConfig`, store into the same
  `atomic.Pointer` mid-test, second request succeeds) and
  `TestGEExchange_Route_GoogleStackBuiltWithoutExchangeOrTrust` (integration,
  via a real `New()` with neither Google trust nor the exchange configured).
- **O4.** Added `TestNoPackageLevelMutableState`: a `go/parser`-based AST check
  that `auth_external_bearer.go` and `google_identity_resolver.go` declare no
  package-level `var` other than `errors.New(...)` sentinels. Named so Phase
  2's `google_credential_cache.go` can be added to the file list directly.

### Nit

- **N1.** Fixed the `default:`-arm comment in `auth.go` — it referenced a
  Google-ID-token `"typ"` detection that doesn't exist; `detectTokenType`
  routes every 3-segment token to `tokenTypeUser`, so in Phase 1 this arm never
  sees a JWT at all. Reworded to say it's reserved for opaque access tokens
  (Phase 2).
- **N2.** Added `errExternalBearerPrincipalRejected` sentinel for the SA
  rejection in `authenticateExternalBearer`, replacing the ad-hoc
  `fmt.Errorf`. Still maps to 401 via the `default` arm of the
  `errors.Is`-driven switch in `serveExternalBearer`.

### FYI

- **F1 (recorded, no code change).** The exchange's status code for a *real*
  SA ID token changes from 403 to 401 pre-Phase-3. Before §4.2(i), the SA
  check ran before the `aud`/`azp` disagreement check in `ValidateIDToken`; a
  real SA ID token (`azp` = numeric SA unique ID ≠ `aud`) now fails
  `ErrGoogleFieldDisagreement` first, so the exchange returns 401 "credential
  metadata inconsistent" instead of 403 "service account credentials not
  accepted." Still fail-closed. Phase 3's §4.2(ii) SA audience rule makes these
  tokens validate and restores the 403 via the exchange's Step 1.5. Our test
  fakes (`TestGEExchange_ServiceAccount`, `TestExternalBearer_ServiceAccountIDToken_Rejected`)
  construct `IsServiceAccount: true` directly rather than a real SA claim
  shape, so they don't exercise this transition — Phase 3's S7/SA tests should
  use the real SA claim shape (`azp` = numeric ID, `aud` = configured
  audience) to catch it.
- **F2, F3.** No action (reviewer confirmed correct as-is).

### Dead code

- **D1.** Removed the unreachable `case errors.Is(err, ErrGoogleServiceAccount):`
  arm in `ge_exchange.go`'s `Exchange()`. Removed `ErrGoogleServiceAccount`
  itself — `grep -rn ErrGoogleServiceAccount` (repo-wide, including `extras/`)
  showed only the definition and the one test, both gone. Fixed the stale
  "Not a service account" line in `ValidateIDToken`'s doc comment.
- **D2.** Covered by R3 (constructor injection replaces `SetResolver`).

### Verification (fix round 1)

- `gofmt -l` clean on all touched files.
- `go build -buildvcs=false ./...` (whole repo) — clean.
- `go vet -buildvcs=false ./...` (whole repo) — clean.
- Targeted run of every Phase-1-relevant test (`TestExternalBearer_*`,
  `TestGoogleTrust_*`, `TestGEExchange_*`, `TestProductionValidator_*`,
  `TestNoTokenInfo*`, `TestNoPackageLevelMutableState`, and the resolver/store
  helper tests) — 123 tests, all green.
- `go test -buildvcs=false ./pkg/hub/ -run TestGEExchange` (includes the new
  route/wiring tests, `//go:build !no_sqlite`) — all green.
- Full `go test -buildvcs=false -timeout 40m ./pkg/hub/ ./pkg/hub/authzop/` —
  see the report to ap-em for the final pass/fail breakdown.

## Fix round 2 (review r2, verdict REQUEST CHANGES: 0 Critical, 4 Required, 4 Optional, 1 Nit, 3 FYI)

Reviewer: `ap-p1-rev-2`. Report: `/scion-volumes/scratchpad/projects/auth-passthrough/reviews/p1-r2-ap-p1-rev-2.md`.
EM dispositions: `/scion-volumes/scratchpad/projects/auth-passthrough/briefs/ap-p1-dev-fix-r2.md`.
All r1 findings were independently re-verified resolved by this reviewer (fresh mutation testing:
M5, M6, M7, M7b, M8, M10 and the R1 probe all killed). Every r2 finding resolved below except none
deferred — item 6 arrived mid-round (see below) and is included.

### Required

- **1 (production `roleFor` untested, M11 survived).** Both existing U5 tests injected a
  hand-written stub `roleFor` into `NewGoogleIdentityResolver` directly, proving only that the
  resolver *uses* `roleFor` — not that `server.go`'s actual closure
  (`func(ctx, email) string { return srv.getUserRole(ctx, email, "", "") }`) honours `admin_emails`
  in production. Added `TestGEExchange_Route_ProductionResolverHonoursAdminEmails`: builds a real
  server via `New()` with `cfg.AdminEmails = []string{"admin@gmail.com"}`, calls
  `srv.authConfig.GoogleResolver.Resolve` directly for both a listed and an unlisted Gmail address,
  and asserts `admin`/`member` respectively.
- **2 (`issuer_type: user` guard untested, M21 survived).** Added
  `newGoogleFederationAuthWithIssuerType` (sibling to `newGoogleTrustFederationAuth`, configurable
  issuer type) and `TestGoogleTrust_RequiresIssuerTypeUser` (table-driven over
  `user`/`service_account`/`hub`). Added the middleware-level case,
  `TestExternalBearer_ServiceAccountFederationIssuer_NotApplicable`: a validly-signed Google user ID
  token, with Google trusted only as `issuer_type: service_account`, falls through to the original
  401 with zero validator calls.
- **3 (three remaining bare issue-number references).** Two were in test comments
  (`auth_external_bearer_test.go:528,1145` at the time of review): the `:528` comment lived inside
  the now-deleted `_Golden401` test (see finding 9) and went with it; the `:1145` comment (the O4
  section header) reworded to `GoogleCloudPlatform/scion#1847`. The third was the `58544e50e` commit
  message itself — reworded via the same rebase that carries this round's commits (approved,
  `--force-with-lease` as last round). Verified with the two commands the disposition specified
  (`git log ... | grep -nE ...` and `git diff ... | grep -nE ...`) — see the note below on that
  second command's behavior.
- **4 (golangci-lint errcheck).** `auth_external_bearer_test.go`'s
  `TestNoTokenInfoOutsideGoogleCredentialValidator` had a bare `defer f.Close()`. Changed to
  `defer func() { _ = f.Close() }()`. `GOGC=40 golangci-lint run --new-from-rev=ca486fa
  --concurrency=1 ./pkg/hub/...` now reports 0 issues.

**Note on the R8 verification command.** The disposition's second command
(`git diff ca486fa..HEAD | grep -nE '^\+.*(^|[^/A-Za-z0-9])#[0-9]{3,}'`) has a quirk in GNU grep:
the `^` inside the alternation, not being at the true start of the overall pattern, does not act as
a real "start of line" anchor once preceded by `^\+.*` — empirically it degrades the alternation to
"always satisfiable", so the command flags every `#NNN` occurrence on a `+` line regardless of the
preceding character, including fully-qualified `GoogleCloudPlatform/scion#1847` references. I
verified this with a minimal repro (`printf` into the same `grep -nE` invocation) before concluding
the extra hits were false positives, and cross-checked with a plain `grep -rn "#1847"
pkg/hub/*.go .design/project-log/*.md | grep -v "scion#1847"` (no qualifier stripped), which found
nothing after the fixes above. Flagging this rather than silently working around it, in case the EM
wants the command itself corrected for future rounds.

### Optional

- **5 (R6 golden oracle recomputed at HEAD, not pinned to `ca486fa`).** Disposition: deviation
  accepted (recorded here) — go-jose error-string wording is library-version-dependent, and the
  reviewer's own 22-case replay against `ca486fa` proved byte-identity. Did the requested cheap
  hardening: golden case (d), plus two new cases with no library-dependent text (an empty
  `Authorization` header, and a `scion_pat_`-shaped token with no `UATSvc` configured), now assert
  against true hardcoded byte literals instead of `wantErrorBody`'s live reconstruction. Cases (a),
  (b), (c) keep the live-reconstruction approach, since their text does depend on go-jose's wording.
- **6 (internal resolver errors → 401) — arrived mid-round, un-held.** EM decision: validator/
  principal-policy errors stay 401; a `Resolve` error wrapping `store.ErrNotFound` (the bound user's
  record is gone) → 403 `forbidden`, logged at Warn; any other `Resolve` error → 503 `store_error`,
  logged at Error (matching the Hub-JWT path's store-fault handling, `auth.go`'s `UserStore.GetUser`
  check) — `errors.Is` only, `%w` wrapping preserved end to end. Implemented via a new
  `classifyResolveError` helper and `errExternalBearerInternalError` sentinel (wraps the original
  error with a second `%w`, so both the sentinel and the original chain remain `errors.Is`-matchable
  and the original error is still loggable). Tests: `TestExternalBearer_ResolveErrNotFound_Forbidden`
  and `TestExternalBearer_ResolveInternalError_ServiceUnavailable`, both using a `stubUserStore` and
  a pre-seeded orphan binding, asserting exact bodies.
- **7 (bare `accounts.google.com` untested, M22 survived).** Added
  `TestExternalBearer_ClassifyBareGoogleIssuer_IDToken` (classifier unit test) and
  `TestExternalBearer_BareGoogleIssuer_Authenticates` (U1 end-to-end variant with
  `claims["iss"] = "accounts.google.com"`).
- **8 (I1 production shape only indirectly covered).** Added
  `TestExternalBearer_NoTrustProductionShape_Golden401`: `GoogleValidator`/`GoogleResolver` non-nil
  (matching O3's always-built production shape) but `FederationAuth` empty, with a real
  Google-signed token for the configured audience — asserts the original 401 prefix and validator
  calls = 0, so the `googleTrust` gate (not the nil-guard that only golden case (a) exercised) is
  what's actually proven.
- **9 (over-claiming `_Golden401` name).** Deleted `TestExternalBearer_NoGoogleTrustConfigured_Golden401`
  — golden case (a) in `TestExternalBearer_ConfiguredTrustInvariant_Golden` supersedes it with an
  exact byte comparison instead of a prefix check.

### FYI

- **F1–F3.** No action (reviewer confirmed correct as-is; F1 re-verified resolver-extraction
  fidelity, F2 confirmed `Co-authored-by` placement, F3 was a security pass with no findings).

### Verification (fix round 2)

- `gofmt -l` clean.
- `go build -buildvcs=false ./...` (whole repo) — clean.
- `go vet -buildvcs=false ./pkg/hub/...` — clean.
- `GOGC=40 golangci-lint run --new-from-rev=ca486fa --concurrency=1 ./pkg/hub/...` — 0 issues.
- Targeted run of every Phase-1-relevant test — 130+ tests, all green.
- `go test -buildvcs=false ./pkg/hub/ -run TestGEExchange_Route` (includes the new item 1/2 wiring
  and middleware tests) — all green.
- Full `go test -buildvcs=false -timeout 40m ./pkg/hub/ ./pkg/hub/authzop/` — see the report to
  ap-em for the final pass/fail breakdown.
