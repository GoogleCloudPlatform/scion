# Project storage phase 5b: one Provisioner, lock-litter GC on every open, workspace-root lock and marker dropped

**Date**: 2026-10-27
**Issues**: ptone/scion#5834 and ptone/scion#5836 (lifecycle phase ptone/scion#5833), tracking ptone/scion#5805
**Stacks on**: phase 5a (deletion owned by the authority)

## Summary

Before this phase, the provisioning sentinel and lock had two homes. The
Kubernetes init container kept them in the project's provisioning state
directory (`<subpath_root>/<project-id>/provision`), but every provisioner
also took a second lock in the workspace root, still read a sentinel left
there by older images, and added it to the checkout's git excludes. An init
container without the state-directory mount kept both in the workspace.
Leaf creation was spread over the hub and the brokers, each calling
`shareddirs.EnsureLeaf` directly, and `POST /shared-dirs` created nothing.

Now:

- The state directory is the only place the sentinel and lock live.
  Nothing reads, writes, locks or git-excludes anything in the workspace
  root, except a one-shot promotion step at hub start.
- Every Scion-owned directory is created through one Provisioner in
  `pkg/projectfs`.
- Lock litter is swept on every open of the state directory.

## Pieces

- `pkg/provision`:
  - `ProvisionInput.LegacyDir` is removed, with the ordered legacy-then-state
    lock pair and `excludeLegacySentinel`;
  - `SentinelDirs` is the sentinel directory alone;
  - `ProvisionShared` and `RemoveMountedWorktree` refuse a sentinel
    directory that is the workspace or inside it;
  - `RemoveMountedWorktree` takes only the state-directory lock (`""`
    means `ProjectStateDir(base)`);
  - a sentinel or lock entry in the workspace root is workspace content now
    (the clone refuses such a workspace, naming the entries);
  - `garbageCollectLockLitter` also runs on the shared-plain pre-lock fast
    path, and lists before probing the server clock, so an open with
    nothing to sweep writes nothing;
  - `PromoteLegacyMarker` is the promotion step (`legacy_promote.go`, the
    only file allowed to name the workspace-root marker and lock; a scan
    test over `pkg/`, `cmd/` and `extras/` enforces that).
- `sciontool provision`: shared-plain and worktree-per-agent fail without
  `SCION_PROVISION_STATE_DIR` ("this pod has no provisioning state
  directory; the runtime broker must be upgraded"), before anything is
  created.
- `pkg/projectfs` (store-free leaf primitives):
  - `EnsureSharedLeaf` is the leaf primitive (the confined walk, mode 2775,
    default ACL);
  - `Resolved.EnsureLeaf` uses it on a shared backend, and on the local
    backend creates the directory as before (0755);
  - `Layout.ProvisionStateDir` resolves `<ProjectRoot>/provision`, and
    `Layout.ProjectTreeIDs` lists the project trees.
- `pkg/projectfs/provisioner.Provisioner`:
  - `EnsureLeaf` (over `Resolved.EnsureLeaf`);
  - `OpenStateDir` creates and checks the state directory and sweeps it;
  - `ProvisionSharedWorkspace` is the shared-plain provisioning of both the
    hub (at project create) and the broker (the backstop before a local
    container starts).
- Why the Provisioner is a separate package: `pkg/config`, and through it
  the conduit relay, imports `pkg/projectfs`, and the relay must never reach
  `pkg/store` (`TestRelayCodeUsesOnlyRegistryStore`). `pkg/provision` imports
  `pkg/store`, so what needs it lives in `pkg/projectfs/provisioner`, and
  `pkg/projectfs` keeps depending only on `pkg/api` and `pkg/shareddirs`.
  The first CI run caught a version that had the Provisioner in
  `pkg/projectfs`.
- Hub:
  - `provisionSharedWorkspace`, the workspace leaf in `populateAgentConfig`
    and the shared-dir write path go through the Provisioner;
  - `POST /shared-dirs` creates the leaf where the hub can resolve it (best
    effort; first use still creates it otherwise);
  - `startLegacyProvisionPromotion` runs the promotion in the background at
    every start, with one log line per project it changes or fails on and a
    summary line.
- Broker and agent manager:
  - the backstop uses the Provisioner;
  - `nfsSharedCheckoutProvisioned`, `worktreeBaseIsProvisioned` and
    `sharedWorkspaceProvisioned` read the state directory only;
  - a worktree removal that cannot use the state directory leaves the
    worktree in place;
  - every leaf creation calls `projectfs.EnsureSharedLeaf`.

## The promotion step

The step runs for every project tree on the hub's shared workspace Layout.
It acts on a workspace with a workspace-root marker, its exclude line, or
workspace-root lock litter. Under the project's provisioning locks it:

1. writes the state-directory sentinel when only the workspace-root marker
   (a regular file) says the workspace is provisioned;
2. removes the marker and its `/.scion-provisioned` exclude line;
3. removes workspace-root lock litter older than twice the stale threshold,
   probing the server clock in the state directory.

A live workspace-root lock is never removed. A tree with nothing to promote
is skipped without a lock or a write, so a second run changes nothing. For
worktree-per-agent projects with live worktrees, the next start reuses the
base: no clone and no recursive chown.

## Upgrade notes (documented in `hosted/ha/kubernetes.md`)

- Agent images from before the provisioning state directory provision again
  on every Pod start and must be rebuilt.
- Pods built by a broker without state-directory support fail provisioning
  until the broker is upgraded.
- Workspace-root entries the promotion keeps (a live lock, or entries an
  older image writes again) block a first clone until removed.

## Not changed

- The non-git create-time bootstrap (`resolveGCSWorkspaceDir`,
  `applySharedBootstrap`, `AppliedConfig.WorkspaceStoragePath`) stays, per
  design v9. Its "workspace in use" check now reads the state-directory
  sentinel only.
- `config.EnsureSharedDirs` (local backend, CLI) still creates directories
  itself, with the same mode.

## Verification

See the PR's acceptance criteria section for the test names.

## Merge-forward onto phase 5a's final head (2026-10-27)

P5b moved from its original base (phase 5a at eb51fb5a4) to phase 5a's
merge-forward head 607bfcfd. That head is phase 5a plus phase 3b e3e8208c,
plus main 807e67c5.

Conflicts: main split code into `_unix`/`_other` files for the Windows build
of `./cmd/scion`, and three P5b edits sat in moved code:
`ensureNFSLeafClaim` (now in `nfs_leaf_unix.go`), `nfsRemovalStateDir` (now
in `nfs_removal_state_unix.go`) and the migration's state-directory leaf (now
in `project_storage_migrate_unix.go`). Each was resolved by taking
upstream's split and re-applying the P5b change in the moved code.

