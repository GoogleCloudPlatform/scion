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

profiles:
  default:
    runtime: k8s
```

### Brokers with More Than One Runtime

A Runtime Broker can serve several runtimes through its profiles, for example a Docker default plus one or more Kubernetes profiles. Besides its default runtime, the broker tracks each distinct auxiliary runtime separately: Kubernetes runtimes are told apart by context and namespace, so two profiles that target different clusters or namespaces are never merged. Every per-agent path follows the runtime the agent was dispatched to, not the broker default: the Hub endpoint rewrite, extra hosts and the worktree check at dispatch, the runtime reported in the create response (and recorded by the Hub), and start and restart, which look the agent up on the default runtime and then on each auxiliary runtime. For example, an agent on a Kubernetes profile of a Docker-default broker gets the Kubernetes defaults, such as its GCP identity mode, rather than the Docker ones.

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

Hub-managed projects without git that use **Empty-per-agent** (each agent gets its own private directory that starts empty) are not yet supported on the NFS workspace backend: a Runtime Broker with `workspace_storage.backend: nfs` refuses to start such an agent with an error. Without NFS workspace storage (including `gke-shared-volume`), see [Empty-per-agent workspaces](#empty-per-agent-workspaces) below.

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

### Empty-per-agent Workspaces

On clusters without NFS workspace storage (including `gke-shared-volume`), an agent in an Empty-per-agent project (a Hub-managed project without git created with workspace mode `per-agent`; see [Workspaces & Sharing Modes](/scion/local/workspaces-and-sharing/)) gets its own EmptyDir workspace volume. It starts empty, as intended, but **its contents are lost when the agent stops or its Pod is replaced**, so suspend/resume does not keep them either. Git clone-per-agent workspaces on EmptyDir behave the same way. Have agents write anything that must survive to a [shared directory](#shared-directory-pvcs).

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
