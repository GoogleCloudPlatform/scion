# Design: Relative Workspace Paths (logic-path)

**Branch:** `scion/logic-path` (off upstream `main`)
**Tracking:** [ptone/scion#466](https://github.com/ptone/scion/issues/466)
**Origin:** Alternative design from [PR #699 comment](https://github.com/GoogleCloudPlatform/scion/pull/699#issuecomment-4958423808)
**Status:** approved design — ready for implementation

## Problem

For directory (non-git) projects, an agent cannot currently be confined to a
subdirectory of the project workspace. The existing `--workspace` flag only
accepts absolute host paths. When a project is organized as a monorepo or
multi-package directory, every agent mounts the entire project root — there is
no way to scope an agent to `packages/web` or `services/api` as a **contained
mount** (only the subtree visible as `/workspace`).

PR #699 proposed a separate `--workspace-subdir` flag with a dedicated field
threaded end-to-end. During review, an alternative was worked out: extend the
existing `--workspace` flag to accept both absolute and relative paths, using
`filepath.IsAbs()` to distinguish the two modes. This eliminates a new flag and
data-model field while preserving the same security invariants.

## Core Idea

`--workspace` already accepts an absolute host path. Extend it to also accept a
**relative path**, interpreted as a subdirectory within the project's "logical
root":

```
scion start agent1 "task" --workspace /absolute/host/path   # current: mount exact host path
scion start agent1 "task" --workspace packages/web           # NEW: scope to project subdirectory
```

### Key Concepts

- **Logical root:** Every project has a logically constant base path. The hub,
  dispatcher, and broker each know their own local version of it (hub-managed
  path, provider local path, `settings.WorkspacePath`, or
  `filepath.Dir(projectDir)`), but conceptually it's the same reference point.
  A relative path against that logical base is well-defined and portable across
  the hub→broker boundary.

- **Containment, not workdir:** This is about **full containment** by mounting
  only the subtree. The agent's `/workspace` IS the subdirectory — the agent
  cannot see or access files outside it. This is distinct from a hypothetical
  `--workdir` behavior where the agent would start in a subdirectory but still
  have the full project mounted.

- **Detection:** `filepath.IsAbs()` — deterministic and unambiguous. No
  heuristics needed.

## Scope (v1)

**In scope:**
- Directory (non-git) projects — linked and hub-managed
- Shared-workspace git projects (the shared workspace mount is a directory, same
  logic applies)

**Out of scope (future extension):**
- Per-agent git projects (clone-based provisioning) — see [Future Work](#future-work)

**Rationale:** For per-agent git projects, `gitClone` config takes precedence
over `workspace` in the provisioner's if-else chain (`provision.go:485-571`).
Supporting relative workspace for clone-based projects would require
restructuring the provisioner to clone first, then mount only the subdir —
meaning the agent loses `.git` access. This is a meaningfully different use case
best addressed separately.

When a relative workspace is provided for a per-agent git (clone-based) project,
the broker MUST reject it with a clear error rather than silently ignoring it.

## Design Decision: `./subdir` Interpretation

### Background

The design comment raised a wrinkle: `--workspace ./subdir` today resolves via
`filepath.Abs()` in the broker's `ProvisionAgent()` (`provision.go:514`). Under
the new design, this would be reinterpreted as project-relative.

### Analysis

The `filepath.Abs()` call runs on the **broker**, not at the CLI. The CLI
passes the workspace string verbatim to the hub (`cmd/common.go:536,733`),
which threads it to the dispatcher, which sends it to the broker. So
`--workspace ./subdir` today resolves against the **broker's CWD** — not the
user's CWD. For remote brokers, this is effectively meaningless/broken.

### Decision: Option 3 — Document the Change

All relative paths (including `./` and `../` prefixed) are **project-relative**.
For host-specific mounts, use absolute paths.

This is a clean win with no real backward compatibility concern:
- CWD-relative resolution against the broker's CWD was never useful
- The flag help already says "Host path to mount as /workspace" — a relative
  path was never a documented host path
- Users wanting a specific host directory can use an absolute path

Update the flag help text to:
```
Host path or project-relative subdirectory to mount as /workspace
```

## Pipeline Changes

The workspace value flows through 5 stages. Changes are required in 3 of them.

### Stage 1: CLI (`cmd/start.go`, `cmd/create.go`, `cmd/common.go`)

**Change: Flag help text only**

The `--workspace` flag definition stays the same (same flag name, same variable).
Only update the help string:

```go
// cmd/start.go:53, cmd/create.go:325
startCmd.Flags().StringVarP(&workspace, "workspace", "w", "",
    "Host path or project-relative subdirectory to mount as /workspace")
```

No path resolution at the CLI layer — the value is passed verbatim.

### Stage 2: Hub `buildAppliedConfig()` (`pkg/hub/handlers_agent_create_helpers.go:80`)

**Change: None**

Copies `req.Workspace` verbatim into `AgentAppliedConfig.Workspace` (line 88).
Both absolute and relative values pass through unchanged. ✓

### Stage 3: Hub `populateAgentConfig()` (`pkg/hub/handlers_agent_create_helpers.go:122`)

**Change: Guard against overwriting relative workspace**

Current code (lines 147-152) unconditionally overwrites workspace for
hub-managed and shared-workspace projects:

```go
// CURRENT
if project != nil && (project.GitRemote == "" || project.IsSharedWorkspace()) {
    workspacePath, err := hubManagedProjectPath(project.Slug)
    if err == nil {
        agent.AppliedConfig.Workspace = workspacePath  // ← always overwrites
    }
}
```

New code — only overwrite when workspace is empty or absolute (a user-provided
relative path must survive to the broker):

```go
// NEW
if project != nil && (project.GitRemote == "" || project.IsSharedWorkspace()) {
    existingWorkspace := agent.AppliedConfig.Workspace
    if existingWorkspace == "" || filepath.IsAbs(existingWorkspace) {
        workspacePath, err := hubManagedProjectPath(project.Slug)
        if err == nil {
            agent.AppliedConfig.Workspace = workspacePath
        }
    }
    // else: relative workspace from user — preserve verbatim for broker
}
```

**Why this is safe:** An absolute `--workspace` is an explicit user override
that mounts a specific host path; the hub should not overwrite it with the
hub-managed path either. Currently, absolute user values ARE overwritten —
this guard fixes that pre-existing issue as a side effect.

### Stage 4: Dispatcher `buildCreateRequest()` (`pkg/hub/httpdispatcher.go:347`)

**Change: Only clear absolute workspace for local-path projects**

Current code (lines 431-432) unconditionally clears workspace when the broker
has a local project path:

```go
// CURRENT
if projectInfo.projectPath != "" {
    workspace = ""  // ← clears everything, including relative paths
}
```

New code — only clear absolute workspace; a relative path must survive to
the broker for project-root-relative resolution:

```go
// NEW
if projectInfo.projectPath != "" {
    if workspace == "" || filepath.IsAbs(workspace) {
        workspace = ""
    }
    // else: relative workspace — keep it; broker joins with its own project root
}
```

**Rationale:** The dispatcher clears absolute workspace because the broker
derives its own project root from the local path. But a relative workspace is
portable — it means the same thing regardless of which absolute path the broker
uses as the project root.

### Stage 5: Broker `ProvisionAgent()` (`pkg/agent/provision.go:400`)

**Change: Branch workspace handling on `filepath.IsAbs()`**

Current code (lines 511-525) always resolves workspace to absolute:

```go
// CURRENT — Case 1: Explicit Workspace
} else if workspace != "" {
    absWorkspace, err := filepath.Abs(workspace)
    // ... mount absWorkspace
    explicitWorkspace = true
}
```

New code — branch based on absolute vs relative:

```go
// NEW — Case 1: Explicit Workspace
} else if workspace != "" {
    if filepath.IsAbs(workspace) {
        // Current behavior: mount this exact host path
        absWorkspace, err := filepath.Abs(workspace)
        if err != nil {
            return "", "", nil, fmt.Errorf("failed to resolve absolute path for workspace %s: %w", workspace, err)
        }
        if _, err := os.Stat(absWorkspace); os.IsNotExist(err) {
            return "", "", nil, fmt.Errorf("workspace path does not exist: %s", absWorkspace)
        }
        workspaceSource = absWorkspace
        agentWorkspace = ""
        explicitWorkspace = true
    } else {
        // NEW: resolve relative subdir against project root
        if gitClone != nil {
            return "", "", nil, fmt.Errorf("relative --workspace is not supported for git-clone projects; use an absolute path or remove --workspace")
        }
        projectRoot := resolveProjectRoot(settings, projectDir)
        resolved, err := resolveWorkspaceSubdir(projectRoot, workspace)
        if err != nil {
            return "", "", nil, err
        }
        workspaceSource = resolved
        agentWorkspace = ""
        explicitWorkspace = true
    }
}
```

Note: The `gitClone != nil` check is a belt-and-suspenders guard. In normal
flow, `gitClone` is checked first in the if-else chain (line 485) and wins over
`workspace`. But if future refactoring changes the order, this explicit check
prevents silent misbehavior.

## New Functions

### `resolveProjectRoot()` (`pkg/agent/provision.go`)

Extracts project root resolution logic from the existing Case 3 code:

```go
// resolveProjectRoot determines the project root directory on this broker.
// Used for resolving relative --workspace paths against the project's logical root.
func resolveProjectRoot(settings *config.Settings, projectDir string) string {
    if settings != nil && settings.WorkspacePath != "" {
        return settings.WorkspacePath
    }
    return filepath.Dir(projectDir)
}
```

This matches the existing Case 3 (non-git, no explicit workspace) logic at
lines 564-567, extracted for reuse.

### `resolveWorkspaceSubdir()` (`pkg/agent/provision.go`)

Containment guard identical in semantics to PR #699's implementation:

```go
// resolveWorkspaceSubdir resolves a relative workspace subdirectory path
// against a project root, with containment checks to prevent directory
// traversal and symlink escapes.
//
// Returns the resolved absolute path, or an error if:
//   - subdir is an absolute path (programming error — callers should check)
//   - subdir contains ".." components that would escape the root
//   - the resolved real path is outside the project root (symlink escape)
//   - the resolved path does not exist
func resolveWorkspaceSubdir(projectRoot, subdir string) (string, error) {
    // Belt-and-suspenders: reject absolute paths
    if filepath.IsAbs(subdir) {
        return "", fmt.Errorf("workspace subdir must be relative, got absolute path: %s", subdir)
    }

    // Clean the path and reject traversal
    cleaned := filepath.Clean(subdir)
    if cleaned == "." {
        // --workspace . means the project root itself — valid but a no-op
        return projectRoot, nil
    }
    if strings.HasPrefix(cleaned, "..") {
        return "", fmt.Errorf("workspace subdir %q escapes project root (contains '..')", subdir)
    }

    // Join with project root
    joined := filepath.Join(projectRoot, cleaned)

    // Verify the path exists
    if _, err := os.Stat(joined); os.IsNotExist(err) {
        return "", fmt.Errorf("workspace subdirectory does not exist: %s", joined)
    }

    // Resolve symlinks and verify containment
    realRoot, err := filepath.EvalSymlinks(projectRoot)
    if err != nil {
        return "", fmt.Errorf("failed to resolve project root: %w", err)
    }
    realJoined, err := filepath.EvalSymlinks(joined)
    if err != nil {
        return "", fmt.Errorf("failed to resolve workspace subdir: %w", err)
    }

    // Containment check: the real resolved path must be under the real project root
    rel, err := filepath.Rel(realRoot, realJoined)
    if err != nil || strings.HasPrefix(rel, "..") {
        return "", fmt.Errorf("workspace subdir %q resolves to %s which is outside project root %s", subdir, realJoined, realRoot)
    }

    return realJoined, nil
}
```

**Security properties:**
1. Rejects absolute paths (defense in depth — caller should have checked)
2. Rejects `..` traversal in the raw path
3. Resolves symlinks via `filepath.EvalSymlinks()` before containment check
4. Verifies the resolved real path is under the real project root via
   `filepath.Rel()` — catches symlinks pointing outside the root
5. Requires the path to exist (prevents mount of nonexistent directories)
6. Runs broker-side where the real filesystem is visible

## Data Model

No new fields or schema changes. The existing `Workspace` string field carries
both modes (absolute = host path, relative = project subdirectory). The
`ExplicitWorkspace` boolean is set to `true` for both modes, preserving
resume/restart semantics.

| Struct | Field | Change |
|--------|-------|--------|
| `CreateAgentRequest` (`pkg/hubclient/agents.go:170`) | `Workspace` | None — accepts both forms |
| `AgentAppliedConfig` (`pkg/store/models.go:139`) | `Workspace` | None — stores both forms |
| `RemoteAgentConfig` (`pkg/hub/server.go:468`) | `Workspace` | None — carries both forms |
| `ScionConfig` (`pkg/api/types.go:483`) | `ExplicitWorkspace` | None — set for both modes |

## Backward Compatibility

| Scenario | Before | After |
|----------|--------|-------|
| `--workspace /abs/path` | Mount `/abs/path` | **Unchanged** |
| `--workspace` omitted | Derive from project | **Unchanged** |
| `--workspace ./subdir` | Resolve against broker CWD (broken for remote) | Resolve against project root |
| `--workspace packages/web` | Resolve against broker CWD (broken for remote) | Resolve against project root |
| `--workspace` on git-clone project | Mount absolute path | **Unchanged** for absolute; error for relative |

The only behavioral change is for relative paths, which were broken for remote
brokers (resolved against the broker's CWD, not the user's). This is a fix, not
a break.

## Security Invariants

1. **Absolute-path behavior fully unchanged** — no code path changes for
   `filepath.IsAbs(workspace) == true`.
2. **Relative paths stay relative through the pipeline** — no early resolution.
   Joined with the broker's own project root only at the point where the real
   filesystem is visible.
3. **Containment guard runs broker-side** — `resolveWorkspaceSubdir()` uses
   `filepath.EvalSymlinks()` + `filepath.Rel()` to catch both `..` traversal
   and symlink escapes.
4. **`ExplicitWorkspace = true`** for relative workspace mounts — prevents
   workspace widening on resume, matching existing behavior for explicit
   absolute mounts.

## Resume / Restart Semantics

When an agent created with `--workspace packages/web` is resumed:

1. `ProvisionAgent()` is called again with the persisted `workspace` value
   (`packages/web` — relative).
2. The same `resolveWorkspaceSubdir()` runs, resolving against the same
   project root.
3. `ExplicitWorkspace = true` prevents `detectRepoRoot()` from discovering a
   parent git repo and widening the mount.
4. The agent gets the same `/workspace` mount.

This is identical to the current resume behavior for absolute `--workspace`,
just with relative resolution added.

## Testing Strategy

### Unit Tests (`pkg/agent/provision_test.go`)

1. **`TestResolveWorkspaceSubdir`** — the containment guard:
   - Valid subdir resolves correctly
   - `"."` resolves to project root
   - Absolute path rejected
   - `..` traversal rejected
   - Nested `..` traversal rejected (`a/../../b`)
   - Symlink-to-outside rejected
   - Nonexistent path rejected

2. **`TestResolveProjectRoot`** — project root resolution:
   - With `settings.WorkspacePath` set → returns that
   - Without → returns `filepath.Dir(projectDir)`

3. **`TestProvisionAgent_RelativeWorkspace`** — end-to-end provisioner:
   - Relative workspace resolves against project root and mounts as `/workspace`
   - `ExplicitWorkspace = true` is set
   - Default (no workspace) still mounts project root

4. **`TestProvisionAgent_RelativeWorkspace_GitClone_Rejected`** — error case:
   - Relative workspace with gitClone config returns clear error

### Unit Tests (`pkg/hub/handlers_agent_create_helpers_test.go`)

5. **`TestPopulateAgentConfig_RelativeWorkspacePreserved`**:
   - Relative workspace is NOT overwritten by `hubManagedProjectPath`
   - Absolute workspace IS overwritten (existing behavior preserved)
   - Empty workspace IS overwritten (existing behavior preserved)

### Unit Tests (`pkg/hub/httpdispatcher_test.go`)

6. **`TestBuildCreateRequest_RelativeWorkspaceSurvives`**:
   - Relative workspace survives when `projectInfo.projectPath != ""`
   - Absolute workspace is still cleared (existing behavior)

### Resume Test (`pkg/agent/provision_test.go`)

7. **`TestGetAgent_RelativeWorkspaceResume`**:
   - Agent created with relative workspace, stopped, resumed — gets same mount
   - `ExplicitWorkspace` flag persists across resume

## Future Work

### Per-Agent Git Projects (Clone-Based)

For monorepo git projects where agents need to be scoped to a subdirectory:

- **Option A:** Clone the full repo, then bind-mount only the subdir as
  `/workspace`. Agent gets filesystem containment but loses `git` access.
- **Option B:** Clone the full repo, set `--workdir` to the subdir (new flag).
  Agent starts in the subdir but can still navigate up to the full repo. No
  containment, but full git access.
- **Option C:** Both — `--workspace packages/web` for containment,
  `--workdir packages/web` for scoping without containment.

These are distinct use cases that should be addressed in a follow-up design.

### `--workdir` Flag

A `--workdir` concept (set the working directory within the mounted workspace
without containment) is complementary to the relative `--workspace` feature.
This is intentionally out of scope for v1 to keep the change minimal and
well-defined.

---

## Phased Implementation Plan

### Phase 1: Broker-Side Resolution (Core)

**Files changed:**
- `pkg/agent/provision.go` — new `resolveWorkspaceSubdir()`, `resolveProjectRoot()`,
  and branched workspace handling in `ProvisionAgent()`

**Tests:**
- `pkg/agent/provision_test.go` — `TestResolveWorkspaceSubdir`,
  `TestResolveProjectRoot`, `TestProvisionAgent_RelativeWorkspace`,
  `TestProvisionAgent_RelativeWorkspace_GitClone_Rejected`

**Scope:** This phase is self-contained. Even without the hub/dispatcher
changes, a broker receiving a relative workspace (e.g. from a direct broker
CLI invocation) will resolve it correctly.

### Phase 2: Hub & Dispatcher Guards

**Files changed:**
- `pkg/hub/handlers_agent_create_helpers.go` — guard in `populateAgentConfig()`
- `pkg/hub/httpdispatcher.go` — guard in `buildCreateRequest()`

**Tests:**
- `pkg/hub/handlers_agent_create_helpers_test.go` —
  `TestPopulateAgentConfig_RelativeWorkspacePreserved`
- `pkg/hub/httpdispatcher_test.go` —
  `TestBuildCreateRequest_RelativeWorkspaceSurvives`

**Scope:** These changes let relative workspace values flow through the
hub→dispatcher→broker pipeline without being overwritten or cleared.

### Phase 3: CLI Polish & Resume Verification

**Files changed:**
- `cmd/start.go` — update flag help text
- `cmd/create.go` — update flag help text

**Tests:**
- `pkg/agent/provision_test.go` — `TestGetAgent_RelativeWorkspaceResume`

**Scope:** Flag help text update and verification that resume/restart works
correctly with relative workspace.

### Implementation Notes

- **All three phases can ship as a single PR.** They are ordered by dependency
  (broker core → pipeline threading → CLI polish) but the total change is small
  enough for one review cycle.
- **Estimated lines changed:** ~80 lines new code + ~120 lines tests.
- **Risk:** Low — changes are guarded by `filepath.IsAbs()` checks that are
  no-ops for all existing uses. The new code paths only fire for the new
  relative-path input.
