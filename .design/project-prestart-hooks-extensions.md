# Pre-Start Hooks: Hub-Level Attachment + Web UI — Design Document

**Status:** Draft  
**Author:** lifecycle-hooks-arch-2 (Architect Agent)  
**Date:** 2026-07-27  
**Extends:** `.design/project-prestart-hooks.md`  
**Branch:** `scion/lifecycle-hooks-arch-2` (based on `scion/lifecycle-hooks-dev`)  
**Handoff target:** Engineering Manager (EM) — see [Implementation Phases](#implementation-phases)

---

## Problem & Goals

The v1 pre-start hook feature (shipped on `scion/lifecycle-hooks-dev`, PR #879) is
project-scoped only. Two gaps remain:

1. **Hub-level attachment.** A hub administrator cannot define a default pre-start
   hook that applies to all projects without a project-level override. Hub admins
   need the same resource model that project owners have, scoped to the hub rather
   than a single project.

2. **No web UI.** The v1 feature is CLI-only. Two UI surfaces are required:
   - Hub Resources page (`/settings`) — manage hub-scoped hooks (hub admins).
   - Project Settings → Resources section (`/projects/{id}/settings`) — manage
     project-scoped hooks and see the inherited hub hook (project owners).

**Success criteria:**

1. A hub admin can create a named hook at hub scope via API, CLI, and web UI.
2. When an agent is created in a project with no active project hook, the hub hook
   (if any) is staged at `30-project-custom` as the fallback.
3. When a project has an active project hook, it overrides the hub hook entirely —
   the hub hook is not staged. (Execution model: project wins, one slot runs.)
4. Both UI surfaces follow existing UI conventions (tabs in Hub Resources and
   Project Settings Resources section); no new navigation patterns are invented.
5. All v1 project-hook acceptance criteria (from the original design doc) continue
   to pass.

---

## Non-Goals

- Additive / sequential execution (both hub and project hooks running per agent).
  User explicitly confirmed Option A (override) over Option B (sequential). This
  decision is **load-bearing** — if reversed later, the single-slot assumption in
  the broker must also change.
- Multiple simultaneously-active hooks at hub scope. One active hub hook at a time,
  mirroring the one-active-per-project invariant.
- Hub admin UI beyond the Hub Resources tab. A separate dedicated admin page for
  lifecycle hooks is not designed here.
- Post-start, pre-stop, or other hook points. Still deferred (v1 non-goal carries
  forward).

---

## Proposed Design

### 1. Execution Model (load-bearing)

**Option A — Fallback/Override (confirmed).** One pre-start hook ever runs per
agent. Resolution order at agent-create time:

```
1. GetActiveProjectPreStartHook(projectID)
   → found → stamp into AppliedConfig → done
2. GetActiveHubPreStartHook()
   → found → stamp into AppliedConfig → done
3. Neither found → no script staged
```

The staged filename is always `30-project-custom`, regardless of hook scope. The
broker does not need to know whether the script originated from a hub or project
hook — it only sees `AppliedConfig.ProjectPreStartHookScript`.

`AppliedConfig.ProjectPreStartHookID` continues to hold the hook's UUID for audit.
No new AppliedConfig field is needed.

**Why this is load-bearing:** The broker stages a single file. If sequential
execution were added later, a new prefix slot (`25-hub-custom`) would need to be
introduced, and both `init.go` and the broker would need changes to handle two
abort-on-failure conditions independently. The sequential change is not
backward-compatible. This design does not preclude adding it later, but it is
not a trivial follow-on.

---

### 2. Schema Extension

**Approach: add `scope` to the existing `ProjectPreStartHook` entity.**

This mirrors how `HarnessConfig` handles both global and project-scoped resources
via `scope` / `scope_id` fields without a separate entity. A separate
`HubPreStartHook` entity was considered and rejected (see
[Alternatives Considered](#alternatives-considered)).

**Changes to `pkg/ent/schema/projectprestarthook.go`:**

```go
// Add to Fields():
field.Enum("scope").
    Values("project", "hub").
    Default("project"),

// Make project_id optional (was NotEmpty — hub-scoped hooks have no project):
field.String("project_id").
    Optional(),   // was: NotEmpty()
```

**Index changes** (in `Indexes()`):

```go
// Replace: index.Fields("project_id", "slug").Unique()
// With:    composite including scope so hub hooks (project_id="") don't collide with
//          project hooks that happen to share a slug.
index.Fields("scope", "project_id", "slug").Unique(),

// Keep existing:
index.Fields("project_id", "status"),

// Add: efficient lookup of hub-scoped active hook
index.Fields("scope", "status"),
```

**Migration:** `AutoMigrate` adds the `scope` column with default `"project"` on
hub restart. All existing rows acquire `scope="project"` with no data migration.
The `project_id` column becomes nullable in the DB schema but existing rows are
unaffected (they have non-empty values). The Ent `Optional()` annotation permits
empty strings — no new null handling is needed in Go code.

**Existing indexes on `(project_id, slug)` become `(scope, project_id, slug)`.** The
old composite index is dropped and replaced. Existing data: project-scoped rows
still satisfy uniqueness within `(scope="project", project_id=<id>, slug)`.

---

### 3. Store Layer Extension

**`pkg/store/project_pre_start_hook.go` — extend `ProjectPreStartHookStore`:**

```go
// Add to the store model:
type ProjectPreStartHook struct {
    // ... (existing fields) ...
    Scope     string `json:"scope"`      // "project" | "hub"
}

// Add to ProjectPreStartHookStore interface:

// GetActiveHubPreStartHook returns the single active hub-scoped hook,
// or store.ErrNotFound if none exists.
GetActiveHubPreStartHook(ctx context.Context) (*ProjectPreStartHook, error)

// ListHubPreStartHooks returns all hub-scoped hooks (all statuses),
// ordered by creation time descending.
ListHubPreStartHooks(ctx context.Context) ([]*ProjectPreStartHook, error)

// CreateHubPreStartHook creates a new hub-scoped hook and archives any
// existing active hub hook atomically.
CreateHubPreStartHook(ctx context.Context, hook *ProjectPreStartHook) (*ProjectPreStartHook, error)

// UpdateHubPreStartHook updates mutable fields of a hub-scoped hook.
UpdateHubPreStartHook(ctx context.Context, hook *ProjectPreStartHook) (*ProjectPreStartHook, error)

// ActivateHubPreStartHook sets the identified hub-scoped hook to active
// and archives all other hub-scoped hooks atomically.
ActivateHubPreStartHook(ctx context.Context, hookID string) (*ProjectPreStartHook, error)

// DeleteHubPreStartHook hard-deletes a hub-scoped hook. Returns
// store.ErrInvalidInput if the hook is currently active.
DeleteHubPreStartHook(ctx context.Context, hookID string) error

// GetHubPreStartHook returns a specific hub-scoped hook by ID.
GetHubPreStartHook(ctx context.Context, hookID string) (*ProjectPreStartHook, error)
```

**Ent adapter** (`pkg/store/entadapter/project_pre_start_hook_store.go`) adds
corresponding implementations. Hub-scoped queries filter on `scope = "hub"`;
project-scoped queries filter on `scope = "project"` (added explicitly, for
safety, so existing `project_id`-only queries don't accidentally match hub rows).

The `entPSHToStore` conversion function gains a `Scope` field mapping.

---

### 4. Hub-Level API Routes

**New top-level routes** registered in `pkg/hub/server.go`:

```
GET    /api/v1/pre-start-hooks
POST   /api/v1/pre-start-hooks
GET    /api/v1/pre-start-hooks/{hookId}
PUT    /api/v1/pre-start-hooks/{hookId}
DELETE /api/v1/pre-start-hooks/{hookId}
POST   /api/v1/pre-start-hooks/{hookId}/activate
```

**New file:** `pkg/hub/hub_pre_start_hook_handlers.go`

These mirror `handleProjectPreStartHooks` / `handleProjectPreStartHookByID` but:
- Call `s.store.GetHubPreStartHook` / `s.store.ListHubPreStartHooks` etc.
- Use `HubIdentity`-based authorization (hub admin only), not project-owner auth.

**Authorization:** Hub admin role required for all methods (GET and mutating). A
non-admin user calling these endpoints receives 403. Reasoning: hub hooks affect
all agents on the hub — they are a hub-admin concern, not a project-owner concern.
This mirrors the harness-config authorization model at hub/global scope.

**Request/response shapes** are identical to the project-level equivalents.
`CreateHubPreStartHookRequest`, `UpdateHubPreStartHookRequest`, and
`ListProjectPreStartHooksResponse` (reused) are sufficient.

**Registration in `pkg/hub/server.go`:**

```go
s.mux.HandleFunc("/api/v1/pre-start-hooks", s.handleHubPreStartHooks)
s.mux.HandleFunc("/api/v1/pre-start-hooks/", s.handleHubPreStartHookByID)
```

---

### 5. Hub-Side Stamping — Fallback Logic

**`pkg/hub/handlers_agent_create_helpers.go`** — the existing stamping block
becomes a two-step resolution:

```go
if project != nil && agent.AppliedConfig.ProjectPreStartHookID == "" {
    // 1. Project-scope hook (takes precedence)
    hook, err := s.store.GetActiveProjectPreStartHook(ctx, project.ID)
    if err != nil && !errors.Is(err, store.ErrNotFound) {
        s.agentLifecycleLog.Warn("failed to resolve project pre-start hook",
            "project_id", project.ID, "error", err)
    }

    // 2. Hub-scope fallback (only if no project hook found)
    if hook == nil {
        hook, err = s.store.GetActiveHubPreStartHook(ctx)
        if err != nil && !errors.Is(err, store.ErrNotFound) {
            s.agentLifecycleLog.Warn("failed to resolve hub pre-start hook", "error", err)
        }
    }

    if hook != nil {
        agent.AppliedConfig.ProjectPreStartHookID = hook.ID
        agent.AppliedConfig.ProjectPreStartHookScript = hook.Script
    }
}
```

No broker changes are needed. The broker continues to check
`AppliedConfig.ProjectPreStartHookScript` and stage `30-project-custom` if set.

---

### 6. Hub Client Extension

**`pkg/hubclient/client.go`** — add to the `Client` interface:

```go
// HubPreStartHooks returns the hub-scoped pre-start hook service.
HubPreStartHooks() HubPreStartHookService
```

**New file:** `pkg/hubclient/hub_pre_start_hook.go`

```go
type HubPreStartHookService interface {
    List(ctx context.Context) (*ListPreStartHooksResponse, error)
    Get(ctx context.Context, hookID string) (*store.ProjectPreStartHook, error)
    Create(ctx context.Context, req *CreateProjectPreStartHookRequest) (*store.ProjectPreStartHook, error)
    Update(ctx context.Context, hookID string, req *UpdateProjectPreStartHookRequest) (*store.ProjectPreStartHook, error)
    Activate(ctx context.Context, hookID string) (*store.ProjectPreStartHook, error)
    Delete(ctx context.Context, hookID string) error
}
```

This service calls `/api/v1/pre-start-hooks` (no project ID prefix).

---

### 7. CLI Extension — `scion hub hook`

**New file:** `cmd/hub_pre_start_hook.go`

Subcommand path: `scion hub hook <subcommand>`

```
scion hub hook list
    List all hub-scoped pre-start hooks (active + archived).
    Output: table with ID, slug, status, created date.

scion hub hook create  --name <name> --script <file-or->
                       [--slug <slug>] [--description <desc>]
    Create a new hub-scoped hook and activate it.
    --script accepts a file path or "-" for stdin.

scion hub hook show    <slug-or-id>
    Print hook details including full script content.

scion hub hook update  <slug-or-id>
                       [--script <file-or->] [--name <name>] [--description <desc>]
    Update mutable fields of a hub-scoped hook.

scion hub hook activate <slug-or-id>
    Set an archived hub hook to active (archives the current active hub hook).

scion hub hook delete  <slug-or-id>
    Delete an archived hub hook. If the hook is active, --force is required.
```

Registered in `cmd/hub.go` under `hubCmd`.

**Pattern:** follows `cmd/project_pre_start_hook.go` exactly, replacing
`c.ProjectPreStartHooks(projectID)` calls with `c.HubPreStartHooks()`.

---

### 8. Web UI — Two New Surfaces

Pre-start hooks are not file-based resources, so `scion-resource-list` does not
apply. A new **bespoke list+CRUD component** is introduced, following the patterns
of `scion-env-var-list` and `scion-injected-skills-panel` (self-contained,
manages its own loading state and inline forms).

#### 8a. New Component: `scion-pre-start-hook-list`

**New file:** `web/src/components/shared/pre-start-hook-list.ts`

```typescript
@customElement('scion-pre-start-hook-list')
export class ScionPreStartHookList extends LitElement {
  /** API base path — '/api/v1/projects/${id}' for project scope,
   *  '/api/v1' for hub scope. */
  @property({ type: String }) apiBasePath = '';

  /** When true, no create/edit/delete affordances are shown. */
  @property({ type: Boolean }) readonly = false;

  /**
   * Hub-level fallback hook to display as an inherited indicator.
   * Only relevant when scope is project. When set, a read-only
   * "Inherited from hub: <name>" banner is shown if no project hook
   * is active. The banner disappears when a project hook is activated.
   */
  @property({ type: Object }) inheritedHook: PreStartHookSummary | null = null;
}
```

**Rendered structure (illustrative):**

```
┌─ Pre-Start Hooks ───────────────────────────────────────────────────┐
│  ╔════ Inherited from hub: "baseline-setup" ═══╗ (dimmed, read-only) │
│  ║  This hub-default script will run when no    ║                    │
│  ║  project hook is active.                     ║                    │
│  ╚═══════════════════════════════════════════════╝                    │
│                                                                       │
│  [+ Create Hook]                                                      │
│                                                                       │
│  ┌──────────────────────────────────────────────────────────────┐    │
│  │ NAME          SLUG          STATUS    CREATED   ACTIONS      │    │
│  │ Install tools install-tools ● active  Jul 25    [Edit][Arch] │    │
│  │ Old setup     old-setup     ○ archived Jul 10  [Activate][Del]│    │
│  └──────────────────────────────────────────────────────────────┘    │
└───────────────────────────────────────────────────────────────────────┘
```

**Inline create/edit form** (opens as an inline panel, not a dialog — follows
`scion-injected-skills-panel` expand pattern):

```
Name:        [________________]
Slug:        [________________]  (auto-derived from name)
Description: [________________]
Script:      [textarea, monospace, 10 rows, 64 KB max enforced client-side]

[Save]  [Cancel]
```

**API calls made by the component:**
- List: `GET {apiBasePath}/pre-start-hooks`
- Create: `POST {apiBasePath}/pre-start-hooks`
- Update: `PUT {apiBasePath}/pre-start-hooks/{id}`
- Activate: `POST {apiBasePath}/pre-start-hooks/{id}/activate`
- Delete: `DELETE {apiBasePath}/pre-start-hooks/{id}`

The "inherited from hub" banner is purely visual — the component does not fetch the
hub hook itself; the parent page fetches it and passes it via `inheritedHook`. This
keeps the hub API call centralized in the page, not duplicated per-component.

---

#### 8b. Hub Resources Page — `web/src/components/pages/settings.ts`

Add a **"Pre-Start Hooks"** tab to the existing `sl-tab-group` in
`renderResourcesSection()`, after the "Harness Configs" tab:

```typescript
<sl-tab slot="nav" panel="pre-start-hooks"
        ?active=${this.activeTab === 'pre-start-hooks'}>
  Pre-Start Hooks
</sl-tab>

<sl-tab-panel name="pre-start-hooks">
  <p class="tab-intro">
    Hub-wide default pre-start hook. Staged for any agent whose project
    has no project-level hook active.
  </p>
  <scion-pre-start-hook-list
    apiBasePath="/api/v1"
    ?readonly=${!this.pageData?.isAdmin}
  ></scion-pre-start-hook-list>
</sl-tab-panel>
```

No `inheritedHook` prop — the hub settings page shows the hub hooks themselves,
not an upstream fallback.

Deep-link: `/settings?tab=pre-start-hooks` (follows existing tab deep-link pattern).

---

#### 8c. Project Settings Page — `web/src/components/pages/project-settings.ts`

**State addition:**

```typescript
@state() private hubHook: PreStartHookSummary | null = null;
```

**Load the hub fallback hook** alongside other project data. Add to
`loadProject()` (or a parallel call in `connectedCallback`):

```typescript
// Fetch the active hub hook (if any) for the inherited indicator.
// Non-admin users may call this endpoint (read-only GET). If 403, treat as null.
const hubResp = await apiFetch('/api/v1/pre-start-hooks?status=active&limit=1');
if (hubResp.ok) {
  const data = await hubResp.json();
  this.hubHook = data.hooks?.[0] ?? null;
}
```

**Add "Pre-Start Hooks" tab** to `renderResourcesSection()`, after "Harness Configs":

```typescript
<sl-tab slot="nav" panel="pre-start-hooks"
        ?active=${this.activeResourcesTab === 'pre-start-hooks'}>
  Pre-Start Hooks
</sl-tab>

<sl-tab-panel name="pre-start-hooks">
  ${this.renderPreStartHooksContent()}
</sl-tab-panel>
```

**New render method:**

```typescript
private renderPreStartHooksContent() {
  const canEdit = canAny(this.project!._capabilities, 'update', 'manage');
  return html`
    <div class="section-header" style="margin-bottom: 1rem;">
      <div class="section-header-text">
        <p style="margin: 0;">
          Project-scoped pre-start scripts staged before each agent container starts.
          A project hook overrides the hub-wide default.
        </p>
      </div>
    </div>
    <scion-pre-start-hook-list
      apiBasePath="/api/v1/projects/${this.projectId}"
      ?readonly=${!canEdit}
      .inheritedHook=${this.hubHook}
    ></scion-pre-start-hook-list>
  `;
}
```

Deep-link: `/projects/{id}/settings?tab=pre-start-hooks`.

---

### 9. Types (`web/src/shared/types.ts`)

Add the `PreStartHookSummary` type (used by `inheritedHook` prop):

```typescript
export interface PreStartHookSummary {
  id: string;
  name: string;
  slug: string;
  scope: 'project' | 'hub';
  status: string;
}
```

The full `ProjectPreStartHook` type (for list responses) is a superset of this.

---

## Alternatives Considered

### 1. Separate `HubPreStartHook` entity

A distinct `pkg/ent/schema/hubprestarthook.go` entity with its own table, store
interface, and API handlers — no `scope` field on `ProjectPreStartHook`.

**Rejected because:**
- Duplicates the entire schema, store, and API surface without meaningful
  structural difference. The only difference is `project_id` is absent.
- `HarnessConfig` already established the `scope`+`scope_id` pattern for
  exactly this case. Diverging creates maintenance inconsistency.
- Two entities mean two migration steps and two test suites for the same logical
  feature. The added isolation is not worth the cost.

### 2. Sequential execution (Option B — both hub and project hooks run)

Hub hook stages at `25-hub-custom`, project hook at `30-project-custom`. Both
run during `EventPreStart`. If either exits non-zero, agent aborts.

**Rejected (user confirmed Option A explicitly)** and structurally complex:
- `init.go` abort logic currently checks a single "project hook staged" boolean.
  Two-hook support requires independent abort decisions per slot.
- Failure attribution: if the hub hook fails, should the project owner see the
  error? They didn't write it. Error UX is murkier.
- Broker staging needs two `WriteFile` calls and two AppliedConfig fields.
- Not reversible without also clearing `25-hub-custom` slots from agent homes
  on rollback.
- The use case (hub admin mandating a hook that always runs) is not an explicit
  current requirement.

### 3. Hub hook attached via project-level settings (default annotation)

Store the hub hook reference as a hub-level annotation or setting, resolved
client-side (in CLI or hub stamping) by lookup. No new resource entity.

**Rejected because:**
- No versioning or naming — same problem as the annotation approach rejected in
  the v1 design.
- "Default hook" is itself a resource worth managing with history, activation,
  and slug identity. Annotations don't support any of that.

### 4. Hub hook as a special `project_id = "__hub__"` sentinel

Use a reserved sentinel project ID (e.g., `"__hub__"`) to mark hub-scoped
hooks in the existing `project_pre_start_hooks` table, without adding a `scope`
column.

**Rejected because:**
- The sentinel would be a magic string, not a typed enum. Any code that iterates
  `project_id`-filtered queries would need a "not sentinel" guard to avoid
  surfacing hub hooks as if they were project hooks.
- The index `(project_id, slug)` would require an additional exception for the
  sentinel scope. A typed `scope` column is cleaner and self-documenting.
- HarnessConfig precedent uses `scope`; consistency wins.

### 5. No bespoke component — extend `scion-resource-list` with a new `kind`

Add `kind = 'pre-start-hook'` to the existing `ResourceKind` union in
`resource-list.ts` and extend the component to handle inline scripts.

**Rejected because:**
- `scion-resource-list` is designed for **file-based** resources (templates,
  harness-configs). Its model (`ResourceItem`, clone/delete/file-browse actions)
  does not map to pre-start hooks, which have status transitions (activate/archive)
  and an inline script field — not a file tree.
- Adding a third divergent `kind` would bloat `resource-list.ts` with
  special-cased branches. The `scion-env-var-list` / `scion-injected-skills-panel`
  precedent shows that resource-specific components are the right call here.

---

## Migration / Rollout

| Step | Risk | Rollback |
|---|---|---|
| Ent schema: add `scope`, make `project_id` optional | None — new column with default; no existing data affected | Drop `scope` column, revert `project_id` to `NotEmpty` |
| Index change: `(project_id, slug)` → `(scope, project_id, slug)` | None — existing rows satisfy the new index | Revert index definition |
| New store methods (`GetActiveHubPreStartHook`, etc.) | None — additive interface extension | Remove methods |
| New `/api/v1/pre-start-hooks` routes | None — new routes, no existing handlers affected | Remove route registrations |
| Stamping change (two-step resolution) | **Low** — only activates when a hub hook exists; existing projects with no hub hook are unchanged | Revert to single `GetActiveProjectPreStartHook` call |
| Web UI tabs | None — new tabs behind existing auth gates | Remove tab entries |
| CLI `scion hub hook` | None — new subcommand | Remove subcommand registration |

**Deployment order:** All backend changes ship in one release. Schema migration
runs on hub restart. No data migration scripts required. The web UI and CLI
changes are independently deployable once the API surface is available (they
degrade gracefully if the endpoint is absent).

**Backward compatibility:** Agents created before this change have
`AppliedConfig.ProjectPreStartHookScript` set from the project hook at creation
time. This field is unaffected. The hub hook fallback only applies to agents
created after this feature ships.

---

## Open Questions

1. ~~**Hub hook list access for non-admins (web UI).**~~ **Resolved (2026-07-27):**
   Read (GET list/detail) is open to any authenticated user; all mutations
   (POST/PUT/DELETE/activate) require hub admin. Mirrors HarnessConfig GET
   authorization. The "Inherited from hub" banner in project settings is therefore
   always visible to project owners.

2. **CLI: where does `scion hub hook` live in the command tree?** The current design
   places it under `scion hub hook` (subcommand of `hubCmd`). If hub admin commands
   are expected to live under a different namespace (e.g., `scion admin hook`), the
   CLI placement should change. This is easily reversible.

3. **Script viewer in project settings UI.** Should clicking a project-scope hook
   in the list open the script inline (textarea in place) or navigate to a detail
   page (`/projects/{id}/pre-start-hooks/{id}`)? The current design uses inline
   expansion (consistent with env-var-list patterns). If a dedicated detail page is
   preferred (consistent with harness-config-detail page), a new page component and
   routing entry are required. Inline is simpler and sufficient for scripts up to
   64 KB.

---

## Implementation Phases

Three parallel workstreams after the schema/store phase (A1 must complete first).
Suggested EM breakdown:

### Workstream A — Backend (1 developer)

**A1 — Schema + Store** _(unblocks everything else)_

Files touched:
- `pkg/ent/schema/projectprestarthook.go` (add `scope`, make `project_id` optional, update indexes)
- `pkg/store/project_pre_start_hook.go` (add hub-scoped methods to interface, extend store model with `Scope`)
- `pkg/store/entadapter/project_pre_start_hook_store.go` (hub-scoped query implementations; update `entPSHToStore`)
- Run `go generate ./pkg/ent/`

Tests: `pkg/store/entadapter/project_pre_start_hook_store_test.go` — add hub-scope CRUD, activate-archives-previous (hub scope), verify project-scope and hub-scope rows do not interfere.

**A2 — Hub API + Agent Stamping** _(depends on A1)_

Files touched:
- `pkg/hub/hub_pre_start_hook_handlers.go` (new — GET list/detail, POST create, PUT update, DELETE, POST activate; hub-admin auth)
- `pkg/hub/server.go` (register `/api/v1/pre-start-hooks` and `/api/v1/pre-start-hooks/` routes)
- `pkg/hub/handlers_agent_create_helpers.go` (extend stamping to two-step project→hub fallback)
- `pkg/hubclient/hub_pre_start_hook.go` (new — `HubPreStartHookService`)
- `pkg/hubclient/client.go` (add `HubPreStartHooks()` to interface and concrete client)

Tests:
- `pkg/hub/hub_pre_start_hook_handlers_test.go` (new) — CRUD endpoints, hub-admin-only auth (non-admin gets 403), script-too-large gets 400.
- `pkg/hub/handlers_agent_create_helpers_test.go` — extend to verify: (a) hub hook stages when no project hook exists; (b) project hook stages (not hub hook) when both are active.

---

### Workstream B — Frontend Web UI (1 developer, can start after A1)

Workstream B can be developed in parallel with A2, using the project-level hooks
API (already shipped) for component development. Full integration tests require
A2.

**B1 — Shared Component**

Files touched:
- `web/src/shared/types.ts` (add `PreStartHookSummary` interface)
- `web/src/components/shared/pre-start-hook-list.ts` (new — `scion-pre-start-hook-list` component)
- `web/src/components/index.ts` (register new component)

Behavior:
- List hooks from `GET {apiBasePath}/pre-start-hooks`
- Inline create/edit form with name, slug, description, script textarea
- 64 KB client-side script size check before POST/PUT
- Status badges (active = filled circle, archived = empty circle)
- Per-row activate and delete actions; edit opens inline form
- "Inherited from hub" banner when `inheritedHook` prop is set and no active project hook exists

**B2 — Hub Resources Page**

Files touched:
- `web/src/components/pages/settings.ts` (add "Pre-Start Hooks" tab + `scion-pre-start-hook-list`)

**B3 — Project Settings Page**

Files touched:
- `web/src/components/pages/project-settings.ts` (add `hubHook` state, fetch active hub hook in `loadProject()`, add "Pre-Start Hooks" tab + `renderPreStartHooksContent()`)

B3 depends on B1. B2 and B3 can be done in the same PR or split.

---

### Workstream C — CLI (1 developer, can start after A1)

**C1 — Hub Hook CLI Commands**

Files touched:
- `cmd/hub_pre_start_hook.go` (new — `hubHookCmd` + `list`, `create`, `show`, `update`, `activate`, `delete` subcommands)
- `cmd/hub.go` (register `hubHookCmd` as subcommand of `hubCmd`)

Pattern: mirrors `cmd/project_pre_start_hook.go` exactly, using
`c.HubPreStartHooks()` instead of `c.ProjectPreStartHooks(projectID)`.

---

### Workstream D — Integration Tests (after A2 + B3 + C1)

**D1 — End-to-End Scenarios**

1. Hub hook only → agent created → `30-project-custom` staged with hub script.
2. Project hook only → agent created → `30-project-custom` staged with project script.
3. Both hub and project hooks active → agent created → `30-project-custom` contains
   project script (not hub script).
4. Hub hook active, project hook archived → agent created → hub script stages.
5. No hooks → agent created → `30-project-custom` absent.
6. Update hub hook after agents exist → existing agents unchanged (script baked into
   `AppliedConfig`); new agent picks up updated hub hook.

---

## Acceptance Criteria

QA should verify all of the following before signing off on the extension:

### Hub-level CRUD

1. **Create hub hook via API:** `POST /api/v1/pre-start-hooks` with valid JSON
   creates a hook with `scope: "hub"`, `status: "active"`. Non-admin user
   receives 403.

2. **One active hub hook at a time:** Creating a second hub hook archives the first.

3. **Script size limit:** `POST` with `script` exceeding 64 KB returns 400.

4. **CLI create:** `scion hub hook create --name "baseline" --script setup.sh`
   creates an active hub hook visible in `scion hub hook list`.

5. **CLI from stdin:** `cat setup.sh | scion hub hook create --name "baseline" --script -`
   works.

### Project overrides hub

6. **Hub hook fallback:** Agent created in project with no active project hook
   has `30-project-custom` staged from the hub hook script.

7. **Project hook overrides:** Agent created in project with active project hook
   (and active hub hook) has `30-project-custom` staged from the project hook
   script. Hub hook content does not appear.

8. **Archive project hook → hub fallback resumes:** After archiving a project
   hook, the next agent created in that project stages the hub hook (not the
   archived project hook).

### Web UI — Hub Resources page

9. **Pre-Start Hooks tab visible** at `/settings?tab=pre-start-hooks` for hub
   admins. Non-admin authenticated users can view the list (read-only).

10. **Create from UI:** Hub admin can create, activate, and delete hub hooks via
    the web UI. A non-admin sees the list but has no create/edit/delete affordances.

11. **Deep-link:** Navigating directly to `/settings?tab=pre-start-hooks` opens
    the Pre-Start Hooks tab without requiring other tabs to load first.

### Web UI — Project Settings page

12. **Pre-Start Hooks tab visible** at `/projects/{id}/settings?tab=pre-start-hooks`
    for project owners. Non-owner members cannot access project settings (existing
    gate, unchanged).

13. **Inherited hub banner:** When no project hook is active, the inherited hub
    hook (if any) is shown in a read-only "Inherited from hub" indicator. When a
    project hook is activated, the banner disappears.

14. **Project-scope CRUD from UI:** Project owner can create, activate, and delete
    project-scoped hooks. Each action refreshes the list.

### Regression — v1 scenarios

15. All 15 acceptance criteria from `.design/project-prestart-hooks.md` continue
    to pass unchanged.

16. **No regression for agents without hooks:** Projects with no hub or project
    hook behave identically to pre-feature behavior. `30-project-custom` is absent.

17. **Existing agents unaffected by hub hook creation:** Agents created before a
    hub hook is defined do not retroactively acquire the hook script. The script is
    baked into `AppliedConfig` at creation time.
