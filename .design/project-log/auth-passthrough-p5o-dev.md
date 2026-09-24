# auth-passthrough Phase 5 observability (`577419d51..e9b9c22ad`)

**Developer:** ap-p5o-dev
**Spec:** `impl-design.md` §4.7, §5 Phase 5 (observability half), §6 row O1; lead ruling of 2026-09-24 13:30
amending §4.7 (appended to the brief) — `outcome=store_error` distinct from `upstream_error`, a `Resolve` error
wrapping `store.ErrNotFound` maps to `forbidden`, outcomes map 1:1 to the §4.4 status table, `principal=unknown`/
`kind=unknown` before those are known, closed label sets defined as constants and tested.
**Prior phases:** Phase 1-4 project logs in this directory; read `auth_external_bearer.go`,
`google_credential_cache.go`, `external_bearer_ratelimit.go`, `ge_exchange.go`, `metrics.go`, `otel_metrics.go`
(and `gcp_metrics.go`/`otel_gcp_metrics.go`, the closest existing precedent for a paired dependency-free +
OTel-backed recorder) before starting.

## Commits

1. `a215153f8` — feature: the three counters, wired into `AuthConfig`/`Server`, and into
   `cmd/server_foreground.go`'s existing OTel-export block.
2. `36f0ba363` — tests: one per external-bearer outcome, cache result, and exchange outcome, plus the closed-set
   regression guard.
3. `e9b9c22ad` — test: Phase 4's `allowed_domains` rejection landed on the branch mid-task (rebase pulled it in);
   added the one test its rejection path was missing from the counter's coverage.

## What changed

- **New `pkg/hub/external_bearer_metrics.go`** — label types and constants (`ExternalBearerKind`,
  `ExternalBearerPrincipal`, `ExternalBearerOutcome`, `GoogleValidatorCacheResult`, `GEExchangeOutcome`) and
  three recorder interfaces (`ExternalBearerMetricsRecorder`, `GoogleValidatorCacheMetricsRecorder`,
  `GEExchangeMetricsRecorder`). No package-level `var` at all (only consts/types/interfaces), so it's added to
  `TestNoPackageLevelMutableState`'s file list cleanly.
- **New `pkg/hub/otel_external_bearer_metrics.go`** — `OTelExternalBearerMetrics`, one struct implementing all
  three interfaces via three `metric.Int64Counter`s registered under the existing `instrumentationScope`,
  mirroring `OTelMetricsRecorder`/`OTelGCPTokenMetrics`'s construction/registration/error-wrapping shape exactly.
  Instrument names (`scion.hub.external_bearer`, `scion.hub.google_validator_cache`,
  `scion.hub.ge_exchange.requests`) are dot/underscore-segmented so OTel's Prometheus bridge (lower-cases,
  joins on `_`, appends `_total` to a monotonic sum) produces exactly `scion_hub_external_bearer_total`,
  `scion_hub_google_validator_cache_total`, `scion_hub_ge_exchange_requests_total`. Not added to
  `TestNoPackageLevelMutableState` — see Deviations #1.
- **`pkg/hub/auth_external_bearer.go`** — `authenticateExternalBearer` now returns a third value,
  `externalBearerAttempt{kind, principal}`, threaded through every return point and updated the moment each
  becomes known (kind right after classification succeeds; principal right after validation returns
  `IsServiceAccount`). `serveExternalBearer`'s existing outcome `switch` — already the single place deciding the
  HTTP status — now also calls `recordExternalBearer(cfg, attempt, outcome)` in every branch including the
  success path, so the metric can never diverge from the response it accompanies (one switch, both jobs).
  `AuthConfig.ExternalBearerMetrics` is `*atomic.Pointer[ExternalBearerMetricsRecorder]` — see Deviations #2 for
  why a plain field doesn't work here.
- **`pkg/hub/google_credential_cache.go`** — `cachingGoogleCredentialValidator` gained an
  `atomic.Pointer[GoogleValidatorCacheMetricsRecorder]` field, a `SetMetrics` method, and a `WithCacheMetrics`
  `CacheOption`. `validate`'s singleflight callback now returns a `viaUpstream bool` alongside the cache entry,
  so **every** caller collapsed into one flight (leader and followers alike) records a result — not just the
  leader — classified as `miss` (`viaUpstream`), `hit` or `negative_hit` (`cacheResultFor(err)`).
