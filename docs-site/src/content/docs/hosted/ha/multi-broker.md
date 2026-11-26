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

A scheduled agent ([`dispatch_agent`](/scion/hosted/user/scheduling/)) has no `--broker` flag and goes through priorities 2 to 5 under the schedule's recorded authority, so it runs on the same broker as a manual create in that project. Where a manual create would ask for `--broker`, the fire fails instead; set the project's default broker so scheduled agents have one.

**Under `broker_policy.admin_managed_only`:** when the `hub.admin_managed_brokers` [experiment](/scion/reference/experiments/) and [`broker_policy.admin_managed_only`](/scion/reference/security/#7-admin-managed-runtime-brokers) are both enabled, non-managed Runtime Brokers are excluded from every rung of the selection cascade — including explicit `--broker`, project default, Hub default, single-provider auto-select, scheduled creates, agent-created agents, and harness-config image operations — even if the Runtime Broker is already linked as a project provider, and with no super-admin exemption. An explicit `--broker` that is not admin-managed, or a project whose online providers are all rejected solely by the policy, fails with `403 broker_policy_admin_managed_only` (`operation: "dispatch"`, with `details.eligibleBrokers: 0` when selection finds no eligible Runtime Broker). Existing stopped or suspended agents on a non-managed Runtime Broker also cannot be started, resumed, restarted, or woken (`operation: "start"`) until an administrator approves the Runtime Broker or moves the agent to an admin-managed Runtime Broker.

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

With the `hub.flat_runtime_brokers` [experiment](/scion/reference/experiments/) on (default on), the Hub accepts Runtime Brokers that each serve exactly one runtime target with a stable identity. A new agent placed on such a Runtime Broker is pinned to its runtime target and stays on it across restarts, and the Hub refuses a dispatch for a different target. The agent's record shows the pin as a read-only `pinnedRuntimeTarget`, and the Runtime Broker's record shows its `runtimeTarget` (see the [Agents API](/scion/reference/api/#agents-apiv1agents)). A pinned agent cannot be [moved to another Runtime Broker](#moving-an-agent-to-another-runtime-broker), and an agent cannot be moved onto a flat Runtime Broker (`409 runtime_target_move_unsupported`). Existing profile-based Runtime Brokers are unchanged.

The experiment also switches the web UI's **Brokers** pages to the Brokers area. The list shows each Runtime Broker's kind (flat or legacy), runtime target, and reported project storage layout (`server.project_storage`: backend, NFS server/export/`subpath_root`, and `homes`), and marks the Runtime Brokers that run in this server's own process. Each Runtime Broker's page has Overview, Agents & projects, Health and Configure tabs; the Overview and Health tabs also show the Runtime Broker's project storage layout and any layout mismatch with the Hub (when a mismatch list is absent on an unhealthy storage status, the page notes that layout agreement with the Hub is not confirmed). Health is shown only to users who can read the Hub health summary. Configure is shown only to users who can update or unregister the Runtime Broker. A legacy Runtime Broker's Overview tab also shows a read-only **Plan a move to flat Runtime Brokers** section (see [below](#planning-a-move-from-a-legacy-runtime-broker)). With the experiment off, the Brokers pages are unchanged.

To run flat Runtime Brokers, add one entry per instance under `server.broker.instances` in the global `settings.yaml`. Each entry has an immutable local `key`, a `name` that it registers under, and exactly one `runtime_target`: the local Docker daemon, a Kubernetes cluster and namespace, or (behind the `hub.flat_cloudrun_target` [experiment](/scion/reference/experiments/#server-experiments-with-notes), on a co-located Hub + Runtime Broker host) a Cloud Run project and region (`type: cloudrun`, activated using only the instance's own `runtime_target.credentials_file` and requiring `runtime_target.block_service_account` plus [`server.project_storage`](/scion/reference/server-config/#project-storage-serverproject_storage) with `backend: nfs` and `homes: nfs`; on a remote host without a co-located Hub, `cloudrun` instances are not activated).

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

Each of those four defaults keeps its usual precedence: an agent's own Kubernetes setting still wins over it, and an unset field falls back to the same global setting as before. By contrast, pod placement on a flat Kubernetes instance (`node_selector`, `tolerations`, and `runtime_class_name` on `runtime_target`) is owned by the instance: the instance's placement is applied to every agent pod, and on create, start, or restart an agent's `kubernetes.nodeSelector` may only add keys the instance does not set (never changing an operator key such as `scion.dev/pool`), each of its `kubernetes.tolerations` must match one of the instance's tolerations (with empty-key `Exists` always refused), and its `kubernetes.runtimeClassName` must be empty or match the instance's value—otherwise the operation is refused before any pod or volume is touched. (The same operator-owned placement rules also apply on regular Kubernetes Runtime Brokers via `runtimes.<name>` and `profiles.<name>`.) Before upgrading an existing Kubernetes deployment, run [`scion doctor`](/scion/reference/cli/#scion-doctor) (`flat-placement-preflight` on a flat instance host, `legacy-placement-preflight` on a regular Runtime Broker host) to find any existing agents whose stored placement disagrees with the operator policy; see [Flat instance placement](/scion/reference/server-config/#flat-instance-placement). Workspace, shared-directory and Agent home storage placement come from [`server.project_storage`](/scion/reference/server-config/#project-storage-serverproject_storage) for the whole deployment; the former `shared_dir_storage_backend`, `shared_dir_storage_backends`, `home_storage_backend` and `home_storage_leaf` keys on `runtime_target` are removed and rejected at load time (and any legacy per-agent `shared-dir-storage.json` record that pins `nfs` without shared project storage is refused at agent start with `422 storage_migration_required`). See [the runtime target fields](/scion/reference/server-config/#flat-instance-runtime-target-serverbrokerinstancesruntime_target) for every field.

An entry may also set `agent_defaults`: the image registry, default harness config, volumes, resources, harness overrides, required secrets and clone depth of the agents it runs. Each default sits where the matching profile setting sat, so templates, the agent's own configuration and the request keep winning where they won over a profile. See [flat instance agent defaults](/scion/reference/server-config/#flat-instance-agent-defaults-serverbrokerinstancesagent_defaults).

Each flat instance reports a read-only presentation of its runtime target and of its defaults for new agents (`runtimeTargetPresentation`) on its own heartbeats: the instance `key` and `name`; the target's `type`, display name, Cloud Run `region`, Kubernetes namespace and GKE flag, priority class, shared directory storage class and size, `safeToEvict`, `nodeSelector`, `tolerations`, `runtimeClassName`, and `placementReported`; and the agent defaults' default harness config, image registry, clone depth, resources, volume mounts (`target`, `readOnly`, `type`), secret declarations (`key`, `type`, `description`), and harness overrides (image, pull policy, auth type, user, environment variable names only, volumes, resources). Kubeconfig paths and contexts, Cloud Run `project_id`, `service_account`, `block_service_account`, `assignable_service_accounts`, `network`, `subnetwork`, and `credentials_file`, service-account policy, volume sources, secret targets, and environment values are never published, and credentials in the image registry are redacted. Except for the instance-owned pod placement fields (`nodeSelector`, `tolerations`, `runtimeClassName`), these are defaults for new agents, not the configuration of existing agents. The Overview tab on a flat Runtime Broker's page shows them in the **Runtime target** card, and `scion hub brokers info <broker>` prints them (`runtimeTargetPresentation` in `--json`).

#### Planning a move from a legacy Runtime Broker

To preview migrating a legacy Runtime Broker host to `server.broker.instances`, run `scion runtime-broker convert --plan` on the host, or read the publishable plan that a legacy Runtime Broker sends on its heartbeats at [`GET /api/v1/runtime-brokers/{id}/conversion-plan`](/scion/reference/api/#conversion-plan-apiv1runtime-brokersidconversion-plan) (requires `broker.read` and the `hub.flat_runtime_brokers` experiment; `404` while the experiment is off). The Hub-served view pairs the reported instance presentations, `profileMap`, and findings (`code`, `severity`, `path`, `profiles`) with the Hub's readable agents, readable projects, and proposed per-instance `maxAgents` capacity split.

With the experiment on, the Overview tab of a legacy Runtime Broker's page in the Brokers area shows this plan in a read-only **Plan a move to flat Runtime Brokers** section. It renders:
- **Proposed instances**: each instance's `key` (and `name` when different) and its published target, default, and Kubernetes placement rows (`instanceRows`).
- **Profile map**: each mapped profile and its proposed instance key, plus any profile left unmapped by a `blocking` finding, shown as `no mapping (<code>)`.
- **Findings**: a table of `severity` (`blocking` or `notice`), `code`, settings key `path`, `profiles`, and a fixed explanation per code (never the converter's raw message). When profiles that share a target are split onto separate instances (`profile_conflict` at `profiles.*`), the row includes a table of the published fields that differ between those instances (`unset` where absent on an instance), or `"These profiles differ in a setting that is not published to the Hub; run scion runtime-broker convert --plan on the host to see it."` when no published field differs.
- **Capacity**: the capacity outcome (`preserve_by_split`, `operator_decision_required`, or `no_limit`), the legacy `maxAgents` override when set, the proposed per-instance `allocations` (`instance`, `maxAgents`), and any `reasons` and `flags` (`proposed_instance_keys`).
- **Still bound on the Hub**: the non-terminal agents bound to the Runtime Broker that you can read (`id`, `name`, `projectId`, `phase`, `profile`, and `mapping`: `mapped` with `instance`, `no_mapping`, or `profile_unknown`), counts of unreadable agents (`hiddenCount`) and readable finished agents (`terminalCount`), and the projects the Runtime Broker provides that you can read (`id`, `name`, `activeProfile`, and `serviceAccountProfiles`, each with its mapped instance) plus `hiddenCount` for unreadable projects.

The section has no actions, and it never shows the settings block or a settings value the Hub does not receive: run `scion runtime-broker convert --plan` on the host for the full plan. For Cloud Run profiles, conversion folds `cloudrun.block_service_account` and `cloudrun.assignable_service_accounts` onto `runtime_target` (splitting proposed instances when profiles in a region differ), emits `flat_target_field_required` (`blocking`) when a setting with a legacy source is missing or invalid (`project_id` and `region`, plus `service_account`, `block_service_account`, `assignable_service_accounts`, and `network`/`subnetwork` on legacy `cloudrun` entries), and emits `flat_target_operator_input_required` (`notice`) for each required setting with no legacy source (`runtime_target.credentials_file` on every Cloud Run target, plus `runtime_target.service_account`, `runtime_target.block_service_account`, and `runtime_target.network` on legacy `cloudrun-instances` entries; `convert --plan` treats these as operator decisions and exits `3`, so copy the proposed YAML into `settings.yaml` and supply those fields rather than running `--apply`). When a plan has no blocking findings, needs no operator decision, and no non-terminal agent is bound to the legacy Runtime Broker on the Hub, run [`scion runtime-broker convert --apply`](/scion/reference/cli/#scion-runtime-broker) on the host to register the flat instances on the Hub, write `server.broker.instances` to `settings.yaml`, and link the instances to every project the Runtime Broker provides (recording the run in `<global>/runtime-brokers/.convert/` for `convert --status` and `convert --rollback`). On a standalone Runtime Broker host (`--host-mode=remote`), stop the local server before running `--apply` or `--rollback`. On a co-located host that runs the Hub and the Runtime Broker in the same `scion server start` process (`--host-mode=colocated`, with `--single-hub-replica` when the Hub database is Postgres), run `--apply` (or `--rollback`) first while the Hub is running so it can complete the Hub steps, then stop `scion server` and re-run the same command to complete the local `settings.yaml` write or restore. The web section shows a notice instead of a plan when the Runtime Broker has never reported one (`404 conversion_plan_not_reported`; upgrade the Runtime Broker or run `scion runtime-broker convert --plan` on the host), hides itself on a flat Runtime Broker (`409 runtime_broker_not_legacy`), and shows a warning banner when the Runtime Broker shortened the plan to fit the reporting limits (`plan_truncated`).

#### Inconsistent agent placement

Every operation that reaches a Runtime Broker for an existing pinned agent first checks the agent's saved placement: start, restart, stop, suspend, delete, reset-auth, messages, keys, logs, exec, attach, conduit streams and port proxying, workspace sync, and dispatches queued for a Runtime Broker that was offline. Reading the agent (get, list, status) is not checked. The placement is inconsistent when the agent's pin does not name the Runtime Broker it is assigned to, the pin is incomplete, the agent has no Runtime Broker, the Runtime Broker no longer serves a single runtime target, or that Runtime Broker serves another runtime target. The Hub then refuses the operation with `409 runtime_target_pin_stale` (or `409 runtime_target_mismatch`), or with `409 runtime_broker_missing` when the Runtime Broker the agent is pinned to is no longer registered. Nothing is sent to any Runtime Broker, and Scion never looks for another one to run the agent on.

An ordinary delete is refused in the same way, with or without `force` (when a delete's second delete, of the run the Runtime Broker reported holding, is refused this way, the first delete did reach the Runtime Broker, which deleted nothing): removing the agent's record without reaching its Runtime Broker could leave a running workload behind and release its capacity with no evidence that it was torn down. The agent's record and its accounting stay as they are. A stop that was queued while the Runtime Broker was offline and is refused this way when the Runtime Broker reconnects leaves the agent stopped with the container status `stop_queued`, and the agent keeps its Runtime Broker capacity slot until an inventory shows the container gone or the placement is repaired. No per-agent repair is available in this release; the recovery path is tracked in [ptone/scion#4844](https://github.com/ptone/scion/issues/4844).

Because such an agent's record stays, it also counts as a live agent of its project: a project delete without `force` is refused while it remains ([ptone/scion#4811](https://github.com/ptone/scion/issues/4811)). A project delete with `force` removes the project's agent records with only best-effort runtime deletes. That is an operator's explicit, project-wide choice; Scion never selects it on its own. The refusal of a project delete without `force` flags each listed agent whose placement is inconsistent with `placementRefusal` (the code its own delete answers) and links this section, so you can tell which agents a plain delete will not remove. For such an agent nothing is sent to any Runtime Broker: its workload is left running there with no agent record, the Hub logs an error naming the agent and its pinned Runtime Broker, and the workload has to be removed on that Runtime Broker by hand.

#### Capacity of flat Runtime Brokers

The `max_agents_per_broker` limit applies per Runtime Broker ID, and every flat instance has its own ID: two instances have two limits, and the sum of their limits is the host's effective ceiling. Filling one instance never consumes or bypasses another's capacity. Set each instance's cap with its `maxAgents` setting (see [Broker Settings](/scion/reference/api/#broker-settings-apiv1runtime-brokersidsettings)); `0` means unlimited, and an instance without the setting inherits the hub-wide default. An agent pinned to one instance that still holds a reservation on another is refused with `409 broker_reservation_scope_conflict`, and its reservation is never moved.

A legacy Runtime Broker's limit is not copied to the instances that replace it, because copying it to each instance would multiply the aggregate. The conversion rule is: a positive legacy `maxAgents` of L is split across n instances when L is at least n (by default floor(L/n) each, the remainder to the first instance; an operator may choose any positive caps that sum to at most L); with fewer than n slots, or when the legacy Runtime Broker only inherited a binding or the Hub default (each instance would inherit it, so the aggregate would become n times larger), an operator decision is required; a legacy `maxAgents` of `0` (unlimited) needs nothing. A plan computed while Runtime Broker quotas are not enforced, or whose receiving instance would get fewer slots than the legacy Runtime Broker's active reservations, is flagged. When profiles that share a runtime target differ in fields a flat instance can hold (`agent_defaults` or Kubernetes `runtime_target` placement/defaults), conversion splits them into one proposed instance per set of profiles with equal values, emitting a notice that names the differing fields and the resulting instances while preserving literal harness-override `env` and secret declarations per split instance.

#### GCP identity on flat Cloud Run and per-broker defaults

Flat `cloudrun` instances always apply native Cloud Run identity regardless of [`hub.cloudrun_native_identity`](/scion/reference/experiments/#server-experiments-with-notes), without sending service-account capability reports on heartbeats: the Hub determines native execution from the stored flat Runtime Broker's `runtimeTarget.type == "cloudrun"`, strips `gcp:token:*` scopes from the agent's Hub tokens, and refuses `/api/v1/agent/gcp-token` and `/api/v1/agent/gcp-id-token` (`403`). Every flat `cloudrun` instance must set `runtime_target.block_service_account` to a dedicated zero-role customer-managed GCP service account (distinct from `service_account`) and may optionally list up to 64 customer-managed GCP service accounts in `runtime_target.assignable_service_accounts`; [`scion doctor`](/scion/reference/cli/#scion-doctor) checks `iam.serviceAccounts.actAs` across `service_account`, `block_service_account`, and `assignable_service_accounts` using the instance's `credentials_file` credentials.

- **`unset`, `block`, and confined default `passthrough`**: run the Cloud Run instance as `runtime_target.block_service_account` with `SCION_METADATA_MODE=block` inside the container. Upgrading an existing flat `cloudrun` deployment without `block_service_account` fails config validation, and existing no-identity agents switch from `service_account` to `block_service_account` and recreate their Cloud Run instance on the next start (workspaces and Agent homes remain on NFS). The Hub's auth preflight treats these modes on flat Cloud Run as having no ambient GCP credentials (`agentGCPCredentialsAvailable = false`), so agents using Vertex AI or other Google Cloud auth need `assign`, explicit `passthrough`, or a default service-account rung.
- **`assign`**: runs the Cloud Run instance natively as the Hub-assigned service account when its email appears in `runtime_target.assignable_service_accounts` (init firewall refused), or fails before any ownership record or instance is created with `400` naming `server.broker.instances[<key>].runtime_target.assignable_service_accounts`.
- **Explicit `passthrough`**: runs the Cloud Run instance natively as `runtime_target.service_account` (no native marker required). Authorized when the caller is the broker owner, a Hub admin, or acting in a project with an owner-consented and dispatchable provider link (`auto_provide` or `broker.dispatch` on flat Runtime Brokers; Hub-own embedded host identities stay admin-only), audited with `authPath` precedence `owner > admin > provider_grant`. `runtime_target.service_account` is stored as the flat Cloud Run row's private GCP host pair (refreshed automatically on co-located activation without `HostIdentity` metadata observation, or claimed via remote `scion runtime-broker register --instance`) and redacted from broker read/list responses and user-facing `403` refusals unless the caller holds `update` on the broker. Under [`gcp_iam_check_mode: enforce`](/scion/reference/server-config/#gcp-iam-check-mode), the caller must hold `iam.serviceAccounts.actAs` on `runtime_target.service_account` before dispatch across every path (and on initial remote `register --instance`; changing `runtime_target.service_account` later on a remote instance requires clearing the stored host-SA pair or updating it via the API and re-running `scion runtime-broker register --instance` before continuing to serve provider-granted projects). With `gcp_iam_check_mode: "off"`, broker/project authority still applies while `actAs` enforcement is disabled across `owner`, `admin`, and `provider_grant` alike (logged as `mechanism=check-disabled` with `authPath`, admitting any agent-creating user in linked projects on `auto_provide` brokers). See [Passthrough Mode Security & PATCH Parity](/scion/hosted/ha/permissions/#passthrough-mode-security--patch-parity).
- **Per-broker project defaults**: because flat Runtime Brokers do not use profiles, projects configure broker-specific default service accounts (`assign` mode, up to 64 entries, evaluated between an explicit request identity and the project-wide default: `explicit > by-broker > project > hub`) in `defaultGCPIdentityServiceAccountIDByBroker`, gated on `hub.flat_runtime_brokers` (stored entries are ignored at ladder resolution while `hub.flat_runtime_brokers` is off). Use `scion project identity default set --broker <name|slug|id> <account>`, `scion project identity default unset --broker <name|slug|id>`, or `scion project identity default show`, or configure **Per Runtime Broker default service accounts** under **Project Settings > Configuration > Auth & Security** in the web UI (which also previews `Assign <SA> (inherited from project default for Runtime Broker "<name>")` on `/agents/new` when a flat Runtime Broker is selected). When a Runtime Broker rejects a default-supplied GCP identity with `400` on create, the Hub attributes the default rung and how to change or clear it. See [Per-Broker Default Service Accounts](/scion/hosted/ha/permissions/#per-broker-default-service-accounts).

## Moving an Agent to Another Runtime Broker

`scion reincarnate <agent> --broker <name|id>` moves an agent to another Runtime Broker, for example to drain a Runtime Broker or to reach different hardware or compute providers. The agent keeps its ID, slug, and generation chain. The move is a [reincarnation](/scion/reference/cli/#scion-reincarnate): the agent starts a new generation on the target with a Hub-built preamble and the handoff you provide.

### Transfer-mode ladder and `--force`

The Hub evaluates three transfer modes in strict order and picks the highest state-preserving mode that qualifies:

1. **`export` (shared NFS export, always preferred):** Both Runtime Brokers mount the same NFS export, so the target sees the existing workspace directory in place.
2. **`bundle` (portable archive via Hub object storage, experimental):** When `export` fails on a waivable export or placement check (`workspace_storage_reported`, `same_export`, `workspace_on_export`, or the Kubernetes export constraint on `target_profile`), the Hub evaluates `bundle` next — regardless of whether `--force` was passed. The source Runtime Broker stops the agent, streams compressed archives of the workspace and `$HOME` to Hub object storage (GCS), and the target Runtime Broker hydrates them before starting the new generation. Requires the [`hub.agent_move_bundle`](/scion/reference/experiments/#server-experiments-with-notes) experiment (alpha, off by default).
3. **`recreate` (fresh workspace on target, requires `--force`):** If neither `export` nor `bundle` applies and `--force` was passed, the target builds a fresh `clone-per-agent` or `empty-per-agent` workspace and the source removes its local workspace after the new generation starts.

`--force` means **"allow losing local state if no preserving mode applies."** Passing `--force` never downgrades an eligible `export` or `bundle` move: if `export` applies, `--force` prints `--force was not needed: the workspace is carried through the shared NFS export.`; if `bundle` applies, `--force` prints `--force was not needed: the workspace and home are copied through hub storage.`.

If `export` fails on a non-waivable check (`runtime_target`, `workspace_mode`, non-placement `target_profile`, `target_health`, `access`, `capability`, or `capacity`), the move is refused immediately without evaluating `bundle` or `recreate`. When `export` fails on a waivable check and `bundle` also fails, the Hub attaches `bundleUnavailable` to the verdict and the CLI prints `bundle transfer unavailable: <reason>` above the `--force` hint (and also prints it on a forced `recreate` dry run so you can see why `bundle` did not apply).

```bash
# Check the move without changing anything (shows selected transfer mode and checks)
scion reincarnate my-agent --broker broker-b --dry-run

# Preview a forced move when neither shared NFS export nor bundle transfer applies
scion reincarnate my-agent --broker broker-b --dry-run --force

# Move the agent (selects export, or bundle when eligible)
scion reincarnate my-agent --broker broker-b --handoff-file handoff.md

# Execute a forced move (still uses export or bundle if eligible; otherwise recreates)
scion reincarnate my-agent --broker broker-b --force --handoff-file handoff.md
```

### What moves and what is regenerated

| State | `export` | `bundle` | `recreate` (`--force`) |
| :--- | :--- | :--- | :--- |
| **Agent identity, relationships, config** | Kept (new generation) | Kept (new generation) | Kept (new generation) |
| **Workspace** (`.git`, uncommitted changes, unpushed commits, untracked files) | Kept in place on the shared NFS export | Copied through Hub storage (including `.git`, file modes, empty dirs, internal relative symlinks; excluding root `.scion` and workspace-targeted file secrets) | Recreated fresh on target (fresh git clone or fresh empty dir); uncommitted, untracked, and unpushed files on source are **lost** |
| **Agent home (`$HOME`)** | Regenerated on target (unless experimental K8s NFS home on the same share; see below) | Copied through Hub storage, except secrets, credentials, and caches; target provision-rendered files win over bundled files | Regenerated fresh from template and harness-config on target |
| **Harness session** | Recreated fresh (no conversation history; continuity from handoff) | Recreated fresh (continuity from handoff; conversation history files are kept in `$HOME`) | Recreated fresh (no conversation history; continuity from handoff) |
| **Credentials & agent token** | Regenerated fresh on target | Regenerated fresh on target | Regenerated fresh on target |
| **Running processes & open ports** | Lost (ports cleared) | Lost (ports cleared) | Lost (ports cleared) |

- **Experimental NFS home (`export` mode):** an agent with a persistent [NFS home](/scion/hosted/ha/kubernetes/#persistent-agent-home-nfs) (the `hub.k8s_nfs_home` experiment) has its home on the shared-dir export, not on the source. In `export` mode, the target decides the home on its first start from its own settings. If the target also has NFS home enabled for the agent's profile (Kubernetes runtime, the experiment on, home storage `nfs`, and the same shared-dir share and `subpath_root`), it mounts the same home, so the home travels with the move. Otherwise the agent starts with a fresh local home on the target, and the old NFS home is left on the export untouched. The `export` same-export check covers workspace storage only, not shared-dir storage.

### Requirements

- **Workspace transfer (`export`, `bundle`, or `recreate`):**
  - **Shared NFS export (`transferMode: "export"`, default):** Set [`server.project_storage`](/scion/reference/server-config/#project-storage-serverproject_storage) to the `nfs` backend on both Runtime Brokers, with the same `server`, `export`, and `subpath_root` for the first share (`nfs.shares[0]`), which holds the workspaces. Each Runtime Broker also writes or reads an export identity marker, `.scion-export-id`, in `<subpath_root>` through its own mount, and the two must report the same marker, so `subpath_root` must exist and be writable. Matching settings alone are not enough: two Runtime Brokers that configure the same address but mount different directories are refused. The Runtime Broker records where the agent's workspace is each time it starts the agent:
    - Shared-workspace (shared-plain) and Hub-managed workspaces are on the export on Docker, Podman, Apple `container`, Cloud Run, and Kubernetes with a PV claim (`nfs.shares[].pv_name`).
    - Clone-per-agent and empty-per-agent workspaces are on the export only on Kubernetes with a PV claim, so both the agent's current runtime and the target's profile must be Kubernetes.
    - An agent that has not started since its Runtime Broker began recording this has an unknown placement and is refused on `export`. Reincarnate it once without `--broker` to record it.
  - **Bundle transfer (`transferMode: "bundle"`, experimental):** Requires all of the following:
    1. The [`hub.agent_move_bundle`](/scion/reference/experiments/#server-experiments-with-notes) experiment enabled on the Hub.
    2. HTTP-signable Hub object storage configured (`gcs` under [`server.storage`](/scion/reference/server-config/#storage-serverstorage)); `local` storage cannot issue signed URLs usable by remote Runtime Brokers.
    3. Both source and target Runtime Brokers online and advertising `agentMove` and `agentMoveBundle` (and neither is a flat Runtime Broker).
    4. Workspace sharing mode is `clone-per-agent` or `empty-per-agent` (any git remote state, including no git remote).
    5. A durable source runtime (see [Runtime support for bundle transfer](#runtime-support-for-bundle-transfer)) whose files fit within the Hub's [`server.reincarnate.move_bundle`](/scion/reference/server-config/#reincarnate-serverreincarnate) limits and have a broker-owned start-time home-exclusion record (`.scion-move-bundle-home.json`). On Kubernetes, [`hub.k8s_nfs_home`](/scion/reference/experiments/#server-experiments-with-notes) must also be enabled (with the agent started after enabling it), and `$HOME` files on an NFS home owned by the pod user are probed for readability during precheck.
  - **Recreated workspace (`transferMode: "recreate"`, with `--force`):** When `export` fails on `workspace_storage_reported`, `same_export`, `workspace_on_export`, or the Kubernetes export constraint on `target_profile` and `bundle` does not apply, `--force` waives those checks when the agent uses a `clone-per-agent` workspace (`per-agent` in a git project, which requires a git remote on the project) or an `empty-per-agent` workspace (`per-agent` in a project without git). The target Runtime Broker must advertise `agentMoveRecreate`, and neither the agent directory nor the workspace directory may already exist on the target (`409 move_target_not_fresh` otherwise).
  - Worktree-per-agent agents and agents in linked projects cannot move in any mode, and legacy workspaces recorded with a GCS storage path cannot move in `export` mode. Shared-workspace (`shared`) agents can move only in `export` mode, because bundling or recreating a project-wide shared directory on the target would fork it across Runtime Brokers.
- **The target profile.** The profile the agent runs under (its own, else the project's active profile, else the target's default profile) must exist on the target and be available.
- **Both Runtime Brokers support agent move and are up to date.** Each must report `agentMove` (plus `agentMoveBundle` on both sides for `bundle`, or `agentMoveRecreate` on the target for `recreate`). An older Runtime Broker on either side gets `412 Precondition Failed`, and nothing is touched.
- **Both Runtime Brokers are online.** The target must be reachable (and report its NFS mount healthy for an `export` move, or for a `recreate` move onto a Kubernetes profile with NFS workspace storage; `503` otherwise). The source must be online too, because it exports the bundle (in `bundle` mode) and removes its local copy of the agent after the move. A move off an offline source is refused with `412`.
- **Room on the target** under its [per-broker agent limit](#considerations) (`429` otherwise). This check only reads the current count; the limit is enforced when the quota moves, after the agent is stopped (see [What happens during a move](#what-happens-during-a-move)).

### Runtime support for bundle transfer

Bundle transfer snapshots workspace and `$HOME` **after** the source Runtime Broker stops the agent so files are quiescent (no torn git index or half-written history). A runtime is eligible for `bundle` only when both workspace and `$HOME` survive `Stop` on a broker-accessible filesystem:

| Runtime | Source & Target | Requirement / Reason if refused |
| :--- | :--- | :--- |
| `docker`, `podman`, `container` (Apple Container) | Supported | Host bind mounts survive stop. Shared-layout and git worktree workspaces are refused. |
| `kubernetes` (NFS) | Supported | Requires the [`hub.k8s_nfs_home`](/scion/reference/experiments/#server-experiments-with-notes) experiment on (with the agent started after enabling it), `server.project_storage` with `backend: nfs`, `homes: nfs`, `homes_options.allow_incomplete_phases: true` (see [Agent home storage](/scion/reference/server-config/#agent-home-storage-serverhome_storage)), a share with `pv_name` mounted on the Runtime Broker, and `home-storage.json` recording `backend: "nfs"`. An agent started with `homes: nfs` while `hub.k8s_nfs_home` is off gets a pod-local home and is refused. Target provision writes `.scion-home-seed.json` so `home-prepare` runs in seed-over mode. |
| `cloudrun`, `cloudrun-instances` (NFS) | Supported | Requires `server.project_storage` with `backend: nfs`, `nfs.mount_root`, and `shares[0]` (`id`, `server`, `export`) mounted on the Runtime Broker, and a per-agent workspace (legacy shared-checkout Cloud Run agents are refused). Both workspace and home live on the workspace NFS share; target Start seeds provision-rendered home files over the imported leaf. |
| Plain `kubernetes` pods (non-NFS workspace or pod-local home) | Refused | `Stop` deletes the pod, so pod-local workspace and/or home do not survive stop (`workspace and home live only in the pod and are deleted on stop` / `agent home lives only in the pod and is deleted on stop`). |
| `cloudrun-sandbox` | Refused | `Stop` deletes the sandbox, storage is container-ephemeral and slug-keyed (not project-scoped), and home is not persistent (`Cloud Run Sandbox workspace and home are not durable for bundle moves`). `agentMoveBundle` is not advertised. |
| `substrate` (and unknown runtimes) | Refused | Not supported (`not yet supported`). `agentMoveBundle` is not advertised. |

### Bundle limits and configuration

Bundle transfer bounds bytes, entry counts, directory depth, path length, archive part size, and job timeouts. Configure them on the Hub under `server.reincarnate.move_bundle` in `settings.yaml` (startup file only; see [Reincarnate (`server.reincarnate`)](/scion/reference/server-config/#reincarnate-serverreincarnate)). The Hub sends the resolved limits to the Runtime Brokers on every precheck, export, and import request; Runtime Brokers reject requests with missing or non-positive limits and do not apply their own defaults.

| Config key (`server.reincarnate.move_bundle.*`) | Default | Notes |
| :--- | :--- | :--- |
| `workspace_max_bytes` | `4294967296` (4 GiB) | Uncompressed regular-file bytes in the workspace. |
| `workspace_max_entries` | `250000` | Total files, directories, and symlinks in the workspace. |
| `home_max_bytes` | `1073741824` (1 GiB) | Uncompressed regular-file bytes in `$HOME` after exclusions. |
| `home_max_entries` | `100000` | Total entries in `$HOME` after exclusions. |
| `max_depth` | `64` | Maximum directory depth per root. |
| `max_path_len` | `4096` | Maximum relative path length in bytes per root. |
| `part_size` | `67108864` (64 MiB) | Compressed tar.gz part size (`maxParts = ceil(max_bytes / part_size) + 8`; reader refuses manifest part sizes under 1 MiB unless equal to configured `part_size`). |
| `export_timeout` | `"20m"` | Whole source export job timeout. |
| `import_timeout` | `"20m"` | Whole target import job timeout. |
| `part_stall_timeout` | `"2m"` | Per-part no-progress transfer timeout. |

### Bundle exclusions and target merge

Exclusions are evaluated on the **source Runtime Broker** before any byte leaves the host, using the broker-owned start-time record `<agentDir>/.scion-move-bundle-home.json` (written when the agent starts) unioned with Hub-resolved file secret targets:

- **Workspace:** excludes root `.scion/**` plus any workspace-targeted file secrets and auth files. `.git/**` **is** included.
- **`$HOME` baseline Scion files:** `.scion/scion-token`, `.scion/scion-token.tmp`, `.scion/secrets.json`, `.scion/harness/secrets`, `.scion/harness/outputs`, `.scion-home-reset-id`, and temporary files matching Scion's guarded-writer naming patterns.
- **`$HOME` per-agent secrets and harness auth:** all file-type secrets and harness auth files recorded at start or resolved by the Hub.
- **`$HOME` identity-bound credentials:** `.config/gcloud/credentials.db`, `.config/gcloud/access_tokens.db`, `.config/gcloud/application_default_credentials.json`, `.config/gcloud/legacy_credentials/**`, `.docker/config.json`, and `.kube/config`.
- **`$HOME` caches:** `.cache/**`, `.npm/_cacache/**`, and `go/pkg/mod/cache/**`.
- **Hard links and symlinks:** on Unix, hard-linked copies of excluded secret and credential files are also skipped (failing closed if the credential hard-link scan cap is exceeded). Relative symlinks whose target stays inside the root are preserved; absolute or escaping leaf symlinks (such as Kubernetes `/scion-staging/...` secret links) are skipped and counted, while unresolvable ancestor symlinks fail closed. Sockets, FIFOs, and device nodes are skipped and counted.
- **Case-insensitive hosts:** on macOS (`darwin`) and Windows (`windows`) Runtime Brokers, exclusion prefixes match case-insensitively so case variants created through APFS/HFS+/NTFS mounts (for example `.Docker/config.json`) cannot bypass exclusions. Linux stays case-sensitive for `$HOME` and workspace extra exclusions; workspace root `.scion` is always matched case-insensitively on every OS.
- **Target commit:** workspace entries are renamed into the freshly provisioned empty workspace directory. For `$HOME`, **provision-rendered files on the target win**: existing files rendered by target provisioning (harness config, settings, `agent-info.json`, skills, injected auth) are kept, and bundled files fill in everything else (history, dotfiles, tool state). On Kubernetes NFS, `home-prepare` runs in seed-over mode; on Cloud Run NFS, Cloud Run start seeds target-rendered files over the imported leaf.

### Checks, dry run, and `bundleUnavailable` reasons

The Hub evaluates `export` checks in order: `runtime_target`, `workspace_mode`, `workspace_storage_reported`, `same_export`, `workspace_on_export`, `workspace_recreatable`, `target_profile`, `target_health`, `access`, `capability`, and `capacity`.

When `export` fails on a waivable check (`workspace_storage_reported`, `same_export`, `workspace_on_export`, or the Kubernetes placement constraint on `target_profile`), the Hub evaluates `bundle` in this order: `runtime_target`, `workspace_mode`, `workspace_storage_reported` (waived), `same_export` (waived), `workspace_on_export` (waived), `bundle_experiment`, `workspace_bundleable`, `bundle_storage`, `target_profile`, `target_health` (reachability only), `access`, `capability` (both sides `agentMove` + `agentMoveBundle`, source online), `bundle_source` (precheck RPC for durability, readability, and limits), and `capacity`.

The CLI prints the selected transfer mode (`export`, `bundle` with estimated size, or `recreate`), every check (`passed`, `failed`, `not_evaluated`, or `waived`), and the move's `consequences` (`kept`, `carried`, `recreated`, and `lost`).

Common `bundleUnavailable` / precheck refusal reasons and how to resolve them:

| Reason (`bundleUnavailable`) | Cause and next step |
| :--- | :--- |
| `bundle transfer requires the hub.agent_move_bundle experiment` | Enable `hub.agent_move_bundle` in **Admin → Server Config → Experiments** or via `scion hub experiments enable hub.agent_move_bundle`. |
| `bundle transfer supports clone-per-agent and empty-per-agent workspaces; this agent's workspace is <mode>` | Shared-plain or worktree-per-agent workspaces cannot use bundle transfer. Use a shared NFS export (`export` mode) for shared-plain workspaces. |
| `hub object storage is not configured` | Configure GCS under [`server.storage`](/scion/reference/server-config/#storage-serverstorage) on the Hub. |
| `hub object storage provider "<provider>" cannot issue signed URLs a remote broker can use` | Switch Hub object storage from `local` (or another non-HTTP-signable provider) to `gcs`. |
| `broker <name> does not support bundle move; upgrade it` | Upgrade the named Runtime Broker, or confirm its runtime supports bundle moves (and has NFS configured for Kubernetes or Cloud Run). |
| `restart the agent once to enable bundle moves` | The agent has no start-time home-exclusion record (`.scion-move-bundle-home.json`). Restart or reincarnate the agent once in place, then retry the move. |
| `bundle exceeds limit: <detail>` (for example `workspace bytes (...) exceed configured limit` or `home entries (...) exceed configured limit`) | Remove large or numerous files from the workspace or `$HOME`, or raise the corresponding [`server.reincarnate.move_bundle`](/scion/reference/server-config/#reincarnate-serverreincarnate) limit on the Hub. |
| `workspace and home live only in the pod and are deleted on stop` / `agent home lives only in the pod and is deleted on stop` | Plain Kubernetes pods lose state on stop (and a Kubernetes agent started with `homes: nfs` while `hub.k8s_nfs_home` is off records a pod-local home). Enable [`hub.k8s_nfs_home`](/scion/reference/experiments/#server-experiments-with-notes), configure NFS workspace and NFS home storage (`homes: nfs`, `homes_options.allow_incomplete_phases: true`), and start the agent once after enabling them — or pass `--force` to recreate. |
| `Cloud Run requires NFS project storage for bundle moves` | Configure NFS `server.project_storage` on the Cloud Run Runtime Broker. |
| `Cloud Run Sandbox workspace and home are not durable for bundle moves` | Cloud Run Sandbox is refused for bundle moves. Pass `--force` to recreate, or run on Cloud Run Instances (NFS). |
| `NFS workspace or home export is not mounted on this broker` / `recorded NFS home share is not configured on this broker` / `agent home storage record is unreadable` | Verify the NFS mount and `server.project_storage` / home share configuration on the source Runtime Broker. |
| `broker lacks read permission on source home` | Precheck probes `$HOME` files on an NFS home owned by the pod user and detected files unreadable by the Runtime Broker user (for example `0600` pod-owned files on a `root_squash` NFS export). Fix export permissions or file modes so the Runtime Broker can read them. (Unreadable workspace files are not probed during precheck and instead surface during export as `bundle export failed:`.) |
| `shared layout workspace is not yet supported for bundle moves` / `shared layout workspace is not supported for bundle moves` / `worktree workspace is not supported for bundle moves` | Bundle transfer requires an isolated `clone-per-agent` or `empty-per-agent` workspace (not a shared layout or git worktree). |
| `source <root> contains an unsupported path` / `source <root> contains an unportable symlink or hardlink` / `source <root> cannot be bundled` | Remove or fix non-portable filenames, unresolvable ancestor symlinks, or other unreadable/invalid entries in the named root (details are logged at `Warn` on the source Runtime Broker). |
| `a previous move-bundle job is still running` | Wait for the in-flight bundle export or import job on the source Runtime Broker to finish, then retry. |
| `not yet supported` | The source runtime (for example `substrate`) does not support bundle transfer. |
| `source bundle precheck failed` / `could not resolve the agent's secret targets` | Check source Runtime Broker connectivity and Hub secret backend health, then retry. |

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

When [`broker_policy.admin_managed_only`](/scion/reference/security/#7-admin-managed-runtime-brokers) is active, the target Runtime Broker must also be admin-managed (embedded, auto-provide, or explicitly approved); moving to a non-managed Runtime Broker is refused with `403 broker_policy_admin_managed_only` (`operation: "move"`), including for super-admins. Moving an agent off a non-managed Runtime Broker onto an admin-managed Runtime Broker is allowed and is the supported way to migrate existing agents.

### What happens during a move

The Hub accepts an eligible move with `202 Accepted` and runs it in the background. It re-runs the checks (including the pre-stop `bundle_source` precheck in `bundle` mode), stops the agent on the source, and then:

- **In `export` mode:** moves quota, assigns the agent to the target, links the target if needed, and provisions on the target. The target first confirms through its own mount that the workspace directory exists; if it does not, the target refuses to provision and the move rolls back.
- **In `bundle` mode:**
  1. **Export:** the Hub starts an export job on the source Runtime Broker, which streams gzip-compressed tar parts of `workspace` and `home` to Hub object storage under `hubs/<hub-id>/move-bundles/<reincarnation-id>/` (or `move-bundles/<reincarnation-id>/` when `hub_id` is unset) using 15-minute single-object signed `PUT` URLs bound to exact byte sizes. The Hub polls every 2 seconds (tolerating up to 3 consecutive transient transport/5xx errors), updates `agent.Message` (`migrating: uploading snapshot ...`, throttled every 10 seconds), and touches the reincarnation record's `updated_at` every 30 seconds so long transfers are never reaped by the 30-minute stale-reincarnation sweep.
  2. **Verify manifest:** the Hub verifies every part exists in object storage with its declared size and within limits, then writes `manifest.json` and records its canonical SHA-256 hash.
  3. **Assign & provision:** the Hub moves quota, assigns the agent to the target, links the target if needed, and provisions a fresh marker-guarded agent directory on the target with an empty workspace directory (`workspaceSource: "move-bundle"`) and normal home rendering.
  4. **Import:** the Hub starts an import job on the target Runtime Broker using 15-minute signed `GET` URLs (proactively refreshed before expiry). Each part's size and SHA-256 are verified before decompression into a sibling `.bundle-*` staging directory, then committed into the workspace and merged into `$HOME` (with provision-rendered files winning). The Hub polls and updates `agent.Message` (`migrating: restoring snapshot ...`).
- **In `recreate` mode:** moves quota, assigns the agent to the target, links the target if needed, and checks that neither the agent directory nor the workspace directory already exists on the target (`409 move_target_not_fresh` otherwise). It creates the agent directory and a fresh `clone-per-agent` or `empty-per-agent` workspace, stamping each created directory with `.scion-move` carrying the reincarnation ID so a rollback can identify them.

The Hub then starts the new generation with the preamble and handoff (`resume=false`), asks the source to remove its local copy of the agent (its container, broker-local agent directory, home, and — in `bundle` or `recreate` mode — its source-local workspace directory; in `export` mode cleanup never touches the export, and in no mode does it delete the remote git branch), and in `bundle` mode deletes the staging prefix in object storage best-effort.

These steps run after the CLI has already received `202`, so a failure here does not come back as an HTTP error. The agent goes to the `error` phase, and its status message reports the reason (signed URLs are always redacted from errors and logs):

- **Failure before the stop (recheck / precheck / stop):** fails from `pending`. Nothing has moved: the agent stays assigned to the source, untouched, and may still be running there. No staging objects are created.
- **Bundle export or manifest verification failure:** fails from `stopping` before target assignment. The reincarnation record's error is prefixed with `bundle export failed:` (or `bundle exceeds limit:`), the agent's status message is set to `move failed: <err>; agent state is intact on <src>; \`scion start\` to resume`, source files are untouched (export is read-only), and the Hub deletes the staging prefix best-effort. A broker restart mid-export returns `404` on poll and fails definitively on this path.
- **No room on the target, or another failure after the stop and before assignment:** Moving the agent's Runtime Broker quota reservation from the source to the target runs atomically in one store transaction: it checks the target's cap and frees the source slot only when the target reservation commits. If the target has no room when the quota moves (the dry-run capacity check only reads the current count), or anything else fails after the stop and before the agent is assigned to the target, the source quota reservation remains in place untouched and the agent is left stopped on the source in the `error` phase, for example with `reincarnation failed: target broker quota: …`. Nothing was moved; start it or retry the move later.
- **Rollback (provision, bundle import, or definitive start failure):** If anything fails after the agent is assigned to the target and before the new generation is running — including a bundle import failure (`bundle import failed:`), import timeout, broker restart (`404`), or a start that definitely left no container — the Hub rolls the move back. It removes the agent's state on the target best-effort (in `bundle` and `recreate` modes, using marker-guarded deletion that removes only directories stamped with `.scion-move` for that reincarnation ID plus any `.bundle-*` staging siblings, leaving any pre-existing directory untouched), deletes the staging prefix best-effort, and restores the agent to the source with its quota, previous configuration, and source workspace and `$HOME` intact. The agent is left stopped on the source in the `error` phase; start it or retry the move. A provider link created by the move stays. If the Hub log warns that the agent's state on the target could not be removed, clean up the target as described under [Cleaning up stale state](#operator-notes-and-limits) below; the target is not running the agent.
- **Ambiguous start:** If the target's start fails in a way that may have left a container, the agent stays on the target in the `error` phase with its quota there, and the source is not cleaned up (`skipped:ambiguous-start`). In `bundle` mode, the staging prefix is deleted only after the reincarnation record transitions to terminal `failed`.
- **Moving to a freshly linked target:** A target that has never hosted the project is linked to it by the move, just before it provisions the agent. In `export` mode, if the target cannot confirm the workspace through its mount, it refuses to provision the agent, and the move fails and rolls back as above; the agent's status message names the missing workspace, and nothing on the export is lost. Check that the target mounts the export and that the project's workspace is visible there, then retry. The provider link stays, so the retry no longer needs `project.update`.

### Bundle staging cleanup and orphan sweep

In `bundle` mode, temporary archives are staged under `hubs/<hub-id>/move-bundles/<reincarnation-id>/` (or `move-bundles/<reincarnation-id>/` when `server.hub.hub_id` is unset). Cleanup runs at three layers:

1. **Immediate terminal cleanup:** the move worker deletes the staging prefix best-effort as soon as the move reaches `completed` or `failed`.
2. **Periodic orphan sweep:** the Hub's recurring `"reincarnation-sweep"` job (every 5 minutes) also sweeps `move-bundles/` whenever Hub object storage is configured — **even when `hub.agent_move_bundle` is turned off**, so leftovers from earlier moves are still reclaimed. Each pass inspects up to 100 `<reincarnation-id>/` prefixes and deletes a prefix only when its reincarnation record is missing or terminal (`completed` or `failed`) with `UpdatedAt` older than **1 hour** (strictly greater than the 15-minute signed-URL TTL). Non-terminal moves are never touched.
3. **GCS bucket lifecycle backstop:** configure a 7-day GCS bucket lifecycle rule for `move-bundles/` so orphaned staging objects cannot accumulate if the Hub is down or decommissioned. See [Deploy on GCP — 1c. GCS Bucket](/scion/hosted/ha/setup-gcp/#1c-gcs-bucket) for the recommended lifecycle rule and `gcloud` command.

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
