# Design: gh:// Resolution Cache Durability (Track B)

**Author:** ps-cache-arch-b  
**Date:** 2026-07-27  
**Status:** Approved by user — ready for implementation (pending PR #878 merge)  
**Design doc destination (in PR):** `.design/ps-cache-durability.md`

---

## Problem & Goals

The GitHub skill resolution cache (`GitHubResolutionCache`) is per-process, ephemeral, and unshared. On Cloud Run, each broker instance has a permanently cold cache, causing `resolveCommitSHA` — a rate-limited GitHub REST API call — to be made on every agent-creation request. With a shared outbound IP (Cloud NAT), 10 concurrent broker instances each making 2 API calls per skill can exhaust the 60/hr unauthenticated limit in minutes, and stresses the 5000/hr authenticated limit at scale.

**Root causes (from `ps-cache-inv` investigation):**

1. `GitHubSkillResolver` is instantiated per-request — disk cache is reloaded from JSON on every request, even within the same instance.
2. Cache is local to each broker instance — N instances = N independent cold caches for the same URI.
3. Disk cache is ephemeral on Cloud Run — lost on instance restart.

**Note on Track A / PR #878:** Track A (upstream PR #878, `scion/project-skills-cache-auth-fix`) is addressing the immediate auth-gap symptom and the full-SHA short-circuit. Do not duplicate those changes. PR #878 makes three changes relevant to Track B:
1. `resolveCommitSHA` short-circuits for full 40-char SHA refs — after #878 lands, SHA refs never reach the Hub's resolve path from Phase 3 (they short-circuit at the broker). The Hub-side SHA TTL constant is still worth defining for completeness.
2. Cache-init error is now logged — Phase 1's singleton approach moves this from a per-request log to a startup log; both are additive.
3. `provisionCredentials["GITHUB_TOKEN"]` is now a general fallback token — Phase 3's fallback resolver inherits this behavior.

**Track B must be implemented on a branch rebased onto main after PR #878 merges.** The Phase 1 singleton change touches the same files as #878 (github_skill_resolver.go) — rebase is mandatory before starting Track B implementation.

**Goals:**

- Eliminate per-request cold-cache loads within a single broker instance (within-instance fix).
- Eliminate duplicate GitHub API calls across multiple broker instances (cross-instance fix).
- Make the resolution cache durable across broker instance restarts.
- HA-compatible: cache survives Hub restarts (DB-backed, not in-memory).
- No change to how skill files reach agent containers — this design only affects the metadata resolution step.

---

## Non-Goals

- Full-SHA short-circuit (Track A).
- Cache init error logging (Track A).
- Auth gap fix (Track A).
- Changing skill file delivery into agent containers (bind-mount for Docker, tar-stream via K8s exec for GKE — these are correct and unchanged).
- Rate-limit observability / alerting.
- Caching skill file bytes at the Hub level (Hub returns URLs; brokers download files from GitHub CDN as they do today).

---

## Background: The Three-Cache Architecture

Understanding what each cache does is essential — the investigator's findings contain a key code detail worth restating here.

| Cache | Location | Keyed by | Stores | Survives restart? |
|---|---|---|---|---|
| `GitHubResolutionCache` | Broker disk (`~/.scion/cache/github-resolution/`) | `sha256(URI + token_hash)` | Resolution metadata: commitSHA, file list, URLs, bundle hash. **File bytes are `json:"-"` — not persisted.** | No (Cloud Run ephemeral). |
| `skCache` (`templatecache.Cache`) | Broker disk (`~/.scion/cache/skills/`) | Bundle content hash | Actual skill file bytes, content-addressed. Already a server-level singleton. | No (Cloud Run ephemeral). |
| In-memory map | Per `GitHubSkillResolver` instance | Same as resolution cache | Same as resolution cache, but ephemeral per-request. | N/A — destroyed per request today. |

**Critical finding:** `ResolvedFile.Content` has tag `json:"-"` — file bytes are never written to the disk resolution cache. After a broker restart, a resolution cache HIT returns the metadata but `Content == nil`, so `installOneSkill` falls through to download files from `raw.githubusercontent.com` (GitHub's CDN, which is not subject to the same rate limits as the REST API). The resolution cache's sole job is to save the two rate-limited REST API calls: `GET /repos/{owner}/{repo}/commits/{ref}` and `GET /repos/{owner}/{repo}/contents/{path}`.

**What Track B needs to cache:** Only the resolution metadata (commitSHA, file list, URLs, bundle hash) — less than 1 KB per skill. File delivery to containers is unchanged.

---

## Proposed Design

### Approach: Hub-Side Shared Resolution Cache (Option C)

All broker instances route `gh://` skill resolution through the Hub's existing `/api/v1/skills/resolve` endpoint. The Hub maintains a DB-backed (PostgreSQL via ent ORM) cache of `(uri, token_scope) → {commitSHA, file_list}`. The Hub calls GitHub API only on cache misses. Since all brokers share one Hub, all instances share one resolution cache. The DB backend makes the cache durable across Hub restarts.

This design has three sub-changes that can be deployed independently in sequence.

---

### Sub-change 1 (Phase 1): Broker Singleton Resolution Cache

**What it fixes:** Per-request cold disk loads within a single instance.  
**Independent of Hub changes.** Deployable alone.

**Current code path (handlers.go:759):**
```go
// Per-request: loads JSON from disk every time
ghResolver := agent.NewGitHubSkillResolverWithCredentials(defaultGHToken, req.ProvisionCredentials)
```

`NewGitHubSkillResolver()` calls `NewGitHubResolutionCache()` which calls `c.load()` (disk read) on every request. The result is a fresh, ephemeral in-memory cache that's discarded after the request.

**Change:**

Add `ghResolutionCache *agent.GitHubResolutionCache` to the `server` struct alongside `skCache` and `hcCache`. Initialize once at startup in `initHubIntegration`. Pass the singleton into `NewGitHubSkillResolverWithCredentials`.

```pseudocode
// server struct:
ghResolutionCache *agent.GitHubResolutionCache

// initHubIntegration:
ghResDir, err := agent.GitHubResolutionCacheDir()
if err != nil {
    slog.Warn("github resolution cache: cannot determine cache dir", "error", err)
} else {
    cache, err := agent.NewGitHubResolutionCache(ghResDir, agent.DefaultResolutionCacheTTL)
    if err != nil {
        slog.Warn("github resolution cache: init failed (running uncached)", "error", err)
        // nil cache is safe — resolver falls through to API call
    }
    s.ghResolutionCache = cache
}

// handlers.go:
ghResolver := agent.NewGitHubSkillResolverWithCredentials(
    defaultGHToken, req.ProvisionCredentials, s.ghResolutionCache,
)
```

`NewGitHubSkillResolverWithCredentials` signature gains an optional `*GitHubResolutionCache` parameter (or the singleton is wired via a setter). The cache's `mu sync.RWMutex` already makes it safe for concurrent goroutines.

**TTL consideration:** Also increase `DefaultResolutionCacheTTL` from 5 minutes to 30 minutes for branch refs. For SHA refs, Track A's short-circuit means Hub is never called, so TTL is moot. A separate `DefaultSHAResolutionCacheTTL = 24h` constant can be added for completeness when the Hub-side design lands.

**Effect:** Within one instance's lifetime, the second request for the same skill URI makes 0 GitHub API calls. The first request still makes 2 (resolveCommitSHA + listContents) on a cold start.

---

### Sub-change 2 (Phase 2): Hub DB Schema and Resolver

#### New ent Schema Entity

```go
// pkg/ent/schema/github_resolution_cache.go
type GitHubResolutionCache struct { ent.Schema }

func (GitHubResolutionCache) Fields() []ent.Field {
    return []ent.Field{
        field.String("cache_key").NotEmpty().Unique(),  // sha256(uri + ":" + token_scope)
        field.String("original_uri").NotEmpty(),         // for debugging
        field.String("commit_sha").NotEmpty(),
        field.JSON("file_entries", []GitHubFileEntry{}), // [{path, url, hash, size}, ...]
        field.String("bundle_hash").NotEmpty(),
        field.String("token_scope").Default("public"),   // GitHub App installation ID or "public"
        field.Time("expires_at"),
        field.Time("create_time").Default(time.Now).Immutable(),
    }
}

func (GitHubResolutionCache) Indexes() []ent.Index {
    return []ent.Index{
        index.Fields("cache_key").Unique(),
        index.Fields("expires_at"),  // for efficient TTL eviction
    }
}

func (GitHubResolutionCache) Annotations() []schema.Annotation {
    return []schema.Annotation{
        entsql.Annotation{Table: "github_resolution_cache"},
    }
}
```

`GitHubFileEntry` is a value type (not ent entity): `{Path, URL, Hash string; Size int64}`. Stored as JSONB on Postgres, TEXT (JSON string) on SQLite.

**SQLite compatibility:** Phase 2 works transparently on both SQLite (single-node Hub deployments) and PostgreSQL (HA deployments). The ent `field.JSON()` type maps to JSONB on Postgres and TEXT on SQLite — identical to the existing `HubSetting.value` field pattern. The eviction query only filters by `expires_at` (a standard TIME field); no Postgres-specific JSONB operators are used. The `Get`/`Put` operations are plain row lookups by primary key. SQLite's single-writer model is fine for single-node deployments; Postgres handles concurrent Hub instances for HA.

#### Cache Key Design

```
cache_key = sha256hex(normalized_uri + ":" + token_scope_id)

normalized_uri = "gh://{owner}/{repo}/{path}@{ref}"  // lowercased owner/repo
token_scope_id = GitHub App installation ID (stable, not the rotating token value)
               = "public" for unauthenticated/public repos
```

**Why installation ID not token hash:** App tokens rotate every hour. The installation ID is stable for the lifetime of a GitHub App installation on a repo. Using it as the scope ID ensures cache entries remain valid across token rotations. For public repos, `"public"` is used (no credential).

#### Hub Server Changes

Add to `Server` struct:
```pseudocode
ghResolutionStore *GitHubResolutionStore  // nil if DB not available
```

Initialize in `Server.Start()` or `initHubIntegration`.

Add `resolveGitHubSkill(ctx, uri, projectID string) (*ResolvedSkillResponse, error)`:

```pseudocode
func (s *Server) resolveGitHubSkill(ctx, rawURI, projectID string) (*ResolvedSkillResponse, error) {
    // 1. Parse gh:// URI (owner, repo, path, ref)
    ghRef, err := parseGitHubSkillURI(rawURI)

    // 2. Determine token scope
    installID, token, err := s.resolveGitHubToken(ctx, projectID)
    // installID = "public" if no App integration for this project
    // token = minted App installation token, or ""

    // 3. Cache lookup
    cacheKey = computeCacheKey(ghRef, installID)
    if entry, ok := s.ghResolutionStore.Get(ctx, cacheKey); ok {
        return entryToResponse(entry), nil
    }

    // 4. Cache miss: call GitHub API
    commitSHA, fileList, bundleHash, err := resolveViaGitHubAPI(ctx, ghRef, token)
    
    // 5. Determine TTL
    ttl = branchRefTTL  // default 30 min, from Hub admin settings
    if isFullSHA(ghRef.Ref) { ttl = shaRefTTL }  // 24h

    // 6. Store and return
    s.ghResolutionStore.Put(ctx, cacheKey, {commitSHA, fileList, bundleHash, installID, expiresAt: now+ttl})
    return buildResponse(ghRef, commitSHA, fileList, bundleHash), nil
}
```

Wire into `handleSkillsResolve` before the existing `resolveSkill` call:

```pseudocode
// In the per-skill loop in handleSkillsResolve:
if strings.HasPrefix(skillRef.URI, "gh://") {
    resolved, err := s.resolveGitHubSkill(ctx, skillRef.URI, req.ProjectID)
    if err != nil {
        resolveErrors = append(resolveErrors, ResolveSkillError{...})
    } else {
        resolved = append(resolved, *resolved)
    }
    continue
}
// existing registry-skill resolution follows
```

#### Token Resolution on Hub

```pseudocode
func (s *Server) resolveGitHubToken(ctx, projectID string) (installID, token string, err error) {
    if projectID == "" {
        return "public", "", nil
    }
    project, err := s.store.GetProject(ctx, projectID)
    if project.GitHubInstallationID == nil {
        return "public", "", nil
    }
    token, _, err = s.mintGitHubAppToken(ctx, project)
    installID = strconv.FormatInt(*project.GitHubInstallationID, 10)
    return installID, token, err
}
```

`mintGitHubAppToken` already exists on the Hub server struct (`handlers_github_app_webhook.go:704`). This reuses the exact same mechanism used by `httpdispatcher.go` for agent creation.

#### Background TTL Eviction

Start a goroutine in Hub startup:

```pseudocode
go func() {
    ticker := time.NewTicker(10 * time.Minute)
    for range ticker.C {
        if err := s.ghResolutionStore.PurgeExpired(ctx); err != nil {
            slog.Warn("github_resolution_cache: eviction failed", "error", err)
        }
    }
}()
```

`PurgeExpired` deletes rows where `expires_at < now()`. Max expected table size: (number of distinct skill URIs × projects) × row size ≈ negligible.

#### Hub Admin Settings

Add two new keys to Hub admin settings (existing `hub_settings` table):

```json
{
  "github_resolution_cache": {
    "branch_ref_ttl_minutes": 30,
    "sha_ref_ttl_hours": 24,
    "enabled": true
  }
}
```

These can be read at resolve time; the defaults ship in the seed data.

---

### Sub-change 3 (Phase 3): Broker Routing Change

Remove local `GitHubSkillResolver` as the primary handler for the `"gh"` URI scheme. `gh://` URIs fall through to `HubSkillResolver`, which calls Hub's `/api/v1/skills/resolve` endpoint.

```pseudocode
// handlers.go — BEFORE:
ghResolver := agent.NewGitHubSkillResolverWithCredentials(defaultGHToken, req.ProvisionCredentials)
router.Register("gh", ghResolver)

// handlers.go — AFTER:
// gh:// URIs fall through to Hub resolver (hub handles them server-side)
// Keep ghResolver available as fallback only:
ghResolver := agent.NewGitHubSkillResolverWithCredentials(defaultGHToken, req.ProvisionCredentials, s.ghResolutionCache)
router.RegisterFallback("gh", ghResolver)  // only called if Hub returns resolve error for gh:// URI
```

**`RegisterFallback` semantics** (new method on `RoutingSkillResolver`, or handled in the `HubSkillResolver` error path): if Hub returns a `not_found` or `resolve_failed` error for a `gh://` URI, retry with the registered fallback resolver. This provides resilience during Hub downtime and gradual rollout.

**`ProjectID` in resolve request:** `HubSkillResolver.Resolve()` must pass `req.ProjectID` in `ResolveSkillsRequest.ProjectID`. Verify this is already threaded through; if not, add it. The Hub needs `ProjectID` to mint the App token.

---

## Alternatives Considered

### B1: Full-SHA Short-Circuit Only (Already in Track A)

Eliminates API calls for pinned SHA refs (the most common production pattern). Does not help with branch refs or the per-instance problem. Track B is the correct home for the remaining cross-instance issue.

### B2: Shared Persistent Volume (GCS FUSE or NFS)

Mount the broker's `~/.scion/cache/github-resolution/` on a GCS FUSE volume or Cloud Filestore NFS share. All instances read/write the same JSON file.

**Rejected because:**
1. `os.Rename()` atomicity — the current write pattern (`write tmpfile → rename`) is NOT atomic on GCS FUSE. Concurrent writes from N instances can silently lose entries (last-write-wins on the underlying object), and the tmpfile+rename trick doesn't protect against this on GCS FUSE.
2. NFS avoids the rename issue but introduces network latency on every cache read/write, and requires per-Cloud-Run-service NFS mount provisioning.
3. GCS FUSE has significant per-operation latency (10–100ms) for small random file reads — unacceptable for a cache that's consulted on every request.
4. Does not survive Hub restarts (separate concern, but Hub-side DB handles both broker-restart and Hub-restart durability in one design).

### Option A: Broker Singleton Only (No Hub Changes)

Lift `GitHubResolutionCache` to server-level singleton. Fixes the per-request cold load. Does NOT solve the cross-instance problem — 10 fresh broker instances each build their own independent in-process caches. Adopted as **Sub-change 1 (Phase 1)** as an independent improvement and as the L1 in-process cache in the layered design, but insufficient alone for HA broker fleets.

### Hub In-Memory Cache (DB-less)

Hub maintains a process-level in-memory `sync.Map` for resolution entries. Simpler than DB. Rejected because: (a) multiple Hub instances lose the single-point-of-truth property, (b) Hub restarts clear the cache — cold start amplification happens at Hub restarts too if Hub fleet scales. DB-backed cache satisfies the user's HA requirement.

---

## Migration / Rollout

This design is three independently-deployable phases:

**Phase 1 — Broker singleton (no Hub changes, no DB change)**
- Broker restart needed (not a Hub change).
- Immediately reduces per-request disk I/O within each broker instance.
- Backward-compatible: nil singleton is handled gracefully (resolver falls through to API).
- Also bump `DefaultResolutionCacheTTL` from 5 min → 30 min.

**Phase 2 — Hub DB and resolver (Hub change + DB migration)**
- DB migration: add `github_resolution_cache` table (additive, zero-downtime migration via ent's `AutoMigrate` or explicit migration script).
- Hub instances without the new code simply skip the `gh://` branch — the broker's local fallback (still registered) handles resolution.
- Hub instances with the new code begin populating the cache. Old and new Hub instances can coexist during rollout.
- No broker change at this phase.

**Phase 3 — Broker routing change (broker change, depends on Phase 2 being complete)**
- After Hub is fully updated (Phase 2 complete across all Hub instances), update brokers to prefer Hub for `gh://` resolution.
- Keep fallback to local resolver (Sub-change 1 singleton cache) during rollout for resilience.
- Monitor Hub logs for `gh://` cache hit/miss ratio to validate.
- Once hit rate stabilizes, the fallback can be kept indefinitely (adds resilience, costs nothing in the happy path).

**Forward compatibility:** If Hub is upgraded before the broker, Hub handles `gh://` URIs but brokers haven't changed their routing yet — brokers still call local resolver, Hub's new capability is unused but not harmful.

**Backward compatibility:** If broker is upgraded (Phase 3) before Hub (Phase 2), Hub returns resolve errors for `gh://` URIs, broker falls back to local resolver — no user-visible failure.

---

## Open Questions

1. **Hub instance count:** If Hub itself runs N > 1 instances for HA, the in-memory tier is absent (no `ghResolutionCache` on Hub), and the DB table is the only shared state. This is correct — the DB provides the cross-Hub-instance durability the user required. Confirm Hub instances share a single PostgreSQL DB (expected: yes, standard for HA Postgres deployments).

2. **Monitoring:** The Hub's resolve handler should emit a structured log line distinguishing cache hit vs. miss for `gh://` URIs (`"github_resolution_cache_hit": true/false`). The Phase 3 broker routing change makes these logs the primary observability signal for the new system. Add a metric counter if the Hub has a Prometheus registry.

3. **GKE broker topology:** The investigation focused on Cloud Run brokers. If brokers also run on GKE (separate deployment), the same design applies — GKE broker instances also share the same Hub, so they benefit from Phase 2 and 3 identically.

## Resolved Design Decisions (from user sign-off)

- **Credential passing for `?token=SECRET_NAME` URIs:** Hub has access to project secrets via its existing secrets store. Developer should look up the named secret using `ProjectID` via `s.store` (same access path used elsewhere in the Hub for project secret resolution). If the secret is not found Hub-side, fall back to broker-local resolution.

- **SQLite compatibility:** Phase 2 works on both SQLite and Postgres (see note in ent schema section above).

- **PR #878 sequencing:** Track B implementation starts only after PR #878 merges into upstream main. Branch must be rebased onto post-#878 main before starting Phase 1.

---

## Implementation Phases

### Phase 1 — Broker Singleton Cache (~30 LoC)

Load-bearing files: `pkg/runtimebroker/server.go`, `pkg/runtimebroker/handlers.go`, `pkg/agent/github_skill_resolver.go`

1. Export `githubResolutionCacheDir()` → `GitHubResolutionCacheDir()` (needed by server.go).
2. Add `ghResolutionCache *agent.GitHubResolutionCache` field to `server` struct.
3. Initialize in `initHubIntegration` alongside `skCache` init; log error on failure (don't fatal).
4. Add `resolutionCache *GitHubResolutionCache` parameter to `NewGitHubSkillResolverWithCredentials` (or a `WithResolutionCache(c)` option). Remove the per-request `NewGitHubResolutionCache` call from `NewGitHubSkillResolver`.
5. Pass `s.ghResolutionCache` from `handlers.go`.
6. Bump `DefaultResolutionCacheTTL` from 5m to 30m.
7. Unit test: verify two sequential `resolveOne` calls with same URI produce one disk-cache load.

### Phase 2 — Hub DB Schema and Resolver (~350 LoC + generated ent code)

Load-bearing files: new `pkg/ent/schema/github_resolution_cache.go`, new `pkg/hub/github_resolution_store.go`, `pkg/hub/skill_handlers.go`, `pkg/hub/server.go`

1. Add ent schema entity `GitHubResolutionCache` (fields as above). Run `go generate ./pkg/ent/...` to regenerate ent client.
2. Implement `GitHubResolutionStore`: `Get`, `Put`, `PurgeExpired` backed by ent client.
3. Add `resolveGitHubToken(ctx, projectID)` to Server (reuses `mintGitHubAppToken`).
4. Add `resolveGitHubSkill(ctx, uri, projectID)` to Server.
5. Wire into `handleSkillsResolve`: detect `gh://` prefix, dispatch to `resolveGitHubSkill`, continue.
6. Start background `PurgeExpired` goroutine in Hub startup.
7. Add `github_resolution_cache` section to Hub admin settings seed data (TTL defaults).
8. Unit test: `resolveGitHubSkill` with mock GitHub API and mock DB — verify cache hit skips API call; cache miss populates DB.
9. Integration test (if Hub integration test infra available): two sequential resolve calls for same URI → 1 GitHub API call.

### Phase 3 — Broker Routing Change (~40 LoC + tests)

Load-bearing files: `pkg/runtimebroker/handlers.go`, `pkg/agent/routing_skill_resolver.go` (or wherever `Register` is defined)

1. Add `RegisterFallback(scheme string, resolver SkillResolver)` to `RoutingSkillResolver`, or handle fallback in the Hub error path.
2. In `handlers.go`: stop calling `router.Register("gh", ghResolver)`; call `router.RegisterFallback("gh", ghResolver)` (or equivalent fallback registration).
3. Ensure `HubSkillResolver.Resolve` passes `ProjectID` from resolve opts into `ResolveSkillsRequest.ProjectID`.
4. Update broker integration test: mock Hub returning `gh://` resolution result; verify broker uses it.
5. Update broker integration test: mock Hub returning error for `gh://`; verify broker falls back to local resolver.

---

## Acceptance Criteria

QA tester should verify all of the following before this is considered done:

1. **Cross-instance deduplication (Phase 2+3):** Simulate 10 fresh broker instances resolving the same `gh://` URI simultaneously. Exactly 1 GitHub REST API call is made (observable via Hub access logs or GitHub rate-limit remaining header dropping by 1 not 10). All others return Hub cache hits.

2. **Hub restart durability (Phase 2):** Populate the resolution cache. Restart the Hub. Verify the previously cached entry is still returned (not expired) — confirming DB-backed durability.

3. **Branch-ref TTL (Phase 2):** With branch_ref_ttl = 2 minutes (override for testing): cache an entry at t=0; verify cache hit at t=1m; verify fresh GitHub API call at t=3m and cache updated with new entry.

4. **Within-instance deduplication (Phase 1):** On a single broker instance, create two agents using the same `gh://` skill back-to-back. Verify second creation makes 0 GitHub API calls (within TTL window). First creation still makes 2.

5. **Hub unavailability fallback (Phase 3):** Simulate Hub timeout for `gh://` resolve. Verify broker logs a warning and falls back to local resolution, returning a valid skill result to the agent creation request.

6. **Private repo skill with App token (Phase 2):** A skill using `gh://` on a private repo, where the project has a GitHub App installation, resolves successfully via the Hub using the minted App token. Cache is populated under the installation ID scope.

7. **Log observability:** Hub emits a distinguishable log entry for each `gh://` resolve with `cache_hit: true/false` (or equivalent). Hit rate trend visible after traffic runs through the new code.

8. **No regression for Hub-registered skills:** Hub-registered skills (`scion://`, `project://` etc.) resolve identically to before — the new `gh://` branch does not interfere with the existing `resolveSkill` path.
