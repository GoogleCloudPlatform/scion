---
title: Supported Agent Harnesses
---

Scion supports multiple LLM agent "harnesses". A harness is an adapter that allows Scion to manage the lifecycle, authentication, and configuration of a specific agent tool.

:::note[All harnesses use container-script provisioning]
Scion no longer ships compiled-in ("builtin") harness implementations. Every harness is now
defined declaratively as a bundle under `harnesses/<name>/` (a `config.yaml` plus a
`provision.py` container-script) and is provisioned uniformly inside the agent container. The
shared `scion_harness` Python library standardizes auth resolution, logging, instruction
projection, and MCP configuration across all bundles. See
[Building Custom Images](/scion/local/custom-images/) and
[Harness-Specific Settings](/scion/reference/harness-settings/) for how bundles are packaged and
managed.

A harness-config that still declares `provisioner.type: builtin`, or a container-script provisioner
with an empty `command`, cannot be launched: the start fails before the container is created and
the error names the fix for where the config lives. A Runtime Broker rejects it with
`422 harness_config_unusable`; through the Hub, the message is currently included in the dispatch
error, which the Hub reports as a runtime error. For a global harness-config of a bundled harness the
fix is `scion harness-config upgrade <name> --activate-script`; for a project or template copy, edit
its `config.yaml` (on the broker host, when the broker launched from its own copy); for a Hub
harness-config, pull it with `scion harness-config pull <name>`, repair it, and upload it with
`scion harness-config sync <name>` (add `--global` for a global record). See `harnesses/README.md`
for details.

