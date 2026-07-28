# Phase 2 Code Review: Hub DB Schema and GitHub Resolution Resolver

**Reviewer:** Code Review Agent  
**Date:** 2026-07-28  
**Branch:** `scion/ps-cache-em-b`  
**Commits Reviewed:** `d814b2a1..38f541f8` (2 commits: b8a45bed + 38f541f8)  
**Files Changed:** 24 files (23 implementation + 1 project log)

---

## Executive Summary

**VERDICT: ✅ APPROVE**

The Phase 2 implementation successfully adds a Hub-side DB-backed GitHub skill resolution cache that is shared across all broker instances. The implementation is architecturally sound, follows the design specification closely, and includes appropriate test coverage for the core store operations.

**Key Strengths:**
- ✅ Correct cache key design using stable installation ID (not rotating tokens)
- ✅ Proper TTL handling (30m for branches, 24h for SHAs)
- ✅ SQLite and Postgres compatibility via ent ORM
- ✅ Nil-safe store handling throughout
- ✅ Structured logging with cache_hit observability
- ✅ Background eviction with singleton locking
- ✅ Idempotent Hub settings seeding

**Minor Gaps (non-blocking):**
- No integration test verifying cache hit actually skips GitHub API call
- Settings seed values are currently hardcoded (not read at resolve time)
- No rate-limit error handling guidance in GitHub API helpers

All critical design requirements are satisfied. The code is production-ready. Recommendations below can be addressed in follow-up work.

---

## Findings

### ✅ POSITIVE: Critical Design Decisions

**P1. Stable Cache Key Design** (github_resolution_store.go:114-124)

The cache key correctly uses GitHub App installation ID (stable) rather than the rotating token value:

```go
func computeCacheKey(owner, repo, skillPath, ref, tokenScope string) string {
    normalized := fmt.Sprintf("gh://%s/%s/%s@%s",
        strings.ToLower(owner),
        strings.ToLower(repo),
        skillPath,
        ref,
    )
    h := sha256.Sum256([]byte(normalized + ":" + tokenScope))
    return hex.EncodeToString(h[:])
}
```

And in `resolveGitHubToken`:
```go
installID = strconv.FormatInt(*project.GitHubInstallationID, 10)
return installID, mintedToken, nil
```

This ensures cache entries remain valid across token rotations (tokens expire hourly, installation IDs are stable).

**P2. Correct TTL Values** (agent/github_resolution_cache.go:32-37)

```go
DefaultResolutionCacheTTL = 30 * time.Minute
DefaultSHAResolutionCacheTTL = 24 * time.Hour
```

Matches the design spec exactly. The merge from Phase 1 preserved the 30-minute value (not the 5-minute value that Phase 2 initially branched with).

**P3. SQLite Compatibility** (ent/schema/github_resolution_cache.go:51)

```go
field.JSON("file_entries", []GitHubFileEntry{})
```

Uses `field.JSON()` which maps to TEXT on SQLite and JSONB on Postgres. No Postgres-specific JSONB operators are used in queries. Eviction uses standard TIME comparison. This design works on both single-node SQLite and HA Postgres deployments.

**P4. Observability** (skill_handlers.go:1562, 1606)

Structured logging clearly distinguishes cache hits from misses:
```go
slog.InfoContext(ctx, "github_resolution_cache: cache hit",
    "uri", rawURI, "commit_sha", entry.CommitSHA[:12], "cache_hit", true)

slog.InfoContext(ctx, "github_resolution_cache: cache miss, resolved via API",
    "uri", rawURI, "commit_sha", commitSHA[:12], "files", len(fileEntries), "cache_hit", false)
```

The `cache_hit` boolean field enables metric aggregation and hit-rate monitoring.

**P5. Nil-Safe Store Handling** (skill_handlers.go:1548-1558, 1599-1603)

All `ghResolutionStore` accesses check for nil before use:
```go
if s.ghResolutionStore != nil {
    entry, hit, err := s.ghResolutionStore.Get(ctx, cacheKey)
    // ...
}
```

This gracefully handles SQLite-only deployments where ent client is not available.

