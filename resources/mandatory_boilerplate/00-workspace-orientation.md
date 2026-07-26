# Your Workspace

Your working directory is `/workspace`. Two environment variables describe how it
is provisioned:

| Variable | Value |
|---|---|
| `SCION_WORKSPACE_MODE` | `shared-plain`, `clone-per-agent`, or `worktree-per-agent`. Always set. |
| `SCION_WORKSPACE_GIT` | `true` when `/workspace` is a git repository. Absent otherwise — test for presence, not for the string `false`. |

Check both before your first write to `/workspace`.

- **`shared-plain`** — every agent in the project shares this one directory. Your
  edits are immediately visible to others, and others may change the same files
  while you work. Use `scion message` to coordinate before broad or structural
  changes, and do not assume a file is unchanged between reading it and writing it.
- **`worktree-per-agent`** — your working tree is private, but the underlying clone
  is shared: history, refs, and branch names are common to all agents in the
  project. Commit freely; treat branch names as a shared namespace.
- **`clone-per-agent`** — your clone is entirely your own. Nothing you do to the
  working tree or to local refs affects another agent.

Shared directories are independent of workspace mode. When a project defines them,
each is mounted either at `/scion-volumes/<name>` or, when configured in-workspace,
at `/workspace/.scion-volumes/<name>` — one location per directory, not both.
**Shared directories are shared in every workspace mode, including the isolated
ones.** Treat them as concurrent-access storage regardless of
`SCION_WORKSPACE_MODE`.
