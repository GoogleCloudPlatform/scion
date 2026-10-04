---
title: Running Scion on Kubernetes
---

Scion supports running agents as Pods in a Kubernetes cluster. This enables remote execution, resource management, and scaling beyond a single machine.

## Prerequisites

- A running Kubernetes cluster (GKE, EKS, AKS, or self-managed).
- Kubeconfig file configured with access to the target cluster (or running within the cluster using In-Cluster Authentication).
- Scion agent images available to the cluster (pushed to a container registry accessible by the cluster).
- Appropriate RBAC permissions for pod creation, execution, and secret management.

:::note
Scion utilizes the native Kubernetes Go client API for operations like `exec` and `attach`. The `kubectl` binary is **not** required on the host machine.
:::

Use `scion doctor` to verify prerequisites before starting agents.

## Configuration

Configure the Kubernetes runtime in your global `~/.scion/settings.yaml`:

```yaml
runtimes:
  k8s:
    type: kubernetes
    context: my-cluster-context    # kubectl context (optional, defaults to current)
    namespace: scion-agents        # target namespace (default: "default")
    gke: false                     # enable GKE-specific features
    list_all_namespaces: false     # list agents across all namespaces
    priority_class_name: scion-agent-priority  # default PriorityClass for agent pods (optional)
    # shared_dir_storage_class: standard-rwx  # RWX class for shared-dir PVCs (see below)
    # shared_dir_size: 10Gi                    # size per shared-dir PVC
    kubernetes_service_account_mappings:  # GCP identity mode "assign" — see below
      agent-worker@my-project.iam.gserviceaccount.com: agent-worker-ksa

profiles:
  default:
    runtime: k8s
    # kubernetes_service_account_mappings can also be set per profile, to
    # override the runtime-level mapping above for a given GSA email.
```

### Brokers with More Than One Runtime

A Runtime Broker can serve several runtimes through its profiles, for example a Docker default plus one or more Kubernetes profiles. Besides its default runtime, the broker tracks each distinct auxiliary runtime separately: Kubernetes runtimes are told apart by context and namespace, so two profiles that target different clusters or namespaces are never merged. Every per-agent path follows the runtime the agent was dispatched to, not the broker default: the Hub endpoint rewrite, extra hosts and the worktree check at dispatch, the runtime reported in the create response (and recorded by the Hub), and start and restart, which look the agent up on the default runtime and then on each auxiliary runtime. For example, an agent on a Kubernetes profile of a Docker-default broker gets the Kubernetes defaults, such as its GCP identity mode, rather than the Docker ones.

### Agent-Level Kubernetes Configuration

Per-agent or per-template Kubernetes settings in `~/.scion/settings.yaml`:

```yaml
kubernetes:
  namespace: custom-namespace          # override runtime namespace
  context: alternate-context           # override runtime context
  serviceAccountName: agent-sa         # Workload Identity / IRSA — see the
                                        # GCP Identity Mode "assign" section
                                        # below if this agent also uses GCP
                                        # identity mode "assign": a value set
                                        # here directly on the create/start
                                        # request is rejected if it names a
                                        # different KSA than the mapping; a
                                        # template-only value is silently
                                        # overridden by the mapping instead.
  runtimeClassName: gvisor             # sandboxed runtime (gVisor, Kata, etc.)
  priorityClassName: scion-agent-priority  # overrides the runtime-level default, if any
  safeToEvict: false                   # ask the autoscaler not to evict the pod (see below)
  imagePullPolicy: IfNotPresent        # Always, IfNotPresent, or Never
  nodeSelector:
    pool: agents
    accelerator: gpu
  tolerations:
    - key: dedicated
      operator: Equal
      value: agents
      effect: NoSchedule
  resources:
    requests:
      nvidia.com/gpu: "1"
    limits:
      nvidia.com/gpu: "1"
```

### Shared Directory PVCs

