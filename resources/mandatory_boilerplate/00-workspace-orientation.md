# Workspace Boundaries

Container isolation does not imply filesystem isolation. At task start, check
`pwd`, `SCION_WORKSPACE_MODE`, and `SCION_WORKSPACE_GIT`. The Git flag is `true`
for Git workspaces and otherwise unset. The mode describes the primary workspace;
named shared mounts have their own sharing boundary. If the mode is missing or
the facts contradict each other, report the mismatch before destructive Git
operations. Continue inspection without changing repository state.

- **`shared-plain`:** Files are shared. In a Git checkout, HEAD and the staging
  index are shared too. Agree on file ownership before overlapping edits, and
  coordinate exclusive access for staging and committing. Do not switch branches,
  reset, stash, clean, merge, or rebase while others are using that checkout.
- **`worktree-per-agent`:** Separate worktrees have their own files, HEAD, and
  index. Branches, tags, stash, and repository configuration are shared. Change
  only refs you own. If attached to an existing shared worktree, apply the
  `shared-plain` rules to that checkout.
- **`clone-per-agent`:** Files and local Git state are private. Remote branches
  and named shared mounts may still be shared.

Inspect other revisions with `git show` or `git diff`. Use task-owned temporary
storage for extracted snapshots. Preserve others' changes. Missing host paths in
Git metadata do not authorize pruning or repairing other worktrees.

Workspace mode does not determine network access or credentials; follow the
project's remote policy. Keep shared mounts, including `/scion-volumes/<name>` or
`.scion-volumes/<name>`, out of cleanup and commits.

With a host or remote Docker daemon, bind-mount sources refer to the daemon's
filesystem. Use verified host mappings with `--mount`, or transfer files with
`docker cp`. Container paths, socket paths, and `localhost` need not match the
daemon host's.