Windows build: the same pattern covers P5b's own new code.
- `pkg/projectfs/leaf_other.go` makes `EnsureSharedLeaf`,
  `Resolved.EnsureLeaf` and `Layout.ProjectTreeIDs` fail with
  `errors.ErrUnsupported`.
- `Layout.ProvisionStateDir` and `provisionDir` move to files built on every
  platform.
- `pkg/projectfs/provisioner` builds everywhere; its methods fail with
  `ErrUnsupported` off unix.

The no-compile checks are recorded with the merge: the Windows identifier
and method scans, `GOOS=windows go list` over `./cmd/scion` and the Windows
test package list, the duplicate-identifier scan, and gofmt.

## Bounded reads of agent-writable files in the promotion step

The workspace root is agent-writable, so the promotion must not trust what
it finds there.
- The workspace's `.git/info/exclude` is read only through `openGitExclude`
  (each component opened with `O_NOFOLLOW`, `O_NONBLOCK`, and a regular-file
  check on the opened descriptor). A file above 1 MiB is skipped with a
  warning, and the read itself goes through a limited reader. A symlink, a
  FIFO or a device is never followed or read.
- The workspace root is listed in chunks, and only lock-litter names are
  kept, at most 4096 per pass (`lockLitterEntries`, shared with the
  recurring state-directory sweep). A root with very many entries is never
  held in memory as a whole.

## Merge with main after phase 5a landed (2026-11-03)

Phase 5a reached main as one squashed commit, while this branch carried
phase 5a's history as merges. The branch was brought onto main by a merge
commit, never a rebase (`0babe834`, main at `84bf9deb`). Files this branch
never changed take main's version; checks found no leftover of the older
phase 5a history anywhere else. Main had moved on in four areas that touch
this phase, and the merge carries each of them:

- **Workspace-root lock re-added on main.** Main's newer code passed the
  removed workspace-root lock directory again: `sciontool provision`, the
  hub's provisioning at create (for git projects) and the broker's
  shared-plain backstop. All three are removed in the merge, so the state
  directory stays the only home of the sentinel and lock.
- **Clone token bound to the recorded remote.** Main offers the clone token
  only when the clone URL names the project's recorded remote. This phase's
  Provisioner predated that rule and passed the clone URL as the remote. The
  hub and the broker now compute the gated token and remote before calling
  the Provisioner, which passes them through. The hub's refusal of an ssh
  URL without a project credential now runs before any filesystem work.
- **Leaf owner.** Main gives a root creator's new Mount leaves the Layout's
  owner. The single leaf primitive gained `projectfs.EnsureSharedLeafOwned`,
  and `Resolved.EnsureLeaf` uses the owner the Layout assigns, as
  `Resolved.Open` does. The provisioning state directory keeps the zero
  owner. No code outside `pkg/shareddirs` and `pkg/projectfs` calls the
  `shareddirs` leaf functions directly.
- **One lock-litter sweep.** Main added an exported `SweepLockLitter` for
  agent directories. It is now the only implementation, over this phase's
  bounded listing; the promotion step and the state-directory sweep on open
  call it.

**Behaviour change, signed-URL bootstrap.** A hub-managed shared workspace
that holds only a provisioning lock or sentinel left in the workspace root
by an older image is now treated as existing content: the bootstrap skips
it and writes nothing into it, where main failed it as busy. This follows
from the state directory being the only place provisioning bookkeeping
lives. A test checks that the leaf's listing is byte-identical before and
after. The earlier "busy" test keeps its fixture, now asserts the skip, and
is renamed `TestSignedCaseB_ArtifactsOnlyLeafIsExistingContent` so that its
name matches what it asserts.

**Sharer registry at the cap.** A worktree sharer registry of exactly 4096
markers (or 65536 entries) was reported as truncated, so finding or
unregistering a sharer failed closed although everything had been read.
Truncation is now reported only for a marker or an entry beyond a cap. The
error names the registry directory and asks for stale markers to be removed
(`2a6bdc69`).

Recorded with the merge: the per-file resolution, the security-relevant
hunks, and the no-compile checks (conflict markers, gofmt, the
workspace-root marker scan emulated over the tree, the duplicate-identifier
scan in four platform contexts, a secret-shape scan). The compile and test
gate follows.
