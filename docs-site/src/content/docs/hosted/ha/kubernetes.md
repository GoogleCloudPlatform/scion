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

profiles:
  default:
    runtime: k8s
```

### Agent-Level Kubernetes Configuration

Per-agent or per-template Kubernetes settings in `~/.scion/settings.yaml`:

```yaml
kubernetes:
  namespace: custom-namespace          # override runtime namespace
  context: alternate-context           # override runtime context
  serviceAccountName: agent-sa         # Workload Identity / IRSA
  runtimeClassName: gvisor             # sandboxed runtime (gVisor, Kata, etc.)
  priorityClassName: scion-agent-priority  # overrides the runtime-level default, if any
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
| EmptyDir (workspace) | Supported | Default workspace volume, always created |
| GCS FUSE CSI | Supported | Requires `gcsfuse.csi.storage.gke.io` CSI driver; GKE only |
| Local/bind-mount | Not supported | Logged as warning, skipped. Use tar sync instead |
| PersistentVolumeClaim | Supported | Used for the NFS-backed shared `workspace_storage` backend; requires a pre-provisioned PV/PVC (for example, Filestore-backed) and `workspace_storage.backend: nfs` in `settings.yaml`. See [NFS workspace export requirements](#nfs-workspace-export-requirements) |

### NFS Workspace Export Requirements

With `workspace_storage.backend: nfs`, each project's workspace is mounted into the agent Pod from the shared volume at the subPath `<subpath_root>/<project-id>/workspace` (`subpath_root` defaults to `projects`). When `shared_dir_storage` is unset or `local`, the project's shared directories are mounted from the same volume at `<subpath_root>/<project-id>/shared-dirs/<name>`. These directories have to exist before the Pod starts. How they get created depends on whether the broker that creates the Pod has the export mounted at `workspace_storage.nfs.mount_root/<share id>`:

- **Broker has the export mounted (recommended).** Before it creates the Pod, the broker creates the workspace directory and each of those shared directories itself, the same way it creates shared-directory leaves: missing parent directories get mode `2755`, and each new directory gets mode `2775` (setgid) with a default ACL that gives the group write access. Existing directories and their contents are left as they are. For this to help on exports that map root or all users to an anonymous user, the broker's writes must not be mapped to an anonymous user that cannot write in `<subpath_root>/<project-id>`. If the broker is not allowed to create a directory (permission denied or a read-only mount, for example a broker that does not run as root where `<subpath_root>/<project-id>` is owned by root), it logs a warning and leaves that directory to the kubelet, as described in the next item. If the path cannot be used at all (a symlink or a regular file where a directory should be, or an export mount path that is not a directory), agent create fails straight away with an error that names the export requirement, rather than timing out while the Pod waits.
- **Broker does not have the export mounted.** The kubelet creates the directories when the Pod starts. This only works on exports that let root create directories (`no_root_squash`, the Filestore default). On exports that map root to an anonymous user (`root_squash` or `all_squash`), the kubelet's mkdir is denied and the Pod stays in `CreateContainerConfigError` ("failed to create subPath directory for volumeMount workspace"). Mount the export on the broker, or use `no_root_squash`.

`mount_root/<share id>` must be the mounted export itself, not a parent of the mount point or an ordinary directory. The broker treats it as the export once it exists: if the export is not actually mounted there, the directories are created on the broker's local disk, and the Pod still depends on the kubelet creating them on the export.

On exports that map root or all users to an anonymous user, the workspace provisioning init container cannot change file ownership, because the NFS server decides ownership changes. When the broker created all of these directories, or found them already in place with setgid and group write (for example `2775`), the provisioning step tries to set ownership to `workspace_storage.nfs.uid`/`gid`, logs a warning if that is not allowed, and continues. If any of them already exists without setgid and group write (for example a root-owned `0755` directory left by an earlier kubelet mkdir), or was left to the kubelet, a failed ownership change still stops the Pod; fix that directory's group and mode on the export. Agents get access through the directories' group: set `workspace_storage.nfs.gid` to the group that owns `<subpath_root>/<project-id>` on the export (the agent Pod uses it as its `fsGroup`), so files created under the setgid directories stay writable by the agent.

### Secret Modes

| Mode | Status | Prerequisites |
|---|---|---|
| Native K8s Secret | Supported (default) | Secret create/delete RBAC |
| GKE Secret Store CSI | Supported | `gke: true`, Secrets Store CSI Driver + GCP provider, SecretProviderClass CRD |
| File-based secret decoding | Supported | Injected via K8s Secret volumes for file-based decoding |
| ResolvedAuth files | Supported | Injected via K8s Secret volumes (not hostPath) |

Secrets are composable: `ResolvedAuth` and `ResolvedSecrets` are applied independently (not mutually exclusive).

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

Deleting an agent that is still in the `created` phase, before its broker has reported back, also sends the delete to the broker when the broker is reachable. A broker error does not block removing the agent's Hub record.

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
