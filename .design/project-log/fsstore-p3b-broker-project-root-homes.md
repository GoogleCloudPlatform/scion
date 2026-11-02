# Project storage phase 3b: broker project root, homes on one share, broker-side migration hooks

**Date**: 2026-10-23
**Issues**: ptone/scion#5826, ptone/scion#5823 (broker read side, shared-plain provisioning backstop, migration step 3 hook), ptone/scion#5825 (homes); phase ptone/scion#5820, tracking ptone/scion#5805
**Stacks on**: phase 3a (broker Layout, `server.project_storage`) and phase 2 (hub seed of `<ProjectRoot>/.scion`, the migration engine)

## Summary

Before this phase a broker on shared project storage (nfs, Cloud Run or GKE
volumes) ran a hub-managed project from its own broker-local
`~/.scion/projects/<slug>`: each broker created and read its own copy of the
project configuration, while the workspace lived on the share. Two brokers of
one project could disagree on settings, templates and identity, and the hub's
seed on the share was never read.

### Repo root detection takes its candidates from the layout (ptone/scion#5826)

`detectRepoRoot` verifies a candidate root with bounded git probes and refuses
on disagreement, as before. The candidate now comes from `pkg/projectfs`: the
local Layout's workspace of the project's own root, or, for a workspace the nfs
backend placed on the share, `Layout.Resolve(workspace)`
(`<ProjectRoot>/workspace`). A backend path the Layout does not give refuses
the start.

### Brokers read project configuration from the project root (ptone/scion#5823)

- On a shared Layout, a hub-managed project dispatched by slug runs from
  `Layout.ProjectRoot(uuid)` (create and start/restart). The broker checks that
  the share is mounted (`EnsureShareMounted`; otherwise 503
  `project_storage_unavailable` naming the mount point) and that
  `<ProjectRoot>/.scion` is the hub-seeded configuration of this project:
  a real directory whose `project-id` names the project and which has a
  `project-slug` (otherwise 409). It never creates or repairs that directory.
- Project configuration (settings, templates, harness-configs, hub identity) is
  that directory itself; it is never redirected to an external
  `project-configs` directory. The repository's own `workspace/.scion` is not
  read on shared backends (its use as a lower template tier is deferred to
  ptone/scion#5923; shared-backend brokers never read it before either).
- The project name is the hub-written `project-slug`, never the UUID directory
  name.
- Broker-private state stays broker-local: every agent's state directory is
  `~/.scion/project-configs/<slug>__<id8>/.scion/agents/<name>` whatever its
  sharing mode, so nothing broker-private lands on the share and two brokers of
  one project never share it. The ownership store and launch journal were
  already keyed by broker, project ID and agent.
- The workspace configuration record (from the read-only `.scion` mount work)
  is keyed by the project root; the broker only records, it writes nothing.
- Agents provisioned before the move in a non-shared mode keep their state in
  `~/.scion/projects/<slug>/.scion/agents/<name>`; look-ups fall back to it
  (no new agent is ever provisioned there; temporary, retired with
  ptone/scion#5835) and agent delete removes it. When such an agent has its own agent-scoped workspace in that directory
  (clone-per-agent, possibly with unpushed work), exactly that agent directory
  is admitted as an extra workspace root until the agent is recreated (logged
  once per agent); a shared or project-scoped workspace is never admitted from
  the legacy tree, and such an agent's restart is refused cleanly. Upgrade
  note: push the work of these agents and recreate them at your convenience.
- A broker without its own mount of the share now refuses: leaf creation no
  longer leaves directories to the node, and on a shared Layout the NFS
  dispatch check refuses a Kubernetes or Cloud Run dispatch whose share is not
  mounted.

### Workspace bootstrap targets the shared workspace (ptone/scion#5823)

On a shared Layout the broker applies a CLI workspace bootstrap upload to the
project's workspace on the share (`<ProjectRoot>/workspace`), under the
project's provisioning locks, only when no provisioner has populated it (no
provisioning sentinel); it then writes the sentinel
(`provision.MarkProvisioned`). A workspace that carried the sentinel when the
create was admitted is not overwritten (the existing warning). No broker-local
record or workspace marker is written.

### Shared-plain provisioning backstop (ptone/scion#5823)

On a shared Layout, a shared-plain git project dispatched to Docker, Podman or
Apple container gets `provision.ProvisionShared` on its shared workspace before
the container starts, with the provisioning init container's layout (sentinel
and lock in the project state directory, the workspace root as the legacy
sentinel location, ownership from the Layout, chown failure not fatal). A
provisioned checkout is reused; a lost sentinel or an older project is cloned
under the lock, with the agent's git token bound to the clone URL's host. The
in-container clone is then not requested.

