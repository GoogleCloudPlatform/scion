# fsstore P2-M2: project storage migration (M2/M3) and the Files tab notice

**Date**: 2026-10-23
**Issue**: ptone/scion#5819 (phase 2 of ptone/scion#5805, part of ptone/scion#5813)
**Design**: fs-cleanup design-v7 §Migration M2/M3, §P1a rule 4/5 (gs://scion-xproject-exchange/fs-cleanup/design-v7.md, sha256 ff4583c6…64a95e2)

## Summary

Phase 2 retires the hub's legacy project trees. Without a migration, files
that hubs on shared storage wrote to `hub-projects/<slug>`, to their own
`~/.scion/projects/<slug>`, or to hub-local
`project-configs/<slug>__<short>/shared-dirs` would disappear from the
Files tab. This adds the one-shot maintenance migration
`project-storage-migrate` and a Files tab notice that shows until it has run.

- **Engine: `pkg/projectfs/migrate`.** It is a new leaf package that imports
  only projectfs, shareddirs, api, projectkeys and x/sys. It contains:
  - `Move`: the M2 rule.
  - `MigrateProject` and `Run`: steps 4 → 1 → 2 → 3, plus an empty step-5
    slot for phase 3.
  - `CleanObjects` and `LegacyObjectPrefix`: M3.
  - `LegacyHubProjectsRoot`: the only place the retired `hub-projects` name
    survives, as one constant (the A2 exception).

  All of its decision logic is tested without pkg/hub.
- **Hub**: `ProjectStorageMigrateExecutor` (`pkg/hub/project_storage_migrate.go`)
  gathers the hub's workspace Layout (P2's `workspaceLayout`), its shared-dir
  Layout (P1's `sharedDirLayout`), the projects, and the object storage. The
  seed entry is in entadapter, and the migration is listed in
  `rerunnableMigrations`. `workspace_migration_notice.go` fills
  `migrationNotice` on the workspace listing from a cached background dry-run
  plan.
- **Web**: `scion-workspace-migration-notice` is rendered by
  `scion-file-browser` when the listing carries the notice.

## Decisions (fs-cleanup-arch rulings on ptone/scion#5819)

- **Rename only into an ABSENT target, and only atomically.** A directory
  is renamed with `renameat2(RENAME_NOREPLACE)`. Where the filesystem rejects
  the flag (the Linux NFS client rejects every rename flag), it is never
  renamed; it is copied into the EnsureLeaf-created target instead. A file is
  placed with NOREPLACE, or with `linkat` + `unlinkat`, never with a plain
  rename. A target created concurrently is never replaced or overwritten,
  and no lock is needed (review round 1, finding 1, which made the
  provisioning-lock approach and its `provision/` side effect unnecessary).
  An existing target, even an empty one, may be a live bind or subPath mount
  source, so the migration copies into it. Copying never overwrites
  (O_EXCL), lists each conflict, and keeps the source as
  `<name>.migrated-<UTC ts>`. A source file identical to the target counts as
  identical, not as a conflict, so a re-run after a partial failure stays
  clean.
- **Identity binding (Q8).** Legacy trees are found by slug. On delete, the
  old hub removed only the root its probe resolved, so a stale tree can
  belong to a deleted project whose slug was reused. A tree migrates only
  when the markers it carries name the project's ID: `.scion/project-id`, the
  `settings.yaml` project ID keys (current and pre-rename, read through
  `pkg/projectkeys`), and for the hub-local tree the
  hub-workspaces/broker-workspaces record.
  - A marker naming another ID is a MISMATCH: never migrated.
  - No marker at all is UNVERIFIED: skipped unless `adoptUnverified`
    (`"true"` or a list of slugs).

  Both outcomes are reported with the markers read and their IDs. The project
  stays pending, M3 keeps its objects, and the notice names the reason and
  the parameter.
- **Provisioning locks, no sentinel write** (fs-cleanup-arch 01:22Z).
  Review round 2 found that on NFS the workspace is always copied,
  `.git/` first. A first agent start during that copy would take
  "reusing prior clone" and write the sentinel over a partial checkout.
  - For a project with legacy content, steps 4/1/2 hold, copy included,
    every lock a provisioner of that workspace may take:
    `provision.WithWorkspaceProvisionLocks(workspace)`, which derives the
    dirs with ProvisionShared's own resolvers.
    - The project root is locked by the hub-side Cloud Run provisioner
      (no SentinelDir). Review round 3 found this one missing.
    - `<ProjectRoot>/provision` is locked by the Kubernetes init container.
      The hub creates it first with `shareddirs.EnsureLeaf`.
    - The locks are taken in that fixed order with
      `acquireOrderedFileLocks`. No provisioner takes both, so the order
      cannot deadlock.
  - The locks refuse missing directories and keep heartbeats.
  - The copy runs under the locks' context: a lost lock stops it at the
    next entry. A stop caused by the run itself (timeout, shutdown) is
    reported as such.
  - Agent starts wait on the migration and may fail cleanly; the report
    and docs say so.
  - History: the arch's ruling went lock → no lock (NOREPLACE makes renames
    safe) → lock (the copy window).
