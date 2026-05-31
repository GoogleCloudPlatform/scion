# Grove Shared Directories

## Status
**Phase 2 Implemented** | March 2026

## Problem Statement

Currently, each scion agent operates in full isolation — its own home directory, its own git worktree workspace, and its own mounted volumes. There is no built-in mechanism for agents within a grove to share persistent, mutable state via the filesystem.

Common use cases that require shared directory access between agents:

- **Shared build caches**: Multiple agents working on the same project could benefit from a shared compilation cache (e.g., `GOCACHE`, `node_modules/.cache`, Bazel output base).
- **Shared artifacts**: One agent produces build artifacts or data files that another agent needs to consume.
- **Shared context / knowledge base**: A directory containing reference materials, design docs, or generated context that all agents should read (and optionally write to).
- **Coordination files**: Lock files, status markers, or message-passing files for lightweight inter-agent coordination.

Users can manually configure volume mounts in `settings.yaml` to achieve this, but the approach requires:
1. Manually managing host-side directories
2. Coordinating mount targets across agent configurations
3. No grove-level abstraction — each agent's config must be updated individually

## Design Goals

1. **Grove-scoped**: Shared directories belong to a grove and are available to all agents within it.
2. **Named by slug**: Each shared directory is identified by a simple slug (e.g., `build-cache`, `artifacts`, `shared-context`).
3. **Leverage existing infrastructure**: Use the existing `VolumeMount` / `Volumes` config and runtime mount machinery. Shared dirs are internally represented as synthesized `VolumeMount` entries — no runtime code changes needed.
4. **Deterministic mount paths**: Agents should find shared directories at a well-known, predictable location.
5. **Runtime-portable**: Work across Docker, Podman, Apple container, and Kubernetes (with appropriate adaptation).
6. **Minimal configuration**: Declaring a shared directory at the grove level should be sufficient — agents should not need per-agent volume config.

## Proposed Data Model

### Settings Extension

Add a `shared_dirs` field to grove-level `settings.yaml`:

```yaml
# settings.yaml
shared_dirs:
  - name: build-cache
    read_only: false
  - name: shared-context
    read_only: true           # agents get read-only access by default
  - name: artifacts
  - name: workspace-cache
    in_workspace: true        # mount inside the workspace tree instead of /scion-volumes
```

### Go Types

```go
// SharedDir defines a grove-level shared directory available to all agents.
type SharedDir struct {
    Name        string `json:"name" yaml:"name"`                                   // Slug identifier (e.g., "build-cache")
    ReadOnly    bool   `json:"read_only,omitempty" yaml:"read_only,omitempty"`      // Default access mode
    InWorkspace bool   `json:"in_workspace,omitempty" yaml:"in_workspace,omitempty"` // Mount inside workspace instead of /scion-volumes
}
```

The `Name` field must be a valid slug: lowercase alphanumeric with hyphens, no spaces or special characters.

### Host-Side Storage

Shared directories are stored alongside agent homes in the grove's external config directory:

```
~/.scion/grove-configs/<slug>__<uuid>/
├── agents/
│   ├── agent-1/
│   │   └── home/
│   └── agent-2/
│       └── home/
└── shared-dirs/           # NEW
    ├── build-cache/       # One directory per declared shared dir
    ├── shared-context/
    ├── artifacts/
    └── workspace-cache/
```

This location is:
- Outside the git repository (no git interaction concerns)
- Alongside agent homes (consistent with existing external storage pattern)
- Per-grove (naturally scoped)
- Persistent across agent restarts and reprovisioning

For hub-managed groves, shared dirs live at `~/.scion/grove-configs/<hub-grove>/shared-dirs/<name>/` on each broker — the same grove-configs path used for agent homes. The `~/.scion/groves/<hub-grove>/` path is reserved for hub-managed workspaces, not configuration state.

## Mount Target Strategy

Each shared directory can be mounted in one of two locations, controlled by the `in_workspace` flag:

### Default: `/scion-volumes/<name>`

When `in_workspace` is false (the default), the shared directory is mounted under a dedicated root:

```
/scion-volumes/build-cache
/scion-volumes/shared-context
/scion-volumes/artifacts
```

