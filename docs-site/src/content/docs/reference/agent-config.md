---
title: Agent Configuration (scion-agent.yaml)
description: Reference for Scion agent templates and configuration.
---

The `scion-agent.yaml` file acts as the blueprint for an agent. It defines the environment, resources, and harness configuration required to run the agent.

## File Locations

- **Templates**: `.scion/templates/<template-name>/scion-agent.yaml`
- **Active Agents**: `.scion/agents/<agent-name>/scion-agent.yaml`

:::note[Format Change]
Previous versions of Scion used `scion-agent.json`. The new versioned settings system uses `scion-agent.yaml`, though JSON is still supported as valid YAML.
:::

## Configuration Fields

### Core Fields

| Field | Type | Description |
| :--- | :--- | :--- |
| `schema_version` | string | Should be `"1"`. |
| `default_harness_config` | string | The name of the default harness config to use (e.g., `gemini`, `antigravity`). |
| `agent_instructions` | string | Role-specific instructions for the agent (harness-agnostic). |
| `system_prompt` | string | The system prompt to use for the agent (harness-agnostic). |
| `image` | string | Override the container image defined in the harness config. |
| `env` | map | Environment variables to inject into the container. |
| `volumes` | list | Additional volume mounts. |
| `detached` | bool | Run in background (default `true`). |
| `command_args` | list | Additional arguments passed to the harness entrypoint. |
| `task_flag` | string | CLI flag name for passing the task (e.g., `--input`). When set, the task is delivered as a flag value instead of a positional argument. |
| `model` | string | LLM model identifier override. |

:::caution[Harness Field Deprecated]
The `harness` field is no longer supported in `scion-agent.yaml`. Templates must be harness-agnostic. Use `default_harness_config` to specify a preferred harness, which can be overridden by users at runtime.
:::

### Platform and Workspace Skills

Scion automatically injects contextual platform skills into the agent's environment during provisioning:

- **`git-sandbox`**: Injected if the agent is running in a Git-backed workspace. Provides operational context for git worktree and branch management.
- **Platform Skills**: Core instructions (such as status signaling, messaging, and command operations) are dynamically injected to guide the agent in interacting with the system.

These platform skills are managed by Scion and do not need to be manually included in your template definition.

### Limits & Resources

| Field | Type | Description |
| :--- | :--- | :--- |
| `max_turns` | int | Maximum number of LLM turns before the agent stops. Exceeding this triggers a `LIMITS_EXCEEDED` state and termination. |
| `max_duration` | string | Maximum runtime duration (e.g., `"2h"`, `"30m"`). Exceeding this triggers a `LIMITS_EXCEEDED` state and termination. |
| `resources` | object | Container resource requests/limits (see below). |

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

Specify a different Hub endpoint for this agent.

```yaml
hub:
  endpoint: "https://hub.example.com"
```

### Required Secrets (`secrets`)

Define secrets required by the agent. These follow the same schema as [Orchestrator Settings Secrets](/scion/reference/orchestrator-settings/#required-secrets).

### Gemini Settings (`gemini`)

Harness-specific settings for Gemini.

```yaml
gemini:
  auth_selectedType: "vertex-ai"
```

### Telemetry (`telemetry`)

Override telemetry settings for this template or agent. These merge on top of any telemetry configuration defined in `settings.yaml` (global or project scope), using last-write-wins semantics.

```yaml
telemetry:
  enabled: true
  cloud:
    endpoint: "monitoring.googleapis.com:443"
  filter:
    events:
      exclude:
        - "agent.user.prompt"
  resource:
    service.name: "my-specialized-agent"
```

See the [Orchestrator Settings Reference](/scion/reference/orchestrator-settings/#telemetry-configuration-telemetry) for the full field reference and the [Metrics guide](/scion/hosted/single-node/metrics/#configuration-hierarchy) for how telemetry settings merge across scopes.

### Kubernetes Specifics (`kubernetes`)

Overrides for Kubernetes runtimes.

```yaml
kubernetes:
  namespace: "custom-ns"
  serviceAccountName: "workload-identity-sa"
  runtimeClassName: "gvisor"
```

## Resolution Logic

When an agent starts:

1.  **Template Load**: Scion loads `scion-agent.yaml` from the selected template.
2.  **Harness Resolution**: It resolves the `harness_config` against the active profile's `harness_configs` map in `settings.yaml`.
3.  **Overrides**: CLI flags (e.g., `--image`, `--env`) override values in `scion-agent.yaml`.
4.  **Final Config**: The resolved configuration is written to the agent's runtime directory.

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
the template sit hub `agent_defaults` and the broker's own `settings.yaml`, and
`resources` does not follow this ordering at all.

Environment variables are governed by a **separate** precedence system that ranks
the same-named sources differently. A rule taken from one and applied to the other
gives answers backwards.

See **[Settings Precedence](/reference/settings-precedence/)** for both systems in
full, the `SCION_*` variables Scion injects, and the known gaps.
:::

`resources` merges **field by field** rather than as a whole block. A template
that sets only a memory limit keeps that limit and still picks up the project's
CPU and disk defaults. The same is true of the three limit fields.

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
hub-created agent.
