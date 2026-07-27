# Project Pre-Start Hook — Design Document

**Status:** Draft  
**Author:** lifecycle-hooks-arch (Architect Agent)  
**Date:** 2026-07-27  
**Implements:** `lifecycle-hooks-arch` brief  
**Handoff target:** `lifecycle-hooks-dev`

---

## Problem & Goals

Project owners need a way to run a custom shell script inside every agent
container for their project, before the agent process starts. The script runs
at the `EventPreStart` hook point — after container setup but before the child
process launches — giving it access to the full container filesystem and the
ability to install packages, write config files, set env vars, and so on.

**Success criteria:**

1. A project owner (no hub-admin role required) can register a named custom
   script via API and CLI.
2. When an agent is created in that project, the broker automatically stages
   the script into `$HOME/.scion/hooks/pre-start.d/30-project-custom` before
   container start.
3. If the script exits non-zero, agent startup is aborted and the agent enters
   error phase.
4. If no hook is registered, behavior is identical to today.

---

## Non-Goals

- Post-start, pre-stop, or session-end hook points. This design is pre-start
  only. Other hook points are explicitly deferred.
- Hub-level lifecycle hooks (`/api/v1/admin/lifecycle-hooks`). Those fire HTTP
  callbacks on phase transitions from the hub server and are an unrelated
  mechanism. See `.design/lifecycle-hooks.md`.
- Script sandboxing, signing, or admin approval before staging. The project-
  owner access tier (same as `ProjectSettings` PUT) is the authorization gate.
- GCS storage for large hook artifacts. Script content is stored inline in the
  DB (bounded to 64 KB); large artifact delivery is a future concern.
- Multiple simultaneously-active hooks per project. Exactly one `active` hook
  is staged per provisioning pass.
- Environment overlay output (`outputs/env.json` injection into the child
  process). The hook script can write files and env; env overlay requires the
  harness manifest plumbing. Can be added in a follow-on.

---

## Proposed Design

### Entity: `ProjectPreStartHook`

A new first-class Ent entity. Follows the structural pattern of `HarnessConfig`
(`pkg/ent/schema/harnessconfig.go`) but is inherently project-scoped (no
`scope`/`scope_id` indirection needed).

```go
// pkg/ent/schema/projectprestarthook.go
type ProjectPreStartHook struct{ ent.Schema }

func (ProjectPreStartHook) Fields() []ent.Field {
    return []ent.Field{
        field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
        field.String("project_id").NotEmpty(),           // project UUID
        field.String("name").NotEmpty(),                 // human label, e.g. "Install dev tools"
        field.String("slug").NotEmpty(),                 // URL-safe, unique within project
        field.String("description").Optional(),
        field.String("script").NotEmpty(),               // raw script content; max 64 KB at API layer
        field.Enum("status").
            Values("active", "archived").
            Default("active"),
        field.String("created_by").Optional(),           // user email
        field.String("updated_by").Optional(),
        field.Time("created").Default(time.Now).Immutable(),
        field.Time("updated").Default(time.Now).UpdateDefault(time.Now),
    }
}

func (ProjectPreStartHook) Indexes() []ent.Index {
    return []ent.Index{
        index.Fields("project_id", "slug").Unique(),
        index.Fields("project_id", "status"),            // list active hooks efficiently
    }
}

func (ProjectPreStartHook) Annotations() []schema.Annotation {
    return []schema.Annotation{
        entsql.Annotation{Table: "project_pre_start_hooks"},
    }
}
```

**One active hook per project.** When a new hook is created or an existing hook
is activated, all other hooks for the project are atomically archived. The API
enforces this; the DB does not (no unique partial index on `status = 'active'`
to keep the schema simple and portable across SQLite/Postgres).

**Script size limit: 64 KB.** Enforced at the hub API layer (HTTP handler),
not in the DB schema. Scripts larger than 64 KB are rejected with 400.

---

### Migration