Each project [shared directory](/scion/local/workspace/#5-project-shared-directories) gets its own `ReadWriteMany` PersistentVolumeClaim, created on first use and reused by later agents in the same project. Two keys control these claims:

| Key | Default | Description |
|---|---|---|
| `shared_dir_storage_class` | cluster default class | StorageClass for new shared-dir PVCs. It must support `ReadWriteMany`. |
| `shared_dir_size` | `10Gi` | Requested size for each new shared-dir PVC. |

You can set them in three places. Each key is resolved separately, and the first source that sets it wins:

1. The agent's or template's `kubernetes:` block.
2. The profile entry in `settings.yaml`.
3. The profile's runtime entry in `settings.yaml`.

If none of them sets a key, the cluster's default StorageClass and `10Gi` are used.

An empty value means "not set", so it does not override a lower source. Once a runtime entry or profile sets a class, a template cannot reset it to the cluster default; name the class explicitly instead.

`shared_dir_size` must be a positive Kubernetes quantity such as `10Gi` or `1Ti`. Settings validation and the admin settings API reject other values, and an agent start fails with an error naming the key that holds the bad value.

The settings values are read every time an agent starts. They apply only on the Kubernetes runtime.

On GKE Autopilot the default class (`standard-rwo`) cannot provision `ReadWriteMany` volumes. The claim stays unbound and the agent pod stays `Pending`. To avoid this, set an RWX class such as `standard-rwx` (Filestore CSI) on the runtime or the profile:

```yaml
runtimes:
  gke-autopilot:
    type: kubernetes
    context: my-autopilot-cluster
    namespace: scion-agents
    shared_dir_storage_class: standard-rwx
    shared_dir_size: 1Ti

profiles:
  gke:
    runtime: gke-autopilot
    # Optional per-profile override; wins over the runtime entry.
    # shared_dir_storage_class: premium-rwx
```

A template or agent can still override this for itself:

```yaml
kubernetes:
  shared_dir_storage_class: standard-rwx
  shared_dir_size: 10Gi
```

:::note
Existing PVCs are reused as they are and never changed. A new class or size only applies to claims created after the change. To move an existing shared directory to a new class, delete its PVC (`scion-shared-…`, labelled `scion.shared-dir=<name>`) after copying out its data. When an agent reuses a claim whose class differs from the requested one, Scion logs a warning naming the claim and both classes.
:::

:::caution[Cost with many projects]
Each dynamically provisioned RWX PVC can be its own backing volume. On GKE, every `standard-rwx` claim is a separate Filestore instance, with that tier's minimum capacity. With many projects or shared directories this adds up quickly. For larger fleets, use the NFS backend instead: one pre-provisioned RWX export, mounted by every pod with a `subPath` per project and directory, and no per-directory PVCs. See [`server.shared_dir_storage`](/scion/reference/server-config/#shared-directory-storage-servershared_dir_storage) (`backend: nfs`) or the NFS [`server.workspace_storage`](/scion/reference/server-config/#workspace-storage-serverworkspace_storage) backend, which serves shared directories from the workspace export.
:::

### Resource Configuration

Standard compute resources use the common `resources` field:

```yaml
resources:
  requests:
    cpu: "500m"
    memory: "1Gi"
  limits:
    cpu: "2"
    memory: "4Gi"
  disk: "20Gi"    # maps to ephemeral-storage (both requests and limits)
```

Extended resources (GPUs, custom devices) use `kubernetes.resources`.

### GKE Workload Identity

When running in Google Kubernetes Engine (GKE), Scion natively supports Workload Identity for secure access to GCP APIs (like Vertex AI or Cloud Storage) without passing long-lived service account keys.

1. Enable the `gke: true` flag in your runtime configuration.
2. Ensure your cluster is configured with Workload Identity.
3. Bind a Kubernetes Service Account to a Google Service Account.
4. Set the `serviceAccountName` in the agent's Kubernetes configuration to match the bound KSA.

This provides the agent container with an ambient identity, which the underlying harness (e.g., Gemini or Claude via Vertex) can automatically resolve using Application Default Credentials (ADC).

:::tip[GOOGLE_CLOUD_PROJECT / GOOGLE_CLOUD_LOCATION]
Vertex AI auth also needs a project key (usually `GOOGLE_CLOUD_PROJECT`) and, for harnesses that require one, a region key — `GOOGLE_CLOUD_LOCATION` for most of those, though some (e.g. Claude, Gemini) also accept `CLOUD_ML_REGION` or `GOOGLE_CLOUD_REGION`. Rather than setting these per project, set them once at hub scope:

```bash
scion hub env set --scope hub --always GOOGLE_CLOUD_PROJECT=<project>
scion hub env set --scope hub --always GOOGLE_CLOUD_LOCATION=<region>
```

or declare them in broker `settings.yaml`, under `harness_configs.<name>.env` (or `profiles.<profile>.harness_overrides.<name>.env`), or in the harness-config directory's own `config.yaml` `env:` block. Any of these sources satisfies the broker's env preflight, so a Kubernetes Hub does not need a per-project step just for these two variables.
:::

:::note[Broker Workload Identity]
Runtime Brokers use the same Workload Identity mechanism for OIDC transport tokens when connecting to an IAP-protected Hub. The broker's GSA needs `roles/iap.httpsResourceAccessor` on the Hub backend service (or `roles/run.invoker` for Cloud Run invoker mode) — this is separate from the agent dispatch transport SA. See [Brokers behind IAP](/scion/hosted/ha/auth-proxy-iap/#brokers-behind-iap) for the full setup.
:::

### Pod Priority and Preemption

By default, agent pods have no `priorityClassName`, which puts them at priority 0 — the first choice when the scheduler needs to evict something to make room for a higher-priority pod (for example a `system-cluster-critical` pod like `kube-dns` being rescheduled during a node scale-down). On GKE Autopilot and Standard this is a real, observed failure mode: an agent pod can be preempted mid-run with no indication beyond a plain stop.

Set a priority class so agent pods are not the default eviction target compared to other ordinary (priority-0) workloads. Scion does not create the `PriorityClass` object itself — create one on the cluster first:

```yaml
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: scion-agent-priority
value: 1000           # above the default (0), below cluster-critical classes
preemptionPolicy: Never # don't let this class preempt other pods to schedule
globalDefault: false
description: "Priority class for Scion agent pods"
```

Then reference it by name, either as a runtime-level default or per template/agent (the per-template value wins if both are set):

```yaml
runtimes:
  k8s:
    type: kubernetes
    priority_class_name: scion-agent-priority
```

```yaml
kubernetes:
  priorityClassName: scion-agent-priority
```

The name must be a valid DNS-1123 subdomain and must already exist on the cluster; an unset value (the default) leaves pods at priority 0, today's behaviour.

A user `PriorityClass` like the one above cannot protect agent pods against `system-cluster-critical` or `system-node-critical` pods (priority values around 2×10⁹) — those can still preempt a lower-priority agent pod regardless of this setting. It only changes the outcome among ordinary workloads, making a ready-to-preempt-anything priority-0 pod no longer the first choice. Avoiding the kube-dns case specifically is a matter of cluster capacity headroom (for example Autopilot's balloon pods, or keeping spare node capacity), not pod priority.

#### Preempted and evicted status

Scion also distinguishes a Kubernetes-initiated disruption from a plain stop or a crash. When the runtime observes a pod that was removed by the scheduler or the kubelet rather than exiting normally, the agent's exit reason reflects it instead of reading as a generic crash:

| Signal observed on the pod | Exit reason |
|---|---|
| Pod status reason `Evicted` (kubelet node-pressure eviction) | `evicted` |
| `DisruptionTarget` condition, reason `PreemptionByScheduler` | `preempted` |
| `DisruptionTarget` condition, reason `TerminationByKubelet` or `EvictionByEvictionAPI` | `evicted` |
| `DisruptionTarget` condition, any other reason (for example a taint-manager or pod-GC removal) | `evicted` |

This is reported as soon as either signal is observed: a pod still `Running` but already committed to termination (it has a `deletionTimestamp` and a live `DisruptionTarget` condition — most of what preemption and the Eviction API delete this way), or a pod that has actually reached a terminal state (`Failed`/`Succeeded`) while still carrying the signal. A `DisruptionTarget` condition with no `deletionTimestamp` yet is not reported — that pod is still finishing its grace period and has not stopped. It depends on the runtime observing one of these two states before the pod object is removed from the API server entirely; if the pod disappears between polls without either ever being observed, the agent may instead be reported through a different, more generic terminal path rather than as preempted/evicted. Docker and other non-Kubernetes runtimes are unaffected.

### Safe-to-Evict

Cluster autoscalers remove underused nodes by evicting the pods on them. Agent pods are bare pods with no controller to recreate them, so an eviction ends the agent's run. To ask the autoscaler to leave agent pods alone, set `safe_to_evict: false`. Scion then adds this annotation to the pod:

```yaml
metadata:
  annotations:
    cluster-autoscaler.kubernetes.io/safe-to-evict: "false"
```

The setting is opt-in, and only `false` has an effect. Leaving it unset, or setting it to `true`, adds no annotation; that is the default behaviour.

You can set it on a runtime, on a profile, or on a template or agent:

```yaml
# settings.yaml — runtime default
runtimes:
  k8s:
    type: kubernetes
    safe_to_evict: false
```

```yaml
# settings.yaml — profile value, overrides the runtime
profiles:
  long-running:
    runtime: k8s
    safe_to_evict: false
```

```yaml
# template or agent scion-agent.yaml — overrides profile and runtime
kubernetes:
  safeToEvict: false
```

Scion uses the first value it finds, in this order:

1. The template or agent `kubernetes.safeToEvict`
2. The agent's profile's `safe_to_evict`: the `--profile` flag, or the profile the agent was created with, falling back to the active profile
3. The `safe_to_evict` on that profile's runtime entry

An explicit `true` at a higher level wins over a `false` lower down. For example, `safeToEvict: true` on a template turns the annotation off for that template, even when the runtime sets `false`.

Other runtimes (Docker, Podman, Apple, Cloud Run) accept the setting and ignore it. `scion config validate` warns when it is set on a non-Kubernetes runtime, or on a profile that uses one, and Scion logs a warning at agent start when it is ignored.

#### GKE Autopilot

On GKE Autopilot, the annotation makes the pod an [extended run time pod](https://cloud.google.com/kubernetes-engine/docs/how-to/extended-duration-pods). GKE then doesn't evict the pod for scale-down or node auto-upgrades for up to seven days. After that, the node can be scaled down or upgraded as usual. Before you enable it, note these points from the GKE documentation:

- **Disruptions it doesn't prevent:** priority-based preemption, system Pod evictions, kubelet out-of-memory eviction, Compute Engine VM maintenance, node auto-repair, and anything an operator starts, such as a manual upgrade or a node drain. The [Pod Priority and Preemption](#pod-priority-and-preemption) settings above still matter.
- **Resources and cost:** extended run time pods have higher minimum resource requests than ordinary Autopilot pods. You're billed for the requests at standard rates, and GKE places each pod on its own node where it can.
- **Workloads it can't be combined with:** Spot Pods, custom compute classes, and inter-Pod affinity.
- **Limits:** at most 50 extended run time workloads with different CPU requests per cluster.
- **Image pull time:** the time spent pulling the image counts toward the run time.

Check the current GKE page before you rely on these values, because they can change.

#### GKE Standard and other clusters

With the open source cluster autoscaler, including GKE Standard, the annotation stops scale-down from evicting the pod for as long as it runs. The node the pod runs on isn't removed for being underused, which can keep idle nodes running. The annotation doesn't protect against node auto-upgrades, preemption, node failure, or a manual drain.

### PodDisruptionBudgets

Scion doesn't create PodDisruptionBudgets. Operators who want voluntary disruptions, such as node drains and GKE surge upgrades, to wait for agent pods can create one themselves.

Agent pods are bare pods with no owning workload. For bare pods, Kubernetes supports only an integer `minAvailable`; it doesn't support `maxUnavailable` or percentages. A budget per agent works well. Each agent pod has the label `scion.name=<agent-name>`:

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: scion-agent-my-agent
  namespace: scion-agents
spec:
  minAvailable: 1
  selector:
    matchLabels:
      scion.name: my-agent
```

Before you use one, consider the following:

- **Drains wait:** while a budget blocks eviction, `kubectl drain` keeps retrying until it times out. Bare pods also need `kubectl drain --force`. When a drain does evict an agent pod, nothing recreates it, so the budget only delays the end of the run.
- **GKE upgrades:** during surge upgrades, GKE respects budgets and the termination grace period for up to one hour. After that, it evicts the remaining pods.
- **Not covered:** a budget only applies to evictions through the Eviction API. It doesn't cover preemption, kubelet node-pressure eviction, node failure, or VM maintenance.
- **Overlapping budgets:** the Eviction API refuses to evict a pod that more than one budget selects. Don't let selectors overlap.
- **Cleanup:** a budget whose agent is gone matches nothing and has no effect. Delete it when you delete the agent.

A budget complements `safe_to_evict: false`. The annotation covers autoscaler scale-down, including Autopilot's extended run time. A budget makes drains and upgrades wait.

### Maintenance Windows and Exclusions

On GKE, [maintenance windows and exclusions](https://cloud.google.com/kubernetes-engine/docs/concepts/maintenance-windows-and-exclusions) control when automatic upgrades run. That makes them the main way to keep upgrades away from long agent runs:

- **Maintenance window:** limits automatic upgrades to times you choose, for example off-hours, when fewer agents are running.
- **"No upgrades" exclusion:** blocks upgrades for up to 90 days, but GKE recommends 30 or fewer. A cluster can have at most three, and they must leave at least 48 hours of maintenance availability in any rolling 92-day period.
- **"No minor upgrades" and "no minor or node upgrades" exclusions:** these can last until the end of support for the cluster's minor version, but GKE recommends keeping them under six months. "No minor or node upgrades" also blocks node upgrades.

Windows and exclusions don't stop Compute Engine maintenance, and most control plane repairs ignore them. GKE can also override them to apply critical security patches. They reduce disruptions, but agent runs still need to survive an occasional node loss.
### GCP Identity Mode "assign" (Workload Identity mapping)

GCP identity mode **block** and **passthrough** work the same way on Kubernetes as on
any other runtime (see [Hub-Default GCP Identity](/scion/hosted/ha/permissions/#hub-default-gcp-identity)
for the mode ladder), except that **block** is not offered on the Kubernetes runtime at
all — see the [permissions guide](/scion/hosted/ha/permissions/#hub-default-gcp-identity)
for what a dispatch resolving to it does instead.

**assign** on Kubernetes is different from every other runtime: it does not use the
sciontool metadata server emulator (no `SCION_METADATA_MODE=assign`, no
`GCE_METADATA_HOST`/`GCE_METADATA_ROOT` override, no in-pod emulator process). Instead,
the pod runs as a Kubernetes ServiceAccount (KSA) already bound to the assigned Google
service account (GSA) via Workload Identity, and GCP client libraries in the container
reach the real GKE metadata server directly — the same mechanism the manual
`serviceAccountName` field above and the "Enable GKE Workload Identity" steps use, just
selected automatically from the GSA the create request, project, or hub resolved.

Scion never creates, annotates, or grants IAM bindings for these KSAs — an operator
must pre-provision each one (the cluster has Workload Identity Federation enabled, the
node pool runs `GKE_METADATA`, the KSA exists, is annotated with
`iam.gke.io/gcp-service-account`, and the GSA grants it `roles/iam.workloadIdentityUser`),
then tell the broker which KSA belongs to which GSA via `kubernetes_service_account_mappings`
in its own global settings (runtime-level, profile-level, or both — a profile-level entry
for a given GSA overrides the runtime-level entry for that same GSA). This mapping is
operator-configured only: it is read from the broker's own global settings, never from a
project's settings.yaml, so a project cannot redirect an assign dispatch to a different
identity than the one the operator mapped. In a database-backed hosted deployment, the
global settings database row is authoritative for this mapping after first boot, not the
`settings.yaml` file — see setup-gcp.md §2i, linked below, for how to update it there. The
pod's namespace also comes only from the broker's settings: the selected runtime entry's
`namespace`, then the runtime's default namespace. Profiles have no namespace setting of
their own; a profile that needs a different namespace selects its own runtime entry that
sets it (with its own mapping, if the KSA differs). The mapped KSA must be provisioned in
that namespace. A request-level `kubernetes.namespace` is accepted only when it equals
that resolved namespace. A dispatch is also refused with 400 when a project's
settings.yaml overrides the selected entry's `namespace` or `context`, or, on a broker
with `ForceRuntime`, when the forced runtime's namespace differs from the entry's. See
[setup-gcp.md §2i](/scion/hosted/ha/setup-gcp/#2i-gke-workload-identity-for-gcp-identity-mode-assign)
for the full `gcloud`/`kubectl` recipe, including the cluster and node-pool prerequisites
and the additional GCP roles the mapped GSA needs when the agent also uses `gke: true`
volumes (CSI Secret Manager, GCS FUSE).

A Kubernetes dispatch in assign mode whose GSA has no entry in
`kubernetes_service_account_mappings` fails before any pod is created, with an error
naming the setting to add — it does not fall back to the emulator (which assign never
uses on Kubernetes) or to the pod's default identity. The mapping is authoritative over
an explicit `kubernetes.serviceAccountName` set directly on the create or start request:
a request-level value that names a *different* KSA than the mapping is rejected, rather
than silently preferring either source, because the pod would otherwise run as an
identity other than the one that was actually assigned. A `serviceAccountName` set only
in a template (not on the request itself) is not this case — it is invisible at the
point the mapping is resolved, and is silently overridden by the mapping, the same way
any other request-level value already overrides a template value.

## Architecture & Security

### Native Client & In-Cluster Authentication
Scion communicates directly with the Kubernetes API using the native Go client, providing high-performance `exec` and `attach` streams without relying on external binaries. If Scion is running inside a Kubernetes cluster (e.g., as a Scion Hub deployment), it automatically detects and uses the in-cluster service account tokens for authentication.

### Pod Security Hardening
To ensure secure execution, Scion enforces the following pod security policies automatically:
- **Non-Root Execution**: Agent pods run as the unprivileged `scion` user (UID `1000`).
- **Environment Injection**: Standard environmental variables like `HOME` (`/home/scion`), `USER` (`scion`), and `LOGNAME` (`scion`) are explicitly injected to prevent sandbox escapes and ensure consistent toolchain behavior.

## Reliability & Auto-Recovery

### Terminal Pod State Reconciliation
Scion's Kubernetes runtime actively monitors pod phases and reconciles terminal states (e.g., `Failed`, `Evicted`, or unexpectedly `Succeeded`). This active reconciliation improves auto-recovery, ensuring that failed agents are properly cleaned up and rescheduled if necessary.

### GKE Autopilot Auto-Detection
When running on GKE Autopilot, Scion automatically detects the environment and applies the correct scheduling tolerations required by Autopilot to seamlessly provision workloads without manual node selector configuration.

### Exec Readiness on New Nodes
On a node that has just scaled up from zero, such as on GKE Autopilot, the API server's exec tunnel to the kubelet can take tens of seconds to come up after the container starts. Before its first exec into a new pod, the runtime probes the tunnel and retries transient failures (for example, `error dialing backend: No agent available`) with exponential backoff for up to about 90 seconds. If the tunnel still is not ready, the start fails with `pod exec tunnel not ready`.

## Support Matrix

### Volume Types

| Volume Type | Status | Notes |
|---|---|---|
| EmptyDir (workspace) | Supported | Default workspace volume, always created. Contents are lost when the Pod stops (see [Empty-per-agent workspaces](#empty-per-agent-workspaces)) |
| GCS FUSE CSI | Supported | Requires `gcsfuse.csi.storage.gke.io` CSI driver; GKE only |
| Local/bind-mount | Not supported | Logged as warning, skipped. Use tar sync instead |
| PersistentVolumeClaim | Supported | Used for the NFS-backed shared `workspace_storage` backend; requires a pre-provisioned PV/PVC (for example, Filestore-backed) and `workspace_storage.backend: nfs` in `settings.yaml`. See [NFS workspace export requirements](#nfs-workspace-export-requirements) |

### NFS Workspace Export Requirements

With `workspace_storage.backend: nfs`, each project's workspace is mounted into the agent Pod from the shared volume at the subPath `<subpath_root>/<project-id>/workspace` (`subpath_root` defaults to `projects`). When `shared_dir_storage` is unset or `local`, the project's shared directories are mounted from the same volume at `<subpath_root>/<project-id>/shared-dirs/<name>`. These directories have to exist before the Pod starts. How they get created depends on whether the broker that creates the Pod has the export mounted at `workspace_storage.nfs.mount_root/<share id>`:

- **Broker has the export mounted (recommended).** Before it creates the Pod, the broker creates the workspace directory and each of those shared directories itself, the same way it creates shared-directory leaves: missing parent directories get mode `2755`, and each new directory gets mode `2775` (setgid) with a default ACL that gives the group write access. Existing directories and their contents are left as they are. For this to help on exports that map root or all users to an anonymous user, the broker's writes must not be mapped to an anonymous user that cannot write in `<subpath_root>/<project-id>`. If the broker is not allowed to create a directory (permission denied or a read-only mount, for example a broker that does not run as root where `<subpath_root>/<project-id>` is owned by root), it logs a warning and leaves that directory to the kubelet, as described in the next item. If the path cannot be used at all (a symlink or a regular file where a directory should be, or an export mount path that is not a directory), agent create fails straight away with an error that names the export requirement, rather than timing out while the Pod waits.
- **Broker does not have the export mounted.** The kubelet creates the directories when the Pod starts. This only works on exports that let root create directories (`no_root_squash`, the Filestore default). On exports that map root to an anonymous user (`root_squash` or `all_squash`), the kubelet's mkdir is denied and the Pod stays in `CreateContainerConfigError` ("failed to create subPath directory for volumeMount workspace"). Mount the export on the broker, or use `no_root_squash`.

`mount_root/<share id>` must be the mounted export itself, not a parent of the mount point or an ordinary directory. The broker treats it as the export once it exists: if the export is not actually mounted there, the directories are created on the broker's local disk, and the Pod still depends on the kubelet creating them on the export.

On exports that map root or all users to an anonymous user, the workspace provisioning init container cannot change file ownership, because the NFS server decides ownership changes. When the broker created all of these directories, or found them already in place with setgid and group write (for example `2775`), the provisioning step tries to set ownership to the agent runtime uid (`1000`) and `workspace_storage.nfs.gid`, logs a warning if that is not allowed, and continues. If any of them already exists without setgid and group write (for example a root-owned `0755` directory left by an earlier kubelet mkdir), or was left to the kubelet, a failed ownership change still stops the Pod; fix that directory's group and mode on the export. Agents get access through the directories' group: set `workspace_storage.nfs.gid` to the group that owns `<subpath_root>/<project-id>` on the export (the agent Pod uses it as its `fsGroup`), so files created under the setgid directories stay writable by the agent.

#### Sharing Modes on the NFS Workspace

How agents of a git-backed project use `<subpath_root>/<project-id>/workspace` depends on the project's sharing mode:

- **Worktree-per-agent.** The workspace directory holds the project's shared checkout, and each agent gets its own git worktree at `<subpath_root>/<project-id>/workspace/worktrees/<agent-name>`, on the agent's branch: the branch given at create, used exactly as given (for example `feature/login`), otherwise the agent name. The provisioning init container clones the shared checkout on the project's first start, detaches its HEAD so that no branch is held by the shared checkout, and adds the agent's worktree with relative paths. It also sets `gc.auto 0` in the shared checkout and adds `worktrees/` to its `.git/info/exclude`. A shared checkout provisioned earlier in another mode keeps its files and current branch. The agent container mounts the shared `.git` at `/repo-root/.git` and its worktree at `/repo-root/worktrees/<agent-name>`, and starts in the worktree, the same layout as worktree-per-agent on Docker. When the shared checkout has already been provisioned, the broker creates the agent's empty worktree directory before the Pod starts, as described above for the workspace directory. Every agent Pod in this mode runs the provisioning init container; a per-project file lock on the export runs them one at a time, and a Pod waits up to the init container's provisioning timeout (5 minutes) for it, as long as a Pod that waits for the first clone to finish.
- **Shared-plain.** Every agent mounts the workspace directory at `/workspace`.
- **Clone-per-agent.** Each agent gets its own directory next to the workspace directory, `<subpath_root>/<project-id>/agents/<agent-name>`, and its own clone of the repository in `agents/<agent-name>/workspace`, described below. The project's workspace directory is not used.

Hub-managed projects without git that use **Empty-per-agent** (each agent gets its own private directory that starts empty) use the same agent directory as clone-per-agent, `<subpath_root>/<project-id>/agents/<agent-name>/workspace`, described below, but nothing is cloned into it. The project's workspace directory is not used. Without NFS workspace storage (including `gke-shared-volume`), see [Empty-per-agent workspaces](#empty-per-agent-workspaces) below.

Worktree-per-agent needs git 2.48 or later in the provisioning init container, in the agent images and on the broker. Worktrees with relative paths, which this mode adds, set a repository extension that older git versions cannot read, so once a project has one, older git can no longer use its shared checkout. With an older git in the init container, the agent's worktree directory is left empty instead, as described below for older images.

The provisioning init container runs as root, while the shared checkout and the worktrees belong to the agents' user (uid 1000, the Pod's user). Its git commands list exactly the shared checkout and the agent's worktree in git's `safe.directory` setting, for those commands only, so git works in them. Afterwards it sets the ownership of what it added or changed in this step (the agent's new worktree, its entry and branch under `.git`, `.git/config` and the agent's entry in the sharing registry) to that user and the group the provisioning step chowns to. A worktree that already exists is left as it is.

Starting an agent in worktree-per-agent mode stops with an error that says what to do when:

- the branch name is not a valid git branch name.
- the agent's branch is checked out in the shared checkout. Switch the shared checkout to another branch or detach it (`git switch --detach`, run in the shared checkout), or start the agent with a different branch.
- the agent's branch is checked out in another agent's worktree. Start the agent with a different branch, or delete the agent that uses that worktree together with its files, on a broker that mounts the export (which removes the worktree). If no agent uses it any more, remove it (`git worktree remove worktrees/<agent-name>`, run in the shared checkout) and start the agent again.
- the agent's kept worktree is on a branch other than the one requested, for example when a new agent is created with the same name as an earlier one whose files were kept, and with another branch. Create the agent with the branch the kept worktree is on, or delete the agent together with its files and create it again with the new branch. This does not apply to a restart of the same agent: an agent that switched branches inside its own worktree is restarted with the branch it was created with, and its worktree is reused as it is.
- git's entry for the agent's existing worktree points to another directory. Move the directory out of the way, or delete the agent with its files, and start the agent again.

These errors name paths relative to the shared checkout, `<subpath_root>/<project-id>/workspace` on the export. If git has no entry for the agent's existing worktree any more (for example after a `git worktree prune` while the directory was missing), the directory is moved aside to `worktrees/.stale-<agent-name>-<suffix>`, with its files, and a new worktree is added. The moved directory's `.git` file is renamed to `.git.moved`, so the directory holds plain files and is no longer a worktree. Remove it once you no longer need its files.

Deleting an agent together with its files removes its worktree through the broker's mount of the export: under the same per-project lock as the provisioning init container, the worktree directory is renamed to `worktrees/.removing-<agent-name>-<suffix>` (its `.git` file renamed to `.git.moved`, as above) and `git worktree prune` drops its entry. Its files are then deleted in the background, without the lock. The broker waits up to 30 seconds for the lock; the steps under it are not cut short, and `git worktree prune` gets up to 2 minutes. A removal that stops partway leaves at most a `.removing-` directory, which does not stop an agent created again with the same name and is deleted by the next removal in that project. The broker must be able to delete files owned by the agents' user (uid 1000) on the export; otherwise each background delete fails with a warning, `.removing-` directories collect in `worktrees/`, and an operator has to remove them. The agent's branch is always kept, whatever the delete's branch option says, so an agent created again with the same name gets a fresh worktree on that branch. If the delete removed nothing, because the agent's files were kept or the export is not mounted on the broker, an agent created again with the same name reuses the existing worktree when the requested branch is the one the worktree is on, or the one it was created with; with another branch it stops with the error above. A failed removal is logged as a warning with the worktree's path and does not fail the delete.

`git worktree prune` runs in the shared checkout during provisioning and removal, and drops git's entries for worktrees whose directory it cannot find on the export, for example a worktree an agent adds under `/tmp`. To add more worktrees from inside an agent, create them inside the agent's own worktree directory with `git worktree add --relative-paths`.

With an agent image whose provisioning init container only clones the shared checkout, the agent's worktree directory stays empty, and the agent container clones the repository into it on start. The agent still gets its own checkout, but as a separate clone rather than a worktree of the shared checkout. A later start with a newer image keeps that checkout as it is, and deleting the agent leaves it in place.

In clone-per-agent mode the agent container mounts `agents/<agent-name>/workspace` at `/workspace`, starts there, and clones the repository into it on its first start, with the agent's own credentials, the same way clone-per-agent works on Docker. The agent name must be an agent slug (lower-case letters, digits and dashes); otherwise starting the agent stops with an error. The broker creates `agents/<agent-name>` and the empty `workspace` directory in it before the Pod starts, both with the modes described above for the workspace directory (`2775` with a default ACL that gives the group write access), because the provisioning init container writes in `agents/<agent-name>` too. Every agent Pod in this mode runs the provisioning init container, which mounts `agents/<agent-name>` and does not clone: under a file lock in that directory it creates `workspace` if it is missing, sets the ownership of the `workspace` directory itself while it is empty, records the agent's branch in `agents/<agent-name>/.scion-agent-branch`, prepares the shared directories, and writes its sentinel in `agents/<agent-name>`. The lock, the branch record and the sentinel stay outside `workspace`, so the agent container finds it empty and clones into it. The agent's branch is the branch given at create, otherwise `scion/<agent-name>`, the same default as on Docker.

A restart reuses the agent's workspace as it is. Starting an agent whose kept workspace holds files and was created for another branch, for example a new agent created with the same name as an earlier one whose files were kept, stops with an error that names both branches: create the agent with the branch the kept workspace was created for, or delete the agent together with its files and create it again. A workspace that is still empty, for example after a failed clone, takes the requested branch. As for the clone step, a workspace that holds only `.scion`, `.scion-volumes` or `.agents` entries, such as the mount points of shared directories, counts as empty. A failed clone removes what it wrote inside `/workspace` and leaves the directory itself, so the next start clones again. As on Docker, two Pods started for the same agent at the same time (for example during a broker failover) are not kept from cloning into the same workspace at once.

Deleting an agent together with its files, on a broker that mounts the export, removes its workspace: under the same file lock as the provisioning init container, `agents/<agent-name>/workspace` is renamed to `agents/<agent-name>/.removing-workspace-<suffix>` and the branch record is removed; the files are then deleted in the background. The broker waits up to 30 seconds for the lock. As for worktrees, the broker must be able to delete files owned by the agents' user, a failed removal is logged as a warning and does not fail the delete, and an agent created again with the same name starts from an empty workspace on any branch. The broker removes both the agent's worktree and its agent directory when they exist, whatever mode the project is in at delete time, so an agent created before the project's mode changed is still removed; nothing outside `worktrees/<agent-name>` and `agents/<agent-name>` is touched.

Older agent images behave in one of two ways. With an image whose `sciontool` predates `SCION_WORKSPACE_MODE`, the provisioning init container prepares `agents/<agent-name>` as a workspace without a repository, and the agent container still clones into the empty `workspace` directory the broker created. When the export is not mounted on the broker, the kubelet creates that directory instead, as described above. An image whose `sciontool` knows worktree-per-agent but not clone-per-agent stops the provisioning init container with an error that clone-per-agent mode must not use the NFS backend, and the agent does not start; use an agent image with this version of `sciontool` for clone-per-agent projects.

In Empty-per-agent mode the agent's directory works the same way as in clone-per-agent mode, without the branch and the clone. The broker creates `agents/<agent-name>` and the empty `workspace` directory in it before the Pod starts, with the same modes, and creates nothing else in `agents/<agent-name>`. The agent container mounts `agents/<agent-name>/workspace` at `/workspace`. No container in the Pod mounts the project's workspace directory. The provisioning init container mounts `agents/<agent-name>`, creates `workspace` if it is missing, sets the ownership of the `workspace` directory while it is empty, prepares the shared directories and writes its sentinel. It records no branch and runs no git. The workspace's files are kept across restarts and suspend/resume, and deleting the agent together with its files removes its workspace as described above for clone-per-agent. Starting such an agent stops with an error, so it never falls back to the project's workspace directory, when:

- the agent name is not an agent slug (lower-case letters, digits and dashes).
- the NFS share has no `pv_name`, so the Pod cannot mount the agent's directory.
- the agent runs on a runtime other than Kubernetes on a broker with `workspace_storage.backend: nfs`. Use a broker with local workspace storage for those runtimes.

An agent image whose `sciontool` predates `SCION_WORKSPACE_MODE` prepares `agents/<agent-name>` as a plain workspace, and the agent still gets the empty `workspace` directory the broker created. An image whose `sciontool` knows Empty-per-agent but not this NFS support stops the provisioning init container with an error; use an agent image with this version of `sciontool`.

### Empty-per-agent Workspaces

With NFS workspace storage, an Empty-per-agent agent's workspace is on the export and is kept when the agent stops; see [Sharing Modes on the NFS Workspace](#sharing-modes-on-the-nfs-workspace).

On clusters without NFS workspace storage (including `gke-shared-volume`), an agent in an Empty-per-agent project (a Hub-managed project without git created with workspace mode `per-agent`; see [Workspaces & Sharing Modes](/scion/local/workspaces-and-sharing/)) gets its own EmptyDir workspace volume. It starts empty, as intended, but **its contents are lost when the agent stops or its Pod is replaced**, so suspend/resume does not keep them either. Git clone-per-agent workspaces on EmptyDir behave the same way. Have agents write anything that must survive to a [shared directory](#shared-directory-pvcs).

### Persistent Agent Home (NFS)

With [`server.home_storage`](/scion/reference/server-config/#agent-home-storage-serverhome_storage) (or a profile's `home_storage_backend: nfs`) and the `hub.k8s_nfs_home` experiment, a Kubernetes agent keeps its home directory on the NFS export of its profile's shared-dir storage. Files, harness sessions and credentials the harness writes into the home then survive stops, restarts and pod replacements. The feature is in development and also needs `server.home_storage.allow_incomplete_phases`.

- **Layout**: each agent's home is `<subpath_root>/<project id>/agents/<agent slug>/home-<agent id>` on the export, mounted by `subPath` at the agent's home directory. It is named after the hub agent ID, so a new agent with the same name gets a new, empty home.
- **Export requirements**: the export's group must be the pod group, gid 1000. The home directory is mode 2771 and owned by that group, and the agent writes through the group. A start whose home is not writable by uid 1000 fails with `home_storage_unavailable` and names the requirement.
- **Creating the home directory** (`home_storage_leaf`):
  - `pod` (default): a `home-leaf` init container in the agent's pod creates it. The container runs as root with only the `CHOWN`, `FOWNER` and `DAC_OVERRIDE` capabilities and mounts only the agent's directory. This needs an export without root squashing; on a squashed export the start fails with `home_leaf_failed`. The broker does nothing on its host.
  - `broker`: the broker creates it through its own mount of the export at the shared-dir storage `mount_root` (for example an fstab entry with `nofail`). This works on root-squashed and all-squashed exports. The mount is checked only when an agent using it starts; a missing mount fails that start with `home_storage_unavailable` and never affects broker startup or agents on other runtimes.
- **Each start**: a `home-prepare` init container (uid 1000) checks the home and its sentinel file `.scion-home-seed.json`. A new home is seeded once: the image's own home directory is copied if it is smaller than `skeleton_max_bytes`, then the broker's copy of the agent home is transferred. Later starts transfer the broker's copy over the existing home and clear `.scion/hooks` first. A home with content but no sentinel, or one belonging to another agent, fails the start without changes.
- **Secrets**: staged secret and auth files stay in memory or read-only volumes under `/run/scion`. In the home they appear as symbolic links, so their content is never written to the export. A file, link or directory the user put at a link target that scion never linked, or a symbolic link in its parent path, is kept and the link is skipped with a warning. If `.scion` or `.scion/hooks` in the home is a symbolic link, the start fails rather than transferring through it. `SCION_SECRETS_FILE` points at the staged `secrets.json`. The harness's own staged secrets are copied into memory only (`SCION_HARNESS_SECRETS_DIR`), never into the home, and its generated outputs are written to memory too.
- **Images**: the image's sciontool must support NFS homes (`sciontool version --features` lists `home-v1`); older images fail with a message saying so.
- **One pod at a time**: pods with an NFS home are never force-deleted. Stop and delete use the pod's grace period (`stop_grace_seconds`, default 30) and return at once. A start waits for the previous pod to stop, for at most the grace period plus `termination_wait_seconds` (default 15). It is refused with the retryable `previous_pod_unconfirmed` when the previous pod cannot be read, its node is unreachable, or the bound runs out. The previous pod's secrets are removed only once it has stopped. The pod name is unique per agent and a start waits for the previous pod's confirmed termination, so two pods do not write to the same home; a per-agent advisory lock is also taken when the broker is given a store locker. Keep `stop_grace_seconds` plus `termination_wait_seconds` below the hub's 90 second start window; the server warns at startup when it is not.
- **Sync**: `scion sync` copies the workspace only; the home stays on the export.
- **Delete**: deleting an agent does not remove its home from the export in this version.
- **Credentials at rest**: the agent's hub token and environment files (`.scion/scion-token`, `.scion/scion-env`), credentials that harness scripts copy into native home files (for example API-key auth files), and harness OAuth tokens are stored on the export while the home exists.

### Secret Modes

| Mode | Status | Prerequisites |
|---|---|---|
| Native K8s Secret | Supported (default) | Secret create/delete RBAC |
| GKE Secret Store CSI | Supported | `gke: true`, Secrets Store CSI Driver + GCP provider, SecretProviderClass CRD |
| File-based secret decoding | Supported | Injected via K8s Secret volumes for file-based decoding |
| ResolvedAuth files | Supported | Injected via K8s Secret volumes (not hostPath) |

Secrets are composable: `ResolvedAuth` and `ResolvedSecrets` are applied independently (not mutually exclusive).

File-type secrets, harness auth files, and `secrets.json` whose targets are inside the agent home are not mounted there directly. Their volumes are mounted under `/run/scion/` (`secrets-store`, `agent-secrets`, `auth-files`), and after the home sync each file is copied to its target in the home as the Pod user, with mode `0600`. This keeps the home writable for non-root Pods: a direct `subPath` mount would make the container runtime create missing parent directories owned by root, and the home sync would then fail with "Permission denied". Targets outside the agent home keep their direct `subPath` mounts.

### Hub Transport Credential

When the Hub uses transport auth (see [Auth Proxy (IAP)](/scion/hosted/ha/auth-proxy-iap/)), it sends the initial transport credential as `SCION_TRANSPORT_TOKEN` with each start, resume, and restart. On Kubernetes, the runtime does not write this value into the Pod spec as a plain environment value. Instead it:

- stores it in the agent's per-agent Secret (`scion-agent-<agent>`) under the key `scion-transport-credential`, and
- sets `SCION_TRANSPORT_TOKEN` in the container with `valueFrom.secretKeyRef` pointing at that key.

The per-agent Secret is created even when the agent has no other secrets. It is rebuilt on every start, resume, and restart, so the new Pod always reads the value the Hub sent for that dispatch. In GKE mode the value is stored in this Kubernetes Secret, not in the SecretProviderClass. If a user or project secret also targets `SCION_TRANSPORT_TOKEN`, the value from the Hub is used and the other secret is skipped, with a warning in the broker log. A secret named `scion-transport-credential` is also skipped, because that key is reserved. `SCION_TRANSPORT_TOKEN_EXPIRY` and `SCION_TRANSPORT_AUDIENCE` stay plain environment values. Docker and the other runtimes are unchanged.

No extra RBAC is needed: this uses the same `secrets` create, list, and delete permissions the runtime already needs for agent Secrets (see [Required Permissions](#required-permissions)).

### Sync Modes

| Mode | Status | Notes |
|---|---|---|
| Tar snapshot | Supported | Default. Full workspace snapshot via `pods/exec` streaming |
| GCS volume sync | Supported | For GCS-mounted volumes via `gcloud storage rsync` |

Tar sync includes retry with exponential backoff (1s, 2s, 4s — up to 3 retries) for transient errors (connection resets, broken pipes, timeouts).

### Pod Spec Features

| Feature | Status |
|---|---|
| Resource requests/limits | Supported |
| Extended resources (GPUs) | Supported |
| Ephemeral storage (disk) | Supported (requests + limits) |
| RuntimeClassName | Supported |
| ServiceAccountName | Supported |
| PriorityClassName | Supported (runtime default and per-template/agent override; the class must already exist on the cluster) |
| Safe-to-evict annotation | Supported (opt-in `safe_to_evict: false` on a runtime or profile, or `safeToEvict: false` on a template/agent) |
| NodeSelector | Supported |
| Tolerations | Supported |
| ImagePullPolicy | Supported (Always, IfNotPresent, Never) |
| FSGroup security context | Supported (auto-set from host GID) |

### Namespace Management

| Feature | Status |
|---|---|
| Default namespace | Supported |
| Per-agent namespace | Supported (via config or labels) |
| Multi-namespace listing | Supported (`list_all_namespaces: true`) |
| Namespace/pod ID format | Supported (`namespace/podname` for all operations) |
| Namespace annotation | Supported (`scion.namespace` persisted on pod) |

## Required Permissions

The user or service account running scion needs the following RBAC permissions in the target namespace:

When Scion runs on GCE or GKE and the kubeconfig's exec credential plugin fails (for example, `gke-gcloud-auth-plugin` is not on the process `PATH`), it falls back to Application Default Credentials and requests the `cloud-platform` and `userinfo.email` scopes. The `userinfo.email` scope makes GKE see the caller as its service account email rather than its numeric ID, so RBAC bindings whose subject is the email match. On a plain GCE VM, the instance's access scopes must already include `userinfo.email` for this to work.

### Minimum RBAC

| Resource | Verbs |
|---|---|
| pods | create, get, list, delete |
| pods/exec | create |
| pods/log | get |
| secrets | create, list, delete |

### Additional for GKE Mode

| Resource | Verbs |
|---|---|
| secretproviderclasses (secrets-store.csi.x-k8s.io) | create, list, delete |

### Additional for Multi-Namespace

| Resource | Verbs |
|---|---|
| namespaces | get, list |
| pods (cluster-wide) | list |

### Example ClusterRole

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: scion-agent-manager
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["create", "get", "list", "delete"]
- apiGroups: [""]
  resources: ["pods/exec", "pods/log"]
  verbs: ["create", "get"]
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["create", "list", "delete"]
- apiGroups: [""]
  resources: ["namespaces"]
  verbs: ["get", "list"]
```

## Execution Flow

1. **Start**: `scion start` creates a Pod with the configured image, resources, and secrets. Pods don't keep a workspace across a stop, so when the Hub starts an existing agent it sends the same git clone config, branch, and workspace mode that it sends on create. This lets the agent's workspace be recreated on the new Pod.
2. **Sync**: Workspace and agent home are transferred to the Pod via tar streaming over `pods/exec`.
3. **Ready**: Pod readiness is polled with detailed error classification (image pull, scheduling, config errors).
4. **Attach**: `scion attach` connects to the tmux session inside the Pod via `pods/exec`.
5. **Sync back**: `scion sync from <agent>` retrieves workspace changes via tar streaming.
6. **Delete**: `scion rm <agent>` deletes the Pod and associated Secrets/SecretProviderClasses. `scion stop` uses the same deletion path, so the per-agent Secret and, in GKE mode, the SecretProviderClass are deleted when the agent is stopped or deleted. If the Pod is removed outside scion, the objects are removed on the next stop/delete or start of that agent.
7. **Incomplete start**: if a start ends before the Pod is running because it was cancelled (for example, the agent was deleted while its Pod was still `Pending`) or its request timed out, the runtime removes the Pod, the per-agent Secrets and, in GKE mode, the SecretProviderClass created by that start. This includes a Pod that the API server created as the start was cancelled. Each start labels its objects with `scion.start_id`, and cleanup only removes objects that carry that start's value. A newer agent created with the same name keeps its objects. If a start fails for another reason after its Pod exists, the Pod is kept so its status and logs can be inspected, and it is removed by the next stop or delete.

Deleting an agent that is still in the `created` phase also sends the delete to its broker when the broker is reachable. This covers an agent that was provisioned on its broker but not started (a provision-only create), whose worktree is removed, along with its branch unless the delete keeps it, and an agent whose start is still in flight because the creating request timed out. A broker error does not block removing the agent's Hub record.

**What remains after a delete**: shared-directory PersistentVolumeClaims (`scion-shared-<project>-<dir>`) are project-scoped. They are kept when an agent is deleted, so other agents in the project can keep using them. The NFS workspace volume is also left in place. The Pod, the `scion-agent-<name>` and `scion-auth-<name>` Secrets, and the SecretProviderClass are removed.

## Diagnostics

Run `scion doctor` to verify your Kubernetes runtime configuration:

```bash
scion doctor
```

This checks:
- Cluster connectivity and authentication
- Namespace existence and access
- Pod CRUD and exec permissions
- Secret management permissions
- (GKE mode) SecretProviderClass CRD availability
- (GKE mode) Secrets Store CSI driver installation
- (GKE mode) GCS FUSE CSI driver installation

Use `scion doctor --format json` for machine-readable output.

To find out where agent start time goes, check the structured logs from the Kubernetes runtime, the agent manager, the Hub dispatcher, and `sciontool init`. Start-phase records carry millisecond timings (fields ending in `_ms`, such as `wait_ready_ms`, `scheduled_ms`, `sync_ms`, and `chown_ms`) and byte counts where data is copied.

## Error Handling

The Kubernetes runtime provides structured error messages with remediation hints:

| Error | Remediation |
|---|---|
| ImagePullBackOff / ErrImagePull | Verify image name and registry access; check `imagePullPolicy` |
| InvalidImageName | Check image name format |
| CreateContainerConfigError | Check secret references and volume mounts. For "failed to create subPath directory" on the NFS workspace, see [NFS workspace export requirements](#nfs-workspace-export-requirements) |
| CrashLoopBackOff | Check container logs with `scion logs` |
| Unschedulable | Check node selectors, tolerations, and resource availability |
| Invalid resource values | Error includes the field name and invalid value |

## Limitations

- Workspace sync uses tar snapshots (not live filesystem). Changes require explicit `scion sync`.
- Local/bind-mount volumes are not supported on remote clusters.
- Pod networking depends on cluster CNI configuration.
- Authentication credentials must be propagated via Secrets or Workload Identity.
