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
  Instrument names are `scion.hub.external_bearer`, `scion.hub.google_validator_cache` and
  `scion.hub.ge_exchange.requests`. **Correction (fix round 1, R5):** the paragraph originally here claimed an
  OTel Prometheus bridge that does not exist in this binary; see that round's entry below for the real export
  path and metric type names. Not added to `TestNoPackageLevelMutableState` — see Deviations #1.
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

---

## Fix round 1 (review `p5o-r1-ap-p5o-rev.md`, REQUEST CHANGES on `206ae48e2`)

**Commits:** `dcc2b9909` (feature), `647a6bb29` (tests).

Verdict was REQUEST CHANGES with Critical 0, Required 5, Optional 5, Nit 2, FYI 4 — the outcome/label/nil-guard
logic itself was judged correct (14/14 targeted mutants killed); every Required and Optional finding was about
code or wiring with no test at all, or a doc comment describing the wrong export mechanism. Disposition below
per `ap-p5o-dev-fix-r1.md`, including its 14:52 amendment (O1 confirmed by the lead; fold the new counters into
the existing `combinedMetrics` JSON `handleMetrics` serves, and align every comment with the amended §4.7:
real Cloud Monitoring names, the `/metrics` section, POST-only exchange counting, the exact soak-gate wording).

