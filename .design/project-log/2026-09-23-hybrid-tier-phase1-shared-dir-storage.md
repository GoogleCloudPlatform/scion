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

---

## Round 2 review fixes (PR #1779 @f75607b2 → this round)

hy-em's round 2 dispositions (`reviews/r2-dispositions.md`, from `r2-code.md`,
`r2-test.md`, `r2-security.md`) marked 9 items FIX, 1 DECLINED, 2 NOTED.
Headline: round 1's AC5 fix (`LoadEffectiveSettings("")`) was **not**
global-only — an empty path resolves a project from the process's current
working directory (`FindProjectRoot`) and merges it on top of global — and a
new HIGH security finding showed shared-dir names and the project ID were
never validated in the nfs branch, allowing cross-project traversal inside
the shared export. All FIX items are addressed below; none conflicted with
the design.

1. **AC5 still CWD-dependent (C1/T1/S-F2, High).** Added
   `config.LoadGlobalSettings()` (`pkg/config/settings_v1.go`): resolves
   `config.GetGlobalDir()` explicitly and calls
   `LoadEffectiveSettings(globalDir)`, so the project-layer steps in
   `LoadVersionedSettings` are skipped (`projectPath == globalDir`) —
   regardless of the caller's CWD. `pkg/agent/run.go` now calls this helper
   instead of `LoadEffectiveSettings("")`. Updated the doc comments on
   `V1SharedDirStorageConfig` and in run.go, which previously described the
   old (broken) call as "global-only". The test fixture
   (`newSharedDirStorageRunFixture`) now `chdir`s into the **project**
   directory (not `$HOME`) by default, so every test built on it exercises
   the realistic CWD; verified by hand that `TestStartSharedDirStorage_
   ProjectLevelOnly_Ignored` fails against the old
   `LoadEffectiveSettings("")` call and passes with the fix.
