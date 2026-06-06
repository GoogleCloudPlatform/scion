# Design: NFS-Coordinated Workspace Sharing Across Nodes

**Branch:** `postgres/wave-b-integration`
**Date:** 2026-06-02
**Author:** nfs-architect agent
**Status:** Design proposal — **all open questions (Q1–Q6) resolved with maintainer** (see §11)
**Vocabulary:** follows `GLOSSARY.md` (Runtime Broker, Project, workspace sharing modes)
**Reviewers:** @ptone
**Context:** Multi-node Scion (Postgres-backed Hub, brokers/agents spread across VMs and GKE/Cloud Run) needs a shared filesystem so an agent can reach its project workspace regardless of which node it lands on.

Inputs (verified against source):
`pkg/runtime/common.go`, `pkg/runtime/k8s_runtime.go`, `pkg/config/shared_dirs.go`,
`pkg/api/types.go`, `pkg/store/models.go`, `pkg/ent/schema/{project,agent}.go`,
`pkg/runtimebroker/{types,handlers,start_context}.go`, `pkg/agent/run.go`,
`scripts/starter-Hub/`, `pkg/gcp/storage.go`.

---

## 1. Problem statement

Workspace storage in Scion is **node-local** today. Two facts make that fatal once
agents can be scheduled across nodes:

1. **Docker/VM path.** The Runtime Broker computes a host path
   (`~/.scion/project-configs/<slug>__<uuid>/...` or a git checkout) and bind-mounts
   it into the container: `-v HOST:/workspace` (`pkg/runtime/common.go:181-241`). That
   host path only exists on the node where the Runtime Broker created it. A second agent for
   the same project, dispatched to a different VM, sees an empty disk.

2. **Kubernetes path.** The workspace volume is an **EmptyDir**
   (`pkg/runtime/k8s_runtime.go:1080-1087`); the Runtime Broker then copies files into the pod
   after start via `kubectl cp`, gated on a `/tmp/.scion-home-ready` marker
   (`k8s_runtime.go:317-350`). Contents live only inside that pod and die with it.
   There is no shared durable workspace at all.

Shared directories are the one place a cross-node primitive already exists: on K8s
they are **project-scoped `ReadWriteMany` PVCs** named `scion-shared-<project>-<name>`
(`k8s_runtime.go:657-751`), storage class from `KubernetesConfig.SharedDirStorageClass`.
On Docker they are plain host bind mounts (`pkg/config/shared_dirs.go`). So the RWX
PVC concept is proven; we extend the same idea to the **workspace** and give Docker an
equivalent via NFS.

**Goal:** a project's workspace (and shared dirs) live on a network filesystem
addressable from every node, so any agent — on any VM, GKE pod, or Cloud Run
instance — mounts the same bytes. The Hub coordinates *which* NFS path maps to *which*
project/agent and tells the runtime how to mount it.

### 1.1 Non-goals

- **Creating & permissioning the NFS store.** Operator/Terraform owns *creation and
  permissioning* of the NFS instance and its shares — this is the **only** thing that
  happens outside the Hub / Runtime Broker lifecycle (maintainer, Q1). Everything else, including
  **mounting** the share, is the Hub / Runtime Broker's job (§4.2). A single NFS instance may
  expose **multiple shares** and serve **multiple Hub instances within one project**.
- **A distributed POSIX lock manager.** We rely on NFS-native advisory locking plus
  Scion's existing per-agent state isolation; we do *not* build a lock service.
- **Replacing GCS-FUSE volumes.** `type: gcs` volumes (`common.go:142-163`,
  `k8s_runtime.go:1238-1275`) stay as-is; NFS is a parallel backend.
- **Auto-migration of existing node-local workspaces.** New backend applies to new
  projects/agents; migration is a separate effort (§10).

---

## 2. Current architecture (verified)

