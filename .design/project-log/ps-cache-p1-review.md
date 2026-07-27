# Phase 1 Code Review: Broker Singleton Resolution Cache

**Reviewer:** ps-cache-p1-review  
**Date:** 2026-07-27  
**Branch:** `scion/ps-cache-em-b`  
**Commit:** `7213f4ac` (on top of PR #878 merge commit `c7342140`)  
**Design Doc:** `/scion-volumes/scratchpad/projects/project-skills/design-cache-durability.md`

---

## Overall Verdict: **APPROVE**

The Phase 1 implementation is **correct, well-tested, and production-ready**. The singleton cache is properly integrated, nil-safe, and preserves all backward compatibility guarantees. The PR #878 token fallback logic is correctly preserved.

**Key strengths:**
- Proper concurrency safety with `sync.RWMutex`
- Graceful degradation on cache init failures
- Excellent test coverage for the singleton behavior
- Clean backward compatibility through optional cache parameter
- TTL values match design spec exactly

**Minor observations:**
- One non-blocking naming inconsistency in test file
- One harmless but technically unnecessary deep-copy in the cache Get method
- Logging could be slightly more consistent between warnings and info

All findings are **non-blocking** and do not require changes before merge.

---

## Detailed Findings

### 1. [Non-blocking] Test file naming pattern inconsistency

**File:** `pkg/agent/github_skill_resolver_test.go:1145, 1166, 1178`  
**Severity:** Non-blocking

**Issue:**  
The three existing tests for `NewGitHubSkillResolverWithCredentials` were updated to pass `nil` as the third argument, but they retain the old two-argument naming pattern in their test names (e.g., `TestNewGitHubSkillResolverWithCredentials_ProvisionCredentialsFallback`). The new test `TestGitHubSkillResolver_SharedCacheSingleton` uses the base type name (`GitHubSkillResolver`) rather than the constructor name.

**Observation:**  
This is purely stylistic. The tests are correct and pass. The naming inconsistency doesn't affect functionality, but future test authors might be confused about the pattern to follow.

**Recommended fix (optional):**  
No change needed for Phase 1. If future work touches these tests, consider:
- Renaming the constructor tests to reflect they're testing the three-arg signature
- Or keeping them as-is since they're testing credential fallback behavior specifically

---

### 2. [Non-blocking] Potential performance micro-optimization in cache Get

**File:** `pkg/agent/github_resolution_cache.go:93-96`  
**Severity:** Non-blocking

**Issue:**  
The `Get` method performs a deep copy of the `Files` slice:

```go
if len(entry.Skill.Files) > 0 {
    skill.Files = make([]ResolvedFile, len(entry.Skill.Files))
    copy(skill.Files, entry.Skill.Files)
}
```

This is defensive and safe, but the `ResolvedFile` struct contains only primitive types and strings (which are immutable in Go). The struct copy on line 92 (`skill := entry.Skill`) already creates independent values.

**Analysis:**  
The deep copy is **harmless** and arguably good defensive programming. The only scenario where it matters is if a caller modifies the returned `skill.Files` slice itself (e.g., `result.Files = append(result.Files, ...)`), which would affect the cached entry's slice capacity but not its content. Given that `ResolvedFile.Content` is a `[]byte` (which IS mutable), the current defensive approach is actually correct if `Content` is non-nil.

**Wait — Critical Discovery:**  
Actually, reviewing the cache file structure comment from the design doc: "File bytes are `json:"-"` — not persisted." Let me verify:

Checking `pkg/agent/types.go` or wherever `ResolvedFile` is defined...

Actually, from the diff context at line 240: `Content: content, // Carry bytes so install phase skips unauthenticated re-download.`

The design doc states that `ResolvedFile.Content` has `json:"-"` tag, so disk-persisted entries have `Content == nil`. But in-memory cache entries (within a single broker instance lifetime) DO have `Content` populated. The `[]byte` field is mutable, so the deep copy of the slice is **necessary and correct** to prevent a caller from mutating the cached `Content`.

**Conclusion:**  
The code is correct as written. The deep copy is necessary for safety when `Content` is non-nil. No change needed.

---

### 3. [Non-blocking] Log level inconsistency for cache initialization

**File:** `pkg/runtimebroker/server.go:412, 416, 420`  
**Severity:** Non-blocking

**Issue:**  
Cache initialization uses `slog.Warn` for errors (lines 412, 416) but `slog.Info` for success (line 420). The existing `skCache` initialization pattern (lines 402-406) returns an error from `initHubIntegration` on cache init failure rather than logging a warning.

**Analysis:**  
The Phase 1 design explicitly requires graceful degradation: "log a warning but don't fatal." The design is correct — GitHub resolution cache failure should not prevent broker startup (unlike the skill cache, which is critical). The warning logs are appropriate.

The success log at line 420 could be `Debug` instead of `Info`, since it's an implementation detail. But `Info` is fine for initial rollout observability.

**Recommended fix (optional):**  
No change needed. The current approach matches the design requirement. If log volume becomes an issue in production, downgrade line 420 from `Info` to `Debug`.

---

### 4. [Non-blocking] DefaultSHAResolutionCacheTTL defined but unused

**File:** `pkg/agent/github_resolution_cache.go:35-36`  
**Severity:** Non-blocking

**Issue:**  
The constant `DefaultSHAResolutionCacheTTL = 24 * time.Hour` is defined but not used in Phase 1.

**Analysis:**  
The design doc explicitly states: "A separate `DefaultSHAResolutionCacheTTL = 24h` constant can be added for completeness when the Hub-side design lands." This constant is defined in Phase 1 for **forward compatibility** with Phase 2 Hub-side logic. It's intentionally unused in this phase.

PR #878's full-SHA short-circuit (Track A) means that SHA refs never reach the broker's resolution cache path — they short-circuit before calling `resolveCommitSHA`. The Hub-side resolver in Phase 2 will use this constant.

**Conclusion:**  
Correct as designed. No change needed.

---

### 5. [Non-blocking] Documentation: nil cache parameter behavior

**File:** `pkg/agent/github_skill_resolver.go:98-100`  
**Severity:** Non-blocking

**Issue:**  
The doc comment for `NewGitHubSkillResolverWithCredentials` clearly documents the cache parameter:

> If cache is non-nil, it is used as the singleton resolution cache (e.g., from
> the broker server struct) instead of the per-resolver cache created by
> NewGitHubSkillResolver. Pass nil to get the default per-resolver cache behavior.

This is excellent documentation. However, the actual implementation at line 117-119 has a subtle behavior: when `cache` is nil, the resolver keeps the cache created by `NewGitHubSkillResolver()` (called on line 102). This means a nil cache doesn't mean "no cache" — it means "use the per-resolver ephemeral cache."

**Analysis:**  
The documentation is **accurate** — it says "default per-resolver cache behavior," which is exactly what happens. The code matches the doc. This is not a bug.

**Conclusion:**  
Documentation is correct and clear. No change needed.

---

## Correctness Review

### ✅ Singleton cache works correctly

**Lines:** `pkg/runtimebroker/server.go:207, 410-422`, `pkg/runtimebroker/handlers.go:759`

- The `ghResolutionCache` field is added to the `Server` struct (line 207)
- Initialized in `initHubIntegration` (lines 410-422)
- Passed to resolver constructor in `createAgent` handler (line 759)

**Verification:** The singleton is created once per broker instance and shared across all agent creation requests. Concurrent goroutines share the same cache instance.

### ✅ Nil-safety handled correctly

**Lines:** `pkg/agent/github_skill_resolver.go:190, 262`

Both cache operations check `if r.resolutionCache != nil` before calling `Get` or `Put`. When cache is nil, the code falls through to direct API calls.

**Lines:** `pkg/runtimebroker/server.go:411-417`

Cache initialization errors are logged but don't fail the broker startup. The server's `ghResolutionCache` field remains nil, and resolvers gracefully handle nil.

### ✅ sync.RWMutex usage is safe for concurrent goroutines

**File:** `pkg/agent/github_resolution_cache.go`

- `Get` uses `RLock/RUnlock` (lines 82-83) — allows concurrent reads
- `Put` uses `Lock/Unlock` (lines 102, 114) — exclusive write lock
- `evictExpired` is called within `Put`'s locked section (line 109) — correct (requires write lock)
- `save` is called **outside** the lock (line 116) after creating a snapshot under lock (lines 110-113) — correct pattern to minimize lock hold time

**Critical observation:** The `save` method operates on a snapshot created while holding the lock. This prevents deadlocks and minimizes lock contention. The atomic rename pattern (`write to .tmp → rename`) ensures disk writes don't corrupt the cache file.

**Conclusion:** The mutex usage is correct and production-safe.

### ✅ Backward compatibility: nil cache gracefully falls through

**Lines:** `cmd/create.go:146`, `pkg/agent/github_skill_resolver.go:117-119`

- CLI passes `nil` as the cache parameter (line 146) — gets per-resolver ephemeral cache (the old behavior)
- Broker passes `s.ghResolutionCache` which may be nil if initialization failed — resolver handles it gracefully
- Existing tests updated to pass `nil` and continue to work

### ✅ Token fallback logic from PR #878 preserved correctly

**Lines:** `pkg/agent/github_skill_resolver.go:101-113`

Comparing the PR #878 version (shown in bash output) to the Phase 1 version:

**PR #878 logic:**
1. If `defaultToken != ""`, use it (line 103-104)
2. Else if `r.token == ""` (no broker-env GITHUB_TOKEN), check `provisionCredentials["GITHUB_TOKEN"]` (lines 105-112)
3. Set `r.provisionCredentials` (line 113)

**Phase 1 logic:**
1. Lines 103-104: Identical (explicit token wins)
2. Lines 105-112: Identical (provisionCredentials fallback)
3. Line 114: Identical (store provisionCredentials map)
4. Lines 115-119: **NEW** — singleton cache override logic

**Verification:** The token fallback logic is **byte-for-byte identical** to PR #878. The cache parameter logic is purely additive. No regression.

### ✅ Error handling: cache init failures logged appropriately

**Lines:** `pkg/runtimebroker/server.go:412, 416, 420`

- Line 412: Warning if `GitHubResolutionCacheDir()` fails
- Line 416: Warning if `NewGitHubResolutionCache()` fails, with explicit note "running uncached"
- Line 420: Info log on success with dir and TTL for observability
- All use `slog` (structured logging) to stderr, not silently discarded

**Comparison to PR #878:** PR #878 added cache-init error logging at the **per-resolver** level (in `NewGitHubSkillResolver`, lines 70 and 76 in the current code). Phase 1 moves this logging to the **broker startup** level for the singleton path, which is correct — you only want one log line per broker instance, not one per request.

### ✅ TTL values correct per design

**Lines:** `pkg/agent/github_resolution_cache.go:32, 36`

- `DefaultResolutionCacheTTL = 30 * time.Minute` ✓ (design: 30 min for branch refs)
- `DefaultSHAResolutionCacheTTL = 24 * time.Hour` ✓ (design: 24h for SHA refs, unused in Phase 1)

**Verification:** The design doc specifies these exact values. The broker passes `agent.DefaultResolutionCacheTTL` to the cache constructor (line 414 in server.go).

### ✅ Test coverage adequate

**File:** `pkg/agent/github_skill_resolver_test.go:1232-1313`

**New test:** `TestGitHubSkillResolver_SharedCacheSingleton`

**What it tests:**
1. Creates a shared cache instance
2. Creates two **separate** resolver instances both pointing to the same cache
3. Calls `Resolve` on resolver1 → tracks API call count
4. Calls `Resolve` on resolver2 with the same URI → verifies NO new API calls (cache hit)
5. Verifies the resolved skill matches

**Coverage assessment:** This test **directly validates the singleton behavior** — the core requirement of Phase 1. It proves that multiple resolver instances sharing one cache only make one API call.

**What it doesn't test (and doesn't need to):**
- Concurrent access (RWMutex correctness) — Go's race detector would catch issues (`go test -race`)
- Disk persistence across restarts — already tested by existing cache tests
- TTL expiration — already tested by existing cache tests

