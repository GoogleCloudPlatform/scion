---
title: Multi-Broker Setup
description: Connect multiple machines to a single Scion Hub for distributed agent execution.
---

## Overview

A single Scion Hub can dispatch agents to **multiple Runtime Brokers**. Each broker is a machine — a laptop, cloud VM, or Kubernetes cluster — that runs agent containers. This lets teams pool compute resources and target specific machines for specific workloads.

## Architecture

```
                    ┌──────────┐
       ┌────────────┤ Scion Hub├────────────┐
       │            └────┬─────┘            │
       │                 │                  │
  ┌────▼─────┐    ┌──────▼───┐    ┌────────▼──────┐
  │ Broker A  │    │ Broker B │    │   Broker C    │
  │ (laptop)  │    │(cloud VM)│    │ (K8s cluster) │
  └───────────┘    └──────────┘    └───────────────┘
```

Each broker maintains a persistent WebSocket connection to the Hub. The Hub acts as the control plane; brokers handle container execution locally.

## Adding a Broker

On each machine you want to register:

1. **Install Scion**, sign in to the Hub (`scion hub auth login --hub-url <hub-url>`), and set the Hub endpoint in your global settings (`scion -g global config set --global hub.endpoint <hub-url>`).
2. **Start the broker**:
   ```bash
   scion runtime-broker start
   ```
3. **Register the broker** with the Hub:
   ```bash
   scion runtime-broker register
   ```
4. **Authorize projects** the broker should serve:
   ```bash
   scion runtime-broker provide --project <project>
   ```

Repeat for each machine. See [Runtime Broker](/scion/hosted/ha/runtime-broker/) for detailed setup.

## Broker Selection

When starting an agent, the Hub resolves a broker through a priority cascade:

