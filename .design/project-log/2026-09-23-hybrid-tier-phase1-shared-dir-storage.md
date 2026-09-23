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

---

## Round 1 review fixes (PR #1779 @ff442f6f → this round)

hy-em's round 1 dispositions (`reviews/r1-dispositions.md`, from `r1-code.md`
and `r1-test.md`) marked all 13 items FIX. All 13 are addressed below; none
conflicted with the approved design.

1. **AC5 — `shared_dir_storage` honoured at project level (C1/T2, High).**
   `pkg/agent/run.go` now loads `server.shared_dir_storage` from a
   **global-only** settings load (`config.LoadEffectiveSettings("")`),
   never from the project-merged `settings` used for everything else.
   `workspace_storage` is untouched, per §3.2.6 and the dispositions' note
   that its pre-existing project-level exposure is a separate, out-of-scope
   follow-up. Tests: `TestStartSharedDirStorage_ProjectLevelOnly_Ignored`
   (project-only block ignored) and
   `TestStartSharedDirStorage_GlobalWinsOverProjectLevel` (global wins over
   a conflicting project-level value) in the new
   `pkg/agent/run_shared_dir_storage_test.go`.
2. **Unknown `backend` fails open (C2/T3, High).**
   `V1SharedDirStorageConfig.Validate()` now rejects any `Backend` not
   exactly `""`, `"local"`, or `"nfs"` (no trim/fold — `"NFS "`/`"Nfs"` are
   rejected). `resolveSharedDirs` calls `Validate()` whenever `sdCfg != nil`,
   before branching on backend, so an unrecognized value can no longer fall
   through to the local-layout branch. Tests:
   `TestSharedDirStorageConfig_Validate` (config-level table, `pkg/config`),
   `TestResolveSharedDirs_NFS_InvalidConfig_FailsClosed` (agent-level table:
   `nsf`, `"NFS "`, `Nfs`, `garbage`).
3. **`run.go` production wiring untested (High).** Added
   `pkg/agent/run_shared_dir_storage_test.go`, modelled on
   `TestStartPropagatesNFSWorkspaceBackendToRunConfig`: (i) kubernetes-named
   mock → `RunConfig.SharedDirStorage` populated
   (`TestStartSharedDirStorage_GlobalWinsOverProjectLevel` also covers this);
   (ii) local-container mock + existing host base → `RunConfig.Volumes`
   entry with the resolved source, and `SCION_VOLUMES` set on the caller's
   env map (`TestStartPropagatesSharedDirStorageToLocalContainerRunConfig`);
   (iii) no project ID → `Start` errors naming "hub project ID" and `Run` is
   never called (`TestStartSharedDirStorageNFS_MissingProjectID_ErrorsAndNeverRuns`).
   AC3 and the unset-block variant: `TestStartSharedDirStorage_Unset_AC1AndAC3`
   asserts `SharedDirStorage == nil`, `WorkspaceBackendName == ""`, and a
   local (non-NFS) `Workspace` path.
4. **mkdir before the export-root guard (C3/T4, Medium).** Reordered
   `resolveSharedDirs`: `runtime.NFSSharedDirsToVolumeMounts` (which runs
   `ValidateNotExportRoot` for every dir) is now called *before* any
   `os.MkdirAll`. That function does no I/O, so a traversal in the project
   ID now fails before any directory is created. Test:
   `TestResolveSharedDirs_NFS_TraversalProjectID_GuardBeforeMkdir` — asserts
   both the error and `os.IsNotExist` on the escaped path.
5. **k8s branch coverage gaps — ReadOnly, multiple dirs, InWorkspace (C4/T5,
   Medium).** `TestBuildPod_SharedDirStorageNFS_PerDirDetails` in
   `pkg/runtime`: two dirs (`{scratchpad, ReadOnly}`, `{b, InWorkspace}`),
   asserting per dir: volume name, `ClaimName`, `PVC.ReadOnly`,
   `mount.ReadOnly`, `SubPath`, `MountPath`, plus that the two volume names
   are distinct.
6. **AC3 untested in test (a) (C4/T6, Medium).**
   `TestSharedDirStorage_DockerAndK8s_SameLayout` now asserts the pod's
   `workspace` volume is `EmptyDir` with no PVC.
