# Git-Workspace Hybrid Groves

**Created:** 2026-03-25
**Status:** Draft / Proposal
**Related:** `hosted/git-groves.md`, `hosted/sync-design.md`, `hosted/git-ws.md`, `grove-dirs.md`

---

## 1. Overview

### 1.1 Problem Statement

Today, Hub-created groves come in two flavors:

| Type | Workspace Strategy | Agent Isolation |
|------|-------------------|-----------------|
| **Git-based** (`gitRemote` set) | Each agent clones the repo independently into its container. Workspace is ephemeral — lost on container deletion. | Full isolation: each agent has its own `.git`, branch, and working tree. |
| **Hub-managed** (no `gitRemote`) | A single shared workspace at `~/.scion/groves/<slug>/` is mounted into all agents. | No isolation: agents share the same files. Concurrent writes can conflict. |

Both models have significant limitations for a common use case: **teams that want a git-backed project but prefer a shared, persistent workspace** on the Hub rather than ephemeral per-agent clones.

**Pain points with the current git-based model:**

1. **Every agent clones independently** — wasteful for large repositories, especially with multiple agents on the same grove.
2. **Workspace is ephemeral** — if a container is deleted (not just stopped), all uncommitted work is lost. Users must remember to push.
3. **No shared state** — agents cannot see each other's file changes unless they push to the remote and pull.
4. **Clone latency** — each agent start incurs clone time, especially for large repos.

**Pain points with the current hub-managed model:**

1. **No git integration** — no ability to commit, push, create branches, or open PRs.
2. **No source of truth** — the workspace is a bare directory with no version history.
3. **No reproducibility** — if the workspace is lost, there's no way to restore it from a remote.

### 1.2 Proposal

Introduce a **git-workspace hybrid** grove mode: a git grove that provisions a **single shared git clone** into the hub-managed workspace path instead of per-agent clones. Agents mount this shared workspace, just as hub-managed groves work today.

This is offered as a **sub-mode of git-based groves** — the existing per-agent clone behavior is retained as the default. Users choose the workspace mode when creating a git grove.

This combines the benefits of both models:
- Git integration (branches, commits, push/pull, PRs)
- Shared persistent workspace (survives agent deletion)
- Single clone (no per-agent duplication)
- Hub-managed lifecycle

### 1.3 Goals

1. Allow creating a git grove that provisions a shared, persistent workspace.
2. The workspace is a real git clone — agents can commit, branch, push, and pull.
3. Only one clone operation per grove (not per agent).
4. Agents share the workspace filesystem, similar to hub-managed groves.
5. Support private repositories via `GITHUB_TOKEN` or GitHub App credentials.

### 1.4 Non-Goals

- Per-agent branch isolation within the shared workspace (agents must coordinate).
- Automatic merge/conflict resolution between agents.
- Multi-broker workspace replication (deferred — one broker at a time initially).
- SSH key-based git auth (HTTPS + token only in this phase).
- Submodule support.

---

## 2. Design Alternatives Considered

### 2.1 Alternative A: Shared Bare Clone + Per-Agent Worktrees

**Concept:** Clone the repo once as a bare repository on the broker. Each agent gets a git worktree from the shared bare clone, providing per-agent isolation with shared object storage.

```
~/.scion/groves/<slug>/
├── repo.git/           # Shared bare clone (object store)
├── worktrees/
│   ├── agent-alpha/    # git worktree for agent-alpha
│   └── agent-beta/     # git worktree for agent-beta
```

**Pros:**
- Per-agent branch isolation (each agent works on its own branch).
- Disk-efficient: objects are shared, only working trees are duplicated.
- Familiar pattern — matches current local worktree behavior.

**Cons:**
- Agents still can't see each other's uncommitted changes (separate working trees).
- Worktree creation adds startup latency (though less than a full clone).
- More complex lifecycle management (worktree cleanup, branch tracking).
- Doesn't match the hub-managed "shared workspace" mental model that users expect.