2. **HIGH security — shared-dir name / project-ID traversal (S-F1).** In the
   nfs branch of `resolveSharedDirs`, before `Resolve`: `api.ValidateSharedDirs(dirs)`
   (names must be plain slugs — no `.`/`/`) and a new `validSharedDirProjectID`
   check (rejects empty, `.`, `..`, or any `/`/`\`). Added a confinement
   check after `Resolve`: every shared dir's `HostPath` parent must equal
   `<HostBase>/<subpath_root>/<projectID>/shared-dirs` exactly (defense in
   depth; also protects the K8s subPath, since it comes from the same
   `Resolve` output). Tests: `TestResolveSharedDirs_NFS_PoC_
   TraversalNamesAndProjectID` for all three published PoC inputs (`../../../projects`,
   `../../victim/shared-dirs/scratchpad`, project ID `../victim`), each on
   both `docker` and `kubernetes` runtime names — error returned, nothing
   created; plus `TestValidSharedDirProjectID_RejectsTraversal`.
3. **Project ID source in nfs mode (S-F4).** Before changing this, per the
   disposition's stop-and-report condition, I confirmed the hub→broker
   dispatch path: `pkg/hub/httpdispatcher.go` `DispatchAgentStart` (lines
   ~2049-2054) and `DispatchAgentRestart` (~2317-2327, explicitly commented
   "Identity vars at highest precedence") both set
   `resolvedEnv["SCION_PROJECT_ID"]`/`SCION_GROVE_ID` unconditionally, after
   merging `agent.AppliedConfig.Env` (user-supplied), `envFromStorage`, and
   `resolvedSecrets` — so user-supplied env cannot override the
   hub-authoritative project ID. This holds, so I proceeded (no stop/report
   needed). `run.go`'s nfs branch now sources the project ID for
   `resolveSharedDirs` from `opts.Env["SCION_PROJECT_ID"]`/`SCION_GROVE_ID`
   only, not from the general `projectID` variable (which still falls back
   to `settings.Hub.ProjectID` / the project-id file, and is unchanged for
   everything else — labels, `RunConfig.ProjectID`, etc.). Test:
   `TestStartSharedDirStorageNFS_ProjectSettingsProjectID_Ignored` — a
   project-level `hub.project_id` with no env project ID still errors "hub
   project ID"; verified by hand that it fails without the fix.
4. **Symlink confinement (S-F3).** After `MkdirAll`, `resolveSharedDirs` now
   requires `filepath.EvalSymlinks(sd.HostPath)` to equal the expected path
   under `filepath.EvalSymlinks(res.HostBase)`, before any chmod and before
   the path is returned as a mount source. Tests:
   `TestResolveSharedDirs_NFS_SymlinkedLeaf_FailsClosed` (pre-existing
   symlinked leaf pointing outside the export; chmod never runs) and
   `..._SymlinkedIntermediate_FailsClosed` (the project directory itself is
   a symlink outside the export — `MkdirAll` following the symlink to
   create the leaf underneath it is a known, accepted limitation per the
   disposition's "after MkdirAll" framing; what matters is that chmod and a
   successful return never happen).
5. **Global-settings load error fails open (C3/T5).** `run.go` now returns
   the error (`"loading global settings for server.shared_dir_storage: %w"`)
   when `config.LoadGlobalSettings()` fails and there are shared dirs to
   mount, instead of logging at Debug and silently falling back to the local
   layout. Test: `TestStartSharedDirStorage_MalformedGlobalSettings_FailsClosed`
   (malformed global settings.yaml) — Start errors, Run is never called.
6. **AC3 in the nfs-set Start tests (C2/T3).** Moved/added the AC3
   assertions (`WorkspaceBackendName==""`, `NFSPVClaimName==""`,
   `NFSUID==0`/`NFSGID==0`, exact `Workspace` path) into
   `TestStartSharedDirStorage_GlobalWinsOverProjectLevel` (k8s) and
   `TestStartPropagatesSharedDirStorageToLocalContainerRunConfig` (docker) —
   the cases that actually set `shared_dir_storage: nfs`. `Workspace`'s
   expected value (the project root, `filepath.Dir(projectScionDir)`) was
   confirmed by instrumenting a throwaway debug test against the fixture,
   then hardcoded as an exact `assert.Equal`, replacing the old, nearly
   vacuous `NotContains(Workspace, "srv")`.
7. **AC1 at the Start level (T2).** Renamed the unset test to
   `TestStartSharedDirStorage_Unset_AC1` (its AC3 assertions held only
   trivially and are superseded by item 6) and added real assertions:
   `RunConfig.Volumes` contains the legacy
   `<GetSharedDirsBasePath(projectDir)>/scratchpad → /scion-volumes/scratchpad`
   entry, the directory exists on disk, and `SCION_VOLUMES` is set on the
   caller's env map. Added
   `TestStartSharedDirStorage_NoSharedDirs_SCIONVolumesNotSet` for the
   complementary case.
8. **Test (b) couldn't detect a missing `EnsureSharedDirs` call (T4).**
   `TestResolveSharedDirs_Unset_MatchesLegacyBehavior` now calls
   `resolveSharedDirs` FIRST on a fresh project dir, asserts the shared dirs
   exist from *that* call, and only then computes `want` via
   `SharedDirsToVolumeMounts` alone (no mkdir) against the same dir — the
   previous version called `EnsureSharedDirs` itself before
   `resolveSharedDirs`, so the "exists" assertion was pre-satisfied
   regardless of what the code under test did.
9. **Nits.** Reworded the `TestResolveSharedDirs_NFS_ResolveLevelMisconfig_FailsClosed`
   comment (C4: `Validate` *does* catch empty `mount_root`/`shares[0].id`;
   the old comment implied otherwise). Fixed a stale test name in
   `pkg/runtime/shared_dir_storage_test.go`'s comment (C5/T7: referenced a
   test that doesn't exist; now points at
   `TestStartSharedDirStorage_GlobalWinsOverProjectLevel`). Reworded the
   guard-ordering comment in `shared_dir_storage.go` (S-F5: it previously
   implied the export-root guard alone stopped "a traversal in a malformed
   project ID" — it only ever bounded the host base; the new confinement
   check from item 2 is what closes the per-project-subtree gap).
10. **DECLINED — T6** (local branch swallows the `SharedDirsToVolumeMounts`
    error untested): intentionally preserved pre-existing behaviour (AC1
    byte-identical). No action.
11. **NOTED — C6/C7:** legacy-format global settings silently drop the
    server block (design already requires `schema_version: "1"`); no code
    change, a Phase 2 docs/startup-log item. Performance (one extra settings
    load per `Start` when shared dirs exist): no action, negligible.

No design conflicts found. Confirmed the item-3 stop-and-report precondition
held (see above) rather than improvising past it.

### Gate results (round 3 verification, env: clean `env -i PATH=$PATH HOME=<tmp> GOPATH=... GOCACHE=... GOMODCACHE=...`, `-count=1`)

- `go build ./...` — pass.
- `go vet ./pkg/config/... ./pkg/runtime/... ./pkg/agent/...` — pass, no output.
- `go test ./pkg/config/... ./pkg/runtime/... ./pkg/agent/... -count=1` — pass:
  ```
  ok  github.com/GoogleCloudPlatform/scion/pkg/config              2.1s
  ok  github.com/GoogleCloudPlatform/scion/pkg/config/opsettings    0.1s
  ok  github.com/GoogleCloudPlatform/scion/pkg/config/templateimport 0.0s
  ok  github.com/GoogleCloudPlatform/scion/pkg/runtime              32.8s
  ok  github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun     0.0s
  ok  github.com/GoogleCloudPlatform/scion/pkg/agent                6.7s
  ok  github.com/GoogleCloudPlatform/scion/pkg/agent/state          0.0s
  ```
- `gofmt -l pkg/agent pkg/config pkg/runtime` — clean.
- Manually verified (temporary local revert, not committed) that two of the
  new regression tests fail against the pre-fix code:
  `TestStartSharedDirStorage_ProjectLevelOnly_Ignored` against the old
  `LoadEffectiveSettings("")` call, and
  `TestStartSharedDirStorageNFS_ProjectSettingsProjectID_Ignored` against
  reading the general `projectID` variable instead of `opts.Env` directly.

---

## Round 3 review fixes (PR #1779 @76e7e425 → this round)

hy-em's round 3 dispositions (`reviews/r3-dispositions.md`, from `r3-code.md`,
`r3-test.md`, `r3-security.md`) marked 5 items FIX, plus two live amendments
delivered mid-round (item 6 superseded by 6′; item 7 superseded by 7′, both
from nfs-gke via hy-em). Verdicts going in: code REQUEST CHANGES (1 Medium),
security APPROVE (2 Low/2 Nit), tests APPROVE (4 Low/3 Nit). All R2 items
were confirmed resolved by all three reviewers.

1. **Medium (C1 = S-L1) — project settings could still choose the nfs
   project ID via harness-config env on non-hub starts.** The R2 fix
   (round 2 item 3) restricted the nfs branch to `opts.Env`, but
   `resolveAuthEnvOverlay` (called earlier in `Start`) copies a project's
   `harness_configs.<name>.env` into `opts.Env` for any key not already
   present — including `SCION_PROJECT_ID`/`SCION_GROVE_ID` — so on a
   hubless start (no dispatch-provided ID), a project's settings could still
   fill the key before the nfs branch read it. Fix: snapshot
   `hubDispatchedProjectID := projectID` immediately after the initial
   `opts.Env`-only computation at the top of `Start` (before the
   `settings.Hub.ProjectID` fallback and before any settings-driven env
   merging runs), and use only that snapshot in the nfs branch — deleting
   the late re-read of `opts.Env`. Rewrote the run.go comment block to state
   the invariant accurately (it only holds because the snapshot happens
   before the overlay, not because `opts.Env` is somehow immutable).
   Before implementing, per the disposition's explicit stop-and-report
   condition, I re-confirmed (this time reading httpdispatcher.go myself
   again for the exact line ranges hy-rev-3/hy-aud-3 cited) that hub
   dispatch sets the identity vars unconditionally after merging user env in
   both `DispatchAgentStart` and `DispatchAgentRestart` — holds, so no
   escalation was needed. Tests: harness-config env `SCION_PROJECT_ID` and
   `SCION_GROVE_ID` (no dispatch ID) ⇒ "hub project ID" error, `Run` never
   called; a dispatch-only `SCION_GROVE_ID` (no `SCION_PROJECT_ID`) still
   resolves correctly. Manually verified the two "ignored" tests fail
   against a reverted (re-read `opts.Env`) version of the fix.
2. **Low (C2/T1 part 2) — the PoC "nothing created" check couldn't detect
   creation.** The old check compared `len(os.ReadDir(mountRoot))` before
   and after; since `hostBase` was already the only entry there, nothing
   inside `hostBase` could move that count. Replaced with `assertDirEmpty`,
   asserting `hostBase` itself has zero entries after a rejected PoC input —
   meaningful because every mkdir in the nfs branch creates its first new
   component as a direct child of `hostBase`.
3. **Low (T1 part 1/S-N1) — traversal defenses weren't pinned
   independently.** Extracted the confinement check into `confineSharedDir`
   (hostPath, hostBase, subPathRoot, projectID, name) so it can be unit
   tested directly with inputs that pass name/ID validation but resolve to
   the wrong parent (`TestConfineSharedDir`). The PoC table now asserts the
   *specific* layer expected to catch each input (`"invalid name"` for the
   two name PoCs via `api.ValidateSharedDirs`, `"invalid hub project ID"`
   for the project-ID PoC via `validSharedDirProjectID`), not just "some
   error".
4. **Low (T2) — confinement/symlink checks untested with a non-default
   `subpath_root` or a symlinked `mount_root`.** Both are legitimate
   operator layouts (§3.2.1 documents `subpath_root`; a symlinked
   `mount_root` is a normal way to point at a separately mounted disk), and
   a false positive in either check would reject every nfs agent start.
   Added `TestResolveSharedDirs_NFS_CustomSubPathRoot_StillWorks` (docker +
   kubernetes) and `TestResolveSharedDirs_NFS_SymlinkedHostBase_StillWorks`
   (also pins that the returned Docker bind source is the resolved real
   path, satisfying part of disposition 7').
5. **Nit (S-N2) — `validSharedDirProjectID` was deny-list only.** Replaced
   with an allow-list, `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`. Requiring the
   first character to be alphanumeric subsumes the old deny-list (rejects
   `.`, `..`, any run of dots, and both path separators) and additionally
   rejects control characters, spaces, and unbounded length. Table test
   covers both the traversal cases and the new allow-list boundaries
   (length limit, control chars).
6′. **FIX, amended mid-round by hy-em/nfs-gke, superseding the original
    item 6 — malformed global settings must not regress unset deployments.**
    hy-tst-3's probe showed that on `main`, a malformed global
    `settings.yaml` with one shared dir still starts the agent (legacy local
    volume); the R2 fix made this fail closed unconditionally, which would
    hit essentially every agent, since every project has a default
    scratchpad. New behaviour: on a `LoadGlobalSettings()` error, read the
    raw global settings bytes (new `config.GlobalSettingsMentions(substr)`
    helper in `pkg/config`) — fail closed only if they mention
    `"shared_dir_storage"` (the operator plausibly intended to configure
    it); otherwise `slog.Warn` and proceed exactly like `main`. Tests:
    malformed file *without* the key ⇒ Start succeeds with the legacy
    volume (`..._NoKey_SucceedsAsMain`); malformed file *with* an
    nfs block ⇒ Start still errors, `Run` never called
    (`..._MentionsKey_FailsClosed`, renamed from the R2 test). Also added a
    direct `pkg/config` unit test for `GlobalSettingsMentions`. Removed the
    one-line PR-body note the original item 6 had asked for (superseded).
7′. **FIX, amended mid-round by hy-em/nfs-gke, superseding the original
    item 7 (POSTPONED-ACCEPTED) — eliminate the symlink TOCTOU with
    `os.Root`.** The R2 fix (mkdir, then check with `EvalSymlinks`) could
    already have created a directory outside the export through a
    symlinked intermediate component before the check ever ran. Rewrote the
    mkdir/chmod section to open an `os.Root` (Go 1.26) at the
    (symlink-resolved) host base and do `root.MkdirAll`/`root.Stat`/
    `root.Chmod` through it — `os.Root` refuses to traverse a symlink that
    would escape the root at all, so nothing is created outside the base on
    any failure path, including the symlinked-intermediate case R2 had
    accepted as a known limitation. Dropped the manual `EvalSymlinks`-
    equality assertion as redundant: `os.Root` enforces the same property
    structurally, for every operation, not as a one-time check after the
    side effect already happened (documented in the code comment as
    requested, rather than silently dropped). The returned Docker bind
    `Source` is now the resolved real path (`filepath.EvalSymlinks` on the
    already-`os.Root`-verified leaf), not the unresolved one — patched onto
    the `[]api.VolumeMount` slice `NFSSharedDirsToVolumeMounts` produced,
    matched by index since both are built from the same ordered `dirs`
    slice. Tests: symlinked leaf, symlinked `projects/<pid>` intermediate,
    symlinked `shared-dirs` intermediate — all refused, and (unlike R2)
    nothing created inside the symlink's target at all; a symlinked
    `mount_root`/host base (valid layout) still works, with the resolved
    Source pinned. Added the short PR-body threat note the disposition
    asked for (design Erratum E1 precondition: pods can't reach nfsd
    directly, so only kubelet mounts for pod specs naming the PVC, or local
    VM access, can write above a project's leaf).
8. **DECLINED (T5, T6, T7 nits).** No action: dead-code guard kept as
   defence, `GetGlobalDir` error branch not portably testable, k8s
   `Backend=="nfs"` guard equivalent to today's only-ever-nfs case.

No design conflicts found. The item-3 stop-and-report precondition
(confirming the hub dispatch path) was re-verified and held, so no
escalation was needed this round either.

### Gate results (round 4 verification, env: clean `env -i PATH=$PATH HOME=<tmp> GOPATH=... GOCACHE=... GOMODCACHE=...`, `-count=1`)

- `go build ./...` — pass.
- `go vet ./pkg/config/... ./pkg/runtime/... ./pkg/agent/...` — pass, no output.
- `go test ./pkg/config/... ./pkg/runtime/... ./pkg/agent/... -count=1` — pass:
  ```
  ok  github.com/GoogleCloudPlatform/scion/pkg/config              2.0s
  ok  github.com/GoogleCloudPlatform/scion/pkg/config/opsettings    0.1s
  ok  github.com/GoogleCloudPlatform/scion/pkg/config/templateimport 0.1s
  ok  github.com/GoogleCloudPlatform/scion/pkg/runtime              32.6s
  ok  github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun     0.0s
  ok  github.com/GoogleCloudPlatform/scion/pkg/agent                7.3s
  ok  github.com/GoogleCloudPlatform/scion/pkg/agent/state          0.0s
  ```
- `gofmt -l pkg/agent pkg/config pkg/runtime` — clean.
- PR body updated: "Behaviour note" on the malformed-global-settings
  exception, and a "Security" section summarizing the CWD fix, traversal
  validation, project-ID snapshot, os.Root hardening, and the threat-model
  precondition (design Erratum E1) per disposition 7'.

---

## Round 4 review fixes (PR #1779 @08b78b41 → this round)

hy-em's round 4 dispositions (`reviews/r4-dispositions.md`, from `r4-code.md`,
`r4-test.md`, `r4-security.md`) marked 6 items FIX and 1 optional. **Headline:
round 3's 7' fix had a Medium regression** — dropping the R3 resolved-leaf
equality check as "redundant with os.Root" was factually wrong (os.Root
bounds escapes from the host *base*; it does not bound the returned Source
to the project's own *leaf*), and all three reviewers found the same bug
independently with probes. All FIX items are addressed below; the previously
inaccurate claims in code comments, the PR body, and this log are corrected
per item 4. This round included the requested self-check: all three attached
probes were run against the fixed code before pushing (see "Self-check"
below).

1. **Medium (C1=T1=S-M1, all three reviewers) — restore the leaf-confinement
   check.** Restored, after the `os.Root` mkdir/chmod operations: compute
   `wantResolvedLeaf := filepath.Join(resolvedHostBase, rel)` and require
   `filepath.EvalSymlinks(sd.HostPath) == wantResolvedLeaf`, failing closed
   otherwise; `volumes[i].Source` is only ever set to the checked, resolved
   value. `os.Root` and this check are complementary, not redundant: `os.Root`
   stops creates/chmods from leaving the *host base* (including through a
   component swapped mid-operation); the equality check is what confines the
   *returned bind Source* to the *project's own leaf*, which `os.Root` alone
   does not do — it happily follows an in-base relative symlink (e.g. one
   project's `shared-dirs` pointing at another's). Added
   `TestResolveSharedDirs_NFS_InBaseSymlinkedLeaf_ToVictim_FailsClosed` and
   `..._InBaseSymlinkedIntermediate_ToVictim_FailsClosed` (relative symlinks
   that stay *inside* the export, unlike every R3 symlink test, which only
   used targets outside it) — both assert the victim directory is untouched
   (same mode, no new files). Manually verified the intermediate test fails
   without the restored check (the leaf case is independently caught by
   item 6's new Lstat pre-check, so both layers are now pinned by different
   tests). Also assert the primary layer's message in each of the three
   existing outside-pointing symlink tests (T4 nit/S-N2).
2. **Low (S-L1) — legacy-format global settings silently drop the block.**
   A global `settings.yaml` with no `schema_version: "1"` takes the legacy
   loader path, which has no `server` field at all: `LoadGlobalSettings`
   returns `err == nil` with `Server == nil`, so the 6' fail-closed branch
   never runs. Added an `else if` after the successful-load case: if
   `Server`/`SharedDirStorage` came back nil AND
   `config.GlobalSettingsMentions("shared_dir_storage")`, fail closed with
   `"global settings mention server.shared_dir_storage but it was not
   loaded (missing schema_version: \"1\"?)"`. Tests: a well-formed legacy
   file *with* an nfs block errors and Run is never called
   (`TestStartSharedDirStorage_LegacyGlobalSettings_MentionsKey_FailsClosed`);
   the companion *without* a mention still succeeds exactly like main
   (`..._LegacyGlobalSettings_NoKey_SucceedsAsMain`).
3. **Low (C2) — tests assumed `t.TempDir()` is symlink-free.** macOS puts
   its temp dir under `/var`, itself a symlink to `/private/var`; comparing
   an already-resolved `Source` against a path built from the raw
   `t.TempDir()` value would fail spuriously there (CI is Linux-only, so
   this didn't block CI, but the repo ships an Apple `container` runtime).
   Added a `newResolvedTempDir(t)` helper (`t.TempDir()` +
   `filepath.EvalSymlinks`) and switched every `mountRoot := t.TempDir()` in
   `shared_dir_storage_test.go` to use it, plus the separate `t.TempDir()`
   call in the symlinked-host-base test.
4. **Low (C3/S-N1) — make the PR body, code comments, and this log
   accurate.** Rewrote the `os.Root` section comment in
   `shared_dir_storage.go` to state the two-layer property precisely (base
   confinement via `os.Root`, leaf confinement via the equality check) and
   removed the false "redundant" claim. Rewrote the PR body's "Security"
   section and "Behaviour note" the same way, and explicitly removed
   "accepted for Phase 1 and tracked as a follow-up via nfs-gke" — 7' is
   fixed in Phase 1, not deferred. The one honest residual documented is
   that the Docker daemon re-resolves the bind path at container create,
   after `resolveSharedDirs` already returned, which no path-based check
   run before that point can close.
5. **Low (T2) — factor the AC1 assertions into a shared helper.** Extracted
   `assertLegacyLocalSharedDirBehavior` (SharedDirStorage nil, the legacy
   volume, the on-disk directory, `SCION_VOLUMES` set) out of
   `TestStartSharedDirStorage_Unset_AC1`, and call it from both the
   `NoKey_SucceedsAsMain` and the new `LegacyGlobalSettings_NoKey_...` tests
   so the three "must produce output identical to the unset case" tests
   can't silently drift apart.
6. **Nits (C4/C5).** Replaced the "treat any `root.Stat` error as
   not-exists" pattern with an explicit `root.Lstat` + `errors.Is(...,
   fs.ErrNotExist)` check, which also gives a clear "is a symlink; refusing"
   or "exists but is not a directory" error instead of the confusing
   `mkdirat: file exists` `os.Root` would otherwise surface for a
   leaf-is-a-symlink input — and, as a side effect, independently closes
   the in-export leaf-symlink case from item 1 one layer earlier, before
   any mkdir is attempted.
7. **Optional (T3) — done since trivial.** Added a `chmod 000` case to
   `TestGlobalSettingsMentions` (skipped when running as root), covering the
   read-error branch returning `false`.

No design conflicts found.

### Self-check: the three attached probes, run before pushing

Per hy-em's request, all three reviewers' probe files were copied into
`pkg/agent/` one at a time, run, and removed (never committed):
- `reviews/r4-code-probe_inroot_symlink_test.go.txt` (hy-rev-4): all 4 cases
  now refused (`is a symlink; refusing` for the leaf variants,
  `resolves through a symlink to ... want ...` for the intermediate).
- `reviews/r4-test-probe_intra_export_symlink_test.go.txt` (hy-tst-4): all 5
  cases refused; the dangling-inside case confirms the victim's `newdir` is
  not created.
- `reviews/r4-security-probe_test.go.txt` (hy-aud-4): the in-base
  cross-project symlink is refused; the 20,000-iteration race test, which
  found 6,603 escapes at 08b78b41, found **0** escapes and 20,000 errors at
  the fixed head — the final equality check, computed after all `os.Root`
  operations complete, closes the race window entirely rather than merely
  narrowing it.

### Gate results (round 5 verification, env: clean `env -i PATH=$PATH HOME=<tmp> GOPATH=... GOCACHE=... GOMODCACHE=...`, `-count=1`)

- `go build ./...` — pass.
- `go vet ./pkg/config/... ./pkg/runtime/... ./pkg/agent/...` — pass, no output.
- `go test ./pkg/config/... ./pkg/runtime/... ./pkg/agent/... -count=1` — pass:
  ```
  ok  github.com/GoogleCloudPlatform/scion/pkg/config              2.0s
  ok  github.com/GoogleCloudPlatform/scion/pkg/config/opsettings    0.1s
  ok  github.com/GoogleCloudPlatform/scion/pkg/config/templateimport 0.0s
  ok  github.com/GoogleCloudPlatform/scion/pkg/runtime              32.6s
  ok  github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun     0.0s
  ok  github.com/GoogleCloudPlatform/scion/pkg/agent                6.9s
  ok  github.com/GoogleCloudPlatform/scion/pkg/agent/state          0.0s
  ```
- `gofmt -l pkg/agent pkg/config pkg/runtime` — clean.
- PR body updated with the corrected "Security" section and "Behaviour
  note" (item 4).

## Round 5 review fixes (PR #1779 @b8a7d235 → this round)

hy-em's round 5 dispositions (`reviews/r5-dispositions.md`, from `r5-code.md`,
`r5-test.md`, `r5-security.md`) marked 4 items FIX and 1 no-action. **Round 6
is the last review round before escalation**, so every item below was done
completely and self-verified against the reviewers' own probes before
pushing (numbers under "Self-check" below). Headline: round 4's fix was
correct for the returned `Source`, but two new regressions were found in the
mechanism used to get there — a Medium in the legacy-format "mentions" check,
and a Low in the `os.Root`-based mkdir/chmod, both fixed below by replacing
`os.Root` entirely with a race-free raw-syscall component walk.

1. **Medium (C1=T1=S-L2) — the "mentions" substring check regressed a
   well-formed v1 file with a comment.** Round 4's S-L1 fix
   (`config.GlobalSettingsMentions("shared_dir_storage")`) was meant only for
   files that took the *legacy* loader path (no `server` struct to trust),
   but it fired unconditionally after a successful `LoadGlobalSettings` call
   — including on a well-formed `schema_version: "1"` file whose only mention
   of the key was a YAML comment (e.g. a commented-out `# shared_dir_storage:`
   block, which is the design's documented rollback path: "comment out the
   block to disable"). Every agent Start with a shared dir failed on such a
   broker, with a misleading "missing schema_version" error even though
   `schema_version` was present. Fixed by adding
   `config.GlobalSettingsIsLegacyFormat()` (`pkg/config/settings_v1.go`),
   which reuses the existing `detectHierarchyFormat` — called with the global
   directory itself as the "project path", so its project-layer check is
   skipped and it reports purely on the global file's format — and gating
   `run.go`'s fail-closed branch on
   `GlobalSettingsIsLegacyFormat() && GlobalSettingsMentions(...)`. A
   successfully-loaded v1 file is now trusted at face value, exactly like
   `main`: no parsed `SharedDirStorage` struct means it genuinely was not
   configured. The malformed-file branch (6') is unchanged — the raw
   substring check there remains a deliberate, accepted trade-off, since a
   file that fails to parse at all has no struct to trust in the first place.
   Tests added: `TestStartSharedDirStorage_V1GlobalSettings_CommentedOutBlock_SucceedsAsMain`
   (v1 file, comment-only mention, succeeds byte-identical to main via
   `assertLegacyLocalSharedDirBehavior`); the existing
   `LegacyGlobalSettings_MentionsKey_FailsClosed` (legacy format with a real
   block still fails closed, `Run` never called) and
   `..._NoKey_SucceedsAsMain` (legacy, no mention, succeeds) needed no
   changes and continue to pass unmodified. T5 nit: also assert
   `"loading global settings"` in
   `MalformedGlobalSettings_MentionsKey_FailsClosed`'s error, pinning the
   wrapping context and not just the setting name. C4 nit: the
   `"(missing schema_version...)"` error text is now accurate on every path
   that can produce it, since that branch only fires for a legacy-loaded
   file.
2. **Low (C2=T2=S-L1, all three reviewers) — `os.Root` still let in-export
   symlinks cause real filesystem side effects before refusing.** `os.Root`
   only refuses a traversal that would *escape the root*; it does not refuse
   a symlink that stays *inside* it. So `root.Lstat`/`root.MkdirAll`/
   `root.Chmod` happily followed an in-export symlink — e.g.
   `projects/<pid> → victim` or `projects/<pid>/shared-dirs →
   ../B/shared-dirs` — meaning that when the victim's leaf was absent, a
   real setgid directory was created inside the victim's own tree (or at the
   export root) *before* the post-hoc resolved-leaf equality check ever ran,
   violating round 4's explicit "nothing created in the victim" requirement.
   A swap race could additionally chmod an *existing* victim leaf (measured
   7,032/20,000 in `reviews/r5-security-probe_test.go.txt` against the
   round-4 code). Fixed by removing `os.Root` from this path entirely and
   replacing it with a race-free component walk
   (`pkg/agent/shared_dir_storage_unix.go`, new file, `//go:build unix`,
   using `golang.org/x/sys/unix`, already a go.mod dependency at v0.47.0):
   the resolved host base is opened as a directory fd, and each component of
   `rel` (`<subpath_root>/<pid>/shared-dirs/<name>`) is opened with
   `openat(parentfd, comp, O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC)`; on
   `ENOENT`, `mkdirat` then a reopen with the same flags (a concurrent
   creator's `EEXIST` is fine — the reopen with `O_NOFOLLOW` still refuses
   any symlink the racing process left behind); `ELOOP`/`ENOTDIR` fails
   closed naming the component. Because `O_NOFOLLOW` refuses to open or
   traverse a symlink atomically as part of the single syscall, there is no
   separate stat-then-open window for a concurrent swap to land in — this is
   what eliminates the race, not merely narrows it. The leaf is `fchmod`'d
   via its already-open fd (`0o2775`, the traditional Unix `mode_t` value,
   sidestepping the recurring `os.FileMode`-vs-`mode_t` setgid bit-layout bug
   from earlier rounds) — never a path-based chmod, and only when this call
   created the leaf. Discovered and fixed along the way: on this Linux
   kernel, `openat(O_DIRECTORY|O_NOFOLLOW)` on a symlink returns `ENOTDIR`,
   not `ELOOP` (verified with a throwaway probe program), so
   `classifyComponentOpenError` does a secondary `Fstatat(...,
   AT_SYMLINK_NOFOLLOW)` check purely to produce an accurate "is a symlink"
   vs. "exists but is not a directory" error message — the refusal itself is
   already correct either way. The final `EvalSymlinks` strict-equality
   check on the returned `Source` is kept as defense in depth, per the
   disposition. Builds verified for both `linux` (native) and `darwin`
   (`GOOS=darwin go build ./pkg/...`) — there is no Windows target.
   Tests: rewrote the three symlink tests whose assertions were tied to the
   old three-layer error wording (`"stat shared dir"`,
   `"resolves through a symlink"`) to assert the new unified
   `"is a symlink"` message, which now fires identically for every symlinked
   component regardless of layer. Added, all *without* a pre-created victim
   leaf and walking the tree afterward to confirm nothing was created:
   `projects/<pid> → victim`
   (`..._InBaseSymlinkedIntermediate_ToVictim_NoLeaf_FailsClosed`),
   `projects/<pid>/shared-dirs → ../victim/shared-dirs`
   (`..._InBaseSymlinkedSharedDirsComponent_ToVictim_NoLeaf_FailsClosed`), a
   component symlinked to the export root
   (`..._ComponentSymlinkToExportRoot_FailsClosed`); plus a deterministic
   version of the race probe, an existing victim leaf at mode `0700` that
   must stay exactly `0700`
   (`..._InBaseSymlinkedLeaf_ExistingVictimLeaf_ModeUnchanged`). T4 nit: left
   an explanatory comment on `createSharedDirViaComponentWalk` instead of a
   deterministic mid-walk swap test — forcing that exact interleaving would
   need a production-code test hook solely to pause the walk, for a property
   syscall atomicity already guarantees and the probe already verifies
   empirically at 20,000 iterations.
3. **Low (T3) — the "exists but is not a directory" branch was untested
   (mutant L2 survived).** Added
   `TestResolveSharedDirs_NFS_LeafIsRegularFile_FailsClosed` (a plain file at
   the leaf) and `..._IntermediateIsRegularFile_FailsClosed` (a plain file at
   an intermediate component), both asserting fail-closed with "not a
   directory" and no `Source`/volumes.
4. **Low/Nit (C3, S-N1) — PR body and log overclaim.** After item 2, updated
   the PR body's "Security" section: replaced the "two layers" bullet
   (`os.Root` for base confinement, the equality check for leaf confinement)
   with a single accurate description of the component walk refusing a
   symlink at *any* component before touching anything, kept the equality
   check described as defense in depth, and updated the "Behaviour note" and
   "Test plan" sections to describe the item 1 legacy-format gating precisely
   (no longer "fails closed when the raw file mentions the key" without the
   legacy-format qualifier) and to report the round 5 self-check numbers.
   `os.Root` wording was removed from every place it no longer applies;
   remaining mentions in the PR body are explicitly comparative/historical
   ("this replaced an earlier `os.Root`-based approach..."). Code comments in
   `shared_dir_storage.go`/`shared_dir_storage_unix.go` already stated the
   corrected mental model as part of the item 2 rewrite (no separate pass
   needed). The one residual kept exactly as written in the prior
   PR-body-only update: D2, the Docker daemon's own bind-path re-resolution
   at container-create time (E1 precondition; impact = arbitrary broker-uid
   path such as `/home/scion`; accepted by ptone; E2 hardening tracked in
   `ptone/scion#1794`, required before Phase 3).
5. **No action / noted.** The `SCION_` env-var prefix leaking into
   `LoadGlobalSettings` (pre-existing koanf-mapping FYI) — hy-em will raise a
   follow-up with nfs-gke separately. K10, G2, S3/S11/S14b/S20/N3/N6 survivors
   keep the same round 4 classification (declined/equivalent/dead-code/
   race-only) — no new action needed.

No design conflicts found.

### Self-check: reviewers' round 5 probes, run before pushing

All three probe files were copied into `pkg/agent/` one at a time (splitting
`r5-security-probe_test.go.txt`'s two concatenated `package agent` blocks
into two separate files first, and renaming `r5-test-probe_inbase_create_test.go.txt`'s
helper functions to avoid a name collision with the security probe's
same-named helper), run, and removed — never committed:

- `reviews/r5-security-probe_test.go.txt`
  (`TestProbeR5_PidToVictim_NoLeaf`, `..._SharedDirsToVictim_NoLeaf`,
  `..._SharedDirsToVictim_LeafExists`, `..._ProjectsToRoot`, `..._PidToRoot`):
  all 5 refused, tree unchanged before/after in every case.
  `TestProbeR5_RaceInBaseChmod` (5,000 iterations, in-base swap race):
  **0 accepted, 0 victim leaves created** (was a live gap under round 4's
  `os.Root` code — the whole point of this probe). `TestProbeR5_RaceChmodExistingVictim`
  (20,000 iterations, existing 0700 victim leaf): **0 accepted, 0 chmods**
  (down from 7,032/20,000 stray chmods measured against the round-4 code).
- `reviews/r5-test-probe_inbase_create_test.go.txt`
  (`TestZZR5_IntermediateToVictim_NewLeaf`, `..._SharedDirsToVictim`,
  `..._ComponentToExportRoot`, `..._LeafToExportRoot`,
  `..._PartialCreateOnSecondFailure`): all refused; the walk confirms no
  `newdir`/`scratchpad` entry created in the victim's tree and no
  `scratchpad` created at the export root in any case.
- `reviews/r5-test-probe_mention_comment_test.go.txt`
  (`TestZZR5_V1CommentedOutBlock`, `..._V1CommentNoServer`,
  `..._V1NullBlock`): all succeed with `Run` called, matching `main`, per
  item 1's required behavior. `TestZZR5_LegacyCommentOnly` (a *legacy*-format
  file whose only mention is a comment) fails closed as expected — this is
  the accepted trade-off explicitly scoped by the disposition to the
  v1-format case only; the disposition's required tests (v1-comment-only
  succeeds, legacy-with-block still fails closed, legacy-no-mention
  succeeds) all pass.

