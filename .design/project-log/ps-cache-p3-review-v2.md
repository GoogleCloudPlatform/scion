# Phase 3 Code Review v2 — APPROVE

**Branch:** `scion/ps-cache-p3`  
**Commits reviewed:** `1d4775ec` + `fb971fc1`  
**Reviewer:** `ps-cache-p3-review`  
**Date:** 2026-07-29  
**Verdict:** ✅ APPROVE — safe to merge

---

## Summary

Both in-diff conditions from the v1 REQUEST CHANGES are met, and met well.

**BLOCKING-3 fixed:** `retryErrorsWithFallback` now keys the retry set by *ref* (not URI), so every `As` alias of an errored URI reaches the fallback. The new test `TestRoutingSkillResolver_RegisterFallback_SameURIDifferentAliases` is a REAL regression guard — it FAILS against the old resolver (`1d4775e`) and passes against the new one. Not a tautological test.

**handlers.go revert:** Exactly the split recommended in v1. Net production diff vs main is now THREE COMMENT LINES. `RegisterFallback` has zero production callers. BLOCKING-1 and BLOCKING-2 are unreachable via the broker. Merging this is safe.

The `ps-cache-p3-fix` commit message independently identifies the correct reason for keying the merge filter on `retriedURIs` rather than `errored` — that is independent thinking, not patch-application. `echoResolver` (new mock for per-alias testing) was also the right call.

---

## Verification Status

**Tests:** 26/26 pass via EM-container run (`go test ./pkg/agent/... -run 'TestRoutingSkillResolver|TestDetectScheme'` and `go test ./pkg/runtimebroker/...`) and independently verified by reviewer via standalone compilation of the routing resolver against type stubs (go vet clean, gofmt clean, 26/26).

**Note:** The commit message on `fb971fc1` says "NOT VERIFIED" (tests could not run in the developer's full-disk container). The EM container ran the same tests successfully. The contradiction is a record-keeping issue in the commit message — the tests are confirmed green by two independent executions.

---

## Findings

### RESOLVED

- **BLOCKING-1** (hash format mismatch): Deferred — unreachable via broker now that `Register("gh",...)` is back. Tracked in Phase 2b fix workstream (`ps-cache-p2b-fix`).
- **BLOCKING-2** (missing Content field): Same — deferred to Phase 2b.
- **BLOCKING-3** (same-URI alias drop): ✅ Fixed in `fb971fc1`. Confirmed by regression test.
- **I-7** (handlers.go no broker test): ✅ RESOLVED — comment-only change in handlers.go means there is no broker behaviour to test.

### STILL OPEN — Phase 2b priority (LIVE on main regardless of revert)

- **I-3 (IMPORTANT — LIVE):** Hub's `gh://` branch in `/api/v1/skills/resolve` runs `resolveGitHubSkill()` before `CheckAccess` with the caller-supplied `ProjectID`. The registry path has a cross-project access check (`TestSkillAuthz_Resolve_ForbiddenSkill:211`). The `gh://` branch has no equivalent: a caller can POST with a `ProjectID` they do not own and the Hub will mint that project's GitHub App token. The endpoint IS authenticated (nil-identity test exists), but cross-project isolation is unguarded. This is live on main; reverting `handlers.go` changes broker routing, not Hub endpoint behaviour. **Fix before Phase 3 flip, not after.**

### DEFERRED — fix before Phase 3 "flip the switch" PR

- **I-1:** `retryErrorsWithFallback` silently discards primary resolver's `Resolved` entries for URIs that also have errors (contradicts the Hub's spec of returning either Resolved or Error per URI, not both — but fix is defensive).
- **I-2:** `retryErrorsWithFallback` over-retry: if Hub returns the same URI in both `Resolved` and `Errors` (possible if a URI appears twice in the request), the retry path produces 3 resolved entries for 2 refs. Visible (not silent), but worth fixing.
- **I-4:** `ctx` passed to fallback is the same ctx used for the Hub call; a Hub timeout that cancels `ctx` will also immediately fail the fallback. A parent context should be used.
- **I-5:** Primary resolver errors are discarded on retry; no structured log of Hub's original errors before they are replaced.
- **I-6:** Hub may omit a URI from its response entirely (no Resolved, no Error entry); silently dropped.