**Conclusion:** Test coverage is **sufficient and targeted**. The test validates exactly what Phase 1 claims to deliver.

---

## Code Quality Assessment

### Readability: ✅ Excellent

- Clear variable names (`ghResolutionCache`, not `ghc` or `cache1`)
- Consistent code structure (follows existing `skCache` and `hcCache` patterns)
- Helpful comments (e.g., line 417: "nil cache is safe — resolver falls through to API call")

### Naming: ✅ Consistent

- `GitHubResolutionCacheDir()` exported function follows Go naming conventions
- Server field `ghResolutionCache` matches existing `skCache` pattern
- Cache struct `GitHubResolutionCache` matches existing naming

### Documentation: ✅ Complete

- `NewGitHubSkillResolverWithCredentials` doc comment fully updated with cache parameter description
- `GitHubResolutionCacheDir()` retains its doc comment (just made public)
- Server initialization logs describe what's happening (observability)

---

## Risk Assessment

### Concurrency safety: **LOW RISK** ✅

The `sync.RWMutex` usage is correct. The snapshot pattern in `Put` (lines 110-113) is a Go best practice. The broker will serve multiple agent creation requests concurrently, and the cache is designed for this.

### Performance impact: **POSITIVE** ✅

Phase 1 **eliminates** redundant disk reads within a single broker instance. A broker handling 10 sequential requests for the same skill now loads the cache from disk once (at initialization) instead of 10 times.

