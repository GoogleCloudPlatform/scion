---
title: Orchestrator Settings (settings.yaml)
description: Configuration reference for the Scion CLI and orchestrator.
---

This document describes the configuration for the Scion orchestrator, managed through `settings.yaml` files. These settings control the behavior of the CLI, local agent execution, and connections to the Scion Hub.

## File Locations

Scion loads settings from the following locations, merging them in order (later sources override earlier ones):

1.  **Global Settings**: `~/.scion/settings.yaml` (User-wide defaults)
2.  **Project Settings**: `.scion/settings.yaml` (Project-specific overrides)
3.  **Environment Variables**: `SCION_*` overrides.

## Versioned Format

Settings files use a versioned format identified by the `schema_version` field. The current version is `1`.

```yaml
schema_version: "1"
active_profile: local
default_template: gemini
```

:::note[Legacy Format]
Files without `schema_version` are treated as legacy format. Run `scion config migrate` to automatically convert legacy files to the versioned format.
:::

## Top-Level Fields

| Field | Type | Description |
| :--- | :--- | :--- |
| `schema_version` | string | **Required**. Must be `"1"`. |
| `active_profile` | string | The name of the profile to use by default (e.g., `local`, `remote`). A new project's `.scion/settings.yaml` does not set it, so the project uses the global value; set it in the project file to pin a different profile for that project. On a Runtime Broker co-located with a database-backed Hub that stores at least one profile, profiles come from the Hub's stored profiles; with none stored, the settings file's profiles apply. The default profile is then `active_profile` if it is a stored profile. Otherwise, if the Hub stores exactly one profile, that profile is used (the Runtime Broker logs which one and why). Built-in profiles that agents or projects still reference, and the built-in runtimes they need, keep resolving beside the stored profiles (a stored profile of the same name wins). With several stored profiles, a built-in profile that `active_profile` names is kept as well and used as the default. Otherwise a dispatch that needs the default profile is refused with an error that names the available profiles, listing kept built-ins after `retained built-in:`. The Hub's own reads of the default profile follow the same rule. The runtime selector (`GET`/`PUT /api/v1/system/runtime`, used during onboarding) does not: it always reads and writes the profile that `active_profile` names. |
| `default_template` | string | The default template to use when creating agents (e.g., `gemini`, `antigravity`). |
| `default_harness_config` | string | Default harness config to use. |
| `default_harness_auth` | string | Default authentication type for new agents (e.g., `api-key`, `vertex-ai`, `none`). |
| `image_registry` | string | Registry prefix for all standard harness images. Rewrites the registry portion of `scion-*` images (e.g., `ghcr.io/myorg`). See [Building Custom Images](/scion/local/custom-images/). |
| `default_max_turns` | int | Default maximum number of turns an agent can take before termination. |
| `default_max_model_calls` | int | Default maximum number of LLM model calls an agent can make. |
| `default_max_duration` | string | Default maximum execution time (e.g., `"2h"`, `"45m"`) for an agent. |
| `default_resources` | object | Default resource requests and limits (`requests`, `limits`, `disk`). See [Resource Specification](#resource-specification-resources) below. |
| `default_gcp_identity_mode` | string | Hub server only. Hub-wide fallback GCP metadata mode for new agents: `block`, `passthrough`, or `assign`. Applied when neither the create request nor the project's default GCP identity names one. See [Hub-Default GCP Identity](/scion/hosted/ha/permissions/#hub-default-gcp-identity). |
| `default_gcp_identity_service_account_id` | string | Hub server only. ID of the verified, hub-scoped service account assigned when `default_gcp_identity_mode` is `assign`. |

## CLI Configuration (`cli`)

General behavior settings for the command-line interface.

```yaml
cli:
  autohelp: true
  interactive_disabled: false
```

| Field | Type | Description |
| :--- | :--- | :--- |
| `autohelp` | bool | Whether to print the usage block after an argument or flag error. Default: `true`. |
| `interactive_disabled` | bool | If `true`, disables all interactive prompts (useful for scripts). |

## Hub Client Configuration (`hub`)

Settings for connecting the CLI to a Scion Hub.

```yaml
hub:
  endpoint: "https://hub.example.com"
  project_id: "uuid-or-slug"
```

| Field | Type | Description |
| :--- | :--- | :--- |
| `endpoint` | string | **Deprecated and ignored by the CLI**, which takes its Hub only from the hub list ([Working with Several Hubs](/scion/hosted/user/multiple-hubs/)); accepted so existing settings files still load, and nothing in the CLI writes it. The value in the global settings is imported once into the hub list as a named hub (`local` for the workstation hub), and becomes the current hub when none is current; for a linked project, its own value (or the inherited global one) is imported once as that directory's link entry. To add a hub, use `scion hub add <name> <url>`. When an agent is provisioned, a `hub.endpoint` in the agent's `scion-agent.yaml` still sets the Hub endpoint that agent is given; when neither it nor the Runtime Broker supplies one, the project's settings `hub.endpoint` is used if those settings still mark the Hub as enabled (the deprecated `hub.enabled: true`, or an endpoint with a token or API key). Servers use `server.hub.public_url` and `server.broker.hub_endpoint` instead, and fall back to this value only as a deprecated legacy source (see [Server Configuration](/scion/reference/server-config/)). |
| `project_id` | string | The unique identifier for this project on the Hub. |
| `linked` | bool | Written by the CLI: `true` once the project is linked, `false` after `scion hub unlink` (agent commands then refuse until `scion hub link`). Unset means never linked; the project links on its first command. |
| `enabled`, `local_only` | bool | **Deprecated and ignored.** Accepted so existing settings files still validate. `scion hub status` notes it when they are set; see the [migration page](/scion/reference/local-mode-removal/). |

:::caution[Moved Fields]
Legacy fields like `token`, `apiKey`, and broker identity fields (`brokerId`) have been removed. 
- **Dev Auth** is now handled via `server.auth.dev_token` (or `SCION_DEV_TOKEN`).
- **Broker Identity** is now configured in the `server.broker` section (see [Server Configuration](/scion/reference/server-config/)).
:::

## Runtimes (`runtimes`)

Defines the execution backends available to Scion.

```yaml
runtimes:
  docker:
    type: docker
    host: "unix:///var/run/docker.sock"

  podman:
    type: podman
    host: "unix:///run/user/1000/podman/podman.sock"
  
  remote-k8s:
    type: kubernetes
    context: "my-cluster"
    namespace: "scion-agents"
```

| Field | Type | Description |
| :--- | :--- | :--- |
| `type` | string | The runtime type: `docker`, `podman`, `container` (Apple), `kubernetes`, or `cloudrun` (Cloud Run Instances). |
| `host` | string | (Docker/Podman) The daemon socket or TCP address. Optional for Podman (defaults to CLI). |
| `context` | string | (Kubernetes) The kubectl context name. |
| `namespace` | string | (Kubernetes) The target namespace. |
| `sync` | string | File sync strategy (e.g., `tar`). |
| `gke` | bool | (Kubernetes) Enable GKE-specific features (e.g., Workload Identity, Autopilot scheduling). Default: `false`. |
| `priority_class_name` | string | (Kubernetes) Default `priorityClassName` applied to agent pods using this runtime entry. Must name a `PriorityClass` that already exists on the cluster — Scion does not create one. An agent's inline config `kubernetes.priorityClassName` overrides this (a template's `kubernetes` block is ignored). Unset means no priority class (pods schedule at priority 0). |
| `list_all_namespaces` | bool | (Kubernetes) List agents across all namespaces. Default: `false`. |
| `shared_dir_storage_class` | string | (Kubernetes) Default StorageClass for shared-dir PVCs. Must support `ReadWriteMany`. A profile's value wins over this, and an agent's inline config `kubernetes.shared_dir_storage_class` wins over both (a template's value is ignored). No default: on the local backend, a start that needs a new shared-dir claim fails until a class is set (see [Kubernetes: starts that need a new claim](/scion/hosted/ha/kubernetes/#starts-that-need-a-new-claim)). |
| `shared_dir_size` | string | (Kubernetes) Default size for each shared-dir PVC, as a positive Kubernetes quantity (e.g. `10Gi`, `1Ti`). Same precedence as `shared_dir_storage_class`. Default: `10Gi`. |
| `safe_to_evict` | bool | (Kubernetes) Set to `false` to add the `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"` annotation to agent pods. Only `false` has an effect; unset or `true` adds nothing. A profile's value wins over this, and an agent's inline config `kubernetes.safeToEvict` wins over both (a template's value is ignored). Ignored, with a validation warning, on other runtime types. |
| `kubernetes_service_account_mappings` | map | (Kubernetes) Map of lowercase GCP service account email to Kubernetes ServiceAccount name, used by GCP identity mode `assign`: the agent pod runs as the mapped ServiceAccount through GKE Workload Identity. The ServiceAccount must already exist in this entry's namespace and be bound to the service account; Scion does not create or bind it. A profile's entry for the same email wins over this. Without an entry, the broker uses the one ServiceAccount in the namespace annotated `iam.gke.io/gcp-service-account` with the email, if there is exactly one; that discovery needs `list` on `serviceaccounts` in the namespace. Read from global settings only; a project's `settings.yaml` value is ignored. See [GCP identity mode "assign"](/scion/hosted/ha/kubernetes/#gcp-identity-mode-assign-workload-identity-mapping). |
| `kubernetes_block_service_account` | string | (Kubernetes) Kubernetes ServiceAccount the agent pod runs as when its GCP identity mode is `block`. Use a dedicated ServiceAccount with no Workload Identity annotation and no IAM grants; Scion does not create or check it. Unset or empty: the pod runs as the namespace's default ServiceAccount. A profile's value wins over this. Read from global settings only. See [block](/scion/hosted/ha/kubernetes/#block). |
| `env` | map | Environment variables to set for the runtime. |

Storage placement is not a per-runtime or per-profile setting. The former `shared_dir_storage_backend`, `shared_dir_storage_backends`, `home_storage_backend` and `home_storage_leaf` keys are removed: settings validation, the Hub admin settings API (`PUT /api/v1/admin/server-config`) and flat Runtime Broker instance loading reject them with an error that names [`server.project_storage`](/scion/reference/server-config/#project-storage-serverproject_storage), which places every agent's workspace, shared directories and home. If an existing settings file or stored runtime/profile still carries one of these keys, startup reports `storage_overrides` as degraded (`storage_override_errors` on `GET /api/v1/admin/server-config`) and starting an agent on that profile or runtime (or an agent whose legacy `shared-dir-storage.json` pins `nfs` while the project storage layout is not shared) is refused before anything is created with `422 storage_migration_required`. For a node-local directory, such as a build cache, use an ordinary `volumes:` entry instead.

The `cloudrun` runtime needs an NFS (Filestore) workspace share: set [`server.project_storage.backend`](/scion/reference/server-config/#project-storage-serverproject_storage) to `nfs` with the share in `nfs.shares[0]`. Agent start on `cloudrun` with any other workspace backend is refused before an instance is created.

:::note
The `tmux` field is now deprecated. All agent sessions are wrapped in tmux by default.
:::

## Harness Configs (`harness_configs`)

Named configurations for agent harnesses. This replaces the legacy `harnesses` map.

```yaml
harness_configs:
  gemini:
    harness: gemini-cli
    image: "us-central1-docker.pkg.dev/.../scion-gemini-cli:latest"
    user: scion
    model: "gemini-1.5-pro"
  
  claude-beta:
    harness: claude
    image: "custom-claude:beta"
    env:
      ANTHROPIC_BETA: "true"
```

| Field | Type | Description |
| :--- | :--- | :--- |
| `harness` | string | **Required**. The harness type (e.g., `gemini-cli`, `claude`, `opencode`). |
| `image` | string | Container image to use. |
| `user` | string | Unix username inside the container. |
| `model` | string | Default model identifier. |
| `task_flag` | string | CLI flag name for passing the task (e.g., `--input`). When set, the task is delivered as a flag value instead of a positional argument. |
| `args` | list | Additional CLI arguments for the harness. |
| `env` | map | Environment variables injected into the container. |
| `volumes` | list | Volume mounts. |
| `auth_selected_type` | string | Authentication method selection (harness-specific). |
| `secrets` | list | Required secrets for this harness configuration (see below). |

### Resource Specification (`resources`)

Defines the compute and disk constraints for an agent's container. The same shape is used by `default_resources`, `profiles.<name>.resources`, `profiles.<name>.harness_overrides.<harness-config>.resources`, and an agent's inline-config `resources` field (a template's is ignored). Harness configs themselves (`harness_configs.<name>`) have no `resources` field.

```yaml
resources:
  requests:
    cpu: "500m"
    memory: "1Gi"
  limits:
    cpu: "2"
    memory: "8Gi"
  disk: "40Gi"
```

| Field | Type | Description |
| :--- | :--- | :--- |
| `requests.cpu` | string | CPU request (for example `"500m"` or `"1"`). Kubernetes only. |
| `requests.memory` | string | Memory request (for example `"1Gi"`). Docker/Podman: `--memory-reservation`. Kubernetes: memory request. |
| `limits.cpu` | string | CPU limit. Docker/Podman: `--cpus`. Kubernetes: CPU limit. |
| `limits.memory` | string | Memory limit. Docker/Podman: `--memory`. Kubernetes: memory limit. Exceeding it OOM-kills a process in the container. |
| `disk` | string | Kubernetes only. Sets the pod's `ephemeral-storage` request **and** limit to this value. Ignored by Docker/Podman. |

Tiers merge field by field: a field set at a higher tier wins, and fields it leaves empty come from lower tiers. See [Settings Precedence](/scion/reference/settings-precedence/).

**Defaults.**

- All runtimes: `limits.cpu: "2"` is applied when no tier sets a CPU limit, unless `runtime.enforce_resource_defaults` is `false` (default `true`). There is no default memory limit.
- Kubernetes only: `cpu`, `memory` and `ephemeral-storage` each get a default **request** (`250m`, `512Mi` and `10Gi`) only when neither a request nor a limit is set for that resource, in `resources` or in `kubernetes.resources`. When a limit is set without a request, the request is left unset and Kubernetes sets it equal to the limit. So with the built-in CPU limit a pod requests and is limited to 2 CPU, and a profile that sets only `limits.memory` is scheduled at that limit.
- Kubernetes never adds a default memory or ephemeral-storage limit. Those limits apply only when you set `limits.memory` or `disk` (or the same key in `kubernetes.resources.limits`). With `runtime.enforce_resource_defaults: false` and no resources set, a Kubernetes pod gets the three default requests and no limits.
- The built-in CPU limit never ends up below a larger CPU request. If `requests.cpu` is above `2` and no tier sets `limits.cpu`, the built-in limit is raised to the request, on every runtime. If `kubernetes.resources.requests.cpu` is above the resulting limit and `kubernetes.resources.limits.cpu` is unset, `kubernetes.resources.limits.cpu` is set to that request; Docker and Podman keep `--cpus 2`. A CPU limit you set yourself is never changed, so keep it at or above your CPU request.

### Required Secrets

Define secrets that must be provided to the agent. During agent creation, Scion utilizes an interactive `secrets-gather` pipeline to prompt for missing values if they are not already securely stored on the backend, ensuring sensitive credentials are never written to plain text configuration files.

```yaml
secrets:
  - key: GEMINI_API_KEY
    description: "Gemini API key"
    type: environment
  - key: service_account
    description: "Service account JSON"
    type: file
    target: /run/secrets/sa.json
```

| Field | Type | Description |
| :--- | :--- | :--- |
| `key` | string | **Required**. The secret key name. |
| `description` | string | Human-readable description. |
| `type` | string | Projection type: `environment` (default), `variable`, or `file`. |
| `target` | string | For `file` type, the path where the secret is mounted. |

## Profiles (`profiles`)

Profiles bind a Runtime to a set of Harness Configs and overrides. They allow you to switch between environments (e.g., "Local Docker" vs "Remote Kubernetes") easily.

```yaml
profiles:
  local:
    runtime: docker
    default_template: gemini
    default_harness_config: gemini
    harness_overrides:
      gemini:
        image: "gemini:dev"
```

| Field | Type | Description |
| :--- | :--- | :--- |
| `runtime` | string | **Required**. Name of a runtime defined in `runtimes`. |
| `default_template` | string | Default template for agents created under this profile. |
| `default_harness_config` | string | Default harness config name for agents created under this profile. |
| `image_registry` | string | Profile-level registry. Beats the top-level `image_registry` of the same source; a saved Hub top-level value and a node's `SCION_IMAGE_REGISTRY` beat a settings-file profile value. See [Image registry order](/scion/reference/settings-precedence/#image-registry-order). |
| `volumes` | list | Volume mounts for agents created under this profile, appended after the harness config's `volumes`. Each entry has `target` (required), `source`, `read_only`, `type`, `bucket` (GCS bucket name), `prefix` (GCS object prefix), `mode` (mount options), `server` (NFS server host or IP) and `volume_name` (Cloud Run or GKE volume name). `type` is `local` (default; host bind mount, requires `source`), `gcs` (GCS FUSE mount, requires `bucket`), `nfs` (NFS mount, requires `server` and `source`), `cloudrun-volume` (Cloud Run managed volume, requires `volume_name`) or `gke-shared-volume` (GKE-provided shared volume such as a Filestore CSI PVC, requires `volume_name`). |
| `harness_overrides` | map | Per-harness-config overrides. Keys match `harness_configs` names. |
| `secrets` | list | Required secrets for agents created under this profile. |
| `resources` | object | Resource requests and limits for agents created under this profile. See [Resource Specification](#resource-specification-resources). |
| `shared_dir_storage_class` | string | (Kubernetes) StorageClass for shared-dir PVCs created under this profile. Wins over the runtime entry's value; an agent's inline config `kubernetes.shared_dir_storage_class` wins over this (a template's value is ignored). |
| `shared_dir_size` | string | (Kubernetes) Size for each shared-dir PVC created under this profile. Same precedence as `shared_dir_storage_class`. |
| `safe_to_evict` | bool | (Kubernetes) Safe-to-evict setting for agent pods created under this profile. Wins over the runtime entry's value; an agent's inline config `kubernetes.safeToEvict` wins over this (a template's value is ignored). |
| `clone_depth` | string or int | Git clone depth for clone-per-agent workspaces of agents under this profile: `full` for a full (non-shallow) clone, or an integer N from 1 to 999999999 for a clone of depth N. A template or agent `clone_depth` wins over this. Unset (absent, null or empty) keeps the default shallow (depth 1) clone. Applies only when the workspace is cloned (a new agent, or a recreated workspace). |
| `kubernetes_service_account_mappings` | map | (Kubernetes) Per-profile override of the runtime entry's `kubernetes_service_account_mappings`: for each service account email listed here, this ServiceAccount name wins over the runtime entry's. Other emails fall through to the runtime entry. Read from global settings only. |
| `kubernetes_block_service_account` | string | (Kubernetes) Per-profile override of the runtime entry's `kubernetes_block_service_account`. Read from global settings only. |

A profile cannot choose where its agents' storage lives: the removed `shared_dir_storage_backend`, `shared_dir_storage_backends`, `home_storage_backend` and `home_storage_leaf` keys are rejected by validation, reported as `storage_overrides` at startup, and refused at agent start with `422 storage_migration_required` (see the note under the runtime entry table above). Use `volumes` for a node-local directory.

**Shared-dir PVC class and size (Kubernetes).** Each key is resolved separately, and the first source that sets it wins: the agent's inline config `kubernetes:` block (a template's is ignored), then the profile, then the profile's runtime entry, then the built-in default (the cluster's default class and `10Gi`). On GKE Autopilot, set an RWX class such as `standard-rwx`. See [Shared Directory PVCs](/scion/hosted/ha/kubernetes/#shared-directory-pvcs).

**Safe-to-evict (Kubernetes).** The first source that sets `safe_to_evict` wins: the agent's inline config `kubernetes.safeToEvict` (a template's is ignored), then the agent's profile (`--profile`, or the profile the agent was created with, falling back to the active profile), then that profile's runtime entry. An explicit `true` at a higher level turns the annotation off even when a lower level sets `false`. On GKE Autopilot the annotation makes the pod an extended run time pod, which has its own limits and cost. See [Safe-to-Evict](/scion/hosted/ha/kubernetes/#safe-to-evict).

**Clone depth.** The first source that sets `clone_depth` wins: the agent's or template's `clone_depth`, then the agent's profile (`--profile`, or the profile the agent was created with, falling back to the active profile). When neither sets it, a clone-per-agent workspace is a shallow clone of depth 1. `full` clones the whole history, so `git rev-parse --is-shallow-repository` prints `false` and rebase, merge-base, `git log` and blame work without `git fetch --unshallow`. `0`, values above 999999999 and other values are rejected; use `full` for a full clone. The setting applies only when the broker clones the repository per agent (clone-per-agent); on the Kubernetes runtime it reaches both the NFS provisioning init container and the in-container clone. It takes effect only when the workspace is cloned: a new agent, or an agent whose workspace is recreated. Restarting an agent whose workspace is already a shallow clone keeps it shallow; to deepen it in place, run `git fetch --unshallow` in the workspace. For example:

```yaml
profiles:
  gke:
    runtime: gke-autopilot
    clone_depth: full
```

**Agent timezone.** Profiles do not set the `TZ` of a Hub-dispatched agent: a `TZ` in a profile's `harness_overrides` env is not used (profiles have no `env` key). The Hub-level default is `agent_defaults.default_timezone` (in `settings.yaml`, the top-level `default_timezone` key). For the full order and pins, see [Times and Timezones](/scion/reference/times-and-timezones/#agent-tz-hub-dispatched-agents).

#### Removed: profile `timezone`

The profile `timezone` field was removed. A request to `PUT /api/v1/admin/server-config` that still sends `profiles.<name>.timezone`, even as an empty string, is rejected with `422`. At startup a Postgres-backed Hub strips stored values from its profile settings. If exactly one zone was set and no Hub default existed, it is copied into `agent_defaults.default_timezone`, and `agent_defaults` then becomes admin-managed, so `settings.yaml` no longer seeds it on that Hub. A file-based Hub never rewrites `settings.yaml`; it logs each leftover key, and the `default_timezone: <zone>` line to add when one zone would have been copied.

## Volume Source Restrictions

A Runtime Broker refuses any agent mount whose host source reaches the broker host's own control files. This applies to every `volumes:` entry, whatever sets it (settings, profiles, harness configs, templates, flat Runtime Broker instance `agent_defaults`, or a create or start request from the Hub). It applies equally to read-only and read-write mounts. It also applies to shared directories, workspaces, agent homes and harness auth files.

The protected paths are, in the Scion global directory (`~/.scion`):

| Class | Paths |
| :--- | :--- |
| Credentials | `broker-credentials.json`, `hub-credentials/`, `dev-token`, `hub-profiles/`, `transport-token`, `scion-token` |
| Local settings | `settings.yaml`, `settings.yml`, `settings.json`, `.settings.lock`, `server.yaml` |
| Host identity and state | `runtime-brokers/`, `runtime-broker-state/`, `broker-workspaces/`, `hub-workspaces/`, `workspace-config-records/`, `server.pid`, `.scion` (the global project marker) |
| Host plugins | `plugins/` |
| Hub state | `hub-id`, and `hub.db` with its `-wal`, `-shm` and `-journal` files |

The broker's configured state directory (`server.broker.state_dir`) and a co-located Hub's SQLite database file are protected wherever they are configured.

A mount source is refused when:

- it equals one of these paths, contains one, or lies inside a protected directory;
- it is the Scion global directory itself, or contains it (for example `~` or `/`);
- it lies inside the Scion global directory but outside the agent-data subtrees below. For `volumes:` and shared directories, only these are allowed:
  - `~/.scion/workspace/...`
  - `~/.scion/projects/<project>/workspace/...`
  - `~/.scion/projects/<project>/worktrees/<name>/...`
  - `~/.scion/projects/<project>/.scion/agents/<agent>/workspace/...`
  - `~/.scion/project-configs/<dir>/.scion/agents/<agent>/workspace/...`
  - `~/.scion/project-configs/<dir>/shared-dirs/<name>/...`

  Harness auth files and `SCION_DEV_BINARIES` may not come from the Scion global directory at all.
- it cannot be resolved: a relative path (other than a named volume), a dangling symbolic link, or a path the broker cannot read.

Symbolic links are resolved on both sides before the comparison, and the resolved path is what gets mounted. The error a requester sees has the code `control_path_mount`. It names the source as configured and the class of path it reaches, never the resolved host path. The broker log adds the resolved paths.

The check runs at each of these points:

1. **Runtime Broker start.** The broker does not start when its worktree base or its NFS mount root (`nfs.mount_root` of the workspace storage settings) equals or contains a protected path. A flat Runtime Broker host does not start when an instance's `agent_defaults` or harness override volumes reach one.
2. **Agent create and start.** A Hub request whose volumes reach a protected path is refused with `400`, before anything is saved. The agent's merged volumes are checked again before its configuration is written.
3. **Container creation.** Every bind is checked just before the container is created, and the broker records the binds it requested on the container (the `scion.bind_sources` label). Right after creation, the binds the runtime reports must match that record exactly and are checked again. A refused or mismatching bind stops the container and fails the start.
4. **Running agents.** The broker re-checks the binds of running agents on its default runtime and on every non-default Docker, Podman or Apple container runtime it has registered. At startup it registers the runtime profiles of Hub-managed projects, of shared-storage projects that have agents on this broker, and of the project it was started from. It registers other projects' runtime profiles once it has used them since it started. It does this at startup, every 60 seconds, and sooner when the protected paths resolve somewhere else. An agent with a refused or mismatching bind is stopped and marked `error`. An agent whose mounts cannot be verified twice in a row is stopped too. A Docker or Podman agent created before this check existed has no bind record. It keeps running only if its container runtime is reached over a local unix socket and every bind source the runtime reports exists on the host and passes the check. Otherwise it is stopped and must be started again. That covers a remote daemon, whether selected with `DOCKER_HOST`, a Docker context, `CONTAINER_HOST` or a Podman connection, and a locality that cannot be determined. Apple container agents created before this check existed are always stopped once. If a protected path cannot be resolved at all (for example a dangling symbolic link at `~/.scion/settings.yaml`), nothing can be verified, so every running agent on the host is stopped until it is fixed.

**Limitations.**

- **Remote Docker daemon.** These checks protect a broker whose container runtime runs on the same host. With a remote Docker daemon (`DOCKER_HOST` pointing at another machine), bind sources name paths on the daemon's host, which the broker cannot verify. The operator must keep the broker's control files off that host and out of every bind source.
- **Apple container runtime.** The re-checks after creation and for running agents read only the binds the broker recorded on the container when it created it, not the runtime's own view of what it mounted.
- **Non-default runtimes of other projects after a restart.** Take an agent created before this check existed, on a non-default runtime (for example Podman on a broker whose default is Docker), in a linked project the broker has not used since it started. That agent is not re-checked until the broker uses its project again. Every new agent is still checked when it is created.
- **Agents created before this check existed.** For these agents only, the re-check trusts the bind sources the runtime reports, since there is no record to compare them with. A Docker Desktop path of the form `/host_mnt/<host path>` is judged as `<host path>`. Locality is judged by the daemon endpoint alone. A local unix socket that forwards to a daemon on another host or filesystem (for example an SSH-forwarded socket) is therefore treated as local.
- **Runtime listing.** If the broker cannot list a runtime's containers, it cannot tell which agents to check, so it stops none and tries again at the next check.
- **Hard links.** Directories are not walked. A hard link to a protected file, placed inside an allowed directory by a process on the host, is not detected. An agent cannot create such a link itself.

## Telemetry Configuration (`telemetry`)

Controls agent telemetry collection, forwarding, privacy filtering, and debug output. Telemetry settings can be defined at global or project scope and are merged across the hierarchy (last write wins). A template's `telemetry` block is ignored; the Hub telemetry configuration and project telemetry policy also apply, and an agent's own config can only turn telemetry on or off. See the [telemetry ladder](/scion/reference/settings-precedence/#the-telemetry-ladder).

See the [Metrics & OpenTelemetry guide](/scion/hosted/single-node/metrics/) for operational details.

### Basic Example

```yaml
telemetry:
  enabled: true
  cloud:
    enabled: true
    endpoint: "monitoring.googleapis.com:443"
    protocol: grpc
  filter:
    events:
      exclude:
        - "agent.user.prompt"
```

### Cloud Forwarding (`telemetry.cloud`)

Settings for forwarding telemetry to a cloud OTLP backend.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `enabled` | bool | `true` | Enable cloud forwarding. |
| `endpoint` | string | — | Cloud OTLP endpoint URL. |
| `protocol` | string | `grpc` | Transport protocol: `grpc` or `http`. |
| `provider` | string | — | Cloud provider name (for example `gcp`). |
| `cloud_logging` | bool | `false` | Send the Scion server's (Hub's) own logs directly to Google Cloud Logging. Read once at server startup; `SCION_CLOUD_LOGGING`, when set (even to `false`), takes precedence over the file. Agents do not use it: the value is carried into the agent's resolved telemetry config (`telemetry.cloud.cloud_logging`) but is not passed to the agent as an environment variable. On a database-backed Hub it cannot be changed through `PUT /api/v1/admin/server-config` (`422 startup_only_key_rejected`). |
| `headers` | map | — | Additional headers for OTLP export (e.g., `Authorization`). |
| `tls.enabled` | bool | `true` | Enable TLS for the connection. |
| `tls.insecure_skip_verify` | bool | `false` | Skip TLS certificate verification (development only). |
| `batch.max_size` | int | `512` | Maximum spans per batch export. |
| `batch.timeout` | string | `5s` | Maximum wait time before flushing a partial batch. |

### Hub Reporting (`telemetry.hub`)

Settings for reporting telemetry summaries to the Scion Hub.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `enabled` | bool | `true` | Enable Hub telemetry reporting. Auto-enabled in hosted mode. |
| `report_interval` | string | `30s` | Interval between Hub reports. |

### Local Debug Output (`telemetry.local`)

Settings for local debug telemetry output. These keys are accepted so existing settings files still load, but no component reads them today, so they have no effect.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `enabled` | bool | `false` | Accepted but ignored. |
| `file` | string | — | Accepted but ignored. |
| `console` | bool | `false` | Accepted but ignored. |

### Filtering (`telemetry.filter`)

Controls event filtering, attribute redaction, and sampling.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `enabled` | bool | `true` | Enable event filtering. |
| `respect_debug_mode` | bool | `true` | Accepted but ignored: no component reads this key today. |
| `events.include` | list | `[]` | Event types to include (empty = all). |
| `events.exclude` | list | `["agent.user.prompt"]` | Event types to exclude. |
| `attributes.redact` | list | See below | Attribute names to replace with `[REDACTED]`. |
| `attributes.hash` | list | `["session_id"]` | Attribute names to SHA256 hash. |
| `sampling.default` | float | `1.0` | Default sampling rate (0.0–1.0). |
| `sampling.rates` | map | `{}` | Per-event-type sampling rate overrides. |

Default redacted attributes: `prompt`, `user.email`, `tool_output`, `tool_input`.

### Resource Attributes (`telemetry.resource`)

Static key-value pairs added to all telemetry events. Useful for tagging deployments.

```yaml
telemetry:
  resource:
    service.name: "scion-agent"
    deployment.env: "staging"
```

## Server Configuration (`server`)

When running the `scion server` (Hub or Broker), configuration is read from the `server` section of `settings.yaml`.

See the [Server Configuration Reference](/scion/reference/server-config/) for details.

## Environment Variable Overrides

Settings can be overridden using environment variables with the `SCION_` prefix.

| Setting | Environment Variable |
| :--- | :--- |
| `active_profile` | `SCION_ACTIVE_PROFILE` |
| `default_template` | `SCION_DEFAULT_TEMPLATE` |
| `hub.project_id` | `SCION_HUB_PROJECT_ID` |
| `cli.autohelp` | `SCION_CLI_AUTOHELP` |
| `telemetry.enabled` | `SCION_TELEMETRY_ENABLED` |
| `telemetry.cloud.enabled` | `SCION_TELEMETRY_CLOUD_ENABLED` |
| `telemetry.cloud.cloud_logging` (server process only) | `SCION_CLOUD_LOGGING` |
| `telemetry.cloud.endpoint` | `SCION_OTEL_ENDPOINT` |
| `telemetry.cloud.protocol` | `SCION_OTEL_PROTOCOL` |
| `telemetry.cloud.tls.enabled` (inverted) | `SCION_OTEL_INSECURE` |
| `telemetry.cloud.tls.insecure_skip_verify` | `SCION_OTEL_SKIP_TLS_VERIFY` |
| `telemetry.cloud.tls.ca_file` | `SCION_OTEL_CA_FILE` |
| `telemetry.cloud.tls.cert_file` | `SCION_OTEL_CERT_FILE` |
| `telemetry.cloud.tls.key_file` | `SCION_OTEL_KEY_FILE` |
| `telemetry.hub.enabled` | `SCION_TELEMETRY_HUB_ENABLED` |

See [Local Governance](/scion/local/local-governance/) for more on variable substitution.
