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

1. **Install Scion**, add the Hub to your hub list and make it current (`scion hub add <name> <hub-url> --use`), and sign in (`scion hub auth login`). `register`, `join` and the other Runtime Broker administration commands (`deregister` picks the hub connection from the Runtime Broker's stored connections (with `--name` when there are several), or, with none stored, the registration in the global settings, but sends the deregistration to the Hub the CLI selects, so select that connection's Hub first) use that Hub (or `--hub`, which takes a URL or a hub list nickname). The Runtime Broker process takes its Hub from `server.broker.hub_endpoint` instead, which `register` and `join` save when it is unset (except on a host that runs a workstation Hub) (see [Runtime Broker prerequisites](/scion/hosted/ha/runtime-broker/#0-prerequisites)).
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

### Agents that start other agents

When an agent runs `scion start` or `scion create` inside its container, the CLI reads the Hub endpoint from the agent's environment (`SCION_HUB_ENDPOINT`, then `SCION_HUB_URL`), so no `--hub` flag is needed. The Hub resolves the new agent's broker through the same cascade as any other create. The profile is resolved the same way too: `-p` first, then the project's active profile, then the selected broker's default profile. The creating agent's broker and profile are never inherited: they are used only when `--broker` or `-p` names them, or when the cascade or the project's active profile selects them on its own. To run agent-launched agents on a particular broker or profile, such as a Kubernetes profile, set the project's default broker and active profile, or pass the flags.

### Flat Runtime Brokers (experimental)

With the `hub.flat_runtime_brokers` [experiment](/scion/reference/experiments/) on (default off), the Hub accepts Runtime Brokers that each serve exactly one runtime target with a stable identity. A new agent placed on such a Runtime Broker is pinned to its runtime target and stays on it across restarts, and the Hub refuses a dispatch for a different target. The agent's record shows the pin as a read-only `pinnedRuntimeTarget`, and the Runtime Broker's record shows its `runtimeTarget` (see the [Agents API](/scion/reference/api/#agents-apiv1agents)). A pinned agent cannot be [moved to another Runtime Broker](#moving-an-agent-to-another-runtime-broker), and an agent cannot be moved onto a flat Runtime Broker (`409 runtime_target_move_unsupported`). Existing profile-based Runtime Brokers are unchanged.

The experiment also switches the web UI's **Brokers** pages to the Brokers area. The list shows each Runtime Broker's kind (flat or legacy), runtime target, and reported project storage layout (`server.project_storage`: backend, NFS server/export/`subpath_root`, and `homes`), and marks the Runtime Brokers that run in this server's own process. Each Runtime Broker's page has Overview, Agents & projects, Health and Configure tabs; the Overview and Health tabs also show the Runtime Broker's project storage layout and any layout mismatch with the Hub (when a mismatch list is absent on an unhealthy storage status, the page notes that layout agreement with the Hub is not confirmed). Health is shown only to users who can read the Hub health summary. Configure is shown only to users who can update or unregister the Runtime Broker. With the experiment off, the Brokers pages are unchanged.

To run flat Runtime Brokers, add one entry per instance under `server.broker.instances` in the global `settings.yaml`. Each entry has an immutable local `key`, a `name` that it registers under, and exactly one `runtime_target`: the local Docker daemon, or a Kubernetes cluster and namespace.

```yaml
server:
  broker:
    instances:
      - key: local-docker
        name: local-docker
        runtime_target:
          type: docker
      - key: k8s-batch
        name: k8s-batch
        runtime_target:
          type: kubernetes
          context: batch-cluster
          namespace: agents
          shared_dir_size: 40Gi
          safe_to_evict: false
```

Each `key` must be unique. A process with `server.broker.instances` hosts only the flat instances, not a regular Runtime Broker. With the Hub in the same process, the instances register through it. A process without the Hub hosts them remotely: register each one first with `scion runtime-broker register --instance <key>`, which saves the instance's own credentials, and they are checked against the Hub at every start. Startup is refused if an entry does not validate. An instance that cannot serve is refused and reported, and the other instances keep running. If the Hub refuses the registration, for example a first registration while the `hub.flat_runtime_brokers` experiment is off, the process still starts and logs that the instance was not activated. See [`server.broker`](/scion/reference/server-config/#broker-settings-serverbroker) for the fields.

A flat instance never uses Runtime Broker Profiles: not the active profile, and not a profile named in a request. Creating an agent on a flat Runtime Broker with any `profile` is refused before anything is written with `422 runtime_profile_unsupported`. Default settings that a profile or its runtime entry would supply to a regular Runtime Broker are set on the instance's `runtime_target` instead. These are:

- the shared directory PVC class and size;
- `safe_to_evict`;
- the pod priority class;
- GKE mode.

Each of those four defaults keeps its usual precedence: an agent's own Kubernetes setting still wins over it, and an unset field falls back to the same global setting as before. By contrast, pod placement on a flat Kubernetes instance (`node_selector`, `tolerations`, and `runtime_class_name` on `runtime_target`) is owned by the instance: the instance's placement is applied to every agent pod, and an agent's `kubernetes.nodeSelector`, `tolerations`, or `runtimeClassName` is refused before any pod or volume is touched unless it matches the instance's value (or the instance sets the matching `allow_agent_node_selector`, `allow_agent_tolerations`, or `allow_agent_runtime_class_name` escape hatch when that placement field is unset on the instance). Before upgrading an existing flat Kubernetes deployment, run [`scion doctor`](/scion/reference/cli/#scion-doctor) (`flat-placement-preflight` check) to find any existing pinned agents whose stored placement disagrees with their instance; see [Flat instance placement](/scion/reference/server-config/#flat-instance-placement). Workspace, shared-directory and Agent home storage placement come from [`server.project_storage`](/scion/reference/server-config/#project-storage-serverproject_storage) for the whole deployment; the former `shared_dir_storage_backend`, `shared_dir_storage_backends`, `home_storage_backend` and `home_storage_leaf` keys on `runtime_target` are removed and rejected at load time (and any legacy per-agent `shared-dir-storage.json` record that pins `nfs` without shared project storage is refused at agent start with `422 storage_migration_required`). See [the runtime target fields](/scion/reference/server-config/#flat-instance-runtime-target-serverbrokerinstancesruntime_target) for every field.

An entry may also set `agent_defaults`: the image registry, default harness config, volumes, resources, harness overrides, required secrets and clone depth of the agents it runs. Each default sits where the matching profile setting sat, so templates, the agent's own configuration and the request keep winning where they won over a profile. See [flat instance agent defaults](/scion/reference/server-config/#flat-instance-agent-defaults-serverbrokerinstancesagent_defaults).

Each flat instance reports a read-only presentation of its runtime target and of its defaults for new agents (`runtimeTargetPresentation`) on its own heartbeats: the instance `key` and `name`; the target's `type`, display name, Kubernetes namespace and GKE flag, priority class, shared directory storage class and size, `safeToEvict`, `nodeSelector`, `tolerations`, `runtimeClassName`, and `placementReported`; and the agent defaults' default harness config, image registry, clone depth, resources, volume mounts (`target`, `readOnly`, `type`), secret declarations (`key`, `type`, `description`), and harness overrides (image, pull policy, auth type, user, environment variable names only, volumes, resources). Kubeconfig paths and contexts, service-account policy, volume sources, secret targets, and environment values are never published, and credentials in the image registry are redacted. Except for the instance-owned pod placement fields (`nodeSelector`, `tolerations`, `runtimeClassName`), these are defaults for new agents, not the configuration of existing agents. The Overview tab on a flat Runtime Broker's page shows them in the **Runtime target** card, and `scion hub brokers info <broker>` prints them (`runtimeTargetPresentation` in `--json`).

#### Inconsistent agent placement

Every operation that reaches a Runtime Broker for an existing pinned agent first checks the agent's saved placement: start, restart, stop, suspend, delete, reset-auth, messages, keys, logs, exec, attach, conduit streams and port proxying, workspace sync, and dispatches queued for a Runtime Broker that was offline. Reading the agent (get, list, status) is not checked. The placement is inconsistent when the agent's pin does not name the Runtime Broker it is assigned to, the pin is incomplete, the agent has no Runtime Broker, the Runtime Broker no longer serves a single runtime target, or that Runtime Broker serves another runtime target. The Hub then refuses the operation with `409 runtime_target_pin_stale` (or `409 runtime_target_mismatch`), or with `409 runtime_broker_missing` when the Runtime Broker the agent is pinned to is no longer registered. Nothing is sent to any Runtime Broker, and Scion never looks for another one to run the agent on.

An ordinary delete is refused in the same way, with or without `force`: removing the agent's record without reaching its Runtime Broker could leave a running workload behind and release its capacity with no evidence that it was torn down. The agent's record and its accounting stay as they are. A stop that was queued while the Runtime Broker was offline and is refused this way when the Runtime Broker reconnects leaves the agent stopped with the container status `stop_queued`, and the agent keeps its Runtime Broker capacity slot until an inventory shows the container gone or the placement is repaired. No per-agent repair is available in this release; the recovery path is tracked in [ptone/scion#4844](https://github.com/ptone/scion/issues/4844).

Because such an agent's record stays, it also counts as a live agent of its project: a project delete without `force` is refused while it remains ([ptone/scion#4811](https://github.com/ptone/scion/issues/4811)). A project delete with `force` removes the project's agent records with only best-effort runtime deletes. That is an operator's explicit, project-wide choice; Scion never selects it on its own. The refusal of a project delete without `force` mentions the force option generically, also when an agent whose placement is inconsistent is what blocks the delete. For such an agent nothing is sent to any Runtime Broker: its workload is left running there with no agent record, the Hub logs an error naming the agent and its pinned Runtime Broker, and the workload has to be removed on that Runtime Broker by hand.

#### Capacity of flat Runtime Brokers

The `max_agents_per_broker` limit applies per Runtime Broker ID, and every flat instance has its own ID: two instances have two limits, and the sum of their limits is the host's effective ceiling. Filling one instance never consumes or bypasses another's capacity. Set each instance's cap with its `maxAgents` setting (see [Broker Settings](/scion/reference/api/#broker-settings-apiv1runtime-brokersidsettings)); `0` means unlimited, and an instance without the setting inherits the hub-wide default. An agent pinned to one instance that still holds a reservation on another is refused with `409 broker_reservation_scope_conflict`, and its reservation is never moved.

A legacy Runtime Broker's limit is not copied to the instances that replace it, because copying it to each instance would multiply the aggregate. The conversion rule is: a positive legacy `maxAgents` of L is split across n instances when L is at least n (by default floor(L/n) each, the remainder to the first instance; an operator may choose any positive caps that sum to at most L); with fewer than n slots, or when the legacy broker only inherited a binding or the hub default (each instance would inherit it, so the aggregate would become n times larger), an operator decision is required; a legacy `maxAgents` of `0` (unlimited) needs nothing. A plan computed while broker quotas are not enforced, or whose receiving instance would get fewer slots than the legacy broker's active reservations, is flagged.

## Moving an Agent to Another Runtime Broker

`scion reincarnate <agent> --broker <name|id>` moves an agent to another Runtime Broker, for example to drain a broker or to reach different hardware. The agent keeps its ID, slug, and generation chain. The move is a [reincarnation](/scion/reference/cli/#scion-reincarnate): the agent starts a new generation on the target with a Hub-built preamble and the handoff you provide.

The workspace is never copied. An ordinary move works when both Runtime Brokers mount the **same NFS export**, so the target sees the very directory the source used (`transferMode: "export"`), uncommitted and unpushed changes included. A move that does not meet that requirement is refused unless you pass `--force` (`transferMode: "recreate"`). When a forced move would apply, a move without `--force` prints a hint pointing at `--dry-run --force` and `--force`. With `--force`, the target builds a fresh clone-per-agent or empty-per-agent workspace and the source removes its local workspace after the new generation starts.

```bash
# Check the move without changing anything
scion reincarnate my-agent --broker broker-b --dry-run

# Preview a forced move when the workspace is not on a shared NFS export
scion reincarnate my-agent --broker broker-b --dry-run --force

# Move it (when the workspace is carried through the shared NFS export)
scion reincarnate my-agent --broker broker-b --handoff-file handoff.md

# Execute a forced move that builds a fresh workspace on the target
scion reincarnate my-agent --broker broker-b --force --handoff-file handoff.md
```

### Requirements

- **Workspace transfer (`export` or `recreate`):**
  - **Shared NFS export (`transferMode: "export"`, default):** Set [`server.project_storage`](/scion/reference/server-config/#project-storage-serverproject_storage) to the `nfs` backend on both Runtime Brokers, with the same `server`, `export`, and `subpath_root` for the first share (`nfs.shares[0]`), which holds the workspaces. Each Runtime Broker also writes or reads an export identity marker, `.scion-export-id`, in `<subpath_root>` through its own mount, and the two must report the same marker, so `subpath_root` must exist and be writable. Matching settings alone are not enough: two brokers that configure the same address but mount different directories are refused. The Runtime Broker records where the agent's workspace is each time it starts the agent:
    - Shared-workspace (shared-plain) and Hub-managed workspaces are on the export on Docker, Podman, Apple `container`, Cloud Run, and Kubernetes with a PV claim (`nfs.shares[].pv_name`).
    - Clone-per-agent and empty-per-agent workspaces are on the export only on Kubernetes with a PV claim, so both the agent's current runtime and the target's profile must be Kubernetes.
    - An agent that has not started since its Runtime Broker began recording this has an unknown placement and is refused on `export`. Reincarnate it once without `--broker` to record it.
  - **Recreated workspace (`transferMode: "recreate"`, with `--force`):** When the export checks fail on `workspace_storage_reported`, `same_export`, `workspace_on_export`, or the Kubernetes export constraint on `target_profile`, `--force` waives those checks when the agent uses a clone-per-agent workspace (`per-agent` in a git project, which requires a git remote on the project) or an empty-per-agent workspace (`per-agent` in a project without git). The target Runtime Broker must advertise `agentMoveRecreate`, and neither the agent directory nor the workspace directory may already exist on the target (`409 move_target_not_fresh` otherwise).
  - Worktree-per-agent agents and agents in linked projects cannot move in either mode, and legacy workspaces recorded with a GCS storage path cannot move in `export` mode. Shared-workspace (`shared`) agents can move only in `export` mode, because recreating a project-wide shared directory on the target would split it across Runtime Brokers.
- **The target profile.** The profile the agent runs under (its own, else the project's active profile, else the target's default profile) must exist on the target and be available.
- **Both Runtime Brokers support agent move and are up to date.** Each must report the agent-move capability (and `agentMoveRecreate` on the target for a `recreate` move). An older Runtime Broker on either side gets `412 Precondition Failed`, and nothing is touched.
- **Both Runtime Brokers are online.** The target must be reachable (and report its NFS mount healthy for an `export` move, or for a `recreate` move onto a Kubernetes profile with NFS workspace storage; `503` otherwise). The source must be online too, because it removes its local copy of the agent after the move. A move off an offline source is refused with `412`.
- **Room on the target** under its [per-broker agent limit](#considerations) (`429` otherwise). This check only reads the current count; the limit is enforced when the quota moves, after the agent is stopped (see [What happens during a move](#what-happens-during-a-move)).

### What moves and what is regenerated

- **Moves:** the agent's identity and generation chain. In `export` mode, its workspace stays in place on the shared NFS export; only the Runtime Broker that runs the agent changes. The agent's quota reservation moves from the source to the target, and its exposed ports are cleared.
- **Regenerated:** the agent home (broker-local, built fresh from the template and harness config on the target, as any reincarnation does), and, in `recreate` mode, the workspace on the target (a fresh clone of the project's git remote for an agent with a clone-per-agent workspace, with the agent's branch checked out if it was pushed, otherwise created from the default branch, or a fresh empty workspace directory for an agent with an empty-per-agent workspace). In `recreate` mode, uncommitted, untracked, and unpushed files in the source workspace are not carried over, and the new generation's preamble notes that the workspace was recreated on the target Runtime Broker. Harness session history is not carried over; continuity comes from the handoff.
- **Experimental NFS home:** an agent with a persistent [NFS home](/scion/hosted/ha/kubernetes/#persistent-agent-home-nfs) (the `hub.k8s_nfs_home` experiment) has its home on the shared-dir export, not on the source. The target decides the home on its first start, from its own settings. If the target also has the NFS home enabled for the agent's profile (Kubernetes runtime, the experiment on, home storage `nfs`, and the same shared-dir share and `subpath_root`), it mounts the same home, so the home travels with the move. Otherwise the agent starts with a fresh local home on the target, and the old NFS home is left on the export untouched. Either way nothing is lost. The move's same-export check covers workspace storage only, not shared-dir storage.

### Checks and dry run

The Hub checks the move in a fixed order: `runtime_target`, `workspace_mode`, `workspace_storage_reported`, `same_export`, `workspace_on_export`, `workspace_recreatable`, `target_profile`, `target_health`, `access`, `capability`, and `capacity`. The first failing check decides the answer, and the CLI prints the transfer mode of an eligible move (`export` or `recreate`), every check (`passed`, `failed`, `not_evaluated`, or `waived`), and the move's `consequences` (what is `kept`, `carried`, `recreated`, and `lost`). With `--force`, the export checks run first so an eligible shared-export move still uses `transferMode: "export"`; only when they fail on `workspace_storage_reported`, `same_export`, `workspace_on_export`, or the Kubernetes export constraint on `target_profile` are those three export checks marked `waived`, `target_profile` re-evaluated for a recreated workspace, and `workspace_recreatable` evaluated for a fresh clone-per-agent or empty-per-agent workspace on the target (which must advertise `agentMoveRecreate`).

A refused move is refused **before any side effect**: the agent is not stopped, no quota changes, and the target is not linked to the project. `--dry-run` runs the same checks and returns the verdict and plan without doing anything else. Without `--dry-run`, the CLI sends a dry run first and stops if it is refused (printing the verdict on stderr), so a real request is only sent for an eligible move, and never to a Hub that does not support `--broker`, or, when `--force` is passed, to a Hub that does not support `--force`. An accepted move prints only the plan; preview a forced move's consequences with `--dry-run --force`. Patch flags such as `--model` or `--image` combine with `--broker`, and the checks judge the patched configuration.

### Permissions

Moving another agent needs `agent.lifecycle` on it, as any reincarnation does; an agent moving itself needs no permission unless it also passes patch flags. If you are not the agent, you must also be able to delegate its role: a non-admin reincarnating an agent with a privileged role gets `403` from this authority check before any of the move checks run. The agent keeps its existing delegator unless you also change its role with `--role`, in which case you become its recorded delegator. A user who moves an agent whose delegation chain has no recorded provenance also becomes its recorded delegator without a role change, as with any reincarnation (see [Identity & Access (RBAC)](/scion/hosted/ha/permissions/)).

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

The Hub accepts an eligible move with `202 Accepted` and runs it in the background. It re-runs the checks, stops the agent on the source, moves its quota, assigns it to the target, links the target to the project if needed, and provisions the agent on the target:

- In `export` mode, the target first confirms through its own mount that the workspace directory exists; if it does not, the target refuses to provision the agent, and the move fails and rolls back.
- In `recreate` mode, the target first checks that neither the agent directory nor the workspace directory already exists on the target; if either is present, it refuses with `409 move_target_not_fresh` without writing anything, and the move rolls back. Otherwise it creates the agent directory and a fresh clone-per-agent or empty-per-agent workspace, stamping each created directory with that move's reincarnation ID so a rollback can identify them.

The Hub then starts the new generation with the preamble and handoff, and finally asks the source to remove its local copy of the agent: its container, its broker-local agent directory, its home, and, in `recreate` mode, its source-local workspace directory. In `export` mode that cleanup never touches the export, and in neither mode does it delete the agent's remote git branch.

These steps run after the CLI has already received `202`, so a failure here does not come back as an HTTP error. The agent goes to the `error` phase, and its status message reads `reincarnation failed: …` with the reason, for example `provision on the target broker failed: …`.

- **Failure before the stop.** If the re-run checks or the stop fail, nothing has moved: the agent stays assigned to the source, in the `error` phase, and may still be running there.
- **No room on the target, or another failure after the stop.** Moving the agent's Runtime Broker quota reservation from the source to the target runs atomically in one store transaction: it checks the target's cap and frees the source slot only when the target reservation commits. If the target has no room when the quota moves (the dry-run capacity check only reads the current count), or anything else fails after the stop and before the agent is assigned to the target, the source quota reservation remains in place untouched and the agent is left stopped on the source in the `error` phase, for example with `reincarnation failed: target broker quota: …`. Nothing was moved; start it or retry the move later.
- **Rollback.** If anything fails after the agent is assigned to the target and before the new generation is running, including a start that definitely left no container, the Hub rolls the move back: it removes the agent's local state on the target (best effort; in `recreate` mode, removing only agent and workspace directories stamped with that move's reincarnation ID so any pre-existing directory on the target is left untouched), and restores the agent to the source with its quota, previous configuration, and source workspace intact. The agent is left stopped on the source, in the `error` phase; start it or retry the move. A provider link created by the move stays. If the Hub log warns that the agent's state on the target could not be removed, clean up the target as described under [Cleaning up stale state](#operator-notes-and-limits) below; the target is not running the agent.
- **Ambiguous start.** If the target's start fails in a way that may have left a container, the agent stays on the target in the `error` phase, with its quota there, and the source is not cleaned up.
- **Moving to a freshly linked target.** A target that has never hosted the project is linked to it by the move, just before it provisions the agent. In `export` mode, if the target cannot confirm the workspace through its mount, it refuses to provision the agent, and the move fails and rolls back as above; the agent's status message names the missing workspace, and nothing on the export is lost. Check that the target mounts the export and that the project's workspace is visible there, then retry. The provider link stays, so the retry no longer needs `project.update`.

### Operator notes and limits

The Hub records the outcome of the source cleanup on the move's reincarnation record (`sourceCleanup`, not shown by the CLI). A completed move counts as completed whatever the cleanup outcome; the Hub log has a warning when the cleanup failed or was skipped for lack of a run ID. The outcomes are:

- `done`: the source removed its local copy of the agent.
- `failed:<reason>`: the source could not remove it, and its local state is left in place.
- `skipped:no-run-id`: the agent had no run ID on the source, because an older version started it. Without one, the source could only find the agent by name, and on a Kubernetes namespace shared by both Runtime Brokers that could remove the agent's new pod, so the Hub leaves the source alone.
- `skipped:ambiguous-start`: the target's start may have left a container, so the agent stays on the target (see above).
- `skipped:interrupted`: the move failed before it completed. That covers a rollback, and a move cut short, for example by a Hub restart. The Hub moves the quota back to whichever Runtime Broker the agent is assigned to, and does not clean up the source.

**Cleaning up stale state.** If the agent ended up on the target (its Runtime Broker in `scion list` or the agent record is the target) and the source was not cleaned up, the source Runtime Broker may still hold the agent's old container or pod, its broker-local agent directory (in the project's directory under `~/.scion/projects/` on that broker), its home, and (after a `recreate` move) its old source-local workspace. The Hub does not remove this automatically. Remove the old container or pod with the runtime's own tools. On Kubernetes, check that its `scion.run_id` label is the source's run and not the agent's new pod, especially when both Runtime Brokers share a namespace. Then remove the broker-local agent directory. After a rollback the same applies the other way round: if the Hub log warns that the target's local state could not be removed, clean up the target this way. Do not clean up the Runtime Broker the agent is assigned to. **Never delete anything on the NFS export:** an `export`-mode workspace lives there and is in use.

## IAP-Protected Hubs

When the Hub is behind [Google IAP](/scion/hosted/ha/auth-proxy-iap/), **all** brokers connecting to it need transport auth configured. Each broker must carry an OIDC token to traverse the platform guard.

In a multi-hub setup using the broker's multistore, each hub connection can have its own transport settings:

- **`transportMode`** and **`transportAudience`** are per-connection fields in the credentials file. Different hubs may use different IAP OAuth client IDs.
- A single broker can serve both IAP-protected and plain (non-IAP) hubs simultaneously — connections without transport fields behave as before.
- Environment variables (`SCION_TRANSPORT_MODE`, `SCION_TRANSPORT_AUDIENCE`) apply globally and override all credential-file values. Use per-connection fields when serving hubs with different audiences.

See [Brokers behind IAP](/scion/hosted/ha/auth-proxy-iap/#brokers-behind-iap) for the full deployment guide.

## Considerations

- Each broker manages its own **port pools, container images, and local storage**. Images must be available on each broker independently.
- **Shared directories** (mounted volumes) only work within a single broker by default. Agents on different brokers cannot share a local directory. To share them across brokers, set [`server.project_storage.backend`](/scion/reference/server-config/#project-storage-serverproject_storage) to `nfs` in every broker's global settings, pointing at the same NFS export; shared directories then live on the export with the workspaces.
- **Workspace strategy** depends on the project's sharing mode: worktree-per-agent projects use a git worktree per agent over a shared base clone on the broker, shared-workspace projects mount one checkout, and other git projects use a per-agent clone.
- **Per-broker agent limit.** The Hub caps how many agents can be running on each broker with the `max_agents_per_broker` limit (default **100**). The limit is checked before an agent is created and again when it is started, resumed, or restarted. A request over the limit fails with `429 Too Many Requests` (`quota_exceeded`) instead of overloading the broker's host. A restart that is refused this way leaves the running container alone; it is not stopped first. Only running agents count: stopping, suspending, or exiting an agent frees its slot. Admins can change the hub-wide default through the [admin limits API](/scion/reference/api/#admin-apiv1admin), and override it for a single broker — including to a value *lower* than the hub-wide default — through that broker's settings (see [Broker Settings](/scion/reference/api/#broker-settings-apiv1runtime-brokersidsettings)). The Hub does not balance load across brokers; size the limit to each machine's resources, and lower it below the default if any broker in the fleet is a single-node Cloud Run deployment.
- **Global project on multi-hub brokers.** A broker connected to more than one Hub rejects agents in the global project with `409 Conflict` and the error code `global_project_disabled` (formerly `global_grove_disabled`).
