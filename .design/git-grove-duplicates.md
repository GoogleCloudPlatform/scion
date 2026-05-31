# Git Grove Duplicates: Multiple Groves per Git Repository

**Created:** 2026-03-28
**Status:** Draft
**Related:** `hosted/git-groves.md`, `hosted/hub-groves.md`, `hosted/hosted-architecture.md`

---

## 1. Overview

Today, the system enforces a **1:1 relationship between git remote URLs and groves**. This is implemented through:

1. **Deterministic UUID5 IDs** — `HashGroveID(NormalizeGitRemote(url))` always produces the same grove ID for a given repository, making grove creation idempotent.
2. **UNIQUE constraint on `git_remote`** in the database schema, preventing multiple groves from sharing the same normalized remote URL.
3. **Lookup-by-git-remote** in the registration flow, which returns an existing grove rather than creating a new one when the URL matches.
4. **Suppressed "Register as new" option** in the linking dialog when `hasGitRemote=true`.

This design worked well for the initial model where one grove = one project = one repository. However, it prevents teams from running **multiple independent agent groups against the same repository** — a critical use case for larger projects where separate feature teams should work in isolation.

### Motivating Use Case

A team working on `github.com/acme/widgets` wants three independent groves:

- `acme-widgets` — mainline development, general-purpose agents
- `acme-widgets-1` — Team A working on the auth rewrite
- `acme-widgets-2` — Team B working on the API v2 migration

Each grove has its own set of agents, templates, and settings. Agents in one grove do not see or interact with agents in another grove, even though they all work against the same repository.

### Goals

1. Allow multiple groves to reference the same git remote URL.
2. Preserve convenient linking of local checkouts to existing hub groves.
3. Enforce unique slugs across all groves (with automatic serial numbering for duplicates).
4. Maintain backward compatibility — existing single-grove-per-repo workflows continue to work without changes.

### Non-Goals

- Merging or synchronizing work between groves sharing the same repository.
- Cross-grove agent visibility or communication.
- Changing the hub-managed (non-git) grove model.

---

## 2. Current State Analysis

### 2.1 Where the 1:1 Assumption is Encoded

| Location | Mechanism | Impact |
|----------|-----------|--------|
| `pkg/util/git.go:710` | `HashGroveID()` — deterministic UUID5 from normalized URL | Same URL always = same ID |
| `pkg/store/sqlite/sqlite.go:174` | `git_remote TEXT UNIQUE` constraint | DB rejects duplicate git remotes |
| `pkg/hub/handlers.go:2605-2612` | `createGrove()` derives ID from git remote | New groves auto-collide on ID |
| `pkg/hub/handlers.go:3185-3193` | `handleGroveRegister()` looks up by git remote | Registration returns existing grove |
| `pkg/hubsync/prompt.go:172` | `hasGitRemote` suppresses "Register as new" option | Users cannot create duplicates via link dialog |
| `pkg/config/init.go:109-117` | `GenerateGroveID()` hashes git remote | Local init produces deterministic ID |
| `pkg/ent/schema/grove.go:46` | Ent schema: `git_remote` field marked `Unique()` | ORM enforces uniqueness |

### 2.2 Branch Qualifier (Partial Solution)

The system already supports an `@branch` qualifier in the identity string (e.g., `github.com/acme/widgets@release/v2`), which produces a different grove ID. However, this is tied to a specific branch and doesn't cover the general case of multiple teams working on the same branch or across branches freely.

### 2.3 Local Grove ID vs Hub Grove ID

The system already separates `grove_id` (local, drives config-dir paths) from `hub.groveId` (explicit hub link). This separation is crucial and will be preserved — it means local grove identity can differ from the hub grove being linked to.

---

## 3. Proposed Design

### 3.1 Remove the UNIQUE Constraint on `git_remote`

The `git_remote` column becomes a regular indexed (non-unique) column. Multiple grove records can share the same normalized git remote URL.

**Migration:**