**Verdict:** This is a valid future extension that would leverage the existing local-mode worktree patterns, but it doesn't fulfill the "shared workspace" goal for this scope. It's essentially an optimization of the current git-based model. Marked as a potential future option that could be offered alongside the shared workspace mode.

### 2.2 Alternative B: Clone Once, Mount Shared (Proposed)

**Concept:** Clone the repo once into the hub-managed workspace path. All agents mount the same directory. The workspace is a normal git working tree (not bare).

```
~/.scion/groves/<slug>/
├── .git/               # Full git clone
├── src/
├── README.md
└── ...
```

**Pros:**
- Simple mental model: one workspace, multiple agents.
- Agents can see each other's file changes in real-time.
- Full git operations available (commit, push, pull, branch).
- Minimal lifecycle complexity — it's just a directory.
- Matches hub-managed behavior exactly (just with git pre-initialized).

**Cons:**
- No isolation: concurrent agent writes can conflict.
- Single branch at a time (or agents must coordinate git operations).
- Potential for `.git` lock contention with concurrent git operations.

**Verdict:** This is the simplest approach and matches the user's stated desire for a "shared workspace with git". The lack of isolation is a known trade-off that users accept when choosing hub-managed groves today.

### 2.3 Alternative C: Git Clone + GCS Sync Hybrid

**Concept:** Clone the repo once into GCS storage. Sync to broker workspace via the existing GCS signed-URL mechanism. Agents mount the synced copy.

**Pros:**
- Multi-broker support (any broker can sync from GCS).
- Leverages existing sync infrastructure.

**Cons:**
- No live git operations inside the container (synced copy has no `.git` context unless we sync that too).
- Requires additional sync steps on agent start.
- Git state in GCS is awkward (`.git` directory is large and changes frequently).
- Adds latency and complexity for what should be a simple clone.

**Verdict:** Over-engineered for the initial use case. GCS sync is designed for non-git workspaces. Using it for git repos fights against its design.

### 2.4 Alternative D: In-Container Clone per Agent (Current git-grove behavior)

**Concept:** Keep the current model where each agent clones independently.

**Pros:**
- Already implemented and working.
- Full per-agent isolation.

**Cons:**
- Doesn't address any of the pain points listed in Section 1.1.
- Redundant clones, ephemeral workspaces, no shared state.

**Verdict:** This remains the default behavior for git groves. It is **not being replaced** — users will choose between this mode and the new shared workspace mode when creating a git grove via the grove creation form (see Section 5.1).

### 2.5 Decision

**Alternative B (Clone Once, Mount Shared)** is the proposed approach, offered as a sub-mode of git groves alongside the existing per-agent clone mode (Alternative D). It provides the best balance of simplicity, git integration, and shared workspace behavior. The isolation trade-offs are acceptable because:

1. Users choosing "shared workspace" mode are explicitly opting into shared state.
2. The same trade-off already exists for hub-managed groves.
3. Per-agent isolation can be layered on later (Alternative A) as a separate grove option.

---

## 3. Detailed Design

### 3.1 Grove Type Modeling

The hybrid is modeled as a **sub-type of git grove** via a label, not a new top-level type. A git URI as a grove identifier is central to the type and model across the system, and the hybrid is fundamentally a git grove with a different workspace strategy.

- **Data model:** A grove with `gitRemote` set AND `scion.dev/workspace-mode: shared`.
- **GroveType stays `git`** — no new top-level type. Code that checks `GroveType == "git"` continues to work.
- **Sub-type differentiation** uses the label only where behavior diverges (clone vs. mount, worktree creation, workspace file management).

```go
// Check workspace mode where behavior differs:
func (g *Grove) IsSharedWorkspace() bool {
    return g.GitRemote != "" && g.Labels["scion.dev/workspace-mode"] == "shared"
}
```

**Rationale:**
- Git URI remains the primary type discriminator.
- Existing code that handles git groves (remote display, credential provisioning, branch UI) works without modification.
- Fewer places to update — only the divergent paths need to check the sub-type.
- Cleaner extension path — future sub-modes (e.g., per-agent worktrees from Alternative A) add more label values, not more top-level types.

### 3.2 Bootstrap: Host-Side Cloning