**P6. Full SHA Short-Circuit** (github_resolution_store.go:131-133)

```go
if isFullCommitSHA(ref) {
    return ref, nil
}
```

The `ghResolveCommitSHA` function short-circuits for full 40-char SHAs, avoiding GitHub API calls for pinned refs. Combined with Track A (PR #878), this provides defense-in-depth.

**P7. Singleton Eviction with Advisory Lock** (server.go:2487-2489, store/concurrency.go:63-65)

```go
s.scheduler.RegisterRecurringSingleton("github-resolution-cache-eviction", 10, 
    store.LockGitHubResolutionCacheEviction, s.githubResolutionCacheEvictionHandler())
```

Uses advisory lock `0x5C10000A` to ensure only one Hub instance runs eviction per interval in HA deployments. Correct use of the singleton pattern.

---

### 📝 NON-BLOCKING: Test Coverage Gaps

**N1. No Integration Test for Cache Hit Skipping API Call** (github_resolution_store_test.go)

The existing tests verify store operations (Get, Put, PurgeExpired) but do NOT verify that a cache hit actually skips the GitHub API call. The tests use an in-memory SQLite ent client but never mock or spy on the GitHub API helpers.

**Recommendation:**
Add a test in `skill_handlers_test.go` (or a new test file) that:
1. Mocks the GitHub API (e.g., via httptest server or test spy)
2. Calls `resolveGitHubSkill` twice with same URI
3. Asserts that the GitHub API is called exactly once (cache hit on second call)

**Example structure:**
```go
func TestResolveGitHubSkill_CacheHit(t *testing.T) {
    // Setup: mock server, test ent client, Hub server with ghResolutionStore
    apiCallCount := 0
    mockAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        apiCallCount++
        // Return mock commit SHA and file list
    }))
    defer mockAPI.Close()

    // First call: cache miss, should hit API
    resolveGitHubSkill(ctx, "gh://owner/repo/skill@main", projectID)
    require.Equal(t, 1, apiCallCount)

    // Second call: cache hit, should NOT hit API
    resolveGitHubSkill(ctx, "gh://owner/repo/skill@main", projectID)
    require.Equal(t, 1, apiCallCount, "cache hit should not call GitHub API")
}
```

This is non-blocking because the store tests verify caching logic, and manual QA can verify the end-to-end flow. But adding this test would provide stronger confidence.

**N2. Settings Seed Values Not Actually Read** (server.go:3257-3261)

The `seedGitHubResolutionCacheSettings` function seeds default TTL values into `hub_settings`:
```go
settingValue := map[string]interface{}{
    "branch_ref_ttl_minutes": 30,
    "sha_ref_ttl_hours":      24,
    "enabled":                true,
}
```

However, `resolveGitHubSkill` hardcodes the TTL values from `agent.DefaultResolutionCacheTTL` constants:
```go
if isFullCommitSHA(ghRef.Ref) {
    ttl = agent.DefaultSHAResolutionCacheTTL  // hardcoded 24h
} else {
    ttl = agent.DefaultResolutionCacheTTL  // hardcoded 30m
}
```

The seeded hub_settings values are never read at resolve time.

**Recommendation:**
Either:
1. Read the TTL values from hub_settings in `resolveGitHubSkill` (operator-tunable), OR
2. Document that the seeded values are informational-only and operators must update the constants to change TTL

Current state is not incorrect (constants work fine), but it's a bit misleading to seed settings that aren't consumed.

**N3. No Test for Eviction Handler** (server.go:3238-3250)

The `githubResolutionCacheEvictionHandler` is tested indirectly via `TestGitHubResolutionStore_PurgeExpired`, but there's no test that verifies the handler itself (timeout behavior, nil-store handling, error logging).

**Recommendation:**
Add a unit test that invokes `githubResolutionCacheEvictionHandler()` directly and verifies:
- Returns immediately if `ghResolutionStore == nil`
- Calls `PurgeExpired` with correct context
- Handles context timeout gracefully

This is low-priority because the handler is a thin wrapper around `PurgeExpired`, which IS tested.

---

### 📝 NON-BLOCKING: Error Handling & Edge Cases

**N4. GitHub API Error Messages Could Be More Actionable** (github_resolution_store.go:148-156, 193-195)

When GitHub API calls fail, the error messages include HTTP status and response body, but don't provide guidance on next steps (e.g., rate limit errors, auth failures).

```go
if resp.StatusCode != http.StatusOK {
    body, _ := io.ReadAll(resp.Body)
    return "", fmt.Errorf("GitHub API error %d resolving %s@%s: %s", 
        resp.StatusCode, repo, ref, string(body))
}
```

**Recommendation:**
Consider adding specific handling for common error codes:
- 401/403: "Authentication failed or insufficient permissions. Verify GitHub App installation."
- 404: "Repository or ref not found. Verify URI is correct."
- 429: "Rate limit exceeded. Cache will retry with backoff."
- 5xx: "GitHub API unavailable. This is a transient error; resolution will be retried."

This is non-blocking because the current generic errors are sufficient for debugging, but targeted messages would improve operator experience.

**N5. No Validation of Bundle Hash Computation** (skill_handlers.go:1632-1643)

The `computeBundleHash` function delegates to `transfer.ComputeContentHash`, which is assumed to be correct. There's no test verifying that the bundle hash is stable and deterministic for the same file set.

**Recommendation:**
Add a test that verifies:
```go
files1 := []GitHubFileEntry{{Path: "a", Hash: "h1"}, {Path: "b", Hash: "h2"}}
files2 := []GitHubFileEntry{{Path: "a", Hash: "h1"}, {Path: "b", Hash: "h2"}}
hash1 := computeBundleHash(files1)
hash2 := computeBundleHash(files2)
require.Equal(t, hash1, hash2, "bundle hash must be deterministic")
```

This is low-priority because `transfer.ComputeContentHash` is presumably tested elsewhere, but it would document the expected behavior.

---

### 📝 NON-BLOCKING: Code Quality & Style

**N6. Magic Number for Eviction Interval** (server.go:2487)

The eviction interval is hardcoded as `10` minutes:
```go
s.scheduler.RegisterRecurringSingleton("github-resolution-cache-eviction", 10, ...)
```

**Recommendation:**
Extract to a named constant:
```go
const githubResolutionCacheEvictionIntervalMinutes = 10
```

This makes it easier to find and tune the interval without searching for magic numbers.

**N7. Inconsistent Error Wrapping Style** (github_resolution_store.go:148, skill_handlers.go:1516)

Some errors use `%w` (wrap), others use `%s` or inline string concat. All should use `%w` for proper error chain unwrapping.

Example inconsistency:
```go
// ✅ Good (wraps error)
return "", fmt.Errorf("failed to resolve commit SHA for %s@%s: %w", repo, ref, err)

// ❌ Inconsistent (doesn't wrap)
return nil, fmt.Errorf("invalid gh:// URI: %w", err)  // This one is OK
return "", "", fmt.Errorf("failed to get project: %w", err)  // This one is OK too
```

Actually, on closer inspection, all errors ARE wrapped with `%w`. False alarm. No action needed.

**N8. Potential Short SHA Truncation Panic** (skill_handlers.go:1562, 1606)

The code assumes `entry.CommitSHA` is always at least 12 characters when logging:
```go
"commit_sha", entry.CommitSHA[:12]
```

If GitHub ever returns a short SHA (edge case), this would panic. In practice, `isFullCommitSHA` validates 40-char SHAs, so this is safe. But defensive code would check length first or use a helper.

**Recommendation:**
Add a helper function:
```go
func shortSHA(sha string) string {
    if len(sha) < 12 { return sha }
    return sha[:12]
}
```

This is extremely low-priority (borderline pedantic) because the validation guarantees 40-char SHAs.

---

## Detailed Review by Criterion

### 1. ✅ Correctness: resolveGitHubSkill Cache Flow

**Status:** PASS

The `resolveGitHubSkill` function implements the design spec correctly:

1. ✅ Parses gh:// URI via `agent.ParseGitHubSkillURI`
2. ✅ Resolves token scope via `resolveGitHubToken` (installID or "public")
3. ✅ Computes cache key with `computeCacheKey(owner, repo, path, ref, installID)`
4. ✅ Checks cache via `ghResolutionStore.Get(ctx, cacheKey)`
5. ✅ On hit: returns cached entry, logs cache_hit: true
6. ✅ On miss: calls GitHub API, stores entry, logs cache_hit: false
7. ✅ Returns `ResolvedSkillResponse` with CDN URLs

No logic errors detected.

### 2. ✅ Token Handling: resolveGitHubToken Fallback

**Status:** PASS

The `resolveGitHubToken` function correctly implements the fallback logic:

```go
if projectID == "" {
    return "public", "", nil
}
project, err := s.store.GetProject(ctx, projectID)
if project.GitHubInstallationID == nil {
    return "public", "", nil
}
mintedToken, _, err := s.mintGitHubAppToken(ctx, project)
```

- ✅ Returns "public" for missing projectID
- ✅ Returns "public" for projects without GitHub App integration
- ✅ Mints token via existing `mintGitHubAppToken` (reuses established pattern)
- ✅ Returns installation ID as string for stable cache keying

### 3. ✅ Cache Key Design: Installation ID (Stable)

**Status:** PASS

The cache key uses `installID` (stable GitHub App installation ID) NOT the rotating token value. This is critical for correctness.

```go
installID = strconv.FormatInt(*project.GitHubInstallationID, 10)
cacheKey := computeCacheKey(ghRef.Owner, ghRef.Repo, ghRef.SkillPath, ghRef.Ref, installID)
```

Tokens rotate every hour. Installation IDs are stable for the lifetime of the App installation. Cache entries remain valid across token rotations. ✅

### 4. ✅ TTL Values: 30m Branch, 24h SHA

**Status:** PASS

TTL constants are correct:
```go
DefaultResolutionCacheTTL = 30 * time.Minute  // branch/tag refs
DefaultSHAResolutionCacheTTL = 24 * time.Hour  // full commit SHAs
```

And used correctly:
```go
if isFullCommitSHA(ghRef.Ref) {
    ttl = agent.DefaultSHAResolutionCacheTTL
} else {
    ttl = agent.DefaultResolutionCacheTTL
}
```

The 30-minute value matches the design (not the 5-minute value mentioned in the review requirements as a potential conflict). ✅

### 5. ✅ SQLite Compatibility

**Status:** PASS

Schema uses only SQLite/Postgres-compatible types:
- `field.String()` → TEXT/VARCHAR
- `field.Time()` → DATETIME/TIMESTAMP
- `field.JSON()` → TEXT/JSONB
- `field.UUID()` → TEXT/UUID

Queries use standard SQL:
- `Where(githubresolutioncache.ExpiresAtLT(time.Now()))` → `WHERE expires_at < ?`
- No Postgres-specific JSONB operators (`->`, `->>`, `@>`, etc.)

Single-writer SQLite model is sufficient for single-node deployments. Postgres handles concurrent writes for HA. ✅

### 6. ✅ Observability: Structured Logging

**Status:** PASS

All resolve operations log with `cache_hit: true/false`:
```go
slog.InfoContext(ctx, "github_resolution_cache: cache hit",
    "uri", rawURI, "commit_sha", entry.CommitSHA[:12], "cache_hit", true)
```

This enables metric aggregation:
```
sum(rate(log{msg="github_resolution_cache", cache_hit="true"}[5m])) / 
sum(rate(log{msg="github_resolution_cache"}[5m]))
```

Cache failures log as warnings (not errors), preserving fallback behavior:
```go
if err != nil {
    slog.WarnContext(ctx, "github_resolution_cache: cache lookup failed", ...)
}
```

### 7. ✅ Eviction: Background PurgeExpired

**Status:** PASS

Background eviction is correctly wired:

**Registration** (server.go:2487-2489):
```go
if s.ghResolutionStore != nil {
    s.scheduler.RegisterRecurringSingleton("github-resolution-cache-eviction", 10,
        store.LockGitHubResolutionCacheEviction, s.githubResolutionCacheEvictionHandler())
}
```

**Handler** (server.go:3238-3250):
```go
func (s *Server) githubResolutionCacheEvictionHandler() func(ctx context.Context) {
    return func(ctx context.Context) {
        ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
        defer cancel()
        if s.ghResolutionStore == nil { return }
        if err := s.ghResolutionStore.PurgeExpired(ctx); err != nil {
            slog.Error("Scheduler: github resolution cache eviction failed", "error", err)
        }
    }
}
```

- ✅ Only registered when store is available
- ✅ Uses singleton lock (one instance per tick across all Hubs)
- ✅ 15-second timeout prevents runaway queries
- ✅ Nil-check in handler (defense-in-depth)
- ✅ Errors logged but don't crash the scheduler

**PurgeExpired implementation** (github_resolution_store.go:104-109):
```go
func (s *GitHubResolutionStore) PurgeExpired(ctx context.Context) error {
    _, err := s.client.GitHubResolutionCache.
        Delete().
        Where(githubresolutioncache.ExpiresAtLT(time.Now())).
        Exec(ctx)
    return err
}
```

Correct use of `ExpiresAtLT(time.Now())`. Index on `expires_at` ensures fast scans. ✅

### 8. ✅ Hub Settings Seed

**Status:** PASS

**Seed function** (server.go:3256-3286):
```go
func (s *Server) seedGitHubResolutionCacheSettings(ctx context.Context) error {
    settingValue := map[string]interface{}{
        "branch_ref_ttl_minutes": 30,
        "sha_ref_ttl_hours":      24,
        "enabled":                true,
    }
    // ... idempotent upsert logic
}
```

- ✅ Seeded during `New()` (server.go:962-965)
- ✅ Idempotent: checks if setting exists before upserting
- ✅ Correct values (30 min, 24 hr, enabled)
- ✅ Logs seed success

**Minor note:** Settings are not currently read at resolve time (see finding N2). Non-blocking.

### 9. ✅ handleSkillsResolve Wiring

**Status:** PASS

The gh:// branch is correctly wired BEFORE the existing `api.ParseSkillURI` path:

```go
for _, skillRef := range req.Skills {
    if strings.HasPrefix(skillRef.URI, "gh://") {
        ghResolved, err := s.resolveGitHubSkill(ctx, skillRef.URI, req.ProjectID)
        if err != nil {
            resolveErrors = append(resolveErrors, ResolveSkillError{...})
        } else {
            resolved = append(resolved, *ghResolved)
        }
        continue  // ✅ Skips existing registry-skill path
    }
    // Existing registry-skill resolution follows
    uri, err := api.ParseSkillURI(skillRef.URI)
    // ...
}
```

- ✅ Prefix check is correct
- ✅ `continue` prevents fallthrough to registry-skill path
- ✅ Error handling matches existing pattern
- ✅ No impact on non-gh:// URIs (scion://, project://, etc.)

### 10. ✅ Test Coverage: Store Operations

**Status:** PASS (with minor gap noted in N1)

**Test file:** `github_resolution_store_test.go` (176 lines)

Tests cover:
1. ✅ `TestGitHubResolutionStore_GetPut` - basic Get/Put operations
2. ✅ `TestGitHubResolutionStore_Expiration` - expired entries return miss
3. ✅ `TestGitHubResolutionStore_PurgeExpired` - eviction deletes expired, keeps valid
4. ✅ `TestComputeCacheKey` - deterministic, case-insensitive owner/repo, scope-sensitive
5. ✅ `TestIsFullCommitSHA` - validates 40-char lowercase hex

All tests use in-memory SQLite ent client (no mocks needed for ent). Tests are hermetic and fast.

**Gap:** No integration test verifying cache hit skips GitHub API call (see N1). Non-blocking.

### 11. ✅ Error Handling: GitHub API Errors

**Status:** PASS

GitHub API errors are surfaced correctly:

```go
if resp.StatusCode != http.StatusOK {
    body, _ := io.ReadAll(resp.Body)
    return "", fmt.Errorf("GitHub API error %d resolving %s@%s: %s", 
        resp.StatusCode, repo, ref, string(body))
}
```

Errors propagate up to `resolveGitHubSkill`, which returns them to `handleSkillsResolve`, which appends them to `resolveErrors`. The existing error-handling path is reused. ✅

Nil-store is handled gracefully (cache operations are skipped, fallback to API succeeds):
```go
if s.ghResolutionStore != nil {
    // ... cache operations
}
// API call happens regardless of store availability
```

### 12. ✅ Conflict Resolution: DefaultResolutionCacheTTL = 30m

**Status:** PASS

The review requirements warned that Phase 2 might have incorrectly reset `DefaultResolutionCacheTTL` to 5 minutes (if it branched before Phase 1). Actual value in the code:

```go
DefaultResolutionCacheTTL = 30 * time.Minute
```

✅ Correct. No conflict. The merge from Phase 1 preserved the 30-minute value.

---

## Additional Observations

### Schema Design Quality

The ent schema is well-structured:

```go
field.String("cache_key").NotEmpty().Unique(),
field.String("original_uri").NotEmpty(),  // ✅ Debugging aid
field.String("commit_sha").NotEmpty(),
field.JSON("file_entries", []GitHubFileEntry{}),
field.String("bundle_hash").NotEmpty(),
field.String("token_scope").Default("public"),
field.Time("expires_at"),  // ✅ Indexed for eviction
field.Time("create_time").Default(time.Now).Immutable(),
```

- ✅ `cache_key` is unique (primary lookup key)
- ✅ `original_uri` is retained for debugging (not used in queries)
- ✅ `expires_at` is indexed (fast eviction scans)
- ✅ `create_time` is immutable (audit trail)
- ✅ `token_scope` defaults to "public" (handles unauthenticated case)

No redundant fields. No missing constraints. Clean schema design.

### GitHub API Helpers: Defensive Programming

**Full SHA short-circuit** (github_resolution_store.go:131-133):
```go
if isFullCommitSHA(ref) {
    return ref, nil
}
```

Avoids API call for refs like `abcdef1234567890abcdef1234567890abcdef12`. Combined with Track A (broker-side short-circuit in PR #878), this provides two layers of defense.

**SHA validation** (github_resolution_store.go:164-166):
```go
if !isFullCommitSHA(sha) {
    return "", fmt.Errorf("GitHub returned invalid SHA %q for %s@%s", sha, repo, ref)
}
```

Validates GitHub's response before accepting it. Protects against malformed API responses.

**Timeout on API calls** (github_resolution_store.go:146, 185):
```go
client := &http.Client{Timeout: 30 * time.Second}
```

Prevents hung GitHub API calls from blocking resolve requests indefinitely.

All good defensive patterns. ✅

### Upsert Semantics

The `Put` implementation uses ent's `OnConflict().UpdateNewValues()`:

```go
return s.client.GitHubResolutionCache.
    Create().
    SetCacheKey(cacheKey).
    // ... all fields
    OnConflict().
    UpdateNewValues().
    Exec(ctx)
```

This is correct upsert semantics: if `cache_key` exists (unique constraint violation), update the row with new values (refreshing `expires_at`). If not, insert. ✅

### Git Blob SHA vs. Content Hash

The implementation correctly distinguishes between:
- **Git blob SHA** (`file_entries[].Hash`) — from GitHub API, identifies file version
- **Bundle hash** (`bundle_hash`) — content-addressed hash of the entire file set (from `transfer.ComputeContentHash`)

The bundle hash is used for skill versioning (same files → same bundle hash, regardless of commit). Git blob SHAs are used for content-addressed file delivery. Both are needed. ✅

---

## Security Review

### 1. ✅ No Token Leakage in Cache Keys

Cache keys use installation ID (numeric), not token values. Tokens are ephemeral (1-hour TTL) and NEVER stored in the DB. ✅

### 2. ✅ No Plaintext Secrets in Logs

Logs include:
- `uri` (public info)
- `commit_sha` (public info)
- `cache_hit` (boolean)

Tokens are NEVER logged. ✅

### 3. ✅ SQL Injection Prevention

Ent ORM uses parameterized queries. All user inputs (cache_key, URI components) pass through ent query builders, which sanitize inputs. No raw SQL execution. ✅

### 4. ✅ SSRF Protection

GitHub API calls are limited to `api.github.com` (or `s.config.GitHubAppConfig.APIBaseURL` for GHES):
```go
apiBase := githubAPIBase  // "https://api.github.com"
if s.config.GitHubAppConfig.APIBaseURL != "" {
    apiBase = s.config.GitHubAppConfig.APIBaseURL
}
```

User-controlled URIs are parsed and validated by `ParseGitHubSkillURI` before reaching the API helpers. No arbitrary URL fetching. ✅

### 5. ✅ Rate Limit Amplification Mitigation

This is the ENTIRE PURPOSE of Phase 2. By caching resolution results, the system reduces GitHub API calls from O(brokers × requests) to O(unique URIs). ✅

---

## Performance Considerations

### 1. ✅ Cache Lookup Latency

Single-row DB lookup by unique key (`cache_key`). Postgres uses B-tree index on unique constraints. Expected latency: <5ms for warm cache, <50ms for cold. Acceptable for the resolve path.

### 2. ✅ Eviction Query Efficiency

```sql
DELETE FROM github_resolution_cache WHERE expires_at < NOW()
```

Index on `expires_at` makes this a fast range scan + delete. Expected table size: O(unique skill URIs × projects) ≈ thousands of rows, not millions. Eviction should complete in <1 second.

### 3. ✅ JSON Field Size

`file_entries` is stored as JSON. Typical size: 10-50 files × 200 bytes/entry ≈ 2-10 KB. Well within Postgres JSONB limits (1 GB, though practical limit is ~100 KB for performance). SQLite stores as TEXT, no size issues. ✅

---

## Backward Compatibility

### 1. ✅ No Breaking Changes to Existing Skill Resolution

The gh:// branch is additive. Existing `scion://`, `project://`, and registry skills continue to use the existing `resolveSkill` path unchanged. ✅

### 2. ✅ Graceful Degradation

If `ghResolutionStore == nil` (SQLite-only deployments without ent), the code falls through to GitHub API calls without caching. No errors, just reduced caching benefit. ✅

### 3. ✅ DB Migration is Additive

Adding the `github_resolution_cache` table is a zero-downtime migration:
```sql
CREATE TABLE github_resolution_cache (
    id UUID PRIMARY KEY,
    cache_key TEXT UNIQUE NOT NULL,
    -- ... other fields
);
CREATE INDEX idx_expires_at ON github_resolution_cache(expires_at);
```

No ALTER TABLE on existing tables. No data backfill. Ent's `AutoMigrate` handles this cleanly. ✅

---

## Final Verdict

**✅ APPROVE**

The Phase 2 implementation is production-ready. All critical requirements are met:

1. ✅ Correct cache-lookup-then-API flow
2. ✅ Stable installation ID cache keys (not rotating tokens)
3. ✅ Proper TTL values (30m branch, 24h SHA)
4. ✅ SQLite and Postgres compatibility
5. ✅ Structured logging with cache_hit observability
6. ✅ Background eviction with singleton locking
7. ✅ Idempotent settings seed
8. ✅ Nil-safe store handling
9. ✅ No breaking changes to existing skill resolution
10. ✅ Comprehensive store tests

**Minor improvements** (non-blocking, can be addressed in follow-up):
- Add integration test verifying cache hit skips GitHub API call (N1)
- Consider reading TTL values from hub_settings at resolve time (N2)
- Add specific error guidance for common GitHub API failures (N4)

**Recommendation:** Merge to main after approval. Phase 3 (broker routing change) can proceed.

---

## Recommended Follow-up Work

1. **Integration test** for cache hit skipping API call (N1) — Priority: Medium
2. **Use hub_settings TTL values** at resolve time (N2) — Priority: Low
3. **Enhanced GitHub API error messages** (N4) — Priority: Low
4. **Eviction handler unit test** (N3) — Priority: Low

None of these block Phase 2 merge or Phase 3 implementation.

---

**Review completed:** 2026-07-28  
**Next step:** Merge Phase 2 to main, proceed with Phase 3 (broker routing change)
