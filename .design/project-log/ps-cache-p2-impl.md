# Phase 2 Implementation Log: Hub DB Schema and GitHub Resolution Cache

**Date:** 2026-07-28  
**Agent:** ps-cache-p2-dev  
**Branch:** `scion/ps-cache-p2-dev`  
**Commit:** 05f8b83

## Summary

Successfully implemented Phase 2 of the gh:// skill cache durability fix. This adds a Hub-side DB-backed resolution cache shared across all broker instances, eliminating per-request cold cache loads and duplicate GitHub API calls.

## Design Reference

Full design doc: `/scion-volumes/scratchpad/projects/project-skills/design-cache-durability.md`

Phase 2 implements Sub-change 2 from the design:
- DB-backed cache table via ent ORM
- Hub-side GitHub API resolution with cache lookup
- Shared cache across all broker instances
- Background TTL eviction
- Hub admin settings for operator visibility

## Files Changed

### New Files

1. **pkg/ent/schema/github_resolution_cache.go**
   - Ent schema entity for the cache table
   - Fields: cache_key (unique), original_uri, commit_sha, file_entries (JSON), bundle_hash, token_scope, expires_at, create_time
   - Indexes: cache_key (unique), expires_at (for eviction)
   - GitHubFileEntry value type: {Path, URL, Hash, Size}

2. **pkg/hub/github_resolution_store.go**
   - GitHubResolutionStore wrapping ent client
   - Get(ctx, cacheKey) - returns entry if valid and not expired
   - Put(ctx, cacheKey, entry) - upserts cache entry
   - PurgeExpired(ctx) - deletes expired entries
   - GitHub API helpers:
     - ghResolveCommitSHA - resolves ref to 40-char SHA, short-circuits for full SHAs
     - ghListContents - fetches file list with git blob SHAs and CDN URLs
   - computeCacheKey - sha256(normalized_uri + ":" + token_scope)
   - isFullCommitSHA - validates 40-char lowercase hex SHA

3. **pkg/hub/github_resolution_store_test.go**
   - TestGitHubResolutionStore_GetPut - basic cache operations
   - TestGitHubResolutionStore_Expiration - TTL expiration behavior
   - TestGitHubResolutionStore_PurgeExpired - eviction logic
   - TestComputeCacheKey - cache key computation
   - TestIsFullCommitSHA - SHA validation
   - All tests use in-memory SQLite ent client

4. **pkg/ent/githubresolutioncache*.go** (generated)
   - Ent-generated client code for GitHubResolutionCache entity
   - CRUD operations, query builders, type definitions

### Modified Files

1. **pkg/hub/skill_handlers.go**
   - Added imports: time, agent package
   - Added githubAPIBase constant
   - resolveGitHubToken(ctx, projectID) - determines token scope, mints GitHub App tokens
   - resolveGitHubSkill(ctx, rawURI, projectID) - main resolution logic with cache lookup
   - buildResolvedSkillResponse - constructs response from cache entry
   - computeBundleHash - computes content hash from file entries
   - Wire gh:// detection into handleSkillsResolve (BEFORE api.ParseSkillURI)

2. **pkg/hub/server.go**
   - Added ghResolutionStore field to Server struct
   - Initialize ghResolutionStore in SetIntegrationHA when ent client available
   - Register github-resolution-cache-eviction task in StartBackgroundServices (10 min interval)
   - githubResolutionCacheEvictionHandler - background eviction task handler
   - seedGitHubResolutionCacheSettings - seed hub_settings["github_resolution_cache"] on startup
   - Call seed function in New() after seedPlatformSkillInsertions

3. **pkg/agent/github_resolution_cache.go**
   - Added DefaultSHAResolutionCacheTTL = 24 * time.Hour constant
   - Updated DefaultResolutionCacheTTL comment to clarify branch/tag refs

4. **pkg/store/concurrency.go**
   - Added LockGitHubResolutionCacheEviction = 0x5C10000A advisory lock constant

5. **go.mod**
   - Updated dependencies (go mod tidy)

## Key Decisions

### Cache Key Design

Chose `sha256(normalized_uri + ":" + token_scope)` where:
- normalized_uri = `gh://owner/repo/path@ref` (lowercased owner/repo)
- token_scope = GitHub App installation ID (string) or "public"

This ensures:
- Different token scopes get separate cache entries (public vs. private repo access)
- Installation ID (not rotating token) ensures cache survives token rotation
- Deterministic key for same URI regardless of case variations

### TTL Strategy

- Branch/tag refs: 5 minutes (DefaultResolutionCacheTTL, unchanged from Phase 1)
- Full commit SHAs: 24 hours (new DefaultSHAResolutionCacheTTL)

