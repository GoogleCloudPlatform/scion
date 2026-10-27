# Project storage phase 3a: one storage block, hub/broker layout identity, shared dirs on the layout

**Date**: 2026-10-23
**Issues**: ptone/scion#5821, ptone/scion#5822, ptone/scion#5823 (layout identity), ptone/scion#5824, ptone/scion#5859 (phase ptone/scion#5820, tracking ptone/scion#5805)
**Design**: the project storage cleanup design tracked in ptone/scion#5805 (one config block, read-only workspace configuration mount, config migration)

## Summary

Phase 1 added `pkg/projectfs`. The hub's shared-dir browser used it with a
layout synthesized from two of the three storage blocks. This phase makes
one block the only storage configuration and routes hubs, brokers and
every runtime through it.

- **`server.project_storage`** (`pkg/config/project_storage.go`). It has a
  backend (`local`, `nfs`, `cloudrun-volume`, `gke-shared-volume`) for
  workspaces and shared dirs, a `homes` choice (`local`, `nfs`), and
  `homes_options` (the former `home_storage` knobs). It is Layer-0: it is
  not in the DB, and `config set` cannot write it. The settings loader
  drops every storage block a project's settings.yaml sets and keeps the
  global ones, so a project file places neither workspaces nor shared dirs
  nor homes (this also closes the project-level `workspace_storage`
  override that existed before).
- **Synthesis (M1)**. Without the block, `SynthesizeProjectStorage` builds
  it from the deprecated blocks. Shared dirs follow the workspace backend
  (one backend places both). A hub with `shared_dir_storage: nfs` but no
  nfs workspace keeps its shared dirs on that export through a transitional
  split
  (`LegacySharedDirsNFS` / `projectfs.Layout.SharedDirs`), which cannot be
  configured. The startup warning prints the exact block to write. The
  hub health check `project_storage_config` stays degraded until the block
  is written. Startup is refused only when the configuration is ambiguous:
  the block together with an old one, two nfs blocks that name different
  trees, or nfs homes with no share. A settings file that mentions a
  storage key but loads no block also refuses startup.
- **Removed overrides**. `shared_dir_storage_backend`,
  `shared_dir_storage_backends`, `home_storage_backend` and
  `home_storage_leaf` on runtimes, profiles and flat instances are
  rejected with a message that names `volumes:`. This applies to settings
  validation, the hub admin API, flat instance loading and the profile
  converter. The per-agent shared-dir backend record, the
  `reincarnate --shared-dir-backend` option and its hub/broker plumbing
  are gone. A hub request that still carries the option gets 400.
- **Layout identity**. The broker builds its Layout from the same struct
  and reports `homes` in its heartbeat descriptor.
  `projectfs.Layout.DescriptorDiff` compares a hub Layout with a
  descriptor: the backend, the nfs server, export and subpath root, the
  export marker when both sides know it, and homes. The dispatcher refuses
  shared-plain and worktree-per-agent dispatches of hub-native projects to
  a mismatched broker with a 409 that carries the diff. The health summary
  shows `layout_mismatch` for each broker.
- **Shared dirs on the Layout**. The agent manager resolves every shared
  dir through the Layout on every runtime. Docker, Podman and Apple
  bind-mount the shared tree, which closes the gap where Docker kept shared
  dirs node-local on nfs hubs. k8s `buildPod` has one subPath branch.
  Readers of placement go through `V1ServerConfig.Effective*`, so a file
  with only `server.project_storage` behaves like the equivalent old
  block.
- **Realize**. `Layout.Realize` places a resolved directory as a bind
  mount or a pod subPath mount, and the container target stays the
  Mount's own. `ProjectConfigEntry` is the workspace's own `.scion`
  entry. The read-only `.scion` mount of a shared workspace is emitted
  through both, on Docker/Podman/Apple and on Kubernetes.
- **Deploy and docs**. Helm and Terraform render `server.project_storage`.
  Helm refuses storage blocks in `config.extra` while
  `workspaceStorage.nfs` is on. The server-config reference documents the
  block, the deprecated blocks and the removed keys. The hybrid-tier guide
  covers the upgrade sequence. The web admin page no longer edits the
  removed overrides and drops them on load.

## Decisions

- **Shared-dir declaration DELETE** deletes the directory and its data on
  every shared backend through the confined deleter
  (`shareddirs.DeleteSharedDir`), as on the local backend, which is
  unchanged. This is disclosed in the user docs. Phase 1 kept the leaf
  when the layout came from `workspace_storage`. With one block there is one rule. While the hub
  default makes the scratchpad Required, removing its declaration is
  refused with 409.