### Gate results (round 5, env: clean `env -i PATH=$PATH HOME=<tmp> GOPATH=... GOCACHE=... GOMODCACHE=...`, `-count=1`)

- `go build ./...` — pass.
- `GOOS=darwin go build ./pkg/...` — pass.
- `go vet ./pkg/config/... ./pkg/runtime/... ./pkg/agent/...` — pass, no output.
- `go test ./pkg/config/... ./pkg/runtime/... ./pkg/agent/... -count=1` — pass:
  ```
  ok  github.com/GoogleCloudPlatform/scion/pkg/config              2.0s
  ok  github.com/GoogleCloudPlatform/scion/pkg/config/opsettings    0.1s
  ok  github.com/GoogleCloudPlatform/scion/pkg/config/templateimport 0.1s
  ok  github.com/GoogleCloudPlatform/scion/pkg/runtime              32.8s
  ok  github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun     0.0s
  ok  github.com/GoogleCloudPlatform/scion/pkg/agent                6.9s
  ok  github.com/GoogleCloudPlatform/scion/pkg/agent/state          0.0s
  ```
- `gofmt -l` on every touched file — clean.
- PR body updated (item 4) — confirmed via `gh api .../pulls/1779` that the
  patched body was applied as sent.

## Round 6 addendum: env-free + project-free global read, hermeticity, test/nit cleanup (PR #1779 @9174c89c → this round)