Rationale: Commit SHAs are immutable, so longer TTL is safe and reduces API calls.

### Initialization Pattern

ghResolutionStore is initialized in SetIntegrationHA rather than New() because:
- Requires ent client, which is set via SetIntegrationHA
- Only available in Postgres/HA mode (SQLite mode doesn't use ent client for hub operations)
- Nil-safe: all ghResolutionStore usage checks for nil before calling methods

### GitHub API Helper Placement

Placed ghResolveCommitSHA and ghListContents in github_resolution_store.go (Hub package) rather than agent package because:
- Hub-only code (broker doesn't need direct GitHub API access for resolution)
- Avoids import cycle (Hub → agent is OK, agent → Hub is not)
- Co-located with the cache store that uses them

## Test Coverage

All tests pass:
```
go build ./...          # Clean
go vet ./...            # Clean
go test ./pkg/hub/...   # PASS (github_resolution_store tests)
go test ./pkg/ent/...   # PASS (entc tests only)
```

Test cases added:
- Basic Get/Put operations
- TTL expiration (expired entries not returned)
- PurgeExpired (removes expired, keeps valid)
- Cache key computation (deterministic, case-insensitive owner/repo)
- SHA validation (40-char lowercase hex)

## Integration Points

### Cache Lookup Flow

1. handleSkillsResolve detects `gh://` prefix
2. Calls resolveGitHubSkill(ctx, uri, projectID)
3. resolveGitHubToken determines token scope (public or installation ID)
4. Compute cache_key from URI + scope
5. Check ghResolutionStore.Get(ctx, cache_key)
   - Hit: return cached entry, log cache_hit: true
   - Miss: call GitHub API, store entry, log cache_hit: false
6. Return ResolvedSkillResponse with files from GitHub CDN URLs

### Background Eviction

Scheduler task runs every 10 minutes:
- Holds advisory lock LockGitHubResolutionCacheEviction
- Calls ghResolutionStore.PurgeExpired(ctx)
- Deletes rows where expires_at < now()
- Singleton across all Hub replicas (one replica per tick)

### Settings Seed

On every Hub startup:
- seedGitHubResolutionCacheSettings upserts hub_settings["github_resolution_cache"]
- Default value: `{"branch_ref_ttl_minutes": 30, "sha_ref_ttl_hours": 24, "enabled": true}`
- Idempotent: skips if already exists to avoid revision churn
- Provides operator visibility and tuning capability

## Gotchas / Edge Cases

### Ent Codegen

Running `go generate ./pkg/ent/...` generates ~4000 lines of code across multiple files. This is expected and normal for ent. The generated files are:
- pkg/ent/githubresolutioncache.go
- pkg/ent/githubresolutioncache_create.go
- pkg/ent/githubresolutioncache_query.go
- pkg/ent/githubresolutioncache_update.go
- pkg/ent/githubresolutioncache_delete.go
- pkg/ent/githubresolutioncache/githubresolutioncache.go
- pkg/ent/githubresolutioncache/where.go
- Updates to pkg/ent/client.go, mutation.go, tx.go, etc.

All generated files must be committed.

### SQLite Compatibility

The design is compatible with both SQLite and Postgres:
- JSON field maps to TEXT (SQLite) or JSONB (Postgres)
- No Postgres-specific operators used
- Eviction query uses standard TIME field comparison
- Single-writer model OK for single-node SQLite deployments

### Nil Safety

ghResolutionStore is nil when ent client is unavailable (e.g., SQLite-only mode). All usage sites check for nil:
- resolveGitHubSkill: `if s.ghResolutionStore != nil { ... }`
- Background eviction: only registered `if s.ghResolutionStore != nil`

### DownloadURLInfo.Size Type

DownloadURLInfo.Size is int64, not string. Initially attempted to use strconv.FormatInt but corrected to direct assignment.

## Next Steps (Phase 3)

Phase 3 will add broker routing changes:
- Remove local GitHubSkillResolver as primary handler for gh://
- Route gh:// URIs to Hub's /api/v1/skills/resolve
- Keep local resolver as fallback for Hub unavailability
- Thread projectID through resolve request

## Acceptance Criteria Status

✅ go build ./... clean  
✅ go vet ./... clean  
✅ go test ./pkg/ent/... passes  
✅ go test ./pkg/hub/... passes  
✅ handleSkillsResolve detects gh:// and routes to resolveGitHubSkill  
✅ Cache hit logs cache_hit: true  
✅ Cache miss logs cache_hit: false + API call  
✅ Background eviction goroutine started  
✅ Hub settings seed entry added  
✅ Ent schema generates cleanly  

All Phase 2 acceptance criteria met.
