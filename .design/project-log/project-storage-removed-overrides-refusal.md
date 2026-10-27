# Removed per-profile storage overrides: startup reports, agent start refuses

**Date**: 2026-10-27
**Issue**: ptone/scion#6371 (a regression from project storage phase 3a, ptone/scion#5820)
**Design**: the project storage cleanup design tracked in ptone/scion#5805 (§P2 and migration M1: the removed override keys are rejected with a message that names the replacement)

## Problem

Before `server.project_storage`, a profile or runtime entry could choose
shared-dir and home storage with `shared_dir_storage_backend`,
`shared_dir_storage_backends`, `home_storage_backend` and
`home_storage_leaf`. A common setup kept `server.shared_dir_storage` on
the local backend with an nfs block, and set
`profiles.<p>.shared_dir_storage_backend: nfs` on the Kubernetes profile.
Its shared dirs were mounted from the nfs claim (`shares[0].pv_name`) by
subPath.

Phase 3a removed the keys, and the docs said they "refuse startup".
They were rejected only by `scion config validate`, by admin settings
saves and by flat-instance loading. Hub start, broker start, the
co-located settings overlay and agent start never checked them, so the
runtime silently ignored them. With the override ignored, the shared-dir
layout was local, and the Kubernetes runtime created one ReadWriteMany
claim per shared dir with no storage class. The cluster's default class
captured those claims; when that class cannot provide RWX they stayed
Pending, and the agents could not start. The existing data on the export
was no longer mounted.

## Root cause: why phase 3a's tests and review missed it

- The rejection was asserted only at the function level: the removed-override
  tests called `RemovedStorageOverrideErrors` directly. No test loaded a
  settings file with a per-profile override through a path that runs agents.
- One phase 3a test (`TestStartSharedDirLayout_ProfileOverrideIgnored`)
  asserted the silent ignore as intended behaviour. It is now
  `..._ProfileOverrideRefused`.
- The synthesis tests covered only the four `server.*` blocks. The
  per-profile resolver and its startup log line were deleted in the same
  change, so nothing reported the keys any more.
- The Kubernetes tests build pods and claims from hand-made `RunConfig`s,
  never from loaded settings. No test asserted at the claim level what a
  legacy configuration produces.
- The hub's stored runtimes and profiles (the settings overlay a
  co-located broker reads) are a second source that no validation path
  checks at load.

## Fix

Per the architect's ruling, this fixes the gap without re-adding the
feature. There is no translation and no storage-class change.

- **Agent start refuses.** `checkStartStorageRefusals` (pkg/agent) runs
  before any side effect. It refuses a start whose profile, or that
  profile's runtime entry, carries a removed key. It also refuses an agent
  whose per-agent shared-dir backend record from an earlier version pins
  nfs while the layout is not shared. That covers the live
  `shared-dir-storage.json` and the `.retired` copy that phase 3a's first
  start moved aside; the record is kept. The record is broker-local (in
  the agent's state directory, never mounted into the container) and is
  read bounded: regular file only, `O_NOFOLLOW|O_NONBLOCK`, at most 64 KiB.
  A record that cannot be read safely fails the start closed. Both cases are a
  `config.StorageMigrationRequiredError`. The broker answers it with 422
  `storage_migration_required` on create, start, restart and the async
  launch, and the hub relays it with the code intact. The public text
  names the keys and the migration. The log text adds the claim and
  subpath root that decide where the data is. Agents of other profiles
  start normally.
- **Startup reports.** Hub, broker and flat-host start, and the overlay
  install, log an ERROR naming each key, where it was found and the
  migration. The process keeps running: overlay keys are repaired through
  the admin API, which needs a running hub. The hub reports the
  non-critical `storage_overrides` health check (live from the stored
  settings, so it clears after a save) and lists the messages in
  `storage_override_errors` on `GET /api/v1/admin/server-config`. The
  broker reports `storage_overrides` on `/healthz`.
- **Migration text.** Set `server.shared_dir_storage.backend: nfs`
  (transitional; workspaces stay put), or write `server.project_storage`
  with `backend: nfs`. Check `shares[0].pv_name` and `subpath_root`
  against the live export, because they decide where the data is. Keep
  `server` and `export` accurate. Then run `scion config validate`.
- **Operator steps that exist.** The startup warnings said "run the
  project-storage migration", but no CLI command does that. The migration
  is the hub's `project-storage-migrate` maintenance migration, which
  moves the hub's own trees. Each broker moves its own node-local shared
  dirs on its first start with its shared-dir layout on the share (the
  phase 3b start hook). One pinned constant,
  `config.ProjectStorageMigrationText`, now names both exactly and is used
  by every warning and doc line. A test rejects texts that name
  nonexistent steps.

## Tests

- Upgrade regression (`pkg/runtime`). A legacy per-profile nfs settings
  file, and a variant with hub-stored profiles, goes through the real
  loader and the agent-start resolution into Kubernetes claim creation on
  a fake clientset. Before the fix it fails, creating default-class RWX
  claims. After the fix the start is refused with no claim, and the
  unaffected profile is admitted.
- `pkg/agent`:
  - the profile chain (profile, runtime entry, hub overlay), unaffected
    and active profiles;
  - the record-pin table (record kept);
  - `Manager.Start` refused with zero cluster API actions.
- `pkg/config`:
  - every key and place;
  - the remedy follows the keys;
  - the public text carries no claim;
  - the per-profile chain and the flat view;
  - the pinned migration text, and no warning names a nonexistent step.
- `pkg/runtimebroker`: 422 on create, start and restart; the async launch
  code; `/healthz` degraded and its cache.
- `pkg/hub`:
  - the relay table gains the new 422 code;
  - the health check and admin field clear after the stored overrides are
    removed;
  - the file error is reported.
- `cmd`: the startup ERROR names the key, the claim and the migration, and
  the broker check.

## Deferred

- A storage-class or static-PV option for per-dir claims on the local
  backend is a separate feature, planned in the Kubernetes runtime lane.
- Auto-translation of the removed keys was rejected (design OQ2-A).
- A web label for the launch failure code `storage_migration_required`
  (`web/src/shared/agent-launch-labels.ts`); until then the web shows the
  code with "_" as spaces.