The clone operation is performed **directly by the Hub/broker process** on the host, rather than via a bootstrap container. The Hub already has access to the token infrastructure it uses to provision tokens to containers, and host-side cloning offers significant advantages in speed, simplicity, and error handling.

#### 3.2.1 Bootstrap Flow

```
Web UI / CLI                Hub/Broker                 Host Filesystem
   |                         |                          |
   |-- create grove -------->|                          |
   |   (gitRemote, mode:     |                          |
   |    shared)               |                          |
   |                         |                          |
   |                         |-- mkdir grove dir ------>|
   |                         |   (~/.scion/groves/slug) |
   |                         |                          |
   |                         |-- git clone ------------>|
   |                         |   (using GITHUB_TOKEN    |
   |                         |    or App credentials)   |
   |                         |                          |
   |                         |   [on failure: rm dir,   |
   |                         |    return creation error] |
   |                         |                          |
   |<-- grove ready ---------|                          |
   |   (or creation error)   |                          |
```

#### 3.2.2 Clone Operation

The Hub/broker performs the clone directly:

```go
// Conceptual host-side clone:
func cloneSharedWorkspace(grovePath, cloneURL, branch, token string) error {
    // Build authenticated URL
    authURL := fmt.Sprintf("https://oauth2:%s@%s/%s.git", token, host, path)

    // Clone into grove workspace directory
    cmd := exec.Command("git", "clone", "--branch", branch, authURL, grovePath)
    if err := cmd.Run(); err != nil {
        // Cleanup on failure — return to pre-creation state
        os.RemoveAll(grovePath)
        return fmt.Errorf("clone failed: %w", err)
    }

    // Configure git identity
    gitConfig(grovePath, "user.name", "Scion")
    gitConfig(grovePath, "user.email", "agent@scion.dev")

    return nil
}
```

#### 3.2.3 Error Handling

If the clone fails (bad URL, expired token, network error):

- The workspace directory is cleaned up (removed).
- The grove creation request fails with a descriptive error.
- The system returns to the state before grove creation was attempted — no partial grove records.
- The error response includes actionable guidance (e.g., "Check GITHUB_TOKEN", "Verify repository URL").
- The user retries by simply re-submitting the grove creation form.

This is simpler than maintaining a grove status state machine for provisioning — a failed clone is a failed creation, not a grove in an error state.

### 3.3 Agent Workspace Mounting

Once the shared workspace is cloned, agents mount it identically to hub-managed groves:

```go
// In agent provisioning (pkg/agent/provision.go):
if grove.IsSharedWorkspace() {
    // Mount the shared workspace — same as hub-managed
    workspaceSource = hubNativeGrovePath(grove.Slug)
    // Skip worktree creation — workspace already exists
    shouldCreateWorktree = false
}
```

The container sees `/workspace` with a full git clone. Agents can:
- Read and modify files (shared with all other agents on this grove)
- Run `git status`, `git diff`, `git log`
- Commit and push changes
- Create and switch branches (with coordination — only one branch checked out at a time)

### 3.4 Git Credential Management

#### 3.4.1 Per-Agent Credential Helper

Git credentials are configured **per agent in the agent's home directory**, not in the shared workspace. This keeps the workspace clean and mirrors the approach used for agents in standard (clone-based) git groves.

Each agent's container is provisioned with a credential helper in `$HOME/.gitconfig` (or `$HOME/.config/git/config`):

```bash
# In agent home directory (outside workspace):
[credential]
    helper = !f() { echo "username=oauth2"; echo "password=${GITHUB_TOKEN}"; }; f
```

This approach:
- Keeps credentials out of the shared workspace entirely (no `.scion/git-credentials` file).
- Uses the same pattern as clone-based agents — one credential mechanism for all git grove agents.
- Each agent receives its token via `GITHUB_TOKEN` environment variable (existing infrastructure).
- Token rotation is handled at the agent/container level, not the workspace level.

#### 3.4.2 Universal Credential Helper Pattern

The credential helper approach should be standardized across **all** agent types that work with a git remote, whether they clone into an agent-specific workspace or operate on a shared clone:

| Agent Type | Workspace | Credential Location |
|------------|-----------|-------------------|
| Clone-based (per-agent) | Agent-specific clone | `$HOME/.gitconfig` in container |
| Shared workspace (hybrid) | Shared grove clone | `$HOME/.gitconfig` in container |

This ensures a consistent pattern and avoids any credentials leaking into committed files.

#### 3.4.3 Token Refresh

Token refresh conforms with whatever approach is currently used for agents in standard git-based groves. No new token management infrastructure is introduced for the hybrid mode — the same `GITHUB_TOKEN` injection and refresh mechanisms apply.

### 3.5 Hub-Side Changes

#### 3.5.1 Grove Creation Handler

The existing `POST /api/v1/groves` handler needs to:

1. Accept a new `workspaceMode` field (or detect it from labels).
2. When `workspaceMode: "shared"` and `gitRemote` is set:
   a. Create the hub-managed workspace directory (`~/.scion/groves/<slug>/`).
   b. Perform the host-side git clone into the workspace directory.
   c. On clone success: create the grove record.
   d. On clone failure: clean up the workspace directory and return an error.
3. The grove is either `ready` on creation or the creation fails — no intermediate provisioning states.

#### 3.5.2 API Request Extension

```json
// POST /api/v1/groves
{
  "name": "my-project",
  "slug": "my-project",
  "gitRemote": "https://github.com/org/repo.git",
  "workspaceMode": "shared",
  "labels": {
    "scion.dev/default-branch": "main"
  }
}
```

#### 3.5.3 Grove Status

Since the clone is synchronous with grove creation, the existing grove status model does not require new provisioning states. A grove either exists (clone succeeded) or creation failed (no grove record). This simplifies both the API and the UI — there is no "provisioning" state to poll for.

### 3.6 Agent Provisioning Changes

When an agent is started on a shared-workspace git grove:

1. **Skip git clone** — the workspace already contains a clone.
2. **Skip worktree creation** — agents share the workspace directly.
3. **Mount shared workspace** — same bind mount as hub-managed groves.
4. **Configure credential helper** — per-agent `$HOME/.gitconfig` with `GITHUB_TOKEN`.
5. **Branch field behavior** — the agent creation form preserves the branch name field. For shared-workspace groves, the default value is the workspace's current branch (instead of an agent-named branch as used for clone-based agents). Agents can change branches but must coordinate since only one branch can be checked out at a time.

```go
// Decision logic in ProvisionAgent():
switch {
case opts.GitClone != nil:
    // Standard git grove: clone inside container
    // (existing behavior)

case grove.IsSharedWorkspace():
    // Hybrid: mount shared workspace, no clone, no worktree
    workspaceSource = hubNativeGrovePath(grove.Slug)
    shouldCreateWorktree = false

case isGit && noExplicitWorkspace:
    // Local git grove: create worktree
    // (existing behavior)

default:
    // Hub-managed: mount shared workspace
    // (existing behavior)
}
```

### 3.7 Workspace File Management

The existing `handleGroveWorkspace` handler (`grove_workspace_handlers.go:84-86`) currently rejects file management for groves with `gitRemote` set:

```go
if grove.GitRemote != "" {
    Conflict(w, "Workspace file management is only available for hub-managed groves")
    return
}
```

For shared-workspace groves, this check should be relaxed:

```go
if grove.GitRemote != "" && !grove.IsSharedWorkspace() {
    Conflict(w, "Workspace file management is only available for hub-managed and git-shared groves")
    return
}
```

---

## 4. Open Questions

### 4.1 Concurrency and Coordination

**Q: How should agents coordinate when sharing a git workspace?**

With a shared workspace, two agents running concurrently could:
- Edit the same file simultaneously
- Run `git checkout` to different branches
- Run conflicting git operations (e.g., concurrent commits)

**Resolution:** No formal coordination mechanism. This is up to the grove owner and users to manage — the same as hub-managed groves today. Document the shared workspace semantics clearly and let users decide coordination strategies via agent instructions.

### 4.2 Branch Management