hy-em raised this mid-round-6 (not part of `reviews/r6-dispositions.md` proper — round 6's own
code/test/security reviews were interim at @9174c89c, blocking on this fix): nfs-gke flagged the
round-5 env-free work as still incomplete, in three escalating messages folded into
`reviews/r6-addendum-env.md`. hy-rev-6's interim review (`reviews/r6-code.md`) independently
confirmed the same Medium and raised four more small items, all folded into the same push.

**Headline finding (Medium, blocking UAT): `LoadGlobalSettings` still went through the general,
env-merging loader chain.** Round 5 added `LoadGlobalSettings()` believing it was already
global-only and CWD-independent, but it still called `LoadEffectiveSettings(globalDir)` →
`LoadVersionedSettings`/`LoadSettingsKoanf`, both of which unconditionally merge koanf's `SCION_`
environment provider. hy-em reproduced live against the ii2 hub: `SCION_AUTO_EXPOSE_PORTS=true`
(an ordinary in-container variable, also present in every scion-agent's own container) maps to the
bare key `auto_expose_ports`, which collides with a struct-typed field elsewhere in
`VersionedSettings` and makes koanf's `Unmarshal` fail outright — and because the file separately
mentions `shared_dir_storage`, the round-4/5 fail-closed branch then failed *every* agent Start on
that broker, configured or not. The full 21-variable ii2 hub env set alone (`findings/
ii2-hub-env-names.txt`) does NOT trigger this — only a collision does — but the broker's full env is
not fully known, so the risk could not be ruled out in general.

**A second, independent finding surfaced in the same message (Low, hy-aud-6, folded in because the
loader was being rewritten anyway): `LoadGlobalSettings`/`GlobalSettingsIsLegacyFormat` were not
actually global-only when `~/.scion` itself contains a `project-id` or `grove-id` file.**
`resolveEffectiveProjectPath(globalDir)`, called internally by the general loaders and by
`detectHierarchyFormat`, treats whatever directory it is given as if it might itself be a project
directory: if `~/.scion` has its own `project-id` file (plausible — a broker's `~/.scion` can double
as its own hub-side project identity), it resolves to
`~/.scion/project-configs/<slug>__<id>/.scion/settings.yaml` — a completely unrelated project's
split-storage settings file — and merges it in on top. hy-aud-6 reproduced this with a
`mount_root: /evil` block showing up from that file.

**Fix — a fully dedicated, standalone loader.** `LoadGlobalSettings()` (`pkg/config/settings_v1.go`)
no longer calls `LoadEffectiveSettings`/`LoadVersionedSettings`/`LoadSettingsKoanf` at all. It now
calls a new `loadGlobalSettingsOnly(globalDir)`, which reads ONLY
`GetSettingsPath(GetGlobalDir())` plus embedded defaults:
- New `detectDirSettingsFormat(dir string) (hasVersioned, missingSchemaVersion bool)`
  (`pkg/config/settings_v1.go`) — a single-directory, no-project-resolution version/format check,
  extracted from `detectHierarchyFormat`'s global-check block (behavior-preserving refactor;
  `detectHierarchyFormat` itself is unchanged for its other, legitimate callers that DO want project
  layering).
