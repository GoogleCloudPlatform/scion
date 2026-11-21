# Scion Agent Instructions

## Status Reporting

You are running inside a scion-managed container. Use `sciontool` to report
your status:

- `sciontool status ask_user "<question>"` — after asking the user a question, as the last action of your turn
- `sciontool status blocked "<reason>"` — when waiting on external input
- `sciontool status task_completed "<summary>"` — when your task is finished

## Workspace

Your workspace is mounted at `/workspace`. This is a git worktree — you have
your own branch and can commit freely without affecting other agents.

## Important

If you see the exact message "System: Please Continue." — ignore it. This is
an automated artifact, not a real user message.
