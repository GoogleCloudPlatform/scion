# auth-passthrough whole-PR "upstream polish" sweep

**Agent:** ap-polish-dev · **Branch:** `scion/auth-passthrough` · **Base:** `a53175c23`
**Head after sweep:** `44c76320e`

## Goal

Remove review-history narration and internal design/test-plan IDs from everything
upstream-bound (Go code/test comments, doc comments, README/sample files,
docs-site pages) under `pkg/`, `cmd/`, `extras/`, `docs-site/`, limited to lines
added by this branch (`git diff a53175c23 HEAD`). Comments only — no code, string
literal, log message, test assertion, or test logic changed.

## Known items (1-8) → file:line → change

| # | File:line (final) | Before | After |
|---|---|---|---|
| 1 | `pkg/hub/server.go:1680` | `...fails closed for every new one (review r1 finding 1).` | `...fails closed for every new one.` |
| 2 | `pkg/hub/ge_exchange_test.go:549` | `...the plain ErrGoogleFieldDisagreement case in the switch below and return 401...` (inaccurate: no switch appears "below" in this test file) | `...case in ge_exchange.go's error switch and return 401...` (the real `case errors.Is(err, ErrGoogleFieldDisagreement):` lives in `ge_exchange.go:189`) |
| 3 | `pkg/hub/auth_external_bearer.go:59-64` | `...Design §4.2(ii) only makes SA ID tokens validate...Renamed from errExternalBearerPrincipalRejected (Phase 2), which rejected every SA identity outright before Phase 3 gave ID tokens an admission policy.` | `...Only SA ID tokens validate (azp/sub-bound audience rule); access tokens carry no equivalent binding, so an SA is never admitted this way, on any project.` (rename history dropped; this is a variable rename, not a test-function rename, so nothing to list under the rename-tracking rule) |
| 4 | `pkg/config/federation_config.go:119-129` (`isActiveGoogleUserIssuer`) | — | **Verified, no change needed.** `git log -p` shows the historical `"allowed_domains reuses this same predicate"` wording (present at an earlier commit) was already rewritten in a prior round to `"Any Google-issuer-scoped field that only takes effect through that gate (AllowedGCPProjects, AllowedDomains) does nothing on any other shape..."` — accurate now that `AllowedDomains` exists and no longer describes "reuse." |
| 5 | `pkg/hub/external_bearer_snapshot_metrics_test.go:25`, `pkg/hub/ge_exchange_route_test.go:501`, `pkg/hub/google_credential_cache_test.go:595`, `pkg/hub/external_bearer_metrics_test.go:32`, `pkg/hub/ge_exchange_metrics_test.go:30` | `// O1 — ...` / `// C4 — ...` / `// C6-style` / `// I3's` banners | Row-ID prefix dropped, banner reworded to lead with the behaviour (e.g. `// The in-process counters exist and move regardless of GCP export configuration...`) |
| 6 | `pkg/hub/federation_auth.go:57`, `pkg/config/federation_config.go:249,263` | `(design §4.1)`, `(design §4.1/§4.4)` ×2 | parentheticals dropped, surrounding sentence kept |
| 7 | ~90 lines across `pkg/hub/auth_external_bearer*.go(+test)`, `google_*.go(+test)`, `ge_exchange*.go(+test)`, `external_bearer_*.go(+test)`, `otel_external_bearer_metrics*.go`, `federation_auth_warning_test.go`, `authzop/catalog.go`, and two `extras/scion-a2a-bridge` test comments | review-round refs, `design §x.y` citations, and row/mutation IDs (`Phase N`, `I1-I4`, `O1-O4`, `R1-R7`, `U1-U6`, `S1-S7`, `C1-C6`, `K1-K3`, `F1-F2`, `M11`/`M22`/`M29`) | reworded to plain behavioural descriptions; see commits below for the full diff |
| 8a | `.design/project-log/auth-passthrough-p4-dev.md:321` (R1 row) | Garbled nested-backtick quoting describing where two literal NBSP bytes sat | Reworded as plain prose: "one inside an escaped-string example... and one inside a bare quoted-space example..." |
| 8b | `.design/project-log/auth-passthrough-p5b-dev.md:575` (N1 row, "F4 fix round 1" section) | — | **Verified, no change needed.** The row already reads "Partially fixed at the time (corrected in fix round 2, see below)..." — coherent, not garbled, at HEAD. |

Items 9-11 (not in the "1-8" table above but also required by the brief):

- **Item 9** — `pkg/hub/ge_exchange_route_test.go:508-521`: the comment was `Start()`-only ("it is Start(), not registerRoutes/New(), that captures authConfig..."). Confirmed via `cmd/server_foreground.go:2427` (`webSrv.MountHubAPI(hubSrv.Handler(), ...)`) that combined mode captures via `Handler()` from `initWebServer`, not `Start()`. Reworded to cover both call sites, consistent with the production comment on `AuthConfig.ExternalBearerMetrics` in `pkg/hub/auth.go:92` (which already said "Start(), Handler()").
- **Item 10** — `pkg/hub/server.go`: `SetExternalBearerMetrics` (`:2550`), `SetGoogleValidatorCacheMetrics` (`:2577`), and `ExternalBearerSnapshotMetrics` (`:2594`) each had a `(design §4.7)` reference; all three dropped.
- **Item 11** — `.design/project-log/auth-passthrough-p5o-dev.md:306,316`: the fix-round-3 "Commits" line labeled `92d1bf321` (the wording-only commit) as `(this entry)`; the entry itself was committed as `d4acb1620`. Fixed to `92d1bf321` (wording fix), `d4acb1620` (this entry). The R1 row's `server.go` line refs (`:4222`, `:3925`, `:4091`) were stale; re-checked against this sweep's final tree and corrected to `:4224` (`registerRoutes`), `:3927` (`Start()`'s `applyMiddleware` call), `:4093` (`Handler()`'s `applyMiddleware` call) — one lower than this sweep's pre-edit HEAD (`:4225`/`:3928`/`:4094`) because a comment edit in this sweep removed one line above them.

## Discovery grep (before / after)

Command (per brief, run with the real `/usr/bin/grep`, not a wrapped `grep`):

```
git diff a53175c23 HEAD -- pkg cmd extras docs-site | /usr/bin/grep -nE '^\+' | \
  /usr/bin/grep -iE 'review|round|finding|phase [0-9]|\bEM\b|item [0-9]|\br[0-9]\b|§|impl-design|F4-|\bK[12]\b|\b[ORNMCI][0-9]{1,2}\b|FYI|carry|ruling|lead'
```

- **Before this sweep:** 397 hits
- **After this sweep:** 168 hits

⚠️ Note on tooling: the interactive shell's `grep` is a wrapper around `ugrep`
(`type grep` shows a function that execs `ugrep -G ...`). That wrapper's regex
engine treats the embedded `^` in `(^|[^/A-Za-z0-9])` as matching after any
`.*`, so it flags **every** `#NNN` occurrence regardless of context — including
correctly-qualified refs like `GoogleCloudPlatform/scion#1847`. The real
`/usr/bin/grep` (GNU grep 3.8) does not have this quirk and was used for every
gate command below and for the discovery grep counts above.

## Remaining 168 hits — why each stays

All 168 are one of two categories; none is narration.

**A. False positives from the keyword regex (substring/technical-term matches):**
- `round`/`Round` inside `context.Background()`, `StartBackgroundServices` (substring "ground" contains "round")
- `round trip` / `round-trip` — genuine technical term for a network round trip (HTTP call to Google), not "review round"
- `RoundTrip` / `RoundTripper` / `erroringRoundTripper` — the real Go `http.RoundTripper` interface/type
- `leader` / `follower` — genuine singleflight terminology (the caller that runs the shared upstream call vs. the callers collapsed into it), not "review lead"
- `leading` (as in "leading wildcard", "leading dot", "leading zero") — substring match on "lead"
- `carrying` / "can still carry a body-level error field" — plain English "carry", not "carry-in"
- "ensured by code review" (`auth_external_bearer_test.go:744`) — a generic statement that structural properties are enforced by the review process in general, not a reference to a specific dated review round
- "ID token the same way" (`auth_transport_process_test.go`) — substring match of "fyi" inside "verifying"
- diff header lines (`+++ b/cmd/server_foreground.go`) — a grep-context artifact, not file content

**B. Protected string literals inside test assertions (brief forbids changing these — "no behaviour change: do not change code, string literals, log messages, test assertions"):**
- `auth_external_bearer_access_token_test.go:216` — `t.Errorf(...".. (Phase 2: trust configured + opaque token is now a candidate access token)")`
- `auth_external_bearer_test.go:1723` — `t.Errorf(... "— I4 requires no mutable package-level state")`
- `external_bearer_ratelimit_test.go:131,134` — `t.Errorf(... "(design §4.4: 5 rps)")`, `"(design §4.4: burst 20)"`
- `ge_exchange_route_test.go:410,415,418,446,449,486` — `t.Error(...)`/`t.Fatalf(...)` messages naming `"(design §4.4 wiring)"`, `"(design §4.2(iii))"`, `"(O3)"` ×2, `"(production roleFor must honour AdminEmails, M11)"`
- `google_credential_cache_test.go:100` — `t.Error(... "(design §4.2(iii): sorted(aud))")`

(An earlier pass in this sweep mistakenly edited these seven literals; caught by
re-running the comments-only proof gate below, which failed until reverted.)

## Test renames

None. No test function name in this branch's added lines carries a narration
suffix (`_R1`, `_r2`, etc.) requiring the brief's rename allowance.

## Byte hazard

```
perl -ne 'print "$ARGV:$.\n" if /\xC2\xA0/' $(git diff --name-only a53175c23 -- pkg cmd extras docs-site .design)
```
— empty (checked after every batch of edits and again at the end).

## Gates

