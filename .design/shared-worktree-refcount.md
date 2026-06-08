# Design: Shared-Worktree Refcount / Last-Sharer Teardown (#168, Q7)

**Branch:** `scion/shared-worktree-refcount` (off upstream `main`)
**Tracking:** #168 (child of #158). Q7 in `worktree-per-agent.md`.
**Status:** proposal — decisions teed up for @ptone.

## Problem (from #168)
Shared worktrees (N agents on one branch) use an **implicit owner model with no refcount**:
- **Local mode** already supports sharing: a *joiner* created with `--branch <b>` (when `<b>`
  already has a worktree) gets the owner's worktree bind-mounted (`provision.go:422-428`,
  `run.go:758-763`); the joiner's own dir holds no `.git`.
- **Bug:** deleting the **owner** runs `RemoveWorktree` (+ branch) **out from under live
  joiners**. Deleting a joiner is already safe (no `.git` at its path).
- **Hub-managed mode (Phase 1):** a 2nd agent with `--branch <existing>` does **not** join —
  `ensureWorktree` hits "already checked out" and the broker falls back to clone-per-agent
  (`provision.go:442`, `start_context.go`). So hub has no sharing yet.

## Goal
A real **last-sharer teardown**: a shared worktree (and its branch) is removed only when the
final mounting agent exits. Apply uniformly to **local + hub-managed**.

## Decisions — RESOLVED 2026-06-08 (ptone)
- **D1 = marker file** in the shared base (no schema migration; unified local + hub).
- **D2 = project owns the worktree** (ownerless; last sharer tears down).
- **D3 = include hub-join** (a 2nd `--branch` agent attaches to the existing worktree).

## Decisions (original options, for reference; recommendations in **bold**)

### D1 — How to track the agent→worktree association
- **(A) Refs marker file in the shared base** — a small file per worktree (e.g.
  `<base>/.git/worktrees/<id>/scion-sharers` or `<base>/worktrees/.sharers/<branch>`) listing
  sharer agent IDs; append on attach, remove on delete, teardown when empty. **Unified for
  local + hub** (both have the base on disk), no schema migration, naturally co-located with
  the worktree. Concurrency via the existing per-project advisory lock / provision mutex.
- (B) store/ent schema (a sharers table) — durable + queryable, but DB-only (doesn't cover
  local non-hub use) and adds a migration; would still need a filesystem path for local.
- (C) Enumerate agents at delete time — scan all project agents, check who references the
  branch/worktree; no new state but O(n) and fragile.

**Recommend (A)** — simplest mechanism that satisfies "local + hub uniformly" without a
schema change. (#168 mentions ent; flagging that (A) avoids it. Your call.)

### D2 — Ownership model
- **Ownerless worktree** — drop the owner/joiner asymmetry: the worktree belongs to the
  *project*, every mounting agent is just a sharer in the refcount; last sharer out tears it
  down. Eliminates the "delete owner = nuke joiners" footgun entirely.
- (alt) Deferred ownership / hand-off on owner delete — more moving parts, keeps asymmetry.

**Recommend ownerless** (matches #168's "make the worktree ownerless").

### D3 — Hub-managed join enablement (scope check)
Refcount is meaningless in hub mode until a 2nd `--branch <existing>` agent can actually
**join** (bind-mount the existing worktree) instead of falling back to clone-per-agent. So
#168 for hub implies enabling the join:
- detect an existing worktree for the requested branch → set the new agent's
  `opts.Workspace` to that existing worktree path (bind-mount), skip `git worktree add`;
- register the agent as a sharer (D1).

**Recommend including hub-join enablement in this work** (it's the precondition for hub
refcount). If you'd rather scope #168 to teardown-only and keep hub sharing for a follow-up,
say so and I'll split it.

## Proposed implementation (pending D1–D3)
1. **Sharer registry** (D1): helper to add/remove/list sharers for a worktree, guarded by the
   per-project lock. Local + hub call the same helper.
2. **Attach path:** local already attaches (`provision.go:422-428`) — add sharer registration.
   Hub (`start_context.go` `resolveWorktreeProvision`): when the branch's worktree already
   exists, attach (bind-mount existing) + register, instead of failing/falling back.
3. **Teardown** (`DeleteAgentFiles`): deregister the agent; only `RemoveWorktree` (+ branch
   when `removeBranch`) if it was the **last** sharer; otherwise just detach (remove the
   agent's own dirs, leave the shared worktree). Honor refcount rather than keying on
   `agents/<name>/workspace/.git` presence.
4. **Tests:** create owner + joiner (local and hub); both see the same tree; delete owner →
   worktree persists; delete last → worktree + branch removed; concurrent attach/delete under
   the lock.

## Out of scope
GC/base teardown (Q2/Q3 — keep base); K8s node-local (Phase 3, Q4); migration (Q5).
