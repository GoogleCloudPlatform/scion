# Project storage phase 5a: project and agent deletion owned by the authority

**Date**: 2026-10-26
**Issues**: ptone/scion#5835 (delete side of the lifecycle phase ptone/scion#5833), tracking ptone/scion#5805
**Stacks on**: phase 3b (broker project root, forced-external agent state, legacy agent admission)

## Summary

Before this phase, a deleted project's storage on a shared backend had
several deleters with different guards. The hub removed `shared-dirs/` (all
shared backends) and, on `nfs` only, the whole `projects/<uuid>` tree. Every
provider broker removed `projects/<uuid>` again from its own mount, and
whichever ran second found nothing. The volume backends kept the uuid tree.
Nothing removed an agent's `agents/<slug>` directory (homes, secrets), an
agent's or project's object-storage prefix, or the per-dir shared-dir
claims on Kubernetes.

Now every tree has one deleter, and every deletion is one audit line
(`project storage deletion`, with `deleter`, `tree`, `project_id`, `path`,
`outcome`).

| Tree | Deleter | When |
|---|---|---|
| `projects/<uuid>` on every shared Layout (nfs, both volume backends, the shared-dir export of the transitional split) | hub, from its own mount | after the project delete commits; retried once on a part-way failure; kept if the project ID exists again |
| `hub-projects/<slug>` on the share, the hub's `~/.scion/projects/<slug>`, and their `.migrated-<ts>` copies | hub | same; only when the identity markers name the project (never by name alone) |
| `hubs/<hub>/workspaces/<uuid>/` | hub | same, when the hub has object storage |
| `agents/<slug>` on the share | hub | after a hard delete that deletes files, or the retained-files purge; only `home-<id>`/`secrets-<id>` while another agent record has the slug |
| `hubs/<hub>/workspaces/<uuid>/<agentID>/` | hub | same as `agents/<slug>` |
| the agent's own workspace or worktree on the share | broker (`RemoveNFSAgentFiles`, unchanged) | agent delete; needs the provisioning lock and git bookkeeping |
| broker `~/.scion/projects/<slug>`, `project-configs/<slug>__<id8>` | broker (unchanged) | provider cleanup |
| per-dir shared-dir PVCs | broker, through its Kubernetes runtimes | provider cleanup; only claims with the project-id label, a `scion.shared-dir` label and a `scion-shared-<...>-<dir>` name |
| broker-local agent state (forced-external root) and the retired legacy agent dir | broker (`pkg/agent` delete) | agent delete |

The local backend is unchanged: its slug-keyed trees keep their deleters.

## Pieces

- `shareddirs.DeleteTree(hostBase, rel)`: confined removal of a
  multi-component relative path. Every component is opened with
  `O_NOFOLLOW`, entries inside are unlinked and never followed, an absent
  component is nothing to do, and refusals are marked `ErrTreeRefused`.
- `projectfs.Layout.DeleteProject`, `DeleteAgent`, `DeleteAgentLeaves`
  return `[]projectfs.Deletion` for every shared Layout of l. They refuse an
  invalid ID, an nfs share ID that is not one plain element, and a symlinked
  component. `projectfs.Retryable` separates part-way failures from
  refusals.
- `projectfs.ProjectObjectPrefix`/`AgentObjectPrefix`/`DeleteObjectPrefix`
  build and delete exactly the prefix `storage.WorkspaceStoragePath` writes
  under. An unsafe element is an error, never a wider prefix.
- `projectfs.Audit`/`AuditAll`: the audit line.
- `migrate.DeleteLegacyProjectTrees`: the legacy slug trees, with the
  migration's identity binding (`treeIdentity`).
- `runtime.KubernetesRuntime.ReleaseProjectSharedDirClaims`: the claim
  selector. Deletes are preconditioned on the claim UID. Claims of the
  project's name that have no project-id label are returned as unlabelled.
- Hub: `startProjectStorageCleanup` (replaces the nfs-only tree cleanup and
  the deletion service's `shared-dirs` cleanup) and
  `startAgentStorageCleanup` (delete engine tail step, retained-files
  purge). On a shared backend the embedded broker's slug directory is no
  longer removed by name.
- Broker: the project tree cleanup and its agent-in-use snapshot are gone.
  Claims are released, and removals are audited.

## Legacy agent state retired (phase 3b's temporary admissions)

- No look-up resolves an agent to its legacy in-project directory
  `~/.scion/projects/<slug>/.scion/agents/<name>` (`AgentDirForProject`,
  `GetAgentDir`, `ResolveAgentDir` return the external root).
- The legacy workspace admission is removed. Starting an agent whose only
  state is the legacy directory is refused with
  `ErrLegacyAgentStateRetired`, before any side effect, and its old
  workspace stays intact until the agent is deleted.
- Agent delete still finds and removes the legacy directory. It removes
  the agent's state in the forced-external root, with or without a Hub
  project ID, through the external agents root (now covered by a test).

## Verification

See the PR's acceptance criteria section for the test names. Non-hub
packages: vet and the tests of `pkg/shareddirs`, `pkg/projectfs/...`,
`pkg/config`, and scoped tests of `pkg/runtime`, `pkg/agent`,
`pkg/runtimebroker`. pkg/hub is compiled and tested only in the gate.

## Follow-ups

- Claims of a deleted project that predate the project-id label are listed,
  not deleted.
- A git project linked at a local path on a Kubernetes broker gets no
  provider cleanup, so its per-dir claims are not released (unchanged
  dispatch rule `brokerUsesSlugProjectDir`).
