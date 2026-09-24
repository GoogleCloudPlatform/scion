# auth-passthrough Phase 2 (`bb32049f..64516881`): user access tokens + caching + rate limit

**Developer:** ap-p2-dev
**Spec:** `impl-design.md` §4.2(iii), §4.4, §5 Phase 2, §6 rows C1-C6 (+ the access-token half of U2)
**Prior phase:** Phase 1 (`.design/project-log/auth-passthrough-p1-dev.md`), approved after four review
rounds (`reviews/p1-r1..r4-*.md`).

## Commits

1. `bb32049f` — carry-over fixes from Phase 1 review r4 (Optional 1, Nit 2).
2. `64516881` — Phase 2 proper.

## Carry-over from Phase 1 review r4

- **Optional 1**: `trackingUserStore` (`auth_external_bearer_test.go`) now overrides `CreateUser` to
  record the call (`createUserCalled`) and return an error, instead of panicking through the nil
  `store.UserStore` embed. `TestExternalBearer_GetExternalIdentityFault_ServiceUnavailable` and
  `TestGEExchange_ExternalIdentityLookupFault_ServerError` now fail at their own assertions if the F2
  guard regresses, rather than aborting the test binary via SIGSEGV. Also tightened the exchange
  test's status check from `< 500` to `== http.StatusInternalServerError`.
- **Nit 2**: reworded the r3 verification note in this log's Phase 1 entry (`:469-471`), which had
  misstated that a full `pkg/hub` run was not required that round (it was, and it was run).

## Phase 2 implementation

### `google_credential_cache.go` — caching decorator (§4.2(iii))

`NewCachingGoogleCredentialValidator(v GoogleCredentialValidator, opts ...CacheOption) GoogleCredentialValidator`
wraps a base validator with:

- **Key**: `sha256(token) || sha256(sorted(allowedClientIDs))` (each half hashed separately, joined
  with a bare `.`, so the two halves can never collide into each other).
- **TTL**: `min(maxTTL, identity.UpstreamExpiry - now)` for a positive result.
- **Negative cache**: only for `ErrGoogleInvalidCredential`, `ErrGoogleExpiredCredential`,
  `ErrGoogleUntrustedAudience`, `ErrGoogleUnverifiedEmail`. Everything else — including
  `ErrGoogleUpstreamError` and every error not on that list (e.g. `ErrGoogleFieldDisagreement`,
  `ErrGENotConfigured`) — is never cached, so the next request always retries upstream (C4). This is
  the conservative reading of "only for the four listed errors."
- **Defaults**: `maxTTL = 5m`, `negTTL = 30s`, `maxEntries = 10000`, all overridable via `CacheOption`.
- **Eviction**: `store()` evicts expired entries once when the map is at `maxEntries`, then refuses
  the insert if still full. No LRU, so a live entry is never evicted to make room — matches upstream
  PR 1847's policy per the design doc.
- **Singleflight**: `golang.org/x/sync/singleflight` (already a repo dependency) collapses concurrent
  first requests for the same key into one upstream call. A cache re-check inside the singleflight
  callback avoids a redundant upstream call in the narrow race where a different call for the same
  key just finished as this one starts.
