# Project storage phase 2: workspace Files tab on the authority; retire hub-projects; delete the GCS workspace sync and the linked-project cache

**Date**: 2026-10-23
**Issue**: ptone/scion#5813 (sub-issues ptone/scion#5814, ptone/scion#5815, ptone/scion#5816, ptone/scion#5818; the storage migration, ptone/scion#5819, is in `fsstore-p2-m2-project-storage-migrate.md`; tracking ptone/scion#5805)

## Summary

On the Cloud Run hub + GKE + Filestore deployment, a web upload to a
project's workspace landed in `<share>/hub-projects/<slug>`, a slug-keyed hub
copy that no agent mounts; agents use `<share>/projects/<uuid>/workspace`. A
content probe picked between that copy and the hub's local disk on every
request, and a pre-NFS GCS model copied workspaces between the hub and
remote brokers on agent create and stop.

Changes:

- **Hub workspace Layout** (`pkg/hub/workspace_layout.go`): built from the
  in-memory `workspace_storage` (nfs, cloudrun-volume, gke-shared-volume, or
  local). `projectWorkspaceResolved` resolves the workspace Mount through
  `projectfs.Layout.Resolve`: the uuid tree on a shared backend,
  `~/.scion/projects/<slug>` on the local backend (workstation unchanged). An
  empty-per-agent project gets `ErrNoMount` (Files tab 409). A shared backend
  the hub cannot build a Layout for fails closed (503), never local disk.
- **Files tab, archive, pull** open the workspace through `Resolved.Open`
  (the anchored O_NOFOLLOW walk on shared backends). The write gate checks the
  resolved Mount's backend. Linked projects keep the embedded provider's
  path; a linked project served only by remote brokers gets 409 (no cache).
- **pkg/projectfs** (additive): `BackendCloudRunVolume`,
  `BackendGKESharedVolume` (shared uuid tree, durable), `Backend.Shared`,
  `Layout.ProjectConfig`, `Layout.EnsureProjectConfigDir`;
  `shareddirs.EnsurePrivateDir` (2755, no ACL).
- **Project configuration seed** (hub side): on a shared backend
  project create seeds `projects/<uuid>/.scion` (settings with the hub
  connection), beside `workspace/` and in no Mount, plus the identity files
  `project-id` and `project-slug` (the hub is their single writer: at create,
  on slug rename, and in a bounded pass at hub start; a differing
  `project-id` is reported by the `project_config_identity` health check,
  never overwritten). Cloned-identity alignment runs only on the local
  backend.
- **Shared workspace provisioning at create**: on a shared backend the hub provisions a shared-workspace git
  project's uuid workspace through `provision.ProvisionShared` with the k8s
  init container's layout (same locks, same sentinel in
  `<ProjectRoot>/provision`), so Docker/co-located brokers and pods take the
  reuse path. New `provision.ProvisionInput.CloneToken`.
- **Resource import / pull** read the agent-writable workspace through
  confined roots (import works on a private snapshot copy); pull errors are
  fixed text.
- **Deleted**: `hubManagedProjectPath` (method and package-level),
  `resolveDurableOrLegacyPath`, `probeWorkspaceContent` and its in-flight
  map, `volumeBackedProjectPath`, `hubManagedProjectsRoots`, the ephemeral
  path warning; the upload-on-create of hub-managed workspaces (incl. the
  ptone/scion#3765 412); `syncWorkspaceOnStop`, `syncHubManagedWorkspaceBack`,
  `syncHubWorkspaceFromGCS`; `recordHubWorkspace` and `hub-workspaces/`;
  `storage.ProjectWorkspaceStoragePath`; `project_cache.go` (cache refresh,
  status, notify, `sync/status`), its hubclient methods and the broker's
  `/api/v1/workspace/project-upload`.
- **Kept**: `scion sync to|from <agent>`, the broker
  `/api/v1/workspace/{upload,apply}`, `pkg/transfer`, and the CLI workspace
  bootstrap of a non-git checkout (`sync-to/finalize` → broker
  `resolveGCSWorkspaceDir`).
- **Remote broker without shared storage**: a hub-managed or shared-workspace project dispatched to a
  remote broker that has no local path for it and does not report the hub's
  shared workspace storage is refused with 409 before anything is dispatched,
  except a linked non-git project on a local-backend hub (the broker keeps
  the workspace in its own project directory). "Linked" is the server-owned
  project label `scion.dev/project-origin=linked`, set only by register,
  refused on create and register, immutable on update, and not copied by
  clone.
- **Bootstrap guard**: a project-level bootstrap whose
  workspace already existed at admission is not applied; the create answers
  with a warning pointing at `scion sync to`.

## Decisions

- No code names the retired `hub-projects` directory except the storage
  migration's single constant `legacyHubProjectsDir` and its tests; its
  removal is tracked on ptone/scion#5839.
- The volume backends join projectfs as shared uuid-tree backends; shared
  dirs on those hubs stay local until the broker phase (ptone/scion#5823).
- empty-per-agent → 409; worktree-per-agent keeps linked resolution (the
  base checkout is not exposed; UI question on ptone/scion#5841).
- The CLI bootstrap is kept; its destination on shared-backend brokers and
  a sentinel-based guard are tracked on ptone/scion#5823.
- Brokers reading the project configuration at the project root, and the
  broker record keyed by it, are in the broker phase (ptone/scion#5823,
  `ProjectPath := ProjectRoot(uuid)`): no shared-backend broker ever read
  the hub's old seed, so this phase strands nothing.
- A remote broker on the shared storage is not given the hub's mount path
  (it resolves the workspace through its own mount); the co-located broker
  gets the uuid path, which the hub creates before dispatch.

## Known gaps / follow-ups

- Volume backends: the project's uuid tree is not removed on project delete
  by the hub (nfs is, by the ID-keyed tree cleanup); lifecycle phase
  (ptone/scion#5833).
- `project_sync_states` table and store methods are now unused (only the
  deletion service clears rows); drop with ptone/scion#5839.
- Broker backstop for shared-plain provisioning, and asynchronous project
  provisioning (large clones at create), are recorded on ptone/scion#5823.
- An NFS hang on the Files tab path now blocks the request (the probe's 2 s
  timeout is gone with the probe); health still reports mount availability.
- The CLI bootstrap still downloads into the broker-local slug dir on
  shared-backend brokers (ptone/scion#5823).

## Tests

pkg/projectfs volume backends; pkg/hub `project_workspace_layout_test.go`
(uuid tree on nfs and volume backends, unchanged local path, 409s, fail
closed, seed at ProjectRoot, no hub clone, alignment skipped, no
migrationNotice by default), `remote_broker_workspace_test.go` (remote broker refusal), rewritten
delete-dir and embedded-cleanup tests; pkg/runtimebroker existing-workspace
bootstrap test; shared-dir storage loading tests (ptone/scion#5881).
