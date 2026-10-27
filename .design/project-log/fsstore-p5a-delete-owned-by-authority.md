# Project storage phase 5a: project and agent deletion owned by the authority

**Date**: 2026-10-26
**Issues**: ptone/scion#5835 (delete side of the lifecycle phase ptone/scion#5833), tracking ptone/scion#5805
**Stacks on**: phase 3b (broker project root, forced-external agent state, legacy agent admission)

## Summary

Before this phase, a deleted project's storage on a shared backend had
several deleters with different guards. The hub removed `shared-dirs/` (all
shared backends) and, on `nfs` only, the whole `projects/<uuid>` tree. Every
provider broker then removed `projects/<uuid>` again from its own mount, and
whichever ran second found nothing. The volume backends kept the uuid tree.
Nothing removed an agent's homes and secrets on the share, an agent's or a
project's object-storage prefix, or the per-dir shared-dir claims on
Kubernetes.

Now every tree has one deleter, and every deletion is one audit line
(`project storage deletion`, with `deleter`, `tree`, `project_id`, `path`
and `outcome`).

| Tree | Deleter | When |
|---|---|---|
| `projects/<uuid>` on every shared Layout (nfs, both volume backends, the shared-dir export of the transitional split) | hub, from its own mount | After the project delete commits. Retried once on a part-way failure, then by the maintenance pass. Kept if the project ID exists again. A forced delete with agent records defers it (below). |
| `hub-projects/<slug>` on the share, the hub's `~/.scion/projects/<slug>`, and their `.migrated-<ts>` copies | hub | Same time. Only when the identity markers name the project (never by name alone). A copy is identified by its own markers only. |
| `hubs/<hub>/workspaces/<uuid>/` | hub | Same time, when the hub has object storage. |
| `agents/<slug>/home-<id>`, `secrets-<id>`, then `agents/<slug>` only when it is empty | hub | After a hard delete that deletes files, or after the retained-files purge. A non-empty directory is kept and audited (workspace pending broker removal). |
| `hubs/<hub>/workspaces/<uuid>/<agentID>/` | hub | Same as the agent's leaves. |
| the agent's own workspace or worktree on the share | broker (`RemoveNFSAgentFiles`, unchanged) | Agent delete. Needs the provisioning lock and git bookkeeping. The hub never removes it. |
| broker `~/.scion/projects/<slug>`, `project-configs/<slug>__<id8>` | broker (unchanged) | Provider cleanup. |
| per-dir shared-dir PVCs | broker, through its Kubernetes runtimes | Provider cleanup. Only claims with the project-id label, a `scion.shared-dir` label and a `scion-shared-<...>-<dir>` name that no existing pod references. |
| broker-local agent state, and the legacy agent dir of a pre-move agent | broker (`pkg/agent` delete) | Agent delete. |

The local backend is unchanged: its slug-keyed trees keep their deleters.

## Forced project deletes

A forced delete skips the agent guard, so agents may still run on their
brokers. When it finds agent records, the hub removes nothing of the tree.
It writes `projects/<uuid>/provision/.scion-delete-after` (an RFC 3339 time:
now plus the soft-delete retention when that is over 24 h, otherwise now
plus 24 h), through the confined resolver, outside every agent Mount. The
maintenance pass on the purge tick lists the tombstones and acts on each:

- after the time: it removes the tree, the legacy trees and the object
  prefix;
- if the project exists again: it clears the tombstone and keeps the tree.

The same tombstone, written with the current time, lets the pass retry a
non-forced removal that still failed after its retry. A forced delete with
no agent records removes the tree at once.

## Visibility of a missing mount

A shared Layout whose mount point has no subpath root is treated as not
mounted. In that case:

- the deletion is an ERROR audit line (`share not mounted`), never `absent`;
- the maintenance pass logs one ERROR per pass;
- the hub health reports `project_storage_tree` as degraded (a non-critical
  check, bounded by the health timeout).

## Pieces

- `shareddirs.DeleteTree(hostBase, rel)` is the confined removal of a
  multi-component relative path:
  - every component is opened with `O_NOFOLLOW`;
  - entries inside are unlinked, never followed;
  - an absent component is nothing to do;
  - refusals are marked `ErrTreeRefused`.
- More confined shareddirs primitives:
  - `RemoveDirIfEmpty`;
  - `WriteFileInTree`, `ReadFileInTree`, `RemoveFileInTree`,
    `ListDirInTree` and `DirExistsInTree`.
- `shareddirs.DeleteProjectTree` (the old shared-dirs-only project
  cleanup) is removed.
- `projectfs.Layout` deletion methods:
  - `RemoveProjectTree` covers every shared Layout and refuses a missing
    mount;
  - `RemoveAgentStorage(projectID, slug, agentID)` removes the agent's own leaves,
    then the agent directory only when it is empty;
  - `MarkDeleteAfter`, `DeferredDeletions` and `ClearDeleteAfter` handle
    the tombstone;
  - `CheckTreesMounted` checks the mounts;
  - `projectfs.Retryable` separates part-way failures from refusals.
- `projectfs.ProjectObjectPrefix`, `AgentObjectPrefix` and
  `DeleteObjectPrefix` build and delete exactly the prefix
  `storage.WorkspaceStoragePath` writes under.
- `projectfs.Audit` and `AuditAll` write the audit line.
- `migrate.DeleteLegacyProjectTrees` removes the legacy slug trees, using
  the migration's identity binding.
- `runtime.KubernetesRuntime.ReleaseProjectSharedDirClaims` applies the
  claim selector. It skips claims a pod references, preconditions deletes
  on the claim UID, and lists unlabelled claims. `ensureProjectRWXClaim` no
  longer reuses a claim labelled for another project.
- Hub:
  - `startProjectStorageCleanup` replaces the nfs-only tree cleanup and the
    deletion service's `shared-dirs` cleanup;
  - `sweepDeferredProjectDeletions` is the maintenance pass;
  - `startAgentStorageCleanup` runs from the delete engine, with a
    teardown-confirmed flag, and from the retained-files purge;
  - on a shared backend the embedded broker's slug directory is no longer
    removed by name.
- Broker: the project tree cleanup and its agent-in-use snapshot are gone.
  Claims are released, and removals are audited.

## Legacy agent state (phase 3b's temporary admissions)

The retirement is per agent:

- The read-only legacy look-up and the narrow legacy workspace admission
  stay for agents that still exist, so a legacy agent can still be started
  to push its work.
- Deleting the agent removes its legacy directory (and its forced-external
  state), which ends the fallback for that agent.
- A create of a new agent over a legacy directory that no agent owns is
  refused (`ErrLegacyAgentStateRetired`, an agent-state conflict that
  brokers answer with 409 and a remedy).
- The code paths are removed at a release boundary (ptone/scion#5839),
  with an upgrade note asking users to push the work of such agents and
  recreate them.

## Known limitations

Claims of a deleted project are left in place, never deleted wrongly, in
these cases:

- claims that predate the project-id label (listed in the broker log, not
  deleted);
- claims in a namespace chosen by a `scion.namespace` or `namespace` agent
  label;
- claims in the namespace of a Kubernetes profile the broker has not
  resolved since it started;
- claims of the hub's embedded broker;
- claims of a git project linked at a local path on a broker (no provider
  cleanup for it).

## Verification

See the PR's acceptance criteria section for the test names. The non-hub
packages pass vet and their tests. The pkg/hub tests run in CI's shards.