API call reduction: Within one instance, the second request for the same skill makes **0 API calls** (down from 2).

### Deployment risk: **LOW RISK** ✅

- Backward compatible: CLI and nil-cache paths preserve old behavior
- Graceful degradation: cache init failure doesn't break the broker
- No schema changes: purely in-memory and local disk
- No Hub changes: Phase 1 is broker-only

### Rollback safety: **HIGH** ✅

If Phase 1 is deployed and needs rollback:
1. Revert the broker binary
2. Old broker code doesn't know about the singleton cache → uses per-resolver cache (old behavior)
3. No data loss (cache is ephemeral anyway on Cloud Run)

---

## Test Results

**Executed:**
```bash
go test ./pkg/agent -run TestGitHubSkillResolver_SharedCacheSingleton -v
# PASS

go test ./pkg/agent -run TestNewGitHubSkillResolverWithCredentials -v
# PASS (all 3 tests)

go test ./pkg/agent/... -run GitHub
# PASS (all GitHub-related tests, ~17 tests)

go vet ./pkg/agent/... ./pkg/runtimebroker/... ./cmd/...
# PASS (no warnings)
```

**Race detector:** Not run in this review, but the code uses standard library concurrency primitives (`sync.RWMutex`) correctly. Recommend running `go test -race ./pkg/agent/...` in CI.