**Q: Should agents create their own branches in the shared workspace?**

In the current git-based model, each agent gets its own branch (`scion/<agent-name>`). In the shared workspace model, only one branch can be checked out at a time.

**Resolution:** The branch name field on the new-agent form is preserved. For shared-workspace groves, the default value is the current workspace branch (e.g., "main") instead of an agent-named branch (which remains the default for clone-based agents). Power users can use git worktrees within the workspace if needed for parallel branch work.

### 4.3 Multi-Broker Support

**Q: How does this work with multiple brokers?**

**Resolution:** Single broker only. Shared-workspace groves are restricted to a single broker. Multi-broker support requires broader design work and is deferred.

### 4.4 `.scion` Directory in Cloned Workspace

**Q: What happens if the cloned repo already has a `.scion/` directory?**

The cloned repo might contain its own `.scion/` project configuration. The bootstrap should:
- Preserve the repo's `.scion/` directory (it's part of the source code).
- Ensure the Hub's grove settings (in the grove DB record) take precedence over any settings in the cloned `.scion/`.

No credential files are stored in the workspace (see Section 3.4), so there is no `.gitignore` management needed for credentials.

### 4.5 Workspace Size and Cleanup

**Q: How are large cloned workspaces managed?**

**Resolution:** This is a user responsibility. Shallow clones (`--depth 1`) can be offered as an option on the creation form. Workspace growth from commits and fetches is left to users to manage (e.g., `git gc`).

### 4.6 Pulling Updates from Remote

**Q: How do agents get upstream changes?**

**Resolution:** Manual pull. Agents run `git pull` themselves. Automated pull is a future enhancement that requires conflict resolution design.

---

## 5. Web UI Changes

### 5.1 Grove Creation Form

The grove creation flow uses a two-step type selection:

1. **Primary type choice:** `Hub-managed Workspace` or `Git Repository`
2. **Git mode choice (shown when Git Repository is selected):**
   - `Per-agent clone` (default) — each agent gets its own clone (existing behavior).
   - `Shared workspace` — a single shared clone mounted by all agents (new).

```typescript
// Primary type
type GroveType = 'hub' | 'git';

// Git sub-mode (only relevant when GroveType is 'git')
type GitWorkspaceMode = 'per-agent' | 'shared';
```

```html
<!-- Primary type selector -->
<sl-option value="hub">Hub-managed Workspace</sl-option>
<sl-option value="git">Git Repository</sl-option>

<!-- Sub-mode selector (visible when git is selected) -->
<sl-radio-group label="Workspace Mode">
  <sl-radio value="per-agent">Per-agent clone (each agent gets its own copy)</sl-radio>
  <sl-radio value="shared">Shared workspace (single clone, all agents share)</sl-radio>
</sl-radio-group>
```

When `shared` is selected:
- Show the Git Remote URL field (same as per-agent mode).
- Show the Default Branch field.
- Add a note: "A single git clone will be created and shared by all agents."

### 5.2 Grove Detail Page

For shared-workspace git groves, the grove detail page should show:
- Git remote URL (linked to GitHub).
- Current branch checked out.
- File browser (reuse hub-managed workspace file listing).
- A "Pull Latest" action button.

### 5.3 Agent Creation Form

The branch name field on the new-agent form adapts its default based on workspace mode:

| Git Workspace Mode | Default Branch Value |
|-------------------|---------------------|
| Per-agent clone | `scion/<agent-name>` (existing behavior) |
| Shared workspace | Current workspace branch (e.g., `main`) |

---

## 6. Implementation Plan

### Phase 1: Data Model & API (Foundation) ✅ Completed

1. ✅ Add `workspaceMode` field to `CreateGroveRequest` and grove labels.
2. ✅ Add `IsSharedWorkspace()` helper to grove model.
3. ✅ Update `handleGroveWorkspace` to allow file operations on shared-workspace groves.
4. ✅ Update `POST /api/v1/groves` handler to accept and store hybrid grove configuration.

### Phase 2: Host-Side Clone Infrastructure ✅ Completed

