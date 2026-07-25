# Design: Surface Workspace Sharing Mode to Agents via Provisioning Env Vars

**Issue:** [ptone/scion#572](https://github.com/ptone/scion/issues/572)  
**Project slug:** `workspace-mode-env`  
**Status:** Finalized — all open questions resolved (see §Open Questions for decisions)

---

## Problem & Goals

Agents cannot discover how their workspace is provisioned. The workspace sharing
mode taxonomy (`shared-plain` / `clone-per-agent` / `worktree-per-agent`) is
fully implemented in the store and threaded through the broker, but is never
surfaced into the agent container. An agent cannot answer "can another agent see
or overwrite my edits?" at runtime without knowing its mode.

This has two concrete downstream consequences today:

1. The `git-sandbox` platform skill (`inject_when: git_workspace`) assumes
   worktree semantics ("air-gapped from origin," "cannot `git checkout main`")
   — both assertions are false for `clone-per-agent`, where the agent owns a
   full clone with a live remote. Agents in that mode receive confidently-worded,
   incorrect git instructions.

2. Any future mode-aware content (mandatory boilerplate, skills, agent
   instructions) has no env var to branch on, so it would have to be gated at
   provisioning time via `inject_when` — a mechanism the design doc
   `.design/builtin-skills.md` notes "may go away soon."

**Goals:**

- Emit `SCION_WORKSPACE_MODE` (canonical string) and `SCION_WORKSPACE_GIT`
  (boolean) into every agent container env.
- Fix the `WorkspaceMode` propagation gap on `startAgent` and `restartAgent`
  paths so restarted/resumed agents receive the correct mode rather than
  silently falling back to `shared-plain`.
- Leave scope for follow-on work: updating mandatory boilerplate and
  consolidating the `git-sandbox` skill.

**Non-Goals:**

- Updating `mandatory_boilerplate/agent-instructions-preamble.md` — that is
  explicitly listed as follow-on work in the issue.
- Consolidating or rewriting the `git-sandbox` platform skill — follow-on.
- Adding derived vars like `SCION_WORKSPACE_ISOLATION` — a second source of
  truth for what mode already encodes invites drift; excluded by design.
- Changing the `inject_when` mechanism or adding new `inject_when` conditions.

---

## Proposed Design

Three changes ship together (dependency ordering: Change 3 → Change 1 → Change 2).

### Change 1 — Emit `SCION_WORKSPACE_MODE`

**File:** `pkg/runtimebroker/start_context.go`  
**Location:** Step 5 "Agent identity env" block (~line 298), alongside
`SCION_AGENT_ID` and peers.

```go
// Emit canonical workspace sharing mode.
// On the create path, in.WorkspaceMode carries the wire label (e.g. "per-agent").
// On start/restart paths, in.WorkspaceMode is empty but the hub pre-resolves
// the canonical value into resolvedEnv["SCION_WORKSPACE_MODE"] (Change 3).
// Either way, one code path produces a consistent env var.
if in.WorkspaceMode != "" {
    env["SCION_WORKSPACE_MODE"] = string(store.ResolveWorkspaceSharingMode(in.WorkspaceMode))
}
// If in.WorkspaceMode is empty, the hub-injected value (if any) already
// arrived via resolvedEnv and was merged into env before this block.
// Add a guaranteed fallback so the var is always present:
if env["SCION_WORKSPACE_MODE"] == "" {
    env["SCION_WORKSPACE_MODE"] = string(store.SharingModeSharedPlain)
}
```

**Value contract** (pending user confirmation — Open Question 1):

| Wire label (stored in project labels) | `SCION_WORKSPACE_MODE` value |
|---|---|
| `shared` or `""` | `shared-plain` |
| `per-agent` | `clone-per-agent` |
| `worktree-per-agent` | `worktree-per-agent` |

`store.ResolveWorkspaceSharingMode()` (`pkg/store/models.go:225`) already
implements this mapping and defaults empty/unknown to `shared-plain`.

**Why always emit** (not conditional like `SCION_SHARED_WORKSPACE`):  
Agents should always be able to read their mode. A missing var forces agents to
assume `shared-plain` anyway — making the default explicit is clearer.

---

### Change 2 — Emit `SCION_WORKSPACE_GIT`

**File:** `pkg/runtimebroker/start_context.go`  
**Location:** After the git-clone/worktree provisioning block (~line 497), once
`worktreeProvisioned` and `opts.GitClone` are settled.

```go
// Emit SCION_WORKSPACE_GIT when the workspace is a git repository.
// Mode alone is insufficient: shared-plain may be git-backed or not.
isGitWorkspace := worktreeProvisioned || opts.GitClone != nil
if !isGitWorkspace {
    // Covers shared-plain git workspaces on create path (opts.Workspace is
    // the mounted directory, already populated on disk).
    if opts.Workspace != "" {
        isGitWorkspace = util.IsGitRepoDir(opts.Workspace)
    }
}
if !isGitWorkspace {
    // Final fallback for start/restart paths where neither opts.GitClone nor
    // an on-disk git dir is present (e.g. clone-per-agent before the
    // in-container clone executes). Hub injects this via resolvedEnv (Change 3).
    isGitWorkspace = in.ResolvedEnv["SCION_WORKSPACE_GIT"] == "true"
}
if isGitWorkspace {
    env["SCION_WORKSPACE_GIT"] = "true"
}
// Absent when false — avoids encoding a "false" string agents must parse.
```

**Why a separate var from mode:**  
`shared-plain` may be git-backed — `pkg/hub/handlers_projects_core.go:309`
permits `WorkspaceModeShared` for git projects, and the glossary states Linked
projects "may be plain or git-backed." Mode alone cannot disambiguate.

**Why absent rather than `"false"`:**  
Following the `SCION_SHARED_WORKSPACE` and other boolean-presence idiom in the
codebase. Agents test for presence/truth; absence equals false.

---

### Change 3 — Fix `WorkspaceMode` Propagation on Start and Restart

This is a correctness bug independent of Changes 1 and 2, but it is the
**prerequisite** for Changes 1 and 2 to work correctly on start/restart paths.

#### Root cause

`WorkspaceMode` is only populated on the create path
(`pkg/runtimebroker/handlers.go:669`). The `startAgent` (~:1299) and
`restartAgent` (~:1558) handlers pass `startContextInputs` with
`WorkspaceMode: ""`, causing `ResolveWorkspaceSharingMode("")` to return
`shared-plain` — the most permissive mode, and wrong for isolated workspaces.

The same gap exists on the hub dispatch side: neither
`DispatchAgentStart` nor `DispatchAgentRestart` in
`pkg/hub/httpdispatcher.go` send `WorkspaceMode` to the broker, even though
`projectInfo.workspaceMode` is already populated at `httpdispatcher.go:771`.

#### Chosen approach: hub-side resolvedEnv injection

Inject `SCION_WORKSPACE_MODE` (canonical value) and `SCION_WORKSPACE_GIT`
into the `resolvedEnv` map that the hub already builds for both start and
restart dispatch calls. This follows the existing pattern used for
`SCION_AGENT_ID`, `SCION_GROVE_ID`, `SCION_HUB_ENDPOINT`, and
`SCION_METADATA_MODE` — the hub surfaces values the broker's request body
doesn't carry by pre-populating them in `resolvedEnv`.

**Hub-side changes (both `DispatchAgentStart` ~:1287 and
`DispatchAgentRestart` ~:1479 in `pkg/hub/httpdispatcher.go`):**

```go
// Inject workspace mode alongside existing identity vars
// (alongside the SCION_AGENT_ID/SCION_GROVE_ID/SCION_AGENT_SLUG block)
if projectInfo.workspaceMode != "" {
    resolvedEnv["SCION_WORKSPACE_MODE"] = string(
        store.ResolveWorkspaceSharingMode(projectInfo.workspaceMode))
}
// Inject git-ness: per-agent and worktree-per-agent are always git-backed.
// For shared mode, check the agent's applied config for a git clone URL.
switch projectInfo.workspaceMode {
case store.WorkspaceModePerAgent, store.WorkspaceModeWorktreePerAgent:
    resolvedEnv["SCION_WORKSPACE_GIT"] = "true"
case store.WorkspaceModeShared, "":
    if agent.AppliedConfig != nil && agent.AppliedConfig.GitClone != nil {
        resolvedEnv["SCION_WORKSPACE_GIT"] = "true"
    }
}
```

**Broker-side change (none beyond Changes 1 and 2):**  
Because `resolvedEnv` is merged into `env` early in `buildStartContext`, and
Change 1 only overwrites `env["SCION_WORKSPACE_MODE"]` when
`in.WorkspaceMode != ""` (create path only), the hub-injected canonical value
passes through unchanged on start/restart paths. Change 2's fallback
`in.ResolvedEnv["SCION_WORKSPACE_GIT"] == "true"` reads the same injection.

#### Why not add `WorkspaceMode` to `StartAgent`/`RestartAgent` signatures

The alternative (Approach A) is to add `workspaceMode string` to every
`StartAgent` and `RestartAgent` function signature. That interface has
**8 implementations** (HTTPRuntimeBrokerClient, AuthenticatedBrokerClient,
ControlChannelBrokerClient, HybridBrokerClient, and their test doubles), plus
every call site in the hub transport layer and broker HTTP handler.

The resolvedEnv pattern is already the established mechanism for exactly this
class of problem (hub → broker info that the request body doesn't carry). It
requires changes in two hub dispatch functions and two broker functions — far
narrower blast radius.

**Trade-off:** The resolvedEnv approach adds an implicit coupling rather than a
type-checked parameter. Mitigation: the injection sites are co-located with
the existing identity-var injection block, and missing injection is caught at
test time by verifying the env var in start/restart integration tests.

---

### Documentation Changes

1. **`docs-site/src/content/docs/local/workspace.md`** (or
   `workspaces-and-sharing.md`) — Add a section documenting `SCION_WORKSPACE_MODE`
   and `SCION_WORKSPACE_GIT` under the sharing-mode discussion. Note the three
   canonical values and the boolean-presence contract for `SCION_WORKSPACE_GIT`.

2. **`docs-site/src/content/docs/local/skills.md:233-240`** — The `inject_when`
   table may need updating if the follow-on work adds a `workspace_mode` condition;
   leave unchanged in this PR unless the `inject_when` mechanism is touched.

3. **Inline doc fix** (carry along):
   `docs-site/src/content/docs/local/workspace.md:190-191` states shared
   directories appear at **both** `/scion-volumes/<name>` and
   `/workspace/.scion-volumes/<name>` by default. The code makes this
   per-directory via `SharedDir.InWorkspace`. Correct the prose to "either/or
   depending on the directory's `in_workspace` setting."

---

## Alternatives Considered

### Alt 1: Emit the wire label, not the canonical value (for `SCION_WORKSPACE_MODE`)

Expose `"shared"` / `"per-agent"` / `"worktree-per-agent"` directly.

**Rejected:** The wire labels are a legacy implementation artifact — they appear
in project K/V labels and existed before the canonical glossary was written.
Exposing them to agents bakes in a historical accident. The canonical values are
in the glossary and already used in all post-refactor documentation. Agents
reading the env var should see the glossary-aligned names. (Open Question 1
confirms this with the user before finalizing.)

### Alt 2: Encode git-ness in `SCION_WORKSPACE_MODE` (no separate `SCION_WORKSPACE_GIT`)

A `shared-git` fourth mode or a `shared-plain-git` sub-mode could unify the two
signals.

**Rejected:** The three-mode taxonomy is established in the glossary and in the
`WorkspaceSharingMode` constants. Adding a fourth mode to carry git-ness for
one case (shared-plain git) would require updating all mode switches and
documentation. A separate boolean is cheaper and maps cleanly to the actual
orthogonality: mode describes *who shares what*, git-ness describes *what
storage backend is in use*.

### Alt 3: Derive git-ness at runtime from the `.git` directory (no `SCION_WORKSPACE_GIT` env var)

Agents could simply check for the presence of `.git/` at `/workspace/.git`.

**Rejected:** An env var is faster (no filesystem read at startup), available
before any directory access, and consistent with how other workspace properties
are surfaced. It also works correctly in the worktree case, where `/workspace`
is a worktree directory that contains `.git` as a file, not a directory — a
nuance that agents would have to know to handle.

### Alt 4: Add `WorkspaceMode` to `StartAgent`/`RestartAgent` signatures (for Change 3)

Type-safe threading of the wire label through all 8 interface implementations
and test doubles.

**Rejected in favor of resolvedEnv injection.** The resolvedEnv pattern is the
established precedent for this flow and the blast radius is much smaller. If the
signatures need extending for other reasons in the future, `WorkspaceMode` could
be added then.

### Alt 5: Broker-side lookup — read WorkspaceMode from running container labels

The broker could list running containers, find the agent by ID, and read a
container label set at create time.

**Rejected:** WorkspaceMode is not currently stored as a container label. Adding
a label would require a parallel change (and labels are not guaranteed to be
present if the container was created by an older broker version). The resolvedEnv
approach requires zero new storage.

---

## Migration / Rollout

**Backward compatibility:**

- `SCION_WORKSPACE_MODE` and `SCION_WORKSPACE_GIT` are new env vars. Existing
  agents that don't read them are unaffected.
- `SCION_WORKSPACE_MODE` defaults to `shared-plain` for empty/unknown wire
  labels (the current implicit behavior); nothing changes for existing projects
  without an explicit mode label.
- `SCION_SHARED_WORKSPACE` continues to be emitted (no removal in this PR).
  Open Question 3 covers deprecation.

**Forward compatibility:**

- The canonical value set (`shared-plain`, `clone-per-agent`, `worktree-per-agent`)
  matches the glossary and is stable. New modes, if ever added, would require
  a new case in `ResolveWorkspaceSharingMode` and a doc update — no env var
  contract break for existing values.
- The `SCION_WORKSPACE_GIT` absence-equals-false contract is stable: future
  git-backed modes would simply set the var.

**Rollout:**

- Hub and broker versions can deploy independently. If a new hub talks to an
  old broker, the old broker ignores the new identity vars in resolvedEnv
  (they arrive in the env but do nothing without the broker-side Change 1/2
  code). If an old hub talks to a new broker, the broker falls back to
  `shared-plain` for start/restart (same behavior as today, no regression).
- No database migrations. No API version bumps needed.

---

## Open Questions

All three questions resolved (2026-07-25). Decisions recorded below.

**OQ1 — Canonical vs. wire labels in `SCION_WORKSPACE_MODE`:**  
✅ **Decision: canonical values.** `SCION_WORKSPACE_MODE` emits `shared-plain`,
`clone-per-agent`, `worktree-per-agent` — aligned with the glossary. Wire labels
(`shared`, `per-agent`, `worktree-per-agent`) are not exposed to agents.
This is a stable agent API contract; new modes require a new canonical constant
in `store.WorkspaceSharingMode` and a doc update, not a rename.

**OQ2 — Ship Change 3 (WorkspaceMode propagation fix) with this PR or separately:**  
✅ **Decision: Option A — ship all three changes together.** The propagation fix
is a correctness prerequisite for Changes 1 and 2 to be accurate on restart
paths. Single cohesive PR.

**OQ3 — Deprecate `SCION_SHARED_WORKSPACE` once `SCION_WORKSPACE_MODE` lands:**  
✅ **Decision: Option A — deprecate, with a compatibility period.** Sciontool
and the broker/hub do not always ship together; older `sciontool` builds may
still read `SCION_SHARED_WORKSPACE`. This PR therefore:
1. Continues emitting `SCION_SHARED_WORKSPACE` unchanged.
2. Updates `sciontool init` to prefer `SCION_WORKSPACE_MODE` + `SCION_WORKSPACE_GIT`
   (falling back to `SCION_SHARED_WORKSPACE` when the new vars are absent, for
   compatibility with older broker versions).
3. Adds a `// Deprecated:` comment on the `SCION_SHARED_WORKSPACE` emission site.

Phase 2 (eventual removal of `SCION_SHARED_WORKSPACE`) is tracked in a separate
issue filed by the coordinator — see **ptone/scion#TBD** (issue number to be
added once filed). The implementation phases below include a sciontool update
task for the compatibility shim.

---

## Implementation Phases

*(All three changes ship together per OQ2 decision. sciontool compat shim added per OQ3.)*

### Phase 0 — Bug fix: WorkspaceMode propagation (Change 3)

**Commit scope:** Hub (`pkg/hub/httpdispatcher.go`) only.

1. In `DispatchAgentStart` (~:1287), add `SCION_WORKSPACE_MODE` (canonical) and
   `SCION_WORKSPACE_GIT` to the resolvedEnv block alongside existing identity
   vars.
2. In `DispatchAgentRestart` (~:1479), same injection.
3. Add unit tests verifying that start and restart resolvedEnv maps contain the
   correct canonical values for each of the three modes.

### Phase 1 — Emit `SCION_WORKSPACE_MODE` (Change 1)

**Commit scope:** `pkg/runtimebroker/start_context.go`.

1. In step 5 "Agent identity env" (~:298): add the `SCION_WORKSPACE_MODE`
   emission using the conditional + fallback logic from the design above.
2. Add tests:
   - Create path with each of the three wire labels → correct canonical output.
   - Start path with empty `WorkspaceMode` and hub-injected resolvedEnv →
     correct canonical value.
   - Empty/unrecognized mode → `shared-plain`.

### Phase 2 — Emit `SCION_WORKSPACE_GIT` (Change 2)

**Commit scope:** `pkg/runtimebroker/start_context.go`.

1. After the git-clone/worktree block (~:497): add the `isGitWorkspace`
   determination and `SCION_WORKSPACE_GIT` emission.
2. Add tests:
   - Worktree-provisioned path → `SCION_WORKSPACE_GIT=true`.
   - Create path with `GitClone` config → `SCION_WORKSPACE_GIT=true`.
   - Shared-plain with git workspace on disk → `SCION_WORKSPACE_GIT=true`.
   - Start/restart path with hub-injected `SCION_WORKSPACE_GIT=true` →
     var present.
   - Non-git shared workspace → var absent.

### Phase 3 — sciontool compatibility shim (OQ3)

**Commit scope:** `cmd/sciontool/commands/init.go`.

1. Update `sciontool init` to prefer `SCION_WORKSPACE_MODE` + `SCION_WORKSPACE_GIT`
   when both are present, with fallback to `SCION_SHARED_WORKSPACE` for
   older broker versions that don't yet emit the new vars.
2. Add a `// Deprecated: use SCION_WORKSPACE_MODE+SCION_WORKSPACE_GIT` comment
   on the `SCION_SHARED_WORKSPACE` emission site in `start_context.go`.
3. Add tests for both code paths in the fallback logic.

### Phase 4 — Documentation

**Commit scope:** `docs-site/`.

1. Add "Runtime Environment Variables" section to `workspace.md` or
   `workspaces-and-sharing.md` documenting `SCION_WORKSPACE_MODE` (values,
   defaults) and `SCION_WORKSPACE_GIT` (boolean-presence contract). Note
   `SCION_SHARED_WORKSPACE` as deprecated with the tracking issue reference.
2. Fix the shared-directories "both locations" doc bug in `workspace.md:190-191`
   (either/or per `SharedDir.InWorkspace`, not always-both).
3. Update `skills.md:233-240` inject_when table if needed.

---

## Acceptance Criteria

The QA tester should verify:

1. **Create path — all three modes:**
   - Spawn an agent in each of `shared-plain`, `clone-per-agent`, and
     `worktree-per-agent` projects.
   - Inside each agent: `echo $SCION_WORKSPACE_MODE` returns the correct
     canonical value.

2. **Create path — `SCION_WORKSPACE_GIT`:**
   - Agent in a git-backed project: `echo $SCION_WORKSPACE_GIT` → `true`.
   - Agent in a plain (non-git) project: `SCION_WORKSPACE_GIT` is unset.

3. **Start (resume) path:**
   - Stop and re-start an agent in a `clone-per-agent` project.
   - Inside the restarted agent: `SCION_WORKSPACE_MODE` → `clone-per-agent`
     (not `shared-plain`).
   - `SCION_WORKSPACE_GIT` → `true`.

4. **Restart path:**
   - Trigger a restart for an agent in a `worktree-per-agent` project.
   - Inside the restarted agent: `SCION_WORKSPACE_MODE` → `worktree-per-agent`.

5. **Default (no mode label):**
   - Agent in a project with no `scion.dev/workspace-mode` label.
   - `SCION_WORKSPACE_MODE` → `shared-plain`.

6. **`SCION_SHARED_WORKSPACE` backward compatibility (OQ3 deprecation):**
   - Existing shared-plain git agents still receive `SCION_SHARED_WORKSPACE=true`
     (emitted unchanged; deprecation period is in effect).
   - `sciontool init` works correctly when only `SCION_SHARED_WORKSPACE` is
     present (older broker, no new vars) AND when only the new vars are present.

7. **Documentation:**
   - `workspace.md` or `workspaces-and-sharing.md` documents both new vars.
   - The shared-directories "both locations" paragraph is corrected.

8. **No regression:**
   - Existing agents in plain projects function unchanged.
   - `sciontool init` continues to work in shared-plain git workspaces.
