# fsstore P1: pkg/projectfs and the hub shared-dir browser on the uuid tree

**Date**: 2026-10-22
**Issue**: ptone/scion#5806 (sub-issues #5807–#5812, tracking #5805)
**Design**: fs-cleanup design-v3 §P1, §P3 shared-dir rows, Phase 1 (gs://scion-xproject-exchange/fs-cleanup/design-v3.md)

## Summary

A customer running a Cloud Run hub with a GKE broker and Filestore (topology T4,
`server.workspace_storage: nfs` only) uploaded files to the `scratchpad` shared
dir from the web UI, and no agent could see them. The hub resolved shared dirs
through a file-only `config.LoadGlobalSettings()` branch that consulted only
`server.shared_dir_storage`. That block was unset, so uploads landed on the hub
instance's local disk under `project-configs/<slug>__<short>/shared-dirs/`.
Agents mount `projects/<uuid>/shared-dirs/<name>` on the export.

Changes:

- **New leaf package `pkg/projectfs`**: the layout authority. It defines `Mount`,
  `Layout`, `Resolved`, `Layout.Resolve`, `ProjectRoot` and `AgentRoot` for the
  nfs and local backends, plus `ErrNoMount` and `DeclaredSharedDir`. Project
  IDs, agent slugs and shared-dir names are validated once in `Resolve`, using
  `shareddirs.ValidProjectID`, `api.ValidateAgentName`, `api.ValidateSharedDirs`
  and `shareddirs.ConfineLeaf`. `Resolved.Open(create)` is the single
  constructor of confined handles. On nfs it uses the `pkg/shareddirs`
  anchored O_NOFOLLOW walk, with `EnsureLeaf` for create. On local it refuses
  to root at a symlink. The package imports only `pkg/api`, `pkg/shareddirs`
  and the standard library.
- **Thin wrappers**: `runtime.nfsBackend.Resolve`, `runtime.localBackend.Resolve`,
  `config.GetSharedDirPath` and k8s `nfsSharedDirSubPath` now delegate to
  projectfs. `config.NFSLayout` is the one translation of an nfs block into a
  `Layout`. Results are identical for every valid input. The new validation
  applies only to inputs that the callers' own gates already reject.
- **Hub**: `resolveSharedDirPath` builds its Layout from the in-memory
  `ServerConfig`. `server.shared_dir_storage` is now loaded at startup into
  `GlobalConfig` and `ServerConfig`, and `SCION_SERVER_SHARED_DIR_STORAGE_*`
  applies, like `workspace_storage`. The per-request file read
  (`resolveNFSSharedDirPath`) is deleted. Every shared-dir handle comes from
  `Resolved.Open`.
- **Write gate**: on a Cloud Run Service, shared-dir writes are gated on the
  resolved Mount's backend (`projectfs.Backend.Durable`) rather than on the
  workspace's. A refused write is rejected before the directory is created,
  with the error code `shared_dir_writes_unavailable`.

## Decisions

- **T3 precedence (fs-cleanup-arch, 2026-10-22).** Shared-dir Mounts use
  `shared_dir_storage` if it is nfs, otherwise `workspace_storage` if it is
  nfs, otherwise local. This is what the brokers do today: k8s `buildPod`
  checks the `SharedDirStorage.Serves` branch before the workspace-PVC branch,
  and the Docker agent manager honours only `shared_dir_storage`. A startup
  warning names the disagreement when both blocks are nfs and differ. P3
  replaces all of this with `server.project_storage`.
- **cloudrun-volume / gke-shared-volume hubs keep shared dirs local**, as the
  brokers do (per-dir RWX claims). The write gate now answers 503 there
  instead of writing to instance disk. A note on ptone/scion#5823 says the
  gate opens once P3 places shared dirs on the shared volume.
- **Linked project through the co-located broker keeps its local path** when
  the Layout comes from `workspace_storage`. Local-container brokers keep
  shared dirs local unless `shared_dir_storage` is nfs (findings D9). Git
  projects whose only providers are remote brokers used to get a 409; on a
  shared Layout they now resolve to the uuid tree.
- **`Mount.Kind`** is an explicit discriminator (workspace | shared-dir).
  The design's sketch used `Name`, but `workspace` is also a valid
  shared-dir name.
- **`Layout.Resolve` takes a `Project` struct** (ID, plus Slug, Dir and
  ConfigRoot for the local backend) instead of a bare project ID. The local
  backend keeps slug-keyed paths and linked checkouts, so it needs those
  inputs.
- **Out of scope** (later phases): the workspace Files tab, `hub-projects/`,
  `project_storage`, attachments (`attachments_nfs_test.go` expectations
  unchanged; only its setup now sets the in-memory config), lifecycle and
  delete (the project deletion service still reads the settings file), and
  removing the wrappers.

## Tests

- `pkg/projectfs`: a table over backend × Mount × sharing mode, with
  rejection cases for invalid IDs, names, agents, subpath roots, kinds and
  user-owned Mounts. Open round trip, symlink refusal at the project,
  shared-dirs and leaf components (with create and without), the
  symlinked-host-base allowance, and the local backend.
- `pkg/config`: `TestNFSLayout`, `TestGetSharedDirPath_ValidatesName`. The env
  contract test now models `server.shared_dir_storage` on `GlobalConfig`.
- `pkg/hub` (`shared_dir_layout_test.go`): precedence table, warnings, the
  T4 uuid tree for scratchpad and a custom dir (upload plus agent-side write),
  remote-linked project, embedded linked project kept local (and refused on
  Cloud Run), the Cloud Run non-durable refusal, T1 slug paths unchanged, and
  shared_dir_storage winning over workspace_storage.
  `TestSharedDirFiles_SettingsFileNotReadPerRequest` pins A4.

## Pre-existing failures seen on upstream main (not caused by this change)

- `pkg/config TestVersionedSettings_TagsAgree` (api.K8sToleration koanf tags).
- `pkg/agent TestSettingsLoads_FlatPathsUseRequestView` (run_placement.go).
