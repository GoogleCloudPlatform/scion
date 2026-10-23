# Project storage migration and the Files tab notice

**Date**: 2026-10-23
**Issue**: ptone/scion#5819 (part of ptone/scion#5813, tracking ptone/scion#5805)

## Summary

The hub no longer keeps project files in its own legacy trees. Without a
migration, files that hubs on shared storage wrote to `hub-projects/<slug>`,
to their own `~/.scion/projects/<slug>`, or to hub-local
`project-configs/<slug>__<short>/shared-dirs` would disappear from the
Files tab. This change adds the one-shot maintenance migration
`project-storage-migrate` and a Files tab notice that shows until it has run.

- **Engine: `pkg/projectfs/migrate`.** A new leaf package. It imports only
  projectfs, shareddirs, api, projectkeys, yaml and x/sys.
  - `Move` implements the move rule.
  - `MigrateProject` and `Run` run the steps in this order: project
    configuration, then workspace trees, then hub-local shared dirs. A
    step-5 slot is left for the Cloud Run homes move.
  - `CleanObjects` and `LegacyObjectPrefix` handle the object-storage
    cleanup.
  - `LegacyHubProjectsRoot` holds the retired `hub-projects` name as a
    single constant, the only place the name survives (removed by
    ptone/scion#5839).

  All of the decision logic is tested without pkg/hub.
- **Hub.** `ProjectStorageMigrateExecutor` (`pkg/hub/project_storage_migrate.go`)
  gathers the hub's workspace and shared-dir Layouts, its projects and its
  object storage.
  - The migration is seeded in entadapter and listed in
    `rerunnableMigrations`.
  - `workspace_migration_notice.go` fills `migrationNotice` on the workspace
    listing. It reads a cached dry-run plan, computed in the background.
- **Provision.** `provision.WithWorkspaceProvisionLocks` takes every
  provisioning lock a provisioner of a shared workspace may take.
- **Web.** `scion-file-browser` renders `scion-workspace-migration-notice`
  when the listing carries the notice.

## Decisions

- **Placement never replaces or overwrites.**
  - A directory is renamed into its target only when the target is absent,
    and only with `renameat2(RENAME_NOREPLACE)`. Where the filesystem
    rejects that flag (the Linux NFS client rejects every rename flag), the
    directory is copied instead.
  - An existing target, even an empty one, may be a live bind or subPath
    mount source, so the migration copies into it rather than renaming
    over it.
  - Every file is written to a temp sibling (`.scion-migrate-tmp-<rand>`,
    O_EXCL, no-follow). The temp file gets its owner and mode, is fsynced,
    and is then placed with NOREPLACE, or with `linkat` + `unlinkat`. A
    killed run never leaves a partial file under a final name.
  - A copy walk removes leftover temp files: regular files with that prefix,
    owned by the migrator's own uid.
  - Each conflict is listed, and every source is kept as
    `<name>.migrated-<UTC ts>`. Before a source is renamed aside, the target
    filesystem is synced.
  - A source file identical to its target counts as identical, not as a
    conflict, so a re-run after a partial failure stays clean.
- **Symlinks and special files** in a source are never followed or copied.
  They are listed as refused. A symlink at any component of either path
  refuses the move. Every component is opened with O_NOFOLLOW.
- **Identity binding.** Legacy trees are found by slug. The old delete path
  removed only one of a project's possible roots, so a stale tree can belong
  to a deleted project whose slug was later reused.
  - A tree migrates only when the markers it carries name the project's ID.
    The markers are `.scion/project-id`, the `settings.yaml` project-ID keys
    (current and pre-rename, read through `pkg/projectkeys`), and, for the
    hub-local tree, the hub and broker workspace records.
  - A marker naming another ID is a mismatch, and the tree is never
    migrated.
  - A tree with no marker at all is skipped unless the run is given
    `adoptUnverified`: `"true"`, or a list of slugs.
  - Both cases are reported with the markers read and the IDs they name.
    The project stays pending.
  - The migration writes no workspace records; it only reads them.
- **Project configuration first.** The legacy `.scion` entries
  `settings.yaml`, `project-id`, `project-slug`, `templates/` and
  `harness-configs/` are copied to `<ProjectRoot>/.scion`. For a git
  checkout, only untracked entries are copied (`git ls-files --error-unmatch`,
  run with a hardened environment).
  - Broker records and any other entries are never copied there.
  - The entries are copied, not moved, so the legacy tree keeps its identity
    markers until its `.migrated` copy is made. That keeps an interrupted
    run verifiable when it resumes.
  - If this step fails or cannot decide (a git error, or `.git` is a file),
    nothing moves from that source into `workspace/`.
  - The hub's startup identity pass usually creates `<ProjectRoot>/.scion`
    first. This step is then a plain merge: an identical `project-id` is
    identical, and a differing `project-slug` is a kept conflict.
- **The project configuration is never visible to agents.** The workspace
  move never carries these into `workspace/`:
  - the `.scion` of a non-git source;
  - the entries step 4 copied from a git source's `.scion`;
  - step-4 backups;
  - `shared-dirs/`.

  They stay in the source's `.migrated` copy. A legacy tree holding a
  `.scion` but no workspace content is kept aside the same way.
- **Provisioning locks.** On NFS a workspace is always copied. Without a
  lock, a first agent start during the copy could adopt a partial checkout
  and mark it provisioned.
  - For a project with legacy content, the configuration and workspace steps
    hold every lock a provisioner of that workspace may take, for the whole
    copy: the project root (the hub-side Cloud Run provisioner) and
    `<ProjectRoot>/provision` (the Kubernetes init container).
  - The hub first creates `<ProjectRoot>/provision` with
    `shareddirs.EnsureLeaf`, the broker's leaf mode and ACL.
  - The locks are taken in a fixed order. No provisioner takes both, so the
    order cannot deadlock.
  - The copy runs under the locks' context. A lost lock stops it at the next
    entry. A stop caused by the run itself is reported as such.
  - Agent starts for the project wait on the migration and may fail cleanly
    while it runs. The report and the docs say so.
  - The migration never writes a provisioning sentinel.
- **`.git` is copied last.**
  - A workspace source's `.git` is copied into
    `workspace/.git.scion-migrating`. It is renamed to `.git` under the
    locks only when the copy is complete and conflict-free, and only while
    `.git` is absent.
  - A provisioner therefore sees either no `.git` (and refuses a non-empty
    workspace) or a complete one.
  - An interrupted run leaves only the temp dir, and a re-run resumes into
    it. A differing entry in the temp dir keeps the project pending.
  - An existing `.git`, from a broker that cloned first, is a conflict. The
    legacy `.git` is never merged into it.
  - A git project whose tree has no `.git` is reported: its first agent
    start will refuse the workspace until the operator acts.
  - Non-git projects: an interrupted copy leaves a partial tree that a first
    start can adopt. Nothing is lost, and the next run converges. An
    in-progress marker the provisioner honours is an optional follow-up
    (ptone/scion#5834).
- **Ownership.**
  - Migrated entries are chowned to the Layout's uid:gid. Hard-linked files
    in a renamed tree are skipped and reported.
  - Under shared dirs, directories get setgid 2775 with the default ACL and
    files get group rw.
  - A tree renamed into a workspace leaf gets the leaf's own mode and ACL.
  - EPERM (root squash) is reported as "ownership not normalised", with the
    fix-up hint, and is not a failure.
- **Co-located broker.** The hub-local `~/.scion/projects/<slug>` is only
  copied from, never renamed, in either of two cases: a broker in the hub
  process serves the project from it (per-project provider check), or its
  `.scion` holds anything besides the hub's own `settings.yaml`.
  `.scion/agents` and `shared-dirs` are excluded from the copy.
- **Preconditions.** The workspace steps run only on a shared workspace
  Layout. A shared shared-dir Layout alone still gets step 3. On a local hub
  the migration is a no-op.
- **Object-storage cleanup.**
  - It deletes only `[hubs/<hub>/]workspaces/<uuid>/<legacy dir>/`, and only
    when the project's workspace steps ran without failure and its workspace
    holds content. Lock litter, the temp `.git` dir and temp files do not
    count as content.
  - Per-agent sync prefixes are never touched.
  - The legacy directory name comes from
    `pkg/projectkeys.LegacyProjectWorkspaceObjectDir`.
- **Broker-local shared dirs** (each broker's own `project-configs`) are
  moved by the broker Layout work (ptone/scion#5823), which calls
  `migrate.Move`.
- **Notice.** The plan starts in the background at hub start, runs at most
  once at a time, is bounded at 2 minutes (checked per entry inside the
  walks), retries 5 minutes after a failure, and never runs on a request.
  It is recomputed when the Layouts change or after a real run. A project is
  pending when it has something to move or a failed step. Once the migration
  completes, the notice goes away.

## Tests

- `pkg/projectfs/migrate` (Linux), mover:
  - rename into an absent target;
  - copy into an existing empty target (inode kept);
  - merge never overwrites;
  - second run moves nothing;
  - dry run changes nothing;
  - symlinks and special files refused;
  - symlinked path components refused;
  - copy-only with excludes;
  - forced EINVAL (NFS): directories copied, files placed by link and never
    overwritten;
  - a target created between check and rename is not replaced;
  - cancellation;
  - owner applied to the temp file before placement;
  - temp-litter cleanup leaves directories and symlinks alone.
- `pkg/projectfs/migrate`, normalisation: chown set, shared-dir leaf
  contract, EPERM reported, hard links.
- `pkg/projectfs/migrate`, plan and run:
  - local backend no-op;
  - full NFS scenario with conflict, idempotence and object cleanup;
  - object cleanup kept on failure, on skipped workspace steps, and with no
    workspace content;
  - per-agent objects kept;
  - volume backends' legacy root, and fail-closed without a volume name;
  - git seed rule and seed-failure gating;
  - copy-only by evidence and by provider;
  - shared-dirs-only topology;
  - identity mismatch, unverified and per-slug adoption, pre-rename keys,
    workspace records, mixed markers;
  - no workspace records written;
  - hub-seeded target merge with no `.scion*` in `workspace/` (plain, EINVAL
    and EXDEV renames);
  - locks held for the whole move (a waiting provisioner sees only the
    complete tree);
  - lost lock; run stop;
  - `.git` copied last and renamed in; interrupted and resumed; a truncated
    temp file blocks the rename;
  - a killed copy leaves no partial final file;
  - a `.scion`-only tree is kept aside;
  - keep-aside negatives;
  - lock litter ignored.
- `pkg/provision`: lock-directory derivation; missing directories refused;
  while the locks are held, the real `acquireProvisionLock` fails for both
  the Cloud Run shape and the Kubernetes shape.
- `pkg/hub` (sharded): seeded and rerunnable; local no-op; NFS move,
  idempotence and locks released; object cleanup through `storage.NewLocal`;
  volume fail-closed; notice and the `migrationNotice` field; recompute
  after a run; co-located tree untouched; unverified tree, notice text and
  adoption.
- web (vitest): the banner shows, is absent without a notice, and clears on
  reload.

## Follow-ups

- The shared-dir tab has no notice; only the workspace listing carries one.
- The phase-6 cleanup (ptone/scion#5839) deletes `pkg/projectfs/migrate`,
  the projectkeys constants and their allowlist entry.
- Project deletion (ptone/scion#5835) should also remove legacy slug trees
  whose markers name the deleted project's ID.
- On NFS a migrated tree exists twice (target and `.migrated`) until the
  operator deletes the `.migrated` copy.