- **`pkg/hub/ge_exchange.go`** — `handleGEGoogleExchange` calls `s.recordGEExchange(outcome)` at every existing
  return point (rate-limited, not-configured, invalid-request ×2, and the four-way `Exchange()` result switch),
  using the same status-code branching already there — response bytes untouched (`writeError`/`writeJSON` calls
  are unmodified; see `TestGEExchangeMetrics_ResponseBytesUnaffected`).
- **`pkg/hub/server.go`** — `geExchangeMetrics GEExchangeMetricsRecorder` field (Set with `s.mu`, read
  unsynchronized — the same convention `dbMetrics`/`dispatchMetrics`/`gcpTokenMetrics` already use);
  `srv.authConfig.ExternalBearerMetrics` initialized to a fresh empty `atomic.Pointer` in `New()`, before
  `registerRoutes()` captures `authConfig` by value; three new `Set*Metrics` methods
  (`SetExternalBearerMetrics`, `SetGoogleValidatorCacheMetrics` — type-asserts the configured validator against
  an unexported `SetMetrics` interface, `SetGEExchangeMetrics`).
- **`cmd/server_foreground.go`** — one `NewOTelExternalBearerMetrics(mp)` call in the existing OTel-export block
  (same `if cfg.Hub.GCPProjectID != ""` gate as every other Hub OTel recorder), wiring the single recorder into
  all three `Set*Metrics` calls.

## Store_error decision (asked, then ruled on)

I flagged the question the brief asked me to (propose-or-ask on how 503 `store_error` is counted) in my ack to
`ap-em`. The lead's ruling — appended to the brief at 13:30 — is: **`store_error` is its own outcome, distinct
from `upstream_error`**, because they need different alerts (Google-down vs. Hub-store-down). This is exactly
what `serveExternalBearer`'s existing code already does (the `errExternalBearerResolveFailed` arm is checked
separately from, and after, the `ErrGoogleUpstreamError` arm) — no code change was needed to satisfy the
ruling, only instrumenting the two arms with their own outcome constants, which the implementation commit
already does.

## Acceptance — O1: each outcome increments its labelled counter