**Pros:**
- Clean namespace, no collision with workspace or home
- Obvious and discoverable — agents can `ls /scion-volumes/` to see all available shared dirs
- No interaction with git (outside workspace and repo-root)
- No `.gitignore` concerns
- Consistent across all runtimes
- Extensible — `/scion-volumes/` could host other scion-managed mounts in the future

**Cons:**
- Requires agents/tasks to reference a non-standard path
- Not in the workspace, so tools that operate on workspace files won't naturally see shared dir contents

### Workspace Mount: `/workspace/.scion-volumes/<name>`

When `in_workspace: true`, the shared directory is mounted inside the workspace tree:

```
/workspace/.scion-volumes/build-cache
/workspace/.scion-volumes/workspace-cache
```

**Pros:**
- Visible to tools that operate on the workspace
- Feels "close" to the code being worked on
- Useful for caches that tools expect to find relative to the project root

**Cons:**
- **Git interaction**: The `.scion-volumes` directory will appear as untracked content in the git worktree. Users will likely want to add `.scion-volumes/` to their `.gitignore`.
- **Bind mount over existing dir**: If `.scion-volumes/` already exists in the repo, the mount shadows it
- **Agent confusion**: LLM agents may try to commit or reference `.scion-volumes` contents as part of the codebase
- **Workspace sync (K8s)**: Kubernetes runtime syncs workspace via tar — in-workspace shared dirs would need to be excluded from sync to avoid duplicating large caches

### Environment Variable

Inject `SCION_VOLUMES=/scion-volumes` into agent environment so agents and scripts can programmatically discover the shared directory root. In-workspace mounts are also discoverable at `$WORKSPACE/.scion-volumes/` but do not get a separate env var.

### Alternatives Considered for Mount Paths

**Under Home Directory (`/home/<user>/shared/<name>`)**: Rejected because home is per-agent, creating a confusing ownership model. Path varies by runtime user (`/home/scion/` vs `/home/gemini/`). Less discoverable.

**Configurable target per dir**: Rejected in favor of the simpler two-mode approach (`in_workspace` toggle). Arbitrary target paths make it harder to reason about where shared dirs live and create inconsistency across agents.

## Implementation Approach

### Phase 1: Core Implementation (Complete)


#### 1. Config Changes

Add `SharedDir` type to `pkg/api/types.go` and `SharedDirs []SharedDir` to the `Settings` struct in `pkg/config/settings.go`.

Add validation:
- `Name` must be a valid slug (`^[a-z0-9][a-z0-9-]*[a-z0-9]$`)
- No duplicate names within a grove

#### 2. Storage Provisioning

In `pkg/config/init.go` or a new `pkg/config/shared_dirs.go`:
- When shared dirs are defined in settings, ensure `~/.scion/grove-configs/<grove>/shared-dirs/<name>/` exists
- Create directories lazily on first agent start, or eagerly on `scion init` / settings change
- Implement `GetSharedDirPath(grovePath, name) string` helper

#### 3. Volume Injection

In `pkg/agent/run.go`, during `RunConfig` construction:
- Read `shared_dirs` from grove settings
- For each shared dir, synthesize a `VolumeMount` and append to `RunConfig.Volumes`:
  ```go
  target := fmt.Sprintf("/scion-volumes/%s", dir.Name)
  if dir.InWorkspace {
      target = fmt.Sprintf("/workspace/.scion-volumes/%s", dir.Name)
  }
  api.VolumeMount{
      Source:   sharedDirHostPath,
      Target:   target,
      ReadOnly: dir.ReadOnly,
  }
  ```

This reuses the existing `VolumeMount` type and the `buildCommonRunArgs()` → volume deduplication → bind mount pipeline with **zero changes to runtime code**.

#### 4. Environment Variable

Add `SCION_VOLUMES=/scion-volumes` to agent environment variables in the run config.

#### 5. CLI Commands

```bash
# List shared directories for current grove (or specified grove)
scion shared-dir list [--grove <grove>]

# Create a new shared directory
scion shared-dir create <name> [--grove <grove>] [--in-workspace] [--read-only]

# Remove a shared directory (with confirmation)
scion shared-dir remove <name> [--grove <grove>]

# Inspect a shared directory (show path, size, agents using it)
scion shared-dir info <name> [--grove <grove>]
```

