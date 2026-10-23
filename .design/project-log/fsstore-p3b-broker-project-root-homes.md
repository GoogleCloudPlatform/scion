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
  ptone/scion#5923 pending a security review; shared-backend brokers never read
  it before either).
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
  read-only (temporary, retired with ptone/scion#5835) and agent delete removes
  it. Their persisted workspace lies under that legacy directory, so a restart
  from the project root is refused cleanly by the workspace containment check.
- A broker without its own mount of the share now refuses: leaf creation no
  longer leaves directories to the node, and on a shared Layout the NFS
  dispatch check refuses a Kubernetes or Cloud Run dispatch whose share is not
  mounted.

### Homes on one share (ptone/scion#5825)

`projectfs.Layout.AgentHome` and `AgentSecrets` place an agent's home and
secrets at `<subpath root>/<project>/agents/<slug>/{home-,secrets-}<agent ID>`.
The Kubernetes NFS home and the Cloud Run agent runtime both resolve through
them (acceptance A12); Cloud Run moves from `agents/<agent ID>/{home,secrets}`.
Homes on nfs take their share and claim from the project storage Layout.

## Verification

- `pkg/projectfs`, `pkg/config`, `pkg/agent`, `pkg/runtime`,
  `pkg/runtimebroker` unit tests, including: shared project configuration
  predicate and tiers, forced-external state root and legacy fallback, a start
  from a shared root (Docker and Kubernetes), two brokers of one project sharing
  the project root but not their state, refusals for an unmounted share and
  missing or mismatched configuration, and the Kubernetes/Cloud Run home path
  equality.
- A8 (local backend unchanged): local-backend tests unchanged and passing.

## Follow-ups

- ptone/scion#5923: repository `workspace/.scion` template tier on shared
  backends (security review first).
- ptone/scion#5835: remove the legacy in-project agent-state fallback and the
  broker-local legacy trees; broker `deleteProject` still removes only
  broker-local slug trees.