`AutoMigrate` creates the table on hub restart. Add `"ProjectPreStartHook"` to
the `migrationEntities` slice in `pkg/ent/entc/migrate_beta.go` after `"Project"`
(no Ent edges, but `project_id` is a semantic FK):

```go
// pkg/ent/entc/migrate_beta.go
var migrationEntities = []string{
    // … existing entries …
    "ProjectPreStartHook",   // add here; independent entity, no Ent edge
    // …
}
```

Run `go generate ./pkg/ent/` after adding the schema file.

---

### Store Layer

New functions in `pkg/store/` (following the existing harness-config store
pattern):

```go
// GetActiveProjectPreStartHook returns the single active hook for a project,
// or store.ErrNotFound if none is registered.
GetActiveProjectPreStartHook(ctx, projectID string) (*ProjectPreStartHook, error)

// ListProjectPreStartHooks returns all hooks for a project (all statuses).
ListProjectPreStartHooks(ctx, projectID string) ([]*ProjectPreStartHook, error)

// CreateProjectPreStartHook creates a new hook and archives any existing
// active hook for the same project, atomically.
CreateProjectPreStartHook(ctx, hook *ProjectPreStartHook) (*ProjectPreStartHook, error)

// UpdateProjectPreStartHook updates mutable fields (name, description, script).
// Does not change status.
UpdateProjectPreStartHook(ctx, hook *ProjectPreStartHook) (*ProjectPreStartHook, error)

// ActivateProjectPreStartHook sets hook to active and archives all other hooks
// for the same project, atomically.
ActivateProjectPreStartHook(ctx, hookID, projectID string) (*ProjectPreStartHook, error)

// DeleteProjectPreStartHook hard-deletes a hook by ID. Archived hooks only;
// returns an error if the hook is active (caller must archive first).
DeleteProjectPreStartHook(ctx, hookID, projectID string) error
```

Response type in `pkg/store/models.go` (or a new `pkg/store/project_pre_start_hook.go`):

```go
type ProjectPreStartHook struct {
    ID          string    `json:"id"`
    ProjectID   string    `json:"projectId"`
    Name        string    `json:"name"`
    Slug        string    `json:"slug"`
    Description string    `json:"description,omitempty"`
    Script      string    `json:"script"`
    Status      string    `json:"status"` // "active" | "archived"
    CreatedBy   string    `json:"createdBy,omitempty"`
    UpdatedBy   string    `json:"updatedBy,omitempty"`
    Created     time.Time `json:"created"`
    Updated     time.Time `json:"updated"`
}
```

---

### Hub API Surface

Routes are registered inside `handleProjectRoutes`
(`pkg/hub/handlers_projects_core.go`) following the same `if strings.HasPrefix(subPath, …)` dispatch pattern used for `"env"`, `"secrets"`, `"injected-skills"`, etc.

```
GET    /api/v1/projects/{projectId}/pre-start-hooks
POST   /api/v1/projects/{projectId}/pre-start-hooks
GET    /api/v1/projects/{projectId}/pre-start-hooks/{hookId}
PUT    /api/v1/projects/{projectId}/pre-start-hooks/{hookId}
DELETE /api/v1/projects/{projectId}/pre-start-hooks/{hookId}
POST   /api/v1/projects/{projectId}/pre-start-hooks/{hookId}/activate
```

**New file:** `pkg/hub/project_pre_start_hook_handlers.go`

#### Authorization

Mirrors `handleProjectSettings` exactly — `UserIdentity` + `authzService.CheckAccess`:

- `GET` list/detail: `ActionRead` on `Resource{Type:"project", ID:projectID, OwnerID:project.OwnerID}`
- `POST` / `PUT` / `DELETE` / `POST activate`: `ActionUpdate` on the same resource
- Non-`UserIdentity` (agent, broker): always 403 on mutating operations; `GET` returns 200 same as settings

