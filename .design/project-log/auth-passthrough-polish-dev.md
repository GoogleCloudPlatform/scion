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
| `pkg/hub/auth_external_bearer_test.go:571-580` | "This used to be its own `_Golden401` test, but it over-claimed… It's superseded by golden case (a)…" (sits directly above a test named `..._Golden401`) | "Golden case (a) in TestExternalBearer_ConfiguredTrustInvariant_Golden pins the exact bytes with GoogleValidator == nil. Production always builds a non-nil GoogleValidator (only trust gates the path), so this test proves the same no-op in that production shape…" |
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
- "unchanged" describing an invariant (a token/response passed through without modification: `bridge.go:1362`, `auth.go:484`, `auth_external_bearer_test.go:869,1624`, `federation_config.go:230`) — not "unchanged relative to before this PR";
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
- `go test ./pkg/config/...` (SCION_* unset, `GOTMPDIR` outside any checkout) — `pkg/config/opsettings` and
  `pkg/config/templateimport` pass; `pkg/config` itself fails two tests unrelated to this change
  (`TestLoadVersionedSettings_ProjectIDRemapping`, `TestLoadVersionedSettings_GroveIDBackwardCompat`, both a pre-existing
  `auto_expose_ports` decode error in `v7_fixes_test.go`) — confirmed via `git stash` that these fail identically on unmodified
  `88b0a1a32`. Ran `-run 'TestFederationConfig|TestInvalidDomainEntryReason'` explicitly: all pass.
- `go test ./...` from `extras/scion-a2a-bridge/` — all packages pass.
- Bonus (not required this round, since no test name/message changed): a full `go test ./pkg/hub/...` run. It surfaced 4 pre-existing,
  unrelated failures (`TestDEF164_AtAgentSlug_DeliversToAgent`, `TestDEF164_AtAgentSlug_DMConversationCreated`,
  `TestDEF152_AgentToAgentDM_DeliversViaOutbound`, `TestCreateTemplateV2_ScopeIDInjectionBlocked`), all in files this branch never
  touches (`handlers_outbound_def142_test.go`, `handlers_outbound_def152_test.go`, `handlers_user_templates_test.go`).
- Byte hazard (`perl -ne 'print "$ARGV:$.\n" if /\xC2\xA0/'` over every file changed since `a53175c23`) — empty.
- Bare refs — both commands empty.