- `GlobalSettingsIsLegacyFormat()` rewritten to call `detectDirSettingsFormat(globalDir)` directly
  instead of `detectHierarchyFormat(globalDir)` — closing the exact leak hy-aud-6 found, since
  `detectDirSettingsFormat` never calls `resolveEffectiveProjectPath`/`GetProjectConfigDir` at all.
- New `loadGlobalSettingsOnly(globalDir)`, `loadVersionedSettingsFileOnly(dir)` (in `koanf.go`,
  where the koanf JSON parser import doesn't collide with `settings_v1.go`'s `encoding/json`
  import), and `loadLegacySettingsFileOnly(dir)` (also in `koanf.go`) — each a narrowed copy of the
  corresponding general loader with the project-layer steps (in-repo settings, external
  project-config settings) and the `SCION_` environment provider step removed entirely. No DB-backed
  settings overlay is applied either (shared_dir_storage is Layer-0 / broker-local anyway).
- Every other property is unchanged: global-only via `GetGlobalDir`, CWD-independent, the legacy vs.
  v1 format decision, the 6' malformed-file substring heuristic, and v1-loader-authoritative
  behavior for a successfully-parsed file.
- I first attempted a narrower fix — adding a `loadEnv bool` parameter to
  `LoadVersionedSettings`/`LoadSettingsKoanf`/`LoadEffectiveSettings` internally, keeping their
  public signatures unchanged via a thin wrapper — before hy-aud-6's project-configs-leak finding
  arrived. Once that second finding made clear the loader needed to skip project resolution too
  (not just env), the parameterized approach would have left `loadEnv=false` as a half-measure that
  still called `resolveEffectiveProjectPath`. I reverted that plumbing entirely (back to the
  original, single-purpose `LoadVersionedSettings`/`LoadSettingsKoanf`/`LoadEffectiveSettings`, no
  behavior change for their other callers) and replaced it with the fully standalone
  `loadGlobalSettingsOnly` path described above.