### Node-local shared dirs move to the shared tree on broker start (ptone/scion#5823)

A broker whose Layout places shared dirs on the shared tree moves the shared
dirs it kept node-local (`~/.scion/project-configs/<slug>__<id8>/shared-dirs`)
into `<ProjectRoot>/shared-dirs` once per process start, with the
project-storage migration's mover (copy-only into the authority's leaf, never
overwriting, symlinks refused, source kept as `.migrated-<ts>`), under the
project's provisioning locks, matching projects by the hub-written identity on
the share.

### Homes on one share (ptone/scion#5825)

`projectfs.Layout.AgentHome` and `AgentSecrets` place an agent's home and
secrets at `<subpath root>/<project>/agents/<slug>/{home-,secrets-}<agent ID>`.
The Kubernetes NFS home and the Cloud Run agent runtime both resolve through
them, so both resolve the same path for the same agent; Cloud Run moves from `agents/<agent ID>/{home,secrets}`.
Homes on nfs take their share and claim from the project storage Layout; a
recorded nfs home must still be on that share (a home recorded on another
share of the old list is refused). A Cloud Run agent whose home is still at
the old location is refused at start until the migration has moved it.
The project-storage migration's Cloud Run homes step moves existing
`agents/<agent ID>/{home,secrets}` to the new paths, only for stopped agents
(a running agent's are reported deferred and move on a later run); it is the
only code that builds the legacy path.

## Merge with upstream main after the one-storage-block change landed

This branch was built on the earlier commits of the one-storage-block change;
upstream main now carries that change as a single commit, with later fixes.
The branch merged main (167a60d4) with a merge commit, not a rebase. The
merge was computed against the one-storage-block head this branch was built
on, so every line of that change comes from main and only this branch's own
work is applied on top: the diff from main to the merge lists exactly the 43
files of this branch's own range.

Four files needed a manual resolution, each keeping both sides:

- `pkg/agent/provision.go`: the agent-files guard keeps main's sharer-key
  worktree root and adds the legacy agent state dir as an extra root.
- `pkg/agent/run.go`: main recomposes the repository root through
  `composeRepoRoot` (with the fail-closed record intersection). A workspace
  the backend placed on the shared tree now goes through the new
  `composeSharedRepoRoot` (`pkg/agent/repo_root_layout.go`), which takes the
  candidate root from the shared layout and keeps the same intersection.
- `pkg/runtimebroker/handlers.go`: main's ownership marking and test hook
  stay around the bootstrap-outcome download call.
- `pkg/runtimebroker/start_context.go`: main's release-proof path update
  stays, followed by the shared project root resolution.

Checks at the merged head: gofmt on every changed Go file, the pkg/hub
scan-coverage guard, a duplicate top-level identifier scan of pkg/hub in the
default, no_sqlite and integration tag sets, `go vet` and the unit tests of
the touched non-hub packages. pkg/hub and ./cmd are compiled only in the gate.

## Verification

- `pkg/projectfs`, `pkg/config`, `pkg/agent`, `pkg/runtime`,
  `pkg/runtimebroker` unit tests, including: shared project configuration
  predicate and tiers, forced-external state root and legacy fallback, a start
  from a shared root (Docker and Kubernetes), two brokers of one project sharing
  the project root but not their state, refusals for an unmounted share and
  missing or mismatched configuration, and the Kubernetes/Cloud Run home path
  equality.
- Local backend unchanged: local-backend tests unchanged and passing.

## Follow-ups

- ptone/scion#5923: repository `workspace/.scion` template tier on shared
  backends.
- ptone/scion#5835: remove the legacy in-project agent-state fallback and the
  broker-local legacy trees; broker `deleteProject` still removes only
  broker-local slug trees.
