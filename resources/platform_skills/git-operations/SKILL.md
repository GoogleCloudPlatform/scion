---
name: git-operations
description: >-
  Git safety and operational guidance for Scion agents. Covers working tree
  resets, rebase pitfalls, and general git behavior that agents should know
  regardless of workspace mode. For workspace-mode-specific git rules, see
  the git-sandbox skill.
---

# Git Operations

General git safety rules for Scion agents. These apply regardless of
workspace sharing mode — for mode-specific guidance (worktree isolation,
shared-plain index hazards, clone-per-agent remotes), see the `git-sandbox`
skill.

## Working Tree Reset Safety

When cleaning a working tree, **use `git clean -fd`, not `git clean -fdx`**.

- `git clean -fd` removes untracked files but **respects `.gitignore`** — `.scion/`, agent state, and ignored directories survive.
- `git clean -fdx` deliberately defeats `.gitignore` and **deletes everything** not tracked by git, including `.scion/`, `downloads/`, and any local state.

The `-x` flag is not a stronger clean — it is a different operation. Default to `-fd`; use `-x` only with a specific reason and after verifying nothing irreplaceable is in an ignored directory.

**`downloads/` is an inbox, not storage.** Files downloaded into your container are visible only inside it and invisible to every other agent. Move anything worth keeping to `/scion-volumes/scratchpad/` (shared across all agents) promptly. A `downloads/` file that has not been drained to the scratchpad is one command from gone.

## Rebase After Deletion

**When a PR deletes a file, a rebase will tell you about edits to that file and will not tell you about new references to it.**

A concurrent edit to the deleted file raises a modify/delete conflict and halts the rebase — that case is handled, because it demands a decision. A concurrent edit that adds a *reference* to the deleted file **from a different file** produces no conflict at all: git sees two disjoint changes, reports "Successfully rebased," and leaves a dangling reference.

After rebasing a deletion, **grep for the deleted name**. Any inbound-reference count taken before the rebase is stale by construction.