| Concern | Docker / VM | Kubernetes |
|---|---|---|
| Workspace storage | host bind mount `-v HOST:/workspace` (`common.go:185`) | EmptyDir + post-start `kubectl cp` (`k8s_runtime.go:1084`, `:317-350`) |
| Workspace host path | Runtime Broker-computed, node-local (`agent/run.go:755-780`, `start_context.go:92-110`) | n/a (synced in) |
| Container workspace path | `ResolveContainerWorkspace` → `/workspace` or `/repo-root/<rel>` (`common.go:52-69`) | same logic, `config.ContainerWorkspace` |
| Shared dirs | host bind mount under `project-configs/.../shared-dirs/<name>` (`shared_dirs.go:33-118`) | project-scoped RWX PVC `scion-shared-<project>-<name>` (`k8s_runtime.go:657-751`) |
| Volume types | `local`, `gcs` (`api/types.go:248-279`) | `gcs` (CSI), no local bind | 
| Container UID | host user `scion` | UID/GID 1000, `FSGroup=hostGID` (`k8s_runtime.go:1021-1033`) |
| Placement metadata | none on Agent; Runtime Broker decides at dispatch | none |
| Workspace entity | **none** — derived from `Project.GitRemote` + workspace sharing mode (today a 2-value label `scion.dev/workspace-mode` ∈ {`shared`,`per-agent`}, `store/models.go:177-184`; glossary's canonical 3 modes are the target — §3.1) | same |

Key data structures:

- `api.VolumeMount{Source,Target,ReadOnly,Type,Bucket,Prefix,Mode}` (`api/types.go:248-256`).
  `Validate()` only accepts `""|local|gcs` (`:264-276`).
- `api.SharedDir{Name,ReadOnly,InWorkspace}` (`api/types.go:205-210`); persisted as a
  JSON column on the **Project** Ent entity (`ent/schema/project.go:62-63`).
- `api.KubernetesConfig{... SharedDirStorageClass, SharedDirSize}` (`api/types.go:291-302`).
- `config.V1StorageConfig{Provider,Bucket,LocalPath}` — Hub *artifact* storage, not
  workspaces (`config/settings_v1.go:416-420`).
- `runtime.RunConfig{Workspace,RepoRoot,ContainerWorkspace,HomeDir,Volumes,SharedDirs,GitClone,...}`
  (`runtime/interface.go`) — the per-agent contract the Runtime Broker fills and the runtime consumes.
- `runtimebroker.CreateAgentConfig{Workspace,RepoRoot,HomeDir,Volumes,SharedDirs,GitClone}`
  — the Hub→Runtime Broker wire contract (`runtimebroker/types.go:369-411`).

The single most important leverage point: **the runtime already mounts whatever host
path / volume the Runtime Broker hands it.** If we make that host path land on an NFS mount
(Docker) or swap the EmptyDir for an NFS-backed volume (K8s), most of the machinery is
untouched. The design is therefore mostly about **path mapping, provisioning, and
config**, not about rewriting the mount code.

---

## 3. Core concept: a workspace storage backend

Introduce an explicit **workspace storage backend** selected by config, with three
values: `local` (today's behavior, default), `nfs` (this design), and — reserved —
`gcs` (FUSE, already exists for *volumes* but not for the primary workspace).

A backend answers three questions for any (project, agent):

1. **Resolve** — given Project ID / agent ID / workspace sharing mode, what is the
   storage location? For NFS this is a *server-relative export path*, computed
   **deterministically from IDs** (no new DB column required for resolution — any
   replica computes the same path):

   ```
   <export-root>/projects/<project-id>/workspace               # Shared-plain & Worktree-per-agent
   <export-root>/projects/<project-id>/shared-dirs/<name>      # shared directories
   ```

2. **Provision** — ensure the directory exists and, for git projects, is cloned/worktree'd
   (§7). Idempotent; guarded against concurrent first-access (§8.2).

3. **Realize** — emit the runtime-specific mount:
   - Docker: a bind mount whose `Source` is the host NFS mountpoint + relative path.
   - K8s: an NFS-backed volume (static PV+subPath or Filestore-CSI PVC) at the workspace path.
   - Cloud Run: an NFS volume in the service/job spec with `path = <export>/<relative>`.

The Hub owns resolution + the mount spec it sends; the **Runtime Broker/runtime owns
provisioning and the actual mount syscall** (it is the component with filesystem /
cluster access). This mirrors today's split (Hub computes `CreateAgentConfig`, the
Runtime Broker realizes it).

### 3.1 NFS applicability is driven by **workspace sharing mode** (maintainer, Q3)

Per the glossary (`GLOSSARY.md` → *Workspace sharing mode*) there is **one** universal
set of three modes; the backend is **not** a separate per-project toggle — the sharing
mode *is* the selector:

| Workspace sharing mode | What it means | Storage backend |
|---|---|---|
| **Shared-plain** | one workspace directory mounted into every agent, no per-agent isolation (plain/non-git projects) | **shared NFS workspace** |
| **Worktree-per-agent** | each agent gets its own git worktree over one shared checkout (one clone's history) | **shared NFS workspace** (the shared checkout + all worktrees live on it) |
| **Clone-per-agent** | each agent gets its own full git clone | **NOT NFS** — node-local disk (nothing is shared, so there is nothing to put on NFS) |

So: **NFS backs the workspace for both sharing modes that share anything**
(Shared-plain and Worktree-per-agent) and backs **shared directories always**.
**Clone-per-agent** is the sole case that stays on node-local storage — a deliberate
"throwaway isolated clone" path. The `backend` config value (§6.1) therefore really
answers "*is a shared NFS workspace available on this Hub?*"; whether a given agent uses
it follows mechanically from the project's sharing mode.

**Terminology note:** today the code carries only a two-value label
`scion.dev/workspace-mode ∈ {shared, per-agent}` (`store/models.go:177-184`). The
glossary's three canonical modes (Shared-plain / Worktree-per-agent / Clone-per-agent)
are the target vocabulary; aligning the label/enum to all three is a prerequisite
clean-up for this work (Worktree-per-agent is noted as "not yet on Hub-managed
projects").

---

## 4. Model A — VMs / Docker (host-level NFS, bind into container)

### 4.1 Topology

```
            ┌────────────── NFS server (Filestore / self-hosted) ─────────────┐
            │  export:  /scion-workspaces                                       │
            │    projects/<pid>/workspace                                       │
            │    projects/<pid>/shared-dirs/<name>                              │
            └───────▲───────────────────────────────▲──────────────────────────┘
                    │ mount (Runtime Broker, on startup) │
        ┌───────────┴──────────┐         ┌────────────┴─────────┐
        │ VM node-1            │         │ VM node-2            │
        │ /mnt/nfs/workspaces  │         │ /mnt/nfs/workspaces  │
        │  Runtime Broker + dockerd    │         │  Runtime Broker + dockerd    │
        │  agent ctr           │         │  agent ctr           │
        │   -v /mnt/nfs/.../ws │         │   -v /mnt/nfs/.../ws │
        │      :/workspace     │         │      :/workspace     │
        └──────────────────────┘         └──────────────────────┘
```

### 4.2 Host mount — owned by the Hub / Runtime Broker, idempotent on (re)start

**Decision (maintainer, Q1):** mounting is part of the **Hub / Runtime Broker service
lifecycle**, not an operator step. When a Runtime Broker comes online (cold start or restart)
it **ensures the configured share(s) are mounted** before accepting NFS-backed
dispatch; the operator only created+permissioned the store. This must be **idempotent
and restart-safe** — a Runtime Broker bouncing must reconcile, not double-mount or fail on an
already-present mount.

Mount reconciliation at Runtime Broker startup (and re-checked before each NFS dispatch):

```
for each configured share S needed on this node:
    target = <mount_root>/<share-id>           # stable, per-share path
    if not is_mountpoint(target):
        mkdir -p target
        mount -t nfs -o vers=4.1,hard,nconnect=4,_netdev S.server:S.export target
    else:
        verify it points at the expected server:export (else log + remount)
```

Implementation notes:
- A Runtime Broker may need **multiple shares mounted at once** (a single NFS instance can
  expose many shares, and one project may be served by multiple Hub instances). The
  mount layer is therefore a *set* of shares keyed by share-id, each at its own
  `<mount_root>/<share-id>`, not a single global `/mnt/nfs/workspaces`.
- Prefer a managed systemd `.mount`/`automount` unit *written and started by the
  Runtime Broker* over a raw `mount(8)` call, so the OS handles remount-on-reboot and the
  Runtime Broker's job is reconciliation, not lifecycle.
- Run inside the existing Runtime Broker bring-up path (alongside doctor/health checks). On
  mount failure, the Runtime Broker reports unhealthy for NFS-backed projects rather than
  silently falling back to local disk.
- Requires the Runtime Broker to have mount privilege (root or `CAP_SYS_ADMIN`/sudo for
  `mount`); call out in deployment docs.

### 4.3 Path computation change (the only real code change for Model A)

Today the Runtime Broker resolves the workspace/shared-dir host path under
`~/.scion/project-configs/...` (`pkg/config/shared_dirs.go:33-54`,
`runtimebroker/start_context.go:92-110`). With backend=`nfs`, that resolution is
redirected to the NFS mountpoint:

```
hostBase = <MountRoot>/<share-id>            # Runtime Broker ensures this is mounted (§4.2)
workspace host path = hostBase/projects/<pid>/workspace
shared dir host path = hostBase/projects/<pid>/shared-dirs/<name>
```

`SharedDirsToVolumeMounts` (`shared_dirs.go:90-118`) is unchanged in shape — it still
emits `VolumeMount{Source: hostPath, Target: /scion-volumes/<name>}`; only the base
path moves onto NFS. The container sees **no difference** — it is still a bind mount at
`/workspace` (or `/repo-root/<rel>`), and the existing repo-root tmpfs-shadow isolation
(`common.go:357-362`) continues to protect per-agent state.

### 4.4 Lifecycle

- **Create on first agent:** Runtime Broker `mkdir -p` + provision (clone) under the
  project dir if absent (§7). The shared NFS workspace is reused across agents in both
  Shared-plain and Worktree-per-agent modes (§3.1); a Worktree-per-agent agent adds its
  own worktree under the shared checkout.
- **Persist across agents:** the workspace survives agent deletion (it is project-scoped).
  A Worktree-per-agent agent's worktree is removed on that agent's deletion; the shared
  checkout persists.
- **Cleanup:** on project deletion the Hub instructs a Runtime Broker to `rm -rf` the
  project subtree (mirrors `cleanupSharedDirPVCs` on K8s, `k8s_runtime.go:753-770`).
  Optional idle-GC by mtime (§10). (Clone-per-agent workspaces are node-local, not on
  NFS, and are cleaned by the existing local path on agent delete.)

---

## 5. Model B — GKE / Cloud Run (direct NFS per pod/instance)

No shared host mount. Each pod/instance mounts the NFS share directly. **Decision
(maintainer, Q4): the target is Filestore *basic* tier**, which has one share per
instance and a 1 TiB minimum and **no multishare** — so strategy **(a) static RWX PV +
per-workspace `subPath` is THE default**, and the dynamic per-project-share strategy (b)
is an Enterprise-tier-only future option, not used now.

### 5.1 GKE — strategy (a): one RWX PV + per-workspace `subPath` (recommended default)

A single `PersistentVolume` (RWX) points at the Filestore share (or self-hosted NFS).
Each pod mounts it with a `subPath` equal to the project/agent relative path. This
avoids per-workspace PVC/Filestore-share churn (Filestore *basic* tier has a 1 TiB
minimum per instance — one share per workspace is economically impossible).

```yaml
# Provisioned once by operator/Hub-bootstrap:
apiVersion: v1
kind: PersistentVolume
metadata: { name: scion-workspaces }
spec:
  capacity: { storage: 1Ti }
  accessModes: [ReadWriteMany]
  nfs: { server: 10.0.0.2, path: /scion-workspaces }   # or csi: filestore.csi...
  mountOptions: [vers=4.1, hard, nconnect=4]
  persistentVolumeReclaimPolicy: Retain
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: scion-workspaces, namespace: scion-agents }
spec:
  accessModes: [ReadWriteMany]
  storageClassName: ""          # bind to the static PV
  volumeName: scion-workspaces
  resources: { requests: { storage: 1Ti } }
```

Pod spec the runtime builds (replaces the EmptyDir at `k8s_runtime.go:1080-1087`):

```yaml
volumes:
- name: workspace
  persistentVolumeClaim: { claimName: scion-workspaces }
containers:
- name: agent
  volumeMounts:
  - name: workspace
    mountPath: /workspace
    subPath: projects/<pid>/workspace        # <-- per-workspace isolation within share
```

`subPath` is the linchpin: the pod can only see its own project subtree, never the
whole export — this is the K8s mount-isolation story (§9.4).

### 5.2 GKE — strategy (b): Filestore CSI dynamic PVC per project *(Enterprise-only, deferred)*

**Not used with the chosen basic tier (Q4).** Recorded for the future: for projects that
need a *dedicated* share (isolation/quota), the Filestore CSI driver
`filestore.csi.storage.gke.io` with a StorageClass lets the Hub create **one PVC per
project** (project-scoped like shared dirs today, `k8s_runtime.go:707-712`). This only
pencils out on Enterprise / "multishare" tiers (many small shares per instance); on basic
tier each PVC would demand a full 1 TiB instance, so it is economically impossible.
When/if adopted, reuse the existing shared-dir PVC code paths (`createSharedDirPVCs`,
`cleanupSharedDirPVCs`) generalized from "shared dir" to "any RWX workspace claim".

### 5.3 GKE — shared dirs

Already RWX PVCs. With NFS, simply point `KubernetesConfig.SharedDirStorageClass` at
a Filestore/NFS class, or fold shared dirs into the single-share+subPath model
(`subPath: projects/<pid>/shared-dirs/<name>`). No new code beyond config.

### 5.4 Cloud Run

Cloud Run (gen2 execution environment) supports **NFS volume mounts** and Filestore
directly. The Hub emits a volume in the Service/Job spec; each instance mounts at the
workspace path:

```yaml
# Cloud Run service (knative-ish) volume
volumes:
- name: workspace
  nfs:
    server: 10.0.0.2
    path: /scion-workspaces/projects/<pid>/workspace   # server-side path = isolation
    readOnly: false
containers:
- volumeMounts: [{ name: workspace, mountPath: /workspace }]
```

Cloud Run has no `subPath`, so isolation comes from the **server-side `path`** being
the project subdir (the instance can only reach what the export path exposes). The Hub
must therefore put the project id in the NFS `path`, not rely on in-container subPath.

### 5.5 The big consequence for K8s/CR: workspace is pre-populated

Today K8s starts with an empty workspace and the Runtime Broker `kubectl cp`s files in
(`k8s_runtime.go:317-350`). With an NFS-backed workspace the bytes are **already
present** on shared storage. The post-start sync of *workspace contents* becomes
unnecessary in the NFS case — provisioning (clone) happens once, out-of-band (§7), not
per-pod. Home-dir/secret sync and the `/tmp/.scion-home-ready` gate may still be needed
for non-workspace material; that path stays, but the workspace copy step is skipped
when backend=`nfs`. This is a meaningful simplification *and* a behavior change to call
out in review (§11 Q5).

---

## 6. Data model & config changes

### 6.1 New config block (Hub settings)

```go
// pkg/config/settings_v1.go
type V1WorkspaceStorageConfig struct {
    Backend string `json:"backend,omitempty" koanf:"backend"` // "local" (default) | "nfs"

    NFS *V1NFSConfig `json:"nfs,omitempty" koanf:"nfs"`
}

type V1NFSConfig struct {
    // One NFS instance may expose multiple shares (maintainer, Q1); a Runtime Broker
    // mounts the set it needs. MountRoot is the local base under which each
    // share is mounted at <MountRoot>/<share.ID>.
    MountRoot    string       `json:"mount_root,omitempty"`    // e.g. /mnt/nfs ; per-share dir appended
    MountOptions string       `json:"mount_options,omitempty"` // default "vers=4.1,hard,nconnect=4,_netdev"
    Shares       []V1NFSShare `json:"shares,omitempty"`

    // Stable, node-independent ownership for NFS-backed trees (§9.1). Default
    // 1000:1000 to converge with the K8s pod UID/GID. The Runtime Broker advertises
    // these as SCION_HOST_UID/GID for NFS-backed agents instead of os.Getuid().
    UID int `json:"uid,omitempty"` // default 1000
    GID int `json:"gid,omitempty"` // default 1000

    // Kubernetes realization (Model B)
    StorageClass string `json:"storage_class,omitempty"`  // Filestore-CSI dynamic strategy (5.2)
    SubPathRoot  string `json:"subpath_root,omitempty"`   // default "projects"
}

type V1NFSShare struct {
    ID     string `json:"id,omitempty"`      // stable share id → mount dir + (K8s) PV name
    Server string `json:"server,omitempty"`  // e.g. 10.0.0.2 or Filestore IP
    Export string `json:"export,omitempty"`  // server export path, e.g. /scion-workspaces
    PVName string `json:"pv_name,omitempty"` // K8s static PV+subPath strategy (5.1)
}
```

A project selects which share holds its workspaces (default: the single configured
share; explicit when multiple exist). Resolution becomes
`<share>/projects/<pid>/...`, and the Runtime Broker ensures that share is mounted (§4.2)
before realizing the bind mount.

- `Backend` defaults to `local` → **zero behavior change** for existing deployments.
- **No separate per-project backend toggle.** Per the maintainer (Q3), backend selection
  follows the **workspace sharing mode** (§3.1), not an independent flag: when
  `Backend: nfs` is configured on the Hub, **Shared-plain** and **Worktree-per-agent**
  projects use the shared NFS workspace; **Clone-per-agent** always uses node-local disk.
  `Backend` thus gates *availability* of NFS on a Hub; the mode decides *use*. (If NFS is
  not configured, shared modes degrade to single-node local — same as today.)

### 6.2 Extend `VolumeMount` for explicit NFS volumes

For user-declared volumes (and to let shared dirs / workspace flow through one code
path), add `nfs` to the type set:

```go
type VolumeMount struct {
    // ... existing ...
    Type   string // "local" | "gcs" | "nfs"
    Server string `json:"server,omitempty"` // NFS: server host/IP
    // Source is reused as the server export path for NFS; Prefix/subpath optional
}
```

Update `VolumeMount.Validate()` (`api/types.go:259-279`) to accept `nfs` requiring
`Server` + `Source` (export path) + `Target`. The existing `type: "nfs"` fixtures
(`pkg/api/types_test.go`, `pkg/config/templates_test.go`), presently *rejected*, become
valid (maintainer-confirmed, Q6). Workspace, shared directories, and ad-hoc user volumes
all flow through this one NFS volume path.

### 6.3 Generalize the K8s shared-dir PVC helpers

`sharedDirPVCName` / `createSharedDirPVCs` / `cleanupSharedDirPVCs`
(`k8s_runtime.go:657-770`) become a generic "project RWX claim" helper used for both
shared dirs and (strategy 5.2) workspaces. No schema change; reuses
`KubernetesConfig.SharedDir{StorageClass,Size}` plus the new `NFS.StorageClass`.

### 6.4 No new Workspace entity, no placement column

Resolution is deterministic from Project ID + agent ID + workspace sharing mode, so a
replica behind the load balancer computes the same NFS path without a DB lookup —
consistent with the broker-dispatch design (`DESIGN-BROKER-DISPATCH.md`) preference for
derivable state. We add
**no** placement/node column to Agent. (Optional future: cache the resolved path in
`AgentAppliedConfig.WorkspaceStoragePath`, which already exists for the GCS-bootstrap
case, `store/models.go:127-165` — reuse it rather than add a field.)

---

## 7. Workspace provisioning (git clone onto NFS)

A workspace must exist and (for git projects) be cloned/worktree'd before the harness
runs. Where the clone executes differs by model:

- **Model A (Docker):** the **Runtime Broker** has direct filesystem access to
  `/mnt/nfs/workspaces`. It runs the existing clone/worktree logic targeting the NFS
  path. This is the smallest change — same code, different base dir.

- **Model B (K8s/CR):** the Runtime Broker has **no** host access to the pod's NFS mount.
  Two options:
  1. **Init container (recommended):** the pod gets an init container that mounts the
     same workspace volume and performs the clone/worktree into it before the main
     container's gate releases. The clone runs where the mount lives; no extra Hub
     filesystem access needed. Fits the existing gate model (`k8s_runtime.go:317-350`).
  2. **Hub-side provisioner:** a Hub / Runtime Broker node mounts the same export and clones into
     the project subdir out-of-band. Simpler to reason about for shared mode (clone
     once, centrally) but requires the Hub node to have the NFS mount. See §11 Q5.

**First-access guard (shared mode):** multiple agents for the same project may start
concurrently and race to clone the same dir. Guard with one of:
- a **sentinel file** (`.scion-provisioned`) created atomically after a successful clone;
- a **Postgres advisory lock** keyed by project-id (the Hub already uses advisory locks
  — see commit `dcd4e0f6`/`f6d2a727` — so this is a natural, cross-node-correct choice);
- an NFS **advisory `flock`** on a lockfile in the project dir (works on NFSv4).

Recommended: Postgres advisory lock for the *decision* ("am I the provisioner?"),
because it is already cross-node-correct and avoids NFS lock-manager variability.

---

## 8. Sequence diagrams

### 8.1 Agent start — Model A (Docker, shared NFS workspace)

```
User/API        Hub (any replica)         Runtime Broker (node-N)              dockerd / agent ctr
   │  start agent     │                          │                              │
   ├─────────────────►│                          │                              │
   │             resolve backend=nfs             │                              │
   │             relPath=projects/<pid>/workspace│                              │
   │             CreateAgentConfig{              │                              │
   │               Workspace=<relPath>,          │                              │
   │               WorkspaceBackend=nfs}         │                              │
   │                  ├─ dispatch ──────────────►│                              │
   │                  │              validate /mnt/nfs present                  │
   │                  │              hostPath = /mnt/nfs/workspaces/<relPath>   │
   │                  │              acquire advisory lock(pid)                 │
   │                  │              if !provisioned: git clone → hostPath      │
   │                  │              release lock                               │
   │                  │              RunConfig.Workspace = hostPath             │
   │                  │              ├─ docker run -v hostPath:/workspace ─────►│
   │                  │              │                              harness runs│
   │◄─────────────────┤◄─────────────┤◄─────────────────────────────────────────┤
```

### 8.2 Agent start — Model B (GKE, Filestore, static PV + subPath)

```
Hub (any replica)        K8s runtime (Runtime Broker)        kube-apiserver / kubelet        pod
   │ resolve backend=nfs       │                            │                         │
   │ subPath=projects/<pid>/workspace                       │                         │
   │ ── CreateAgentConfig ────►│                            │                         │
   │             ensure PVC scion-workspaces (reuse if exists, k8s_runtime style)     │
   │             buildPod: volume=PVC, mount /workspace subPath=<subPath>             │
   │             initContainer: clone into subPath (advisory-locked)                  │
   │                           ├─ create Pod ──────────────►│                         │
   │                           │              schedule + mount NFS (subPath) ────────►│
   │                           │                            │   initC clones (once)   │
   │                           │                            │   gate releases ───────►│
   │                           │                            │           harness runs  │
   │                           │   (NO kubectl cp of workspace — already on NFS)      │
```

---

## 9. Cross-cutting concerns

### 9.1 Permissions / UID mapping — branch on host-FS vs NFS

Rewritten per the maintainer's Q2 guidance (*study the existing remapping, then branch
on host-vs-NFS, keep it simple*). How it works today and the minimal branch NFS needs:

**How Docker reconciles host-FS ownership today (verified):**
- The Runtime Broker injects its **own host UID/GID** as `SCION_HOST_UID`/`SCION_HOST_GID`
  (`common.go:281-282` → `os.Getuid()/os.Getgid()`).
- Container starts as **root**; `sciontool init` → `setupHostUser()`
  (`cmd/sciontool/commands/init.go:923`) **remaps the image's `scion` user to that host
  UID/GID** via `usermod -o -u $SCION_HOST_UID -g $SCION_HOST_GID scion` (`init.go:1045`;
  direct `/etc/passwd` fast-path on fuse-overlayfs, `:1033/:1086`), `chown`s the
  workspace (`ensureWorkspaceOwnership`, `:1416`), then drops privileges. Files thus land
  on the bind mount **owned by the Runtime Broker's host user** — fine, because the disk is
  node-local.
- Podman rootless uses `--userns=keep-id:uid=1000,gid=1000` + `SCION_KEEPID_UID` with an
  early drop to 1000 (`podman.go:172`, `init.go:975`).
- K8s uses a **fixed** UID/GID 1000 + `FSGroup=hostGID` (`k8s_runtime.go:1021-1033`).

**Why NFS can't reuse the "host-UID" scheme:** one export is shared by agents on
*different* nodes whose brokers may have *different* host UIDs, and NFS authorizes by
**numeric UID on the wire**. Files written by node-1 (owned by node-1's Runtime Broker UID) may
be unwritable by node-2. NFS-backed trees therefore need a **node-independent, stable
UID/GID**.

**The branch (small, localized):** choose *which UID the Runtime Broker advertises to the
container*, on host-vs-NFS:

```
// in buildCommonRunArgs (pkg/runtime/common.go, near :281)
uid, gid := os.Getuid(), os.Getgid()        // host-FS path: UNCHANGED (today's behavior)
if backend == nfs {
    uid, gid = cfg.NFS.UID, cfg.NFS.GID      // stable; default 1000:1000 to match K8s
}
addEnv("SCION_HOST_UID", uid); addEnv("SCION_HOST_GID", gid)
```

The whole downstream remap pipeline (`setupHostUser` → `usermod`/passwd-edit → drop) is
**unchanged** — it already does the right thing for whatever UID it's told; for NFS it
remaps `scion` to the stable UID instead of the host Runtime Broker UID. One branch on the
*value advertised*, no new remap machinery.

**Chown discipline on NFS:** do **not** recursively `chown` the NFS tree on every start
(`ensureWorkspaceOwnership`) — slow over the network and racy across nodes. Skip/guard
it when backend=nfs. Ownership is set **once**: (1) operator permissions the store at
creation (Q1) for the stable GID; (2) the provisioner (§7) `chown`s a project subtree
once at first creation, under the advisory lock.

**Convergence:** set the NFS stable UID/GID = **1000:1000** so Docker, Podman, and K8s
all agree. On K8s, `FSGroup` (today `os.Getgid()`, `k8s_runtime.go:1022`) should be the
stable GID for NFS-backed pods — same host-vs-NFS branch.

**Podman rootless + NFS** is the awkward corner: `keep-id` maps via subuid ranges and
won't cleanly yield a stable shared on-wire UID. Recommend rootful Docker / the fixed
scheme for NFS-backed projects and treat rootless+NFS as initially unsupported (Q2).

- Provisioner sets project subtrees `chown 1000:1000`, mode `0770` (or `2770` setgid for
  GID inheritance). UID alone does **not** isolate projects — all agents share UID 1000
  — so project isolation relies on subPath/server-path scoping, not ownership (§9.4).

### 9.2 Concurrent access (by workspace sharing mode)
- **Clone-per-agent:** node-local, not on NFS (§3.1) → no shared-storage contention at
  all. The isolation escape hatch.
- **Worktree-per-agent (on shared NFS workspace):** each agent gets its **own git
  worktree** over the one shared checkout (one `.git`/object DB, many worktrees — already
  a first-class layout, `common.go:192-196`). Working trees and per-agent index are
  isolated, so the dangerous case (concurrent `git checkout` on one index) does not
  arise; only worktree *add/remove* touches shared `.git` metadata and is guarded by the
  provisioning advisory lock (§7). This is the maintainer-confirmed model for shared git
  workspaces on NFS (Q3).
- **Shared-plain (on shared NFS workspace):** one directory mounted into all agents,
  intentionally no isolation (plain/non-git). Concurrent writers coordinate at the
  application level; Scion's per-agent state already lives outside this mount.
- NFSv4 supports byte-range + `flock` advisory locks; NFSv3 needs `rpc.lockd`. Prefer
  **NFSv4.1** in mount options to get reliable locking and `nconnect` throughput.

### 9.3 Performance
- Git is metadata-heavy (thousands of small files); NFS round-trips dominate
  `git status`/`checkout`. Mitigations:
  - Mount tuning: `vers=4.1, hard, nconnect=4-8, rsize/wsize=1M, actimeo` tuned for
    workload.
  - Storage tier: Filestore **zonal/enterprise** (not basic) for IOPS; or self-hosted
    NFS on SSD.
  - Keep hot, ephemeral, per-agent scratch (build caches, `node_modules`) on **local
    ephemeral** disk or a `shared-dir` with `InWorkspace`, not on the cloned tree.
  - Optional: local `.git` cache with NFS-hosted worktrees (advanced; defer).
- Benchmark git clone + `status` + a build on Filestore tiers before committing a
  default (action item for the integration suite).

### 9.4 Security / isolation — projects and Hubs (Filestore basic) — Q4

**Agent ↔ project isolation (the default we ship):** a single Filestore-basic export is
shared by all nodes, so at the host/cluster level **any node that mounts the export can
reach every project's bytes**. Agent-level isolation comes from only ever exposing the
project's own subtree to the container — **never the export root**:
- K8s: `subPath: projects/<pid>/...` (§5.1) — pod sees only its project.
- Cloud Run: the server-side NFS `path` is the project subdir (§5.4) — no subPath needed.
- Docker: bind-mount only `projects/<pid>/...`, not `<MountRoot>/<share>`.
This is **Tier 1** and, per Q4, what we adopt given the basic-tier constraint.

**Hub ↔ Hub isolation (desired, but constrained by the protocol + tier).** The maintainer
asked whether one Hub (with its own GCP service account) could be prevented from mounting
another Hub's shares. Key fact: **NFS data-plane access is not GCP-service-account-aware.**
A service account governs the *GCP control plane* (creating/IAM on the Filestore
instance), but mounting an NFS export is authorized by **network reachability + host/UID**,
not by IAM/SA. So SA-based mount isolation is **not achievable at the NFS layer on any
tier** — and Filestore *basic* additionally has no per-client export ACLs (those are an
Enterprise feature). The realistic isolation levers are therefore:
  1. **Network/firewall scoping (basic-tier feasible, partial):** Filestore basic attaches
     to one VPC; restrict TCP/2049 to specific source ranges/tags. This separates Hubs
     **only if they sit on different networks/instances** — within one shared instance,
     every authorized client can reach the whole share.
  2. **One Filestore instance per Hub (true Hub isolation, the "more expensive option"):**
     because basic = one share per instance (1 TiB min), real per-Hub isolation means a
     dedicated instance per Hub, firewalled to that Hub's nodes. This is the cost ptone
     flagged.
  3. **Enterprise multishare + per-share network rules:** finer isolation without an
     instance per Hub — but that is the pricier tier we are *not* using.

**Recommendation (matches Q4):** ship **Tier 1** now — single shared basic instance,
subpath/server-path scoping for agent↔project isolation, optional firewall scoping. Treat
strict Hub↔Hub isolation as **deferred**: document that it requires either an
instance-per-Hub (basic) or Enterprise multishare, and is skipped until the cost is
justified. Make the share assignment explicit in config (`V1NFSShare.ID` per Hub) so a
future move to instance-per-Hub is a config change, not a redesign.
- **Never** bind/mount the export root into a container — always the project subtree.

### 9.5 Provisioning lifecycle & reclaim
- Create-on-first-agent; persist project-scoped; cleanup on project deletion (mirror
  `cleanupSharedDirPVCs`). PV `reclaimPolicy: Retain` so deleting a PVC never nukes the
  Filestore share. Optional idle GC by mtime with a long TTL; log what is reclaimed
  (no silent deletion).

---

## 10. Migration & rollout

1. **Phase 0:** land config + `VolumeMount` `nfs` type + validation; backend defaults
   `local`. No runtime behavior change. Unit tests (the existing `nfs` fixtures flip
   from "rejected" to "accepted").
2. **Phase 1 (Model A):** redirect Runtime Broker path resolution to the NFS host mount when
   backend=nfs; provisioning + advisory lock. Integration-test on two GCE VMs sharing
   one Filestore instance (fits the postgres integration VM fleet).
3. **Phase 2 (Model B / GKE):** static PV + subPath workspace volume; init-container
   provisioning; skip workspace `kubectl cp`. Generalize shared-dir PVC helpers.
4. **Phase 3:** Cloud Run NFS volumes; Filestore-CSI dynamic option.
5. **Migration of existing workspaces:** one-shot copy of node-local
   `project-configs/...` trees into the NFS layout, flip the project label. Separate
   tool; not auto.

---

## 11. Maintainer decisions (all resolved)

All six questions were resolved with @ptone over `scion message` (2026-06-02), one at a
time; each decision is folded into the sections noted below.

- **Q1 — NFS server & host-mount ownership. [RESOLVED]** Operator creates and
  permissions the NFS store only; **the Hub / Runtime Broker mounts** the share(s) as part of its
  service bring-up and re-mounts idempotently on restart. A single NFS instance may
  expose multiple shares and serve multiple Hub instances within a project. Reflected
  in §1.1, §4.2, §6.1.
- **Q2 — UID alignment. [RESOLVED]** Studied the existing remapping: Docker advertises
  the Runtime Broker's host UID (`SCION_HOST_UID`, `common.go:281`) and `sciontool init` remaps
  `scion` to it (`usermod`, `init.go:1045`); K8s is fixed at 1000. NFS needs a *stable*
  node-independent UID, so the design **branches on host-vs-NFS**: for NFS-backed agents
  the Runtime Broker advertises a stable configured `NFS.UID/GID` (default **1000:1000**,
  matching K8s) instead of `os.Getuid()`, reusing the unchanged remap pipeline; per-start
  NFS chown is skipped (operator + one-time provisioner chown instead). Podman-rootless +
  NFS is **unsupported initially**. Confirmed by maintainer. Detailed in §9.1, §6.2.
- **Q3 — Sharing mode ↔ NFS, and concurrency. [RESOLVED]** Maintainer set the model
  using the glossary's canonical **workspace sharing modes**: NFS backs **both** the
  workspace **and** shared directories; a **shared NFS workspace** serves **all** sharing
  modes that share state — **Shared-plain** and **Worktree-per-agent** (worktrees ride on
  the shared NFS checkout). The **only** non-NFS case is **Clone-per-agent** (node-local).
  Consequence: **no separate per-project backend toggle** — the sharing mode is the
  selector; Hub `Backend: nfs` only governs availability. Reflected in §3.1, §6.1, §9.2.
- **Q4 — Isolation tier & GKE strategy. [RESOLVED]** Target is **Filestore basic** →
  **Tier 1** default: single shared export + subpath/server-path scoping for agent↔project
  isolation, and **static RWX PV + `subPath`** (§5.1) as the GKE strategy (Filestore-CSI
  dynamic §5.2 is Enterprise-only, deferred). Maintainer wants Hub↔Hub isolation ideally;
  flagged that **NFS mounts are not service-account-gated** (network/host/UID auth only)
  and basic tier has no per-client export ACLs, so true Hub isolation needs either an
  instance-per-Hub (basic) or Enterprise multishare — **deferred** as the costlier option,
  with per-Hub share IDs in config to keep that path open. Reflected in §5, §9.4.
- **Q5 — K8s provisioning & sync change. [RESOLVED — yes to both]** Provision the shared
  NFS workspace via an **init container** (mounts the same NFS volume, clones/worktree-adds
  once under a Postgres advisory lock on Project ID), and **skip the post-start `kubectl
  cp` of workspace contents** when backend=nfs. The home-dir/secret sync and the
  `/tmp/.scion-home-ready` readiness gate are unchanged, and the workspace `kubectl cp` is
  retained for the local backend. Reflected in §5.5, §7, §8.2.
- **Q6 — `VolumeMount` nfs type. [RESOLVED — yes, NFS first-class]** Add `nfs` as a
  first-class `VolumeMount.Type` with a `Server` field (`Source` = server export path);
  extend `Validate()` to require `Server`+`Source`+`Target`, flipping the existing
  `type: "nfs"` fixtures (`pkg/api/types_test.go`, `pkg/config/templates_test.go`) from
  rejected to valid. Workspace, shared directories, and ad-hoc user volumes all flow
  through this one unified NFS volume path. Reflected in §6.2.

---

## 12. Summary

NFS-backed workspaces reuse Scion's proven RWX-PVC pattern (today's shared dirs) and
its existing "Runtime Broker realizes the mount the Hub describes" split. The change is mostly
**path mapping + provisioning + config**, not a rewrite:

- **Model A (Docker/VM):** the Runtime Broker mounts the NFS share(s) idempotently at
  startup (operator only created+permissioned the store), redirects workspace/shared-dir
  base paths there, and bind-mounts as today. Container unchanged.
- **Model B (K8s/Cloud Run):** workspace volume becomes NFS-backed (static PV+subPath
  default, Filestore-CSI dynamic option; Cloud Run NFS volume with project-scoped server
  path). Workspace is pre-populated on shared storage, so the post-start `kubectl cp`
  of workspace contents is dropped.
- **Coordination:** deterministic path resolution from IDs (no new placement column);
  provisioning guarded by Postgres advisory locks; project-scoped lifecycle mirroring
  `cleanup*PVCs`; isolation via subPath / server-path scoping with a per-project-share
  upgrade path.