1. ✅ Implement host-side git clone in the grove creation handler:
   - ✅ Create workspace directory.
   - ✅ Run `git clone` with token auth (GitHub App or grove secrets).
   - ✅ Configure git identity (`Scion` / `agent@scion.dev`).
   - ✅ On failure: clean up directory and grove record, fail creation.
   - ✅ Sanitize credentials from remote URL and error messages.
2. ✅ Git is invoked via `exec.Command` — requires git on Hub/broker hosts (prerequisite).

### Phase 3: Agent Provisioning Integration ✅ Completed

1. ✅ Update `ProvisionAgent()` to detect shared-workspace groves and skip worktree/clone.
   - ✅ `SharedWorkspace` flag threaded via context from hub → dispatcher → broker → provisioning.
   - ✅ Hub dispatcher resolves `grove.IsSharedWorkspace()` and propagates to broker.
   - ✅ Broker injects `SCION_SHARED_WORKSPACE` env var for sciontool.
2. ✅ Mount shared workspace path for agents on shared-workspace groves.
   - ✅ Hub sets workspace to hub-managed grove path; broker passes through.
   - ✅ ProvisionAgent takes explicit workspace path (skips worktree creation).
3. ✅ Configure per-agent credential helper in `$HOME/.gitconfig` with `GITHUB_TOKEN`.
   - ✅ ProvisionAgent writes credential helper to agent home `.gitconfig`.
   - ✅ sciontool `configureSharedWorkspaceGit()` handles in-container setup with GitHub App support.
4. ✅ Update default branch name logic for new-agent form.
   - ✅ Shared-workspace agents default to grove's `scion.dev/default-branch` label (or "main").
5. ✅ Test concurrent agent access to shared workspace.
   - ✅ Documented as user responsibility (same as hub-managed groves).

### Phase 4: Web UI ✅ Completed

1. ✅ Add git workspace mode sub-selector to grove creation form.
   - ✅ Radio button group (per-agent clone / shared workspace) shown when Git Repository is selected.
   - ✅ Mode-specific hints and informational note for shared workspace mode.
   - ✅ `workspaceMode` and `scion.dev/workspace-mode` label sent in API request.
2. ✅ Enable workspace file browser for shared-workspace groves.
   - ✅ `shouldShowFilesSection()` and `getFileTabs()` updated to include workspace tab for shared-workspace groves.
   - ✅ File loading on page load triggers for shared-workspace groves (same as hub-managed).
3. ✅ Add "Pull Latest" action to grove detail page.
   - ✅ Backend `POST /api/v1/groves/{id}/workspace/pull` endpoint with `git pull --ff-only`.
   - ✅ `PullSharedWorkspace` utility function in `pkg/util/git.go` with token auth and credential sanitization.
   - ✅ "Pull Latest" button in grove header actions (visible for shared-workspace groves with update capability).
   - ✅ Pull result displayed via `sl-alert` (success/error) with dismiss support.
   - ✅ File list auto-refreshes after successful pull.
4. ✅ Adapt agent creation form branch default based on workspace mode.
   - ✅ Shared-workspace groves: placeholder shows grove's default branch (e.g., "main").
   - ✅ Per-agent clone groves: placeholder shows "defaults to agent name" (existing behavior).
   - ✅ Hint text adapts to explain shared branch semantics.
5. ✅ Add `isSharedWorkspace()` helper to shared TypeScript types.

### Phase 5: Credential Standardization & Polish ✅ Completed

1. ✅ Standardize credential helper pattern across all git grove agent types (per-agent `$HOME`-based).
   - ✅ Clone-based agents now write credential helper to `$HOME/.gitconfig` (not workspace `.git/config`).
   - ✅ Both agent types use `sciontool credential-helper` for GitHub App token refresh.
   - ✅ Consistent credential helper pattern: `$HOME/.gitconfig` for all git grove agents.
