# Phase 1 Implementation Log: Broker Singleton Resolution Cache

**Date:** 2026-07-27  
**Branch:** `scion/ps-cache-em-b`  
**Related Design:** `/scion-volumes/scratchpad/projects/project-skills/design-cache-durability.md`

## Summary

Implemented Phase 1 of the gh:// skill cache durability fix: converted the GitHub resolution cache from a per-request ephemeral instance to a broker-level singleton. This eliminates redundant disk cache loads within a single broker instance.

## Files Changed

### pkg/agent/github_resolution_cache.go
- **Exported `GitHubResolutionCacheDir()`**: Renamed `githubResolutionCacheDir` to `GitHubResolutionCacheDir` so it can be called from `pkg/runtimebroker/server.go`
- **Updated TTL constants**:
  - Bumped `DefaultResolutionCacheTTL` from 5 minutes to 30 minutes (branch refs)
  - Added `DefaultSHAResolutionCacheTTL = 24 * time.Hour` (SHA-pinned refs, for Phase 2 Hub-side design)

### pkg/agent/github_skill_resolver.go
- **Updated `NewGitHubSkillResolver()`**: Changed internal call from `githubResolutionCacheDir()` to `GitHubResolutionCacheDir()`
- **Modified `NewGitHubSkillResolverWithCredentials()`**:
  - Added third parameter `cache *GitHubResolutionCache`
  - When `cache` is non-nil, use it directly (singleton path)
  - When `cache` is nil, fall back to creating a new per-resolver cache (backward compatibility)
  - Moved cache initialization logic into this function instead of calling `NewGitHubSkillResolver()`

### pkg/runtimebroker/server.go
- **Added server field**: `ghResolutionCache *agent.GitHubResolutionCache` to the `Server` struct
- **Initialized in `initHubIntegration()`**: 
  - Created singleton cache alongside existing `skCache` and `hcCache`
  - Log warning on failure but don't fatal (nil cache is safe — resolver falls through to API)
  - Logged success with cache dir and TTL for observability

### pkg/runtimebroker/handlers.go
- **Wired singleton cache**: Updated the call to `NewGitHubSkillResolverWithCredentials` to pass `s.ghResolutionCache` as the third argument

### cmd/create.go
- **Fixed backward compatibility**: Updated call to `NewGitHubSkillResolverWithCredentials` to pass `nil` as third argument (CLI doesn't need singleton, per-request is fine)

### pkg/agent/github_skill_resolver_test.go
- **Added `TestGitHubSkillResolver_SharedCacheSingleton()`**: New test verifying that two sequential resolve calls via different resolver instances sharing the same cache produce only one underlying API call (second is a cache hit)

## Key Design Decisions

1. **Graceful degradation**: When cache initialization fails (e.g., cannot determine cache dir), log a warning but continue. The resolver gracefully handles `nil` cache by falling through to direct API calls.

2. **Backward compatibility**: The third parameter to `NewGitHubSkillResolverWithCredentials` defaults to creating a new cache when `nil` is passed. This ensures CLI and test code that doesn't need a singleton continue to work.

3. **Placement alongside existing caches**: The `ghResolutionCache` field was added right after `skCache` in the `Server` struct, and initialization follows the same pattern in `initHubIntegration()`. This maintains consistency with the existing caching infrastructure.

4. **TTL increase**: Bumping the default TTL from 5 to 30 minutes reduces API call frequency while still allowing reasonable freshness for branch refs. SHA refs are immutable and can be cached much longer (24h constant added for future Hub-side design).

## Test Coverage

- **Unit test**: `TestGitHubSkillResolver_SharedCacheSingleton` verifies the singleton behavior — two resolvers sharing a cache only make one API call
- **Existing tests**: All existing GitHub resolver tests pass, confirming backward compatibility
- **Integration validation**: Runtime broker tests pass, confirming the singleton is properly wired and doesn't break agent creation

## Build & Test Results

- `go build ./...` — **PASS** (clean build)
- `go vet ./...` — **PASS** (no vet warnings)
- `go test ./pkg/agent/...` — **PASS** (all 17 GitHub resolver tests)
- `go test ./pkg/runtimebroker/...` — **PASS** (runtime broker integration tests)

## Acceptance Criteria Met

✅ **Within-instance deduplication**: On a single broker instance, the second request for the same `gh://` skill makes 0 GitHub API calls (within TTL window)

✅ **No regression**: All existing tests pass, confirming no breaking changes to skill resolution or agent creation

✅ **Build passes**: `go build ./...` and `go vet ./...` both clean

## Gotchas & Notes

- The cache key includes a hash of the credential token (see `resolutionCacheKey` function), so different credentials for the same URI produce separate cache entries. This prevents cross-credential information disclosure.

- File content bytes (`ResolvedFile.Content`) are NOT persisted to disk (tag `json:"-"`). Only resolution metadata (commitSHA, file list, URLs, bundle hash) is cached. This is intentional — the disk cache saves the two rate-limited REST API calls, while file delivery still goes through `raw.githubusercontent.com` (GitHub's CDN).

- The singleton cache lives for the broker process lifetime. On Cloud Run (ephemeral instances), this provides within-instance deduplication but not cross-instance deduplication. Phase 2 (Hub-side DB cache) addresses that.

## Next Steps (Future Phases)

- **Phase 2**: Hub DB schema and resolver implementation (Hub-side shared cache across all broker instances)
- **Phase 3**: Broker routing change (route `gh://` URIs to Hub resolver, fallback to local on Hub error)

## Dependencies

- This implementation was completed on a branch rebased on top of PR #878 (Track A), as required by the design document
- No external dependencies added — only existing packages used
