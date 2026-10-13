---
title: Agent Configuration (scion-agent.yaml)
description: Reference for Scion agent templates and configuration.
---

The `scion-agent.yaml` file describes an agent's **job**: what it does, which harness config and model it prefers, its instructions, skills, tools and limits. The same file format is used for templates and for an agent's own inline config (`--config`, the create API, the web form), but the two may set different keys. **A template describes the job, not the environment**: where and how the agent runs (image, resources, volumes, Kubernetes settings, telemetry, hub access) comes from the harness-config, settings and profiles, the hub and the project.

## File Locations

- **Templates**: `.scion/templates/<template-name>/scion-agent.yaml`
- **Active Agents**: `.scion/agents/<agent-name>/scion-agent.yaml`

:::note[Format Change]
Previous versions of Scion used `scion-agent.json`. The new versioned settings system uses `scion-agent.yaml`, though JSON is still supported as valid YAML.
:::

## Key classes

| Class | Keys | In a template | In an inline config | Where the value comes from otherwise |
| :--- | :--- | :--- | :--- | :--- |
| **Job keys** | `harness_config` / `default_harness_config`, `model`, `thinking_level`, `agent_instructions`, `system_prompt`, `skills`, `mcp_servers`, `command_args`, `task_flag`, `max_turns`, `max_model_calls`, `max_duration`, `services`, `secrets` (names only), `message_mode` | Set | Set; beats the project and the template | Project defaults, hub `agent_defaults`, broker settings, the harness-config |
| **Template env** | `env` | A [fallback](#environment-variables-env) for the job's own tools | Request env, at its usual System A rank | Settings, hub env scopes, secrets, the runtime |
| **Environment keys** | `image`, `resources`, `volumes`, `kubernetes`, `hub`, `telemetry` | **Ignored, with a warning** | Set for this agent (`telemetry`: on/off only) | The harness-config (`config.yaml`), broker settings and profiles, hub settings, project policy |

A template that sets an environment key still loads. The agent start prints `template <name> sets environment key(s) <keys>; ignored (environment comes from settings)`, the hub logs the same warning when the template is committed, and a hub admin can list every affected template with `GET /api/v1/admin/validate-resources` (`templateConfigFindings`, kind `environment_keys`). Remove the key from the template and set the value where the last column says. For the full precedence of every class, see [Settings Precedence](/scion/reference/settings-precedence/#the-model-a-template-describes-the-job-settings-describe-the-environment).

Edits to a template's `env` take effect on an agent's next start. Its job keys are read when the agent is created.

## Configuration Fields

### Core Fields

| Field | Type | Description |
| :--- | :--- | :--- |
| `schema_version` | string | Should be `"1"`. |
| `default_harness_config` | string | The name of the default harness config to use (e.g., `gemini`, `antigravity`). |
| `agent_instructions` | string | Role-specific instructions for the agent (harness-agnostic). |
| `system_prompt` | string | The system prompt to use for the agent (harness-agnostic). |
| `harness_config` | string | The harness config to use. When a template sets both this and `default_harness_config`, `default_harness_config` wins. |
| `env` | map | Environment variables. In a template, a lowest-precedence fallback; see [Environment variables](#environment-variables-env). |
| `detached` | bool | Run in background (default `true`). |
| `command_args` | list | Additional arguments passed to the harness entrypoint. |
| `task_flag` | string | CLI flag name for passing the task (e.g., `--input`). When set, the task is delivered as a flag value instead of a positional argument. |
| `model` | string | LLM model identifier override. |
| `thinking_level` | int | Harness-agnostic thinking level from 0 to 100, mapped by the harness-config to a native tier (see [Thinking Level Map](/scion/reference/harness-settings/#thinking-level-map-thinking)). |
| `message_mode` | string | Hub only: the initial message mode (`none`, `lineage`, `branch`, `project`, `hub`) for agents created from this template when the create request names none. |

:::caution[Harness Field Deprecated]
The `harness` field is no longer supported in `scion-agent.yaml`. Templates must be harness-agnostic. Use `default_harness_config` to specify a preferred harness, which can be overridden by users at runtime.
:::

### Platform and Workspace Skills

Scion automatically injects contextual platform skills into the agent's environment during provisioning:

- **`git-sandbox`**: Injected if the agent is running in a Git-backed workspace. Provides operational context for git worktree and branch management.
- **Platform Skills**: Core instructions (such as status signaling, messaging, and command operations) are dynamically injected to guide the agent in interacting with the system.

These platform skills are managed by Scion and do not need to be manually included in your template definition.

### Limits

| Field | Type | Description |
| :--- | :--- | :--- |
| `max_turns` | int | Maximum number of LLM turns before the agent stops. Exceeding this triggers a `LIMITS_EXCEEDED` state and termination. |
| `max_model_calls` | int | Maximum number of model calls before the agent stops. |
| `max_duration` | string | Maximum runtime duration (e.g., `"2h"`, `"30m"`). Exceeding this triggers a `LIMITS_EXCEEDED` state and termination. |

### Environment variables (`env`)

```yaml
env:
  REVIEW_STRICTNESS: high
  LINT_CONFIG: /home/scion/.config/lint.yaml
```

In a **template**, `env:` is for the job's own tools, and each variable is a fallback:

- **It applies only when no other source sets the key.** Settings harness-config env, hub, project and user env, request env, secrets, and the env the runtime adds itself (auth, harness and telemetry env) all win over it.
- **Values are literal.** `${VAR}` is not expanded (it is used as written, with a warning), and an empty value is not filled from the broker host's environment (it is skipped, with a warning).
- **`SCION_*` keys are ignored.** That namespace belongs to the platform.
- **Credentials never come from a template.** The auth and credential keys the harness-config's auth metadata lists are never filled from template env, even when nothing else sets them. Supply credentials as secrets or hub env instead.
- **Edits take effect on the next start.** Template env is not stored in the agent's config; the broker reads it from the template's current version on every start. If the template can no longer be found, the start warns and applies none.

In an agent's **inline config**, `env` is request env and ranks above the hub storage scopes; see [Settings Precedence](/scion/reference/settings-precedence/#a2-the-config-rung-sits-above-all-four-storage-scopes).

## Environment keys (inline config only)

The keys in this section are **ignored in templates**, with a warning. They take effect only in an agent's own inline config (`--config`, the create API or the web form), where they apply to that one agent. Otherwise their values come from the harness-config, settings and profiles, and the hub.

| Field | Type | Description | Set it instead in |
| :--- | :--- | :--- | :--- |
| `image` | string | Container image for this agent. | The harness-config's `config.yaml` `image`; settings `harness_configs.<name>.image`; a profile `harness_overrides.<name>.image`; `--image` |
| `volumes` | list | Additional volume mounts. | Settings `harness_configs.<name>.volumes`; the harness-config |
| `resources` | object | Container resource requests/limits (see below). | Settings `default_resources`, profile `resources` and `harness_overrides`; hub `agent_defaults.resources`; project resource defaults |
| `kubernetes` | object | Kubernetes runtime overrides (see below). | Settings runtimes and profiles |
| `hub` | object | Hub endpoint override (see below). | Broker settings |
| `telemetry` | object | Telemetry. In an inline config only `enabled` is honoured. | Settings `telemetry`; hub telemetry config; project telemetry policy |

### Workspace Clone

| Field | Type | Description |
| :--- | :--- | :--- |
| `clone_depth` | string or int | Git clone depth for a clone-per-agent workspace: `full` for a full (non-shallow) clone, or an integer N from 1 to 999999999 for a clone of depth N. Overrides the profile's `clone_depth`. Unset (absent, null or empty) keeps the profile's value, else the default shallow clone of depth 1. Applies only when the workspace is cloned (a new agent, or a recreated workspace); restarting an agent whose workspace is already a shallow clone keeps it shallow. See [Profiles](/scion/reference/orchestrator-settings/#profiles-profiles). |

### Resource Specification

```yaml
resources:
  requests:
    cpu: "500m"
    memory: "512Mi"
  limits:
    cpu: "2"
    memory: "2Gi"
  disk: "10Gi"
```

### Sidecar Services (`services`)

Define auxiliary containers to run alongside the agent (e.g., a headless browser).

```yaml
services:
  - name: browser
    command: ["chromium", "--headless"]
    env:
      DISPLAY: ":99"
    ready_check:
      type: tcp
      target: "localhost:9222"
  - name: delayed-job
    command: ["./worker.sh"]
    ready_check:
      type: delay
      target: "5s"
```

| Field | Type | Description |
| :--- | :--- | :--- |
| `name` | string | **Required**. Service name. |
| `command` | list | **Required**. Entrypoint and arguments. |
| `restart` | string | Restart policy: `no`, `always`, or `on-failure`. |
| `env` | map | Environment variables for the service. |
| `ready_check` | object | Health check to determine if the service is ready. |

#### Ready Check (`ready_check`)

| Field | Type | Description |
| :--- | :--- | :--- |
| `type` | string | `tcp`, `http`, or `delay`. |
| `target` | string | Host:port (tcp/http) or duration (delay). |
| `timeout` | string | Maximum wait time. |

### Hub Override (`hub`)

Inline config only. Specify a different Hub endpoint for this agent.

```yaml
hub:
  endpoint: "https://hub.example.com"
```

### Required Secrets (`secrets`)

Declare the secrets the agent's job needs. A template names secrets only; their values always come from the environment (hub or project secrets). These follow the same schema as [Orchestrator Settings Secrets](/scion/reference/orchestrator-settings/#required-secrets).

### Gemini Settings (`gemini`)

Harness-specific settings for Gemini.

```yaml
gemini:
  auth_selectedType: "vertex-ai"
```

### Telemetry (`telemetry`)

An environment key. A template's `telemetry:` block is ignored. In an agent's inline config only `enabled` is honoured: it turns telemetry on or off for that agent (the CLI's `--enable-telemetry` / `--disable-telemetry`), below the project's telemetry policy. Other inline telemetry fields are ignored with a warning.

```yaml
telemetry:
  enabled: false
```

Everything else about telemetry (exporters, filters, resource attributes) is configured in settings and the hub telemetry config. See the [Orchestrator Settings Reference](/scion/reference/orchestrator-settings/#telemetry-configuration-telemetry) for the fields and the [telemetry ladder](/scion/reference/settings-precedence/#the-telemetry-ladder) for precedence.

### Kubernetes Specifics (`kubernetes`)

Inline config only; ignored in templates. Overrides for Kubernetes runtimes.

```yaml
kubernetes:
  namespace: "custom-ns"
  serviceAccountName: "workload-identity-sa"
  runtimeClassName: "gvisor"
  priorityClassName: "scion-agent-priority"
  safeToEvict: false
```

`priorityClassName` must name a `PriorityClass` that already exists on the cluster — Scion does not create one — and overrides any `runtimes.<name>.priority_class_name` default set in `settings.yaml`. See [Running Scion on Kubernetes](/scion/hosted/ha/kubernetes/#pod-priority-and-preemption) for why to set it.

`safeToEvict: false` adds the `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"` annotation to the pod. It overrides `safe_to_evict` on the profile and the runtime entry, and an explicit `true` turns the annotation off. See [Safe-to-Evict](/scion/hosted/ha/kubernetes/#safe-to-evict).

## Resolution Logic

When an agent starts:

1.  **Template Load**: Scion loads `scion-agent.yaml` from the selected template chain and drops its environment keys and its `env` (with a warning for environment keys).
2.  **Harness Resolution**: It resolves the `harness_config` against the active profile's `harness_configs` map in `settings.yaml`.
3.  **Overrides**: the project defaults, the inline config and CLI flags (e.g., `--model`, `--image`) override the template's job keys.
4.  **Final Config**: The resolved job config and the requester's own environment values are written to the agent's runtime directory. Image, resources, volumes and telemetry from settings, profiles and the hub are applied at each start and are not written there. Template env is applied at each start as a fallback.

### Project settings precedence

Agents created inside a hub project have one more source of values: the
project's own defaults, set in Project Settings and stored as `scion.io/*`
annotations on the project. For these fields the ordering is, highest priority
first:

1.  **The agent-create request** — the value passed to `scion agent create`, the
    API, or the web form.
2.  **The project setting** — the project default.
3.  **The template** — the value in the selected template's `scion-agent.yaml`.

| Field | Project setting |
| --- | --- |
| `harness_config` | `scion.io/default-harness-config` |
| `model` | `scion.io/default-model` |
| Active profile | `scion.io/active-profile` |
| `max_turns` | `scion.io/default-max-turns` |
| `max_model_calls` | `scion.io/default-max-model-calls` |
| `max_duration` | `scion.io/default-max-duration` |
| `resources` | `scion.io/default-resources-cpu-request`, `-memory-request`, `-cpu-limit`, `-memory-limit`, `-disk` |

A project setting is an *override*: it takes precedence over the template the
project uses. Leave it blank to let the template — and then the rest of the
chain — decide. Blank means **unset, falls through** — it does not mean "set to
whatever the default happens to be".

:::note[This is a partial ordering, and it does not cover environment variables]
The three ranks above are the top of a longer chain, not the whole of it — below
the template sit hub `agent_defaults`, the broker's own `settings.yaml` and the
harness-config. A hub default applies only when neither the template nor the
project sets the value. `resources` does not follow this ordering at all, and a
template cannot set it.

Environment variables are governed by a **separate** precedence system that ranks
the same-named sources differently. A rule taken from one and applied to the other
gives answers backwards.

See **[Settings Precedence](/scion/reference/settings-precedence/)** for both systems in
full, the `SCION_*` variables Scion injects, and the known gaps.
:::

`resources` merges **field by field** rather than as a whole block. An inline
config that sets only a memory limit keeps that limit and still picks up the
project's CPU and disk defaults. The three limit fields are filled field by field
in the same way.

:::caution[Behaviour change]
Before this was corrected, three project settings were accepted and persisted
but never reached the agent:

-   **`scion.io/default-harness-config`** was silently outranked by the
    template's `harness_config`, on both the interactive and scheduled
    agent-create paths.
-   **`scion.io/active-profile`** was never applied at all. Agents ran under the
    broker's own active profile instead. This one has the widest reach — the
    profile selects the runtime and manager, and is also consulted when
    resolving harness configs and extracting the agent's env vars and secrets.
-   **Project `resources`** were discarded in full whenever the template set any
    single resource field.

If you had set any of these on a project and worked around them being ignored —
for example by pinning the value in the template instead — that workaround now
competes with the project setting, and the project setting wins. Review your
project defaults before upgrading.
:::

Values resolved above are stamped onto the agent by the hub before dispatch, so
they outrank anything the broker would otherwise supply from its local
`settings.yaml`. That is intended: hub configuration should win for a
hub-created agent. Hub `agent_defaults` are the exception: they fill only what the
request, the project and the template all left unset.
