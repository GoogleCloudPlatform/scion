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
  not in the DB, project settings cannot set it, and `config set` cannot
  write it.
- **Synthesis (M1)**. Without the block, `SynthesizeProjectStorage` builds
  it from the deprecated blocks and keeps today's placement of every data
  class. A hub with `shared_dir_storage: nfs` but no nfs workspace keeps
  its shared dirs on that export through a transitional split
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

- **Shared-dir declaration DELETE** deletes the leaf on every shared
  backend through the confined deleter (`shareddirs.DeleteSharedDir`), as
  on the local backend. Phase 1 kept the leaf when the layout came from
  `workspace_storage`. With one block there is one rule. While the hub
  default makes the scratchpad Required, removing its declaration is
  refused with 409.
- **Cloud Run agent runtimes** cannot mount the shared tree. They keep
  node-local shared dirs on a shared backend, as they do today on
  `workspace_storage: nfs` hubs, and the start returns a user-visible
  warning. This is a capability gap of the runtime type, not a setting,
  and is tracked in ptone/scion#5926. Only the legacy split is refused.
  Refusing every Cloud Run agent would have been an outage, because every
  project has the Required scratchpad.
- **Volume backends** (cloudrun-volume, gke-shared-volume) are shared
  backends in projectfs, so shared dirs follow the workspace onto the
  volume's uuid tree.
- **Environment**. `server.project_storage` takes the hub's
  `SCION_SERVER_*` overlay, as `workspace_storage` did. The deprecated
  `shared_dir_storage` and `home_storage` stay file-only.
- **Out of this phase**: homes on the share and Cloud Run home paths
  (ptone/scion#5825), detectRepoRoot from the Layout (ptone/scion#5826),
  the broker reading project configuration at the project root, the
  bootstrap re-target and the broker migration hook. A stacked
  phase-3b change owns these.

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
