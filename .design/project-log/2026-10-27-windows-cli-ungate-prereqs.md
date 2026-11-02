# Windows CLI: prerequisites before un-gating delete, stop --rm and broker-run commands

**Date:** 2026-10-27
**Branch:** win-cli/ungate-prereqs

The Windows CLI refuses `delete`, `stop` (and so `stop --rm`) and every
command that reaches the pkg/runtime git bind-source code
(`cmd/platform_gate.go` `windowsAllowed`). This branch puts the
prerequisites for ever allowing them in place. `windowsAllowed` is
unchanged.

## Changes

1. **Agent file deletion returns every removal error**
   (`pkg/agent/provision.go` `deleteAgentFiles`). The removals that follow a
   failed `RemoveWorktree` (the worktree-per-agent worktree and the agent
   workspace) and the removal of a worktree-per-agent directory without
   `.git` return their error and stop the delete. On Windows that includes
   the `errors.ErrUnsupported` that `RemoveAllSafe` returns for a symlink.
   The other removals already returned theirs. `removeAllSafe` sits in the
   provision.go seam block with `removeWorktree`, `removeWorktreeEntry` and
   `deleteBranchByName`, so tests can inject failures (`setSeam`).
2. **The podman unshare retry is unix-only and narrow**
   (`pkg/agent/state_dir_removal_{unix,other}.go`). The external state
   directory removal is retried with `podman unshare rm -rf` (rootless
   podman: files owned by mapped subuids) only for `fs.ErrPermission`
   (EACCES, EPERM) or ENOTEMPTY, never for `errors.ErrUnsupported`. A
   failed retry is returned and wraps both errors. ENOTEMPTY is needed for
   the common rootless case: a mode-700 subuid-owned directory that is not
   empty cannot be listed, `RemoveAllSafe` skips the listing's EACCES, and
   the error it returns is the rmdir's ENOTEMPTY. Off unix the stub returns
   the removal error unchanged.
3. **The pkg/runtime test package builds for Windows.** Eight test files
   that use unix-only syscalls carry `//go:build unix`:
   `cloudrun_sandbox_delete_workaround`, `cloudrun_sandbox_diag`,
   `cloudrun_sandbox_runtime`, `cloudrun_sandbox_state_robust`,
   `home_ownership`, `resume_fallback`, `substrate_bootstrap_home` and
   `workspace_project_config_v2`. The helpers other files share moved
   unchanged into untagged files: `newWorkaroundTestRuntime` and
   `writeMockBin` (`cloudrun_sandbox_mock_bin_test.go`), `mockHarness`
   (`mock_harness_test.go`), and `fsSnapshot` and `assertNothingCreated`
   (`fs_snapshot_test.go`). `ran` and `GetCommand`, named in an earlier
   survey, were false matches: no other file uses them.
4. **Windows job coverage** (`.github/windows-test-packages.txt`).
   `./pkg/runtime` runs `TestWindowsGitBindSource_Unsupported` and
   `TestWindowsGitBindCallers_RegisterNoMount`, listed by exact name.
   `windows.yml` checks each name in `-run=^(A|B)$` with a fixed-string
   match, so a pattern such as `TestWindowsGitBindSource_.*` would fail
   the job. `./pkg/agent` adds
   `TestWindowsRetryRemovalInUserNamespace_ReturnsOriginal`, and `./cmd`
   adds `TestWindowsGate_RefusesUngatePrerequisiteCommands`. The Windows
   test binary of `./pkg/runtime` does not link pkg/hub.
5. **Guard test** (`cmd/platform_gate_test.go`,
   `TestWindowsGate_RefusesUngatePrerequisiteCommands`). Each of these
   commands stays out of `windowsAllowed`, is marked for root's hook and is
   refused when run, including through aliases and in every flag position:
   - `delete` (alias `rm`) and `stop` (with and without `--rm`): local
     agent file removal (`cleanupAfterHubDelete` -> `DeleteAgentFiles`).
   - `server start`, `server restart`, `runtime-broker start` and
     `runtime-broker restart`: the Runtime Broker in this process or in the
     daemon it starts, through `AgentManager.Start` -> `Runtime.Run` ->
     `buildCommonRunArgs` (the git bind sources).
   - `start` (alias `run`) and `resume`: agent launches the Hub dispatches
     to a Runtime Broker, which runs the same path.
   `project prune` reaches pkg/runtime (container stop) but not the bind
   sources. `hub link` reaches pkg/runtime List and container removal only.

## Notes

- Junctions on Windows are removed by path in `RemoveAllSafe` phase 2 (not
  followed). Re-verify this when un-gating `delete`.
- `ProvisionAgent` clears a stale workspace with `os.RemoveAll` (not
  `RemoveAllSafe`). It returns its error and is reachable only from
  commands the Windows gate refuses; review it before un-gating `start`.
- Still open: a per-command note, for each `windowsAllowed` command, of the
  exact pkg/runtime symbols it reaches.