**Tests** (`pkg/config/settings_v1_test.go`, `pkg/agent/run_shared_dir_storage_test.go`):
- `TestLoadGlobalSettings_EnvFree`: the full 21-name ii2 hub env set (`ii2HubEnvNames`, mirrored as
  test-only data in both packages) alone; the same set plus each of three struct-colliding variables
  (`SCION_AUTO_EXPOSE_PORTS`, `SCION_SERVER`, `SCION_TELEMETRY`) — asserts the nfs block still loads,
  and that an unset block still behaves like `main` under the same colliding env; plus a
  `mount_root` env-override attempt confirming it has no effect (there never was an env mapping for
  this setting).
- `TestLoadGlobalSettings_GlobalOnly_IgnoresProjectConfigsLeak`: a `~/.scion/project-id` file plus an
  external `project-configs/<slug>__<id>/.scion/settings.yaml` carrying its own nfs block with
  `mount_root: /evil` — both when the real global file has a block (must read the global file's
  value, not the leaked one) and when it doesn't (must not inject one).
- Start-level companions: `TestStartSharedDirStorageNFS_AmbientHubEnv_ColidingVar_StillSucceeds` and
  `..._UnsetBlock_SucceedsAsMain`, which set the ii2 set + `SCION_AUTO_EXPOSE_PORTS=true` as the TEST
  PROCESS's own environment (`t.Setenv`, simulating the broker's real ambient environment) — not
  `opts.Env` (the started agent's env, which `LoadGlobalSettings` never consulted even before this
  fix and would have been the wrong thing to test against).