```sql
-- Drop the unique index on git_remote and replace with a regular index.
-- The existing idx_groves_git_remote already exists as a regular index,
-- so we only need to drop the UNIQUE constraint added inline on the column.
-- SQLite requires table recreation for column constraint changes.
```

Since SQLite doesn't support `ALTER TABLE ... DROP CONSTRAINT`, the migration will use the standard recreate-table pattern.

### 3.2 Move Away from Deterministic UUID5 for Grove IDs

**New behavior:** Grove IDs are always randomly generated UUIDs (`uuid.New()`), regardless of whether the grove has a git remote.

**Rationale:** Deterministic IDs were designed to enforce the 1:1 mapping. With multiple groves per URL, deterministic IDs would cause collisions. Random UUIDs are already used for hub-managed groves and work correctly.

**`HashGroveID()` is retained** but repurposed: it is no longer used for grove ID generation. It may still be useful for other deterministic identifiers (e.g., cache keys), so it is not removed. Callers that used it for grove ID generation are updated.

**`GenerateGroveID()` and `GenerateGroveIDForDir()` simplified:** Always return `uuid.New().String()`.

### 3.3 Enforce Unique Slugs with Serial Numbering

Slugs must remain unique because they are used in:
- Filesystem paths (`~/.scion/groves/<slug>/` for hub-managed, `~/.scion/grove-configs/<slug>__<uuid>/` for external)
- API routing (`/api/v1/groves/<slug-or-id>/...`)
- CLI references (`scion start agent --grove <slug>`)

**Slug generation for duplicate git remotes:**

When a grove is created for a git remote that already has one or more groves, the slug is derived with a serial suffix:

1. Base slug derived from repo name: `acme-widgets`
2. If `acme-widgets` is taken: `acme-widgets-1`
3. If `acme-widgets-1` is taken: `acme-widgets-2`
4. And so on.

**Implementation:** A new store method `GetGrovesByGitRemote(ctx, gitRemote) ([]*Grove, error)` returns all groves for a given remote. The hub handler computes the next available slug by checking existing slugs.