- **Comments-only proof:**
  `go vet ./pkg/hub/... ./pkg/config/... ./cmd/...` — clean.
  `go vet ./...` (from `extras/scion-a2a-bridge/`) — clean.
  `gofmt -l pkg cmd extras` — empty.
  `git diff <start>..HEAD -- '*.go' | grep -E '^[+-][^+-]' | grep -vE '^[+-]\s*(//|$)'` (`<start>` = `d4acb1620`, the branch head before this agent's first commit) — **empty** (no renamed `func Test...` lines either).
- **Lint:** `GOGC=40 golangci-lint run --new-from-rev=a53175c23 --concurrency=1 ./pkg/hub/... ./pkg/config/... ./cmd/...` — `0 issues`. Same command with `./...` from `extras/scion-a2a-bridge/` — `0 issues`.
- **Targeted tests:** `go test ./pkg/config/...` (SCION_* env unset) — `pkg/config/opsettings` and `pkg/config/templateimport` pass; `pkg/config` itself failed three tests (`TestInitProject_NonGitCreatesMarkerAndExternalDir`, `TestInitProject_NonGitRejectsOldStyleDir`, `TestInitProject_NonGitIdempotent`, the last panicking) — confirmed via `git stash` that these failed identically on unmodified `HEAD` (`d4acb1620`), unrelated to `federation_config.go`. Ran `-run 'TestFederationConfig|TestInvalidDomainEntryReason'` explicitly: all pass. `go test ./...` from `extras/scion-a2a-bridge/` — all packages pass. **Root cause (found in fix round 1):** this agent's `GOTMPDIR` sat inside the branch's own git checkout; `TestInitProject_NonGit*` `t.TempDir()`s and `chdir`s into a directory it assumes is *not* inside a git repo, so it failed there and passes with `GOTMPDIR` outside any checkout (e.g. `/tmp/ap-polish-dev-gotmp`) — purely environmental, unrelated to this change.
- **Bare refs:**
  `git log a53175c23..HEAD --format=%B | /usr/bin/grep -nE '(^|[^/A-Za-z])#[0-9]+'` — empty (this agent's own 8 commits are clean; no older-commit hits to report either).
  `git diff a53175c23 | /usr/bin/grep -nE '^\+.*(^|[^/A-Za-z0-9])#[0-9]{3,}'` — empty at final `HEAD`, including over `.design/project-log/auth-passthrough-p1-dev.md` and `auth-passthrough-p5b-dev.md`, both of which contain `GoogleCloudPlatform/scion#1847` cross-repo issue citations from earlier phases' logs: the `[^/A-Za-z0-9]` guard correctly excludes the alnum character preceding `#`, so these are not bare-ref hits (corrected from this log's earlier, inaccurate claim of "two pre-existing hits" — the real `/usr/bin/grep` printed nothing here from the start).

## Commits (base `a53175c23` → head `44c76320e`)

```
251c7bf65 docs(hub): describe external-bearer auth behaviour in comments, not review history
d9ca19bf0 docs(hub): describe Google credential validation in comments, not review history
d79499ade docs(hub): describe external-bearer metrics/rate-limit behaviour, not review history
6a1cbdb04 docs(hub): describe GE exchange and server wiring in comments, not review history
74360ac56 docs(config): describe federation config validation in comments, not review history
4ada65bb9 docs(a2a-bridge): describe hubBearer test behaviour in comments, not review history
44c76320e docs(project-log): fix garbled snippet quoting and stale line refs
```

All pushed to `origin/scion/auth-passthrough`.

## Fix round 1

Review: `reviews/polish-r1-ap-polish-rev.md` (`ap-polish-rev`, REQUEST CHANGES on `75d6d4be7`: 0C / 7R / 3O / 3N / 4FYI).
Fix brief: `briefs/ap-polish-dev-fix-r1.md`. The lead authorized editing test-failure message strings
(`t.Errorf`/`t.Fatalf`/`t.Error`/`t.Fatal`/`t.Logf` format strings in `_test.go` files) this round.

### Findings → file:line → change

| # | File:line | Before | After |
|---|---|---|---|
| R1 | `pkg/hub/external_bearer_metrics.go:110-114` (`ExternalBearerOutcome` doc) | "Each value maps 1:1 to the HTTP status serveExternalBearer returns" — false: `suspended`/`forbidden` share 403, `upstream_error`/`store_error` share 503 | "Each value corresponds to exactly one response (HTTP status plus error code) that serveExternalBearer writes" |
| R2 | `pkg/hub/auth_external_bearer.go:55` (`errExternalBearerNotApplicable` doc) | "the token isn't a JWT with a Google issuer, or — in later phases — no issuer for the token shape" — a non-JWT token with trust configured is now a candidate access token, not not-applicable | "no Google trust configured, or a JWT whose issuer is missing or is not Google" |
| R2 | `pkg/hub/auth_external_bearer.go:108,347` | "SA in this phase" / "service account in this phase" — SA is no longer a blanket rejection (SA ID tokens can be admitted via allowed_gcp_projects) | "SA access token, SA project or user domain not allowed" |
| R3 | `pkg/hub/ge_exchange_test.go:2268` | "covers the fifth minimum case the fix brief asked for" | "covers an ID-token JWKS force-refresh 5xx" |
| R3 | `pkg/hub/google_credential_validator_test.go:711-712` | "previously survived the whole suite" | "is caught only by this test" |
| R4 | `.design/project-log/auth-passthrough-p5b-dev.md:575` (N1 row) | Self-contradictory: says one comment was missed, then says it existed in two files — history shows `F4-*` only ever existed in `internal/bridge/adminoverlay_test.go` | Dropped the "in both … and `cmd/scion-a2a-bridge/main_test.go`" clause |
| R5 | `pkg/hub/external_bearer_ratelimit_test.go:121-126,145` | "were not pinned" (past tense); "unlike the tests above, which shrink burst" (the tests in *this* file don't shrink burst — the ones in `auth_external_bearer_access_token_test.go` do); "override rate/burst" (only burst is overridden) | Present-tense "Pin the limiter defaults…"; corrected cross-reference to `auth_external_bearer_access_token_test.go`; "override burst" |
| R6 | `pkg/hub/auth_external_bearer_access_token_test.go:133-135` | "this keeps the existing SA rejection covering both credential kinds" — false: an SA ID token with a listed project is admitted | "even though SA ID tokens can be admitted via allowed_gcp_projects (see auth_external_bearer_sa_test.go)" |
| R7 | `pkg/hub/auth_external_bearer_test.go:44-45` | Dropping "Phase 1:" earlier turned a historical note into a false present-tense claim: "(Google user ID tokens only)" — the same harness is used by access-token, SA, domain and metrics tests too | Dropped the parenthetical entirely |
| O1 | `pkg/hub/auth_external_bearer.go:110-113` (dup of `:489-495`), `auth_external_bearer_access_token_test.go:174-178`, `auth_external_bearer_test.go:1399-1400,1401-1409` | "An earlier version…" code-history paragraphs | Rephrased as present-tense rationale; the duplicate at `:110-113` was deleted, keeping the one on `classifyResolveError` |
| O2 | `auth_external_bearer_sa_test.go:155`, `external_bearer_metrics.go:44`, `external_bearer_metrics_test.go:600`, `external_bearer_ratelimit.go:57`, `ge_exchange_metrics_test.go:316`, `auth_external_bearer_test.go:1688` | "the design's azp check" / "design's closed-label-set rule" / "used by this design" / "with the design's" defaults / "this design" ×2 | Each reworded to name the behaviour or requirement directly, with no "design" reference. (`auth_external_bearer_sa_test.go:155`'s fix was reverted — see Known limitation below; **superseded, see ap-em ruling further below**.) Also renamed `TestExternalBearerRateLimiter_DefaultsMatchDesign` → `_DefaultsPinned` (no other reference to the old name) |
| O3 | 13 string literals listed in the review (`auth_external_bearer_access_token_test.go:215`, `auth_external_bearer_test.go:1723`, `external_bearer_ratelimit_test.go:131,134,137,140`, `ge_exchange_route_test.go:410,415,418,446,449,486`, `google_credential_cache_test.go:100`) | Narration/design/mutation-ID fragments inside `t.Errorf`/`t.Error`/`t.Fatalf` messages | All 13 edited now that the lead authorized test-failure-message edits; diagnostic content kept, narration removed |
| N1 | `auth_external_bearer_test.go:595` | "as production's unconditional construction guarantees in production" (says "production" twice) | "as New()'s unconditional construction guarantees in production" |
| N2 | `auth_external_bearer_sa_test.go:29-33`, `external_bearer_ratelimit_test.go:121-126,163-171`, `auth_external_bearer_access_token_test.go:165-167`, `ge_exchange_test.go:699`, `auth_external_bearer.go:37-39` | Ragged rewraps left by earlier comment deletions | Reflowed to ~78 columns. Also reflowed several other ragged paragraphs found while reading comments in context (`auth_external_bearer.go`'s error-sentinel docs, `domainOf`, the rate-limit comment, `classifyResolveError`'s dedup; `google_credential_cache.go`'s `withCacheNowFunc`/`WithCacheMetrics`/`negativelyCacheableGoogleError`/`metrics` field/leaderless-call docs; `external_bearer_metrics_test.go`'s label-types doc) |
| N3 | `external_bearer_ratelimit_test.go:56` | Quoted "after 10000 distinct client IPs" scenario had lost its source | "reproducing the full-limiter scenario at a testable scale" |
| F1 | — | `TestInitProject_NonGit*` failures | Root cause found: this agent's `GOTMPDIR` sat inside the branch's own git checkout; those tests `t.TempDir()` + `chdir` into a directory they assume is not in a git repo. Re-ran with `GOTMPDIR=/tmp/ap-polish-dev-gotmp` (outside any checkout): `go test ./pkg/config/...` now passes in full, no failures. Purely environmental, confirmed unrelated to this change. |
| F2 | `.design/project-log/auth-passthrough-polish-dev.md` (Bare refs section) | Claimed "two pre-existing hits" in p1/p5b logs from the second bare-ref grep | Corrected: the `[^/A-Za-z0-9]` guard excludes the alnum character before `#`, so `GoogleCloudPlatform/scion#1847` was never a hit; the real `/usr/bin/grep` printed nothing there from the start |

### Broader sweep (regex-invisible narration)

Ran the requested sweep:
```
git diff a53175c23 HEAD -- pkg cmd extras docs-site | /usr/bin/grep -nE '^\+' | /usr/bin/grep -iE 'design|phase|brief|previously|earlier version|mutant|\bM[0-9]+\b'
```
9 hits. 8 were false positives or legitimate technical usage: "by design" as the ordinary English idiom for "intentionally" (`federation_auth.go`, `ge_exchange_metrics_test.go` ×2), "*atomic.Pointer design" meaning the atomic-pointer field shape, not a document (`ge_exchange_route_test.go`), "previously pushed `auth_scheme`" and "before this protection was added" describing real upgrade-path behaviour, not review history (`extras/scion-a2a-bridge/README.md`), and four generic "a mutant that X would Y" test-rationale sentences (standard mutation-testing vocabulary, not narration about this branch's own review rounds — `main_test.go`, `adminoverlay_test.go`, `auth_external_bearer_domain_test.go` ×2). One genuine miss: `auth_external_bearer_domain_test.go:198` referenced a "U2 check" (a design row ID); reworded to "the ID-token-only domain check".

### Known limitation: one O2 item left unfixed (superseded; see ap-em ruling further below)

`auth_external_bearer_sa_test.go:155`'s trailing comment (`delete(claims, "azp") // azp == "" variant: the design's "azp" check…`) could not be
reworded without breaking the updated comments-only proof: any edit to a trailing comment on a code line necessarily changes that
line's raw text on both sides of the diff, and the removed line (code + old comment together) does not match `^[+-]\s*(//|$)`, so it
would show up as a "non-comment" diff line even though only the comment changed. Moving the comment to its own line above has the
same problem (the old combined line still appears as removed). Left as `git diff 75d6d4be7 -- auth_external_bearer_sa_test.go`
shows no change to this line; reported here for the lead/reviewer to decide whether trailing-comment edits should be added to the
proof's allowed-kinds list in a future round.

### Comments-only proof (updated)

```
$ git diff 75d6d4be7..HEAD -- '*.go' | /usr/bin/grep -E '^[+-][^+-]' | /usr/bin/grep -vE '^[+-]\s*(//|$)'
```
Prints 28 lines (14 −/+ pairs): 13 pairs are `t.Errorf`/`t.Error`/`t.Fatalf` format-string changes in `_test.go` files (the 13 O3
literals — R1, R5 and R6 were comment-only edits and do not appear here), and 1 pair is the renamed `func
TestExternalBearerRateLimiter_DefaultsMatchDesign` → `func TestExternalBearerRateLimiter_DefaultsPinned` line. No other kind of line
appears.

### Gates (fix round 1)

- `gofmt -l pkg cmd extras` — empty.
- `go vet ./pkg/hub/... ./pkg/config/... ./cmd/...` — clean. `go vet ./...` (bridge module) — clean.
- `GOGC=40 golangci-lint run --new-from-rev=a53175c23 --concurrency=1 ./pkg/hub/... ./pkg/config/... ./cmd/...` — `0 issues`. Same
  command with `./...` from `extras/scion-a2a-bridge/` — `0 issues`.
- `go test ./pkg/config/...` (SCION_* unset, `GOTMPDIR=/tmp/ap-polish-dev-gotmp`, outside any git checkout) — all packages pass,
  including all three `TestInitProject_NonGit*` tests (see F1 above).
- `go test ./...` from `extras/scion-a2a-bridge/` — all packages pass.
- Targeted `pkg/hub` tests for every test whose failure message or name changed this round (`TestExternalBearer_AccessToken_ServiceAccount_Rejected`,
  `TestExternalBearer_TrustConfiguredOpaqueToken_AttemptsAccessTokenValidation`, `TestExternalBearer_AccessToken_RateLimitedBeyondBurst`,
  `TestExternalBearer_ServiceAccountIDToken_AZPNotSub_Unauthorized`, `TestNoPackageLevelMutableState`,
  `TestExternalBearerRateLimiter_CleanupAdmitsNewIPAfterMaxAge`, `TestExternalBearerRateLimiter_DefaultsPinned`,
  `TestExternalBearerRateLimiter_DefaultBurstExhaustion`, `TestExternalBearerRateLimiter_HonoursXForwardedForOnlyFromTrustedProxy`,
  `TestGEExchange_Route_SharesValidatorAndResolverWithExternalBearer`, `TestGEExchange_Route_GoogleStackBuiltWithoutExchangeOrTrust`,
  `TestGEExchange_Route_ProductionResolverHonoursAdminEmails`, `TestGEExchange_ExternalIdentityLookupFault_ServerError`,
  `TestGEExchange_RealValidator_IDTokenForceRefreshFailure_ExactBytes`, `TestGoogleCredentialCache_KeyIgnoresAudienceOrder`,
  `TestGoogleCredentialCache_NegativeCacheOnlyForFourListedErrors`, `TestProductionValidator_AccessToken_TokenInfo200WithErrorBody_InvalidCredential`)
  — all pass.
- Byte hazard (`perl -ne 'print "$ARGV:$.\n" if /\xC2\xA0/'` over every file changed since `a53175c23`) — empty.
- Bare refs — both commands empty: commit messages (`git log a53175c23..HEAD --format=%B | /usr/bin/grep -nE '(^|[^/A-Za-z])#[0-9]+'`)
  and added lines (`git diff a53175c23 | /usr/bin/grep -nE '^\+.*(^|[^/A-Za-z0-9])#[0-9]{3,}'`).
- Caught and fixed during this round: one of my own commit messages initially included review-round narration ("Fix round 1 per
  review polish-r1-ap-polish-rev…"); amended before pushing since it had not yet been shared.

### Commits (fix round 1, base `75d6d4be7`)

```
be2721efd docs(hub): fix false statements and missed narration in external-bearer comments
3a39f2139 docs(hub): fix wrong cross-reference and status-mapping claim in metrics/rate-limit comments
48e61ad23 docs(hub): remove remaining narration and reflow ragged comments in GE exchange and credential cache
e501efc5a docs(project-log): fix a self-contradictory row and an inaccurate bare-ref claim
6a6ad1f7d docs(project-log): record fix round 1 findings, changes and gate results
```

**ap-em ruling (post fix round 1):** trailing-comment edits are allowed, provided the code before `//` stays byte-identical. `auth_external_bearer_sa_test.go:156`'s trailing comment (the one Known-limitation item above) was fixed under that ruling: `delete(claims, "azp") ` is identical on both sides of the diff, only the comment text changed from "the design's azp check" to "the azp/sub check".

## Fix round 2

Review: `reviews/polish-r2-ap-polish-rev-2.md` (`ap-polish-rev-2`, REQUEST CHANGES on `88b0a1a32`: 0C / 2R / 2O / 2N / 3FYI). Every
round-1 finding was verified resolved and accurate; what remained was completeness. Brief: `briefs/ap-polish-dev-fix-r2.md` (final
version from `ap-em`, replacing an earlier equivalent draft from `auth-passthrough-lead`).

### R1: 15 test-plan row IDs removed

| File:line | Before | After |
|---|---|---|
| `extras/scion-a2a-bridge/integration/auth_transport_process_test.go:262` | `UATValidator (B3).` | `UATValidator.` |
| `…/auth_transport_process_test.go:297` | `(no exchange, B3)` | `(no exchange)` |
| `…/auth_transport_process_test.go:395` | `— no credential exchange (B3).` | `— no credential exchange.` |
| `…/auth_transport_process_test.go:828` | `TestHubBearerProcessPassthrough is B3: a real bridge process…` | `…proves hubBearer pass-through end to end: a real bridge process…` |
| `…/auth_transport_process_test.go:874` | `B3's core assertion: zero exchanges.` | `The core assertion: zero exchanges.` |
| `extras/scion-a2a-bridge/internal/bridge/auth_test.go:172` | `B1: a Google-shaped (non scion_pat_) credential is admitted…` | `A Google-shaped (non scion_pat_) credential is admitted…` |
| `…/internal/bridge/auth_test.go:201` | `B4 (empty token -> 401).` | `Empty token -> 401.` |
| `…/internal/bridge/auth_test.go:697` | `B2 (hubUAT is unchanged) and hubBearer's defining difference…` | `hubUAT's prefix check and hubBearer's defining difference…` |
| `extras/scion-a2a-bridge/internal/bridge/v0_compat_test.go:819` | `…is the B3-adjacent bridge-side counterpart…` | `…is the companion to TestHubBearerProcessPassthrough…` |
| `pkg/config/federation_config_test.go:395` | `trigger on issuer_type (K1c).` | `trigger on issuer_type.` |
| `pkg/hub/auth_external_bearer_access_token_test.go:106` | `U2 (access-token half) — email_verified=false -> 401.` | `An access token with email_verified=false -> 401.` |
| `pkg/hub/auth_external_bearer_test.go:271` | `U1 (second half) — same sub, changed email…` | `Same sub, changed email…` |
| `pkg/hub/auth_external_bearer_test.go:393` | `U2 (ID-token half) — email_verified=false…` | `An ID token with email_verified=false…` |
| `pkg/hub/auth_external_bearer_test.go:1024` | `Isolates S4 from S2.` | `Isolates the azp/sub check from the project allowlist check.` |
| `pkg/hub/google_credential_validator_test.go:442` | `S4 (validator level) — an SA ID token…` | `At the validator level, an SA ID token…` |

**Generalized sweep (per the brief, to prevent recurrence):**
```
/usr/bin/grep -nE '^\+.*//.*\b[A-Z][0-9]{1,2}[a-z]?\b' <(git diff a53175c23 -- pkg cmd extras)
```
0 hits at final `HEAD`. Sanity-checked the pattern against a pre-fix copy of one of the 15 lines above (`// UATValidator (B3).`) to confirm it does match — it does. The only surviving letter+digit token found anywhere in the branch's diff by broader inspection is the `"U1"` display-name string literal argument in `GenerateTokenPair(..., "U1", ...)` (`auth_external_bearer_test.go`), which the round-2 review itself already identified and said should stay (it is a token display-name value, not a design-row label).

### R2: six "was X, now Y" / code-history sites fixed

| File:line | Before | After |
|---|---|---|
| `pkg/hub/auth_external_bearer_test.go:571-580` | "This used to be its own `_Golden401` test, but it over-claimed… It's superseded by golden case (a)…" (sits directly above a test named `..._Golden401`) | "Golden case (a) in TestExternalBearer_ConfiguredTrustInvariant_Golden pins the exact bytes with GoogleValidator == nil. Production always builds a non-nil GoogleValidator (only trust gates the path), so this test proves the same no-op in that production shape…" **(this rewrite was itself false — see the fix round 3 section: golden case (a) is also wired with a non-nil GoogleValidator; the two tests differ in the token, not in wiring)** |
| `pkg/hub/auth_external_bearer_test.go:1598` | "server.go's New now always constructs them." | "server.go's New always constructs them." |
| `pkg/hub/ge_exchange.go:196-200` (production) | "SA rejection moved out of the validator: the validator now only classifies IsServiceAccount… keeps its pre-existing behaviour… unchanged." | "The validator only classifies IsServiceAccount and each caller decides whether to admit it; the exchange endpoint rejects SA credentials outright." |
| `pkg/hub/ge_exchange_test.go:494-496` | "SA rejection moved out of the validator: the real validator now classifies…" | "The real validator classifies IsServiceAccount rather than erroring…" |
| `pkg/hub/google_credential_validator.go:282-283,293` (production) | "…to keep its behaviour unchanged" / "User tokens (unchanged):" | dropped both parentheticals |
| `pkg/hub/ge_exchange_route_test.go:358-359` | "google_credential_cache.go changed the validator half of this: the external-bearer path now uses a caching decorator…" | "For the validator half, the external-bearer path uses a caching decorator…" |

**Additional grep** (`used to\|moved\|now \|unchanged\|no longer\|superseded\|pre-existing\|new files\|before this`, added comment lines):
32 hits. True positives fixed beyond the six sites above:
- `pkg/hub/auth_external_bearer_test.go:1666-1671` (comment inside `TestNoPackageLevelMutableState`'s file list): "the pre-existing otel_metrics.go and otel_gcp_metrics.go" / "the two pre-existing otel_*.go files" → dropped "pre-existing" (PR-relative framing describing files that simply already exist in the package).
- `pkg/hub/external_bearer_snapshot_metrics_test.go:174`: "proves the pre-existing \"no_metrics\" fallback" → dropped "pre-existing".
- `docs-site/src/content/docs/hosted/single-node/auth.md:310` (table row): "Falls through to the pre-existing rejection for that credential" → "Falls through unchanged to whichever other authentication check applies to that credential".
- `pkg/config/federation_config.go:109`, `pkg/hub/auth_external_bearer_sa_test.go:42`, `pkg/hub/google_credential_cache_test.go:28,43,501`: "Used to gate/prove/widen/put…" reworded to present tense ("Gates…", "Proves…", "widens…", "Puts…") — grammatically these already meant "is used to", not "used to, but no longer", but the ambiguity was cheap to remove.

Every other hit in the 32 is a false positive, re-verified by reading in context:
- `time.Now()` calls, the `now`/`clock` mock-clock variables and parameters (`google_credential_cache.go`, `google_credential_cache_test.go`, `external_bearer_ratelimit_test.go`, `google_identity_resolver.go`, `ge_exchange_ratelimit.go`, `ge_exchange_test.go`, `google_credential_validator_test.go`, `auth_external_bearer_sa_test.go`) — "now" as a variable/function name or `time.Now()`;
- "now" in a present-tense, non-historical sense ("is now full", "should now be admitted", "must now succeed", "must now fit", "Only now, with every issuer… accepted, log…") — describes a runtime condition at a point in execution, not a change from before this PR;
- "unchanged" describing an invariant (a token/response passed through without modification: `bridge.go:1362`, `auth.go:487`, `auth_external_bearer_test.go:869,1624`, `federation_config.go:230`) — not "unchanged relative to before this PR"; **(round 3 found this occurrence's surrounding sentence was itself inaccurate — see the fix round 3 section)**
- "earlier" describing code/loop position, not review history (`auth_external_bearer_test.go:1429,1483,1485`, `federation_auth.go:114`, `google_credential_validator_test.go:403`);
- "moved" inside "no other outcome series moved" (a testing-invariant phrase) and inside "removed" (`main_test.go:117`, substring match);
- `docs-site/.../a2a-bridge.md:358` "Deprecated, superseded by hubBearer" — a real, user-facing deprecation notice, not narration;
- `extras/scion-a2a-bridge/README.md:118` "before this protection was added" / "previously pushed" — real upgrade-path guidance for operators (already reviewed and accepted in fix round 1's own sweep);
- `google_credential_validator_test.go:508` "must not run before this check" — describes assertion ordering inside the test, not a PR-relative claim.

### Known limitation carried forward (not fixed — outside the current authorized-exceptions list)

`pkg/config/federation_config_test.go:556`: the table-test `name:` field `"allowed_projects (the OLD field) on the Google issuer
still errors, and now names allowed_gcp_projects"` feeds `t.Run(tt.name, …)` — it is a subtest name, not a comment and not one of
the `t.Errorf`/`t.Fatalf`/`t.Error`/`t.Fatal`/`t.Logf` format strings the O3 exception covers, and not a trailing comment. Left
unchanged per the strict "no other string literal changes" rule. Flagging for the lead/reviewer, as the trailing-comment case was
in fix round 1, in case this class of edit should also be authorized.

### O1: five PR-relative wording sites fixed

| File:line | Before | After |
|---|---|---|
| `pkg/hub/auth_external_bearer_test.go:741-747` | Orphaned banner: "No package-level mutable state in the new files. (Documented here; ensured by code review: …)", heading an unrelated test | Deleted — the rule is already documented and mechanically enforced by `TestNoPackageLevelMutableState` (`:1655` area) |
| `pkg/hub/google_credential_validator_test.go:546` | "under the unchanged user rule" | "under the user rule" |
| `extras/scion-a2a-bridge/internal/bridge/uatvalidator_test.go:197` | "(since it now handles both types identically)" | "(since it handles both types identically)" |
| `pkg/config/federation_config.go:257-258` (production) | "This is a hard error rather than a startup warning: both fields are new, so no existing config can break." | "This is a hard error rather than a warning, so a misconfiguration fails at startup instead of silently admitting nothing." |
| `pkg/hub/ge_exchange_metrics_test.go:34-35` | "…checks the response bytes/status are exactly what they were before this metric existed…" | "…checks the response bytes/status are byte-identical to the response without a metrics recorder…" |

### O2: `external_bearer_metrics.go:110-114` overstatement fixed

Before: "Each value corresponds to exactly one response … that serveExternalBearer writes" — false for `ok` (served by `next`, no
response written by `serveExternalBearer`) and `not_applicable` (caller writes its own rejection). After (the report's suggested
wording, checked against `externalBearerOutcomes()`'s eight constants and their own docs at `:118-121`): "Each error outcome
corresponds to exactly one response (HTTP status plus error code) that serveExternalBearer writes; ok means the request was served
by the next handler and not_applicable means the caller wrote its usual rejection."

### N1: ragged wraps and dangling word fixed

- `auth_external_bearer.go:274-275` ("silently stripped —" breaking early) — reflowed.
- `auth_external_bearer_access_token_test.go:395-396` ("A units error (e.g." breaking early) — reflowed.
- `ge_exchange_test.go:2270-2271` ("technique as") and `:699-706` ("auth_external_bearer_test.go's") — reflowed as far as the long
  identifier names allow.
- `external_bearer_metrics_test.go:599-603` — dangling "used" after "by this design" was removed in fix round 1; now "Every label
  value is one of the named constants…".

### N2: project-log accuracy fixed

- The comments-only-proof paragraph said "Prints 14 lines: 13 are … format-string changes … (the O3 literals plus the R1/R5/R6
  literal corrections)". Corrected: the command prints **28** lines (14 −/+ pairs), all 13 literal pairs are the O3 set, and R1/R5/R6
  were comment-only edits that do not appear in that proof's output.
- Added "(superseded; see ap-em ruling further below)" markers to the O2 table row and the "Known limitation" heading, since the
  ap-em ruling addendum at the end of the fix-round-1 section already fixed that item.

### Comments-only proof (fix round 2)

```
$ git diff 88b0a1a32 -- '*.go' | /usr/bin/grep -E '^[+-][^+-]' | /usr/bin/grep -vE '^[+-]\s*(//|$)'
```
Empty. This round made zero test-literal, trailing-comment, or test-rename edits — every change is a whole-line comment (or a
markdown/doc-comment line outside `.go` files). Consequently no test needed re-running for a message/name change this round; the
targeted-test gate has nothing new to target.

### Gates (fix round 2)

- `gofmt -l pkg cmd extras` — empty.
- `go build ./...` — clean (both the main module and, separately, `extras/scion-a2a-bridge`).
- `go vet ./pkg/hub/... ./pkg/config/... ./cmd/...` — clean. `go vet ./...` (bridge module) — clean.
- `GOGC=40 golangci-lint run --new-from-rev=a53175c23 --concurrency=1 ./pkg/hub/... ./pkg/config/... ./cmd/...` — `0 issues`. Same
  command with `./...` from `extras/scion-a2a-bridge/` — `0 issues`.
- `go test ./pkg/config/...` — environment-specific; passes with SCION_* unset. Root cause found in fix round 3: this container's
  orchestration harness sets `SCION_AUTO_EXPOSE_PORTS=true` in the ambient environment, which koanf's env-var override binds onto
  the settings schema's `auto_expose_ports` field (a map/struct), causing a decode error in `TestLoadVersionedSettings_*`
  (`v7_fixes_test.go`) — confirmed by toggling just that one variable: `env -u SCION_AUTO_EXPOSE_PORTS go test ./pkg/config/...`
  passes in full, as does unsetting every `SCION_*` variable. Not a fact about the code; not reproducible in an environment
  without that specific variable set. Ran `-run 'TestFederationConfig|TestInvalidDomainEntryReason'` explicitly: all pass
  regardless.
- `go test ./...` from `extras/scion-a2a-bridge/` — all packages pass.
- Bonus (not required this round, since no test name/message changed): a full `go test ./pkg/hub/...` run. It surfaced 4 pre-existing,
  unrelated failures (`TestDEF164_AtAgentSlug_DeliversToAgent`, `TestDEF164_AtAgentSlug_DMConversationCreated`,
  `TestDEF152_AgentToAgentDM_DeliversViaOutbound`, `TestCreateTemplateV2_ScopeIDInjectionBlocked`), all in files this branch never
  touches (`handlers_outbound_def142_test.go`, `handlers_outbound_def152_test.go`, `handlers_user_templates_test.go`).
- Byte hazard (`perl -ne 'print "$ARGV:$.\n" if /\xC2\xA0/'` over every file changed since `a53175c23`) — empty.
- Bare refs — both commands empty.

## Fix round 3

Review: `reviews/polish-r3-ap-polish-rev-3.md` (`ap-polish-rev-3`, REQUEST CHANGES on `ad4eda749`: 0C / 2R / 3O / 2N / 5FYI). Both
Required findings were comments introduced in fix round 2 using wording the round-2 report itself suggested — neither the
round-2 report author nor this agent checked that wording against the code before committing. Lead ruling (23:20, then
`ap-em`'s addendum): subtest/table-case `name:`/`t.Run` renames are authorized when nothing references the name.

**Main lesson applied this round:** every suggested wording below (the report's included) was verified against the actual code
before being committed — the specific lines read are named in each row.

### R1: false claim about golden case (a)'s wiring — fixed

| File:line | Before (false) | After | Code lines verified |
|---|---|---|---|
| `pkg/hub/auth_external_bearer_test.go:571-580` | "Golden case (a) … pins the exact bytes with GoogleValidator == nil … not just with GoogleValidator == nil like golden case (a). Otherwise the nil-guard … could mask a bug in the googleTrust gate itself." | "…even for a validly signed Google ID token. Golden case (a) … pins the same fallback bytes for a malformed JWT; this test uses a real Google-signed token, in production shape (GoogleValidator and GoogleResolver wired, as New() always builds them; FederationAuth pointer empty), so a googleTrust gate that let a Google-shaped token through would reach the validator and fail the zero-calls assertion below." | `:1268` (token constant `nonHubJWT = "not-a.valid-hub.jwt"`), `:1286-1299` (golden case (a)'s table entry: `withTrust: false`, `token: nonHubJWT`), `:1388-1407` (the shared `cfg` built with `GoogleValidator: counting, GoogleResolver: resolver` for every case including (a)); `:612-614` (this test's token: `signIDToken(kp, claims)` with `claims["iss"] = googleIssuerHTTPS` from `validIDTokenClaims()`, `google_credential_validator_test.go:109-122`); `:631-632` (the `counting.totalCalls() != 0` assertion); `server.go:1036` (`New`'s signature, confirming it is the constructor that always builds both) |

Also updated the fix-round-2 table row above (this log, "R2: six … sites fixed" table) to flag that its "After" text was itself
false, and corrected "`auth.go:484`" to "`auth.go:487`" in the fix-round-2 history-word survivor list (F5).

### R2: `ge_exchange_metrics_test.go` file banner overclaimed byte-identity checking — fixed

| File:line | Before (false) | After | Code lines verified |
|---|---|---|---|
| `pkg/hub/ge_exchange_metrics_test.go:33-36` | "…every test also checks the response bytes/status are byte-identical to the response without a metrics recorder…" | "…and checking the response status. TestGEExchangeMetrics_ResponseShapeUnaffected compares the success response with and without a recorder, and the TestGEExchange_*_ExactBytes tests pin the exact bytes: recording must never change the exchange response." | `:70-88` (`TestGEExchangeMetrics_OK`: asserts only `w.Code != http.StatusOK`, no second server, no byte comparison); `:292-308` (`_NilRecorder_NoPanic`: runs without a recorder only, status-only assertion); `:318-355` (`_ResponseShapeUnaffected`: the only with/without comparison, uses `reflect.DeepEqual` after masking `AccessToken`/`ExpiresAt`/`UpstreamExpiresAt` — not byte-identical); confirmed the other 8 status-only tests (`_NotConfigured`, `_InvalidRequest_*`, `_BadRequest_*`, `_InvalidCredential_*`, `_Forbidden_*`, `_ExchangeFailed_*`, `_RateLimited`) via `grep -n '^func Test'` |

### O1: `auth_external_bearer.go:43-44` PR-relative "today" — fixed

"…it never changes the outcome of a request that authenticates **today**…" → "…it never changes the outcome of a request this
path cannot vouch for…" (dropped "today"; avoided the report's suggested wording's repeated "vouch for" clause). Verified against
`errExternalBearerNotApplicable`'s existing, already-accurate doc a few lines below and the "original, byte-identical rejection"
claim already established in earlier rounds.

### O2: `pkg/hub/auth.go:485-487` hook-site comment — fixed (branch-level, outside the sweep delta, ships in the same PR)

Before: "otherwise (or on failure) it returns false/an error and the original 'unrecognized token format' rejection below is
unchanged." False on two counts: `serveExternalBearer` returns only a `bool`, never an error, and on verification *failure* it
writes its own 401 and returns `true` — the "unrecognized token format" rejection is not reached in that case.

After: "serveExternalBearer returns true whenever it has fully handled the request (Google trust configured, whether validation
succeeds or fails), so this arm returns and the 'unrecognized token format' rejection below never runs. It returns false only
when the token is not applicable to this path at all (e.g. no Google trust configured), in which case that rejection runs as
usual." (Diverged from the report's suggested "writes its own response on success or failure" — on success `serveExternalBearer`
calls `next.ServeHTTP` rather than writing a response itself, so the rewrite states the return-value contract instead.)

Code lines verified: `auth_external_bearer.go:293-294` (`func serveExternalBearer(...) bool`), `:302-306` (only the
`errExternalBearerNotApplicable` case returns `false`), `:343-352` (the default/failure case writes 401 and returns `true`),
`:363-365` (the success case calls `next.ServeHTTP` and returns `true`).

### O3: `pkg/config/federation_config.go:257-258` "fails at startup" — fixed to cover all three load paths

After: "…so a misconfiguration is rejected when the config is loaded (startup fails; an admin save or hot reload is refused)
instead of silently admitting nothing." Code lines verified: `server.go:1544-1552` (`New` returns an error when
`NewFederationAuthenticator` fails — startup); `admin_settings_db.go:551-558` (`Validate()` failure → `writeError(...,
http.StatusUnprocessableEntity, ...)` — admin save, 422); `operational_settings.go:1129-1132` (`Validate()` failure →
`slog.Error(..., "keeping old config")`, no swap — hot reload refused, old config kept).

### N1: three ragged wraps — fixed

- `ge_exchange_route_test.go:358-360` ("decorator (re-validates on" breaking early) — reflowed.
- `google_credential_validator_test.go:546-547` ("under the user rule." / "If the SA/user branches…" — short line mid-paragraph) — reflowed.
- `auth_external_bearer.go:277-278` ("matching" / "containsFold's…" breaking early) — reflowed.

### N2: "reverted" implying a prior state — fixed

- `extras/scion-a2a-bridge/internal/bridge/uatvalidator_test.go:196`: "reverted to always" → "changed to always".
- `extras/scion-a2a-bridge/internal/bridge/v0_compat_test.go:768`: "reverted to \"uat\" alone" → "changed to \"uat\" alone".

### F1: subtest-name rename, authorized by lead ruling — fixed, plus a branch-wide sweep

`pkg/config/federation_config_test.go:556`: renamed the `name:` field from `"allowed_projects (the OLD field) on the Google
issuer still errors, and now names allowed_gcp_projects"` to `"allowed_projects on a Google issuer errors and names
allowed_gcp_projects"`. Verified nothing references the old name: `grep -rn "OLD field"` and `grep -rn` for the exact old string
across `.go`, `Makefile`, `.github`, `scripts`, `hack` — no hits outside this one test file (and this log's own historical
record). Ran the renamed subtest: `TestFederationConfig_Validate/allowed_projects_on_a_Google_issuer_errors_and_names_allowed_gcp_projects`
— PASS.

**Branch-wide sweep** (per the lead/ap-em addendum): `git diff a53175c23 HEAD -- '*_test.go' | /usr/bin/grep -nE '^\+.*(name:|t\.Run\()'`
— 75 hits. Triaged every one; only the `:556` row above narrates history (matched by a follow-up `grep -iE
'old|now |review|round|phase|design|finding|previously|earlier|reverted|superseded|unchanged|no longer'` over the 75 hits, which
also flagged two "folds to lower" table-case names as false positives — "fold" contains the substring "old"). Every other `name:`/
`t.Run` string in the 75 is a plain behavioural description (e.g. `"allowed_gcp_projects on the Google issuer (https form) is
valid"`, `"standard SA email"`, `"azp == sub (real metadata-server shape)"`) with no design ID, review reference, or history word.

### F3: project-log gate claim corrected (this log, fix-round-2 section)

The fix-round-2 gates section stated the `TestLoadVersionedSettings_*` `pkg/config` failures were confirmed via `git stash` to
fail "identically on unmodified `88b0a1a32`", asserting environmental causation without identifying the actual cause. The
fix-round-3 review ran the identical command at `ad4eda749` and got a full pass, contradicting that framing. **Root cause found
this round:** this container's orchestration harness sets `SCION_AUTO_EXPOSE_PORTS=true` in the ambient environment, which
collides with the settings schema's `auto_expose_ports` field name via koanf's env-var override, producing the `'auto_expose_ports'
expected a map or struct, got "string"` decode error. Confirmed by toggling only that one variable
(`env -u SCION_AUTO_EXPOSE_PORTS go test ./pkg/config/...` passes in full). The fix-round-2 log text is corrected in place (see
above) rather than left as a now-known-wrong "confirmed via git stash" claim.

### F5: log accuracy — fixed together with R1/O2

- Fix-round-2 log table row for `auth_external_bearer_test.go:571-580` now flags that its "After" text was itself false (see R1
  above), rather than presenting it as settled.
- `auth.go:484` corrected to `auth.go:487` in the fix-round-2 history-word survivor list.

### F2 and F4: no action (per the brief)

F2 (commit-message squash-body concatenation) is being handled by `ap-em`/the lead directly with the person who merges upstream.
F4 ("soak" vocabulary) was already confirmed to stay — it describes the operator-facing `geGoogle`-exchange deletion migration,
not review process.

### Comments-only proof (fix round 3)

```
$ git diff ad4eda749 -- '*.go' | /usr/bin/grep -E '^[+-][^+-]' | /usr/bin/grep -vE '^[+-]\s*(//|$)'
```
Prints exactly one −/+ pair, the authorized subtest-name rename:
```
-			name: "allowed_projects (the OLD field) on the Google issuer still errors, and now names allowed_gcp_projects",
+			name: "allowed_projects on a Google issuer errors and names allowed_gcp_projects",
```
No trailing-comment lines this round (nothing inside `/* */`, either). No other kind of line appears.

### Gates (fix round 3)

- `gofmt -l pkg cmd extras` — empty.
- `go vet ./pkg/hub/... ./pkg/config/... ./cmd/...` — clean. `go vet ./...` (bridge module) — clean.
- `GOGC=40 golangci-lint run --new-from-rev=a53175c23 --concurrency=1 ./pkg/hub/... ./pkg/config/... ./cmd/...` — `0 issues`. Same
  command with `./...` from `extras/scion-a2a-bridge/` — `0 issues`.
- `go test ./pkg/config/...` — passes in full once `SCION_AUTO_EXPOSE_PORTS` (or all `SCION_*`) is unset; see F3 above for the
  root cause found this round. `-run 'TestFederationConfig_Validate'` explicitly, including the renamed subtest: all pass.
- `go test ./...` from `extras/scion-a2a-bridge/` — all packages pass, including `TestUATValidator_TokenTypeClassification` and
  `TestCallerHubClient_BearerTokenType` (N2) run explicitly.
- Targeted `go test -count=1 -run` on every `pkg/hub` test touched this round (`TestExternalBearer_NoTrustProductionShape_Golden401`,
  `TestExternalBearer_ConfiguredTrustInvariant_Golden`, `TestGEExchangeMetrics_OK`, `TestGEExchangeMetrics_ResponseShapeUnaffected`,
  `TestGEExchangeMetrics_NilRecorder_NoPanic`, `TestExternalBearerRateLimiter_DefaultsPinned`,
  `TestGEExchange_Route_SharesValidatorAndResolverWithExternalBearer`,
  `TestProductionValidator_IDToken_UserToken_SAShapedAzpSub_StillRejected`) — all pass.
- Byte hazard (`perl -ne 'print "$ARGV:$.\n" if /\xC2\xA0/'` over every file changed since `a53175c23`) — empty.
- Bare refs — both commands empty.

## Fix round 4

Review: `reviews/polish-r4-ap-polish-rev-4.md` (`ap-polish-rev-4`, **APPROVE** on `e7bf264a0`: 0C / 0R / 1O / 2N / 6FYI). Folding
in the cheap items (O1, N1, N2, F4) so the upstream PR ships with no known comment inaccuracies, per the brief. F1, F2, F5, F6
declined — no action, per the brief (F1/F2: meaning is pinned by surrounding text; F5: squash body, handled by the lead; F6:
environment, already documented as the `SCION_AUTO_EXPOSE_PORTS` root cause in the fix-round-3 section).

### O1: `pkg/hub/auth_external_bearer_test.go:572-580` wording imprecision — fixed

| File:line | Before (imprecise) | After | Code lines checked |
|---|---|---|---|
| `auth_external_bearer_test.go:572-580` | "a validly signed Google ID token" / "a real Google-signed token" / "pins the same fallback bytes" | "a well-formed, signed ID token carrying Google's issuer" / "a Google-issuer token signed with a test key" / "pins the same fallback rejection, byte-exact" / "FederationAuth unset" | `:584` (`kp := newGCVTestKeyPair("test-kid-1")` — a test key pair, not Google's); `:616` (`signIDToken(kp, claims)`, so the token is signed by that test key); `:1268` (golden case (a)'s token constant, `nonHubJWT = "not-a.valid-hub.jwt"`); `:1286-1299` (case (a)'s table entry: `withTrust: false`, `token: nonHubJWT`); `:1398-1407` (the shared `cfg` wiring `GoogleValidator`/`GoogleResolver` non-nil for every case including (a)); `:623-627` (this test's `wantBody` is `"invalid access token: "+verifyErr.Error()`, the same *pattern* case (a) uses at `:1300` but with a different underlying error, so the two tests pin the same fallback shape byte-exactly each, not the same bytes as each other); `:632` (`counting.totalCalls() != 0`); `auth_external_bearer.go:144-149` (`classifyExternalBearer`: a JWT with Google's `iss` classifies as `externalBearerIDToken`, not not-applicable); `:390-425` (`authenticateExternalBearer`: if `googleTrust` wrongly returned ok, classification would proceed and reach `ValidateIDToken`, so a broken gate would fail the zero-calls assertion). Used "FederationAuth unset" per the brief instead of "FederationAuth pointer empty", since this test leaves the field `nil` rather than pointing it at an empty value (this also resolves F3 in the review). |

Adopted the report's suggested wording ("a Google-issuer token signed with a test key"): the token is signed with the local key
from `newGCVTestKeyPair`, and no JWKS is wired in this test (the validator is the fake `newRejectingCountingValidator()`,
`:590-599`, which must never be called).

**Addendum (ap-em, same day):** the "proof must print nothing" constraint above was relaxed. The trailing comment at `:616`
(`token := signIDToken(kp, claims) // a validly-signed Google token — must still be rejected`) was fixed under the
trailing-comment exception (allowed change kind 2: text after `//` only). New text: `// signed with a test key and carrying
Google's issuer; must still be rejected`. Verified: code before `//` (`token := signIDToken(kp, claims) `) is byte-identical on
both sides of the diff; `kp := newGCVTestKeyPair("test-kid-1")` (`:584`) is a test key, and `validIDTokenClaims()`
(`google_credential_validator_test.go:109-122`) sets `iss: googleIssuerHTTPS`.

**Follow-up sweep (ap-em/lead ruling, same day):** searched the whole branch's added lines for the same false-claim class
(`google-signed|signed by google|google signs|signed google (id token|token)|real google`, plus `validly.signed`). Found and
fixed two more genuine false claims, left two accurate/declined, and confirmed no others exist:
- **Fixed** `auth_external_bearer_test.go:869` (banner above `TestExternalBearer_ServiceAccountFederationIssuer_NotApplicable`):
  "a real Google-signed user ID token" → "a Google-issuer user ID token signed with a test key". Verified: `:874`
  (`kp := newGCVTestKeyPair("test-kid-1")`), `:911` (`token := signIDToken(kp, claims)` — signed with that same `kp`).
- **Fixed** `auth_external_bearer_test.go:917-918` (same test, inline comment): "a valid Google-signed JWT" → "a JWT signed
  with a test key and carrying Google's issuer". Verified against the same `:874`/`:911` lines, plus `:1305,:1319` confirming
  golden cases (b)/(c) exist (`b_trust_configured_wrong_signature_hub_jwt`, `c_trust_configured_non_google_iss`), matching the
  comment's "golden cases (b)/(c)" cross-reference.
- **Declined, per instruction** `auth_external_bearer_test.go:217` ("A valid Google-signed user ID token authenticates…") and
  `:433` ("A valid Google-signed ID token whose aud does not match…"): left unchanged. "Google-signed" here is test parlance
  for a token signed under the stubbed Google JWKS the test wires up and validates against with the real
  `GoogleCredentialValidator` — unlike the three fixed sites above, these tests actually run the token through JWKS-based
  signature verification, so "Google-signed" describes what the *validator* treats as authentic within the test's stubbed
  trust boundary, not a false claim about the real Google service having signed it.
- Also checked `auth_external_bearer_sa_test.go:53` ("claims shaped like a real Google service-account ID token") — describes
  claim *shape*, not signature authenticity; accurate, no action. And the three `validly-signed` hits in
  `google_credential_cache.go`/`_test.go` — generic production/test descriptions of the caching decorator's negative-cache
  policy, not a claim about a specific token's provenance; accurate, no action.

### N1: `pkg/hub/ge_exchange_route_test.go:358-364` — reflowed as one unit

The paragraph had a short line (`// benefits from a cache),`) breaking mid-sentence before `// but it wraps the *same base
validator instance*…`. Reflowed the full six-line paragraph to ~78 columns with no mid-sentence breaks.

### N2: `pkg/config/federation_config.go:233` — PR-relative wording removed

"— unrelated to, and unchanged by, AllowedGCPProjects below." → "— unrelated to AllowedGCPProjects below." (dropped the
"unchanged by" clause entirely, per the brief's "or 'unaffected by', whichever reads as PR-independent" — "unrelated to" alone
already states the invariant without any PR-relative framing).

### F4: project-log citation corrected

Fix-round-3 section, R1 row: corrected `:1388-1407` (previously mislabeled as "golden case (a)'s table entry") — it is the
shared `cfg`. Golden case (a)'s actual table entry is at `:1286-1299`, and its token constant is at `:1268`. Both are now cited
separately in that row.

### Comments-only proof (fix round 4)

```
$ git diff e7bf264a0 -- '*.go' | /usr/bin/grep -E '^[+-][^+-]' | /usr/bin/grep -vE '^[+-]\s*(//|$)'
```
Empty. Every change this round is a whole-line comment edit; no trailing comments, test-literal changes, or renames.

### Gates (fix round 4)

- `gofmt -l pkg cmd extras` — empty.
- Byte hazard (`perl -ne 'print "$ARGV:$.\n" if /\xC2\xA0/'` over every file changed since `e7bf264a0`) — empty.
- Bare refs — both commands empty (commit messages and added diff lines).
- Targeted `go test -run 'TestExternalBearer_NoTrustProductionShape_Golden401|TestExternalBearer_ConfiguredTrustInvariant_Golden'`
  and `-run 'TestGEExchange_Route_SharesValidatorAndResolverWithExternalBearer'` (the test at the reflowed banner) — all pass.
- `go test -run 'TestFederationConfig_Validate' ./pkg/config/...` with every `SCION_*` variable unset — all pass, including the
  round-3-renamed subtest.

## Fix round 5

Review: `reviews/polish-r5-ap-polish-rev-5.md` (`ap-polish-rev-5`, **APPROVE** on `4f90a60db`: 0C / 0R / 1O / 1N / 4FYI). Folding
in the Nit and Optional items, plus FYI 1's citation fixes, per the brief.

### Nit: two ragged paragraphs, same class as fix-round-4 N1 — fixed

| File:line | Issue | Fix | Word-for-word check |
|---|---|---|---|
| `pkg/config/federation_config.go:231-237` | `:234` (`// below. It stays an error on every non-hub`) about 46 columns, ragged after the fix-round-4 N2 edit | Reflowed `:231-237` as one unit | Extracted the comment text before and after with the `//` prefixes stripped and joined on spaces; identical string both times |
| `pkg/hub/auth_external_bearer_test.go:868-872` | `:871` (`// through unchanged when Google is`) about 36 columns, ragged after the fix-round-4 `:869` edit | Reflowed `:868-872` as one unit | Same word-for-word check; identical string both times |

Both are whole comment-line edits only — no trailing comments, so the proof over `4f90a60db..HEAD` stays empty.

### Optional: two false sentences in the "Fix round 4" O1 note — fixed

The two sentences following the O1 table (this log) were both false:
- Claimed the fix-round-4 report's suggested wording was "real Google-signed token". Re-read the round-4 report's suggested
  block (quoted in this log's own "Fix round 4" section, `briefs/ap-polish-dev-fix-r4.md` review): it reads "this test uses a
  Google-issuer token signed with a test key" — that developer adopted this wording as committed. "Real Google-signed" was the
  *old, pre-round-4* code wording, not the round-4 report's suggestion.
- Claimed "Google never signs this token — only the JWKS the test wires does". False on two counts: a JWKS holds public keys
  and signs nothing (it is used for verification, not signing), and `TestExternalBearer_NoTrustProductionShape_Golden401`
  wires **no** JWKS at all — it uses the fake `newRejectingCountingValidator()` (`:590-599`), which must never be called. The
  token is signed with the local test key from `newGCVTestKeyPair` (`:584`), full stop.

Replaced with the reviewer's suggested sentence, verified: "Adopted the report's suggested wording ('a Google-issuer token
signed with a test key'): the token is signed with the local key from `newGCVTestKeyPair`, and no JWKS is wired in this test."
(Extended slightly to name the fake validator and its line range for traceability.)

### FYI 1: O1 row citations corrected to HEAD lines

The "Fix round 4" O1 table row cited `:583`, `:615` and `:631`, which were accurate `@e7bf264a0` but drifted to `:584`, `:616`
and `:632` once the fix-round-3 banner rewrite and the fix-round-4 `:869` edit each added one line above them (two lines
total). Corrected all three to the HEAD values. Also corrected `:1296` (cited as where case (a) builds its `wantBody`) — verified
this is wrong at any point: `@e7bf264a0` that line is `t.Fatal("test token must be invalid")`, and the actual
`return wantErrorBody(...)` call is two lines later. Accounting for the same two-line drift, that line is `:1300` at HEAD (not
`:1298`, which is `@e7bf264a0`'s value for that same call, not HEAD's — confirmed by direct inspection of both revisions
rather than assuming the review's suggested `:1298`).

### Comments-only proof (fix round 5)

```
$ git diff 4f90a60db -- '*.go' | /usr/bin/grep -E '^[+-][^+-]' | /usr/bin/grep -vE '^[+-]\s*(//|$)'
```
Empty. Every change this round is a whole-line comment edit; no trailing comments, test-literal changes, or renames. The
project-log edits (the false-sentence replacement and the citation fixes) are markdown, not Go, and not part of this proof.

### Gates (fix round 5)

- `gofmt -l pkg cmd extras` — empty.
- Byte hazard (`perl -ne 'print "$ARGV:$.\n" if /\xC2\xA0/'` over every file changed since `4f90a60db`) — empty.
- Bare refs — both commands empty (commit messages and added diff lines).

## Upstream round 1 (GoogleCloudPlatform/scion#1880 feedback)

Base for this round: `a3ad85e6b` (this branch's own tip after the polish sweep, itself built on `a53175c23`). PR head at the
time feedback arrived: `46a9ea618`. Working head after this round: `39e49136f`.

Brief: `briefs/ap-polish-dev-upstream-r1.md`. Spec: `upstream-1880-r1-dispositions.md`.

Mid-round note: the container restarted once (exit 255) partway through this round. `git status`/`git log
origin/scion/auth-passthrough..HEAD` after the restart showed one committed-but-unpushed commit (`a3ad85e6b`, §1) and all of
§3's G1-G5 guard+test code intact but uncommitted in the working tree — nothing was lost. Two `/tmp` git worktrees used for
§2 had gone stale (pointed at a wiped `/tmp`) and were cleaned up with `git worktree prune`; the container-local Postgres
install was wiped and reinstalled. §2's evidence files, which had been written under `/tmp`, were lost and are redone below,
this time under `/scion-volumes/scratchpad/projects/auth-passthrough/upstream-r1-s2/` so a second crash can't erase them.

### §1: lint — `external_bearer_ratelimit_test.go` missing the sqlite build tag

Added `//go:build !no_sqlite` to `pkg/hub/external_bearer_ratelimit_test.go` (matching sibling `ge_exchange_route_test.go`),
right after the license header, before `package hub`. The file's `TestServer_ExternalBearerRateLimiter_CleanupRunsInBackground`
calls `newTestStore(":memory:")` (`teststore_test.go:39`, itself gated `!no_sqlite`), which is what `go vet -tags no_sqlite
./...` failed on.

Audited every test file this branch adds or changes under `pkg/` and `cmd/` (`git diff --name-only a53175c23 HEAD -- '*_test.go'`
intersected with pkg/cmd) for calls to sqlite-only helpers (defined exclusively in files carrying `!no_sqlite`, `newTestStore`
foremost among them). `external_bearer_ratelimit_test.go` was the only file missing the tag; `ge_exchange_route_test.go`
already had it. Commit: `a3ad85e6b`.

Gate: `make lint` (`go vet -tags no_sqlite ./...`) and `go test -tags no_sqlite ./pkg/hub/... ./pkg/config/...` both pass at
the final head (see "Gates (final head)" below).

### §3: Gemini nil-check guards G1-G5

Each guard follows the same shape: the base `GoogleCredentialValidator` contract is "non-nil identity whenever err is nil";
a validator that breaks this contract by returning `(nil, nil)` must not panic and must not be treated as a successful or a
401-rejected verification. Each was implemented, given a test, and mutation-checked (guard removed, test re-run to confirm
it now fails/panics, guard restored) before committing.

| # | Site | Change | Test | Mutation-check |
|---|---|---|---|---|
| G1 | `auth_external_bearer.go:439` (`authenticateExternalBearer`, after the validate switch) | `if id == nil` → wrap `ErrGoogleUpstreamError`, so `serveExternalBearer`'s outcome switch maps it to 503 `upstream_unavailable` / `ExternalBearerOutcomeUpstreamError`, never 401 | `TestExternalBearer_AccessToken_NilIdentityFromValidator_ServiceUnavailable` (`auth_external_bearer_access_token_test.go`): zero-value `fakeGoogleValidator{}` (returns `(nil,nil)`) → asserts 503, body `upstream_unavailable`/"external identity provider unavailable", one metric call with outcome `ExternalBearerOutcomeUpstreamError` | Removed the guard → `panic: invalid memory address or nil pointer dereference` at `auth_external_bearer.go:439` (`id.IsServiceAccount`), confirmed via `go test -v`. Restored; test passes again with a clean 503. |
| G2 | `ge_exchange.go:196` (`Exchange`, Step 1.5, before the SA-rejection check) | `if identity == nil` → 500, `fmt.Errorf("credential validation failed")` (the same generic message the existing default arm of the err-classification switch already uses) | `TestGEExchange_NilIdentityFromValidator_InternalError` (`ge_exchange_test.go`): zero-value `fakeGoogleValidator{}` → asserts 500, the generic message, and that no user/binding is created | Removed the guard → `panic: invalid memory address or nil pointer dereference` at `ge_exchange.go:200` (`identity.IsServiceAccount`). Restored; re-ran `TestGEExchange_NilIdentityFromValidator_InternalError` plus all four `TestGEExchange_.*ExactBytes` golden tests together — all pass, byte-identical for well-formed input. |
| G3 | `google_credential_cache.go:250` (`store`, before the `err == nil` TTL branch) | `case err == nil && identity == nil:` → `return` without caching (added ahead of the existing `case err == nil:` in the switch, since first-match wins) | `TestGoogleCredentialCache_NilIdentityNilErrorNeverCached` (`google_credential_cache_test.go`): `countingBaseValidator{}` zero value (returns `(nil,nil)`) called 3×, asserts identity/err are both nil each time and the base validator was called 3 times (i.e. never served from cache) | Removed the guard → `panic: invalid memory address or nil pointer dereference` inside `singleflight.Group.doCall` at `google_credential_cache.go:325` (`identity.UpstreamExpiry.Sub(...)`). Restored; re-ran the new test plus the full `TestGoogleCredentialCache_*` suite (20 tests) — all pass. |
| G4 | `google_identity_resolver.go:120` (`Resolve`, before `canonicalizeGoogleIssuer(identity.Issuer)`) | `if identity == nil` → generic error (`"google identity resolver: no identity to resolve"`); both callers already map an unrecognized `Resolve` error to 5xx — `ge_exchange.go`'s default switch arm (500) and `auth_external_bearer.go`'s `classifyResolveError` (unconditionally wraps `errExternalBearerResolveFailed`, which `serveExternalBearer` maps to 503 `store_error`, never the 401 default) | New file `google_identity_resolver_test.go`: `TestGoogleIdentityResolver_Resolve_NilIdentity_ReturnsError` calls `Resolve(ctx, nil, ResolvePolicy{})` directly and asserts a non-nil error and nil user | Removed the guard → `panic: invalid memory address or nil pointer dereference` at `google_identity_resolver.go:120` (`identity.Issuer`). Restored; test passes, and the G1/G2 end-to-end tests (which exercise the two real callers) still pass unchanged. |
| G5 | `otel_external_bearer_metrics.go:70` (`NewOTelExternalBearerMetrics`, before `mp.Meter(...)`) | `if mp == nil` → error (`"otel external bearer metrics: nil MeterProvider"`); `cmd/server_foreground.go:337-344` already treats any error from this constructor as non-fatal (`log.Printf("WARNING: ...")`, then skips wiring `SetExternalBearerMetrics`/`SetGoogleValidatorCacheMetrics`/`SetGEExchangeMetrics`) rather than failing Hub startup | `TestOTelExternalBearer_NilMeterProvider_ReturnsError` (`otel_external_bearer_metrics_test.go`): `NewOTelExternalBearerMetrics(nil, nil)` asserts a non-nil error and nil recorder | Removed the guard → `panic: invalid memory address or nil pointer dereference` at `otel_external_bearer_metrics.go:70` (`mp.Meter(instrumentationScope)`). Restored; test passes, and the existing `TestOTelExternalBearer_NilSnapshotDoesNotPanic` (a real `metric.MeterProvider`, nil `snap`) still passes unchanged. |

Commits: `391264664` (G1), `3175a7db1` (G2), `b75a13214` (G3), `a382f832c` (G4), `39e49136f` (G5) — one commit per guard, per the
brief's suggested grouping.

### §2: `TestCrossReplicaStreamCursor` (extras/scion-a2a-bridge PostgreSQL integration) — pre-existing upstream test defect, not introduced by this branch

**Corrected in fix round 1** (see the "Upstream r1 fix round 1" section below): the original version of this section, as
committed at `b1250499d`, made two false claims and misdescribed the comparison run. Review `ap-up1-rev` caught both and
supplied the actual mechanism, credited throughout. This is the corrected text.

**CI invocation** (read from `.github/workflows/extras-ci.yml`, job `a2a-bridge-postgres-integration`; the trigger is
identical on `HEAD` and on upstream `main`): a `postgres:15` service container (`POSTGRES_USER=scion`,
`POSTGRES_PASSWORD=scion`, `POSTGRES_DB=a2a_test`, port 5432), then
`extras/scion-a2a-bridge/scripts/run-integration-ci.sh` with
`TEST_DATABASE_URL=postgres://scion:scion@127.0.0.1:5432/a2a_test?sslmode=disable` and `TEST_REQUIRE_DATABASE=1`. That script
fails closed on a missing `psql`/DB, verifies connectivity and a canary sentinel row, then runs three phases against
`./integration`: Phase 1 `go test -v ./integration`, Phase 2 `go test -race ./integration`, Phase 3
`go test -count=3 ./integration`, and finally re-verifies the canary row is untouched. The branch's own CI failure
(job `107900942304`, `head_sha=46a9ea618`) was in **Phase 1** (`go test -v ./integration`), not Phase 3.

**The failing check** is `ha_final_process_test.go:641`, `assertNoSSE` after `TASK_STATE_COMPLETED`, which receives an
unexpected extra `kind=artifact-update` event. Line `:550` is not the assertion — it is the `t.Cleanup` evidence logger,
`logCursorFailureEvidence`, whose snapshot is used as forensic evidence below, not as the failure site itself.

**Mechanism** (identified by `ap-up1-rev`'s review, verified independently against the cited lines):
1. The test calls `publishBrokerMessage(..., "cursor-final", TypeAssistantReply, "final once")` **twice**, back to back,
   with the same `msgId` (`ha_final_process_test.go:630-631`).
2. `publishBrokerMessage` stamps `Timestamp: time.Now().UTC().Format(time.RFC3339)`, which has **one-second resolution**
   (`ha_final_process_test.go` around `:264`).
3. In `Bridge.HandleBrokerMessage` → `processAndAppendEvent` (`internal/bridge/bridge.go:1175`), the *message* event is
   deduplicated on `msgId` — so the duplicate message event is correctly dropped. The *artifact* event, however, is
   deduplicated on `dedupKey + ":artifact:" + art.ArtifactID` (`bridge.go:1275`), and `ArtifactID` is
   `deterministicID(msg.Timestamp|msg.Msg)` (`internal/bridge/translate.go:33-40,192`) — a hash that includes the
   second-resolution `Timestamp`.
4. When the two publishes happen to straddle a wall-clock second boundary, the second call's `Timestamp` differs from the
   first's, so its artifact gets a **different** `ArtifactID` and dedup key: a second, un-deduplicated artifact event is
   appended after COMPLETED and dispatched to the SSE stream — exactly the extra event `assertNoSSE` catches.
5. The failure probability per test instance is roughly the gap between the two publishes divided by one second, which is
   why the defect is intermittent and appears more often under repetition (`-count=3`, Phase 3).

**Evidence for the mechanism**: the `:550` cleanup-evidence snapshot shows **two** `cursor-final:artifact` rows in the
task's events table (the message row is correctly singular) in every branch failure captured. Three of these pairs
straddle a wall-clock second boundary cleanly, by `created_at`: this branch's own CI job `107900942304`
(`01:07:10.88482` → `01:07:11.045824`), this developer's local Phase-3 failure (`02:35:50.951593` → `02:35:51.042694`),
and reviewer `ap-up1-rev`'s local Phase-3 failure `L1-run2` (`03:33:40.945863` → `03:33:41.074121`). A fourth local
failure (`ap-up1-rev`'s `run5`, `03:14:58.000430` and `03:14:58.005466`) does **not** straddle a second — both
timestamps fall in the same second, ~5 ms apart, the first only 0.4 ms after the boundary — and is consistent with the
first publish having been stamped in the previous second, which is inference, not something this evidence shows
directly.

**Reachability from this branch's changes: none.** `git diff a53175c23...HEAD -- extras/scion-a2a-bridge` shows
`translate.go`, `processAndAppendEvent`, `HandleBrokerMessage` and all of `internal/state` are unchanged. The only
`bridge.go` change adds `"bearer"` to `callerHubClient`, the path for caller-initiated Hub calls; the HA topology this test
exercises runs `serveHABridgeProcess` with `Scheme: "geGoogle"`, so that arm is never reached. `EffectiveAuthScheme`/
`ApplyOverlay` run only on admin-overlay pushes, which the HA test never sends. `server.go`'s `hubBearer` arms and
`uatvalidator.go` are both unreachable under `geGoogle`. None of the code the mechanism above depends on is touched by this
branch.

**The `integration/` directory is not byte-identical between `a53175c23` and this branch** — an earlier version of this
section claimed otherwise, which was false. Two commits in this branch's history (since squashed into
`GoogleCloudPlatform/scion#1880`, merged upstream as `6ba3730a3`) changed `integration/auth_transport_process_test.go`
(`git diff --numstat a53175c23 b1250499d`: +124/-8 lines: added `TestHubBearerProcessPassthrough`, refactored
`serveFullBridgeProcess` into `serveScionA2ABridgeProcess`) and `integration/alternator_harness_test.go` (+2 lines), and
added a Federation trusted-issuer config to `serveHubProcess`, which the HA topology's `hub` process also uses.
`ha_final_process_test.go` itself — the file containing the defective dedup logic and the test's publish/timestamp code —
**is** byte-identical on both sides; only that narrower claim holds. The wider diff can affect process-startup timing
(and therefore the publish-gap failure *rate*), but it changes none of the code the defect mechanism above depends on, so
it cannot be the cause of the defect itself.

**Run 35411542799 is upstream PR `GoogleCloudPlatform/scion#1748`'s pre-merge draft run**, not an unrelated fork branch — an
earlier version of this section misdescribed it as such, and garbled one citation in the process. PR `GoogleCloudPlatform/scion#1748` (merged
2026-09-19 as `c56bed940`, an ancestor of `a53175c23`) is the PR that **introduced** `ha_final_process_test.go`,
`run-integration-ci.sh`, and the extras-ci Postgres job in the first place. Run `35411542799` (job `105811994668`) is that
PR's own CI run against its pre-merge draft, and it failed on the same `assertNoSSE` assertion after the same double
`cursor-final` publish, with the same second-resolution `Timestamp` — in Phase 3. It contains none of this branch's code:
`adminoverlay.go`, `server.go` and `uatvalidator.go` at that commit have zero matches for
`EffectiveAuthScheme|hubBearer|AuthSchemeWarnDedupe`, and this branch's first hubBearer commit is dated 2026-09-24, five
days after this run.

**Run counts**: this branch **3/35** (this developer's 1/5 full-script runs — `go test -v ./integration`, then
`-race ./integration`, then `-count=3 ./integration` in sequence, with the one failure landing in the Phase-3 leg — plus
`ap-up1-rev`'s independent 2/30 Phase-3-alone runs), upstream-main **0/37** (this developer's 0/5 full-script runs plus
`ap-up1-rev`'s 0/32 Phase-3-alone runs; Postgres 15 local, CI DSN shape, `TEST_REQUIRE_DATABASE=1`, fresh database per run
in both samples). The rates are not equal, and this log does not claim they are: per the mechanism, the per-instance
failure rate is set by the wall-clock gap between the two `publishBrokerMessage` calls, i.e. by process-startup timing,
and this branch's Hub process changes (new `pkg/hub` auth code, the Federation config `serveHubProcess` gained (now part
of `6ba3730a3`), or `TestHubBearerProcessPassthrough` running earlier in the same test binary) could plausibly widen
that gap — this was not measured, and attributing the exact rate difference is out of scope here. Upstream-main itself
never failed locally in either sample, so there is no upstream-main-side artifact-row evidence to compare against; the
only upstream-side failure instance is `GoogleCloudPlatform/scion#1748`'s pre-merge run above, and that run's commit
predates the evidence logger (`logCursorFailureEvidence` does not exist at that commit) with the unexpected-SSE payload
truncated, so it carries no artifact-row evidence either way. What makes this a **pre-existing defect** rather than
something this branch introduced is the failure signature this branch's failures share with that pre-merge run — the
same `assertNoSSE` assertion following the same double `cursor-final` publish with the same second-resolution
`Timestamp` stamping — not the double-artifact-row evidence itself, which was observed only in this branch's own
failures.

**Conclusion**: `TestCrossReplicaStreamCursor` has a pre-existing defect in `internal/bridge`'s artifact dedup key
(`deterministicID` over a one-second-resolution `Timestamp`, `bridge.go:1275`/`translate.go:33-40,192`), already present
and already observed failing in upstream PR `GoogleCloudPlatform/scion#1748`'s own pre-merge CI run before this branch
existed. None of the code this defect depends on is reachable from this branch's changes. **No code change made for
§2** — per the disposition, this is recorded as evidence for the lead to relay to ptone, and the defect itself is out of
scope for `GoogleCloudPlatform/scion#1880`; fixing it is a separate follow-up the lead can offer upstream. Do not
change upstream's test.

### Gates (final head `39e49136f`)

- `go build ./...` — clean.
- `make lint` (`go vet -tags no_sqlite ./...`) — clean.
- `go vet ./pkg/... ./cmd/...` — clean.
- `gofmt -l pkg cmd extras` — empty.
- `GOGC=40 golangci-lint run --new-from-rev=a53175c23 --concurrency=1 ./pkg/hub/... ./pkg/config/...` — 0 issues.
- `go test -tags no_sqlite -count=1 ./pkg/hub/... ./pkg/config/...` — all packages pass.
- Targeted tests for every test added/touched this round, plus all four `TestGEExchange_.*ExactBytes` — all pass (see the
  G1-G5 table above for the per-guard mutation-check detail).
- `extras/scion-a2a-bridge`: `go build ./...` and `go test -count=1 ./...` — all packages pass (`cmd/scion-a2a-bridge`,
  `integration`, `internal/bridge`, `internal/state`).
- Full `pkg/hub` run, `go test -count=1 -timeout 45m ./pkg/hub/...`, at this round's final head (588s, well under the 45m
  timeout): exactly the four documented baseline failures and nothing else — `TestDEF164_AtAgentSlug_DeliversToAgent`,
  `TestDEF164_AtAgentSlug_DMConversationCreated`, `TestDEF152_AgentToAgentDM_DeliversViaOutbound`,
  `TestCreateTemplateV2_ScopeIDInjectionBlocked`. All sibling packages (`pkg/hub/auth`, `authzop`, `githubapp`,
  `imagecheck`, `permissions`) pass. Full output: `upstream-r1-s2/full-pkg-hub-run-39e49136f.log`.
- Hygiene: `git log a53175c23..HEAD --format=%B | /usr/bin/grep -nE '(^|[^/A-Za-z])#[0-9]+'` — empty.
  `git diff a53175c23 HEAD | /usr/bin/grep -nE '^\+.*(^|[^/A-Za-z0-9])#[0-9]{3,}'` — empty. Perl NBSP check over every file
  changed since `a53175c23` — empty.

## Upstream rebase 1 (GoogleCloudPlatform/scion#1880 not mergeable — rebase onto the new upstream main)

Trigger: upstream main moved `a53175c23` → `4726bec5e` (13 commits) while `GoogleCloudPlatform/scion#1880` was in flight, and `git merge-tree` showed
`GoogleCloudPlatform/scion#1880` no longer mergeable. Spec: `upstream-1880-rebase-r1.md` (lead) and `briefs/ap-polish-dev-rebase-r1.md` (ap-em, more
detailed, followed exactly). Pre-rebase head: `b1250499d` (upstream round 1's final head). Post-rebase-and-tests head:
`1a2ee7168`. **Not pushed yet** — held pending ap-em's "PUSH REBASE" signal, since reviewer `ap-up1-rev` was still testing
`b1250499d` when this rebase began. All work below is local only.

### Rebase

`git fetch https://github.com/GoogleCloudPlatform/scion.git main:upstream-main` resolved to `4726bec5e1a40c4adb2925e49ed5f07d705ba971`,
matching the plan. `git rebase upstream-main` hit exactly the one predicted conflict, in `pkg/hub/identity.go`'s `AuthType`
const block: upstream's `AuthTypeSignedURL` (from `GoogleCloudPlatform/scion#1874`) and this branch's `AuthTypeExternalBearer` landed on the same
lines. It surfaced on commit 3 of 115 (`9431f8bf1`, this branch's earliest commit touching `identity.go`) because that is
where `AuthTypeExternalBearer` was first introduced; every later commit in the range replays cleanly once that one conflict
is resolved. Resolved by keeping both constants (order: existing six, then `AuthTypeSignedURL`, then
`AuthTypeExternalBearer`, matching each one's original comment), then `gofmt -w`. `GIT_EDITOR=true git rebase --continue`
completed the remaining 112 commits with no further conflicts.

- Commit count: 115 before (`git log a53175c23..b1250499d --oneline`), 115 after the rebase itself
  (`git log upstream-main..<rebased-head> --oneline`) — no drops, no empty commits. Saved:
  `rebase-r1/commits-before.txt`, `rebase-r1/commits-after.txt` (the latter regenerated after the amend below, so it
  reflects the final 116-commit head — see next section).
- `git range-diff a53175c23..b1250499d upstream-main..HEAD` (saved: `rebase-r1/range-diff.txt`): 114 of 115 commits show
  `=` (byte-identical patch); exactly one shows `!` — commit 3, the `identity.go` resolution, whose only content change is
  keeping both const lines instead of one.

### (a)-(c) verification, with new tests added on top of the rebase

Two new tests were added in one commit on top of the rebased branch (originally `9f16973ba`, amended once — see below — to
`1a2ee7168`), both mutation-checked:

| Item | Verification | Test | Mutation-check |
|---|---|---|---|
| (a) signed-url never reaches serveExternalBearer | `auth.go`'s `UnifiedAuthMiddleware`: Step 3c (`isSignedSkillFileRequest`) only runs inside the `token == ""` branch of Step 3, and `serveExternalBearer` is only called from within Step 4 (the `else` branch) — structurally unreachable from a credential-less request. Confirmed by reading the control flow directly, then pinned with a test. | `TestSignedSkillFileURL_NeverReachesExternalBearer` (new file `auth_signedurl_externalbearer_test.go`): wires a fully-configured external-bearer path (non-nil `GoogleValidator`, `GoogleResolver`, Google trust) behind `UnifiedAuthMiddleware`, sends a request shaped like a signed skill-file capability URL with no `Authorization` header, and asserts `AuthTypeSignedURL` was set, the next handler was reached, the Google validator was called 0 times, and the external-bearer metrics recorded 0 calls. | Temporarily made `extractBearerToken` always return a non-empty forced token (simulating a future bug that routes a credential-less request into Step 4 anyway): the test failed on all three assertions (authType became `external-bearer`, validator called once, one metric call recorded). Reverted; test passes again. |
| (b) no authz bypass for Google-bearer principals | Upstream's `GoogleCloudPlatform/scion#1882` project-workspace authz gate runs per-request regardless of `AuthType`; nothing in this branch's `serveExternalBearer`/`GoogleIdentityResolver` sets any context value the gate treats specially. Verified by direct comparison rather than by reading code alone. | `TestProjectWorkspaceAuthz_ExternalBearerSameDecisionAsHubToken` (new file `project_workspace_externalbearer_authz_test.go`, `!no_sqlite`): binds a member (project owner) and a non-member to Google identities via a pre-existing external-identity binding (bypassing the resolver's email-domain bootstrap policy, which is a separate concern from the authz decision under test), then for each identity issues the *same* workspace-file request once via a normal Hub token and once via a Google bearer token, asserting the two responses carry the same status code (403 non-member, 200 member) and that the non-member response never leaks workspace content. | Swapped which identity backed the Google-bearer call in the non-member subtest (used `aliceIdentity` where `bobIdentity` was expected): the subtest failed (both status-equality and the literal 403 assertion), proving the test is sensitive to which identity is presented, not vacuously passing. Reverted; both subtests pass again. |
| (c) external-bearer 401 golden bytes | Re-ran the existing golden test unmodified at the rebased head. | `TestExternalBearer_NoTrustProductionShape_Golden401` — passes, byte-identical, no test change needed. | n/a (pre-existing test, not touched) |

### Commit-message hygiene fix (amend, unpushed commit only)

The first version of the (a)/(b)/(c) test commit (`9f16973ba`) used unqualified upstream PR numbers in its commit message and in one
test's doc comment, referring to the two upstream PRs by number without qualifying them — a violation of the no-bare-`#NNN`
rule for upstream-bound text, caught by this round's own hygiene grep before anything was pushed. Since `9f16973ba` had not
been pushed anywhere (verified: `origin/scion/auth-passthrough` was still at `b1250499d`, and no other branch or remote
referenced it), it was corrected with `git commit --amend` rather than a follow-up fix commit — the same content, with both
bare references reworded to plain feature descriptions ("the skill-file capability URL feature", "the project-workspace
authorization gate"). New SHA: `1a2ee7168`. Both hygiene greps below are clean against the amended head.

### Gates (rebased + tested head `1a2ee7168`)

- `go build ./...` — clean.
- `make lint` (`go vet -tags no_sqlite ./...`) — clean.
- `gofmt -l pkg cmd extras` — empty.
- `make check-authorization-catalog check-authz-guards check-custom` — all pass (`check-authorization-catalog: all checks
  pass`, `check-authz-guards: analysed 9f16973ba, no violations` — run before the amend, and the amend touched only a test
  doc comment and commit message, not any code the guard scans — `check-conversation-upsert-guard: no violations`,
  `check-security-marker-gates: all gates pass`).
- `GOGC=40 golangci-lint run --new-from-rev=upstream-main --concurrency=1 ./pkg/hub/... ./pkg/config/...` — 0 issues.
- `go test -tags no_sqlite -count=1 ./pkg/hub/... ./pkg/config/...` — all packages pass. `go test -count=1 ./pkg/config/...`
  (sqlite-enabled) — all packages pass.
- `extras/scion-a2a-bridge`: `go build ./...` and `go test -count=1 ./...` — all packages pass.
- Full `pkg/hub` run, `go test -count=1 -timeout 45m ./pkg/hub/...`, compared at two revisions:
  - Rebased head (`9f16973ba`, before the cosmetic amend — the amend changed no `pkg/hub` source or test behaviour): 681s,
    exactly the four documented baseline failures (`TestDEF164_AtAgentSlug_DeliversToAgent`,
    `TestDEF164_AtAgentSlug_DMConversationCreated`, `TestDEF152_AgentToAgentDM_DeliversViaOutbound`,
    `TestCreateTemplateV2_ScopeIDInjectionBlocked`) and nothing else. Full output: `rebase-r1/full-pkg-hub-rebased-9f16973ba.log`.
  - Pure upstream-main tip (`4726bec5e`, via a `/tmp` worktree, removed afterward): 633s, the identical four failures (only
    per-test timings differ) and nothing else. Full output: `rebase-r1/full-pkg-hub-upstream-4726bec5e.log`. Diff of the two
    `--- FAIL` line sets: `rebase-r1/hub-baseline-diff.txt` (test names identical; only durations differ).
  - Conclusion: upstream `4726bec5e` did not change the baseline failure set from `a53175c23`'s, and this branch's rebase
    introduces no new `pkg/hub` failure.
- Hygiene (base `upstream-main`, at the final amended head `1a2ee7168`):
  `git log upstream-main..HEAD --format=%B | /usr/bin/grep -nE '(^|[^/A-Za-z])#[0-9]+'` — empty.
  `git diff upstream-main HEAD | /usr/bin/grep -nE '^\+.*(^|[^/A-Za-z0-9])#[0-9]{3,}'` — empty. Perl NBSP check over every
  file changed since `upstream-main` — empty.

All evidence for this section (commit lists, range-diff, both full `pkg/hub` run logs, the baseline diff) is saved under
`/scion-volumes/scratchpad/projects/auth-passthrough/rebase-r1/`.

**Pushed.** `ap-up1-rev` reviewed `b1250499d` clean (REQUEST CHANGES was doc-only — see "Upstream r1 fix round 1" below),
`ap-em` sent "PUSH REBASE", and this head was pushed with
`git push --force-with-lease=scion/auth-passthrough:b1250499d origin HEAD:scion/auth-passthrough`, confirmed via
`git ls-remote origin scion/auth-passthrough`.

## Upstream r1 fix round 1 (doc-only, on top of the rebase)

Review `ap-up1-rev` (`reviews/up1-r1-ap-up1-rev.md`) verdict on `b1250499d`: REQUEST CHANGES, code clean (§1 and all five
G1-G5 guards independently re-verified, each still killed by mutation), one Required and one Optional finding, both
doc-only. Disposition: `upstream-1880-r1b-dispositions.md` (lead), both ACCEPTed. Applied as one commit on top of the
already-pushed rebase head `74451cc39`, per ap-em's sequencing (rebase first, doc fixes after).

- **R1 (Required):** the "Upstream round 1" section's §2 subsection, above, contained two false statements and one garbled/
  misleading description: the "whole `integration/` package is byte-identical" claim was false (two commits, since
  squashed into `GoogleCloudPlatform/scion#1880`, changed two other files); "reproduces locally on both ... given enough
  repetitions" was false (upstream's local rate was 5/5 pass in this developer's own sample, and 0/32 in the reviewer's
  larger one — never observed failing); and run `35411542799` was mischaracterized as an "unrelated fork branch" when it
  is upstream PR `GoogleCloudPlatform/scion#1748`'s own pre-merge CI run (the PR that introduced the test in the first
  place). The review supplied the actual mechanism (the artifact dedup key's dependence on a one-second-resolution
  `Timestamp`, `bridge.go:1275`/`translate.go:33-40,192`) and the corrected run counts (branch 3/35, upstream 0/37, combining
  this developer's full-script-run counts with `ap-up1-rev`'s Phase-3-alone counts — not both Phase-3-alone, corrected in
  fix round 2 below). §2 above has been rewritten in place around that mechanism, with the false claims removed and the
  run described accurately. The conclusion — pre-existing, not introduced by this branch — is unchanged, but now rests on
  the actual defect and its unreachability from this branch's code, not on a "same rate on both" argument the data never
  supported. Checked the
  "Upstream rebase 1" section (above) for any repeat of the same false claims: none found — its only "byte-identical"
  mentions are about the rebase range-diff and the golden-test bytes, unrelated to the bridge integration directory.
- **O1 (Optional):** `pkg/hub/google_identity_resolver_test.go`'s doc comment claimed
  `TestGEExchange_NilIdentityFromValidator_InternalError` and
  `TestExternalBearer_AccessToken_NilIdentityFromValidator_ServiceUnavailable` provide "end-to-end coverage" of the two
  callers' Resolve-error-to-5xx mapping. False: those two tests exercise the G2/G1 guards, which fire *before* `Resolve` is
  ever called, so neither test reaches it. Reworded to cite the tests that actually exercise that mapping:
  `TestGEExchange_ProvisionNewUser_CreateError_FailsClosed` ("user resolution failed", 500) for the GE-exchange caller, and
  `TestExternalBearer_ResolveInternalError_ServiceUnavailable` plus
  `TestExternalBearer_GetExternalIdentityFault_ServiceUnavailable` (503 `store_error`) for the external-bearer caller.
- F1 (rate asymmetry, no action beyond R1's wording), F2 (G5 typed nil, no action), F3 (G2 message text, no action): no
  code or doc change; the reviewer concurred these need nothing further.

### Comments-only proof

```
$ git diff 74451cc39..HEAD -- '*.go' | /usr/bin/grep -E '^[+-][^+-]' | /usr/bin/grep -vE '^[+-]\s*(//|$)'
```
Empty. The only Go change this round is the one doc-comment rewrite in `google_identity_resolver_test.go`; everything else
is the project log (markdown, not part of this proof).

### Gates

- `gofmt -l pkg cmd extras` — empty.
- `make lint` (`go vet -tags no_sqlite ./...`) — clean.
- `go build ./pkg/hub/...` — clean.
- Targeted: `TestGoogleIdentityResolver_Resolve_NilIdentity_ReturnsError`,
  `TestGEExchange_ProvisionNewUser_CreateError_FailsClosed`, `TestExternalBearer_ResolveInternalError_ServiceUnavailable`,
  `TestExternalBearer_GetExternalIdentityFault_ServiceUnavailable` — all pass.
- Hygiene (base `upstream-main`): commit-body bare-issue-number grep — empty. Diff bare-issue-number grep — empty (this
  round's own first draft of the log text introduced two unqualified issue-number mentions, caught by this same grep and
  fixed before commit by qualifying them as `GoogleCloudPlatform/scion` references, as used throughout this section).
  Perl NBSP check — empty.

Evidence (proof output, hygiene output) saved to `/scion-volumes/scratchpad/projects/auth-passthrough/upstream-r1-fix/`.

## Upstream r1 fix round 2 (doc-only, correcting §2 after merge)

`GoogleCloudPlatform/scion#1880` was merged upstream (squashed as `6ba3730a3`) while a second, independent review
(`ap-up1-rev-2`, `reviews/up1-r2-ap-up1-rev-2.md`, verdict APPROVE) was in progress against the rebase
(`b1250499d..3baf248f0`). That review found the rebase and both new tests fully correct, but flagged five follow-up
inaccuracies in this file's §2 (two Optional, two Nit, one FYI) that shipped in the merged log. Since
`GoogleCloudPlatform/scion#1880` was already merged, this correction is a separate docs-only fork PR against
`ptone/scion`'s `main` (never merged upstream directly, and never touching `GoogleCloudPlatform/scion`), on a fresh
branch off upstream `main`.

Every sentence in §2 and "Upstream rebase 1" above was re-checked against the evidence in `upstream-r1-s2/` (including
`upstream-r1-s2/reviewer/`) and `rebase-r1/` with a command; the full set of check commands and their output is saved to
`/scion-volumes/scratchpad/projects/auth-passthrough/logfix/sentence-checks.log`. One check
(`git merge-base --is-ancestor c56bed940 a53175c23`) could not be answered locally — this clone is shallow
(`git rev-parse --is-shallow-repository` → `true`) and lacks the connecting history — so it was instead verified via
`gh api repos/GoogleCloudPlatform/scion/compare/c56bed940...a53175c23` (`status: ahead, behind_by: 0`, confirming
`c56bed940` **is** an ancestor of `a53175c23`, i.e. the original §2 claim was correct). Every other sentence checked (CI
workflow trigger and phases, the failing assertion and evidence-logger line numbers, the double-publish and RFC3339
lines, the `bridge.go`/`translate.go` dedup-key lines, the `bridge.go` diff being limited to the `"bearer"` case
addition, `serveHABridgeProcess`'s `geGoogle` scheme, `EffectiveAuthScheme`/`hubBearer`/`uatvalidator.go` gating,
`GoogleCloudPlatform/scion#1748`'s merge commit and ancestry, run `35411542799`'s head SHA and PR association, its
Phase-3 failure and zero code matches, this branch's first hubBearer commit date, and the reviewer's `summary.txt` run
tallies) held as originally written.

- **O-1 (Optional):** the "Evidence for the mechanism" paragraph cited `ap-up1-rev`'s `run5` pair
  (`03:14:58.000430`/`03:14:58.005466`) as an example of the artifact rows "straddling a second boundary". They do not —
  both timestamps are in the same second, 5 ms apart (independently re-verified against
  `upstream-r1-s2/reviewer/phase3-b1250499d-run5.log:12,14`). Replaced it with the three pairs that do straddle cleanly:
  this branch's CI job `107900942304`, this developer's own `run3`, and `ap-up1-rev`'s `L1-run2` (all three re-verified
  against their respective logs). `run5` is now described accurately: 0.4 ms after a boundary, consistent with — but not
  proof of — the first publish landing in the previous second. Also stopped calling job `107900942304` an "upstream CI
  job": it is this branch's own CI job, run on upstream's CI infrastructure; the phrase read ambiguously next to
  "upstream-main".
- **O-2 (Optional):** the "Run counts" paragraph's closing sentence claimed both revisions' failures share "the same
  double cursor-final-artifact evidence". They don't: upstream-main itself never failed locally in either sample, so
  there is no upstream-main-side evidence at all, and the only upstream-side failure instance —
  `GoogleCloudPlatform/scion#1748`'s pre-merge run — predates `logCursorFailureEvidence` (it does not exist in that
  commit's version of the test file) and has its unexpected-SSE payload truncated in the CI log, so it carries no
  artifact-row evidence either way. Reworded to say what the evidence does support across both: the same `assertNoSSE`
  assertion following the same double-publish with the same RFC3339 second-resolution stamping, not the
  double-artifact-row detail, which was observed only in this branch's own failures.
- **N2 (Nit):** "+132 lines" for `auth_transport_process_test.go` was the diffstat's total-changed-lines figure, not the
  insertion count. Re-ran `git diff --numstat a53175c23 b1250499d -- .../auth_transport_process_test.go`: +124/-8.
  Corrected.
- **N3 (Nit):** "Run counts (Phase 3 alone, ...)" mislabeled this developer's own 1/5 and 0/5 as Phase-3-alone runs; they
  are full-script runs (`go test -v`, then `-race`, then `-count=3`, each phase in sequence), with the one failure landing
  in the Phase-3 leg. Only `ap-up1-rev`'s 2/30 and 0/32 are genuinely Phase-3-alone. The counts themselves were already
  correct; only the label was wrong. Reworded to say which counts came from which invocation, in both this section and
  the "fix round 1" narrative above (which had repeated the same "Phase-3 samples" label).
- **FYI-3 (no severity, informational):** the log cited the pre-rebase fork SHAs `707f671dd`/`cc9bd9e34` as evidence for
  the `integration/` directory diff. Neither exists on any branch reachable from upstream `main` — they were fork-only
  commits, replaced by different SHAs (`f1cc60bcc`/`665e14386`) after the rebase, and both sets vanished when
  `GoogleCloudPlatform/scion#1880` was squash-merged as `6ba3730a3`. A reader with only an upstream checkout could not
  resolve either. Replaced all three occurrences (the §2 `integration/` and "Run counts" paragraphs, and the "fix round
  1" narrative) with a description of the change plus the squash reference `GoogleCloudPlatform/scion#1880`/`6ba3730a3`,
  which does resolve upstream. (`b1250499d`, cited elsewhere in this file as the pre-rebase head, is unaffected by this
  fix: it is a historical label for what was reviewed, not cited as verifiable evidence, and per `ap-up1-rev-2`'s review
  it remains fetchable by full SHA.)
- N1 (Nit, declined) and the TestCrossReplicaStreamCursor defect itself (declined, separate optional follow-up): out of
  scope for this docs-only pass, per the disposition.

### Diff scope and hygiene

- `git diff --stat upstream-main..HEAD` — exactly one file, `.design/project-log/auth-passthrough-polish-dev.md`.
- Hygiene (base `upstream-main`): commit-body bare-issue-number grep — empty. Diff bare-issue-number grep — empty. Perl
  NBSP check — empty.
- No Go tests: docs-only change, no `.go` file touched.

Evidence (all sentence-check commands and output, the diff --stat, and the hygiene output) saved to
`/scion-volumes/scratchpad/projects/auth-passthrough/logfix/`.