**hy-rev-6 interim items (`reviews/r6-code.md`), folded into the same push:**
- **#3 (Low) — hermeticity.** hy-rev-6 found 9 of this PR's own tests failing outside `env -i` in
  their environment; I independently confirmed the underlying mechanism in a much more heavily
  SCION_*-polluted container (this agent's own runtime env has 44 SCION_* variables, including
  `SCION_AUTO_EXPOSE_PORTS`). Running the full target suite in an ordinary shell showed 40 failures;
  I checked each one against base commit `af48a545` (temporarily checking out the affected files via
  `git checkout af48a545 -- <files>`, confirming the same failures reproduce with zero PR changes
  applied, then restoring via `git checkout 9174c89c8 -- <files>` and `git stash pop` to recover
  in-progress work) and found 39 of the 40 are pre-existing, general `LoadVersionedSettings`/
  `Provision`/`Start` hermeticity issues against ambient `SCION_*`, entirely unrelated to this PR's
  diff (confirmed: none of those 39 test names exist in any file this PR touches). Only
  `TestSharedDirStorageConfig_YAMLRoundTrip` (`pkg/config`, added this PR) was mine to fix: it
  legitimately exercises the general `LoadEffectiveSettings` loader (not the new env-free one, since
  it's testing that the general v1 loader parses the block correctly), so it now clears every
  ambient `SCION_*` variable in its own fixture before running, matching the existing
  `native_telemetry_provision_test.go` pattern (`t.Setenv(key, "")` + `os.Unsetenv(key)`, so the
  original value is restored automatically at test end). The other 39 pre-existing failures are out
  of scope for this PR (analogous to the round-5 "SCION_ env-prefix leaks into LoadGlobalSettings"
  FYI, which hy-em already flagged for a separate nfs-gke follow-up) — not fixed here.
- **#4 (Low) — missing component-walk test shapes.** Added
  `TestResolveSharedDirs_NFS_SubPathRootComponentSymlinkToDot_FailsClosed` (`projects` itself, the
  walk's first component, symlinked to `.` — resolving back to the export root),
  `TestResolveSharedDirs_NFS_SubPathRootComponentSymlinkedOutside_FailsClosed` (same first component,
  symlinked OUTSIDE the export instead), and `TestResolveSharedDirs_NFS_PidComponentSymlinkToDotDot_FailsClosed`
  (`projects/<pid> → ..`, the two-dot form distinct from the existing absolute/victim-relative
  shapes). All three assert refusal and that nothing new was created.
- **#5 (Nit) — stale test comments.** Updated the `os.Root`/`Lstat`-era doc comments on
  `TestResolveSharedDirs_NFS_SymlinkedLeaf_FailsClosed` (and its inline nit comment) and
  `TestResolveSharedDirs_NFS_SymlinkedHostBase_StillWorks` to describe the round-5 component walk
  instead. The two comments hy-rev-6 confirmed as correct history (the round-4/5 comparison prose)
  were left as-is.
- **#6 (Nit) — unhelpful error for a malformed `subpath_root`.** Added `validateSubPathRoot` and a
  call to it from `V1SharedDirStorageConfig.Validate()`: an absolute `subpath_root`, or one
  containing an empty, `.`, or `..` path component, now fails config validation with a clear message
  instead of surfacing a confusing low-level `mkdir path component "": no such file or directory`
  from the component walk. New table-driven test cases added to `TestSharedDirStorageConfig_Validate`.
- **#7/#8** — FYI, no action (matches hy-rev-6's own classification: harmless fd-based chmod-on-a-
  leaf-we-didn't-create when a concurrent creator wins the mkdirat race; a legacy-format file with a
  comment-only mention still fails closed, which is the accepted 6' trade-off, not a defect).

### Gate results (round 6, env: clean `env -i PATH=$PATH HOME=<tmp> GOPATH=... GOCACHE=... GOMODCACHE=...`, `-count=1`, AND separately in an ordinary shell)

- `go build ./...` — pass.
- `GOOS=darwin go build ./pkg/...` — pass.
- `go vet ./pkg/config/... ./pkg/runtime/... ./pkg/agent/...` — pass, no output.
- Clean-env `go test ./pkg/config/... ./pkg/runtime/... ./pkg/agent/... -count=1` — pass:
  ```
  ok  github.com/GoogleCloudPlatform/scion/pkg/config              2.2s
  ok  github.com/GoogleCloudPlatform/scion/pkg/config/opsettings    0.1s
  ok  github.com/GoogleCloudPlatform/scion/pkg/config/templateimport 0.0s
  ok  github.com/GoogleCloudPlatform/scion/pkg/runtime              32.9s
  ok  github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun     0.0s
  ok  github.com/GoogleCloudPlatform/scion/pkg/agent                7.2s
  ok  github.com/GoogleCloudPlatform/scion/pkg/agent/state          0.0s
  ```
- Ordinary-shell (ambient, ~44 `SCION_*` vars including `SCION_AUTO_EXPOSE_PORTS`) same command:
  39 failures, all confirmed pre-existing at base `af48a545` and outside this PR's touched files —
  see hy-rev-6 item #3 above. `TestSharedDirStorageConfig_YAMLRoundTrip` (this PR's own test) now
  passes in this shell too.
- `gofmt -l` on every touched file — clean.
- PR body updated with the env-free/project-free loader rationale, new test names, and the
  hermeticity finding.
