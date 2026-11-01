# Agent-scoped workspace on a shared project Layout

Base: `7fc52cc7`. Packages: `pkg/agent` only.

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
  returns `Layout.AgentRoot(projectID, agent)/workspace`. It applies only on
  the Kubernetes runtime (the runtime name the agent manager records in the
  context in `Start`, `Provision` and `Reprovision`; a caller that records
  none counts as another runtime), because only Kubernetes creates that
  directory (the NFS leaf helpers and the workspace init container). Every
  other runtime keeps the broker-local `<agentDir>/workspace` it had before.
  For clone-per-agent that workspace is outside the project's root on the
  share, so the workspace source guard refuses the start (fail closed, the
  same as before this change); empty-per-agent is admitted, because its own
  workspace is its guard root. Supporting clone-per-agent there (a
  provision-side create on the share with the share's ownership) is a
  follow-up. It only applies to a
  shared project configuration whose project storage Layout
  (`config.ResolveProjectStorage` + `config.ProjectStorageLayout`) is shared,
  and never to an agent whose state is its legacy in-project dir
  (`config.LegacySharedProjectAgentDir`). If the Layout's project root is not
  the directory holding the configuration, it is an error and the start is
  refused.
- Call sites that now use it for the agent-scoped modes, empty-per-agent and
  clone-per-agent. Clone-per-agent means a git clone whose request names the
  clone-per-agent sharing mode (`isClonePerAgentStart`, carried in the
  provisioning and start context). A worktree-per-agent dispatch also carries
  the git clone on Kubernetes, but it keeps `<agentDir>/workspace`:
  - `ProvisionAgent`
  - `GetAgent`, resolved after the stale-directory handling and again once
    the persisted empty-per-agent mode is known
  - `Start`, for the empty-per-agent workspace check
  - the `Reprovision` clone check
- On a shared Layout, provisioning never creates, clears or recreates that
  directory. Start's NFS leaf helpers create it with the share's modes, and
  the provisioning init container clones into it. So a leftover
  `agents/<name>/workspace` (for example after a failed `RemoveNFSAgentFiles`)
  is reused by a new agent with the same name, which is the documented NFS
  behaviour of `RemoveNFSAgentFiles`.
- `Start`'s workspace safety net (the "workspace directory does not exist"
  check, now `checkStartWorkspaceExists`) is skipped when a Kubernetes start
  goes through the nfs workspace backend (`startUsesNFSWorkspaceBackend`, the same
  `runtime.SelectWorkspaceBackend` choice Start makes later; the Kubernetes
  worktree adjustment does not change it). On nfs the workspace is created
  later in Start: the leaf helpers create it with their atomic claim or return
  an error, the Kubernetes init container creates it, or the start is refused
  (`errEmptyPerAgentNFSNoClaim`). A fresh agent-scoped agent's directory on
  the share therefore no longer fails that check before the leaf helper runs.
  The leaf is not created earlier, so the claim (`ownDirCreated`, the move
  rollback mark), `nfsKeepSharedCheckout` and the claim name from
  `backend.Realize` keep their order. Every other start keeps the check,
  including an nfs start on another runtime (Docker, Podman, Apple), where
  nothing creates the workspace and a bind mount of a missing path could
  create it with the wrong owner.
- `HostStateRootFor` is runtime-independent: the record lives in the agent's
  state dir, which on a shared project configuration is the broker-local
  external dir on every runtime, so on a non-Kubernetes runtime the record
  and the broker-local workspace are in the same agent dir.
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
- `TestIsClonePerAgentStart`: only a clone in the clone-per-agent mode is
  agent-scoped, in the options and in the provisioning context.
- `TestReprovision_SharedLayoutClonePerAgentChecksShare`: Reprovision checks
  the clone in the agent's directory on the share.
- `TestReprovision_SharedLayoutWorktreeKeepsOldWorkspace`: a worktree-per-agent
  dispatch on an nfs Layout keeps checking `<agentDir>/workspace`.
- `TestStart_SharedLayoutFreshAgentWorkspaceCreatedByLeafHelper`
  (clone-per-agent, empty-per-agent): with the agent's directory on the share
  absent, Start succeeds and the leaf helper creates it.
- `TestSharedLayoutAgentWorkspace`: Docker, Podman and no recorded runtime
  keep `<agentDir>/workspace`.
- `TestStart_SharedLayoutDockerClonePerAgentRefusedAsBefore`: a Docker clone-per-agent
  start on a shared nfs Layout keeps the broker-local workspace, places
  nothing on the share, and is refused by the workspace source guard ("is
  outside the permitted workspace root"), as before this change.
- `TestCheckStartWorkspaceExists`: a missing workspace is still refused off
  nfs and on non-Kubernetes runtimes on nfs, and skipped for Kubernetes on
  nfs.

## Notes

- On Kubernetes, a clone-per-agent agent's `RunConfig.Workspace` is still
  replaced by the nfs backend's `mount.HostPath` after the guard, as before.
  The pod mounts `agents/<slug>` by subPath (`NFSAgentDirName`). The
  empty-per-agent agent keeps the share path as its workspace source.
- The guard root for these projects is `resolveProjectRoot`: the project root
  on the share, as long as the hub-seeded `settings.yaml` sets no
  `workspace_path`.
- Settings: `Start` passes its resolved settings to `sharedLayoutAgentWorkspace`.
  `GetAgent` and `Reprovision` pass none, so the function loads them with
  `config.LoadEffectiveSettingsFor(ctx, projectDir)`, which is the same call
  `Start` makes for the same project directory. `Start` loads its settings
  after it calls `GetAgent`, so handing that copy to `GetAgent` would mean
  moving `Start`'s settings load ahead of `GetAgent`. That is left as a
  follow-up. Until then, a settings file changed between the two loads makes
  `Start`'s empty-per-agent workspace check refuse the start (fail closed).
- Docker (or any non-Kubernetes runtime) with a shared nfs Layout and the
  clone-per-agent mode is unsupported and fails closed, as before (the
  workspace source guard refuses the broker-local workspace); empty-per-agent
  is admitted. Support for clone-per-agent there is a follow-up.