| Finding | Change | Test | Mutant proven killed |
|---|---|---|---|
| R1: no test of post-`New()` setter wiring | `server.go`: unchanged logic, now exercised end to end | `TestServer_MetricsSettersReachRunningHandler` (`ge_exchange_route_test.go`): builds via `New()`, captures `Handler()` *before* calling all three setters (the only ordering that actually exercises the `atomic.Pointer` publication guarantee), then drives an external-bearer request and an exchange POST through that captured handler, and checks the cache decorator directly | M8 (replace the box instead of `Store`-ing into it), M13 (cache setter no-op), M14 (exchange setter no-op) — each hand-mutated, confirmed to fail this test, reverted |
| R1 (same finding): cache setter's type-assertion failure was silent | `server.go`'s `SetGoogleValidatorCacheMetrics` now `slog.Warn`s and returns instead of silently doing nothing | covered by the same test (the happy path) and by reading | n/a (observability of a defensive branch, not a new outcome) |
| R2: OTel recorder untested | none (recorder unchanged except the dual-write in R5/O1) | `otel_external_bearer_metrics_test.go`: `sdkmetric.NewManualReader`, one test per `Record*` asserting instrument name, unit, exact attribute key set/values, and sum, plus a nil-snapshot no-panic test | M7 (rename an attribute key, e.g. `outcome`→`result`) — the exact-key-set assertion (`wantAttrs`, which checks `Len()` too) fails on both a renamed and an added/dropped key |
| R3(a): singleflight follower `miss` untested | none (behaviour unchanged; it was already correct, per the review's own judgement) | `TestGoogleCredentialCache_MetricsSingleflightFollowersRecordMiss`: the same delay-widened-window technique C6 uses, 6 concurrent callers into one flight, asserts 1 base call and 6 `miss` records | M9 (move the `miss` record into the leader-only callback) — followers would then record 0 instead of `n-1`; failing this test's exact-count assertion |
| R3(b): `GoogleValidatorCacheMiss` doc said "required an upstream call" | `external_bearer_metrics.go`: reworded to "not served from cache… including a singleflight follower… upstream calls ≤ miss" | — (doc only) | n/a |
| R4: review-history narration in 7 lines this half added | Removed from `auth_external_bearer.go`, `external_bearer_metrics.go` (×2) and `external_bearer_metrics_test.go` (×4); each rewritten to state the invariant instead of the review-round provenance. Also fixed the stale "or, from a later phase, a user domain" wording now that the domain check is on the branch | `git diff 206ae48e2 -- pkg/hub cmd/server_foreground.go \| grep -niE '^\+.*(\blead\b\|ruling\|phase [0-9]\|later phase\|\br7\b\|review r)'` — empty (checked before every push in this round) | n/a (static check) |
| R5: wrong exported metric names; nonexistent Prometheus bridge | `otel_external_bearer_metrics.go`'s doc comment rewritten: the only exporter is `pkg/observability/hubmetrics` (`mexporter`) to GCP Cloud Monitoring, gated on `cfg.Hub.GCPProjectID`; the instrument names become `workload.googleapis.com/scion.hub.external_bearer`, `…/scion.hub.google_validator_cache`, `…/scion.hub.ge_exchange.requests` there. The Prometheus-bridge sentence is gone. Corrected the same claim in this log's Phase 5 section (see the note inserted above) | read against `pkg/observability/hubmetrics/hubmetrics.go` (confirmed no Prometheus dependency; `mexporter` is the only exporter) | n/a (doc/log correction) |
| O1 (promoted to required by the lead's 14:52 confirmation): counters absent unless `hub.gcp_project_id` set; not on `/metrics` | New `pkg/hub/external_bearer_snapshot_metrics.go`: `ExternalBearerSnapshotMetrics`, dependency-free, following `gcp_metrics.go`'s shape exactly (mutex + counts, no OTel/GCP import). `Server.New` constructs one unconditionally and wires it as the default recorder for all three design §4.7 counters; `NewOTelExternalBearerMetrics` now takes that same instance and dual-writes into it, so `GET /metrics` counts the same totals whether or not `cmd/server_foreground.go` ever wires the OTel recorder. Added an `"externalBearer"` section to `handleMetrics`'s `combinedMetrics`, next to `"broker"`/`"gcp"`. Confirmed no package-level state was needed (only instance fields and package-level *functions* returning fresh enumeration slices — `TestNoPackageLevelMutableState` still passes with the new file added to its list), so this stayed within "small" and did not need to be escalated back to `ap-em` before building | `TestExternalBearerSnapshotMetrics_RecordsMoveTheSnapshot`, `..._DeterministicKeys`, `TestHandleMetrics_ExternalBearerSection`, `TestHandleMetrics_NoMetricsWhenNothingWired`, plus `TestOTelExternalBearer_*`'s snapshot assertions for the dual-write | n/a (new coverage, not a mutant retrofit) |
| O2: `validate`'s godoc attached to the new type | `google_credential_cache.go`: moved `googleCredValidateResult` and its comment above the `// validate is …` paragraph | pre-existing cache suite, unchanged and green (doc-only fix) | n/a |
| O3: closed-set guard misses untyped literals | Added a `valid()` method (closed switch over its own constants) to each of the five label types, backed by a package-level enumeration *func* (not a var) per type | `TestExternalBearerMetrics_LabelValidMethods`: drives every declared constant through `valid()` (must be true) and several out-of-set values (must be false), per type | a constant added to one of the enumeration funcs without a matching `valid()` case (or vice versa) fails its own subtest |
| O4: non-POST exchange requests uncounted | Documented on `GEExchangeOutcome`: the counter covers POST requests only (the endpoint's only routable method); no new outcome added | n/a (fix by documentation, as directed) | n/a |
| O5: `…ResponseBytesUnaffected` overclaimed | Renamed to `TestGEExchangeMetrics_ResponseShapeUnaffected`; now masks the token/timestamp fields that vary run to run and compares the full decoded struct with `reflect.DeepEqual`, not two hand-picked fields | same test, strengthened | a mutation touching any other response field now fails this test, not just `TokenType`/`User.Email` |
| F3: setter nil-deref on a non-`New()` `Server` | `SetExternalBearerMetrics` now allocates its `atomic.Pointer` box if `nil` instead of panicking | exercised implicitly by `TestServer_MetricsSettersReachRunningHandler` calling it on a `New()`-built server (the guard path itself has no dedicated unit test — it is a one-line defensive nil-check, not a design decision with an observable branch) | n/a |
| N1, N2, F1, F2, F4 | Deferred / no action, per the brief | — | — |

### Deviations / design questions (fix round 1)

1. No new design ambiguities. The 14:52 amendment settled O1 (fold into `combinedMetrics`, dual-write, use the
   real names) before any of this round's implementation started, so nothing here was guessed.
2. `ExternalBearerSnapshotMetrics.RecordExternalBearer` only counts by `outcome`, not the full
   `kind`/`principal`/`outcome` cross-product the OTel-exported series carries. The in-process section is meant
   to answer "is the exchange counter non-zero" and "what does the outcome mix look like on this replica" for
   the soak gate and quick diagnosis; the full per-label breakdown is what Cloud Monitoring is for once export
   is configured. Flagging this simplification in case `ap-em`/the lead wants the full cross-product in
   `/metrics` too — it would still fit the "small, no package-level state" bound, just with more map keys.

### Gates (fix round 1)

- ✅ `go build -buildvcs=false ./...` — clean.
- ✅ `go vet ./pkg/hub/... ./cmd/...` — clean.
- ✅ `gofmt -l pkg/hub cmd` — empty.
- ✅ `GOGC=40 golangci-lint run --new-from-rev=a53175c23 --concurrency=1 ./pkg/hub/... ./cmd/...` — `0 issues.`
- ✅ Targeted `-race`: `TestExternalBearer|TestGEExchange|TestGoogleCredential|TestNoPackageLevelMutableState|
  Metrics|TestOTel|TestNoTokenInfoOutsideGoogleCredentialValidator`, run before and after rebasing onto
  `ap-p4-dev`'s concurrent fix-round commits (`47dac5600`..`e6dc234cb`) — all green both times, no data races.
- ✅ Narration grep (`git diff 206ae48e2 -- pkg/hub cmd/server_foreground.go \| grep -niE
  '^\+.*(\blead\b\|ruling\|phase [0-9]\|later phase\|\br7\b\|review r)'`) — empty.
- ✅ Bare-issue-number greps against `a53175c23` (real GNU grep), at the final commit (`647a6bb29`) — both empty.
- ✅ Manual mutation-resistance pass on the exact mutants the review named as surviving (M7, M8, M9, M13, M14):
  each hand-introduced, confirmed to fail its new test, then reverted and re-diffed clean against the committed
  tree.
- Not run: the full `pkg/hub/...` suite (`-timeout 40m`) — this round changes non-test Hub code, so the brief
  requires it. Requesting the slot from `ap-em` in my report; `ap-p4-dev` may be holding it per the shared,
  serialized-runs rule.

---

## Fix round 2 (review `p5o-r2-ap-p5o-rev-2.md`, REQUEST CHANGES on `714186be2`)

**Commits:** `18f8cecc8` (feature), `727a0a530` (tests).

Verdict was REQUEST CHANGES with Critical 0, Required 2, Optional 3, Nit 4, FYI 5. Every r1 fix was verified
(26/26 targeted mutants killed); the two Required findings were both "a real production path has no test" —
`New()`'s default wiring (the whole reason the in-process `/metrics` section exists) and the `valid()` boundary
check (present but never called from production code). The outcome-only `/metrics` deviation from fix round 1
was judged adequate and kept as-is.

| Finding | Change | Test | Mutant proven killed |
|---|---|---|---|
| R1: `New()`'s default wiring untested (D1–D3 survive; a dropped wire reads as "zero exchange traffic" = soak pass) | No production logic changed | `TestServer_DefaultMetricsWiring_RecordsWithoutSetters` (`ge_exchange_route_test.go`): builds via `New()` with **no** setter calls, GE exchange enabled; drives an exchange POST, a non-Hub bearer to `/api/v1/auth/me`, and a direct cache record; reads `GET /metrics` through the same handler and checks all three counters moved by exactly 1. `TestServer_DefaultMetricsWiring_OTelSetterStillMovesSnapshot` extends it: builds the OTel recorder exactly as `cmd/server_foreground.go` does and proves `/metrics` still moves after the setters replace the default (covers Optional O3 too) | D1 (delete `srv.geExchangeMetrics = srv.externalBearerSnapshot`), D2 (delete the `ExternalBearerMetrics.Store` call), D3 (delete the cache decorator's `SetMetrics` call) — each hand-mutated one at a time, confirmed to fail the first test, reverted and re-diffed clean |
| R2: the `valid()` "fix" is test-only, has no production caller, and its comment overclaimed | `recordExternalBearer`, `recordCache` and `recordGEExchange` (the three functions that already are the single boundary between "a label was computed" and "a recorder saw it") now check every label's `valid()` first, dropping the record with `slog.Warn` (label name + Go type only, never the value) if any is out of set. Converted all five `valid()` methods from a loop over the enumeration func to a closed `switch` over their own named constants — a real boundary check, not a derived one | `TestRecordExternalBearer_DropsInvalidLabel`, `TestRecordCache_DropsInvalidLabel`, `TestRecordGEExchange_DropsInvalidLabel` (an out-of-set label reaches no recorder — the guarantee the comment now actually makes) plus a rewritten `TestExternalBearerMetrics_LabelValidMethods` that cross-checks each `valid()` switch against an independently hardcoded expected set (not the enumeration func, so the switch and the enumeration can't drift apart unnoticed) | a `recordExternalBearer("oops")`-shaped call now visibly drops (log line + zero recorder calls) instead of silently reaching the recorder — the exact untyped-literal case O3 was originally about |
| O1: no reset marker on `/metrics` | Added `Since` (RFC3339 UTC, set at construction, returned unchanged on every snapshot) to `ExternalBearerMetricsSnapshot` | `TestExternalBearerSnapshotMetrics_Since`: present, parseable, fixed across snapshots taken after further recording | n/a (new field) |
| O2: bare `scion_hub_*` names in production comments | Replaced all four (`auth.go`, `google_credential_cache.go`, `auth_external_bearer.go`, `server.go`) with the logical short names `external_bearer_metrics.go` defines, or a pointer to that paragraph | `grep -rn "scion_hub_" pkg/hub/*.go cmd/*.go \| grep -v _test` — empty | n/a (doc-only) |
| O3: `cmd`'s snapshot argument untested | Covered by the R1 extension (`..._OTelSetterStillMovesSnapshot`) | see R1 row | n/a |
| N1: three stale comments after the default wiring changed | Fixed in `external_bearer_metrics.go` (package doc: default is now the in-process snapshot, not "disabled"), `ge_exchange.go` (`recordGEExchange`'s nil case is now only a non-`New()` Server), `handlers_health.go` (`externalBearerSnapshot` is unconditional like `gcpTokenMetrics`, not unlike it) | read against the current wiring | n/a |
| N2: `no_metrics` test comment implied production can reach it | Reworded to say this branch is reachable only for a Server not built through `New()` | — (comment only) | n/a |
| N3: setter-test comment named the wrong mechanism | See Deviations below — the reviewer's proposed replacement wording is itself inaccurate; reworded to state the verified-correct mechanism instead | — (comment only) | n/a |
| N4: new `// O1 —` banners | Deferred to the polish sweep, per the brief | — | — |
| F4: `SetGEExchangeMetrics` comment overstated "read correctly regardless of when this is called" | Reworded: documents the same before-`Start()` requirement `gcpTokenMetrics` already has, which `cmd/server_foreground.go` satisfies | read against `cmd/server_foreground.go`'s call order | n/a |
| F1: branch-level bare-ref grep hit `.design/project-log/auth-passthrough-p4-dev.md`, not this half | Re-ran both greps at the new tip; see Gates below. No code change (not this half's file; `ap-p4-dev` owns that log) | — | — |
| F2, F3, F5 | No action, per the brief | — | — |

### Deviations / design questions (fix round 2)

1. **N3's prescribed wording is itself inaccurate; used corrected wording instead.** The review states "`UnifiedAuthMiddleware`'s
   by-value capture of `authConfig` happens in `registerRoutes`, inside `New()`" and asks me to say so. I checked this against the
   code: `registerRoutes()` (`server.go`) only calls `s.mux.HandleFunc`/`s.guarded` — it never calls `applyMiddleware` or
   `UnifiedAuthMiddleware`. The only two call sites of `applyMiddleware` are `Handler()` and `Start()` (`server.go:3925,4091`), and
   `UnifiedAuthMiddleware(` is called nowhere else in the package. `Start()` is what builds the production `http.Server`'s `Handler`
   exactly once, via `applyMiddleware(s.mux)`, and that is where the by-value capture of `authConfig` actually happens — after
   `New()` returns, which is also when `cmd/server_foreground.go` calls the three setters, before it calls `Start()`. I reworded the
   test's comment to state this (verified) mechanism instead of the review's (unverified) one, since a comment should not assert
   something the code doesn't do. The test itself, and its verdict on M8/M13/M14, are unaffected — I re-ran the same mutation checks
   in this round's Gates below to confirm. Flagging this for `ap-em`/the reviewer rather than silently substituting: happy to change
   the wording again if I've misread something.
2. No other design ambiguities. O1's construction-timestamp fix, and the closed-switch/boundary-enforcement fix for R2, both matched
   the brief's explicit recipe with no judgment calls needed.

### Gates (fix round 2)

- ✅ `go build -buildvcs=false ./...` — clean.
- ✅ `go vet ./pkg/hub/... ./cmd/...` — clean.
- ✅ `gofmt -l pkg/hub cmd` — empty.
- ✅ `GOGC=40 golangci-lint run --new-from-rev=a53175c23 --concurrency=1 ./pkg/hub/... ./cmd/...` — `0 issues.`
- ✅ Targeted `-race`: `TestExternalBearer|TestGoogleCredential|TestGEExchange|TestNoPackageLevelMutableState|TestOTel|
  TestServer_|TestHandleMetrics|TestNoTokenInfoOutsideGoogleCredentialValidator|TestRecord`, run before and after rebasing onto
  `ap-p4-dev`'s concurrent commits (`95e3e3b4e`..`06dbf2722`) — all green both times, no data races.
- ✅ `-race -count=20` on `TestGoogleCredentialCache_MetricsSingleflightFollowersRecordMiss`,
  `TestServer_MetricsSettersReachRunningHandler` and both `TestServer_DefaultMetricsWiring_*` tests — clean.
- ✅ Manual mutation-resistance pass on the three named mutants (D1, D2, D3): each hand-introduced in `server.go`, confirmed to fail
  `TestServer_DefaultMetricsWiring_RecordsWithoutSetters`, then reverted and re-diffed clean against the committed tree.
- ✅ Narration grep (adds `fix round` to the term list per this round's review) — empty on this half's delta since `714186be2`.
- ✅ Commit-message and diff bare-issue-number greps against `a53175c23`, scoped to `pkg/hub` and `cmd/server_foreground.go` — both
  empty. **Re-verifying F1** at this push's tip: the unscoped whole-tree diff grep still has exactly one hit, in
  `.design/project-log/auth-passthrough-p4-dev.md` (Phase 4's own log, `#1847`-derived prose, added by `ap-p4-dev`'s `2981a58a7`) —
  not this half's file, not this half's commit. Relaying to `ap-em`/the Phase 4 owner again in my report, since it's outside my
  ownership.
- Not run: the full `pkg/hub/...` + `./cmd` suite (`-timeout 40m`) — this round changes non-test Hub code, so the brief requires it.
  Requesting the slot from `ap-em` in my report.