**Database enforcement:** Add a proper `UNIQUE` constraint on the `slug` column (currently it's only a non-unique index in SQLite, despite the Ent schema declaring it unique — this is a pre-existing bug that this work fixes).

### 3.4 Update the Linking Dialog

The `ShowMatchingGrovesPrompt()` function is updated to **always show the "Register as new grove" option**, regardless of `hasGitRemote`. The `hasGitRemote` parameter is removed.

When creating a new grove for a URL that already has groves, the dialog shows the proposed serial-numbered slug and asks for confirmation:

```
Found 2 existing grove(s) with the name 'acme-widgets' on the Hub:

  [1] acme-widgets (ID: abc123, remote: github.com/acme/widgets)
  [2] acme-widgets-1 (ID: def456, remote: github.com/acme/widgets)
  [3] Register as a new grove (will be created as 'acme-widgets-2')

Enter choice (or 'c' to cancel):
```

### 3.5 Update the Registration Flow

**`handleGroveRegister()` changes:**

Currently the flow is:
1. Look up by client-provided ID
2. Look up by git remote (returns single grove)
3. Look up by slug (for non-git groves)
4. Create if not found

New flow:
1. Look up by client-provided ID (unchanged)
2. Look up by git remote → returns **list** of matching groves
   - If exactly one match and no explicit ID was provided: return it (backward compatible for existing single-grove setups)
   - If multiple matches: return the list for client-side disambiguation (or use slug to pick one)
3. Look up by slug (for non-git groves, unchanged)
4. Create if not found (with random UUID, serial slug)

**`createGrove()` changes:**

- No longer derives ID from git remote. Always uses client-provided ID or generates a random UUID.
- Computes serial-numbered slug when git remote already has groves.
- Validates slug uniqueness before insert.

### 3.6 Local `scion init` Changes

`GenerateGroveID()` always returns a random UUID. This means re-running `scion init` in the same repository will produce a different grove ID each time (rather than the same deterministic one). This is acceptable because:

- `scion init` is typically run once per project.
- The grove ID is persisted in `.scion/grove-id` (git groves) or the marker file (external groves), so it doesn't need to be re-derivable.
- Hub linking uses name/slug matching (not ID matching) for discovery, so a different local ID doesn't prevent linking.

### 3.7 Preserve Clone vs Shared Workspace Modes

All three workspace strategies (represented as three modes at the model layer) continue to work unchanged:

- **Per-agent clone** (`GitClone` set, `SharedWorkspace=false`): Each agent in a git grove clones the repository independently inside its container. This is the default for git groves.
- **Shared workspace clone** (`GitClone` unset, `SharedWorkspace=true`, `Workspace` set): A single shared git clone is mounted by all agents in the grove, rather than each agent cloning independently.
- **Hub-managed workspace** (`GitClone` unset, `SharedWorkspace=false`, `Workspace` set): Non-git groves with a hub-managed filesystem.

The workspace strategy is determined by grove configuration (specifically the `scion.dev/workspace-mode` label), not by the number of groves sharing a URL. Multiple groves sharing the same git remote may each independently choose their own workspace strategy.

---

## 4. API Changes

### 4.1 `POST /api/v1/groves` (createGrove)

**Request:** No structural changes. The `id` field becomes optional (random UUID if omitted). The `gitRemote` field is no longer required to be unique.

**Response:** Unchanged. Returns the created grove.

**Behavior change:** No longer returns an existing grove when `gitRemote` matches. Each call creates a new grove. Idempotency is achieved by client-provided `id` only.

### 4.2 `POST /api/v1/groves/register` (handleGroveRegister)

**Request:** Unchanged.

**Response:** Extended with a `matches` field when multiple groves share the same git remote:

```json
{
  "grove": { ... },
  "created": false,
  "matches": [
    { "id": "abc123", "name": "acme-widgets", "slug": "acme-widgets" },
    { "id": "def456", "name": "acme-widgets", "slug": "acme-widgets-1" }
  ]
}
```

When `matches` has more than one entry, the client should prompt the user to choose. When it has exactly one entry, the existing behavior (auto-link) is preserved for backward compatibility.

### 4.3 `GET /api/v1/groves` (listGroves)

**Filter addition:** `gitRemote` filter already exists but currently returns at most one result. After this change, it may return multiple results. No API change needed.

### 4.4 Store Interface Changes

**New method:**
```go
// GetGrovesByGitRemote returns all groves matching the normalized git remote.
// Returns empty slice (not error) if none found.
GetGrovesByGitRemote(ctx context.Context, gitRemote string) ([]*Grove, error)
```

**New method (for GitHub App token sourcing):**
```go
// GetInstallationForRepository returns an active GitHub App installation
// that covers the given repository (owner/repo format).
// Returns ErrNotFound if no matching installation exists.
GetInstallationForRepository(ctx context.Context, repoFullName string) (*GitHubInstallation, error)
```

**Deprecated method:**
`GetGroveByGitRemote` (singular) is **removed from the interface**. All callers are migrated:
- `handleGroveRegister()` → uses `GetGrovesByGitRemote()` (plural)
- `handleGroveSyncTemplates()` → uses `GetInstallationForRepository()`

---

## 5. Alternatives Considered

### 5.1 Extend the Branch Qualifier Model

**Approach:** Require users to specify a branch (or arbitrary qualifier) when creating duplicate groves, extending the existing `@branch` mechanism.

**Pros:**
- Minimal schema changes — the identity string is different, so UUID5 and UNIQUE constraints still work.
- Natural semantic meaning when groves actually target different branches.

**Cons:**
- Artificial when teams work across branches freely (the qualifier becomes meaningless, e.g., `@team-a`).
- Couples grove identity to branch selection, which may not reflect actual usage.
- The existing `@branch` mechanism stores the qualifier in the normalized remote and labels, creating a tighter coupling than desired.

**Decision:** Rejected as the primary mechanism. The `@branch` qualifier remains available for branch-specific groves but is not the solution for the general multi-team case.

### 5.2 Grove "Groups" or "Namespaces"

**Approach:** Introduce a namespace/group layer above groves, where a git remote maps to a group and individual groves exist within it.

**Pros:**
- Clean hierarchical model.
- Could support cross-grove features (shared templates, agent migration) in the future.

**Cons:**
- Significant schema and API changes (new entity, new relationships).
- Over-engineered for the immediate need.
- Adds conceptual complexity for users.

**Decision:** Rejected for now. May be revisited if cross-grove coordination becomes a requirement.

### 5.3 Keep Deterministic IDs, Use Composite Key

**Approach:** Keep UUID5 but include a sequence number in the hash input: `HashGroveID(normalized + "#2")`.

**Pros:**
- Deterministic IDs are preserved (useful for idempotent creation in some flows).

**Cons:**
- The sequence number itself needs coordination (what if two clients try to create grove #2 simultaneously?).
- Deterministic creation was valuable precisely because it avoided coordination — adding a sequence number undermines the benefit.
- Random UUIDs are simpler and already proven (hub-managed groves use them).

**Decision:** Rejected. Random UUIDs are simpler and sufficient.

### 5.4 Allow Duplicate Slugs (Disambiguate by ID)

**Approach:** Allow multiple groves to share the same slug. Use ID for disambiguation in API calls.

**Pros:**
- Simpler slug generation (no serial numbering).

**Cons:**
- Breaks CLI usability (`scion start agent --grove acme-widgets` becomes ambiguous).
- Breaks filesystem path derivation (slug is used in directory names).
- Breaks API routing (slug is used as grove identifier in URLs).

**Decision:** Rejected. Slug uniqueness is a hard requirement.

---

## 6. Design Decisions (Resolved)

### Q1: Should `GetGroveByGitRemote` return the "primary" grove?

**Resolved:** No. `GetGroveByGitRemote` (singular) will be **deprecated and removed**. Returning one arbitrary grove when many match introduces unpredictable behavior. All callers will be migrated:

| Caller | Current Usage | Migration Path |
|--------|---------------|----------------|
| `handleGroveRegister()` | Falls back to git remote lookup after ID lookup fails | Use `GetGrovesByGitRemote()` (plural) to get all matches, return the list in the response for client-side disambiguation |
| `handleGroveSyncTemplates()` | Looks up a grove by git remote to find a GitHub App installation for token sourcing | Migrate to look up the `github_installations` table directly by repository name (see Q4), bypassing grove lookup entirely |
| `useraccesstoken_test.go` | Mock returning `ErrNotFound` | Update mock to implement `GetGrovesByGitRemote()` |

The new `GetGrovesByGitRemote()` (plural) is the only git-remote-based lookup. Code paths that need a single grove should use ID-based or slug-based lookups instead.

### Q2: Should the serial suffix be on the slug only, or also the display name?

**Resolved:** Use the "name with qualifier" pattern. When a grove is created as a duplicate:
- **Slug:** `acme-widgets-2` (serial-numbered, URL-safe)
- **Display name:** `acme-widgets (2)` (parenthesized qualifier, human-friendly)

Users who want more control can provide a custom display name at creation time, overriding the default. The slug remains auto-generated and enforced unique.
### Q3: How should `EnsureHubReady` behave with multiple matches?

**Resolved:** Move to **get-by-ID as the universal pattern**. The lookup chain becomes:

1. If the local `hub.groveId` setting is set: look up by ID (definitive, unchanged).
2. If the local `grove_id` matches a hub grove ID exactly: use that grove (unchanged).
3. If neither matches: trigger the disambiguation prompt (showing all groves matching by git remote or name), including the "Register as new" option.

The key change is that ID-based lookup is always preferred. Git-remote-based auto-linking (which assumed a single match) is removed from the auto-sync path. Once a user selects or creates a grove through the disambiguation prompt, the `hub.groveId` is persisted locally, and subsequent syncs use the fast ID path.

### Q4: Impact on GitHub App integration?

**Resolved:** GitHub App installations are **per-repository, shared across groves**.

GitHub App installations are fundamentally tied to a repository (or organization), not to a project-level concept like a grove. The existing `github_installations` table already stores installations independently with a `repositories` list. The `grove.github_installation_id` foreign key is the per-grove link.

**Current behavior that works well with multi-grove:**
- `autoAssociateGitHubInstallation()` already matches installations to groves by comparing the grove's `git_remote` against the installation's `repositories` list.
- `matchGrovesToInstallation()` (webhook handler) iterates all groves and matches by repository — this naturally handles multiple groves for the same repo.

**Changes needed:**
- `autoAssociateGitHubInstallation()` must be called for each newly created grove, even when other groves for the same remote already exist. This already works — no change needed.
- `handleGroveSyncTemplates()` currently looks up a grove by git remote to find a GitHub App installation for token sourcing. This should be migrated to query the `github_installations` table directly by repository name, removing the indirect grove-based lookup. (See new Q6.)
- Per-grove fields (`github_permissions`, `github_app_status`) remain per-grove, since each grove may request different permission scopes or have independent health status.

**No schema changes required** — the existing one-to-many relationship (one installation, many groves referencing it via `github_installation_id`) is already the correct model.

### Q5: Should we add a `UNIQUE` constraint on `slug` in this migration?

**Resolved:** Yes. This migration adds the proper `UNIQUE` constraint on `slug`, fixing the pre-existing discrepancy between the Ent schema (which declares `Unique()`) and the SQLite implementation (which only has a non-unique index). This is tech debt that should be fixed alongside the `git_remote` constraint change since both require the same table-recreation migration pattern.

### Q6: How should `handleGroveSyncTemplates` find GitHub tokens without `GetGroveByGitRemote`?

**Resolved:** Query `github_installations` directly by repository name. Add a store method `GetInstallationForRepository(ctx, repoFullName)` that searches the `repositories` JSON array in `github_installations`. This removes the indirection through groves entirely.

The intent of the lookup is "find a GitHub token that can access this repo," not "find a grove." The `github_installations.repositories` column already contains the needed data. This also simplifies the code and removes a non-obvious coupling between template syncing and grove lookup.

### Q7: Should the `@branch` qualifier interact with multi-grove?

**Resolved:** Keep them independent. The `@branch` qualifier serves a distinct semantic purpose (branch-locked groves) and is orthogonal to team-based isolation. The fully qualified git remote (including any `@branch` suffix) is unrelated to grove identity and uniqueness — it simply produces a different normalized remote URL. A grove with `@release/v2` has a different normalized remote and thus doesn't conflict with unqualified groves. Users can create multiple groves for `github.com/acme/widgets@release/v2` if needed, using the same serial slug mechanism.

---

## 7. Migration Considerations

### 7.1 Existing Data

All existing groves retain their current IDs, slugs, and git remote values. No data migration is needed — only the constraint is relaxed.

### 7.2 Backward Compatibility

- **Existing single-grove-per-repo setups:** Continue to work identically. The registration flow returns the existing grove when exactly one matches.
- **Existing local configurations:** `grove_id` and `hub.groveId` settings are preserved. No local config changes needed.
- **Hub API clients:** The `register` endpoint returns a new `matches` field but the existing `grove` field is still populated. Old clients that ignore `matches` continue to work (they get the first/only match).

### 7.3 Rollback

If this change needs to be reverted, groves that were created as duplicates (sharing a git remote) would need to be manually deleted or have their git remotes cleared before re-adding the UNIQUE constraint.

---

## 8. Implementation Phases

### Phase 1: Schema and Store Layer ✅ COMPLETED

**Goal:** Remove the 1:1 constraint, add slug uniqueness enforcement, and migrate store interface.

1. ✅ **Database migration:** Drop UNIQUE constraint on `git_remote`, add UNIQUE constraint on `slug` (migration V40).
2. ✅ **New store method:** `GetGrovesByGitRemote()` returning `[]*Grove`.
3. ✅ **New store method:** `GetInstallationForRepository()` for direct GitHub installation lookup by repository name.
4. ✅ **Remove `GetGroveByGitRemote()`:** Deleted from interface and implementation. All callers migrated (see Q1).
5. ✅ **Update Ent schema:** Removed `Unique()` from `git_remote` field.
6. ✅ **Slug validation helper:** `NextAvailableSlug(ctx, baseSlug) string` that queries existing slugs and returns the next serial-numbered variant.
7. ✅ **Name generation:** `DisplayNameWithSerial()` helper uses parenthesized qualifier (e.g., `acme-widgets (2)`) when serial suffix is applied.
8. ✅ **Tests:** Store-level tests for multi-grove-per-remote scenarios, slug uniqueness enforcement, serial numbering, and installation-by-repo lookup.

### Phase 2: Grove ID Generation ✅ COMPLETED

**Goal:** Stop generating deterministic IDs from git remotes.

1. ✅ **Simplify `GenerateGroveID()` and `GenerateGroveIDForDir()`:** Always return `uuid.New().String()`.
2. ✅ **Update `createGrove()` handler:** Remove deterministic ID derivation from git remote. Use client-provided ID or random UUID. Uses `NextAvailableSlug()` and `DisplayNameWithSerial()` for serial slug/name assignment.
3. ✅ **Update `handleGroveRegister()` handler:** Same — no deterministic ID fallback. Uses `NextAvailableSlug()` and `DisplayNameWithSerial()` for serial slug/name assignment.
4. ✅ **Update `scion hub grove create` CLI command:** Remove `HashGroveID()` call for ID generation. Server assigns ID.
5. ✅ **Retain `HashGroveID()` function:** Marked as not used for grove IDs but kept for other potential uses.
6. ✅ **Tests:** Verify that creating two groves for the same URL produces different IDs and serial slugs.

### Phase 3: Registration and Linking Flow ✅ COMPLETED

**Goal:** Support creating new groves for URLs that already have groves, with ID-based lookup as the universal pattern.

1. ✅ **Update `handleGroveRegister()`:** Uses `GetGrovesByGitRemote()` (plural). When git remote matches multiple groves, returns the match list in the response. When exactly one match, preserves current auto-link behavior.
2. ✅ **Update `RegisterGroveResponse`:** Added `Matches []GroveMatch` field (both server-side in `pkg/hub/handlers.go` and client-side in `pkg/hubclient/groves.go`).
3. ✅ **Update `ShowMatchingGrovesPrompt()`:** Removed `hasGitRemote` parameter. Always shows "Register as new grove" option. Shows proposed serial-numbered slug via `nextSlug` parameter.
4. ✅ **Update `runHubLink()`:** Uses `NextSlugFromMatches()` to compute proposed slug, passes it to the disambiguation prompt. Populates `Slug` field on `GroveMatch`.
5. ✅ **Update `EnsureHubReady()`:** Prioritizes `hub.groveId` and `grove_id` for ID-based lookup. Removed `hasGitRemote` calculation. Uses `NextSlugFromMatches()` for the disambiguation prompt.
6. ✅ **Update `handleGroveSyncTemplates()`:** Already uses `GetInstallationForRepository()` for GitHub token sourcing (completed in Phase 1).
7. ✅ **Serial slug display:** `NextSlugFromMatches()` helper computes the next available slug client-side; shown in the "Register as new" option.
8. ✅ **Tests:** Multi-match registration test (`TestGroveRegisterMultipleGitRemoteMatches`), `NextSlugFromMatches` unit tests, `ShowMatchingGrovesPrompt` auto-confirm test.

### Phase 4: CLI and Hub-First Creation ✅ COMPLETED

**Goal:** Allow hub-first creation of duplicate groves.

1. ✅ **Update `scion hub grove create`:** When the URL already has a grove, shows existing groves and the proposed serial-numbered slug. Prompts for confirmation (respects `--yes` and `--non-interactive` flags). Updated command Long description to document multi-grove support.
2. ✅ **Add `--slug` override validation:** When `--slug` is provided explicitly, validates uniqueness against the Hub API before creation. Returns a clear error if the slug is already in use.
3. ✅ **Update web UI grove creation:** Git remote URL input triggers a debounced check for existing groves sharing the same remote. An info banner lists existing groves when found. Updated the "Grove Already Exists" dialog text (now only for ID-based idempotency).
4. ✅ **Tests:** `TestCreateGrove_DuplicateGitRemote_SerialSlug` (serial slug assignment for duplicate git remotes), `TestCreateGrove_ExplicitSlug_Unique` (server assigns serial suffix when explicit slug is taken), `TestCreateGrove_ListByGitRemote_ReturnsMultiple` (list API returns all groves sharing a git remote).

### Phase 5: Cleanup and Documentation ✅ COMPLETED

1. ✅ **Audit all callers of `HashGroveID()`:** Verified no callers use it for grove ID generation. Only test references: `TestRegisterGrove_GitBacked_RandomID` (asserts IDs are NOT deterministic), `TestNormalizeGitRemote_CrossProtocolConsistency` and `TestHashGroveID` (test the utility function itself). Function docstring already documents it is not used for grove IDs.
2. ✅ **Verify `GetGroveByGitRemote()` is fully removed:** Confirmed no remaining references in Go source code, tests, or mocks. Only references are in design documentation describing the migration history.
3. ✅ **Update design docs:** Marked `git-groves.md` section 2.2 as superseded by this design, with strikethrough on outdated claims and a note pointing to this document.
4. ✅ **Update test fixtures:** All tests already updated in prior phases. Clarified comments in `TestNormalizeGitRemote_CrossProtocolConsistency` to note `HashGroveID` is no longer used for grove IDs.

---

## 9. Affected Files

| File | Change |
|------|--------|
| `pkg/store/sqlite/sqlite.go` | Migration: drop UNIQUE on git_remote, add UNIQUE on slug. New `GetGrovesByGitRemote()`. Remove `GetGroveByGitRemote()`. |
| `pkg/store/sqlite/github_installation.go` | New `GetInstallationForRepository()` implementation. |
| `pkg/store/store.go` | Add `GetGrovesByGitRemote()` and `GetInstallationForRepository()` to interface. Remove `GetGroveByGitRemote()`. |
| `pkg/store/models.go` | No structural changes. |
| `pkg/ent/schema/grove.go` | Remove `.Unique()` from `git_remote` field. |
| `pkg/ent/migrate/schema.go` | Update generated migration schema. |
| `pkg/config/init.go` | Simplify `GenerateGroveID()` / `GenerateGroveIDForDir()`. |
| `pkg/hub/handlers.go` | Update `createGrove()`, `handleGroveRegister()`, `handleGroveSyncTemplates()`. |
| `pkg/hubsync/prompt.go` | Update `ShowMatchingGrovesPrompt()` — remove `hasGitRemote` param, show serial slug and display name with qualifier. |
| `pkg/hubsync/sync.go` | Update `EnsureHubReady()` — ID-based lookup as universal pattern, remove git-remote auto-linking. |
| `cmd/hub.go` | Update `runHubLink()`, `scion hub grove create`. |
| `pkg/util/git.go` | No changes to `HashGroveID()` itself, but callers change. |
| `pkg/hub/handlers_grove_test.go` | Update tests for new behavior. |
| `pkg/hub/useraccesstoken_test.go` | Update mock to remove `GetGroveByGitRemote()`, add `GetGrovesByGitRemote()`. |
| `pkg/store/sqlite/sqlite_test.go` | Update tests: remove single-match test, add multi-match and installation-by-repo tests. |
| `pkg/util/git_test.go` | Update/add tests. |
