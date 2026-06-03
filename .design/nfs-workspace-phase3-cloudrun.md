# NFS Workspace — Phase 3 (Cloud Run + Filestore-CSI) Design Note

**Status:** Documentation deliverable (N3-1 + N3-2). **No code in this phase.**
**Why doc-only:** Verified against `postgres/wave-b-integration` — Scion has **no Cloud Run
runtime** (`pkg/runtime/factory.go` supports `container`/`docker`/`podman`/`kubernetes` only;
no `run.googleapis.com`/knative anywhere). There is no Cloud Run Service/Job spec to attach an
NFS volume to, so N3-1 "emit an NFS volume in the Cloud Run spec" cannot land as code until a
Cloud Run runtime exists. Building that runtime is a separate, larger effort outside the NFS
plan's scope. This note records the realization design so it's a config/wiring change, not a
redesign, when a Cloud Run runtime is added. (Companion: `nfs-workspace.md` §5.4/§9.4.)

---

## N3-1 — Cloud Run NFS workspace realization (design, for when a Cloud Run runtime exists)

Cloud Run (gen2 execution environment) supports NFS volume mounts (incl. Filestore). When a
Cloud Run runtime is added to `pkg/runtime`, NFS realization should mirror the Docker/K8s
backends already shipped (Wave 1/2):

- **Selection:** reuse `SelectWorkspaceBackend(cfg, mode)` (Wave 1) — NFS applies for
  Shared-plain + Worktree-per-agent; Clone-per-agent stays node-local. No new toggle.
- **Volume spec:** emit an NFS volume in the Cloud Run Service/Job spec:
  ```yaml
  volumes:
  - name: workspace
    nfs:
      server: <V1NFSShare.Server>            # e.g. 10.45.255.170
      path: /<export>/projects/<pid>/workspace   # server-side path = isolation
      readOnly: false
  containers:
  - volumeMounts: [{ name: workspace, mountPath: /workspace }]
  ```
- **Isolation (critical, §9.4):** Cloud Run has **no `subPath`**. Isolation therefore comes
  from the **server-side `path`** being the project subdir — the instance can only reach what
  the export path exposes. The Hub MUST put `projects/<pid>/workspace` in the NFS `path`,
  never the export root. Reuse the spirit of `ValidateNotExportRoot` (Wave 1
  `pkg/runtime/nfs_path_guard.go`): assert the emitted server path is strictly below the
  export root before realizing.
- **UID/GID:** same convergence as Docker/K8s — stable 1000:1000 (`V1NFSConfig.UID/GID`); the
  container runs as 1000. (Cloud Run runs the container user; align with the provisioned
  ownership.)
- **Mount options / tier:** Filestore **basic = NFSv3** (default `vers=3`, set in Wave 1
  N1-7). NFSv4.1 needs Enterprise/zonal.
- **Provisioning:** Cloud Run instances have no host access for the Hub to clone into; the
  workspace must be **pre-provisioned** on NFS (same as the K8s init-container model, Wave 2
  N2-2/N2-2b) — guarded by the per-project Postgres advisory lock
  (`TryAdvisoryLockObject(LockWorkspaceProvision, StableProjectHash(pid))`). A Cloud Run
  runtime would need an equivalent first-access provisioning step (e.g. a pre-create
  provisioning Job, or Hub-side provisioner with NFS access) before starting the instance.
- **Acceptance (future):** a Cloud Run instance mounts only `projects/<pid>/workspace` and
  cannot reach the export root (server-path isolation).

## N3-2 — Filestore-CSI dynamic PVC (Enterprise-only, deferred — recorded upgrade path)

Not implemented (Q4: target is Filestore **basic**, which has no multishare and a 1 TiB
minimum, so one-PVC-per-project is economically impossible; CSI dynamic is Enterprise/zonal
"multishare" only). Recorded upgrade path:

- Reuse the **generalized project-RWX-claim helper** from Wave 2 N2-5 (the generalized
  `sharedDirPVCName`/`createSharedDirPVCs`/`cleanupSharedDirPVCs`) plus `V1NFSConfig.StorageClass`
  (`filestore.csi.storage.gke.io`) → **one PVC per project** instead of the static
  RWX-PV-+-subPath default (Wave 2 N2-1).
- Keep `V1NFSShare.ID` **per-Hub** (already in config) so moving to **instance-per-Hub
  isolation** (the true Hub↔Hub isolation option, §9.4) is a config change, not a redesign.
- When adopted: swap the workspace volume source from static-PV+subPath to a per-project
  dynamic PVC via the generalized helper; lifecycle (create-on-first-agent, cleanup on
  project delete) already mirrors `cleanup*PVCs`.

---

## Summary
Wave 3 is documentation only on this branch: the Cloud Run NFS realization (N3-1) is fully
specified and reuses Wave 1/2 primitives (backend selector, export-root isolation guard,
stable UID, advisory-lock provisioning, vers=3) — it just needs a Cloud Run runtime to attach
to. The Filestore-CSI dynamic per-project strategy (N3-2) is the recorded Enterprise-tier
upgrade, reusing the Wave 2 N2-5 generalized PVC helper, with per-Hub share IDs keeping the
instance-per-Hub isolation path open.
