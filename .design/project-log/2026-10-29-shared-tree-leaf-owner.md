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
  - `Owner{UID, GID}` names the owner a root creator gives the leaf
    directories it creates.
  - `EnsureLeafOwned` gives the leaf the walk itself creates (its own `mkdirat` succeeded) that owner, with `fchown` on
    the descriptor the walk opened, before the leaf's mode and default ACL.
  - This applies only when the process runs as root and the owner names a
    non-root uid and gid.
- **Intermediates stay as before.** `<subpath_root>`, `<project uuid>`,
  `shared-dirs` and `agents/<slug>` keep the creator's owner and mode 2755,
  because agents only traverse them. An agent-owned upper directory would let
  one project's agents rename or unlink other projects' trees. An existing
  intermediate without other-execute is warned about and never changed.
- **Unchanged directories:**
  - a leaf that already existed, or that a concurrent creator made first;
  - anything behind a symlinked component, which the existing `O_NOFOLLOW`
    walk refuses before anything past it is created.
- **Failure:** a failed chown of a leaf this call created removes the leaf,
  as a failed chmod does. The intermediate-rollback concern does not arise:
  intermediates are never chowned, and a failed intermediate chmod behaves as
  before.
- `EnsureLeaf` is the zero-owner form and is unchanged. `EnsurePrivateDir`
  always keeps the creator's ownership; it has no owned form.
- **The project-root trust directories keep the zero owner:**
  - the project configuration directory `<ProjectRoot>/.scion`
    (`EnsurePrivateDir`), which holds the project's identity (project-id,
    project-slug);
  - the provisioning state directory `<ProjectRoot>/provision`, which holds
    the provisioning locks and sentinel.

  Only Scion components write them, and they must never be agent-writable.
  Only the leaves agents use are given the owner.
- **`pkg/projectfs`:** a workspace or shared-dir Mount resolved on a shared
  backend carries the Layout's `UID` and `GID` (`OwnerUID`, `OwnerGID`,
  `Owner()`), and `Resolved.Open(create)` gives the leaf it creates that owner.
  `ProjectRoot`, `ProjectConfig` and `AgentRoot` carry the zero owner, so
  opening them never chowns.
- **Call sites passing the Layout's owner (agent-used leaves only):**
  - shared-dir leaves: `pkg/agent` `resolveSharedDirs` and `pkg/runtime`
    `ResolveSharedDirHostPath`;
  - the broker's shared bootstrap and shared-plain workspace leaves, through
    `ensureWorkspaceAndStateLeaves` in `pkg/runtimebroker`, which keeps their
    provisioning state directory at the zero owner.

  The hub's and the shared-dir migration's provisioning state directories keep
  `EnsureLeaf`, unchanged.

  `ProvisionShared` keeps setting the workspace content's ownership as
  before. It does not chown the leaf a second time.
- **`pkg/config`:** the transitional shared-dir block
  (`LegacySharedDirsNFS`) gets the nfs uid and gid defaults (1000), as the
  workspace block does, so its Layout names an owner.
- **Backends:** the affected deployment uses the nfs backend, whose
  Layout carries the nfs ids. The volume backends (cloudrun-volume,
  gke-shared-volume) carry none and are unchanged.
- **Unchanged:**
  - a non-root creator;
  - Layouts without a uid and gid;
  - nfs agent homes;
  - the hub migration's provisioning lock directory;
  - the Kubernetes NFS workspace, agent and worktree leaves. Their pod runs as
    the container uid, and the provisioning init container chowns them to its
    RunAsUser and fsGroup. A follow-up remains: under a pre-created workspace
    (best-effort init chown) on a root-squash export, the broker should give
    such a leaf setgid and group write for the fsGroup.

## Tests

- `pkg/shareddirs` (`walk_owner_unix_test.go`, with the `geteuid`,
  `fchownDir` and `mkdirat` seams, so they run without root):
  - a root creator chowns only the created leaf; created intermediates are
    not nfs-owned and keep 2755; the leaf keeps 2775;
  - a pre-existing leaf is unchanged;
  - a non-root creator, or a missing owner, is unchanged;
  - a symlinked intermediate is refused and not chowned;
  - a chown failure removes the leaf;
  - a concurrent creator that wins the race is not chowned;
  - an untraversable existing intermediate is left unchanged;
- `TestEnsureLeafOwned_RealRoot` checks the real uid and gid, including root
  intermediates. It runs only as root and skips in CI, which runs
  unprivileged: `sudo go test -run TestEnsureLeafOwned_RealRoot
  ./pkg/shareddirs/`.
- `pkg/projectfs`:
  - `TestResolvedOwner`;
  - `TestEnsureProjectConfigDir_RootCreatorKeepsZeroOwner`: `.scion` is not
    nfs-owned;
  - `TestOpenCreate_RootCreatorTrustDirsNotChowned`: opening the project root
    or the project configuration directory chowns nothing;
  - `TestOpenCreate_RootCreatorOwnsSharedDirLeaf`.
- `pkg/agent` `TestResolveSharedDirs_NFS_LeafOwnerAndAgentGroup`.
- `pkg/runtimebroker`
  `TestEnsureWorkspaceAndStateLeaves_StateDirKeepsZeroOwner`: the workspace
  leaf is owned and the provisioning state directory is not.
- `pkg/config` `TestSynthesizeProjectStorage_LegacySharedDirsOwnerDefaults`.