| Outcome / result | Test(s) | Mutant(s) killed |
|---|---|---|
| `ok` (user, ID token) | `TestExternalBearerMetrics_OK_UserIDToken` | Drop the final `recordExternalBearer` call; swap its outcome constant |
| `ok` (user, access token) | `TestExternalBearerMetrics_OK_UserAccessToken` | Same, proves `kind=access_token` specifically |
| `ok` (service account, ID token) | `TestExternalBearerMetrics_OK_ServiceAccountIDToken` | Proves `principal=service_account` on the success path |
| `not_applicable` (no trust configured) | `TestExternalBearerMetrics_NotApplicable_NoTrust` | Move `attempt.kind`/`principal` off `Unknown` for this branch; drop the call |
| `rejected` (wrong audience, principal unknown) | `TestExternalBearerMetrics_Rejected_WrongAudience` | Swap for `upstream_error`/`store_error`; report `principal=user` before validation ran |
| `rejected` (SA access token, principal known) | `TestExternalBearerMetrics_Rejected_ServiceAccountAccessToken` | Report `principal=unknown` despite `IsServiceAccount` already being known |
| `rejected` (SA project not allowed) | `TestExternalBearerMetrics_Rejected_ServiceAccountProjectNotAllowed` | Report `kind=access_token` (only ID tokens reach this branch) |
| `rejected` (Phase 4 domain not allowed) | `TestExternalBearerMetrics_Rejected_DomainNotAllowed` | Give this branch its own outcome instead of falling into the shared `default:` arm |
| `rate_limited` | `TestExternalBearerMetrics_RateLimited` | Drop the call in the `errors.As(&rlErr)` arm; report `kind=unknown` (kind IS known at this point) |
| `upstream_error` | `TestExternalBearerMetrics_UpstreamError` | Swap for `store_error` — the two are deliberately distinct per the lead's ruling |
| `suspended` | `TestExternalBearerMetrics_Suspended` | Swap for `forbidden` |
| `forbidden` (`Resolve` wraps `store.ErrNotFound`) | `TestExternalBearerMetrics_Forbidden_ResolveErrNotFound` | Let this fall through to the generic `errExternalBearerResolveFailed` arm (`store_error`) instead of the specific `store.ErrNotFound` arm (`forbidden`) — this is the exact case the lead's ruling calls out |
| `store_error` (other `Resolve` fault) | `TestExternalBearerMetrics_StoreError_ResolveInternalFault` | Swap for `forbidden` or `upstream_error` |
| nil-safety (unset field, wired-but-empty pointer, `Store()`d nil interface) | `TestExternalBearerMetrics_NilAuthConfigField_NoPanic` | n/a — proves no panic in three disabled shapes |
| closed label set | `TestExternalBearerMetrics_LabelTypesOnlyConstructedAsConstants` | Introduce `ExternalBearerOutcome(someVar)` (or the other four label types) anywhere outside the const block — this greps every non-test source file for that conversion syntax, the same pattern `TestNoTokenInfoOutsideGoogleCredentialValidator` (I3) already uses |
| cache `miss` then `hit` | `TestGoogleCredentialCache_MetricsRecordsMissThenHit` | Record `hit` on the first (uncached) call, or don't record the second call's `hit` |
| cache `negative_hit` | `TestGoogleCredentialCache_MetricsRecordsNegativeHit` | Classify a negative entry as `hit` |
| `ErrGoogleUpstreamError` never `hit`/`negative_hit` | `TestGoogleCredentialCache_MetricsUpstreamErrorNeverCountsAsHitOrNegativeHit` | Start negatively caching `ErrGoogleUpstreamError` (would also break C4) |
| cache metrics wired post-construction | `TestGoogleCredentialCache_MetricsSetMetricsAfterConstruction` | Drop `SetMetrics`, or have it not take effect for calls already in flight |
| cache nil-safety | `TestGoogleCredentialCache_MetricsNilRecorder_NoPanic` | n/a |
| exchange `ok` | `TestGEExchangeMetrics_OK` | Drop the success-path call |
| exchange `not_configured` | `TestGEExchangeMetrics_NotConfigured` | Swap outcome, or fall through to the generic switch |
| exchange `invalid_request` (malformed JSON, oversize body) | `TestGEExchangeMetrics_InvalidRequest_MalformedJSON`, `..._OversizeBody` | Merge these into `bad_request` |
| exchange `bad_request` | `TestGEExchangeMetrics_BadRequest_UnsupportedCredentialType` | Swap for `invalid_credential` |
| exchange `invalid_credential` | `TestGEExchangeMetrics_InvalidCredential_ForgedToken` | Swap for `bad_request`/`exchange_failed` |
| exchange `forbidden` (SA credential) | `TestGEExchangeMetrics_Forbidden_ServiceAccount` | Swap for `invalid_credential` |
| exchange `exchange_failed` (500) | `TestGEExchangeMetrics_ExchangeFailed_ResolveInternalFault` | Swap for `forbidden`/`invalid_credential` |
| exchange `rate_limited` | `TestGEExchangeMetrics_RateLimited` | Drop the call before the early `return` |
| exchange nil-safety | `TestGEExchangeMetrics_NilRecorder_NoPanic` | n/a |
| exchange response bytes unaffected | `TestGEExchangeMetrics_ResponseBytesUnaffected` | Any change to `writeError`/`writeJSON` call sites while adding the metric call |

All Phase 1-4 external-bearer/cache/exchange suites (`TestExternalBearer*`, `TestGoogleCredentialCache*`,
`TestGEExchange*`, `TestNoPackageLevelMutableState`, `TestNoTokenInfoOutsideGoogleCredentialValidator`) stay
green, run with `-race`.

## Deviations / design questions

1. **`otel_external_bearer_metrics.go` is not added to `TestNoPackageLevelMutableState`.** It declares
   package-level `var (_ Interface = (*Impl)(nil) ...)` compile-time assertions — not mutable state (never
   written after compilation) but not `errors.New(...)` sentinels either, so the AST check's simple heuristic
   would flag them. The two pre-existing files with the identical pattern, `otel_metrics.go` and
   `otel_gcp_metrics.go`, are excluded from that check's file list for the same reason; this file follows that
   precedent rather than inventing a new one. `external_bearer_metrics.go` (the label/interface file, zero
   package-level `var`s) *is* added, and passes trivially.
