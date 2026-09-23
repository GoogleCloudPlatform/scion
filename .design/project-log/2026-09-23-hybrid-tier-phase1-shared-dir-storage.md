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
