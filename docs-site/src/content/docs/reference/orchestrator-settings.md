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
| `active_profile` | string | The name of the profile to use by default (e.g., `local`, `remote`). |
| `default_template` | string | The default template to use when creating agents (e.g., `gemini`, `antigravity`). |
| `default_harness_config` | string | Default harness config to use. |
| `default_harness_auth` | string | Default authentication type for new agents (e.g., `api-key`, `vertex-ai`, `none`). |
| `image_registry` | string | Registry prefix for all standard harness images. Rewrites the registry portion of `scion-*` images (e.g., `ghcr.io/myorg`). See [Building Custom Images](/scion/local/custom-images/). |
| `default_max_turns` | int | Default maximum number of turns an agent can take before termination. |
| `default_max_model_calls` | int | Default maximum number of LLM model calls an agent can make. |
| `default_max_duration` | string | Default maximum execution time (e.g., `"2h"`, `"45m"`) for an agent. |
| `default_resources` | object | Default resource constraints (CPU, memory, disk). See [Resource Specification](#resource-specification-resources) below. |
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
| `autohelp` | bool | Whether to print usage help on every error. Default: `true`. |
| `interactive_disabled` | bool | If `true`, disables all interactive prompts (useful for scripts). |

## Hub Client Configuration (`hub`)

Settings for connecting the CLI to a Scion Hub.

```yaml
hub:
  enabled: true
  endpoint: "https://hub.example.com"
  project_id: "uuid-or-slug"
  local_only: false
```

| Field | Type | Description |
| :--- | :--- | :--- |
| `enabled` | bool | Whether to enable Hub integration for this project. |
| `endpoint` | string | The Hub API endpoint URL. Can be overridden per-agent in `scion-agent.yaml`. |
| `project_id` | string | The unique identifier for this project on the Hub. |
| `local_only` | bool | If `true`, forces local-only operation even if the Hub is configured. |

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
| `priority_class_name` | string | (Kubernetes) Default `priorityClassName` applied to agent pods using this runtime entry. Must name a `PriorityClass` that already exists on the cluster — Scion does not create one. A template/agent `kubernetes.priorityClassName` overrides this. Unset means no priority class (pods schedule at priority 0). |
| `list_all_namespaces` | bool | (Kubernetes) List agents across all namespaces. Default: `false`. |
| `shared_dir_storage_class` | string | (Kubernetes) Default StorageClass for shared-dir PVCs. Must support `ReadWriteMany`. A profile's value wins over this, and a template or agent `kubernetes.shared_dir_storage_class` wins over both. Default: the cluster's default class. |
| `shared_dir_size` | string | (Kubernetes) Default size for each shared-dir PVC, as a positive Kubernetes quantity (e.g. `10Gi`, `1Ti`). Same precedence as `shared_dir_storage_class`. Default: `10Gi`. |
| `safe_to_evict` | bool | (Kubernetes) Set to `false` to add the `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"` annotation to agent pods. Only `false` has an effect; unset or `true` adds nothing. A profile's value wins over this, and a template or agent `kubernetes.safeToEvict` wins over both. Ignored, with a validation warning, on other runtime types. |
| `shared_dir_storage_backend` | string | `local` or `nfs`. Overrides [`server.shared_dir_storage.backend`](/scion/reference/server-config/#per-profile-backend) for agents whose profile uses this runtime. A profile's value wins over this. Read from global settings only. |
| `kubernetes_service_account_mappings` | map | (Kubernetes) Map of lowercase GCP service account email to Kubernetes ServiceAccount name, used by GCP identity mode `assign`: the agent pod runs as the mapped ServiceAccount through GKE Workload Identity. The ServiceAccount must already exist in this entry's namespace and be bound to the service account; Scion does not create or bind it. A profile's entry for the same email wins over this. Read from global settings only; a project's `settings.yaml` value is ignored. See [GCP identity mode "assign"](/scion/hosted/ha/kubernetes/#gcp-identity-mode-assign-workload-identity-mapping). |
| `env` | map | Environment variables to set for the runtime. |

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
| `resources` | object | Resource limits (CPU, memory, disk) for this harness. |

### Resource Specification (`resources`)

Defines the hardware constraints for an agent's execution environment.

```yaml
resources:
  cpu: "2"
  memory: "4Gi"
  disk: "20Gi"
  gpu: 0
```

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `cpu` | string | `"1"` | CPU cores (can be fractional, e.g., `"0.5"`). |
| `memory` | string | `"2Gi"` | Memory limit (e.g., `"1Gi"`, `"512Mi"`). |
| `disk` | string | `"10Gi"` | Ephemeral disk space request. |
| `gpu` | int | `0` | Number of GPUs to request (requires compatible runtime). |

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
| `default_harness_config` | string | Default harness config to use. |
| `default_harness_auth` | string | Default authentication type for new agents under this profile. |
| `image_registry` | string | Profile-level registry override. Takes precedence over the top-level `image_registry`. |
| `harness_overrides` | map | Per-harness-config overrides. Keys match `harness_configs` names. |
| `secrets` | list | Required secrets for agents created under this profile. |
| `shared_dir_storage_class` | string | (Kubernetes) StorageClass for shared-dir PVCs created under this profile. Wins over the runtime entry's value; a template or agent `kubernetes.shared_dir_storage_class` wins over this. |
| `shared_dir_size` | string | (Kubernetes) Size for each shared-dir PVC created under this profile. Same precedence as `shared_dir_storage_class`. |
| `safe_to_evict` | bool | (Kubernetes) Safe-to-evict setting for agent pods created under this profile. Wins over the runtime entry's value; a template or agent `kubernetes.safeToEvict` wins over this. |
| `shared_dir_storage_backend` | string | `local` or `nfs`. Overrides [`server.shared_dir_storage.backend`](/scion/reference/server-config/#per-profile-backend) for agents using this profile. Wins over the runtime entry's value. Read from global settings only. |
| `kubernetes_service_account_mappings` | map | (Kubernetes) Per-profile override of the runtime entry's `kubernetes_service_account_mappings`: for each service account email listed here, this ServiceAccount name wins over the runtime entry's. Other emails fall through to the runtime entry. Read from global settings only. |

**Shared-dir PVC class and size (Kubernetes).** Each key is resolved separately, and the first source that sets it wins: the agent's or template's `kubernetes:` block, then the profile, then the profile's runtime entry, then the built-in default (the cluster's default class and `10Gi`). On GKE Autopilot, set an RWX class such as `standard-rwx`. See [Shared Directory PVCs](/scion/hosted/ha/kubernetes/#shared-directory-pvcs).

**Safe-to-evict (Kubernetes).** The first source that sets `safe_to_evict` wins: the agent's or template's `kubernetes.safeToEvict`, then the agent's profile (`--profile`, or the profile the agent was created with, falling back to the active profile), then that profile's runtime entry. An explicit `true` at a higher level turns the annotation off even when a lower level sets `false`. On GKE Autopilot the annotation makes the pod an extended run time pod, which has its own limits and cost. See [Safe-to-Evict](/scion/hosted/ha/kubernetes/#safe-to-evict).

**Agent timezone.** Profiles do not set the `TZ` of a Hub-dispatched agent: a `TZ` in a profile's `harness_overrides` env is not used (profiles have no `env` key). The Hub-level default is `agent_defaults.default_timezone` (in `settings.yaml`, the top-level `default_timezone` key). For the full order, pins and local mode, see [Times and Timezones](/scion/reference/times-and-timezones/#agent-tz-hub-dispatched-agents).

#### Removed: profile `timezone`

The profile `timezone` field was removed. A request to `PUT /api/v1/admin/server-config` that still sends `profiles.<name>.timezone`, even as an empty string, is rejected with `422`. At startup a Postgres-backed Hub strips stored values from its profile settings. If exactly one zone was set and no Hub default existed, it is copied into `agent_defaults.default_timezone`, and `agent_defaults` then becomes admin-managed, so `settings.yaml` no longer seeds it on that Hub. A file-based Hub never rewrites `settings.yaml`; it logs each leftover key, and the `default_timezone: <zone>` line to add when one zone would have been copied.

## Telemetry Configuration (`telemetry`)

Controls agent telemetry collection, forwarding, privacy filtering, and debug output. Telemetry settings can be defined at global or project scope and are merged across the hierarchy (last write wins). They can also be overridden per-template or per-agent in `scion-agent.yaml`.

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

Settings for local debug telemetry output.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `enabled` | bool | `false` | Enable local debug output. |
| `file` | string | — | Path for JSONL telemetry file output. |
| `console` | bool | `false` | Write debug telemetry to stderr. |

### Filtering (`telemetry.filter`)

Controls event filtering, attribute redaction, and sampling.

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `enabled` | bool | `true` | Enable event filtering. |
| `respect_debug_mode` | bool | `true` | Bypass filters when debug mode is active. |
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
| `hub.endpoint` | `SCION_HUB_ENDPOINT` |
| `hub.project_id` | `SCION_HUB_PROJECT_ID` |
| `cli.autohelp` | `SCION_CLI_AUTOHELP` |
| `telemetry.enabled` | `SCION_TELEMETRY_ENABLED` |
| `telemetry.cloud.enabled` | `SCION_TELEMETRY_CLOUD_ENABLED` |
| `telemetry.cloud.endpoint` | `SCION_OTEL_ENDPOINT` |
| `telemetry.cloud.protocol` | `SCION_OTEL_PROTOCOL` |
| `telemetry.cloud.tls.insecure_skip_verify` | `SCION_OTEL_INSECURE` |
| `telemetry.hub.enabled` | `SCION_TELEMETRY_HUB_ENABLED` |
| `telemetry.local.enabled` | `SCION_TELEMETRY_DEBUG` |

See [Local Governance](/scion/local/local-governance/) for more on variable substitution.