- **`.git` last, renamed in when complete** (fs-cleanup-arch ruling on
  review round 3, finding 3).
  - The top-level `.git` of a workspace source is copied last, into
    `workspace/.git.scion-migrating`. Once that copy is complete and
    `workspace/.git` is still absent, it is renamed to `.git` under the
    locks (a plain rename onto an absent name, which is atomic on NFS).
  - A provisioner therefore sees either no `.git`, and refuses a non-empty
    workspace, or a complete one.
  - An interrupted run leaves only the temp dir; a re-run resumes into it.
  - A broker's existing `.git` is a conflict: the legacy `.git` is not
    merged into it.
  - Non-git projects: a partial tree can be adopted by a first start.
    Nothing is lost, and the next run converges (accepted). An optional
    in-progress marker is a follow-up on #5834.
- **Crash-safe copies** (review round 4, finding 1).
  - `copyFile` writes a temp sibling (`.scion-migrate-tmp-<rand>`, O_EXCL,
    no-follow), fsyncs it, and places it with no-replace semantics
    (NOREPLACE, or linkat + unlink). A killed run never leaves a partial
    file under a final name.
  - The next run's copy walk removes leftover temp files (regular files
    with that prefix only). They never count as workspace content.
  - mergeGit renames `.git.scion-migrating` in only if its merge produced
    no conflict. Otherwise the project stays pending with ".git copy
    incomplete".
  - A legacy tree holding a `.scion` but no workspace content is kept aside
    (`.migrated-<ts>`) after step 4 copied its configuration (round 4,
    finding 2).
- **Step 4 copies, never moves:** the legacy tree keeps its identity
  markers until the workspace move completes and the whole source becomes
  `<name>.migrated-<ts>`. Moving them would make an interrupted run
  unverifiable on resume.
- **Step 4 include list** (01:22Z): `settings.yaml`, `project-id`,
  `project-slug`, `templates/` and `harness-configs/` are copied to
  `<ProjectRoot>/.scion`, for both the move and the copy-only variants.
  Broker records and other entries never do.
  - For a non-git source, whatever stays in `.scion` (and an emptied
    `.scion` is kept whole) is excluded from the workspace move. So are
    every step-4 backup (`.scion.migrated-*`, `.scion/*.migrated-*`) and,
    for git sources, the entries step 4 copied.
  - Those stay in the source's `.migrated` copy: P1a's "not visible to
    agents" (review round 2, finding 2).
  - The hub's startup identity pass usually creates `<ProjectRoot>/.scion`
    first. Step 4 is then a plain merge: an identical project-id is
    IDENTICAL, and a differing project-slug is a CONFLICT (kept).
- **Identity files:** step 4 does not write `project-id`/`project-slug`. The
  hub ensures them (one writer), so that write was removed.
- **Step 4 gates steps 1 and 2.** If the seed step fails or cannot decide
  (git error, `.git` is a file), nothing moves from that source into
  `workspace/`. git gets `-c safe.directory=<dir>`.
- **M3** runs only when the project's workspace steps actually ran
  (workspace Layout shared, nothing skipped, no content left in place) and
  the target workspace holds content (NB3): with neither, the object copy
  may be the only copy.
- **Symlinks** in a source are never followed or copied; they are listed under
  REFUSED, as are special files. A symlink at any component of either path
  refuses the move. Every component is walked with O_NOFOLLOW openat.
- **Step 4 runs before step 1.** A non-git source moves its whole `.scion` to
  `ProjectRoot/.scion`. A git source moves only `settings.yaml` and
  `project-id`, and only when `git ls-files --error-unmatch` reports them
  untracked. git runs with an explicit GIT_DIR, no system or global config,
  no fsmonitor or hooks, and a minimal environment. A `.git` that is a file
  leaves the seed in place.
- **Ownership (Q6).** Migrated entries are chowned to the Layout's uid:gid.
  Under shared dirs, directories get setgid 2775 with the EnsureLeaf default
  ACL and files get group rw. A tree renamed into a workspace leaf gets the
  leaf contract on its top directory. Workspace modes are otherwise kept.
  EPERM (root squash) is reported as "migrated; ownership not normalised",
  with the hybrid-tier fix-up hint, and is not treated as a failure.
- **Co-located broker (Q7).** The hub-local `~/.scion/projects/<slug>` is
  only copied from (never renamed; `.scion/agents` and `shared-dirs`
  excluded) in two cases:
  - the embedded broker serves the project from it (per-project provider
    check; a failed lookup counts as served);
  - its `.scion` holds anything besides the hub's own `settings.yaml`.
- **Precondition.** The workspace steps (1, 2, 4) run only on a shared
  workspace Layout. A shared shared-dir Layout alone still gets step 3. On a
  local hub the migration is a no-op.