2. **`AuthConfig.ExternalBearerMetrics` is `*atomic.Pointer[ExternalBearerMetricsRecorder]`, not a plain
   interface field.** `UnifiedAuthMiddleware(cfg AuthConfig)` captures `cfg` by value exactly once, in
   `registerRoutes()` at the very end of `New()`. Every other Hub OTel-backed recorder (`SetMetrics`,
   `SetDBMetrics`, `SetDispatchMetrics`, `SetGCPTokenMetrics`) is wired from `cmd/server_foreground.go` *after*
   `New()` returns, because building the OTel exporter needs `hubSrv.HubID()`, which only exists once the
   server is constructed — but those all live as plain fields read fresh off `*Server` by `*Server` methods, so
   the post-`New()` timing never mattered for them. `authenticateExternalBearer`/`serveExternalBearer` are
   free functions taking `cfg AuthConfig` by value (deliberately Server-independent, per Phase 1-4's test
   style), so a plain field would freeze at its `New()`-time value — nil — forever, in production. I used the
   same indirection `AuthConfig.FederationAuth` already uses for hot reload (a pointer stored once in `cfg`,
   whose target can be swapped later), since it's precedented in this exact file for the exact same structural
   reason, rather than inventing a new pattern. The cache decorator didn't need this: it's a
   pointer-receiver struct already referenced through an interface value, so a plain `SetMetrics` method
   mutates the one shared instance regardless of how many copies of the interface value exist. I flagged this
   ordering constraint as worth a second look in my report below, since it's the one piece of this phase that
   isn't a straight application of an existing pattern.
3. **Wired the OTel recorder all the way into `cmd/server_foreground.go`,** not just `pkg/hub`. The brief's task
   list names only `pkg/hub/metrics.go`/`otel_metrics.go`, but "how they are injected into Server" (its own
   phrasing) is incomplete without the composition-root wiring that makes the counters reach Cloud Monitoring —
   every existing Hub metric follows that same three-layer shape (interface, OTel impl, `cmd/` wiring). This is
   an additive change to an existing block (one more `NewOTelXxx`/`Set*` group, same shape as the four already
   there), not new plumbing, so I judged it in scope rather than asking first.
4. **Domain-rejection test added after Phase 4 landed mid-task**, per the brief's instruction ("if Phase 4 has
   landed when you finish, make sure its path is counted too"). No code change was needed — `errDomainNotAllowed`
   already falls into `serveExternalBearer`'s shared `default:` 401 arm, which already records `rejected` — only
   a test was missing, added as `TestExternalBearerMetrics_Rejected_DomainNotAllowed`.
5. **Exchange outcome constants are hand-written, not derived by casting the handler's existing `code` string
   variable**, even though today they'd produce identical values. This keeps the metric's label vocabulary
   closed and reviewable independently of the JSON error-code strings (which are a public API surface I don't
   own changing), at the cost of one extra `outcome := ...` assignment alongside each existing `code := ...` in
   `handleGEGoogleExchange`'s status-code switch.

## Gates

- ✅ `gofmt -l` on every changed/new file — clean.
- ✅ `go build -buildvcs=false ./...` — clean (includes `cmd/server_foreground.go`'s new wiring).
- ✅ `go vet ./pkg/hub/... ./cmd/...` — clean.
- ✅ `GOGC=40 golangci-lint run --new-from-rev=upstream-main --concurrency=1 ./pkg/hub/... ./cmd/...` —
  `0 issues.` (`upstream-main` fetched fresh this run; `git merge-base HEAD upstream-main` resolved to
  `a53175c23e61c317841fa04b8c8e63c1097562b4`, matching the brief's stated base).
- ✅ Targeted subset with `-race`: `TestExternalBearer|TestGoogleCredentialCache|TestGEExchange|
  TestNoPackageLevelMutableState|TestNoTokenInfoOutsideGoogleCredentialValidator`, across `pkg/hub` (incl.
  `auth`, `authzop`, `githubapp`, `imagecheck`) — all green, no data races, run twice (once before the Phase 4
  rebase, once after).
- ✅ I3 re-check: `grep -rn tokeninfo pkg/hub --include='*.go' | grep -v _test | grep -v
  google_credential_validator.go` — empty.
- ✅ Bare-issue-number greps against the merge-base, with real GNU grep (`/usr/bin/grep`), at the final commit
  (`e9b9c22ad`): `git log upstream-main..HEAD --format=%B | grep -nE '(^|[^/A-Za-z])#[0-9]+'` and
  `git diff upstream-main..HEAD -- pkg/hub cmd/server_foreground.go | grep -nE '^\+.*(^|[^/A-Za-z0-9])#[0-9]{3,}'`
  — both empty.
- Not run: the full `pkg/hub`/`pkg/config` suite (`-timeout 40m`) — per the brief, this needs the shared
  full-run slot from `ap-em` (serialized with `ap-p4-dev`, who used it last per its project log). Requesting it
  in my report below.
