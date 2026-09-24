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