- **M3** deletes only `[hubs/<hub>/]workspaces/<uuid>/<legacy dir>/`, and only
  when every M2 step of that project succeeded. A conflict is not a failure.
  The per-agent `workspaces/<uuid>/<agentID>/` prefixes (CLI bootstrap,
  `scion sync`) are never touched. The legacy directory name is routed
  through `pkg/projectkeys.LegacyProjectWorkspaceObjectDir`, which is
  allowlisted in the compat-literals guard.
- **Broker-local step 3** (shared dirs under the brokers' own project-configs)
  is phase 3 (#5823), which calls `migrate.Move`.
- **Notice.** The plan starts in the background at hub start, runs at
  most once at a time, and is bounded at 2 min (the context is checked per
  entry inside the walks),
  retries 5 min after a failure, and is never on the request path. It is
  recomputed when the Layouts change or after a real run. A project is
  pending when it has something to move or a failed step. Once the migration
  is completed, no notice is shown.

## Tests

- `pkg/projectfs/migrate` (LIGHT): mover tests:
  - rename into an absent target;
  - copy into an existing empty target (inode kept);
  - a target created before the locked re-check is copied into;
  - merge never overwrites;
  - second run moves nothing;
  - dry run changes nothing;
  - symlinks and special files refused;
  - symlinked source, source component, target and target component refused;
  - copy-only with excludes;
  - forced EINVAL (NFS): directories copied (target inode kept), files
    placed by link and never overwritten;
  - a target created between check and rename is not replaced;
  - a cancelled context stops a copy.

  Normalisation: chown set, shared-dir leaf contract, EPERM reported.
  Plan and run:
  - T1 no-op;
  - full NFS scenario with conflict, idempotence and M3;
  - M3 kept on failure;
  - per-agent objects kept;
  - volume backends' legacy root, and fail-closed without a volume name;
  - git seed rule;
  - empty-per-agent;
  - copy-only by evidence and by provider;
  - shared-dirs-only topology (M3 keeps the object);
  - a failed or undecidable seed step blocks the workspace move;
  - identity mismatch never migrates; unverified skipped and adopted per
    slug; a pre-rename-keyed seed verifies; the workspace record binds;
  - a shared-dirs/ subtree stays out of workspace/;
  - the broker cloned first, then the migration copies (conflicts listed,
    nothing overwritten, re-run 0); a git project without `.git` is reported;
  - a hub-seeded target: plain merge under plain, forced-EINVAL (NFS) and
    EXDEV renames, with no `.scion*` in workspace/; a git seed backup never
    reaches workspace/;
  - the moves hold the provisioning lock (a waiting provisioner sees only the
    complete workspace; `provision/` gets the leaf contract; no lock without
    legacy content); a lost lock stops before any write;
  - M3 keeps objects with no workspace content anywhere; mixed markers
    (the mismatch wins); hard-linked files are not chowned.
  - `.git` copied last and renamed in when complete; interrupted mid-`.git`
    leaves no `.git`, and a re-run resumes; run-stop versus lost-lock
    reporting; a copy of a hard-linked source is chowned; M3 ignores lock
    litter;
  - a truncated file in the temp `.git` blocks the rename (pending); a
    simulated kill mid-copy leaves no final-named partial file and the
    re-run cleans the litter; a `.scion`-only tree is kept aside and
    re-runs neither lock nor report conflicts.
- `pkg/provision`: `TestWorkspaceProvisionLockDirs`, `TestWithWorkspaceProvisionLocks`
  (refuses missing dirs), `TestWithWorkspaceProvisionLocks_ExcludesEveryProvisioner`
  (real `acquireProvisionLock` for the Cloud Run shape (no SentinelDir) and the
  Kubernetes shape (SentinelDir plus LegacyDir) cannot acquire while held).
- `pkg/hub` (sharded, lane): seeded and rerunnable; T1 no-op; NFS move and
  idempotence; M3 through `storage.NewLocal`; volume fail-closed; notice plus
  the `migrationNotice` field on GET workspace/files; recompute after a run;
  the co-located tree left untouched; unverified tree, notice text and
  per-slug adoption; adoptUnverified parsing.
- web (vitest): the banner shows, is absent without a notice, and clears on
  reload.

## Follow-ups

- If P2's seeding already created `ProjectRoot/.scion/settings.yaml` for a
  project, the legacy seed's settings become a conflict. The target is kept,
  and the legacy file stays in `.migrated`.
- The shared-dir tab has no notice; only the workspace listing carries one.
- Phase 6 (#5839) deletes `pkg/projectfs/migrate`, the projectkeys constants
  and their allowlist entry.
- P5 (#5835) `DeleteProject` should also remove legacy slug trees whose
  markers name the deleted ID (fs-cleanup-arch Q8 follow-on (a)).
- On NFS a migrated tree exists twice (target and `.migrated`) until the
  operator deletes the `.migrated` copy.
- Hard-linked files in a renamed tree are not chowned (reported).