The `--grove` flag allows operating on a specific grove when not running from within a grove context (consistent with other `scion` subcommands).

### Phase 2: Kubernetes Support (Complete)

For Kubernetes, local bind mounts are not supported. Shared directories use PersistentVolumeClaims (PVCs) as the backing mechanism.

**Approach: PersistentVolumeClaim (PVC)**

- When a grove has shared dirs and uses a Kubernetes runtime, a PVC is created per shared dir
- PVC access mode: `ReadWriteMany` (RWX) — requires a storage class that supports it (e.g., NFS, GCE Filestore, EFS)
- PVC names are deterministic and grove-scoped: `scion-shared-<grove>-<dir-name>`
- PVCs are created before the pod (in `Run()`) and reused across agents in the same grove
- PVCs are mounted at `/scion-volumes/<name>` (or `/workspace/.scion-volumes/<name>` for in-workspace dirs) in the pod spec
- Local bind-mount volumes with shared dir targets are silently skipped in the K8s runtime (no warning)
- `SharedDirs` are passed through `RunConfig` so the K8s runtime can create PVCs independently of the local volume mount synthesis

**Configuration:**
- `KubernetesConfig` gained two new fields: `SharedDirStorageClass` and `SharedDirSize`
- Storage class configurable via `settings.yaml`:
  ```yaml
  kubernetes:
    shared_dir_storage_class: "standard-rwx"
    shared_dir_size: "10Gi"    # default size per shared dir (default: 10Gi)
  ```

**PVC Lifecycle:**
- Created on first agent start in a grove that declares shared dirs (idempotent — existing PVCs are reused)
- PVCs persist across agent restarts (they are grove-scoped, not agent-scoped)
- Grove-scoped cleanup available via `cleanupSharedDirPVCs()` — called during grove deletion, not agent deletion
- PVCs are labeled with `scion.grove` and `scion.shared-dir` for lifecycle management

**Alternative: EmptyDir (Ephemeral)**

For cases where persistence across pod restarts is not required, an `EmptyDir` could be used. However, EmptyDir is per-pod and not shared across pods, making it unsuitable for multi-agent sharing in K8s. It would only work if all agents for a grove run in the same pod (not the current model).

### Phase 3: Hub Integration

For the hosted architecture:
- Hub API gains shared dir metadata as part of grove registration
- Runtime brokers provision shared dir storage based on hub grove config
- On each broker, shared dirs are stored at `~/.scion/grove-configs/<hub-grove>/shared-dirs/<name>/` — the same grove-configs directory used for agent homes and other grove configuration state
- Cross-broker sharing would require a network filesystem or object storage — out of scope for initial implementation

## Per-Agent Access Control

The default `read_only` flag on `SharedDir` sets the grove-wide default. Per-agent overrides could be supported via agent config or profiles:

```yaml
# In profile or agent template
shared_dir_overrides:
  - name: artifacts
    read_only: false     # This agent can write to artifacts
  - name: build-cache
    exclude: true        # This agent doesn't get build-cache mounted
```

This is a Phase 2+ concern and can be deferred.

## Concurrency and Safety

Shared directories introduce the possibility of concurrent writes from multiple agents. The design intentionally does **not** provide file-level locking or transactional semantics:

- **Filesystem-level guarantees**: POSIX semantics on the host filesystem apply (atomic rename, O_EXCL create, etc.)
- **User responsibility**: Agents/tasks that write to shared dirs must coordinate at the application level (e.g., using lock files, unique filenames, or atomic write patterns)
- **Read-only default**: The `read_only: true` default for new shared dirs encourages a single-writer pattern

## Alternatives Considered

### Alternative: Extend Existing Volume Config

Instead of a dedicated `shared_dirs` concept, users could be told to configure volumes manually in settings:

```yaml
volumes:
  - source: ~/.scion/grove-configs/my-project__abc123/custom-shared/
    target: /scion-volumes/my-data
```

**Why rejected as the user-facing interface:**
- Requires users to know internal grove paths
- No grove-level abstraction — each profile/template must repeat the config
- No lifecycle management (create/delete)
- Doesn't compose well with Kubernetes (local volumes not supported)

