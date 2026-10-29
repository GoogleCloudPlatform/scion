# Owner of directories a root creator makes on a shared project tree

Base: `7fc52cc7`. Packages: `pkg/shareddirs`, `pkg/projectfs`, `pkg/agent`, `pkg/runtime`, `pkg/runtimebroker`, `pkg/hub`.

## Problem

A hub and a co-located broker that run as root create project and
shared-dir directories on the share with `shareddirs.EnsureLeaf`. The walk
created them with the creator's uid and gid and never changed the owner. It
relied on a setgid parent for the group. When `<export>/<subpath_root>` is
not setgid with the nfs gid, new leaves became `root:root 2775`. The broker
then (correctly) does not give agents group 0 as a supplemental group, so
agents running as the nfs uid could not write shared dirs.

## Change

- **`pkg/shareddirs`:**
  - `Owner{UID, GID}` names the owner a root creator gives the directories it
    creates.
  - `EnsureLeafOwned` and `EnsurePrivateDirOwned` give every directory the
    walk creates itself (its own `mkdirat` succeeded) that owner, with
    `fchown` on the descriptor the walk opened, before the modes and the
    default ACL.
  - This applies only when the process runs as root and the owner names a
    non-root uid and gid. A component that already existed, or that a
    concurrent creator made first, is never changed.
  - A symlinked component is refused by the existing `O_NOFOLLOW` walk before
    anything past it is created or chowned.
  - A failed chown of a leaf this call created removes the leaf, as a failed
    chmod does.
  - `EnsureLeaf` and `EnsurePrivateDir` are the zero-owner forms and are
    unchanged.
- **`pkg/projectfs`:** `Resolved` carries the Layout's `UID` and `GID`
  (`OwnerUID`, `OwnerGID`, `Owner()`). `Resolved.Open(create)` and
  `Layout.EnsureProjectConfigDir` use the owned walks, so the hub file
  browser, the hub-native workspace leaf, project configuration directories
  and migration leaves get the owner.
- **Call sites passing the Layout's owner:**
  - shared-dir leaves: `pkg/agent` `resolveSharedDirs` and `pkg/runtime`
    `ResolveSharedDirHostPath`;
  - the broker's shared bootstrap and shared-plain workspace leaves, and their
    provisioning state directories (`pkg/runtimebroker`);
  - the shared-dir migration state directory;
  - the hub's provisioning state directory at shared-workspace provisioning.

  `ProvisionShared` keeps setting the workspace content's ownership as
  before. It does not chown the leaf a second time.
- **Unchanged:**
  - a non-root creator;
  - Layouts without a uid and gid;
  - existing directories;
  - the Kubernetes NFS workspace leaf helpers, whose provisioning init
    container sets ownership;
  - nfs agent homes;
  - the hub migration's provisioning lock directory.

## Tests

- `pkg/shareddirs` (`walk_owner_unix_test.go`, with the `geteuid` and
  `fchownDir` seams, so they run without root):
  - a root creator chowns each created intermediate and the leaf, and the
    leaf keeps 2775;
  - pre-existing directories are unchanged;
  - a non-root creator, or a missing owner, is unchanged;
  - a symlinked intermediate is refused and not chowned;
  - a chown failure removes the leaf;
  - `EnsurePrivateDirOwned` follows the same rule.
- `TestEnsureLeafOwned_RealRoot` checks the real uid and gid. It runs only as
  root and skips in CI, which runs unprivileged:
  `sudo go test -run TestEnsureLeafOwned_RealRoot ./pkg/shareddirs/`.
- `pkg/projectfs` `TestResolvedOwner`: a Layout's ids travel with
  `Resolved`.
- `pkg/agent` `TestResolveSharedDirs_NFS_LeafOwnerAndAgentGroup`: the
  Layout's nfs uid and gid reach the leaf walk. A leaf in the nfs gid becomes
  the agent's supplemental group, and a root-group leaf does not.
