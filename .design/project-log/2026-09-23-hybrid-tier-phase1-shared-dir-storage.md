# Hybrid Deployment Tier — Phase 1: `server.shared_dir_storage`

Implemented Phase 1 of `deploy-config-explore/design.md` (v1.4, APPROVED): a
new `server.shared_dir_storage` setting that lets Docker/Podman/Apple and
Kubernetes runtimes resolve a project's shared directories onto the same
NFS-backed layout, independent of `workspace_storage`. Design references:
§1, §3.2 (all), §3.4, §3.5 item 5, §7 Phase 1, §8.

## Changes

### `pkg/config/settings_v1.go`
- Added `V1SharedDirStorageConfig` (`Backend`, `NFS *V1NFSConfig` — reuses the
  existing NFS type per design §3.2.1) and wired it onto `V1ServerConfig` as
  `SharedDirStorage` (`json/yaml/koanf: shared_dir_storage`).
- Added `(*V1SharedDirStorageConfig).Validate()`: backend=nfs ⇒ NFS!=nil,
  len(Shares)>=1, MountRoot!="", Shares[0].ID!="". Stricter than the sibling
  `V1WorkspaceStorageConfig.ValidateNFS` because shared_dir_storage has no
  other source for the host mount point used by local-container bind mounts.

### `pkg/config/opsettings/koanf.go`
- Added `"server.shared_dir_storage"` to `layer0Prefixes`: per-broker,
  restart-required, never written to the DB (AC5).

### `pkg/agent/shared_dir_storage.go` (new)
- `resolveSharedDirs(...)`: the single resolution point used for both
  runtimes (design §3.2.3 "one function, both runtimes"), extracted out of
  `run.go`'s inline block so it's independently testable.
  - Unset/`""`/`"local"`: unchanged local layout via
    `config.EnsureSharedDirs` + `config.SharedDirsToVolumeMounts`; errors are
    logged and swallowed exactly as before (AC1, byte-identical).
  - `"nfs"`: validates the config, fails closed if the hub project ID is
    empty (G5), resolves via `runtime.NewNFSBackend(...).Resolve`, mkdirs
    each shared dir `0o775|os.ModeSetgid` under the host base **only when the
    host base already exists** (setgid can only ever be inherited from an
    already-setgid parent — see "Pitfalls" below), fails closed naming the
    host base when it's absent on a local-container runtime, and otherwise
    (e.g. a future in-cluster GKE broker, §3.3 T1) proceeds without mkdir.
    Wires `runtime.NFSSharedDirsToVolumeMounts` (AC6: now has a production
    caller) and returns a `*runtime.SharedDirRealization` for the K8s side.
  - `isLocalContainerRuntime(name)`: `docker`/`podman`/`container` — decides
    whether a missing host base is fatal.

### `pkg/agent/run.go`
- Replaced the inline shared-dir block (was `run.go:952-972`) with a call to
  `resolveSharedDirs`, propagating its error (fail-closed) and setting
  `SCION_VOLUMES` only when volumes were actually produced (same condition
  as before). Populates the new `RunConfig.SharedDirStorage` field.

### `pkg/runtime/interface.go`
- Added `SharedDirRealization{Backend, PVClaimName, SubPaths}` and
  `RunConfig.SharedDirStorage *SharedDirRealization`.

### `pkg/runtime/k8s_runtime.go`
- `createSharedDirPVCs`: early return when `SharedDirStorage.Backend ==
  "nfs"` (no dynamic PVCs), checked before the existing
  `workspace_storage:nfs` early return — this is the precedence order for
  AC2/(d).
- `buildPod`'s shared-dir loop: new first branch for
  `SharedDirStorage.Backend == "nfs"` — mounts `SharedDirStorage.PVClaimName`
  by `SubPaths[name]`, honours `ReadOnly`, and fails closed (naming
  `pv_name`) when the claim name is empty, or naming the dir when no subPath
  was resolved for it (G5/AC4). `nfsSharedDirs` (the existing
  `workspace_storage:nfs` branch) is now gated on `!sharedDirStorageNFS`, so
  `shared_dir_storage` wins when both are set (test (d)).
- Untouched: local workspace + EmptyDir + kubectl-cp sync path, and the
  `workspace_storage:nfs` branch when `shared_dir_storage` is unset.

## Tests → (a)-(d) / AC mapping

- **(a)** `pkg/runtime/shared_dir_storage_test.go`:
  `TestSharedDirStorage_DockerAndK8s_SameLayout` — one `V1SharedDirStorageConfig`,
  resolves via `NewNFSBackend(...).Resolve`, then asserts the Docker bind
  source (`NFSSharedDirsToVolumeMounts`) and the K8s `buildPod` subPath both
  land on `projects/<pid>/shared-dirs/scratchpad` under the same host base,
  with no dynamic `scion-shared-*` PVC. Covers AC2, AC6 (exercises
  `ValidateNotExportRoot` inside `NFSSharedDirsToVolumeMounts`).