- **Cloud Run agent runtimes** cannot mount the shared tree. They keep
  node-local shared dirs on a shared backend, as they do today on
  `workspace_storage: nfs` hubs, and the start returns a user-visible
  warning. This is a capability gap of the runtime type, not a setting,
  and is tracked in ptone/scion#5926. Only the legacy split is refused.
  Refusing every Cloud Run agent would have been an outage, because every
  project has the Required scratchpad.
- **Volume backends without an nfs `shared_dir_storage`** (unset, or set to
  local). A synthesized block
  keeps shared dirs where they are (per-dir claims on Kubernetes) through
  the same kind of transitional split, so nothing becomes invisible on
  upgrade. The hub treats them as non-durable until the block is written.
  Once `server.project_storage` places shared dirs on the volume, every
  start that finds a dir's old per-dir claim returns a warning naming it.
  The claim is never deleted; copying its contents is ptone/scion#5932.
- **Retired per-agent records**. An agent whose old record pinned a dir to
  another backend gets a one-time start warning, and the record is moved
  aside.
- **Volume backends** (cloudrun-volume, gke-shared-volume) are shared
  backends in projectfs. With `server.project_storage`, shared dirs follow
  the workspace onto the volume's uuid tree.
- **Environment**. `server.project_storage` takes the hub's
  `SCION_SERVER_*` overlay, as `workspace_storage` did. The deprecated
  `shared_dir_storage` and `home_storage` stay file-only.
- **Out of this phase**: homes on the share and Cloud Run home paths
  (ptone/scion#5825), detectRepoRoot from the Layout (ptone/scion#5826),
  the broker reading project configuration at the project root, the
  bootstrap re-target and the broker migration hook. A stacked
  phase-3b change owns these.

- **Layout identity covers shared-dir placement**: brokers report a
  transitional split's shared-dir backend, and the hub refuses a broker
  whose placement differs.

## Tests

- `pkg/config`: synthesis table, home options, views, `ProjectStorageLayout`,
  removed overrides, the hub loader (file, env, file-only legacy blocks),
  and the fail-closed mention check.
- `pkg/projectfs`: `DescriptorDiff` table, `Realize` (target unchanged for
  bind and pod mounts), `ProjectConfigEntry`, and shared/durable backends.
- `pkg/hub`: the shared-dir layout on project_storage (nfs, volumes,
  invalid configurations fail closed), the Cloud Run gate with no storage
  block, the volume tree, DELETE with the confined deleter, the
  symlink-planted leaf, the Required scratchpad, the dispatch precheck on
  every dispatch path, and the health summary mismatch.
- `pkg/agent`, `pkg/runtime`: Docker bind mounts from the export (with
  project_storage and with workspace_storage alone), local paths unchanged,
  one buildPod subPath branch, the legacy split, the fail-closed cases and
  the Cloud Run warning.
- `cmd`: hub startup resolution, the broker Layout and descriptor.
- `deploy`: the settings template check and the Helm render guards.

## Upstream merges

- **Upstream main e0f573bf (merge 0f9b081a, 2026-10-24).** The R274 check at
  a958db73 failed: upstream touched 17 of this change's files, and the docs
  security page conflicted. The merge resolves that one conflict by keeping
  upstream's two new bullets (CSP `frame-ancestors 'none'`, web page
  scripts) next to this change's NFS bullet. Every Go, config and schema
  file merged without conflict. The diff against the new base is line for
  line the same as the reviewed one.
- **Semantics checked:**
  - **#4020 (shared worktree teardown on hosted agent delete)** keys on the
    worktree-per-agent shared base and the sharer registry. This change
    doesn't move either, and the delete guard never reaches a project tree
    placed by `server.project_storage`.
  - **#4141 (settings papercuts)** routes settings YAML decodes through a
    panic-safe wrapper with the same decode result, so the
    `project_storage` block parses unchanged. This change's own two
    storage-key checks (`CheckStorageBlocksLoaded`,
    `GlobalSettingsStorageKey`) decoded the global settings file with raw
    yaml.v3, which panics even into `interface{}` on some malformed
    merge-key shapes. That would crash the hub at startup or a broker on
    agent Start. They now go through the same safe decoder, so such a file
    is a parse error (the first refuses when the file mentions a storage
    key; the second reports none). A test covers both.
  - **#4166** revokes delegation grants inside the project-delete
    transaction. That runs before the best-effort shared-dir cleanup, as
    before.
  - **#4156:** this change's file responses inherit the stricter sandbox
    CSP.