### NON-BLOCKING (all green to merge now)

- **NB-1:** `retryErrorsWithFallback` over-count when Hub returns URI in both Resolved and Errors (same as I-2 above; visible artefact, not silent failure).
- **NB-2 through NB-6:** Various minor observations per original review.

---

## Entry criteria for Phase 3 "flip the switch" PR

Before `router.Register("gh", ...)` → `router.RegisterFallback("gh", ...)` is re-activated:

1. Phase 2b defects fixed and merged: hash format (BUG-1), expiring URLs (BUG-2), ref-defaulting (BUG-3), cross-project authz (I-3)
2. `retryErrorsWithFallback` I-2 over-retry fixed
3. `ctx` timeout issue (I-4) addressed
4. Broker integration test: Hub success path for `gh://` (currently zero coverage of handlers.go:760)
5. Hub integration test: two sequential resolve calls → 1 GitHub API call (Phase 2 acceptance criterion)

---

## Gemini PR feedback fixes

Applied to `pkg/agent/routing_skill_resolver.go` (branch `scion/ps-cache-p3`).

### 1. Silently omitted refs now trigger the fallback retry (closes I-6)

The fallback retry was gated on `len(sr.Errors) > 0`. If Hub returned a short
result with no matching error entry — a URI dropped outright, or two `As`
aliases of one URI collapsed into a single `ResolvedSkill` — the router
accepted the truncated result and the ref vanished from the install set with no
error surfaced to the caller.

Gate is now `len(sr.Resolved) < len(schemeRefs)`: any shortfall between refs
requested and skills returned triggers the retry, whether or not the primary
bothered to explain itself.

### 2. Retry set keyed by (URI, As), not by URI

`retryErrorsWithFallback` built an `errored map[string]bool` from `sr.Errors`
and retried refs whose URI appeared in it. That could only ever retry refs the
primary explicitly errored on, and treated all aliases of a URI as a unit.

Replaced with a `resolvedRefs map[string]int` built from `sr.Resolved`, keyed by
`refKey(uri, as)` = `uri + "\x00" + as`. Each ref in `schemeRefs` consumes one
matching resolved slot; refs with no slot left are retried. Consequences:

- An alias the primary omitted is retried even when its sibling alias resolved.
- An alias the primary *did* resolve is **not** re-fetched from the fallback
  (avoids the duplicate-work half of I-2/NB-1).
- Counts rather than booleans, so a genuinely duplicated `(URI, As)` pair in the
  request is matched one-for-one instead of collapsing.

Error merging is unchanged in shape: primary errors for URIs that were retried
are superseded by the fallback's outcome; errors for URIs never retried are
preserved. `\x00` cannot occur in a URI or a skill alias, so the key is
unambiguous.

### 3. Log message corrected

`"primary skill resolver reported errors, retrying..."` →
`"primary skill resolver did not resolve all refs, retrying..."`, since the
retry no longer implies the primary reported anything.

### Test coverage

Added `TestRoutingSkillResolver_RegisterFallback_SilentDrop` with three subtests:

| Subtest | Scenario | Asserts |
|---|---|---|
| `ref omitted with no error` | Hub returns 1 of 2 refs, no errors | Only the dropped URI is retried; both come back resolved; no errors |
| `alias omitted with no error` | Hub returns `As:"first"` only, no errors | Only `As:"second"` is retried; both aliases resolved |
| `no retry when every ref is accounted for` | Hub returns both aliases | Fallback never called |

The second subtest is the direct regression guard for the old gate — under
`len(sr.Errors) > 0` the fallback is never invoked and the result is short by
one alias.

Existing tests unchanged and passing, including
`TestRoutingSkillResolver_RegisterFallback_SameURIDifferentAliases` (both
aliases still reach the fallback when Hub errors on the URI).

**Status:** `go vet ./pkg/agent/...` clean; `go test ./pkg/agent/...` fully green.

**Note:** I-4 (shared `ctx` between Hub call and fallback) is *not* addressed
here and remains an open entry-criterion item.
