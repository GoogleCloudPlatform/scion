# ps-cache-p3-flip — Phase 3 gh:// cache flip

**Agent:** ps-cache-p3-flip
**Date:** 2026-07-29
**Branch:** `scion/ps-cache-p3-flip`
**Base:** `0eec8204` "fix Phase 2 hub-side gh:// cache defects (#901)"

Activates Hub-first routing for `gh://` skill URIs. Phase 3 (#900) landed the
`RegisterFallback` routing machinery but deliberately left `gh://` wired to
`Register` — the local GitHub resolver — because the Hub-side resolver still
had the Phase 2 defects that #901 has since fixed. This flips the switch, and
first clears the one reviewer prerequisite (I-4) that was blocking it.

---

## Changes

### 1. I-4 — fallback inherited the Hub's cancelled context

File: `pkg/agent/routing_skill_resolver.go`

`RoutingSkillResolver` passed the caller's `ctx` unchanged to the scheme
fallback. If a caller set a tight deadline and the Hub call consumed it, the
fallback was invoked with an already-cancelled context and failed instantly.
The fallback existed but could not run in the exact scenario — a slow or hung
Hub — that motivated it.

Both call sites now detach cancellation:

```go
// transport-level retry in Resolve()
sr, err = fb.Resolve(context.WithoutCancel(ctx), schemeRefs, opts)

// per-URI retry in retryErrorsWithFallback()
fr, err := fb.Resolve(context.WithoutCancel(ctx), retryRefs, opts)
```

`context.WithoutCancel` keeps the context's values — logging and tracing
metadata still propagate — and drops only the cancellation signal. The caller's
semantic intent is "resolve this skill", not "resolve it only if the Hub
answers in time".

Note this means a fallback call is no longer bounded by the caller's deadline.
That is the intended trade: the local GitHub resolver has its own HTTP client
timeouts, so the call is bounded, just not by a deadline the Hub has already
burned through.

Covered by `TestRoutingSkillResolver_RegisterFallback_CancelledPrimaryContext`,
which uses a `ctxProbeResolver` double that records the cancellation state and
values of the context it receives, and exercises both call sites with an
already-cancelled caller context.

### 2. The flip

File: `pkg/runtimebroker/handlers.go`, plus new `pkg/runtimebroker/skill_router.go`

`router.Register("gh", ghResolver)` → `router.RegisterFallback("gh", ghResolver)`.
`gh://` URIs now go to the Hub first, so the Hub's DB-backed cache absorbs
repeat resolutions and credentials are minted centrally; the local GitHub
resolver stays wired as the backstop for Hub transport errors and per-URI
failures.

The router construction was extracted from the provisioning handler into
`buildSkillRouter(hub, gh, gcp)`. This was not cosmetic: `RoutingSkillResolver`
keeps its routing tables in unexported fields, so with the wiring inline in a
200-line handler there is no way to assert the routing *policy* without driving
a full provisioning request. A test written against the routing machinery in
`pkg/agent` instead would have re-tested `RegisterFallback` — it would pass
whether or not the broker actually called it, which is precisely the regression
worth guarding. The extraction makes the policy directly assertable.

### 3. Tests

**`pkg/runtimebroker/skill_router_test.go`** — the flip itself:

- `TestBuildSkillRouter_GHRoutesToHubFirst` — `gh://` reaches the Hub and the
  local resolver is not called. Reverting to `Register` fails this test
  (verified).
- `TestBuildSkillRouter_GHFallsBackOnHubPerURIError` — a Hub that cannot
  resolve one URI does not fail the request.
- `TestBuildSkillRouter_GHFallsBackOnHubTransportError` — an unreachable Hub
  degrades to direct GitHub resolution.
- `TestBuildSkillRouter_OtherSchemes` — the schemes the flip must leave alone:
  `skill://` and bare names still route to the Hub, `gcp-skill://` still
  bypasses it.

**`pkg/hub/skill_handlers_gh_cache_test.go`** — the payoff:

- `TestSkillsResolve_GHCacheHitOnSecondResolve` — two identical
  `POST /api/v1/skills/resolve` calls for the same `gh://` URI against a fake
  GitHub `httptest.Server`. The first (a miss) makes two API calls,
  `commits/<ref>` then `contents?ref=<sha>`; the second makes zero and returns
  a byte-identical response.
- `TestSkillsResolve_GHCacheKeyedByURI` — a different skill path misses, so the
  cache is not over-sharing entries between skills.

---

## Notes and deviations

**Call count is 2, not 1, on a miss.** The brief anticipated asserting exactly
one GitHub call. A miss with a symbolic ref costs two: `ghResolveCommitSHA` to
pin the SHA, then `ghListContents` at that SHA. Pinning the request to a full
40-hex SHA would make it one — `ghResolveCommitSHA` short-circuits on an
already-resolved SHA — but that skips the commits endpoint rather than caching
it, and takes the 24h SHA TTL path instead of the 30min symbolic-ref path that
real `gh://owner/repo/skill` references use. The test asserts on the symbolic
ref (miss = 2, hit = 0), which exercises the branch production traffic takes.

**`ghResolutionStore` is nil in test servers.** It is only assigned in
`Server.SetIntegrationHA`, so `testServer(t)` leaves the Hub gh:// path
completely uncached. Without wiring it explicitly the cache test would pass
vacuously — both resolves would hit GitHub and the counter comparison would
still hold at the wrong value. The test assigns
`srv.ghResolutionStore = NewGitHubResolutionStore(enttest.NewClient(t))`.
Confirmed the test fails when that line is removed.

Worth flagging for whoever owns Hub test infrastructure: any future test that
touches Hub-side gh:// caching has the same trap, and the failure mode is a
green test rather than a red one.

**Unrelated gofmt fix.** `pkg/agent/skill_resolver.go` picked up a stray blank
line in `0eec8204` and fails `gofmt -l` on this branch. Folded into commit 1 so
the branch passes CI; it is one line of whitespace and touches nothing else.

---

## Post-review amendments

Two reviewers looked at the branch after the sections above were written. Their
findings changed the shape of the I-4 fix and surfaced a diagnosability gap in
I-3. Both are recorded here so the narrative above is not read as current.

**I-4 has moved on — `WithoutCancel` is no longer unconditional.** Commit
`d236e899` reworked the fix into a `fallbackContext(ctx)` helper and added the
H-1 guard (the Hub declines `?token=` URIs, so they route to the local resolver
that can actually read the named broker secret). The helper detaches only a
context that is *spent* — already cancelled/expired, or with less than
`fallbackMinBudget` (10s) left before its deadline. A healthy context with a
usable budget is inherited unchanged, so the caller's deadline still applies and
a client disconnect mid-fallback still stops the work. The min-budget floor was
added in the polish pass: a context with 5ms left passes `ctx.Err() == nil` but
leaves the fallback an unserviceable sliver, which is the same failure mode as a
spent context one moment earlier.

The statement in section 1 that "a fallback call is no longer bounded by the
caller's deadline" therefore applies **only to the detached path**, and even
there the call is bounded — by `fallbackTimeout` (2 minutes), not just by the
HTTP client's per-request timeouts.

The `fallbackTimeout` comment was also corrected. It previously claimed the
local GitHub resolver makes "at most two calls per skill URI"; in fact
`resolveOne` makes two metadata API calls *plus one raw download per file*, so a
15-file skill is ~17 HTTP requests — and the budget covers the entire retry
batch (up to 50 refs) in a single call, not one URI. Two minutes remains
generous for small batches; large batches could hit the ceiling, and the fix if
that ever bites is to scale the budget by retry-set size rather than raise the
constant.

**I-3 — private-repo credential mismatch is a diagnosability gap.** Post-flip,
a private repo that the project's GitHub App can read but the broker's
`GITHUB_TOKEN` cannot will *resolve* successfully at the Hub and then fail at
install time with a bare 404 on the download, because the raw URLs the Hub
returns are fetched with the broker's credential. The resolve/install split
makes this hard to diagnose: the failure surfaces far from its cause and looks
like a missing file rather than a missing permission. Not a correctness
regression — the request was always going to fail — but the error should say so.
Future work: emit a named-credential hint in the download 404 message,
identifying which credential was used and suggesting the mismatch.

---

## Verification

```
gofmt -l pkg/ cmd/ internal/     clean
go build ./...                   ok
go vet ./pkg/{agent,runtimebroker,hub}/   clean
go test ./pkg/agent/...          ok
go test ./pkg/runtimebroker/...  ok
go test ./pkg/hub/...            ok
```

Both new test groups were checked negatively — the broker test fails if the
wiring reverts to `Register`, the hub test fails if the resolution store is not
wired — so neither is vacuous.
