# Git Workspace Research: Current State in Hosted/Remote Architecture

**Created:** 2026-02-19
**Status:** Research / Working Document
**Purpose:** Catalogue how git repositories and workspaces are currently referenced, resolved, and managed across the hosted and remote runtime architecture, as groundwork for iterating on remote git-based workspace handling.

---

## 1. Git Remote as Grove Identity

The normalized git remote URL is the **primary identity mechanism** for groves in the hosted architecture. This is stated explicitly in `hosted-architecture.md`:

> A grove is uniquely identified by its Git remote URL.

### 1.1 Schema

The `git_remote` field is defined with a **unique constraint** at the database level:

- **Ent schema** (`pkg/ent/schema/grove.go:44-47`):
  ```go
  field.String("git_remote").
      Optional().
      Unique().
      Nillable(),
  ```

- **Domain model** (`pkg/store/models.go:122`):
  ```go
  GitRemote string `json:"gitRemote,omitempty"` // Normalized git remote URL (unique)
  ```

The field is optional and nillable because global groves (e.g. the user's home `~/.scion`) have no git remote.

### 1.2 Normalization

All git remote URLs pass through `NormalizeGitRemote()` (`pkg/util/git.go:379-403`) before storage or comparison. The function:

1. Lowercases the entire string
2. Strips protocol prefixes (`https://`, `http://`, `ssh://`, `git://`)
3. Converts SSH notation (`git@host:path` → `host/path`)
4. Removes `.git` suffix

Canonical form: `github.com/org/repo`

Examples:
- `https://github.com/org/repo.git` → `github.com/org/repo`
- `git@github.com:org/repo.git` → `github.com/org/repo`

### 1.3 Detection

`GetGitRemote()` / `GetGitRemoteDir()` (`pkg/util/git.go:326-341`) shell out to `git remote get-url origin` and return the raw URL (or empty string if not in a repo / no origin).

`ExtractRepoName()` (`pkg/util/git.go:345-371`) extracts the final path component (e.g. `repo`) from either SSH or HTTPS format, used to derive human-friendly grove names.

---

## 2. Grove Registration Flow

### 2.1 Hub Handler: `handleGroveRegister`

`POST /api/v1/groves/register` (`pkg/hub/handlers.go:1187-1278`)

Three-tier lookup for existing grove:

1. **By client-provided ID** — if `req.ID` is set, look up by UUID directly
2. **By normalized git remote** — `store.GetGroveByGitRemote(ctx, normalizedRemote)` (exact match)
3. **By slug** (case-insensitive) — only for groves without git remote (global groves)

If no existing grove is found, a new one is created with the normalized git remote set.

### 2.2 Client-Side Registration

**From `hubsync.registerGrove()`** (`pkg/hubsync/sync.go:812-838`):
- Calls `util.GetGitRemote()` to detect the origin URL
- Skips git remote for global groves (`isGlobal` flag)
- Normalizes via `util.NormalizeGitRemote()` before sending to Hub

**From `cmd/broker.go`** (line ~511):
- During broker registration, if a grove is linked and not global, sends `util.NormalizeGitRemote(util.GetGitRemote())` as the `GitRemote` field

### 2.3 Grove ID Resolution at CLI Time

`GetGroveID()` (`cmd/common.go:197-238`) resolves grove ID with priority:

1. `HubContext.GroveID` (set by `EnsureHubReady`)
2. Local `grove_id` from settings
3. **Git remote lookup via Hub API** — calls `List(ctx, &ListGrovesOptions{GitRemote: normalized})` and uses the first match

If no git origin is found, the error message directs the user to `scion hub link`.

### 2.4 Listing/Filtering

The Hub `GET /api/v1/groves` endpoint accepts a `gitRemote` query parameter. The value is normalized and used as either an exact match or prefix filter (`GitRemotePrefix` in the store layer — `pkg/hub/handlers.go:1098`).

---

## 3. Workspace Strategies by Runtime

### 3.1 Local Runtimes (Docker / Apple Containers)

**Git worktree approach** — `pkg/runtime/common.go:130-150`:

When both `config.RepoRoot` and `config.Workspace` are set:
1. Computes the relative path from repo root to workspace
2. Mounts the `.git` directory at `/repo-root/.git`
3. Mounts the worktree at `/repo-root/<relative-path>`
4. Sets `--workdir` to the container workspace path

This gives the agent a fully functional git environment with shared object store.

When only `config.Workspace` is set (no repo root):
- Mounts workspace at `/workspace` directly (no git context)

**Worktree lifecycle** — `pkg/util/git.go`:
- `CreateWorktree()` (line 161) — creates worktree with `--relative-paths` flag, creates new branch
- `RemoveWorktree()` (line 200) — removes directory, prunes worktree records, optionally deletes branch
- `FindWorktreeByBranchIn()` (line 408) — locates worktree by branch name
- Requires git 2.47.0+ for `--relative-paths` support (`CheckGitVersion()`)

### 3.2 Kubernetes Runtime

**K8s-specific logic** — `pkg/runtime/k8s_runtime.go:75-91`:

- If `config.Workspace` is empty, scans volumes for a `/workspace` target mount
- Persists workspace path in pod annotations (`scion.workspace`)
- No git worktree mounting — workspace is an opaque directory

**Proposed git clone approach** — `.design/kubernetes/scm.md`:

The design doc proposes using an init container with `git clone --depth=1` to populate an `emptyDir` volume:
- Init container uses `alpine/git` image
- Clones the repo at the specified branch
- Configures git user identity
- Main container starts with populated `/workspace`

Authentication options documented:
- GitHub App (production, token auto-rotation)
- Personal Access Token (developer testing)
- GKE Workload Identity + Secret Manager (cloud-native)
- SSH keys (generic)

**Status**: The init container approach is documented in the design doc but **not yet implemented** in `k8s_runtime.go`. The current K8s runtime does not provision git workspaces — it relies on external workspace population (volume mounts or sync).

### 3.3 Hosted/Non-Git Workspaces (GCS Sync)

For groves without local filesystem access (remote brokers, non-git groves), the workspace sync system uses cloud storage.

**`AgentAppliedConfig`** (`pkg/store/models.go:85,99-101`):
```go
Workspace            string `json:"workspace,omitempty"`            // Host path
WorkspaceStoragePath string `json:"workspaceStoragePath,omitempty"` // GCS path
```

**Sync operations** — `pkg/hub/workspace_handlers.go`:
- `sync-from` (lines 183-272): Broker uploads workspace to GCS, Hub generates signed download URLs for CLI
- `sync-to` (lines 281-359): Hub generates signed upload URLs for CLI, with incremental deduplication via content hashes
- `sync-to/finalize` (lines 368-496): Validates uploaded files, then either dispatches to broker (bootstrap) or tunnels apply request via control channel

**Storage path convention**: `groves/<groveId>/agents/<agentId>/files/...`

This system operates independently of git — it transfers raw workspace files via GCS signed URLs. There is no git awareness in the sync layer.

---

## 4. Key Observations and Open Questions

### 4.1 Current Gaps

1. **No git clone implementation in K8s runtime** — The `scm.md` design exists but `k8s_runtime.go` has no init container generation code.

2. **Git remote is optional but pivotal** — The entire hosted grove identity system depends on git remote, but the field is nullable. Non-git groves (global groves, manually created groves) fall back to slug-based matching, which is less robust.

3. **Workspace sync is git-unaware** — The GCS-based sync (`workspace_handlers.go`) transfers files without any git context. An agent on a remote broker that receives workspace via sync cannot perform git operations (commit, push, branch) unless git is separately initialized.

4. **No branch tracking in grove registration** — The grove registration flow captures only the repository URL, not the branch. Branch information is local to the agent/worktree setup.

5. **Credential management for remote git is unimplemented** — `scm.md` describes authentication strategies (GitHub App, PAT, SSH) but no code exists for injecting git credentials into K8s pods or remote broker agents.

### 4.2 Where Git Remote is Referenced

| Location | Purpose | File:Line |
|----------|---------|-----------|
| Grove schema (DB) | Unique identity constraint | `pkg/ent/schema/grove.go:44` |
| Grove domain model | Stored field | `pkg/store/models.go:122` |
| Hub grove register handler | Lookup & upsert key | `pkg/hub/handlers.go:1206,1226` |
| Hub grove list handler | Filter parameter | `pkg/hub/handlers.go:1098` |
| Hub client register request | API field | `pkg/hubclient/groves.go:95` |
| Hub client list options | Query filter | `pkg/hubclient/groves.go:78` |
| Hub client create request | Optional field | `pkg/hubclient/groves.go:124` |
| CLI GetGroveID() | Fallback lookup | `cmd/common.go:216-226` |
| HubSync registerGrove() | Registration payload | `pkg/hubsync/sync.go:820,832` |
| Broker registration | Grove provider linking | `cmd/broker.go:511` |
| Broker grove scanning | Auto-detect grove name | `cmd/broker.go:369-371,876-878,1053-1055` |
| URL normalization | Canonical form conversion | `pkg/util/git.go:379-403` |
| URL detection | Shell out to `git remote` | `pkg/util/git.go:326-341` |
| Repo name extraction | Human-friendly name from URL | `pkg/util/git.go:345-371` |

### 4.3 Workspace Mounting by Runtime

| Runtime | Workspace Source | Git Available | Mechanism |
|---------|-----------------|---------------|-----------|
| Docker (local) | Git worktree | Yes (`.git` mounted) | Bind mount of worktree + `.git` |
| Apple Container | Git worktree | Yes (`.git` mounted) | Bind mount of worktree + `.git` |
| Kubernetes | None / Volume | No (currently) | `emptyDir` or PVC; init container proposed |
| Remote Broker (hosted) | GCS sync | No | File transfer via signed URLs |

### 4.4 Design Documents Relevant to This Area

| Document | Focus |
|----------|-------|
| `.design/kubernetes/scm.md` | Git clone strategy for K8s init containers |
| `.design/hosted/sync-design.md` | GCS-based workspace sync (git-unaware) |
| `.design/hosted/hosted-architecture.md` | Grove identity via git remote |
| `.design/hosted/multi-broker.md` | Multi-broker grove provider model |

---

## 5. Implications for Iteration

When evolving git-based workspace handling on remote brokers, these are the key constraints and decisions that will need to be addressed:

1. **Git identity vs. file sync** — Currently the hosted sync layer (GCS) and the git identity layer (grove registration) are disconnected. A remote agent receives files but has no git history, branch context, or ability to commit/push.

2. **Init container vs. sidecar** — `scm.md` proposes init-container-based clone for K8s. For long-running agents that need fresh pulls or push capability, a sidecar or credential refresh mechanism may be needed.

3. **Branch routing** — When an agent starts on a remote broker, it needs to know which branch to work on. This information currently flows through `AgentAppliedConfig` and worktree creation locally, but has no equivalent path for remote execution.

4. **Credential injection** — Remote git operations require auth tokens. The design proposes Kubernetes Secrets + init containers, but no implementation exists. The hosted architecture needs a credential flow that works across Docker, K8s, and potentially other runtimes.

5. **Push-back path** — `scm.md` envisions agents pushing commits to the remote. This requires the remote workspace to be a proper git clone (not just synced files), with configured credentials and push permissions.