- **`Cached(token, allowedClientIDs) bool`**: exposed so the rate limiter can skip cache hits (see
  below). Not part of the `GoogleCredentialValidator` interface — `authenticateExternalBearer`
  type-asserts for it, so every existing test fake (which doesn't implement it) is simply always
  treated as a miss.
- **No package globals**: everything lives on the `*cachingGoogleCredentialValidator` instance.

### `external_bearer_ratelimit.go` — per-IP rate limiter (§4.4)

`externalBearerRateLimiter` reuses `geExchangeRateLimiter`'s bucket algorithm and
`geExchangeClientIP`'s trusted-proxy-aware client-IP extraction (`ge_exchange_ratelimit.go`) rather
than re-implementing either — the two limiters differ only in rate/burst (5 rps / burst 20 here,
sized for arbitrary per-request callers rather than a handful of bridge replicas). `Allow(r
*http.Request) (bool, int)` mirrors `geExchangeRateLimiter.Allow`'s shape. Trusted proxies are parsed
once at construction (`newExternalBearerRateLimiter(cfg.TrustedProxies)`), matching how
`UnifiedAuthMiddleware` itself resolves `cfg.TrustedProxies` at middleware-construction time, not
per request — trusted-proxy config was already not part of the hot-reload surface before this phase.

### `auth_external_bearer.go` — classifier, access-token branch, rate-limit gate

- `classifyExternalBearer` now classifies **every** non-JWT token as `externalBearerAccessToken`
  (previously `externalBearerNotApplicable` in Phase 1). This matches the design's "classification
  without prefix sniff": the classifier itself doesn't know whether Google trust is configured.
  What keeps **I1** intact is call order — `authenticateExternalBearer` only reaches the classifier
  after `googleTrust(cfg)` has already confirmed trust is configured. Absent trust, classify is never
  called, and the request falls through to the original rejection byte-identical, with zero validator
  calls, exactly as in Phase 1. Pinned by
  `TestExternalBearer_ConfiguredTrustInvariant_Golden`'s case (d), now `d_no_trust_opaque_token_default_arm`
  (previously `d_trust_configured_opaque_token_default_arm`, which encoded Phase 1's behaviour and had
  to change — see Deviations below), and by
  `TestExternalBearer_TrustConfiguredOpaqueToken_AttemptsAccessTokenValidation` for the complementary
  "trust configured" case.
- `authenticateExternalBearer` gained an `r *http.Request` parameter (for the rate limiter's IP
  extraction), validates access tokens via `cfg.GoogleValidator.ValidateAccessToken`, and rejects a
  service-account identity (`id.IsServiceAccount`) on **either** token kind — the existing Phase 1
  check already generalized once the switch covers both kinds, so no separate SA-access-token guard
  was needed, only a test proving it (`TestExternalBearer_AccessToken_ServiceAccount_Rejected`).
- The rate-limit gate: on every kind (ID token and access token both go through
  `googleTrust`/`classify` before this point), if `cfg.ExternalBearerLimiter != nil`, check
  `cfg.GoogleValidator.(externalBearerCacheProbe).Cached(token, aud)` first; only consult the limiter
  on a miss. `cfg.ExternalBearerLimiter == nil` (the default in every test that doesn't wire one)
  means unlimited — this is a production-wiring concern, not a correctness requirement for paths that
  don't set it, so no existing Phase 1 test needed to change for this.
- New sentinel `errExternalBearerRateLimited` plus a distinct `*externalBearerRateLimitError` type
  (carrying `retryAfterSeconds`) so `serveExternalBearer` can both `errors.Is` the sentinel and
  `errors.As` the concrete type to recover the `Retry-After` value.

### `server.go` wiring — the validator-sharing decision (design asked to be stated explicitly)

**Decision: the exchange endpoint does *not* go through the caching decorator.** `srv.authConfig.GoogleValidator`
(external-bearer path) is `NewCachingGoogleCredentialValidator(googleValidator)`; `NewGEExchangeService`
is still constructed with the raw `googleValidator`. Rationale:

- The design's explicit ask is "keep exchange behaviour unchanged." The exchange endpoint mints a
  short-lived (default 60s) Hub token per successful exchange rather than re-verifying the Google
  credential on every downstream call, so it doesn't have the per-request re-verification cost the
  cache exists to amortize, and introducing caching would be a latency/behaviour change with no
  stated benefit to that endpoint.
- The "one shared base validator + resolver with `GEExchangeService`" principle from Phase 1 is
  preserved at the *base* level: the decorator wraps the very same `googleValidator` instance the
  exchange uses, so a cache hit and a fresh exchange verification of the same credential still agree
  (same JWKS cache, same clock). Only the resolver needs to be the literally-identical top-level
  instance for the "identical decisions" property (§4.4) — that's still true (`srv.geExchangeService.resolver
  == srv.authConfig.GoogleResolver`) — and it's the resolver, not the validator, that does
  suspension/binding/provisioning, which has no cache either way.
- `TestGEExchange_Route_SharesValidatorAndResolverWithExternalBearer` is updated to assert this
  precisely: same resolver instance, and `srv.authConfig.GoogleValidator.(*cachingGoogleCredentialValidator).base
  == srv.geExchangeService.validator` (same base validator underneath, not same top-level value).

## Deviations from the existing test suite

- `TestExternalBearer_ClassifyNonJWT_NotApplicable` renamed to
  `TestExternalBearer_ClassifyNonJWT_AccessToken` and its assertion flipped, per the classifier change
  above (Phase 1's "not applicable" was the old contract; Phase 2's design explicitly supersedes it).
- `TestExternalBearer_ConfiguredTrustInvariant_Golden` case (d) changed from `withTrust: true` to
  `withTrust: false` (renamed accordingly). With trust configured, an opaque token is now a candidate
  access token by design, so the old assertion ("still not-applicable, 0 validator calls") is no
  longer the correct behaviour for that configuration — the invariant it was protecting (I1: **no**
  trust configured ⇒ byte-identical fallthrough) is preserved by moving the case to `withTrust: false`,
  and the "trust configured" access-token behaviour is now covered by dedicated Phase 2 tests instead.
- `TestGEExchange_Route_SharesValidatorAndResolverWithExternalBearer` updated per the wiring decision
  above.
- `TestNoPackageLevelMutableState`'s file list extended with `google_credential_cache.go` and
  `external_bearer_ratelimit.go` (I4), per the brief.

## New tests

- `google_credential_cache_test.go`: cache-key derivation (token/audience dependence, audience-order
  independence), TTL capped by `UpstreamExpiry` and by `maxTTL` (C3), the negative-cache error
  allowlist proven exhaustively both ways — the four listed errors are cached, `ErrGoogleUpstreamError`
  and five other non-listed errors are never cached (C4), negative-entry expiry, singleflight
  collapsing 50 concurrent misses into one upstream call (C6), `Cached()` hit/miss reporting, and
  evict-expired-then-refuse-insert at `maxEntries` (with a same-cache proof that a live entry is never
  evicted to make room).
- `auth_external_bearer_access_token_test.go`: C1 (valid azp / wrong azp, exact 401 body), the
  access-token half of U2 (unverified email), SA-via-access-token rejection, the Phase 2 classifier
  change end to end through the real middleware, C2 (repeated requests through the real middleware +
  real caching decorator + real validator against counting tokeninfo/userinfo test endpoints — one
  call each), C4 through the middleware (upstream 5xx → 503, and the retry on the next request proves
  it wasn't negatively cached), and C5 (rate-limited beyond burst with `Retry-After` + exact body;
  cache hits not rate-limited, proven with a burst of 1 and 10 repeated requests on the same token).

## Gates

- ✅ `go build -buildvcs=false ./...` (whole repo).
- ✅ `go vet -buildvcs=false ./pkg/hub/...`.
- ✅ `gofmt -l pkg/hub` — clean.
- ✅ `GOGC=40 golangci-lint run --new-from-rev=upstream-main --concurrency=1 ./pkg/hub/...` — 0 issues.
- ✅ Targeted mutation-relevant subset (`TestExternalBearer|TestGoogleTrust|TestGEExchange|TestProductionValidator|TestNoPackage|TestNoTokenInfo|TestMutationClassification|TestGoogleIdentityResolver|TestGoogleCredential|TestFederation`, `-count=1`): all green.
- ✅ Full `go test -timeout 40m ./pkg/hub/ ./pkg/hub/authzop/`: `authzop` ok; `pkg/hub` fails **only**
  the four known pre-existing tests named in the brief (`TestDEF164_AtAgentSlug_DeliversToAgent`,
  `TestDEF164_AtAgentSlug_DMConversationCreated`, `TestDEF152_AgentToAgentDM_DeliversViaOutbound`,
  `TestCreateTemplateV2_ScopeIDInjectionBlocked`). `TestDEF162_AC8_Broker_MentionFires` (documented
  flake) passed this run. A first attempt at this full run failed on host ENOSPC (shared disk filled
  to 100% mid-run, independently of this change) writing its testlog; per ap-em's live disk-pressure
  guidance this was treated as an environment failure, not a test result, and the run was retried
  cleanly once space was available. Per ap-em's subsequent rule (one full `pkg/hub` run per phase,
  ask first), this was the one full run for Phase 2.
- ✅ Bare-`#NNN` checks against `upstream-main` (`git fetch https://github.com/GoogleCloudPlatform/scion.git main:upstream-main`):
  both the commit-message and diff greps print nothing.
- Not run: repo-wide golangci-lint (scoped to `pkg/hub/...` per prior-round OOM guidance); web
  build/typecheck (no web changes in this phase).

## §6 acceptance rows → tests

| Row | Test(s) |
|---|---|
| C1: valid azp → 200; wrong azp → 401 (exact body) | `TestExternalBearer_AccessToken_ValidAzp_Authenticates`, `TestExternalBearer_AccessToken_WrongAzp_Unauthorized` |
| U2 (access-token half): `email_verified=false` → 401 | `TestExternalBearer_AccessToken_UnverifiedEmail_Unauthorized` |
| C2: N requests, same token, within TTL → 1 tokeninfo + 1 userinfo call | `TestExternalBearer_AccessToken_RepeatedRequests_OneUpstreamRoundTrip` (middleware); `TestGoogleCredentialCache_HitAvoidsUpstreamCall`, `TestGoogleCredentialCache_DifferentTokensDoNotShareEntry` (decorator) |
| C3: cache entry never outlives `UpstreamExpiry` | `TestGoogleCredentialCache_TTLCappedByUpstreamExpiry`, `TestGoogleCredentialCache_MaxTTLCapsLongLivedCredential` |
| C4: upstream 5xx → 503, not negatively cached | `TestExternalBearer_AccessToken_UpstreamFault_ServiceUnavailableNotCached` (middleware); `TestGoogleCredentialCache_UpstreamErrorNeverCached`, `TestGoogleCredentialCache_NegativeCacheOnlyForFourListedErrors` (decorator) |
| C5: rate-limited beyond burst → 429 + `Retry-After`; cache hits not rate-limited | `TestExternalBearer_AccessToken_RateLimitedBeyondBurst`, `TestExternalBearer_AccessToken_CacheHitsNotRateLimited` |
| C6: 50 concurrent first requests → 1 upstream call | `TestGoogleCredentialCache_SingleflightCollapsesConcurrentMisses` |
| I1/§3: no trust + opaque → original bytes, 0 validator calls | `TestExternalBearer_ConfiguredTrustInvariant_Golden/d_no_trust_opaque_token_default_arm` |
| I1/§3: Hub JWT/PAT/agent never reach the validator | `TestExternalBearer_ValidHubJWT_NeverTouchesGoogleValidator`, `TestExternalBearer_PATShapedToken_NeverTouchesGoogleValidator`, `TestExternalBearer_ValidAgentToken_NeverTouchesGoogleValidator` (unchanged from Phase 1, still green) |
| I3: tokeninfo grep only in `google_credential_validator.go` | `TestNoTokenInfoOutsideGoogleCredentialValidator` |
| I4: no package-level mutable state, including the two new files | `TestNoPackageLevelMutableState` (file list extended) |
| Phase 1 + `TestGEExchange*` stay green | full targeted run + full `pkg/hub` run, both green apart from the four known pre-existing failures |
| SA identity still not admitted via an access token | `TestExternalBearer_AccessToken_ServiceAccount_Rejected` |

## Out of scope (per brief)

SA ID-token rule, `allowed_projects`, `allowed_domains`, docs, bridge, metrics — all explicitly Phase
3/4/5/later, untouched here.

## Design questions / decisions made without waiting

- **Exchange vs. decorator sharing**: decided as documented above (exchange stays on the raw
  validator; only the external-bearer path is decorated) and stated here per the brief's explicit
  ask, rather than raised as a blocking question — the design text already gave enough to decide
  ("keep exchange behaviour unchanged" + "shares the base validator") without ambiguity.
- No other design ambiguities were hit this phase.

## Fix round 1 (review `p2-r1-ap-p2-rev.md`: REQUEST CHANGES, 1 Critical, 4 Required, 1 Optional, 3 Nit, 2 FYI)

Fix brief: `briefs/ap-p2-dev-fix-r1.md`. All Critical/Required findings, both actionable Nits, and the
Optional finding are fixed; Nit 8 (commit message wording) is left for `ap-em`'s Phase 3 rebase per
the brief (no history rewrite on the shared branch); the two FYIs need no action.

**1 (Critical). External-bearer limiter never ran its cleanup — permanent lockout after `maxEntries`
distinct client IPs.** `server.go` now keeps the limiter on `Server` (`srv.externalBearerRateLimiter`,
also assigned to `authConfig.ExternalBearerLimiter`) and starts its cleanup in
`StartBackgroundServices`, next to `geExchangeRateLimiter.StartCleanup(ctx)`. `externalBearerRateLimiter`
gained a `StartCleanup` method delegating to its embedded `geExchangeRateLimiter`. `geExchangeRateLimiter`
itself gained a `cleanupInterval` field (defaulted to the existing `geExchangeCleanupInterval` constant
in `newGEExchangeRateLimiter`, used by `StartCleanup`'s ticker) so a test can shrink it and observe the
background goroutine actually running, instead of waiting on the production 5-minute interval — this is
a same-package, backward-compatible addition (default behaviour unchanged), not a behaviour change to
the exchange endpoint's own limiter.
- Tests: `TestGEExchange_Route_SharesValidatorAndResolverWithExternalBearer` extended to assert
  `authConfig.ExternalBearerLimiter` and `srv.externalBearerRateLimiter` are both non-nil and identical
  (deleting either wiring line fails this test). `TestExternalBearerRateLimiter_CleanupAdmitsNewIPAfterMaxAge`
  proves `Cleanup(now+maxAge+ε)` frees capacity for a new IP (probe (b)). `TestServer_ExternalBearerRateLimiter_CleanupRunsInBackground`
  proves `StartBackgroundServices` actually invokes `externalBearerRateLimiter.StartCleanup` — not just
  that the field is set — by shrinking `cleanupInterval`/`maxEntries`/`maxAge` and polling for the
  background goroutine to evict a stale entry (probe (a)'s "cleanup is started" half).

**2 (Required). Tokeninfo/userinfo 400 → 503 `upstream_unavailable` instead of 401, never negatively
cached; a failed JWKS `forceRefresh` → `ErrGoogleInvalidCredential` instead of `ErrGoogleUpstreamError`.**
`google_credential_validator.go`: `getTokenInfo`/`getUserInfo` now classify their own failures instead of
leaving it to the caller — a 400/401 (`isGoogleClientErrorStatus`) or a 200 body carrying an `error`
field is `ErrGoogleInvalidCredential`; a network error, decode failure, or any other non-200 (5xx, or an
unexpected status) is `ErrGoogleUpstreamError`. `ValidateAccessToken` no longer re-wraps their result as
`ErrGoogleUpstreamError` unconditionally (`fmt.Errorf("tokeninfo call failed: %w", err)` preserves
whichever sentinel the helper already chose). The `forceRefresh` failure branch in `ValidateIDToken` now
wraps `ErrGoogleUpstreamError`, not `ErrGoogleInvalidCredential` — a failed refresh is an upstream fault,
not a signature verdict.
- **Exchange behaviour is unchanged externally**: `ge_exchange.go`'s status-mapping switch has no case
  for either sentinel, so both still fall through to the same `default:` 401 "credential validation
  failed" — only the internal classification, and therefore the *log* text (`credential_type=... error=...`),
  changes. Corrected in fix round 2 (review r2 required finding 1): the existing `TestGEExchange_*` suite
  passing unchanged is **not** proof of this, because every one of those tests uses `fakeGoogleValidator`
  and never runs `getTokenInfo`/`getUserInfo`/`forceRefresh` — the exact functions this fix changed. The
  real proof is `TestGEExchange_RealValidator_UpstreamFailures_ExactBytes` (tokeninfo 400/5xx, userinfo
  401/5xx) and `TestGEExchange_RealValidator_IDTokenForceRefreshFailure_ExactBytes` (ID-token
  force-refresh 5xx), both added in fix round 2, which drive the real validator against stubbed Google
  endpoints and assert the exchange's exact response bytes.
- Tests against the real validator: `TestProductionValidator_AccessToken_TokenInfo400_InvalidCredential`,
  `_TokenInfo5xx_UpstreamError`, `_UserInfo400_InvalidCredential` (classification, both directions, both
  endpoints), `TestProductionValidator_IDToken_ForceRefreshFailure_UpstreamError` (the mislabel fix,
  using the same `fetchedAt` back-dating technique as the existing `JWKSForceRefresh` test to defeat
  `forceRefresh`'s own 30s throttle). Through the real middleware:
  `TestExternalBearer_TrustConfiguredOpaqueToken_AttemptsAccessTokenValidation` now uses a real
  tokeninfo-400 stub instead of a fake configured with an error the real validator could never produce
  for this input — review r1 finding 2 called out that the fake was hiding exactly this bug.
  `TestExternalBearer_AccessToken_TokenInfo400_NegativelyCached` proves the negative-cache half: two
  requests with the same invalid token cost one tokeninfo call.

**3 (Required). The I1 no-trust golden cases left `GoogleValidator`/`GoogleResolver` nil, masking a
mutation that skips the trust check for non-JWT tokens.** `TestExternalBearer_ConfiguredTrustInvariant_Golden`
now always wires `GoogleValidator`/`GoogleResolver` (matching production shape, O3); only
`FederationAuth` varies with `tt.withTrust`. Verified by hand: mutating
`if !ok { return nil, errExternalBearerNotApplicable }` to `if !ok && looksLikeJWT(token) {` in
`authenticateExternalBearer` now fails case (d) (`counting.totalCalls() != 0`), where before the fix it
passed the whole targeted suite silently. Mutation reverted after confirming the kill.

**4 (Required). Singleflight ran the shared upstream call under the leader's own request context, so
the leader cancelling its own request failed every concurrent follower too.** `google_credential_cache.go`'s
`validate` now calls `upstream` with `context.WithTimeout(context.WithoutCancel(ctx), upstreamCallTimeout)`
(10s, matching `NewGoogleCredentialValidator`'s default `http.Client` timeout) built from whichever
caller happens to be the singleflight leader, instead of that caller's own `ctx` directly. `ValidateIDToken`/
`ValidateAccessToken`'s closures now take the detached `upstreamCtx` as a parameter rather than closing
over the outer `ctx`.
- Test: `TestGoogleCredentialCache_LeaderCancellationDoesNotPoisonFollowers` — a `blockingValidator` puts
  a singleflight leader mid-flight, the test cancels the leader's own context, then starts a follower on
  a live context; both succeed, the base validator is called exactly once, and the result is cached
  positively (not left uncached or negatively cached as a side effect of the leader's cancellation).

**5 (Required). A bare hash-prefixed issue reference in the Phase 2 log (the caching-decorator eviction
bullet), plus a since-falsified "clean" claim about it.** Reworded to spell out "upstream PR 1847"
without the leading `#`. Both brief greps re-run after the final commit of this round (paste in the
report to `ap-em`); this section is itself written to avoid reintroducing the pattern.

**6 (Nit). Stale `default:`-arm comment in `auth.go` describing Phase 1 behaviour** (claimed
`serveExternalBearer` always returns `false` there). Reworded to describe the Phase 2 access-token hook
site.

**7 (Nit). Mixed clocks in the cache's TTL computation** — `store()` used `time.Until(identity.UpstreamExpiry)`
(the real wall clock) while `expiresAt` used `c.now()` (the injectable one). Changed to
`identity.UpstreamExpiry.Sub(c.now())`. The two TTL tests
(`TestGoogleCredentialCache_TTLCappedByUpstreamExpiry`, `_MaxTTLCapsLongLivedCredential`) now use a fake
clock 50 years from the real one (all the cache tests' fake-clock setups were moved to the same offset,
for consistency), so a regression back to the real clock fails loudly instead of silently passing
because the fake clock happened to start near the real one. Fixing this surfaced a latent bug in
`TestGoogleCredentialCache_EvictsExpiredBeforeRefusing`, which reused one `countingBaseValidator` with a
single fixed `UpstreamExpiry` for both `token-1` and `token-2` — under the old mixed-clock bug this
accidentally passed regardless of the fake clock, because `time.Until` used the real, barely-elapsed
wall-clock time. Fixed by adding a small `perTokenExpiryValidator` so `token-2` gets its own long-lived
expiry, isolating the property actually under test (eviction of the stale entry makes room).

**9 (Optional). `Retry-After` was asserted non-empty, not exact.** `TestExternalBearer_AccessToken_RateLimitedBeyondBurst`
now asserts the literal `"1"` (burst 3, default 5 rps: `ceil(1/5) = 1`).

**8 (Nit, no action by me).** Commit message wording on `645168819` — left for `ap-em`'s Phase 3 rebase,
per the brief.

**10, 11 (FYI, no action).**

### Gates (fix round 1)

- ✅ `go build -buildvcs=false ./...`
- ✅ `go vet -buildvcs=false ./pkg/hub/...`
- ✅ `gofmt -l pkg/hub` — clean
- ✅ `GOGC=40 golangci-lint run --new-from-rev=upstream-main --concurrency=1 ./pkg/hub/...` — 0 issues
- ✅ Targeted subset (`TestExternalBearer|TestGoogleTrust|TestGEExchange|TestProductionValidator|TestNoPackage|TestNoTokenInfo|TestGoogleIdentityResolver|TestGoogleCredential|TestFederation`, `-count=1`) — green
- ✅ `-race` targeted subset (`TestExternalBearer|TestGoogleCredentialCache|TestNoPackageLevelMutableState|TestNoTokenInfoOutside|TestGEExchange|TestExternalBearerRateLimiter|TestServer_ExternalBearerRateLimiter`, `-count=1`) — green, no data races (23.8s)
- ✅ Full `go test -timeout 40m ./pkg/hub/ ./pkg/hub/authzop/` at `882e13d4` (run after `ap-em`'s
  go-ahead, own `GOTMPDIR`, deleted after the run): `authzop` ok; `pkg/hub` fails **only** the same four
  known pre-existing tests as every prior round (`TestDEF164_AtAgentSlug_DeliversToAgent`,
  `TestDEF164_AtAgentSlug_DMConversationCreated`, `TestDEF152_AgentToAgentDM_DeliversViaOutbound`,
  `TestCreateTemplateV2_ScopeIDInjectionBlocked`). No ENOSPC this run.
- ✅ Bare-issue-reference greps against `upstream-main`, re-run at the final commit (`882e13d4`): both
  print nothing (exit 1 = no match). This required squashing the fix-round commit locally before
  pushing — the first version of the commit message itself quoted the offending reference verbatim
  while describing the fix, which retripped the same grep; see the commit history note below.

**Note on local history:** the fix-round commit was originally split into two local, not-yet-pushed
commits, the second of which corrected wording in the first. Before pushing, both were squashed into
one clean commit (`git reset --soft` to the prior pushed commit, then a single re-commit) so the grep
checks pass against the pushed history, consistent with how Phase 1's r4 review notes the same
technique was used for the same reason. No already-pushed commit was rewritten.

## Fix round 2 (review `p2-r2-ap-p2-rev-2.md`: REQUEST CHANGES, 0 Critical, 1 Required, 3 Optional, 0 Nit, 3 FYI)

Reviewed at `882e13d4` plus the docs-only `1abe1dc2` log diff. All r1 fixes confirmed resolved (see the
review's own r1 disposition table); this round's finding is a test gap, plus three Optionals, each
given a resolve-or-decline below.

**1 (Required, resolved). The exchange byte-identity regression test the r1 brief required was not
delivered; the project log's "the existing `TestGEExchange_*` suite proves it" claim was false**, since
every `TestGEExchange_*` test uses `fakeGoogleValidator` and none of them exercises
`getTokenInfo`/`getUserInfo`/`forceRefresh` — the exact functions the r1 fix changed.
**Fix:** added `TestGEExchange_RealValidator_UpstreamFailures_ExactBytes` (table test: tokeninfo 400,
tokeninfo 5xx, userinfo 401, userinfo 5xx — real validator via `newTestValidator`, `handleGEGoogleExchange`
driven directly) and `TestGEExchange_RealValidator_IDTokenForceRefreshFailure_ExactBytes` (the fifth
minimum case: ID-token JWKS force-refresh 5xx, using the existing `fetchedAt` back-dating technique).
Both assert the exact 401 body `{"error":{"code":"invalid_credential","message":"credential validation
failed"}}` — all five pass, confirming the property the reviewer's own (deleted) probe found: the
exchange is byte-identical across these upstream-failure shapes. Corrected the log sentence at the fix
round 1 section above to point to these tests instead of the general suite.

**2 (Optional, resolved). The tokeninfo body-`error_description` classification branch was untested and
effectively dead in the one test that looked like it covered it** (that test uses HTTP 400, which is
rejected on status alone before the body is ever parsed; the map key it sets, `"error"`, doesn't even
match the struct's `error_description` json tag). **Fix:** added
`TestProductionValidator_AccessToken_TokenInfo200WithErrorBody_InvalidCredential`: a 200 response body
`{"error_description":"Invalid Value"}` asserts `ErrGoogleInvalidCredential`, not `ErrGoogleUpstreamError`.
Mutating that branch's sentinel now fails this test.

**3 (Optional, resolved). `countingGoogleValidator{}`'s zero value returns `(nil, nil)`, so a full-trust-
skip mutation panics the test binary (nil-pointer dereference on `id.IsServiceAccount`) instead of
failing an assertion** — the same class of problem P1 r4 optional finding 1 fixed for `trackingUserStore`.
Reproduced by hand: mutating `authenticateExternalBearer`'s `if !ok {` to `if false && !ok {` panicked in
both `TestExternalBearer_NoTrustProductionShape_Golden401` and the golden table's case (d), aborting the
binary before the rest of the suite ran. **Fix:** every `&countingGoogleValidator{}` zero-value
construction in `auth_external_bearer_test.go` (7 call sites: the golden table, `NoTrustProductionShape_Golden401`,
`ValidHubJWT_NeverTouchesGoogleValidator`, `ServiceAccountFederationIssuer_NotApplicable`,
`EmptyExpectedAudience_NotApplicable`, `PATShapedToken_NeverTouchesGoogleValidator`,
`ValidAgentToken_NeverTouchesGoogleValidator`) now defaults both `idTokenErr`/`accessTokenErr` to
`ErrGoogleInvalidCredential`. Re-verified: the same mutation now fails cleanly at
`counting.totalCalls() != 0` in all four previously-panicking cases, with no panic and the rest of the
suite still running (145 test lines emitted vs. an abort after ~9 previously). Reverted after confirming.

**4 (Optional, resolved). The design's limiter defaults (5 rps / burst 20, §4.4) were unpinned** — both
C5 tests override `rate`/`burst` to small test values, so widening the production constants would survive
the whole suite. **Fix:** added `TestExternalBearerRateLimiter_DefaultsMatchDesign` (asserts the
constructed limiter's `rate`/`burst` fields equal both the named constants and their literal design
values) and `TestExternalBearerRateLimiter_DefaultBurstExhaustion` (20 unmodified-default requests from
one IP succeed, the 21st is refused).

**5, 6, 7 (FYI, no action).** Singleflight's detached-context trade-off (bounded by the 10s timeout,
intended per r1 finding 4), the `StartBackgroundServices`-based cleanup test's seam choice, and the
full-run relay accuracy — all noted as fine as-is.

### Gates (fix round 2)

- ✅ `go build -buildvcs=false ./...`
- ✅ `go vet -buildvcs=false ./pkg/hub/...`
- ✅ `gofmt -l pkg/hub` — clean
- ✅ `GOGC=40 golangci-lint run --new-from-rev=upstream-main --concurrency=1 ./pkg/hub/...` — 0 issues
- ✅ Targeted subset (`TestExternalBearer|TestGoogleTrust|TestGEExchange|TestProductionValidator|TestNoPackage|TestNoTokenInfo|TestGoogleIdentityResolver|TestGoogleCredential|TestFederation`, `-count=1`) — green
- ✅ `-race` subset matching the reviewer's own gate (`TestExternalBearer|TestGoogleTrust|TestGEExchange|TestProductionValidator|TestNoPackage|TestNoTokenInfo|TestGoogleIdentityResolver|TestGoogleCredential|TestFederation|TestServer_ExternalBearerRateLimiter|TestMutationClassification|TestExternalBearerRateLimiter`, `-count=1`) — green, no data races (27.2s)
- Full `pkg/hub` suite: not re-run this round (test-only changes; `ap-em`'s last relayed run at `882e13d4`
  stands, per the disk rules' "one full run per phase, ask first" — this round doesn't touch
  non-test production files, so no new full run was requested).