2. ✅ Error handling and user guidance for common failure modes.
   - ✅ `GitError` type with classified kinds: `GitErrAuth`, `GitErrNotFound`, `GitErrNetwork`, `GitErrNonFastForward`.
   - ✅ `ClassifyGitError()` parses git stderr to categorize failures.
   - ✅ `UserGuidance()` returns actionable hints for each error kind.
   - ✅ Clone/pull errors return structured guidance in API `error.details.guidance`.
   - ✅ Grove creation returns HTTP 422 for auth/not-found errors (instead of generic 500).
   - ✅ Pull endpoint returns 401 for auth errors, 502 for network errors.
   - ✅ `ErrCodeCloneFailed` and `ErrCodePullFailed` error code constants.
   - ✅ Web UI `extractApiError()` includes guidance hints from structured error responses.
3. ✅ Documentation and template updates.
   - ✅ Code comments updated throughout to document standardized credential patterns.
   - ✅ Provision-time credential setup documented as pre-configuration for sciontool init.

### Future Phases (Deferred)

- **Per-agent worktrees within shared clone** — Alternative A as an opt-in sub-mode.
- **Multi-broker workspace sync** — GCS-based replication of the shared workspace.
- **Automated upstream pull** — scheduled `git pull` with conflict detection.
- **Pre-commit hooks** — block accidental credential commits.

---

## 7. Decisions Record

| # | Question | Status | Decision |
|---|----------|--------|----------|
| 1 | **Workspace model?** | Resolved | Shared workspace (Alternative B). Simplest model, matches hub-managed mental model. |
| 2 | **Retain per-agent clone?** | Resolved | Yes. Per-agent clone remains the default. Shared workspace is a sub-mode choice. |
| 3 | **Type modeling?** | Resolved | Sub-type of git grove via `scion.dev/workspace-mode` label. GroveType remains `"git"`. See Section 3.1 for rationale. |
| 4 | **Clone mechanism?** | Resolved | Host-side clone by Hub/broker. Simpler, faster, better error handling than bootstrap container. |
| 5 | **Credential storage?** | Resolved | Per-agent `$HOME/.gitconfig` credential helper using `GITHUB_TOKEN`. No credentials in workspace. Same pattern as clone-based agents. |
| 6 | **Token refresh?** | Resolved | Conforms with existing git grove agent token management. No new infrastructure. |
| 7 | **Agent coordination?** | Resolved | No formal mechanism. Up to grove owner and users to manage. |
| 8 | **Multi-broker?** | Deferred | Single broker only. |
| 9 | **Branch management?** | Resolved | Branch field preserved. Default is workspace branch for shared mode, agent-named for clone mode. |
| 10 | **Clone failure handling?** | Resolved | Clone failure = creation failure. Clean up, return to pre-creation state. |
| 11 | **Workspace size?** | Resolved | User responsibility. |
| 12 | **Upstream pull?** | Resolved | Manual only. |

---

## 8. References

### Design Documents

| Document | Relevance |
|----------|-----------|
| `hosted/git-groves.md` | Current git-based grove design (per-agent clone). This proposal extends it with a sub-mode. |
| `hosted/sync-design.md` | GCS workspace sync. Non-git bootstrap in Section 13. Informs Alternative C. |
| `hosted/git-ws.md` | Research on current git workspace state. Section 4 gaps are partially addressed here. |
| `hosted/hosted-architecture.md` | Overall hosted architecture. Grove identity model. |
| `grove-dirs.md` | Shared directories design. Shared workspace mounting patterns. |
| `kubernetes/scm.md` | K8s git clone design. Init container approach (related but different). |

### Source Files

| File | Relevance |
|------|-----------|
| `pkg/hub/handlers.go` | Grove creation handler. Hub-managed workspace init. |
| `pkg/hub/grove_workspace_handlers.go` | Workspace file management (currently rejects git groves). |
| `pkg/agent/provision.go` | Agent provisioning. Workspace resolution and worktree creation. |
| `pkg/runtime/common.go` | Container workspace mounting logic. |
| `cmd/sciontool/commands/init.go` | `sciontool init` — git clone phase, credential management. |
| `pkg/store/models.go` | Grove and agent data models. `GitCloneConfig`. |
| `web/src/components/pages/grove-create.ts` | Web UI grove creation form. |