This gives project owners and hub admins full CRUD. Other project members who
have read access can list hooks (to observe what's staged) but cannot mutate.

#### Request/Response shapes

```go
// POST /pre-start-hooks  (create)
type CreateProjectPreStartHookRequest struct {
    Name        string `json:"name"`
    Slug        string `json:"slug"`
    Description string `json:"description,omitempty"`
    Script      string `json:"script"` // raw; size checked before store call
}

// PUT /pre-start-hooks/{id}  (update)
type UpdateProjectPreStartHookRequest struct {
    Name        *string `json:"name,omitempty"`
    Description *string `json:"description,omitempty"`
    Script      *string `json:"script,omitempty"`
}

// List response
type ListProjectPreStartHooksResponse struct {
    Hooks []*store.ProjectPreStartHook `json:"hooks"`
}
```

Single-resource responses return `*store.ProjectPreStartHook` directly.

---

### AgentAppliedConfig Extension

Add two fields to `AgentAppliedConfig` in `pkg/store/models.go`:

```go
// ProjectPreStartHookID is the ID of the active pre-start hook for this
// agent's project at creation time. Used for audit / lineage tracking.
ProjectPreStartHookID string `json:"projectPreStartHookId,omitempty"`

// ProjectPreStartHookScript is the script content, inlined at agent-create
// time for zero-latency delivery to the broker. Bounded to 64 KB.
// When non-empty, the broker stages it into pre-start.d/30-project-custom.
ProjectPreStartHookScript string `json:"projectPreStartHookScript,omitempty"`
```

**Design decision: agent-create-time binding.** The hook is captured into
`AppliedConfig` when the agent is created. Subsequent updates to the project's
hook do not affect running or suspended agents. To pick up a new hook version,
a new agent must be created. This is consistent with `HarnessConfigID`/
`HarnessConfigHash` semantics. See [Alternatives Considered](#alternatives-considered)
for the "always-latest" approach.

**Restart behavior.** On agent restart, `AgentAppliedConfig` is read from the
stored agent record. The broker re-stages the script from
`AppliedConfig.ProjectPreStartHookScript` (idempotent `WriteFile` call). No
hub API round-trip is needed.

---

### Hub-Side Stamping (agent create)

In `pkg/hub/handlers_agent_create_helpers.go`, after the harness-config
ID/hash stamping block (~line 263):

```go
// Stamp project pre-start hook for broker delivery.
// Mirrors HarnessConfigID stamping: resolve the active hook for the project
// and inline its script content into AppliedConfig.
if project != nil && agent.AppliedConfig.ProjectPreStartHookID == "" {
    hook, err := s.store.GetActiveProjectPreStartHook(ctx, project.ID)
    if err != nil && !errors.Is(err, store.ErrNotFound) {
        s.agentLifecycleLog.Warn("failed to resolve project pre-start hook",
            "project_id", project.ID, "error", err)
    }
    if hook != nil {
        agent.AppliedConfig.ProjectPreStartHookID = hook.ID
        agent.AppliedConfig.ProjectPreStartHookScript = hook.Script
    }
}
```

---

### Broker-Side Staging

**New helper** (alongside `writeHookWrapper` in `pkg/harness/container_script_harness.go`
or in a new `pkg/harness/project_hook.go`):

```go
// WriteProjectPreStartHook stages the project-owner-supplied script into the
// agent home's pre-start.d directory at the fixed prefix 30-project-custom.
// The prefix places it after the harness provisioner (20-harness-provision),
// giving provision.py a chance to run first.
func WriteProjectPreStartHook(agentHome, scriptContent string) error {
    dir := filepath.Join(agentHome, ".scion", "hooks", "pre-start.d")
    if err := os.MkdirAll(dir, 0755); err != nil {
        return fmt.Errorf("create pre-start.d: %w", err)
    }
    target := filepath.Join(dir, "30-project-custom")
    return os.WriteFile(target, []byte(scriptContent), 0755)
}
```

**Call site 1 — new agent provisioning** (`pkg/agent/provision.go`, after
`h.Provision()` at ~line 1247):

```go
if opts.AppliedConfig != nil && opts.AppliedConfig.ProjectPreStartHookScript != "" {
    if err := harness.WriteProjectPreStartHook(agentHome, opts.AppliedConfig.ProjectPreStartHookScript); err != nil {
        return "", "", nil, fmt.Errorf("stage project pre-start hook: %w", err)
    }
}
```

**Call site 2 — agent restart** (`pkg/agent/run.go`, after `h.Provision()` at
~line 431):

```go
if opts.AppliedConfig != nil && opts.AppliedConfig.ProjectPreStartHookScript != "" {
    if err := harness.WriteProjectPreStartHook(agentHome, opts.AppliedConfig.ProjectPreStartHookScript); err != nil {
        // Log and continue: the script was previously staged. If it's missing
        // from disk (e.g., ephemeral FS), the run will abort on pre-start failure.
        util.Debugf("Start: project pre-start hook re-staging failed: %v", err)
    }
}
```

The script is idempotently overwritten on every restart. If the `agentHome` is
on a persistent filesystem (the common case), the file from prior provisioning
is already present and this is a no-op overwrite.

---

### Abort-on-Failure Wiring

**Current behavior** (`cmd/sciontool/commands/init.go`, ~line 268):

```go
if err := lifecycleManager.RunPreStart(); err != nil {
    if harnessReq.Required {
        // abort: returns 1
    }
    // continue anyway
}
```

`harnessReq.Required` is true when a container-script harness provisioner
participates in `EventPreStart`. For built-in harnesses (`provisioner.type =
"builtin"`), `Required = false` and pre-start failures are non-fatal today.

**Required change in `init.go`:**

```go
// Detect whether a project pre-start hook was staged by the broker.
projectHookPath := filepath.Join(agentHome, ".scion", "hooks", "pre-start.d", "30-project-custom")
_, projectHookStatErr := os.Stat(projectHookPath)
projectHookStaged := projectHookStatErr == nil

if err := lifecycleManager.RunPreStart(); err != nil {
    log.Error("Pre-start hooks failed: %v", err)
    if harnessReq.Required || projectHookStaged {
        log.Error("Pre-start provisioning is required; aborting startup")
        _ = statusHandler.UpdatePhase(state.PhaseError, "", "")
        _ = statusHandler.SetMessage(fmt.Sprintf("pre-start hook failed: %v", err))
        return 1
    }
    log.Warn("Pre-start hooks failed (non-required harness, no project hook); continuing")
}
```

**Why not always abort?** Built-in harnesses may have pre-start hooks that are
intentionally non-fatal (e.g., best-effort setup scripts in
`/etc/scion/hooks/pre-start.d/`). We only want to enforce abort for the two
cases that the operator explicitly marked as required: (a) a container-script
harness provisioner, and (b) a staged project hook.

**Limitation.** `RunPreStart()` runs all pre-start scripts and returns a
combined error. If the harness provisioner and the project hook both run,
and only one fails, `init.go` cannot attribute the error to a specific script.
The abort fires if either `harnessReq.Required` or `projectHookStaged` is true.
In practice: container-script harnesses always have `Required = true`, so the
abort would fire regardless. For built-in harnesses with `Required = false`,
only the project hook's presence gates the abort — and in that case there is
no harness script in `pre-start.d/` to confuse the attribution.

---

### Script Ordering

```
pre-start.d/ execution order (lexical sort):
  10-...               ← reserved for future system hooks
  20-harness-provision ← container-script harness provisioner (provision.py)
  30-project-custom    ← project owner's script (THIS FEATURE)
  40-...               ← reserved for future project-level extensions
```

The project hook runs **after** `provision.py`. This means:
- `provision.py` has already run: auth credentials are available in
  `$HOME/.scion/harness/outputs/`, harness-native config files are written.
- The project hook can read harness outputs and override/extend them.
- The project hook cannot influence what `provision.py` does (it runs after).

This is the confirmed design decision. If a future use case requires running
before the harness provisioner, a `10-project-pre-provision` prefix would be
reserved for that.

---

### CLI Commands

**New file:** `cmd/project_pre_start_hook.go`

Subcommand path: `scion project hook <subcommand>` (alias: `scion project psh`)

```
scion project hook list     [--project <slug>]
    List all pre-start hooks for the project (active + archived).
    Output: table with ID, slug, status, created date.

scion project hook create   --name <name> --script <file-or-> [--slug <slug>] [--description <desc>] [--project <slug>]
    Create a new pre-start hook and activate it (archives any previous active hook).
    --script accepts a file path or "-" for stdin.

scion project hook show     <slug-or-id>   [--project <slug>]
    Print the hook details including full script content.

scion project hook update   <slug-or-id>   [--script <file-or->] [--name <name>] [--description <desc>] [--project <slug>]
    Update name, description, or script content of an existing hook.

scion project hook activate <slug-or-id>   [--project <slug>]
    Mark an archived hook as active (archives the current active hook).

scion project hook delete   <slug-or-id>   [--project <slug>]
    Delete a hook. If the hook is active, --force is required to confirm.
```

These map 1:1 to Hub API calls. `--project` resolves the project slug to ID
using the existing project-resolution helpers in the CLI.

---

## Alternatives Considered

### 1. Annotations blob on `ProjectSettings`

Store the script as a `scion.io/pre-start-hook-script` annotation on the
project, similar to how `default-model` is stored.

**Rejected because:**
- Annotations are unversioned, unnamed, and non-queryable. There is no way
  to list hook history or activate a previous version.
- The user explicitly confirmed a "proper resource model (named hook script
  entities)" — this alternative directly contradicts the confirmed decision.
- Annotation values cannot be easily managed by the CLI (no type-safe struct).

### 2. Extend `HarnessConfig` with a `pre_start_script` field

Add a `pre_start_script` string column to the existing `HarnessConfig` entity.
The harness provisioner would stage this script alongside its own.

**Rejected because:**
- Harness configs are harness-type-specific (e.g., `claude`, `gemini-cli`).
  A project may want a custom pre-start hook regardless of which harness is
  used; tying it to a harness config creates an unnecessary coupling.
- Harness configs have a complex lifecycle (GCS storage, hash verification,
  image status). A plain script field is a poor fit for this machinery.
- Authorization: harness-config mutation requires harness-config-scoped
  permissions; the project-owner authorization tier is what was confirmed.

### 3. Inline the script in the existing `ProjectSettings` PUT

Add `PreStartHookScript string` to `hubclient.ProjectSettings` and pass it
through `applyProjectSettingsToAnnotations`.

**Rejected because:**
- Same "no history/naming" problem as alternative 1.
- `ProjectSettings` is a general bag of display and operational preferences;
  a multi-line executable script is architecturally out of place.

### 4. Always abort on any `RunPreStart()` failure

Change `init.go` to remove the `harnessReq.Required` gate and always abort
when `RunPreStart()` returns an error, regardless of harness type.

**Not adopted for now.** Built-in harnesses (`provisioner.type = "builtin"`)
have `Required = false` by design — their pre-start scripts are best-effort.
Globally changing the abort behavior would be a silent breaking change for
those deployments. The targeted approach (abort iff harness is required OR
project hook is staged) is surgical and preserves existing semantics.

If the codebase evolves to a point where all pre-start scripts should be
treated as required, removing the `harnessReq.Required` check becomes a
simple one-line follow-on.

### 5. ID + hash delivery (broker fetches from Hub at start time)

Mirror `HarnessConfigID`/`HarnessConfigHash` exactly: stamp IDs only into
`AgentAppliedConfig`, have the broker call the Hub API to fetch the script at
start time, with local caching by content hash.

**Not adopted for the first cut.** Scripts are bounded to 64 KB. Inlining the
content into `AppliedConfig` avoids a broker→Hub round-trip and eliminates a
class of failure (Hub unreachable at agent start). The cost — slightly larger
`AgentAppliedConfig` JSON — is acceptable at these sizes.

If scripts grow large (e.g., bundled binary artifacts), the ID+hash pattern can
be added in a follow-on without breaking existing agents.

---

## Migration / Rollout

| Step | Risk | Rollback |
|---|---|---|
| Add `ProjectPreStartHook` Ent schema + `AutoMigrate` | None — new table, no existing data | Drop table |
| Add `ProjectPreStartHookID/Script` to `AgentAppliedConfig` | None — zero-value for existing agents, JSON forward-compatible | Remove fields (zero-value deserialization safe) |
| Add Hub API handlers (GET/POST/PUT/DELETE) | None — new endpoints behind project-owner auth | Remove route dispatch |
| Add hub-side stamping in `handlers_agent_create_helpers.go` | None — skipped when no hook registered | Remove if block |
| Add broker-side staging in `provision.go` / `run.go` | None — skipped when `ProjectPreStartHookScript` is empty | Remove if block |
| Modify `init.go` abort logic (add project-hook-staged check) | **Low** — additive condition; existing behavior preserved when no hook is staged | Revert condition |

The `init.go` change is the only file that touches existing logic. It is
backward-compatible: when no project hook is staged, `projectHookStaged =
false` and the condition is exactly equivalent to the existing code.

**Deployment order:** Ship in one release. The schema migration runs on hub
restart. No data migration needed. The broker receives the new
`ProjectPreStartHookScript` field automatically via `AgentAppliedConfig` JSON
deserialization.

---

## Open Questions

These are genuine ambiguities that could not be resolved from the codebase or
the confirmed decisions in the brief:

1. **Should `scion project hook` live under `scion project` or at the top
   level as `scion project-hook`?** The CLI convention for project-scoped
   resources is not fully consistent across the codebase. Either works; the
   design uses `scion project hook` as a subcommand group under `scion project`.

2. **What happens if `AppliedConfig.ProjectPreStartHookScript` is updated on
   a long-lived agent (admin patching)?** No mechanism exists today to update
   `AppliedConfig` post-creation. If one is added in the future, the re-staged
   script will take effect on the next restart. No special handling needed now.

3. **Is hard-delete the right behavior for hook deletion, or should it always
   archive?** The design supports hard-delete of archived hooks only. If the
   policy should be "always soft-delete," the store function can be changed
   without API surface changes.

---

## Implementation Phases

Each phase is one commit-sized chunk. Phases 1–3 are pure hub-side and can
land without broker changes. Phase 5 (init.go) is independently deployable.

### Phase 1 — Ent schema + store layer

Files touched:
- `pkg/ent/schema/projectprestarthook.go` (new)
- `pkg/ent/entc/migrate_beta.go` (add to `migrationEntities`)
- `pkg/store/project_pre_start_hook.go` (new — store interface + Ent-backed impl)
- Run `go generate ./pkg/ent/`

Test: `pkg/store/project_pre_start_hook_test.go` — CRUD, activate-archives-previous,
slug-uniqueness-within-project.

### Phase 2 — Hub API handlers

Files touched:
- `pkg/hub/project_pre_start_hook_handlers.go` (new)
- `pkg/hub/handlers_projects_core.go` (add route dispatch for `"pre-start-hooks"`)

Test: `pkg/hub/project_pre_start_hook_handlers_test.go` — all CRUD endpoints,
owner-only auth (non-owner gets 403), script-too-large gets 400.

### Phase 3 — AgentAppliedConfig extension + hub-side stamping

Files touched:
- `pkg/store/models.go` (add `ProjectPreStartHookID`, `ProjectPreStartHookScript`)
- `pkg/hub/handlers_agent_create_helpers.go` (add stamping block after harness-config block)

Test: extend existing agent-create helper tests to assert that when a project
has an active hook, `AppliedConfig.ProjectPreStartHookScript` is set.

### Phase 4 — Broker-side staging

Files touched:
- `pkg/harness/project_hook.go` (new — `WriteProjectPreStartHook` helper)
- `pkg/agent/provision.go` (call `WriteProjectPreStartHook` after `h.Provision()`)
- `pkg/agent/run.go` (call `WriteProjectPreStartHook` after `h.Provision()`)

Test: `pkg/harness/project_hook_test.go` — verifies that `WriteProjectPreStartHook`
writes an executable file at the correct path with correct content; that re-
calling it overwrites the previous content.

### Phase 5 — Abort-on-failure wiring (`init.go`)

Files touched:
- `cmd/sciontool/commands/init.go` (add `projectHookStaged` check)

Test: extend `init.go` tests to verify that when `30-project-custom` exists
and returns non-zero, the container exits 1 even when `harnessReq.Required =
false`.

### Phase 6 — CLI commands

Files touched:
- `cmd/project_pre_start_hook.go` (new)
- `cmd/root.go` or equivalent registration point (register new subcommand)

Test: CLI integration tests for `list`, `create`, `show`, `update`, `activate`,
`delete` subcommands.

### Phase 7 — End-to-end integration test

A single integration test (or QA test scenario) that:
1. Creates a project with no hook → creates an agent → verifies `30-project-custom` is absent.
2. Creates a hook via CLI → creates an agent → verifies the script runs at pre-start.
3. Creates an agent with a failing hook → verifies the agent enters error phase.
4. Archives the hook → creates an agent → verifies `30-project-custom` is absent again.

---

## Acceptance Criteria

The QA tester should verify all of the following before signing off:

1. **Create via API:** `POST /api/v1/projects/{id}/pre-start-hooks` with a
   valid JSON body creates a hook with `status: "active"`.

2. **Authorization:** The same POST from a non-project-owner user returns 403.
   A hub admin can create hooks for any project.

3. **Script size limit:** A POST with `script` exceeding 64 KB returns 400.

4. **Activate archives previous:** Creating a second hook for the same project
   sets the new hook to `active` and sets the previous hook to `archived`.

5. **Staging — no hook:** An agent created in a project with no active hook
   does not have `30-project-custom` in `$HOME/.scion/hooks/pre-start.d/`.

6. **Staging — with hook:** An agent created in a project with an active hook
   has `30-project-custom` staged with `mode 0755` and matching script content.

7. **Ordering:** `30-project-custom` runs after `20-harness-provision` during
   `EventPreStart` (confirmed by both the numeric prefix and a test that
   writes a timestamp from each script and verifies ordering).

8. **Success path:** A project hook that exits 0 allows the agent to start and
   reach `running` phase normally.

9. **Failure path:** A project hook that exits 1 causes the agent to abort
   startup and transition to `error` phase, with a human-readable error message
   visible in the agent's status.

10. **Restart re-staging:** Stopping and restarting an agent with a project hook
    results in `30-project-custom` being re-staged with the same content.

11. **Hook update does not affect existing agents:** Updating a project's hook
    script after an agent is created does not change what the existing agent
    stages on its next restart (the content is baked into `AppliedConfig`).

12. **Delete archived hook:** Deleting an archived hook succeeds. Attempting to
    delete an active hook without `--force` (CLI) / without archiving it first
    (API: archive before DELETE) returns a clear error.

13. **CLI list:** `scion project hook list` shows all hooks for the project
    with correct status column.

14. **CLI create from stdin:** `echo "#!/bin/sh\necho hello" | scion project hook create --name test --script -` creates a hook.

15. **No regression:** Agents in projects without hooks behave identically to
    pre-feature behavior. Existing integration tests pass.