| Priority | Source | Condition |
| :--- | :--- | :--- |
| 1 | **Explicit `--broker` flag** | The named broker must be a provider for the project (auto-linked if not; auto-linking requires update access to the project). |
| 2 | **Project default broker** | Set in project settings; must be online. |
| 3 | **Hub-level default broker** | Set in [Agent Defaults](/scion/reference/admin-settings/#layout-structure) (`default_runtime_broker`); used when the project has no default. Must be a provider, online, and dispatchable. |
| 4 | **Single-provider auto-select** | If exactly one broker provides the project and it is online, it is used automatically. |
| 5 | **Error** | Multiple eligible brokers require explicit selection; no providers is an error. |

- **Target a specific broker** with the `--broker` flag:
  ```bash
  scion start --broker my-cloud-vm
  ```
- **Check broker availability** across all registered brokers:
  ```bash
  scion hub brokers
  ```
  On a broker machine, `scion runtime-broker status` shows that broker's own state.

## Moving an Agent to Another Runtime Broker

`scion reincarnate <agent> --broker <name|id>` moves an agent to another Runtime Broker, for example to drain a broker or to reach different hardware. The agent keeps its ID, slug, and generation chain, and its workspace, uncommitted and unpushed changes included. The move is a [reincarnation](/scion/reference/cli/#scion-reincarnate): the agent starts a new generation on the target with a Hub-built preamble and the handoff you provide.

The workspace is never copied. A move works only when both Runtime Brokers mount the **same NFS export**, so the target sees the very directory the source used. A move that does not meet that requirement is refused; there is no fallback that re-clones or copies the workspace.

```bash
# Check the move without changing anything
scion reincarnate my-agent --broker broker-b --dry-run

# Move it
scion reincarnate my-agent --broker broker-b --handoff-file handoff.md
```

### Requirements

- **The same NFS export on both Runtime Brokers.** Set [`server.workspace_storage`](/scion/reference/server-config/#workspace-storage-serverworkspace_storage) to the `nfs` backend on both, with the same `server`, `export`, and `subpath_root` for the first share (`nfs.shares[0]`), which holds the workspaces. Each Runtime Broker also writes or reads an export identity marker, `.scion-export-id`, in `<subpath_root>` through its own mount, and the two must report the same marker, so `subpath_root` must exist and be writable. Matching settings alone are not enough: two brokers that configure the same address but mount different directories are refused.
- **The workspace is on the export.** The Runtime Broker records where the agent's workspace is each time it starts the agent:
  - Shared-workspace (shared-plain) and Hub-managed workspaces are on the export on Docker, Podman, Apple `container`, Cloud Run, and Kubernetes with a PV claim (`nfs.shares[].pv_name`).
  - Clone-per-agent and empty-per-agent workspaces are on the export only on Kubernetes with a PV claim, so both the agent's current runtime and the target's profile must be Kubernetes.
  - An agent that has not started since its Runtime Broker began recording this has an unknown placement and is refused. Reincarnate it once without `--broker` to record it.
  - Worktree-per-agent agents, agents in linked projects, and workspaces kept as a GCS-synced copy cannot move.
- **The target profile.** The profile the agent runs under (its own, else the project's active profile, else the target's default profile) must exist on the target and be available.
- **Both Runtime Brokers support agent move and are up to date.** Each must report its workspace storage and the agent-move capability. An older Runtime Broker on either side gets `412 Precondition Failed`, and nothing on the export is touched.
- **Both Runtime Brokers are online.** The target must be reachable and report its NFS mount healthy (`503` otherwise). The source must be online too, because it removes its local copy of the agent after the move. A move off an offline source is refused with `412`.
- **Room on the target** under its [per-broker agent limit](#considerations) (`429` otherwise). This check only reads the current count; the limit is enforced when the quota moves, after the agent is stopped (see [What happens during a move](#what-happens-during-a-move)).

### What moves and what is regenerated

- **Moves:** the agent's identity and generation chain, and its workspace on the export. The workspace stays in place on the export; only the Runtime Broker that runs the agent changes. The agent's quota reservation moves from the source to the target, and its exposed ports are cleared.
- **Regenerated:** the agent home. It is broker-local, so the target builds it fresh from the template and harness config, as any reincarnation does. Harness session history is not carried over; continuity comes from the handoff.
- **Experimental NFS home:** an agent with a persistent [NFS home](/scion/hosted/ha/kubernetes/#persistent-agent-home-nfs) (the `hub.k8s_nfs_home` experiment) has its home on the shared-dir export, not on the source. The target decides the home on its first start, from its own settings. If the target also has the NFS home enabled for the agent's profile (Kubernetes runtime, the experiment on, home storage `nfs`, and the same shared-dir share and `subpath_root`), it mounts the same home, so the home travels with the move. Otherwise the agent starts with a fresh local home on the target, and the old NFS home is left on the export untouched. Either way nothing is lost. The move's same-export check covers workspace storage only, not shared-dir storage.

### Checks and dry run

The Hub checks the move in a fixed order: workspace mode, workspace storage reported by both Runtime Brokers, same NFS export, workspace on the export, target profile, target health, access, agent-move capability, and capacity. The first failing check decides the answer, and the CLI prints every check as passed, failed, or not evaluated.

A refused move is refused **before any side effect**: the agent is not stopped, no quota changes, and the target is not linked to the project. `--dry-run` runs the same checks and returns the verdict and plan without doing anything else. Without `--dry-run`, the CLI sends a dry run first and stops if it is refused, so a real request is only sent for an eligible move, and never to a Hub that does not support `--broker`. Patch flags such as `--model` or `--image` combine with `--broker`, and the checks judge the patched configuration.

### Permissions

Moving another agent needs `agent.lifecycle` on it, as any reincarnation does; an agent moving itself needs no permission unless it also passes patch flags. If you are not the agent, you also become its recorded delegator, so you must be able to delegate its role: a non-admin reincarnating an agent with a privileged role gets `403` from this authority check before any of the move checks run.

For a user, the move then needs:

- **Seeing the target.** A target you cannot see is answered exactly like an unknown one: `404 runtime_broker_not_found`. A user can see a Runtime Broker that has AutoProvide on, or already serves the agent's project, or that the user can read (`broker.read`). A purely project-scoped caller therefore gets `404` for a broker outside the project. The Hub's member and viewer roles give signed-in users broker read.
- **Dispatching to the target.** Dispatch on the target (`broker.dispatch`), unless it has AutoProvide on. This applies even when the target already serves the project.
- **Linking the target.** If the target does not serve the project yet, the move links it as a provider, which needs project update (`project.update`).

What each kind of caller can reach:

| Caller | Can move the agent to |
| :--- | :--- |
| Signed-in user | Any Runtime Broker the user can see and dispatch to; one that does not serve the project yet also needs `project.update`. |
| User access token | Only Runtime Brokers with AutoProvide on, because a token is single-project and cannot carry broker read or broker dispatch. The target must already serve the project, unless the token has the `project:update` scope, in which case the move can link it. To reach any other broker, sign in (`scion hub auth login`). |
| The agent itself | Only a Runtime Broker that already serves its project (AutoProvide is not needed); otherwise `409`, and nothing is linked. No extra permission is needed. An agent that runs with a GCP passthrough identity cannot move itself (`403`, "ask a user to move you"). |
| Another agent (for example, a coordinator moving its child) | Only a Runtime Broker that already serves the project and has AutoProvide on, or, if the calling agent has the `project:agent:create` scope, any Runtime Broker that serves the project. An agent never links a new broker, because agents cannot update the project. The calling agent needs the agent lifecycle permission on the agent it moves. |

When a user or another agent moves an agent that runs with a GCP passthrough identity, the passthrough rules are checked again against the target, and the move gets `403` if passthrough is not allowed there.

### What happens during a move

The Hub accepts an eligible move with `202 Accepted` and runs it in the background. It re-runs the checks, stops the agent on the source, moves its quota, assigns it to the target, links the target to the project if needed, and provisions the agent on the target. The target first confirms through its own mount that the workspace directory exists; if it does not, the target refuses to provision the agent, and the move fails and rolls back. The Hub then starts the new generation with the preamble and handoff, and finally asks the source to remove its local copy of the agent: its container, its broker-local agent directory, and its home. That cleanup never touches the export or the agent's branch.

These steps run after the CLI has already received `202`, so a failure here does not come back as an HTTP error. The agent goes to the `error` phase, and its status message reads `reincarnation failed: …` with the reason, for example `provision on the target broker failed: …`.

- **Failure before the stop.** If the re-run checks or the stop fail, nothing has moved: the agent stays assigned to the source, in the `error` phase, and may still be running there.
- **No room on the target, or another failure after the stop.** If the target has no room when the quota moves (the dry-run capacity check only reads the current count), or anything else fails after the stop and before the agent is assigned to the target, the agent is left stopped on the source in the `error` phase, for example with `reincarnation failed: target broker quota: …`. Nothing was moved; start it or retry the move later.
- **Rollback.** If anything fails after the agent is assigned to the target and before the new generation is running, including a start that definitely left no container, the Hub rolls the move back: it removes the agent's local state on the target (best effort), and restores the agent to the source with its quota, previous configuration, and workspace. The agent is left stopped on the source, in the `error` phase; start it or retry the move. A provider link created by the move stays. If the Hub log warns that the agent's state on the target could not be removed, clean up the target as described under [Cleaning up stale state](#operator-notes-and-limits) below; the target is not running the agent.
- **Ambiguous start.** If the target's start fails in a way that may have left a container, the agent stays on the target in the `error` phase, with its quota there, and the source is not cleaned up.
- **Moving to a freshly linked target.** A target that has never hosted the project is linked to it by the move, just before it provisions the agent. If the target cannot confirm the workspace through its mount, it refuses to provision the agent, and the move fails and rolls back as above; the agent's status message names the missing workspace, and nothing on the export is lost. Check that the target mounts the export and that the project's workspace is visible there, then retry. The provider link stays, so the retry no longer needs `project.update`.

### Operator notes and limits

The Hub records the outcome of the source cleanup on the move's reincarnation record (`sourceCleanup`, not shown by the CLI). A completed move counts as completed whatever the cleanup outcome; the Hub log has a warning when the cleanup failed or was skipped for lack of a run ID. The outcomes are:

- `done`: the source removed its local copy of the agent.
- `failed:<reason>`: the source could not remove it, and its local state is left in place.
- `skipped:no-run-id`: the agent had no run ID on the source, because an older version started it. Without one, the source could only find the agent by name, and on a Kubernetes namespace shared by both Runtime Brokers that could remove the agent's new pod, so the Hub leaves the source alone.
- `skipped:ambiguous-start`: the target's start may have left a container, so the agent stays on the target (see above).
- `skipped:interrupted`: the move failed before it completed. That covers a rollback, and a move cut short, for example by a Hub restart. The Hub moves the quota back to whichever Runtime Broker the agent is assigned to, and does not clean up the source.

**Cleaning up stale state.** If the agent ended up on the target (its Runtime Broker in `scion list` or the agent record is the target) and the source was not cleaned up, the source Runtime Broker may still hold the agent's old container or pod, its broker-local agent directory (in the project's directory under `~/.scion/projects/` on that broker), and its home. None of this holds workspace data, and the Hub does not remove it automatically. Remove the old container or pod with the runtime's own tools. On Kubernetes, check that its `scion.run_id` label is the source's run and not the agent's new pod, especially when both Runtime Brokers share a namespace. Then remove the broker-local agent directory. After a rollback the same applies the other way round: if the Hub log warns that the target's local state could not be removed, clean up the target this way. Do not clean up the Runtime Broker the agent is assigned to. **Never delete anything on the NFS export:** the agent's workspace lives there and is in use.

## IAP-Protected Hubs

When the Hub is behind [Google IAP](/scion/hosted/ha/auth-proxy-iap/), **all** brokers connecting to it need transport auth configured. Each broker must carry an OIDC token to traverse the platform guard.

In a multi-hub setup using the broker's multistore, each hub connection can have its own transport settings:

- **`transportMode`** and **`transportAudience`** are per-connection fields in the credentials file. Different hubs may use different IAP OAuth client IDs.
- A single broker can serve both IAP-protected and plain (non-IAP) hubs simultaneously — connections without transport fields behave as before.
- Environment variables (`SCION_TRANSPORT_MODE`, `SCION_TRANSPORT_AUDIENCE`) apply globally and override all credential-file values. Use per-connection fields when serving hubs with different audiences.

See [Brokers behind IAP](/scion/hosted/ha/auth-proxy-iap/#brokers-behind-iap) for the full deployment guide.

## Considerations

- Each broker manages its own **port pools, container images, and local storage**. Images must be available on each broker independently.
- **Shared directories** (mounted volumes) only work within a single broker by default. Agents on different brokers cannot share a local directory. To share them across brokers, set [`server.shared_dir_storage.backend`](/scion/reference/server-config/#shared-directory-storage-servershared_dir_storage) to `nfs` in every broker's global settings, pointing at the same NFS export.
- **Workspace strategy** may differ per broker: local brokers typically use git worktrees (`.scion_worktrees/`), while hub-hosted git projects use a single workspace checkout.
- **Per-broker agent limit.** The Hub caps how many agents can be running on each broker with the `max_agents_per_broker` limit (default **100**). The limit is checked before an agent is created and again when it is started, resumed, or restarted. A request over the limit fails with `429 Too Many Requests` (`quota_exceeded`) instead of overloading the broker's host. A restart that is refused this way leaves the running container alone; it is not stopped first. Only running agents count: stopping, suspending, or exiting an agent frees its slot. Admins can change the hub-wide default through the [admin limits API](/scion/reference/api/#admin-apiv1admin), and override it for a single broker — including to a value *lower* than the hub-wide default — through that broker's settings (see [Broker Settings](/scion/reference/api/#broker-settings-apiv1runtime-brokersidsettings)). The Hub does not balance load across brokers; size the limit to each machine's resources, and lower it below the default if any broker in the fleet is a single-node Cloud Run deployment.
- **Global project on multi-hub brokers.** A broker connected to more than one Hub rejects agents in the global project with `409 Conflict` and the error code `global_project_disabled` (formerly `global_grove_disabled`).
