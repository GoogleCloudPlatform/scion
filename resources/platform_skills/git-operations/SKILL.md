---
name: git-operations
description: >-
  Git safety and operational guidance for Scion agents. Covers working tree
  resets, shallow clones, rebase pitfalls, and general git behavior that agents
  should know regardless of workspace mode. For workspace-mode-specific git
  rules, see the git-sandbox skill.
---

# Git Operations

General git safety rules for Scion agents. These apply regardless of
workspace sharing mode — for local worktree sandbox rules, see the
`git-sandbox` skill.

## Shallow Clones

In-container per-agent clones (`SCION_GIT_DEPTH` set — `clone-per-agent`, or a `worktree-per-agent` start that fell back to cloning) default to a shallow clone of depth 1 (`SCION_GIT_DEPTH=1`; `N` means depth `N`, and `0` means a full clone). `SCION_GIT_DEPTH` reflects the initial clone setting; run `git rev-parse --is-shallow-repository` to check whether the live checkout is still shallow.

**Symptoms of a shallow clone:**
- `git diff main...HEAD` and `git merge-base` fail with `no merge base` (or return empty output) when the branch point is older than the clone depth.
- Two-dot ranges (`git log a..b`) walk to the shallow graft and list hundreds of unrelated commits.
- `git log -S`, `git blame`, and tag comparisons cannot see commits past the shallow boundary; `git merge`/`git pull` fail with `refusing to merge unrelated histories`, and `git rebase` replays the grafted root commit and conflicts.

**How to handle history and range operations (when remote access permits):**
- **Reach a merge base quickly:** `git fetch --deepen=50 origin main` (increase `<N>` until `git merge-base` succeeds).
- **Inspect deeper commit/tree history without downloading all blobs:** `git fetch --filter=blob:none --deepen=200 <remote> <branch>`.
- **Fetch complete history (for full `git log -S`, `blame`, or `rebase`):** `git fetch --unshallow`.
- **Diff against an explicit base commit:** when your brief names the base SHA and only a diff is needed, fetch that commit if missing and run `git diff <base-sha> HEAD`.

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