7. **Weak validation-error assertions (T7, Medium).**
   `TestResolveSharedDirs_NFS_InvalidConfig_FailsClosed` now asserts the
   specific message (`"no nfs block is configured"`) instead of only the
   shared `"server.shared_dir_storage"` prefix. Added
   `TestResolveSharedDirs_NFS_ResolveLevelMisconfig_FailsClosed` for empty
   `mount_root` and empty `shares[0].id`.
8. **Weak byte-identical comparison (C5/T8, Low).**
   `TestResolveSharedDirs_Unset_MatchesLegacyBehavior` now uses one project
   dir for both the "legacy" and "new" calls and asserts
   `assert.Equal` on the full `[]api.VolumeMount`, plus that the directories
   actually exist on disk.
9. **Tautological field test (T9, Low).** Replaced
   `TestServerConfig_SharedDirStorage_Field` with
   `TestSharedDirStorageConfig_YAMLRoundTrip`: writes the §3.2.1 YAML to a
   real global `settings.yaml` and loads it through `LoadEffectiveSettings`,
   pinning the koanf/yaml tags end to end.
10. **Misleading "production caller" comment in test (a) (T10, Low).**
    Reworded: the test exercises `NewNFSBackend(...).Resolve` and
    `NFSSharedDirsToVolumeMounts` directly (package boundaries prevent
    `pkg/runtime` from importing `pkg/agent`); the actual production caller,
    `resolveSharedDirs`, is pinned by the new agent-level tests (items 3, 4,
    8) instead.
11. **Unsupported runtimes silently succeed (C8/T11, Medium).** `nfs` with a
    runtime that is neither local-container nor kubernetes (cloudrun,
    cloudrun-sandbox, or anything else) now errors:
    `server.shared_dir_storage=nfs is not supported on runtime %q`. Test:
    `TestResolveSharedDirs_NFS_UnsupportedRuntime_FailsClosed`.
12. **Setgid comment duplication / test claiming false coverage (C6/T12,
    Nit).** Shortened the production comment to one block; the test comment
    no longer duplicates it. Superseded by item 13: since the leaf is now
    explicitly `chmod`'d, the setgid assertion genuinely covers production
    code again (chmod, unlike mkdir's mode argument, isn't limited to
    parent-inheritance).
13. **§3.5(5) says `0o2775`; mkdir alone can't produce it (C7, small).**
    After `os.MkdirAll`, `resolveSharedDirs` now explicitly
    `os.Chmod`s each **leaf shared dir it just created** (not any
    pre-existing one) to `os.ModeSetgid|0o775`. Tests:
    `TestResolveSharedDirs_NFS_HostBasePresent_MkdirsSharedDirs` (leaf mode
    is exactly `0o2775`, deterministic — `chmod` isn't subject to umask) and
    `TestResolveSharedDirs_NFS_PreexistingSharedDir_NotChmoded` (a
    pre-existing shared dir's mode is left untouched).

No design conflicts found; nothing needed escalation back to hy-em beyond
what the dispositions already recorded (workspace_storage's project-level
exposure, explicitly called out as a separate follow-up, not touched here).

### Gate results (round 2, env: clean `env -i PATH=$PATH HOME=<tmp> GOPATH=... GOCACHE=... GOMODCACHE=...`, `-count=1`)

- `go build ./...` — pass.
- `go vet ./pkg/config/... ./pkg/runtime/... ./pkg/agent/...` — pass, no output.
- `go test ./pkg/config/... ./pkg/runtime/... ./pkg/agent/... -count=1` — pass:
  ```
  ok  github.com/GoogleCloudPlatform/scion/pkg/config              2.1s
  ok  github.com/GoogleCloudPlatform/scion/pkg/config/opsettings    0.1s
  ok  github.com/GoogleCloudPlatform/scion/pkg/config/templateimport 0.1s
  ok  github.com/GoogleCloudPlatform/scion/pkg/runtime              32.6s
  ok  github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun     0.0s
  ok  github.com/GoogleCloudPlatform/scion/pkg/agent                7.0s
  ok  github.com/GoogleCloudPlatform/scion/pkg/agent/state          0.0s
  ```
- `gofmt -l pkg/agent pkg/config pkg/runtime` — clean.
