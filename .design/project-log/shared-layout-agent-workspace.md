# Agent-scoped workspace on a shared project Layout

Base: `7fc52cc7` (scion/fsstore-share-verify-fix). Packages: `pkg/agent` only.

## Problem

A project whose configuration is the hub's shared tree
(`<HostBase>/<subpath_root>/<uuid>/.scion`, `config.IsSharedProjectConfigDir`)
keeps every agent's broker-private state in the broker-local external root
`~/.scion/project-configs/<slug>__<id8>/.scion/agents/<agent>`
(`config.AgentsRootForProject`). The agent manager still derived the
workspace of an agent-scoped mode as `<agentDir>/workspace`. So on a
Kubernetes broker with an nfs Layout, a clone-per-agent (or empty-per-agent)
agent's workspace was a broker-local directory. The workspace source guard
refused it as outside the project's root.

The host-state record had the same root cause. `HostStateRootFor` returned the
share's project root for a non-shared-workspace agent, but the agent dir is in
the external root, so the run was not recorded.

## Change

- `sharedLayoutAgentWorkspace` (new, `pkg/agent/shared_agent_workspace.go`)
  returns `Layout.AgentRoot(projectID, agent)/workspace`. It only applies to a
  shared project configuration whose project storage Layout
  (`config.ResolveProjectStorage` + `config.ProjectStorageLayout`) is shared,
  and never to an agent whose state is its legacy in-project dir
  (`config.LegacySharedProjectAgentDir`). If the Layout's project root is not
  the directory holding the configuration, it is an error and the start is
  refused.
- Call sites that now use it for empty-per-agent and git-clone
  (clone-per-agent) agents:
  - `ProvisionAgent`
  - `GetAgent`, resolved after the stale-directory handling and again once
    the persisted empty-per-agent mode is known
  - `Start`, for the empty-per-agent workspace check
  - the `Reprovision` clone check
- On a shared Layout, provisioning never creates, clears or recreates that
  directory. Start's NFS leaf helpers create it with the share's modes, and
  the provisioning init container clones into it.
- `HostStateRootFor`: a shared project configuration's root is the root of the
  agent dir that `config.AgentDirForProject` resolves (the external root for
  every sharing mode, or the legacy dir). The record is written and read in
  the agent's own state dir.
- Unchanged:
  - `pkg/runtime/workspace_source_guard.go`;
  - non-shared Layouts;
  - shared-plain agents;
  - the legacy workspace admission;
  - the delete paths that act on the broker-local agent dir;
  - `isAgentOwnWorkspace`.

  On Kubernetes, the share's agent workspace is removed by
  `RemoveNFSAgentFiles`.

## Tests (`pkg/agent/shared_agent_workspace_test.go`)

- `TestStart_SharedLayoutClonePerAgentWorkspaceOnShare`
- `TestStart_SharedLayoutEmptyPerAgentWorkspaceOnShare`
- `TestStart_SharedLayoutSharedPlainUnaffected`
- `TestSharedLayoutAgentWorkspace`: shared, non-shared project, local Layout,
  legacy dir, mismatched Layout.
- `TestHostStateRootFor_SharedProjectConfig`
- `TestHostStateRootFor_SharedProjectConfigLegacyAgent`

## Notes

- On Kubernetes, a clone-per-agent agent's `RunConfig.Workspace` is still
  replaced by the nfs backend's `mount.HostPath` after the guard, as before.
  The pod mounts `agents/<slug>` by subPath (`NFSAgentDirName`). The
  empty-per-agent agent keeps the share path as its workspace source.
- The guard root for these projects is `resolveProjectRoot`: the project root
  on the share, as long as the hub-seeded `settings.yaml` sets no
  `workspace_path`.