However, this *is* the internal representation — shared dirs are synthesized into `VolumeMount` entries before being passed to the runtime, reusing all existing mount machinery.

### Alternative: Symlink-Based Sharing

Create symlinks inside agent workspaces pointing to a shared host directory.

**Why rejected:**
- Symlinks don't cross container mount boundaries
- Would require the shared directory to be mounted anyway
- Adds complexity without solving the core problem

### Alternative: Named Docker Volumes

Use Docker named volumes instead of bind mounts for shared dirs.

**Why rejected:**
- Docker named volumes are managed by Docker, not scion — harder to inspect/backup
- Not portable to non-Docker runtimes without adaptation
- Bind mounts are more transparent and consistent with existing scion patterns

## Resolved Design Decisions

The following questions were raised during review and have been resolved:

### Auto-mount vs opt-in per agent

**Decision: Auto-mount.** Shared dirs are automatically mounted to all agents in a grove. This matches the "grove-scoped" model and keeps configuration simple. Per-agent overrides (including `exclude: true`) are available via agent config or profiles — see [Per-Agent Access Control](#per-agent-access-control).

### Naming

**Decision: `shared_dirs`.** This emphasizes the sharing aspect between agents. Alternatives considered were `grove_dirs` (emphasizes scope) and `shared_volumes` (could be confused with the existing `volumes` config).

### Permissions and ownership

**Decision: No additional mechanism needed.** Shared directories are created by the same process that provisions agent home directories. The existing agent-side UID/GID mapping (via `SCION_HOST_UID`/`SCION_HOST_GID`) applies to shared dirs with no changes required.

### Gitignore management for in-workspace mounts

**Decision: Documentation only.** When `in_workspace: true` is used, `.scion-volumes/` will appear as untracked content in the git worktree. Users should add `.scion-volumes/` to their `.gitignore` manually. Scion will not auto-modify `.gitignore` as this changes repo state which may be undesirable.

### Interaction with grove cloning / duplication

**Decision: Clean slate.** When a grove is cloned or duplicated, shared dir *names* are copied to the new grove's configuration but the directories start empty. Contents are not copied (expensive for large caches) and host dirs are not shared across groves (surprising and potentially dangerous).

### Lifecycle of shared dirs relative to agents

**Decision: Grove-scoped lifecycle.** Shared dirs persist when all agents in a grove are deleted — they are grove-scoped, not agent-scoped. They can be individually removed via `scion shared-dir remove`. When a grove itself is deleted or pruned, all of its shared dirs are deleted as well.

### Scope of shared dirs in hosted architecture

**Decision: Broker-scoped only (for now).** Shared dirs are scoped to a single broker. Cross-broker sharing (via GCS or other object storage) is a potential future improvement but is out of scope for the initial implementation.

## Future Considerations

- **Size limits and quotas**: Adding configurable size limits for shared dirs would help prevent runaway cache growth. This could be enforced via periodic `du` checks or a `max_size` field on `SharedDir`. Note that for local bind mounts on runtimes like Docker, native quota enforcement may not be available — enforcement would need to be application-level. Deferred to a future phase.
- **GCS-backed shared dirs**: The existing `gcs` volume type in `VolumeMount` already supports GCS buckets via gcsfuse. A `type: gcs` field on `SharedDir` with `bucket` and `prefix` fields could be a natural extension, and would partially address cross-broker sharing.
- **Snapshot and backup**: If snapshot/backup support is added to scion, shared dirs should be excluded by default (to avoid bloating snapshots with large caches), with an opt-in flag to include them.

## References

- [Grove Mount Protection](grove-mount-protection.md) — Related: agent isolation and mount security
- [GCS Volume Support](initial-gcs-volume-support.md) — Prior art: volume type extension
- [Agent Config Flow](agent-config-flow.md) — How agent configuration is resolved and merged
- `pkg/api/types.go` — `VolumeMount` struct definition
- `pkg/runtime/common.go` — `buildCommonRunArgs()` volume mounting logic
- `pkg/config/settings.go` — Settings struct and volume expansion
- `pkg/agent/run.go` — RunConfig construction and volume injection
