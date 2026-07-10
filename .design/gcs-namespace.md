# Design: GCS Bucket Path Namespacing with Hub ID

**Issue:** [#404](https://github.com/ptone/scion/issues/404)
**Status:** Design
**Author:** hs-arch2 agent
**Date:** 2026-07-10
**Foundation:** PR #667 (hub_id/hub_name from #392) merged into main

---

## Summary

Namespace all GCS storage paths by `hub_id` to prevent cross-hub interference when multiple hub instances share a GCS bucket. Each hub gets exclusive ownership of its storage partition:

```
gs://{bucket}/hubs/{hub-id}/{resource-kind}/{scope}/{slug}/
```

This eliminates the DB/GCS desync problem where Hub A's uploads silently invalidate Hub B's manifest hashes, causing agent dispatch failures.

---

## 1. Current State

### 1.1 GCS Path Structure

All GCS paths are constructed by `ResourceStoragePath()` in `pkg/storage/storage.go:234` — the single source of truth. Paths follow a `{kind}/{scope}/{slug}` layout with no hub partitioning:

```
templates/global/{slug}/
templates/groves/{projectID}/{slug}/
templates/users/{userID}/{slug}/
harness-configs/global/{slug}/
harness-configs/groves/{projectID}/{slug}/
skills/global/{slug}/
workspaces/{groveID}/{agentID}/
```

### 1.2 Storage Interface

The `Storage` interface (`pkg/storage/storage.go:138`) abstracts GCS/local backends with `Upload`, `Download`, `Delete`, `List`, `GenerateSignedURL`, etc. The `storage.Config` struct carries `Provider`, `Bucket`, and credentials — but no hub_id.

### 1.3 DB Manifest Model

Each `Template` and `HarnessConfig` DB record stores:
- `StoragePath` — the path within the bucket (e.g. `templates/global/claude-agent`)
- `StorageBucket` — the bucket name
- `StorageURI` — full URI (`gs://bucket/path/`)
- `Files` — inline JSON manifest of `[]TemplateFile{Path, Size, Hash, Mode}`
- `ContentHash` — aggregate SHA-256 of all file hashes

The DB is authoritative for metadata; GCS holds the file bytes. Object paths are `StoragePath + "/" + TemplateFile.Path`.

### 1.4 Hub ID Availability

`hub_id` is a Layer-0 setting resolved at startup via `cfg.Hub.ResolveHubID()` (`pkg/config/hub_config.go:96`). It defaults to `SHA256(hostname)[:12]` or accepts any explicit string slug. It flows through:

```
settings.yaml / env SCION_SERVER_HUB_ID
  → HubServerConfig.HubID
  → hub.ServerConfig.HubID
  → hub.Server.hubID (private) / .HubID() (public accessor)
```

Currently used for secret namespacing and telemetry. **Not used for GCS paths.**

---

## 2. The Problem

### 2.1 Shared Bucket, No Partitioning

When multiple hub instances share a GCS bucket, they all read and write the same paths. There is no mechanism to prevent Hub A from overwriting Hub B's files or vice versa.

### 2.2 The DB/GCS Desync Failure

**Failure sequence:**

1. Hub A bootstraps `harness-configs/global/claude/` — uploads files to GCS, records SHA-256 hashes in its DB
2. Hub B bootstraps the same path — overwrites Hub A's files with its own version (possibly different content), records its own hashes
3. Hub A dispatches an agent — sends its DB hashes (now stale relative to GCS) to the runtime broker
4. Broker downloads from GCS (gets Hub B's content), verifies hash against Hub A's expected hash
5. Hash mismatch detected in `templatecache/resolver.go:110` → agent fails to start

**Variations:**
- **Startup bootstrap race:** Both hubs bootstrap on startup, last writer wins in GCS
- **Template sync:** `scion template sync` from one hub overwrites another's content
- **File-level edits:** Individual file PUT via template/HC file handlers overwrites shared paths

### 2.3 Existing Self-Healing (Band-Aid)

The codebase has reactive and proactive repair mechanisms (`pkg/hub/harness_config_repair.go`):

- **Reactive:** On dispatch hash-mismatch error, `repairHashMismatch()` in `httpdispatcher.go` syncs DB manifest from actual GCS content, then retries. Uses `singleflight.Group` for dedup.
- **Proactive:** At startup, `SyncAllHarnessConfigsFromStorage()` / `SyncAllTemplatesFromStorage()` validate all resources against GCS and repair mismatches.

**Why this is insufficient:**
- First dispatch after a conflict always fails (repair is reactive)
- Repair syncs DB to GCS, but GCS may hold the wrong hub's content — repair "succeeds" with incorrect data
- No ownership model — repair cannot distinguish "my content" from "another hub's content"
- Race windows between upload and repair
- Startup sync is async — may not complete before first dispatch

---

## 3. Namespaced Path Design

### 3.1 New Path Structure

Prefix all resource storage paths with `hubs/{hub-id}/`:

```
Current:  {kind}/{scope}/{slug}/
Proposed: hubs/{hub-id}/{kind}/{scope}/{slug}/
```

**Examples:**

| Resource | Current Path | Namespaced Path |
|---|---|---|
| Global template | `templates/global/claude-agent/` | `hubs/prod-hub/templates/global/claude-agent/` |
| Project HC | `harness-configs/groves/p-1/custom/` | `hubs/prod-hub/harness-configs/groves/p-1/custom/` |
| Skill | `skills/global/deploy/` | `hubs/prod-hub/skills/global/deploy/` |
| Workspace | `workspaces/g-1/agent-abc/` | `hubs/prod-hub/workspaces/g-1/agent-abc/` |

Full URI: `gs://my-bucket/hubs/prod-hub/templates/global/claude-agent/`

### 3.2 Injection Point

The prefix is injected at the `ResourceStoragePath()` function — the single source of truth. The change is minimal:

```go
func ResourceStoragePath(hubID string, kind ResourceKind, scope, scopeID, slug string) string {
    prefix := resourcePrefix(kind)
    var scopePath string
    switch scope {
    case "global":
        scopePath = prefix + "/global/" + slug
    case "grove", "project":
        scopePath = prefix + "/groves/" + scopeID + "/" + slug
    case "user":
        scopePath = prefix + "/users/" + scopeID + "/" + slug
    default:
        scopePath = prefix + "/" + slug
    }
    if hubID != "" {
        return "hubs/" + hubID + "/" + scopePath
    }
    return scopePath
}
```

When `hubID` is empty (e.g., local-only development), paths remain unchanged. This preserves backward compatibility for single-hub deployments that don't configure `hub_id`.

### 3.3 Workspace Paths

Workspace paths (`WorkspaceStoragePath`, `ProjectWorkspaceStoragePath`) are independent functions that also need namespacing:

```go
func WorkspaceStoragePath(hubID, groveID, agentID string) string {
    path := "workspaces/" + groveID + "/" + agentID
    if hubID != "" {
        return "hubs/" + hubID + "/" + path
    }
    return path
}
```

### 3.4 Hub ID Propagation

Two options for threading hub_id to path construction call sites:

**Option A: Add hubID parameter to all path functions (recommended)**

All path functions gain a `hubID` first parameter. Callers in hub handlers get it from `s.HubID()`. Callers in the runtime broker get it from the dispatch request or broker config.

Pros: Explicit, no hidden state, easy to test
Cons: Changes ~18 call sites, slightly more verbose

**Option B: Store hubID on Storage struct**

Add `HubID` to `storage.Config` and store it on the `Storage` implementation. Path functions become methods on a `PathBuilder` that carries hub_id.

Pros: Fewer call-site changes
Cons: Mixes path logic with I/O abstraction, harder to test path construction in isolation

**Recommendation: Option A.** The path functions are pure and testable. Adding a parameter is a mechanical change. The `Storage` interface should remain focused on I/O operations.

### 3.5 StoragePath in DB Records

DB records store the computed `StoragePath`. After namespacing:
- New records store `hubs/{hub-id}/templates/global/slug`
- Existing records still have `templates/global/slug`
- All GCS operations should use the DB-stored `StoragePath` (not recompute it) — this is already the pattern in most handlers
- Path recomputation only happens during Bootstrap and repair when `StoragePath` is empty

---

## 4. Migration Strategy

### 4.1 Options Evaluation

| Option | Description | Pros | Cons |
|---|---|---|---|
| **Copy-on-first-write** | On first startup with hub-id, copy existing global objects to hub-id prefix | Automatic, no admin action | Expensive for large buckets; race with other hubs; unclear "first startup" detection |
| **Admin migration command** | `scion hub migrate-storage --hub-id <id>` | Explicit, controllable, auditable | Requires manual step; must handle in-flight operations |
| **Backward-compat read** | Read from hub-id prefix first, fall back to legacy global path | Graceful transition, no data movement needed | Indefinite legacy support; two reads per miss; complex to reason about which content you're getting |

### 4.2 Recommendation: Backward-Compatible Read + Admin Migration Command

Use a hybrid approach:

**Phase 1 — Backward-compatible read (automatic):**
- **Writes** always go to the namespaced path (`hubs/{hub-id}/...`)
- **Reads** check namespaced path first; on `ErrNotFound`, fall back to the legacy (un-namespaced) path
- This ensures zero downtime: existing content is immediately accessible, new content goes to the right place
- The `StoragePath` field on DB records is updated to the namespaced path on next write/sync

**Phase 2 — Admin migration command (optional, for cleanup):**
- `scion admin migrate-storage` copies all legacy-path objects to the hub's namespaced prefix
- Updates all DB records' `StoragePath`, `StorageURI` fields to the namespaced paths
- After migration, the legacy fallback can be disabled via a config flag
- Migration is idempotent and safe to re-run

### 4.3 Migration Flow Details

**Startup behavior:**
1. Hub resolves `hub_id`
2. `ResourceStore.Bootstrap()` constructs the new namespaced path for each resource
3. Uploads go to `hubs/{hub-id}/{kind}/{scope}/{slug}/`
4. DB record updated with new `StoragePath` and `StorageURI`
5. Legacy objects in the old path are not touched (they'll be cleaned up by explicit migration)

**Read fallback during transition:**
```go
func (rs *ResourceStore) resolveStoragePath(ctx context.Context, stor storage.Storage, 
    storagePath string, kind ResourceKind, scope, scopeID, slug, hubID string) string {
    // If record already has a namespaced path, use it
    if strings.HasPrefix(storagePath, "hubs/") {
        return storagePath
    }
    // Check if content exists at the namespaced path
    namespacedPath := storage.ResourceStoragePath(hubID, kind, scope, scopeID, slug)
    if exists, _ := stor.Exists(ctx, namespacedPath + "/"); exists {
        return namespacedPath
    }
    // Fall back to legacy path
    return storagePath
}
```

**Admin migration command:**
```
scion admin migrate-storage [--dry-run]
```
- Lists all templates, harness-configs, and skills from DB
- For each resource with a legacy (un-namespaced) `StoragePath`:
  - Copies all files from legacy path to namespaced path in GCS
  - Updates DB record with new `StoragePath`, `StorageURI`
- Reports count of migrated/skipped resources
- `--dry-run` shows what would be migrated without making changes

### 4.4 Single-Hub Deployments

When `hub_id` is not explicitly configured and defaults to `SHA256(hostname)[:12]`:
- The namespacing still applies — this is intentional for future-proofing
- Single-hub deployments with no shared bucket see no functional change (just longer paths)
- To opt out entirely: leave hub_id unset AND don't use shared GCS buckets → paths remain unchanged (the `hubID != ""` guard in `ResourceStoragePath` would need hub_id to always be set when GCS is configured)

**Decision point:** Should we enforce that `hub_id` is always set when `storage-bucket` is configured? This would prevent accidental unnamespaced deployments but adds a breaking requirement.

**Recommendation:** Yes, when a GCS bucket is configured, require `hub_id` to be explicitly set (or auto-default). Log a startup warning if using auto-generated hub_id with a shared bucket.

---

## 5. Impact Analysis

### 5.1 Code Paths That Construct or Parse GCS Paths

**Core path functions (must change signature):**

| Function | File:Line | Change |
|---|---|---|
| `ResourceStoragePath()` | `pkg/storage/storage.go:234` | Add `hubID` first param |
| `ResourceStorageURI()` | `pkg/storage/storage.go:249` | Add `hubID` first param |
| `TemplateStoragePath()` | `pkg/storage/storage.go:255` | Add `hubID` first param |
| `TemplateStorageURI()` | `pkg/storage/storage.go:260` | Add `hubID` first param |
| `HarnessConfigStoragePath()` | `pkg/storage/storage.go:277` | Add `hubID` first param |
| `HarnessConfigStorageURI()` | `pkg/storage/storage.go:282` | Add `hubID` first param |
| `SkillStoragePath()` | `pkg/storage/storage.go:266` | Add `hubID` first param |
| `SkillStorageURI()` | `pkg/storage/storage.go:271` | Add `hubID` first param |
| `WorkspaceStoragePath()` | `pkg/storage/storage.go:288` | Add `hubID` first param |
| `ProjectWorkspaceStoragePath()` | `pkg/storage/storage.go:295` | Add `hubID` first param |
| `WorkspaceStorageURI()` | `pkg/storage/storage.go:300` | Add `hubID` first param |

**Call sites that must pass hubID (hub-side, have access to `s.HubID()`):**

| # | File:Line | Context |
|---|---|---|
| 1 | `pkg/hub/resource_store.go:151` | Bootstrap new resource |
| 2 | `pkg/hub/resource_store.go:196` | Bootstrap existing resource |
| 3 | `pkg/hub/resource_source.go:299` | Resource import (new) |
| 4 | `pkg/hub/resource_source.go:385` | Resource import (existing) |
| 5 | `pkg/hub/resource_source.go:435` | Resource import (update) |
| 6 | `pkg/hub/resource_validate.go:78` | Storage validation |
| 7 | `pkg/hub/harness_config_repair.go:58` | Repair path resolution |
| 8 | `pkg/hub/template_handlers.go:292` | Template create |
| 9 | `pkg/hub/template_handlers.go:846` | Template clone |
| 10 | `pkg/hub/harness_config_handlers.go:198` | HC create |
| 11 | `pkg/hub/harness_config_handlers.go:806` | HC clone |
| 12 | `pkg/hub/skill_handlers.go:385` | Skill create |

**Call sites on the broker side (need hubID from dispatch request):**

| # | File:Line | Context |
|---|---|---|
| 13 | `pkg/runtimebroker/handlers.go:988` | Broker resolving HC path |
| 14 | `pkg/runtimebroker/handlers.go:1004` | Broker resolving template path |

### 5.2 DB Records

Existing DB records have un-namespaced `StoragePath` values. These are used directly (not recomputed) in most read paths. The backward-compatible read strategy handles this: existing records work against both legacy and namespaced GCS paths.

On next write/sync, `StoragePath` is updated to the namespaced value. The admin migration command can bulk-update all records.

### 5.3 Self-Healing Repair

The repair mechanism in `harness_config_repair.go` already uses the DB-stored `StoragePath` when available, and falls back to `ResourceStoragePath()` when empty. After namespacing:
- The fallback path will generate the namespaced path (correct)
- The DB-stored path will be namespaced for new/updated records (correct)
- For legacy records not yet migrated, the DB-stored path is un-namespaced — repair will work against the legacy path (correct during transition)

### 5.4 Runtime Broker

The broker in `pkg/runtimebroker/handlers.go` reconstructs storage paths using `storage.ResourceStoragePath()`. It needs `hub_id` to construct the correct namespaced path.

Options:
- Pass `hub_id` in the dispatch request from hub to broker (preferred — the hub knows its own ID)
- Configure `hub_id` on the broker (problematic — broker may serve multiple hubs)

**Recommendation:** Include `hub_id` (or the already-computed `StoragePath`) in the agent dispatch payload. The hub already includes `StoragePath` on the DB record; the broker should use that rather than recomputing.

### 5.5 Signed URLs

Signed URLs are generated from the storage path. No change needed to signing logic — only the object path changes. Existing signed URLs (in-flight) will continue to work against legacy paths during transition.

### 5.6 Tests

`pkg/storage/storage_test.go` has comprehensive path construction tests. These must be updated:
- Add `hubID` parameter to all test cases
- Add test cases for empty hubID (legacy behavior)
- Add test cases for namespaced paths

---

## 6. Phased Implementation Plan

### Phase 1: Path Function Signature Change (No Runtime Behavior Change)

**Goal:** Update all path functions to accept `hubID` parameter. Pass empty string everywhere — no runtime change.

**Changes:**
1. Add `hubID` first parameter to `ResourceStoragePath()` and all wrapper functions
2. Update all ~14 hub-side call sites to pass `s.HubID()` (initially, pass `""` to preserve behavior)
3. Update 2 broker-side call sites to accept hub_id (initially `""`)
4. Update all tests

**Risk:** Low — purely mechanical, no behavior change.

### Phase 2: Enable Namespaced Writes

**Goal:** New and re-synced resources write to namespaced paths. Reads remain backward-compatible.

**Changes:**
1. Pass `s.HubID()` (non-empty) at all hub-side call sites
2. Update `ResourceStore.Bootstrap()` to use namespaced paths for new resources
3. Update template/HC create handlers to use namespaced paths
4. Update DB records on create/update with namespaced `StoragePath`, `StorageURI`
5. Add startup warning when GCS is configured without explicit `hub_id`

**Test plan:** Create template/HC on hub with explicit hub_id → verify GCS objects at `hubs/{id}/...` path.

### Phase 3: Backward-Compatible Read Fallback

**Goal:** Reads check namespaced path first, fall back to legacy path for un-migrated resources.

**Changes:**
1. Implement read fallback in download URL generation (storage_helpers.go)
2. Implement read fallback in resource validation (resource_validate.go)
3. Implement read fallback in repair sync (harness_config_repair.go)
4. Thread hub_id through broker dispatch request or use DB-stored `StoragePath`

**Test plan:** Start hub with legacy data → verify reads resolve from legacy paths. Create new resource → verify reads use namespaced path.

### Phase 4: Admin Migration Command

**Goal:** Provide a tool to migrate legacy data to namespaced paths.

**Changes:**
1. Add `scion admin migrate-storage` command
2. Implement GCS copy from legacy to namespaced paths
3. Bulk-update DB records with namespaced `StoragePath`
4. Add `--dry-run` flag
5. Add progress reporting

**Test plan:** Run migration on test data → verify objects copied, DB updated, no data loss.

### Phase 5: Cleanup and Hardening

**Goal:** Remove legacy fallback after migration window, harden configuration.

**Changes:**
1. Add config flag to disable legacy fallback (`storage.legacy_fallback: false`)
2. Require explicit `hub_id` when GCS bucket is configured
3. Remove self-healing repair logic that was compensating for cross-hub interference (or simplify it)
4. Documentation: update HA deployment guide, add migration guide

---

## 7. Open Questions

### 7.1 Should workspaces be namespaced too?

Workspaces (`workspaces/{groveID}/{agentID}`) are agent-specific and already scoped by agent ID. However, if two hubs share a bucket, workspace paths could still collide if grove/agent IDs overlap (unlikely with UUIDs, but possible with legacy naming).

**Recommendation:** Yes, namespace workspaces for consistency and defense-in-depth. The change is mechanical — same pattern as resource paths.

### 7.2 Should hub_id be required when using GCS?

Currently hub_id auto-defaults to `SHA256(hostname)[:12]`. For namespacing to be effective, all hub instances sharing a bucket must use different hub_ids. Auto-generated IDs achieve this (different hostnames → different IDs), but are opaque.

**Recommendation:** When `storage-bucket` is configured, log a startup warning if hub_id is auto-generated. Encourage explicit hub_id in HA deployments. Don't make it a hard requirement to avoid breaking existing single-hub deployments.

### 7.3 What about cross-hub sharing?

Some deployments may intentionally want hubs to share templates (e.g., a staging hub using production templates). Namespacing prevents this by default.

**Recommendation:** Defer cross-hub sharing to a future design. If needed, it could be implemented via:
- A `shared/` prefix in the bucket that any hub can read (but not write)
- Template import-from-URL that pulls from another hub's GCS path
- A "template federation" API between hubs

For now, each hub is self-contained. Operators who need shared content can configure the same templates on each hub.

### 7.4 How does this interact with the broker?

The broker currently recomputes storage paths using `storage.ResourceStoragePath()` in `pkg/runtimebroker/handlers.go`. It needs hub_id to compute the correct namespaced path.

**Recommendation:** The hub should include `StoragePath` in the dispatch payload to the broker (it's already stored on the DB record). The broker should use the hub-provided path rather than recomputing. This avoids the broker needing hub_id configuration and handles the case where one broker serves multiple hubs.

### 7.5 Skills — same treatment?

Skills use the same `ResourceStoragePath()` function and would automatically gain namespacing. No special handling needed.

### 7.6 Local storage provider — does namespacing apply?

For `ProviderLocal` (development), namespacing would create nested directories under `~/.scion/storage/hubs/{hub-id}/...`. This is harmless and provides consistency, but may be unnecessarily deep for single-hub dev setups.

**Recommendation:** Apply namespacing uniformly for all providers. Local development typically uses default hub_id, so paths are predictable. The extra directory depth is a minor cosmetic issue.