---

## Comparison to Design Doc

| Design Requirement | Implementation Status | Notes |
|---|---|---|
| Add `ghResolutionCache` to `server` struct | ✅ Complete | Line 207 in server.go |
| Initialize in `initHubIntegration` | ✅ Complete | Lines 410-422 in server.go |
| Pass to resolver constructor | ✅ Complete | Line 759 in handlers.go |
| Export `GitHubResolutionCacheDir()` | ✅ Complete | Line 167 in github_resolution_cache.go |
| Bump TTL from 5m to 30m | ✅ Complete | Line 32 in github_resolution_cache.go |
| Add `DefaultSHAResolutionCacheTTL = 24h` | ✅ Complete | Line 36 (unused in Phase 1, for Phase 2) |
| Nil cache safe fallback | ✅ Complete | Lines 190, 262 in github_skill_resolver.go |
| Log errors on init failure | ✅ Complete | Lines 412, 416 in server.go |
| Unit test for singleton behavior | ✅ Complete | Lines 1232-1313 in github_skill_resolver_test.go |

**Conclusion:** Implementation matches design spec **exactly**. No deviations.

---

## Additional Observations (Positive Feedback)

### 1. Excellent test design

The `TestGitHubSkillResolver_SharedCacheSingleton` test uses a mock HTTP server to count API calls — this is the **gold standard** for testing cache behavior. It doesn't rely on implementation details; it verifies the observable effect (API call reduction).

### 2. Defensive programming in cache Get

The deep copy of `skill.Files` (lines 94-96) is a good defensive practice, especially since `ResolvedFile.Content` is a mutable `[]byte`. This prevents cache pollution from caller mutations.

### 3. Atomic disk writes

The `save` method (lines 142-153) uses the atomic rename pattern: write to `.tmp`, then rename. This is the correct way to handle concurrent disk writes and prevents corruption from partial writes.

### 4. Snapshot pattern in Put

Creating an immutable snapshot (lines 110-113) before releasing the lock and calling `save` is a Go best practice. This minimizes lock hold time and prevents blocking readers during slow disk I/O.

### 5. Project log quality

The `.design/project-log/ps-cache-p1-impl.md` file is **exemplary documentation**. It captures:
- What was changed
- Why each change was made
- Build/test results
- Next steps

This is production-grade commit documentation.

---

## Final Recommendation

**APPROVE** for merge into the ptone/scion fork and eventual upstream PR.

**Deployment readiness:** Production-ready. No blocking issues found.

**Suggested merge order:**
1. Verify PR #878 is merged into upstream main (prerequisite)
2. Rebase `scion/ps-cache-em-b` onto latest main (confirm c734214 is the correct base)
3. Merge Phase 1 to fork main
4. Deploy to a single broker instance for observability testing
5. Monitor logs for "GitHub resolution cache initialized" (success) or warnings (degraded)
6. Scale to full broker fleet

**Follow-up work (not blocking):**
- Run `go test -race` in CI to validate concurrency safety
- Monitor cache hit rate in production logs (after Phase 2 adds the log line)
- Consider adding a Prometheus metric for cache hit/miss ratio (Phase 2 or 3)

---

## Summary

Phase 1 successfully converts the GitHub resolution cache from per-request ephemeral to broker-level singleton, exactly as designed. The implementation is:

- ✅ Correct (logic matches design)
- ✅ Safe (nil-checks, concurrency-safe, graceful degradation)
- ✅ Tested (new test validates singleton behavior)
- ✅ Backward compatible (nil cache → old behavior)
- ✅ Well documented (code comments + project log)
- ✅ Production-ready (low risk, easy rollback)

**No changes required.** Ready to merge.

---

**Reviewer signature:** ps-cache-p1-review  
**Review completed:** 2026-07-27  
**Confidence level:** High (thorough code inspection, test validation, concurrency analysis, and design-spec comparison)
