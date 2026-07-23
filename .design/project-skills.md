# Injected Skills: Design Doc

**Feature:** Project-scoped (and hub/user-scoped) automatic skill injection into agents  
**Issue:** [ptone/scion #542](https://github.com/ptone/scion/issues/542)  
**Author:** ps-arch  
**Date:** 2026-07-22  
**Status:** Final (all design questions resolved with user)

---

## Summary

"Injected skills" is a new multi-scope mechanism that automatically injects skills into every agent at provision time, based on configurable lists associated with the hub, the requesting user, and the project. The agent's final skill set is the union of: hub-injected + user-injected + project-injected + template-declared skills. The existing template-level `finalScionCfg.Skills` pipeline (Step 3b in `ProvisionAgent`) handles installation unchanged — this design only adds new _sources_ that expand that list before provisioning runs.

---

## Problem & Goals

**Problem:** Teams want a set of skills to be available in every agent in a project (e.g., team conventions, shared tooling, scion messaging) without requiring every template to declare them. The current model requires each template's `scion-agent.yaml` to explicitly list skills, which creates duplication and makes project-wide changes fragile.

**Goals:**

1. Allow project admins to associate a list of skill URIs with a project; those skills are injected into every agent provisioned in that project.
2. Allow users to associate a list of skill URIs with their account; those skills are injected into every agent they own.
3. Allow hub admins to define a hub-wide list (including system/built-in skills) injected into all agents.
4. Skills from all scopes are unioned; version conflicts are resolved by specificity (template > project > user > hub).
5. Use the identical resolution, download, and installation machinery as template-declared skills — no new provisioner logic.

---

## Non-Goals

- **Solo/local mode support.** Injected skills lists require hub connectivity. In solo mode, the injected skills lists are absent; template skills work as today. A local equivalent (user settings YAML) is not part of this design.
- **Harness-specific skill filters.** Skills from injected skills lists are installed into all harnesses unconditionally, using the existing `h.SkillsDir()` routing. Per-harness filtering is not in scope.
- **Removing the embedded `PlatformSkillsFS()` injection (Step 3a2).** Phase 6 migrates platform skills to the hub-scope system list, but Step 3a2 is preserved as a fallback for solo mode and non-hub-enabled brokers.
- **Skill bank ownership/visibility.** The existing `scope="project"` / `scope_id=<projectID>` field on `Skill` records (which controls which skills are visible in a project's skill bank) is a separate concept and is not changed by this design.

---

## Design Decisions

### 1. Concept and naming

**Decision:** This is a new feature called "injected skills." It is distinct from skill bank ownership/visibility (which already exists as `scope=project` on `Skill` records). A project-owned skill in the bank does NOT auto-inject; only URIs in the injected skills list do.

**Rationale:** Conflating the two concepts (owning a skill vs. injecting it into every agent) would force every project-scoped skill to auto-inject, which is wrong for skills a project owns but doesn't want universally applied.

### 2. Scope hierarchy

**Decision:** Three injected skills scopes, applied in precedence order (most specific first):

```
template > project > user > hub
```

Every agent gets the union of all four. On version conflict (same base URI, different version pin), the most-specific scope wins and a `slog.Warn` is emitted.

**Alternative considered:** Two-scope only (project + template). Rejected because the user explicitly requested hub and user scope to enable personal and global defaults without requiring a project.

### 3. Data model — project and user scope

**Decision:** A new `skill_injections` ent entity with a `scope` + `scope_id` pattern (matching how `Skill` itself is scoped). One row per skill URI per scope+entity.

```
skill_injections table:
  id          UUID PK
  scope       string  ("project" | "user")
  scope_id    string  (project UUID or user UUID)
  skill_uri   string  (full URI, may include version pin)
  skill_as    string  (optional alias, maps to SkillReference.As)
  optional    bool    (default false; maps to SkillReference.Optional)
  sort_order  int     (user-defined display order; default 0)
  created_at  time.Time
  created_by  string
```

**Indexes:** `(scope, scope_id)` — primary query pattern.

**Alternatives considered:**
- *JSON array on Project entity (like SharedDirs):* Simpler migration, but doesn't naturally extend to user scope, offers no referential integrity, and makes per-entry operations (add/remove by ID) awkward. Rejected in favor of a proper relational table.
- *Separate tables for project and user:* Functionally equivalent but more schema boilerplate. The single-table scope+scope_id pattern is already established in the codebase (see `Skill`).

### 4. Data model — hub scope

**Decision:** Use the existing `hub_settings` table (section `"injected_skills"`). The value is a JSON object with two arrays:

```json
{
  "system": [ ...SkillReference... ],       // immutable, seeded from binary on startup
  "user_defined": [ ...SkillReference... ]  // admin-configurable
}
```

System entries cannot be modified via API; the hub seeds them from `PlatformSkillsFS()` metadata on startup/restart. User-defined entries are managed by hub admins via API.

**Rationale:** Hub-scope settings are already a key-value JSON store with CAS semantics and origin tracking (`seeded` vs `managed`). This is the correct home for global hub settings — not a new DB table. The `UpsertHubSetting` API with `origin="seeded"` and `expectedRevision=-1` already supports the idempotent-seed pattern needed for binary-rebuild updates.

**Alternative considered:** A new `hub_skill_injections` table. Rejected because the hub_settings pattern is already established for hub-global configuration, supports the system/user-defined distinction, and avoids schema proliferation.

### 5. Versioning/pinning

**Decision:** Same as the existing `SkillReference` struct — the URI may include a version pin (e.g., `scion://my-skill@1.2`) or use a floating latest reference. No new version semantics are introduced.

**Rationale:** The existing skill resolution machinery already handles both pinned and floating references. Introducing new version-pinning semantics at the injection layer would duplicate logic.

### 6. Conflict resolution

**Decision:** Deduplicate by _base URI_ (version-agnostic). When the same base URI appears at multiple scopes:
1. The most-specific scope's entry wins (template > project > user > hub).
2. If the winning entry has a different version than the losers, emit `slog.Warn` with the URI, the winning version, and the losing versions.

**What "base URI" means:** Strip the version specifier from the URI for comparison purposes. `scion://my-skill@1.0` and `scion://my-skill@2.0` share base URI `scion://my-skill`.

### 7. Failure handling

**Decision:** No new behavior. The existing `Optional` field on `SkillReference` controls failure semantics:
- `Optional: false` (default): resolution failure fails agent provisioning.
- `Optional: true`: resolution failure emits a warning and the agent starts without that skill.

Insertion-list refs carry the `optional` flag from the `skill_injections` row.

### 8. Provisioner integration point

**Decision:** Hub-side, in `populateAgentConfig` (`pkg/hub/handlers_agent_create_helpers.go`). After template merging, the hub fetches injected-skills refs for all three scopes, runs dedup/conflict resolution, and appends the merged list to `InlineConfig.Skills`. The broker receives the unified list and Step 3b installs everything through the existing pipeline.

**Alternative considered (C2):** New field in broker dispatch request (`ProjectSkills []api.SkillReference`). Rejected: adds broker protocol surface area and adds complexity without benefit, since the existing `InlineConfig.Skills` field is already the correct mechanism.

**Alternative considered (C3):** Broker fetches from hub at provision time. Rejected: extra round-trip latency, harder to test, and violates the "hub computes, broker executes" separation.

### 9. Solo mode

**Decision:** Hub-only feature. Insertion lists are absent in solo mode. Template skills work exactly as today.

### 10. CLI and UI

**Decision:** Both CLI and web UI, sharing a common hub REST API layer. The CLI calls hub API endpoints identically to what the web UI does.

### 11. Platform skills migration

**Decision:** Phase 6 of implementation. Hub startup seeds `hub_settings["injected_skills"].system` from the embedded `PlatformSkillsFS()` metadata (marking entries immutable). The current Step 3a2 embedded injection is preserved as a backward-compat fallback for solo mode and non-hub-enabled brokers. Step 3a2 removal is deferred to a follow-on issue once all brokers are hub-connected.

---

## Architecture

### Data Model Changes

#### New ent entity: `SkillInjection`

**File:** `pkg/ent/schema/skill_injection.go`

```go
// SkillInjection represents one entry in a injected-skills list for a
// project, user, or (future) other scope. Hub scope is handled via hub_settings.
type SkillInjection struct{ ent.Schema }

func (SkillInjection) Fields() []ent.Field {
    return []ent.Field{
        field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
        field.String("scope"),        // "project" | "user"
        field.String("scope_id"),     // project UUID or user UUID
        field.String("skill_uri"),    // full skill URI (may include version pin)
        field.String("skill_as").Optional(),   // alias
        field.Bool("optional").Default(false),
        field.Int("sort_order").Default(0),
        field.Time("created_at").Default(time.Now).Immutable(),
        field.String("created_by").Optional(),
    }
}

func (SkillInjection) Indexes() []ent.Index {
    return []ent.Index{
        index.On("scope", "scope_id"),
    }
}
```

Run `go generate ./pkg/ent/...` to produce the ent codegen. The migration adds a single new table `skill_injections`.

#### New store model: `store.SkillInjection`

**File:** `pkg/store/models.go`

```go
type SkillInjection struct {
    ID        string    `json:"id"`
    Scope     string    `json:"scope"`
    ScopeID   string    `json:"scopeId"`
    SkillURI  string    `json:"skillUri"`
    SkillAs   string    `json:"skillAs,omitempty"`
    Optional  bool      `json:"optional"`
    SortOrder int       `json:"sortOrder"`
    CreatedAt time.Time `json:"createdAt"`
    CreatedBy string    `json:"createdBy,omitempty"`
}

// ToSkillReference converts to the api.SkillReference wire type used by the provisioner.
func (si SkillInjection) ToSkillReference() api.SkillReference {
    return api.SkillReference{URI: si.SkillURI, As: si.SkillAs, Optional: si.Optional}
}
```

#### New store interface: `SkillInjectionStore`

Added to `pkg/store/store.go` and embedded in the top-level `Store` interface:

```go
type SkillInjectionStore interface {
    ListSkillInjections(ctx context.Context, scope, scopeID string) ([]SkillInjection, error)
    AddSkillInjection(ctx context.Context, si *SkillInjection) error
    UpdateSkillInjection(ctx context.Context, si *SkillInjection) error
    RemoveSkillInjection(ctx context.Context, id string) error
    // SetSkillInjections replaces the full list for a scope+scopeID atomically.
    SetSkillInjections(ctx context.Context, scope, scopeID string, refs []api.SkillReference, createdBy string) error
}
```

Implemented in `pkg/store/entadapter/skill_injection_store.go`. Include a round-trip test analogous to `project_store_test.go:TestProject_SharedDirsRoundTrip`.

#### Hub settings schema for hub-scope injected skills

```go
// HubSkillInjectionSetting is the value stored in hub_settings["injected_skills"].
type HubSkillInjectionSetting struct {
    System      []api.SkillReference `json:"system"`       // seeded from binary, immutable
    UserDefined []api.SkillReference `json:"user_defined"` // admin-managed
}
```

System entries are upserted on hub startup with `origin="seeded"`, `expectedRevision=-1`.

---

### Hub-Side Changes

#### New API endpoints

Registered in `pkg/hub/server.go` alongside existing project sub-routes:

```
GET    /api/v1/projects/{projectId}/injected-skills       → list project injected-skills list
POST   /api/v1/projects/{projectId}/injected-skills       → add one entry
PUT    /api/v1/projects/{projectId}/injected-skills       → bulk-replace the full list
DELETE /api/v1/projects/{projectId}/injected-skills/{id}  → remove one entry

GET    /api/v1/users/me/injected-skills                   → list user injected-skills list
POST   /api/v1/users/me/injected-skills                   → add one entry
PUT    /api/v1/users/me/injected-skills                   → bulk-replace the full list
DELETE /api/v1/users/me/injected-skills/{id}              → remove one entry

GET    /api/v1/hub/settings/injected-skills               → list hub-scope injected skills (admin+)
PUT    /api/v1/hub/settings/injected-skills               → set hub user_defined entries (admin)
```

**Authorization:**
- Project injected-skills endpoints: same authz as other project-settings mutations (project owner/admin).
- User injected-skills endpoints: authenticated user, own record only.
- Hub injected-skills GET: any authenticated user (needed for the merge fetch).
- Hub injected-skills PUT: hub admin only.

**Shared request/response types** (`pkg/api/types.go`):

```go
type SkillInjectionEntry struct {
    ID        string `json:"id,omitempty"`      // set on read, not required on write
    SkillURI  string `json:"skillUri"`
    SkillAs   string `json:"skillAs,omitempty"`
    Optional  bool   `json:"optional,omitempty"`
    SortOrder int    `json:"sortOrder,omitempty"`
    // Enriched fields (populated when URI maps to a known bank skill):
    SkillName string `json:"skillName,omitempty"`
    SkillSlug string `json:"skillSlug,omitempty"`
}

type SkillInjectionList struct {
    Entries []SkillInjectionEntry `json:"entries"`
}
```

#### `populateAgentConfig` changes

Add a new call at the end of `populateAgentConfig` in `pkg/hub/handlers_agent_create_helpers.go`:

```go
// Merge injected skills lists from hub, user, and project scopes into
// InlineConfig.Skills so the provisioner's existing Step 3b handles them.
s.mergeInjectedSkills(ctx, agent, project)
```

New private method `mergeInjectedSkills`:

```go
func (s *Server) mergeInjectedSkills(ctx context.Context, agent *store.Agent, project *store.Project) {
    // Ensure InlineConfig exists (populateAgentConfig already does this for telemetry).
    if agent.AppliedConfig.InlineConfig == nil {
        agent.AppliedConfig.InlineConfig = &api.ScionConfig{}
    }

    // Fetch hub-scope injected skills (system + user_defined).
    var hubRefs []api.SkillReference
    if hs, err := s.store.GetHubSetting(ctx, "injected_skills"); err == nil {
        var setting HubSkillInjectionSetting
        if json.Unmarshal(hs.Value, &setting) == nil {
            hubRefs = append(setting.System, setting.UserDefined...)
        }
    }

    // Fetch user-scope injected skills.
    var userRefs []api.SkillReference
    if agent.OwnerID != "" {
        if sis, err := s.store.ListSkillInjections(ctx, "user", agent.OwnerID); err == nil {
            for _, si := range sis {
                userRefs = append(userRefs, si.ToSkillReference())
            }
        }
    }

    // Fetch project-scope injected skills.
    var projectRefs []api.SkillReference
    if project != nil {
        if sis, err := s.store.ListSkillInjections(ctx, "project", project.ID); err == nil {
            for _, si := range sis {
                projectRefs = append(projectRefs, si.ToSkillReference())
            }
        }
    }

    // Template refs are already in InlineConfig.Skills (highest precedence).
    templateRefs := agent.AppliedConfig.InlineConfig.Skills

    // Merge: hub → user → project → template (lowest to highest precedence).
    merged := mergeSkillRefs(hubRefs, userRefs, projectRefs, templateRefs)
    agent.AppliedConfig.InlineConfig.Skills = merged
}

// mergeSkillRefs deduplicates by base URI; later (higher-precedence) lists win.
// Logs a warning when a version conflict is resolved.
func mergeSkillRefs(scopes ...[]api.SkillReference) []api.SkillReference {
    type entry struct {
        ref   api.SkillReference
        scope int // index in scopes (higher = more specific)
    }
    seen := map[string]entry{}
    for i, refs := range scopes {
        for _, ref := range refs {
            base := baseSkillURI(ref.URI)
            if existing, ok := seen[base]; ok && existing.ref.URI != ref.URI {
                slog.Warn("skill injection version conflict resolved",
                    "base_uri", base, "winner", ref.URI, "loser", existing.ref.URI)
            }
            seen[base] = entry{ref: ref, scope: i}
        }
    }
    result := make([]api.SkillReference, 0, len(seen))
    for _, e := range seen {
        result = append(result, e.ref)
    }
    return result
}

// baseSkillURI strips the version specifier from a skill URI for dedup comparison.
// "scion://my-skill@1.0" → "scion://my-skill"; "scion://my-skill" → "scion://my-skill".
func baseSkillURI(uri string) string {
    if i := strings.LastIndex(uri, "@"); i > strings.Index(uri, "://") {
        return uri[:i]
    }
    return uri
}
```

**Error policy:** Hub/user/project injected-skills fetches use best-effort (errors logged, not fatal). A failed fetch means that scope's injected skills are absent for this provisioning; provisioning continues. Required skills that are then missing will fail in Step 3b as normal.

---

### Broker/Provisioner Changes

**None.** The merged skill list lands in `InlineConfig.Skills` before dispatch. Step 3b in `ProvisionAgent` already calls:

```go
result, err := resolver.Resolve(ctx, finalScionCfg.Skills, resolveOpts)
```

`finalScionCfg.Skills` is assembled by merging the template chain's `scion-agent.yaml` files, including the `InlineConfig.Skills` field. With the hub-side merge, project/user/hub injected-skills refs flow in through `InlineConfig.Skills` and are processed identically to template-declared skills — including the harness-specific `h.SkillsDir()` routing, hash verification, and `resolved-skills.json` record.

No changes to `pkg/agent/provision.go`, `pkg/runtimebroker/handlers.go`, or any broker protocol types.

---

### Web UI Changes

**File:** `web/src/components/pages/project-settings.ts`

Add a "Skills" tab to the Resources tab group (alongside Templates, Harness Configs, Shared Dirs):

- **List view:** Table of injected skill entries. For URIs that resolve to known skill bank entries, display the skill name, slug, scope badge, and version. For unknown URIs, display the raw URI.
- **Add:** Skill picker (search/filter the skill bank via the existing `/api/v1/skills` list endpoint filtered to global + project-scoped skills) plus a free-form URI field for external/GCS/GitHub URIs. Optional flag checkbox. Alias field.
- **Remove:** Per-row remove button → `DELETE /api/v1/projects/{projectId}/injected-skills/{id}`.
- **Reorder:** Sort handle for user-controlled ordering (PUT bulk-set).

**File:** User Settings page (new Skills tab):
Same UX, scoped to the authenticated user's injected skills list.

**File:** Hub Settings page (admin section — new Skills section):
- System entries: read-only list with a "seeded by system" badge.
- User-defined entries: same add/remove UX as project/user tabs.

---

### CLI Changes

New `scion project skills` subcommand group and `scion user skills` subcommand group. Both call the hub API defined above.

```
scion project skills list   [project]              # list injected skill entries
scion project skills add    [project] <uri>         # add URI (--as alias, --optional)
scion project skills remove [project] <id|uri>      # remove by ID or URI match

scion user skills list                             # list own injected skill entries
scion user skills add    <uri>                     # add URI (--as, --optional)
scion user skills remove <id|uri>                  # remove by ID or URI match
```

---

### Phase 6: Platform Skills Migration (deferred)

On hub startup, `seedPlatformSkillInsertions()` reads `resources.PlatformSkillsFS()` metadata and upserts entries into `hub_settings["injected_skills"].system` with `origin="seeded"`. These entries:

- Are immutable: the PUT endpoint for hub settings rejects modifications to `system` entries.
- Are refreshed on every hub restart: `UpsertHubSetting(..., expectedRevision=-1, origin="seeded")` is idempotent.
- Are visible in the Hub Settings → Skills UI as read-only "System" items.

**Backward compatibility during migration:**
- Step 3a2 embedded injection is preserved. It installs platform skills from the embedded FS directly into `agentHome/<skillsDir>/`.
- In Phase 6, the hub-scope system entries also include these URIs. Step 3b will attempt to resolve them from the skill bank. If a given platform skill isn't in the skill bank yet, it will fail resolution (if `optional=false`) or be skipped (if `optional=true`).
- **Recommended approach for Phase 6:** Mark all seeded system entries as `optional=true` during migration. Step 3a2 handles required delivery; Step 3b provides the bank-resolved version if available; no provisioning failures.
- Step 3a2 removal is tracked as a follow-on issue: "Remove embedded platform skill injection once all platform skills are in the skill bank."

---

## Alternatives Considered

### A. Project-only scope (not hub/user)

The issue title suggests project-only scope. Rejected: the user explicitly requested hub and user scope in Batch 1 answers. Hub scope is needed for built-in skills; user scope enables personal defaults.

### B. JSON array on Project entity (like SharedDirs)

A single nullable `skill_injections` string column on the `projects` table, JSON-encoded as `[]api.SkillReference`. This is the pattern used by `SharedDirs`.

Rejected because:
1. Doesn't naturally extend to user scope (would need a similar column on `users`).
2. Per-entry operations (add one, remove one by ID) require a full read-modify-write cycle.
3. User explicitly requested "proper DB representation."

### C. Repurpose skill bank scope (auto-inject all project-scoped bank skills)

Make all skills with `scope="project"` / `scope_id=<projectID>` auto-inject. No new data model.

Rejected: conflates ownership (which skills a project manages) with injection (which skills are forced into every agent). A project may own many skills without wanting them all auto-injected. The user explicitly confirmed this distinction in Batch 1.

### D. Broker-side fetch at provision time

Broker queries hub for injected skills lists during provisioning, then augments the skill list before Step 3b.

Rejected: extra round-trip latency, broker protocol change, harder to test. Hub-side merge in `populateAgentConfig` is the established pattern for project/template data enrichment.

---

## Migration / Rollout

- **Schema migration:** `ent generate` produces the new `skill_injections` table. No existing data is affected. The new table is empty on first deploy.
- **API backward compat:** All new endpoints are additive. No existing endpoints change.
- **Provisioner:** `mergeInjectedSkills` is a no-op when all three injected skills lists are empty (the common case on first deploy). `InlineConfig.Skills` is populated exactly as before.
- **Feature flag:** Not required. Empty injected skills lists = no behavior change. Safe to deploy incrementally phase by phase.
- **CLI:** New subcommands are additive. Existing commands are unchanged.
- **Platform skills (Phase 6):** See above. Step 3a2 preserved during migration; optional flag on seeded entries prevents provisioning failures.

---

## Open Questions

None. All nine design questions were resolved with the user on 2026-07-22 (Telegram thread 6130).

For reference, the resolved questions:
1. Scope: injected skills, hub+user+project, union semantics ✓
2. Data model: proper DB table (skill_injections) ✓
3. Versioning: same as existing SkillReference (pinned or floating per-URI) ✓
4. Conflict: dedupe by base URI, most-specific wins, warn on conflict ✓
5. Failure: existing optional flag, no new behavior ✓
6. Platform skills: hub-scope immutable system entries, seeded on startup ✓
7. Solo mode: hub-only, absent in solo mode ✓
8. CLI: both CLI and UI, common hub API ✓
9. Harness: no new code, existing SkillsDir() machinery handles it ✓

---

## Implementation Phases

### Phase 1 — Data model + store

**Branch:** `scion/project-skills-phase1`  
**Scope:** Persistence layer only. No API, no business logic.

- New file `pkg/ent/schema/skill_injection.go`: `SkillInjection` entity.
- Run `go generate ./pkg/ent/...` to produce entadapter boilerplate.
- New file `pkg/store/entadapter/skill_injection_store.go`: `SkillInjectionStore` implementation.
- `pkg/store/models.go`: Add `SkillInjection` struct and `ToSkillReference()`.
- `pkg/store/store.go`: Add `SkillInjectionStore` interface; embed in top-level `Store`.
- `pkg/store/entadapter/skill_injection_store_test.go`: round-trip tests for CRUD and `SetSkillInjections`.
- SQLite migration: new `skill_injections` table via ent codegen.

**Acceptance criteria:**
- `go test ./pkg/store/...` passes.
- `CreateSkillInjection` + `ListSkillInjections` + `RemoveSkillInjection` + `SetSkillInjections` work correctly in tests.
- `ToSkillReference()` returns correct `api.SkillReference` values.

---

### Phase 2 — Hub API

**Branch:** `scion/project-skills-phase2`  
**Scope:** REST endpoints for managing injected skills lists. This is the common layer for CLI and UI.

- `pkg/api/types.go`: Add `SkillInjectionEntry`, `SkillInjectionList`.
- New file `pkg/hub/handlers_skills_injection.go`: handlers for project, user, and hub-scope endpoints.
- `pkg/hub/server.go`: Register new routes.
- Authorization wiring: project admin for project scope; self for user scope; hub admin for hub PUT.
- Unit tests for each handler (list, add, set, remove).
- `HubSkillInjectionSetting` type for hub_settings JSON parsing.

**Acceptance criteria:**
- `POST /api/v1/projects/{id}/injected-skills` adds an entry; `GET` returns it; `DELETE` removes it.
- `PUT /api/v1/projects/{id}/injected-skills` replaces the full list atomically.
- User scope endpoints work identically scoped to `users/me`.
- Hub scope `GET` returns system + user_defined arrays; `PUT` updates only user_defined.
- Unauthorized requests receive 401/403.

---

### Phase 3 — Provisioner integration

**Branch:** `scion/project-skills-phase3`  
**Scope:** Wire the hub API into agent provisioning.

- `pkg/hub/handlers_agent_create_helpers.go`:
  - Add `mergeInjectedSkills(ctx, agent, project)` method.
  - Add `mergeSkillRefs(scopes ...[]api.SkillReference) []api.SkillReference` function.
  - Add `baseSkillURI(uri string) string` helper.
  - Call `mergeInjectedSkills` at end of `populateAgentConfig`.
- Integration test: provision an agent in a project that has injected-skills entries → verify those skills appear in `agentHome/.claude/skills/` (or equivalent).
- Test conflict resolution: same URI at two scopes, verify most-specific wins and warning is logged.

**Acceptance criteria:**
- Agent provisioned in a project with an injected-skills entry has that skill installed.
- Agent provisioned by a user with a user-scope injected-skills entry has that skill installed.
- Hub-scope injected skills appear in all agents.
- Version conflict: most-specific scope entry installed; slog.Warn emitted.
- Optional injected skill that fails resolution: agent starts successfully without the skill.
- Required injected skill that fails resolution: provisioning fails with a clear error.
- Solo-mode agents (no hub): injected skills lists absent, template skills work as before.

---

### Phase 4 — CLI

**Branch:** `scion/project-skills-phase4`  
**Scope:** CLI surface for managing injected skills lists.

- `scion project skills list|add|remove` — calls hub API.
- `scion user skills list|add|remove` — calls hub API.
- `--as`, `--optional` flags for `add`.
- `--help` updated.
- Shell completion for the new subcommands.
- Unit tests for command parsing and hub client calls.

**Acceptance criteria:**
- `scion project skills add <project> scion://my-skill` adds the entry to the project's list.
- `scion project skills list <project>` shows the current list with enriched metadata where available.
- `scion project skills remove <project> <id|uri>` removes the entry.
- User scope commands work identically.
- All commands exit non-zero on API error.

---

### Phase 5 — Web UI

**Branch:** `scion/project-skills-phase5`  
**Scope:** UI for all three scopes, built on a single shared component.

**Key requirement:** All three scopes (project, user, hub) use the **same shared `<injected-skills-panel>` web component**, parameterized by scope and scope ID. This avoids duplication and ensures consistent UX. The component accepts:
- `scope: "project" | "user" | "hub"`
- `scopeId: string` (project/user UUID; empty for hub)
- `readonly: boolean` (for hub system entries)

Placement:
- **Project Settings → Resources → new "Skills" tab** (alongside Templates, Harness Configs, Shared Dirs). Uses `<injected-skills-panel scope="project" scopeId={projectId}>`.
- **User Settings → new "Skills" tab.** Uses `<injected-skills-panel scope="user" scopeId={userId}>`.
- **Hub Settings → new "Skills" section (admin).** Uses `<injected-skills-panel scope="hub">` — system entries rendered with `readonly=true`; user_defined entries editable.

Shared component features:
- Table of injected skill entries; for URIs that resolve to known skill bank entries, display name, slug, scope badge, and version; for unknown URIs, display raw URI.
- **Add:** Skill bank search picker (filtered to global + project-scoped skills via existing `/api/v1/skills` list endpoint) plus a free-form URI field for external/GCS/GitHub URIs. Optional flag checkbox. Alias (`as`) field.
- **Remove:** Per-row remove button.
- **Reorder:** Drag handle → PUT bulk set on drop.
- **System badge:** When `readonly=true`, rows show a lock icon and "System" badge; add/remove controls hidden.

**Acceptance criteria:**
- Project admin can add a skill to the project injected-skills list via the Skills tab.
- Skill bank skills display name/slug; external URIs display raw URI.
- User can add/remove skills from their own Skills tab.
- Hub admin sees both system entries (read-only) and user_defined entries (editable) in Hub Settings → Skills.
- The same component code is used in all three locations — no duplicate implementations.

---

### Phase 6 — Platform skills migration

**Branch:** `scion/project-skills-phase6`  
**Scope:** Represent built-in platform skills in the hub-scope injected skills list.

- `pkg/hub/server.go` (or hub startup): `seedPlatformSkillInsertions()` reads `resources.PlatformSkillsFS()`, builds `[]api.SkillReference` for each platform skill, upserts into `hub_settings["injected_skills"].system` with `origin="seeded"`, `expectedRevision=-1`, all entries `optional=true`.
- Hub Settings UI: system entries show which skill files they represent.
- Step 3a2 (`injectPlatformSkills`) is preserved unchanged (solo-mode backward compat).
- Open tracking issue: "Remove Step 3a2 embedded injection once all platform skills are in the skill bank."

**Acceptance criteria:**
- Hub admin sees platform skills listed as "System" entries in Hub Settings → Skills.
- After hub restart, system entries are refreshed from the current binary.
- System entries cannot be modified via the API or UI.
- Provisioning behavior is unchanged (Step 3a2 still runs; platform skills installed by both paths, then deduplicated at Step 3b by the existing `resolved-skills.json` content-hash check).

---

## Acceptance Criteria (Top-Level)

1. **Project injected skills:** A project admin can add/remove skills from the project injected skills list via web UI and CLI. Skills appear in every agent provisioned in that project.
2. **User injected skills:** A user can add/remove skills from their personal injected skills list. Skills appear in every agent they own, across projects.
3. **Hub injected skills:** A hub admin can manage hub-scope user_defined injected skills entries. Skills appear in all agents on the hub.
4. **Union semantics:** An agent's installed skills are the union of hub + user + project + template skills (no scope silently excluded).
5. **Conflict resolution:** When the same skill URI appears at multiple scopes with different versions, the most-specific scope wins and a warning is logged. No silent data loss.
6. **Failure handling:** Optional injected skills that fail resolution cause a warning but not a provisioning failure. Required skills fail provisioning with a clear error message.
7. **No provisioner changes:** `ProvisionAgent` (`pkg/agent/provision.go`) is unchanged by this feature. The implementation is verified by diffing the file.
8. **Solo mode unchanged:** Provisioning in solo mode (no hub) is unaffected. Template skills work as before. No regressions.
9. **Backward compat:** Existing projects with no injected skills list behave identically to today.
10. **Platform skills visible:** After Phase 6, platform skills appear as immutable system entries in the Hub Settings → Skills UI, updated on hub restart.