`antigravity` and `gemini-cli` are installed by default. `opencode`, `codex`, `copilot`, `hermes`,
`grok-build`, and `muse-code` are opt-in bundles you add via a [harness-config](/scion/reference/harness-settings/#managing-harness-configs).
:::

## 1. Gemini CLI (`gemini-cli`)

The default harness for interacting with Google's Gemini models via the `gemini` CLI tool.

### Authentication
The Gemini harness supports three authentication methods (auto-detected in this order):
- **API Key** (`api-key`): Set `GEMINI_API_KEY` or `GOOGLE_API_KEY` in your environment.
- **OAuth** (`auth-file`): Uses `~/.gemini/oauth_creds.json` if available.
- **Vertex AI** (`vertex-ai`): Uses Application Default Credentials (ADC) with `GOOGLE_CLOUD_PROJECT`.

Auth type can be explicitly set via `auth_selectedType` in your Scion settings profile. See [Agent Credentials](/scion/local/agent-credentials/) for details.

### Configuration
- **scion-agent.yaml**: Can be configured via `agent_instructions` and `system_prompt` fields in the template.
- **Settings File**: `~/.gemini/settings.json` (inside the agent container). Scion automatically updates `security.auth.selectedType` in this file to match the resolved auth method.
- **System Prompt**: `~/.gemini/system_prompt.md` is automatically seeded if `system_prompt` is provided in the agent config. Additionally, Scion injects the system prompt into the `GEMINI_SYSTEM_MD` environment variable to ensure direct pickup by the Gemini CLI tool during initialization.
- **Model Aliases**: Supports both traditional alias sizes and single-letter model alias mappings (`S` / `M` / `L` for Small / Medium / Large). The `provision.py` script automatically maps and handles fallback alias resolution during startup.
- **Default model**: the harness-config declares `model: medium`, so an agent started without a model runs on the model that the `medium` alias maps to in `model_aliases` (see `harnesses/gemini-cli/config.yaml`). The broker resolves that tier before it sets `SCION_MODEL`. The container image does not pin a model in `settings.json`.
- **Model selection**: the model is resolved in this order: the agent's resolved model (`--model`, template `model:`, or `SCION_MODEL`), then `harness_config.model`. Tier aliases (`small`, `medium`, `large`, `extra-large`, and `S` / `M` / `L` / `XL`) are resolved through the harness's `model_aliases` table. The provisioner writes the resolved model to `model.name` in `~/.gemini/settings.json` on every provision. If neither source gives a model, it removes any existing `model.name`, so a stale value is never kept and the Gemini CLI uses its own built-in default.

### Known Limitations
- The `gemini` CLI tool must be installed in the container image (included in default images).

---

## 2. Claude Code (`claude`)

A harness for Anthropic's "Claude Code" agent.

### Authentication
Claude supports four authentication methods (auto-detected in this precedence order):
- **API Key** (`api-key`): Set `ANTHROPIC_API_KEY` in your host environment. Scion propagates this to the agent and pre-approves it in `.claude.json` so Claude Code does not prompt for confirmation.
- **OAuth Token** (`oauth-token`): Set `CLAUDE_CODE_OAUTH_TOKEN` (generate with `claude setup-token`). This is also the token captured automatically after an in-agent `claude setup-token` login.
- **Auth File** (`auth-file`): Uses `~/.claude/.credentials.json` (file-secret key `CLAUDE_AUTH`) if available.
- **Vertex AI** (`vertex-ai`): Uses Google Cloud's Vertex AI endpoint with ADC, `GOOGLE_CLOUD_PROJECT`, and `GOOGLE_CLOUD_REGION`. Scion automatically translates `GOOGLE_CLOUD_PROJECT` to `ANTHROPIC_VERTEX_PROJECT_ID` during container provisioning to ensure compatibility with Claude Code's native Vertex AI client.

If no credentials are found, the agent drops to a shell — run `claude setup-token` interactively, then capture the credential with `capture_auth.py` (see [Harness Authentication](/scion/local/agent-credentials/#capturing-credentials-from-a-running-agent)).

Auth type can be explicitly set via `auth_selectedType` in your Scion settings profile. See [Agent Credentials](/scion/local/agent-credentials/) for details.

### Configuration
- **scion-agent.yaml**: Can be configured via `agent_instructions` and `system_prompt` fields in the template.
- **Config File (`~/.claude.json`)**: Scion manages project-specific settings in this file to ensure the agent respects workspace boundaries.
- **Projects**: Scion automatically configures the current workspace as a project in `.claude.json`.
- **Environment Hardening (`~/.claude/settings.json`)**: Scion pre-populates a strict permissions deny list and security hardeners in the agent's native settings file:
    - **Permissions Deny List**: Denies potentially hazardous operations such as `EnterPlanMode`, `ExitPlanMode`, `DesignSync`, `NotebookEdit`, `SendMessage`, `PushNotification`, `RemoteTrigger`, `ReportFindings`, `ScheduleWakeup`, `AskUserQuestion`, `CronCreate`, `CronDelete`, and `CronList`.
    - **Hardening Flags**: Disables experimental or outbound-connecting features by setting `disableBundledSkills`, `disableWorkflows`, `disableRemoteControl`, `disableClaudeAiConnectors`, and `disableArtifacts` to `true`.
- **Model Resolution & Aliases:** The Claude harness's container-side `provision.py` dynamically resolves the requested model (provided via `--model` / `SCION_MODEL`) using the harness configuration's `model_aliases` mapping:
  - `small` &rarr; `haiku` (or whatever `ANTHROPIC_DEFAULT_HAIKU_MODEL` resolves to)
  - `medium` &rarr; `sonnet`
  - `large` &rarr; `opus`
  - `extra-large` &rarr; `fable`
  The resolved model is set in the environment overlay as `ANTHROPIC_MODEL`. If no model is requested, it falls back to the default model `opus`. Note that setting `ANTHROPIC_MODEL` directly in your settings or a template environment block acts as an explicit, non-overridable pin.
- **Claude Code version guard:** Claude Code versions older than 2.1.280 reject Opus 5.5 (`claude-opus-5-5*`, and the `opus` alias) with a `400 claude_code_version_too_old` error. If the container's `claude` binary is older than 2.1.280 and the resolved model is Opus 5.5, `provision.py` falls back to `claude-opus-4-8` and logs a warning. Rebuild the `scion-claude` image with Claude Code 2.1.280 or later to use Opus 5.5.
- **Auto-updater disabled:** Scion sets `DISABLE_AUTOUPDATER=1` in the container, so Claude Code does not try to update itself in the background. To upgrade Claude Code, rebuild the harness image.

### Effort (Thinking Level)
When `SCION_THINKING_LEVEL` is set (0–100, from `--thinking-level` on `scion start` or Hub agent defaults), the provisioner sets `CLAUDE_CODE_EFFORT_LEVEL` in the environment of the `claude` process. The table comes from the `thinking:` block in the bundle's `config.yaml` (see [Thinking Level Map](/scion/reference/harness-settings/#thinking-level-map-thinking)):

| Thinking Level | `CLAUDE_CODE_EFFORT_LEVEL` |
| :--- | :--- |
| 0–25 | `low` |
| 26–50 | `medium` |
| 51–75 | `high` |
| 76–100 | `xhigh` |

Values outside the 0–100 range are clamped. The top tier is `xhigh` rather than `max` to avoid `max`'s excessive-token runs; `max` is still reachable by setting `CLAUDE_CODE_EFFORT_LEVEL` directly or with a custom `thinking:` table. When a model does not support a level, Claude Code uses the highest level it supports below that one (for example, `xhigh` runs as `high` on Sonnet 4.6). See [Claude Code model configuration](https://code.claude.com/docs/en/model-config).

When the level is unset or blank, the variable is not set, so Claude Code keeps its own per-model default effort. A value that is not an integer (`abc`, `1.5`) also sets nothing, and logs a warning. The variable outranks `--effort`, `/effort` and the `effortLevel` setting in `settings.json`. If `CLAUDE_CODE_EFFORT_LEVEL` is already set in a template or harness-config `env:` block (even to an empty value), the provisioner leaves it alone, and logs a warning if a thinking level was also requested.

### Known Limitations
- Claude Code is a beta tool and its configuration format may change.

---

## 3. OpenCode (`opencode`) [Experimental]

The OpenCode TUI.

### Authentication
OpenCode supports two authentication methods (auto-detected in this order):
- **API Key** (`api-key`): Set `ANTHROPIC_API_KEY` or `OPENAI_API_KEY` in your environment (Anthropic preferred).
- **Auth File** (`auth-file`): Uses `~/.local/share/opencode/auth.json` if available. Scion copies this file from your host when the agent is created.

### Configuration
- **Config File**: `~/.config/opencode/opencode.json`.
- **Environment**: Respects standard OpenCode environment variables.
- **Model Resolution**: Supports model selection via the `SCION_MODEL` environment variable. The provisioning script resolves it with `scion_harness.resolve_model`, which maps a size alias through the harness-config's `model_aliases` to configure the underlying model.
- **Catalog Pre-fetch**: The provisioner automatically pre-fetches the `models.dev` catalog to ensure fresh model data is available before startup.

### Hooks
OpenCode communicates lifecycle events to the Scion Hub via a **hook bridge plugin**. The hook bridge translates OpenCode events into standard Scion lifecycle signals using the `opencode` hook dialect, enabling status reporting, activity tracking, and notification dispatch.

### Known Limitations
- **Auth File Copy**: The `auth.json` file is copied only when the agent is **created**. If you update your host credentials, you may need to manually update the file in the agent or recreate the agent.

---

## 4. Codex (`codex`)

A harness for the OpenAI Codex CLI.

### Authentication
Codex supports two authentication methods (auto-detected in this order):
- **API Key** (`api-key`): Set `CODEX_API_KEY` or `OPENAI_API_KEY` in your environment (Codex-specific key preferred). Scion automatically generates a proper `auth.json` in the agent home for API key workflows.
- **Auth File** (`auth-file`): Uses `~/.codex/auth.json` if available. Scion copies this file from your host when the agent is created.

### Configuration
- **Config File**: `~/.codex/config.toml`.
- **Default Flags**: Runs with `--full-auto` approval mode enabled by default with unified flag formatting.
- **Resume Support**: Automatically uses the `resume` positional argument to continue existing sessions.
- **Notify Bridge**: Scion configures `notify = "sh ~/.codex/scion_notify.sh"` so Codex notify payloads can drive Scion state updates.
- **OpenTelemetry**: When telemetry is enabled, Scion performs telemetry reconciliation at start to ensure consistent OTLP export (default `localhost:4317`).

### Reasoning Effort (Thinking Level)
When `SCION_THINKING_LEVEL` is set (a value from 0–100, provided via `--thinking-level` on `scion start` or via Hub agent defaults), the Codex provisioner maps it to the `model_reasoning_effort` key in `~/.codex/config.toml` using four quartile buckets. The table comes from the `thinking:` block in the bundle's `config.yaml` (see [Thinking Level Map](/scion/reference/harness-settings/#thinking-level-map-thinking)):

| Thinking Level | Reasoning Effort |
| :--- | :--- |
| 0–25 | `low` |
| 26–50 | `medium` |
| 51–75 | `high` |
| 76–100 | `xhigh` |

Values outside the 0–100 range are clamped to the nearest boundary.

When `SCION_THINKING_LEVEL` is unset, blank, or not a valid integer, the provisioner writes `model_reasoning_effort = "medium"` (the block's `default`) rather than leaving the key unwritten. This keeps Codex's own per-model catalog default (which can be `low` for some models) from silently taking over when no one has expressed an explicit preference. A non-integer value also logs a warning.

If a customized Codex `config.yaml` has no `thinking:` block, the provisioner logs a warning and writes no `model_reasoning_effort`, so Codex's own default applies. Copy the block from the bundled `config.yaml`, or run `scion harness-config upgrade codex`, which merges missing top-level keys such as `thinking:` into a customized `config.yaml` without overwriting your values.

### Known Limitations
- **Auth File Copy**: The `auth.json` file is only copied when the agent is **created**.
- **Model selection**: Specific model selection must currently be handled via the `config.toml` or environment variables within the agent.
- **System Prompt Override**: Codex system prompt behavior is unchanged in this iteration; use `agent_instructions` for Scion-managed guidance.

---

## 5. GitHub Copilot CLI (`copilot`)

A harness for GitHub's `copilot` CLI. Opt-in bundle.

### Authentication
Copilot authenticates with a **GitHub token** (auth type `api-key`). Scion resolves the token
from the following environment variables, in order:

1. `COPILOT_GITHUB_TOKEN`
2. `GH_TOKEN`
3. `GITHUB_TOKEN`

The token must be a **fine-grained Personal Access Token** with the "Copilot Requests"
permission; classic (`ghp_...`) tokens are not supported. Scion re-exports the resolved token as
`COPILOT_GITHUB_TOKEN` for the CLI. If no token is found, the agent drops to a shell — run
`copilot login` interactively, then capture the credential with the container's
`capture_auth.py` (see [Harness Authentication](/scion/local/agent-credentials/#capturing-credentials-from-a-running-agent)).

An active GitHub Copilot subscription is required at runtime.

### Configuration
- **Config directory**: `~/.copilot/` (settings in `settings.json`, trusted folders in `config.json`).
- **Instructions**: `agent_instructions` and `system_prompt` are projected into `.github/copilot-instructions.md`. Copilot has no native system-prompt flag, so the system prompt is *prepended to the instructions file*.
- **MCP**: `~/.copilot/mcp-config.json`. Project-scoped MCP servers are not supported (they are demoted to global).
- **Model aliases**: `small` → `claude-haiku-4.5`, `medium` → `claude-sonnet-4.5`, `large` → `claude-opus-4.8`.
- **OpenTelemetry**: When telemetry is enabled, Scion sets `COPILOT_OTEL_ENABLED`, `COPILOT_OTEL_EXPORTER_TYPE=otlp-http`, and standard `OTEL_*` env vars that always point at sciontool's local OTLP/HTTP receiver (port `4318`), so Copilot's logs are redacted and identity-stamped like any other harness's. For local debugging only, `SCION_COPILOT_OTEL_ENDPOINT` overrides the endpoint. It bypasses sciontool's redaction and identity stamping, so never point it at anything but a local collector.

### Known Limitations
- **System Prompt**: approximated via the instructions file (no native override).
- **No hooks**: Copilot exposes no hook dialect.
- **Native metrics on GCP**: Copilot's raw native metrics (such as `gen_ai.client.token.usage`) are rejected by the GCP telemetry provider. Logs are forwarded normally.
- **No project-scoped MCP**.
- **OAuth/Vertex AI**: not supported — Copilot uses GitHub auth only.

---

## 6. Hermes Agent (`hermes`)

A harness for Nous Research's `hermes` agent. Opt-in bundle.

### Authentication
Hermes authenticates with an **LLM provider API key** (auth type `api-key`). Scion selects the
first key present, in this precedence order:

1. `ANTHROPIC_API_KEY`
2. `OPENAI_API_KEY`
3. `GOOGLE_API_KEY` (Google AI Studio, **not** Vertex AI)

The resolved key is written to `~/.hermes/.env` under its original variable name. If no key is
found, the agent drops to a shell — run `hermes setup` interactively, then capture the credential
with `capture_auth.py`.

### Configuration
- **Config directory**: `~/.hermes/` (API key in `.env`).
- **Instructions**: `agent_instructions` and `system_prompt` are projected into `AGENTS.md`. Hermes has no native system-prompt flag, so the system prompt is *prepended to `AGENTS.md`*.
- **MCP**: `~/.hermes/mcp.json`. Project-scoped MCP servers are not supported.
- **Model aliases**: `small` → `google/gemini-3.5-flash`, `medium` → `anthropic/claude-sonnet-4`, `large` → `anthropic/claude-opus-4`.
- **Model Resolution**: Integrates with the `SCION_MODEL` environment variable for fallback model alias resolution. The `provision.py` script resolves it with `scion_harness.resolve_model`, which maps size aliases to the provider/model strings above.

### Known Limitations
- **System Prompt**: approximated via `AGENTS.md` (no native override).
- **No hooks / no OpenTelemetry**: Hermes has a Langfuse integration but no native OTEL, and no Scion hook dialect is wired.
- **No project-scoped MCP**.
- **OAuth/Vertex AI**: not supported — API-key auth only.

---

## 7. Antigravity (`antigravity`)

A harness for Google's Antigravity CLI (the `agy` binary). Opt-in bundle.

:::caution[Not the same as the `antigravity-preview` managed agent]
This `antigravity` **harness** runs the `agy` CLI inside a Scion-provisioned container via
container-script provisioning. It is a **different execution path** from the
`antigravity-preview` *managed-agent base agent* described in
[Managed Agents](/scion/hosted/single-node/managed-agents/), which runs server-side through the
Google Managed Agents (Gemini) API with no container or broker. Choose the harness when you need
a containerized workspace agent; choose the managed agent for repo-less, broker-less tasks.
:::

### Authentication
Antigravity supports three authentication methods, evaluated in priority order (`vertex-ai` > `oauth-token` > `api-key`):

- **Vertex AI** (`vertex-ai`): Google Cloud's Vertex AI mode using Google Cloud Application Default Credentials (ADC) plus `GOOGLE_CLOUD_PROJECT` and `GOOGLE_CLOUD_LOCATION` (or `GOOGLE_CLOUD_REGION`). This mode no longer requires `AGY_TOKEN`. It uses the `gcloud-adc` file secret or automatically resolves ADC via the agent's GCP identity from the metadata server. When no auth file or env credential is present and the agent has a metadata-server service account (for example through the [hub-default GCP identity](/scion/hosted/ha/permissions/#hub-default-gcp-identity)), `vertex-ai` is selected automatically, with no ADC file needed. Requires AGY CLI >= 1.1.10.
- **OAuth token** (`oauth-token`): Provide a JSON file secret named `AGY_TOKEN` containing a `refresh_token`. Scion stages it at `~/.gemini/antigravity-cli/antigravity-oauth-token` and injects it into the container's gnome-keyring at launch.
- **API Key** (`api-key`): The lowest-priority fallback method. Accepts either the `GEMINI_API_KEY` or `GOOGLE_API_KEY` environment secret. The provisioner will automatically set `modelProvider` to `Gemini` in the agent's `settings.json` and authenticate using this key.

If no token is available, run `agy` interactively to log in, then capture the credential with
the Antigravity bundle's `capture_auth.py` (which can also extract the token from gnome-keyring).

### Configuration
- **Config directory**: `~/.gemini/antigravity-cli/`.
- **Instructions**: `agent_instructions` and `system_prompt` are projected into `~/.gemini/GEMINI.md` (system prompt *prepended*).
- **MCP**: `~/.gemini/config/mcp_config.json`.
- **Hooks**: Antigravity ships a hook dialect (`dialect.yaml`) mapping `agy` events to Scion lifecycle events. Hooks fire **project-locally** (wired via `/workspace/.agents/hooks.json`).
- **Runtime**: requires gnome-keyring and D-Bus in the container (provided by the base image); a generated wrapper script bootstraps the keyring and injects the token before launching `agy`.
- **Model selection**: the model is resolved in this order: the agent's model (`--model` / `SCION_MODEL`), then `harness_config.model`, then the operator-set `AGY_MODEL` env var, then the default `Gemini 3.8 Flash (Medium)`. Tier aliases (`small`, `medium`, `large`, `extra-large`) are resolved through the harness's `model_aliases` table. The resolved model is written into `settings.json` on every provision, including into an existing `settings.json`, so changing the model takes effect on the next start.

### Effort (Thinking Level)
When `SCION_THINKING_LEVEL` is set (0–100, from `--thinking-level` on `scion start` or Hub agent defaults), the provisioner adds `--effort <tier>` to the `agy` command in the generated wrapper script (`~/.scion/harness/agy-wrapper.sh`). The table comes from the `thinking:` block in the bundle's `config.yaml` (see [Thinking Level Map](/scion/reference/harness-settings/#thinking-level-map-thinking)):

| Thinking Level | `--effort` |
| :--- | :--- |
| 0–25 | `low` |
| 26–50 | `medium` |
| 51–100 | `high` |

Values outside the 0–100 range are clamped, so `-5` maps to `low` and `150` to `high`. When the level is unset or blank, no `--effort` flag is passed and AGY's own default applies. A value that is not an integer (`abc`, `1.5`) also passes no flag, and logs a warning.

:::note[Changed cut points]
Before ptone/scion#2673 the cut points were 50 and 75 (0–49 `low`, 50–74 `medium`, 75–100 `high`), negative values were silently ignored, and non-integer values were dropped without a warning. Levels 26–49 now map to `medium` and 51–74 to `high`.
:::

### Known Limitations
- **System Prompt**: approximated via `GEMINI.md` (no native override).
- **No OpenTelemetry**: `agy` has no native OTLP export; enterprise mode explicitly disables telemetry.
- **Hooks fire project-locally only**.
- **Runtime dependencies**: requires gnome-keyring/D-Bus and the `jq` tool inside the container.

---

## 8. Grok Build (`grok-build`)

A harness for xAI's `grok` CLI (Grok Build). Opt-in bundle.

### Authentication
Grok Build authenticates with an **xAI API key** (auth type `api-key`). Scion resolves the key
from the `XAI_API_KEY` environment variable.

Alternatively, a file-based auth method (`auth-file`) is supported using `~/.grok/auth.json`,
produced by `grok login --device-auth`. Capture the credential with `capture_auth.py` after login.

A **Vertex AI** auth method (`vertex-ai`) routes inference through Google Cloud's Vertex AI
Model Garden. Set `GOOGLE_CLOUD_PROJECT` and optionally `GOOGLE_CLOUD_REGION` (defaults to the
global endpoint). The provisioner writes `[auth_provider]` and `[model]` entries to
`~/.grok/config.toml` using `gcloud auth print-access-token` for on-demand token refresh.
Application Default Credentials (ADC) are placed automatically when staged.

:::note[Vertex AI API backend]
When using the Vertex AI auth method, the provisioner automatically configures grok to use the
**Chat Completions API backend** (`api_backend = "chat_completions"`) and disables backend search
(`supports_backend_search = false`). This is required because the Vertex AI Model Garden does not
return complete usage metadata under the Responses API, and the `x_search` hosted tool is not
available through Vertex AI.
:::

If no credentials are found, the agent drops to a shell — run `grok login --device-auth`
interactively, then capture the credential with the container's `capture_auth.py`
(see [Harness Authentication](/scion/local/agent-credentials/#capturing-credentials-from-a-running-agent)).

| Mode | Credential | Setup |
|---|---|---|
| API Key | `XAI_API_KEY` | Set env var with xAI API key |
| Auth File | `~/.grok/auth.json` | `grok login --device-auth` + capture |
| Vertex AI | `SCION_METADATA_PROJECT_ID` or `GOOGLE_CLOUD_PROJECT` | Detected from GCP identity or env var |

### Configuration
- **Config directory**: `~/.grok/` (settings in `config.toml`).
- **Instructions**: `agent_instructions` are projected into `~/.grok/AGENTS.md`.
- **System Prompt**: Supported natively via the `--system-prompt-override` flag during launch.
- **MCP**: `~/.grok/config.toml` under `[mcp_servers.*]` TOML sections (supports `stdio`, `sse`, and `streamable-http` transports). Project-scoped MCP servers are not supported (demoted to global).
- **Model aliases**: `small` → `grok-3-mini`, `medium` → `grok-4.5`, `large` → `grok-4.6`, `extra-large` → `grok-4.6` (resolved and injected via `GROK_DEFAULT_MODEL`).
- **Hooks**: 15 Grok lifecycle event hooks are wired to sciontool via `~/.grok/hooks/scion.json` using the `grok-build` dialect, including `PermissionDenied`, `SubagentStart`, `PreCompact`, and `PostCompact`.
- **OpenTelemetry**: When telemetry is enabled, Scion injects `GROK_TELEMETRY_ENABLED`, `GROK_EXTERNAL_OTEL`, and standard `OTEL_*` env vars that always point at sciontool's local OTLP gRPC receiver (port `4317`), never directly at the cloud endpoint. For local debugging only, `SCION_GROK_BUILD_OTEL_ENDPOINT` overrides the endpoint and bypasses sciontool's redaction and identity stamping.

### Known Limitations
- **No max_model_calls** — Grok hooks do not expose model-call start/end events. `max_turns` and `max_duration` are supported.
- **No backend search** — The `x_search` hosted tool is disabled globally via `--disallowed-tools` to ensure compatibility across all auth modes (including Vertex AI, which does not support it).
- **No project-scoped MCP**.
- **OAuth**: not supported — Grok uses xAI auth only.

---

## 9. Muse Code (`muse-code`)

A harness for Meta's `muse` terminal coding agent. Opt-in bundle.

### Authentication
Muse Code authenticates with a **Meta API key** (auth type `api-key`). Scion resolves the key
from the `META_API_KEY` environment variable.

If no credentials are found, the agent drops to a shell — run `muse auth set` interactively,
then capture the credential with the container's `capture_auth.py`
(see [Harness Authentication](/scion/local/agent-credentials/#capturing-credentials-from-a-running-agent)).

| Mode | Credential | Setup |
|---|---|---|
| API Key | `META_API_KEY` | Set env var with Meta API key |

### Configuration
- **Config directory**: `~/.config/muse/` (settings in `settings.json`).
- **Instructions**: `agent_instructions` and `system_prompt` are projected into `AGENTS.md` in the agent home. Muse Code has no native system-prompt flag, so the system prompt is *prepended to `AGENTS.md`*.
- **MCP**: `~/.config/muse/settings.json` under the `mcp_servers` key (supports `stdio` and `streamable-http` transports). Project-scoped MCP servers are not supported (demoted to global).
- **Model aliases**: `small` → `muse-spark-1.2`, `medium` → `muse-spark-1.2`, `large` → `muse-spark-1.2`, `extra-large` → `muse-spark-1.2`.
- **Hooks**: All 13 lifecycle event hooks are wired to sciontool via `settings.json` using the `muse-code` dialect.
- **Skills directory**: `~/.muse/skills`.
- **Default flags**: Runs with `--yolo` (auto-approve) mode enabled by default.

### Known Limitations
- **System Prompt**: approximated via `AGENTS.md` (no native override).
- **Resume**: Partial — resume is an interactive slash command (`/resume --last`), not a CLI flag.
- **No project-scoped MCP**.
- **OAuth/Vertex AI/Auth File**: not supported — Meta API key auth only.

---

## Feature Capability Matrix

The following table summarizes the capabilities supported by each agent harness within Scion.

| Capability | Gemini | Claude | OpenCode | Codex | Copilot | Hermes | Antigravity | Grok Build | Muse Code |
| :--- | :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: |
| **Resume** | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ◐ |
| With Prompt | ✅ | ✅ | ✅ | ❌ | ✅ | ✅ | ✅ | ✅ | ❌ |
| Custom Session ID | ❌ | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| **Interject** | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| Interrupt Key | C-c | C-c | Esc / C-c | C-c | C-c | C-c | C-c | C-c | Esc |
| **Enqueue** | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| **Hooks** | ✅ | ✅ | ✅ | ❌ | ❌ | ❌ | ✅ | ✅ | ✅ |
| Support | ✅ | ✅ | ✅ | ❌ | ❌ | ❌ | ✅ | ✅ | ✅ |
| **OpenTelemetry** | ✅ | ✅  | ❌ | ✅  | ✅ | ❌ | ❌ | ✅ | ❌ |
| **System Prompt Override** | ✅ | ✅ | ❌ | ❌ | ◐ | ◐ | ◐ | ✅ | ◐ |
| **Auth: API Key** | ✅ | ✅ | ✅ | ✅ | ✅¹ | ✅ | ❌ | ✅ | ✅ |
| **Auth: OAuth Token** | ❌ | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| **Auth: Auth File** | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ | ✅² | ✅ | ❌ |
| **Auth: Vertex AI** | ✅ | ✅ | ✅ | ❌ | ❌ | ❌ | ✅ | ✅ | ❌ |

* **Resume with Prompt**: Ability to provide a new task/prompt when resuming an existing session.
* **Interject** (pending feature): Key used to interrupt the agent (e.g., stop generation).
* **Enqueue**: Ability to send messages to the agent while it's running (supported via the built-in Tmux session).
* **Hooks**: Support for lifecycle hooks (e.g., `SessionStart`, `AfterTool`).
* **OpenTelemetry**: Specific events vary by harness and native emitter schema.
* **System Prompt Override**: Support for providing a custom system prompt to the agent (e.g. via `system_prompt.md`). The `gemini-cli` harness has full support via `~/.gemini/system_prompt.md`. ◐ = *partial* — the harness has no native system-prompt flag, so Scion prepends the system prompt to the harness's instructions file: `AGENTS.md` for Hermes and Muse Code, `GEMINI.md` for Antigravity, and `copilot-instructions.md` for Copilot.
* **Auth types**: The universal auth types (`api-key`, `oauth-token`, `auth-file`, `vertex-ai`) each harness accepts. Set an explicit type with `--harness-auth` or `auth_selectedType`; otherwise Scion auto-detects. See [Harness Authentication](/scion/local/agent-credentials/).
    * ¹ **Copilot** authenticates with a **GitHub token** (`COPILOT_GITHUB_TOKEN` / `GH_TOKEN` / `GITHUB_TOKEN`) under the `api-key` type, not an LLM-provider key.
    * ² **Antigravity**'s `oauth-token` default type is a **file-based** OAuth token (`AGY_TOKEN` at `~/.gemini/antigravity-cli/antigravity-oauth-token`), captured under the auth-file capability — it does not accept a raw injected OAuth token the way Claude does.