- **(b)** `pkg/agent/shared_dir_storage_test.go`:
  `TestResolveSharedDirs_Unset_MatchesLegacyBehavior` (nil/empty/`"local"`) —
  byte-identical to `config.EnsureSharedDirs`/`SharedDirsToVolumeMounts`
  called directly. `pkg/runtime/shared_dir_storage_test.go`:
  `TestBuildPod_SharedDirStorageNFS_Unset_UnaffectedLocalBehavior` covers the
  K8s side. Covers AC1, AC3 (workspace untouched — no code path here touches
  workspace fields).
- **(c)** Fail-closed, AC4:
  - `pkg/agent/shared_dir_storage_test.go`:
    `TestResolveSharedDirs_NFS_MissingProjectID_FailsClosed` (missing project
    ID), `TestResolveSharedDirs_NFS_InvalidConfig_FailsClosed` (broken
    config), `TestResolveSharedDirs_NFS_MissingHostBase_LocalContainerRuntime_FailsClosed`
    (missing host base, docker/podman/container — error names the host
    base), `TestResolveSharedDirs_NFS_MissingHostBase_Kubernetes_Succeeds` (T1
    compatibility: missing host base is *not* fatal for a kubernetes-runtime
    broker).
  - `pkg/runtime/shared_dir_storage_test.go`:
    `TestBuildPod_SharedDirStorageNFS_EmptyPVClaimName_FailsClosed` (error
    names `pv_name`), `TestBuildPod_SharedDirStorageNFS_MissingSubPath_FailsClosed`.
  - `pkg/config/settings_v1_test.go`: `TestSharedDirStorageConfig_Validate`
    (all four required-field checks, plus the pass case) — AC4/AC5 support.
- **(d)** `pkg/runtime/shared_dir_storage_test.go`:
  `TestBuildPod_SharedDirStorageNFS_PrecedesWorkspaceStorageNFS` — both
  `SharedDirStorage` and `WorkspaceBackendName == "nfs"` set;
  `shared_dir_storage` wins for both `buildPod` and `createSharedDirPVCs`.
- Supporting: `TestResolveSharedDirs_NFS_HostBasePresent_MkdirsSharedDirs`
  (real mkdir, no stubs — asserts the actual on-disk shared dir and its
  Source path), `TestServerConfig_SharedDirStorage_Field`,
  `TestCreateSharedDirPVCs_SharedDirStorageNFS_SkipsPVCCreation`,
  `TestIsLocalContainerRuntime`, and an added `"server.shared_dir_storage"` /
  `"server.shared_dir_storage.nfs"` pair in
  `opsettings_test.go:TestClassifyKeys_AllLayer0Prefixes` (AC5).

## Pitfalls found during implementation

- **`os.ModeSetgid` is not the Unix octal `0o2000` bit.** Go's `os.FileMode`
  encodes setgid at a different bit position than the traditional Unix mode;
  passing the literal `0o2775` to `os.MkdirAll`/`os.Chmod` silently drops the
  setgid request (`syscallMode()` only recognizes the named `os.ModeSetgid`
  constant). Must write `os.ModeSetgid | 0o775`. Separately, even with the
  correct constant, `mkdir(2)` only ever actually applies setgid to a new
  directory via **kernel inheritance from an already-setgid parent** — an
  unprivileged process cannot request setgid on a brand-new directory via
  the `mkdir` mode argument at all; only an explicit `chmod` after creation
  (or inheritance) sets it. This matches design §3.5.5's assumption that the
  export root is pre-provisioned `scion:scion 2775` by ops — our mkdir calls
  work correctly *because* they run under an already-setgid host base in
  production; the design's own text already hints at this ("so setgid keeps
  the group consistent" — describing inheritance, not our mode argument).
- The container's injected `SCION_AUTO_EXPOSE_PORTS` breaks the settings
  loader in tests, as flagged in the brief; all `go test` runs here used a
  clean `env -i` per the brief's workaround.

## Gate results (env: clean `env -i PATH=$PATH HOME=<tmp> GOPATH=... GOCACHE=... GOMODCACHE=...` for tests)

- `go build ./...` — pass.
- `go vet ./pkg/config/... ./pkg/runtime/... ./pkg/agent/...` — pass, no output.
- `go test ./pkg/config/... ./pkg/runtime/... ./pkg/agent/...` — pass:
  ```
  ok  github.com/GoogleCloudPlatform/scion/pkg/config              1.9s
  ok  github.com/GoogleCloudPlatform/scion/pkg/config/opsettings    (cached)
  ok  github.com/GoogleCloudPlatform/scion/pkg/config/templateimport (cached)
  ok  github.com/GoogleCloudPlatform/scion/pkg/runtime              32.7s
  ok  github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun     (cached)
  ok  github.com/GoogleCloudPlatform/scion/pkg/agent                6.2s
  ok  github.com/GoogleCloudPlatform/scion/pkg/agent/state          (cached)
  ```
- No pre-existing failures encountered; nothing needed to be checked against
  `af48a545` (repo baseline for this branch).

## Out of scope (per brief, Phase 2/3)

Hub file browser resolution (§3.2.5), ignored-field warnings for `uid`/`gid`/
`mount_options`/`storage_class` under `shared_dir_storage`, tier docs,
`deploy.sh` changes. `workspace_storage` behaviour is untouched (verified by
the existing `pkg/runtime/k8s_nfs_test.go` NFS-workspace suite still passing
unmodified, and by the precedence test above).
