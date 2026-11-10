# Expected shared layout on existing upper directories (NFS walk warning)

Base: `155bd8b5`. Packages: `pkg/shareddirs`, `pkg/provision`, `pkg/runtime`, `pkg/agent`, `pkg/runtimebroker`. Follow-up to ptone/scion#7224.

## Problem

`warnIfIntermediateUnsafe` (`pkg/shareddirs/walk_unix.go`) logs a
once-per-process warning for an existing upper directory that is group- or
other-writable or not owned by the broker. A correctly set-up share has
`2775` setgid upper directories owned by the share group, so the warning
fired on every broker start.

## Change

- **Second quiet case.** The existing quiet case (not group/other-writable
  and owned by the broker) stays. An upper directory is also accepted when it
  is not other-writable (sticky or not), has setgid, has the configured nfs
  gid as its group, and is owned by the broker or the configured nfs uid
  (`hasExpectedSharedLayout`). A zero expected gid never matches. Accepted
  directories are counted and logged at Debug, once per walk, count only.
- **Check-only owner.** `EnsureLeafExpecting(hostBase, rel, expected)`
  creates exactly as `EnsureLeaf` does and chowns nothing; `expected` only
  feeds the check. `EnsureLeafOwned` checks against its own owner. The agent
  helpers do not use `EnsureLeafOwned`, which would `fchown` new leaves when
  running as root.
- **Plumbing.** `provision.ResolvedWorkspace` has `OwnerUID`/`OwnerGID`,
  filled by `nfsBackend.Resolve` from the projectfs resolution (Layout UID and
  GID from `EffectiveWorkspaceStorage().NFS`, so `project_storage.nfs` or the
  deprecated `workspace_storage.nfs`), defaulted with
  `provision.DefaultOwnerID`. The agent's `ensureNFSLeaf`/`ensureNFSLeafClaim`
  take the expected owner. Workspace and worktree leaves read it from
  `resolved`; the removal path gets it as an added return of
  `nfsExportWorkspaceOnAnyRuntime`. The expected owner always comes from
  configuration, never from the directory being checked.
- **Broker state leaf**: `ensureWorkspaceAndStateLeaves`
  walks the provisioning-state leaf with `EnsureLeafExpecting(owner)`. Without
  this, the state walk (zero owner) right after the workspace walk would fire
  the warning on brokers.
- Docs: one line in `reference/server-config.md` (NFS shared dirs section).

## Tests

- `TestHasExpectedSharedLayout`: synthetic `unix.Stat_t` table (no
  filesystem, geteuid seam), so it does not depend on the host's ids or on
  the kernel keeping a setgid bit. It pins the zero-gid guard ("gid 0 with
  the zero Owner") and the zero-uid guard ("uid 0 with expected uid 0").
- `TestWarnIfIntermediateUnsafe_ExpectedSharedLayout` (filesystem table,
  includes a wrong-gid row for each accepted owner).
- `TestEnsureLeafExpecting_ExpectedLayoutThroughRealWalk`,
  `TestNFSResolve_Owner`, and the configured-owner tests for each plumbed
  path: `TestEnsureNFSWorkspaceLeaf_ConfiguredShareOwnerReachesWalk`,
  `TestEnsureNFSWorktreeLeaf_ConfiguredShareOwnerReachesWalk`,
  `TestRemoveNFSWorktree_ConfiguredShareOwnerReachesWalk`,
  `TestEnsureWorkspaceAndStateLeaves_ExpectedLayoutNotWarned`.
- Walk-warning assertions match the walk's own message, so the
  once-per-process ACL warning on a filesystem without POSIX ACLs is not
  counted.
- Filesystem tests give directories a non-zero owner and group (chowned to
  4243:4242 when the test runs as root), because a zero expected gid never
  matches.

## Follow-ups

These callers still pass no expected owner, so the warning can still fire on
their walks over a correctly set-up share:

- `pkg/hub/handlers_projects_core.go` (provisioning-state leaf, `EnsureLeaf`)
- `pkg/hub/project_storage_migrate_unix.go` (`EnsureLeaf`)
- `pkg/runtimebroker/shared_dirs_migrate_hook_unix.go` (`EnsureLeaf`)
- `pkg/agent/home_storage_leaf_linux.go` (`EnsureLeaf`)
- `pkg/projectfs/open_unix.go` (`EnsurePrivateDir`)

Accepted limit: members of the share group can replace an upper directory
with another real directory between the walk and a later use (described in
ptone/scion#7224).
