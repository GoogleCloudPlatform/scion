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

`antigravity` and `gemini-cli` are installed by default. `opencode`, `codex`, `copilot`, `hermes`,
and `grok-build` are opt-in bundles you add via a [harness-config](/scion/reference/harness-settings/#managing-harness-configs).
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
- **Model Resolution**: Supports model selection via the `SCION_MODEL` environment variable. When `ctx.model_resolution` is empty, the provisioning script automatically falls back to `SCION_MODEL` to resolve and configure the underlying model.
- **Catalog Pre-fetch**: The provisioner automatically pre-fetches the `models.dev` catalog to ensure fresh model data is available before startup.

### Known Limitations
- **Auth File Copy**: The `auth.json` file is copied only when the agent is **created**. If you update your host credentials, you may need to manually update the file in the agent or recreate the agent.
- **No Hook support**: OpenCode does not have analogous hook support, and so will require use of plugin system to notify the scion orchestrator.

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

### Known Limitations
- **System Prompt**: approximated via the instructions file (no native override).
- **No hooks / no OpenTelemetry**: Copilot exposes no hook dialect or telemetry surface.
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
- **Model Resolution**: Integrates with the `SCION_MODEL` environment variable for fallback model alias resolution. When `ctx.model_resolution` is empty, the `provision.py` script falls back to `SCION_MODEL` to map size aliases to the correct Nous Research endpoints.

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

- **Vertex AI** (`vertex-ai`): Google Cloud's Vertex AI mode using Google Cloud Application Default Credentials (ADC) plus `GOOGLE_CLOUD_PROJECT` and `GOOGLE_CLOUD_LOCATION` (or `GOOGLE_CLOUD_REGION`). This mode no longer requires `AGY_TOKEN`. It uses the `gcloud-adc` file secret or automatically resolves ADC via the assigned GCP Service Account (Hub-managed GCP Identity). Requires AGY CLI >= 1.1.10.
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
- **Default model**: `Gemini 3.7 Flash (Medium)` (override via `AGY_MODEL`).

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
- **Model aliases**: `small` → `grok-3-mini`, `medium` → `grok-3`, `large` → `grok-4`, `extra-large` → `grok-4` (resolved and injected via `GROK_DEFAULT_MODEL`).
- **Hooks**: 11 Grok lifecycle event hooks are wired to sciontool via `~/.grok/hooks/scion.json` using the `grok-build` dialect.
- **OpenTelemetry**: When telemetry is enabled, Scion injects `GROK_TELEMETRY_ENABLED`, `GROK_EXTERNAL_OTEL`, and standard `OTEL_*` env vars pointing at sciontool's local OTLP receiver.

### Known Limitations
- **No max_model_calls** — Grok hooks do not expose model-call start/end events. `max_turns` and `max_duration` are supported.
- **No project-scoped MCP**.
- **OAuth**: not supported — Grok uses xAI auth only.

---

## Feature Capability Matrix

The following table summarizes the capabilities supported by each agent harness within Scion.

| Capability | Gemini | Claude | OpenCode | Codex | Copilot | Hermes | Antigravity | Grok Build |
| :--- | :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: |
| **Resume** | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| With Prompt | ✅ | ✅ | ✅ | ❌ | ✅ | ✅ | ✅ | ✅ |
| Custom Session ID | ❌ | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| **Interject** | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| Interrupt Key | C-c | C-c | Esc / C-c | C-c | C-c | C-c | C-c | C-c |
| **Enqueue** | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| **Hooks** | ✅ | ✅ | ❌ | ❌ | ❌ | ❌ | ✅ | ✅ |
| Support | ✅ | ✅ | ❌ | ❌ | ❌ | ❌ | ✅ | ✅ |
| **OpenTelemetry** | ✅ | ✅  | ❌ | ✅  | ❌ | ❌ | ❌ | ✅ |
| **System Prompt Override** | ✅ | ✅ | ❌ | ❌ | ◐ | ◐ | ◐ | ✅ |
| **Auth: API Key** | ✅ | ✅ | ✅ | ✅ | ✅¹ | ✅ | ❌ | ✅ |
| **Auth: OAuth Token** | ❌ | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| **Auth: Auth File** | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ | ✅² | ✅ |
| **Auth: Vertex AI** | ✅ | ✅ | ✅ | ❌ | ❌ | ❌ | ✅ | ✅ |

* **Resume with Prompt**: Ability to provide a new task/prompt when resuming an existing session.
* **Interject** (pending feature): Key used to interrupt the agent (e.g., stop generation).
* **Enqueue**: Ability to send messages to the agent while it's running (supported via the built-in Tmux session).
* **Hooks**: Support for lifecycle hooks (e.g., `SessionStart`, `AfterTool`).
* **OpenTelemetry**: Specific events vary by harness and native emitter schema.
* **System Prompt Override**: Support for providing a custom system prompt to the agent (e.g. via `system_prompt.md`). The `gemini-cli` harness has full support via `~/.gemini/system_prompt.md`. ◐ = *partial* — the harness has no native system-prompt flag, so Scion prepends the system prompt to the harness's instructions file: `AGENTS.md` for Hermes, `GEMINI.md` for Antigravity, and `copilot-instructions.md` for Copilot.
* **Auth types**: The universal auth types (`api-key`, `oauth-token`, `auth-file`, `vertex-ai`) each harness accepts. Set an explicit type with `--harness-auth` or `auth_selectedType`; otherwise Scion auto-detects. See [Harness Authentication](/scion/local/agent-credentials/).
    * ¹ **Copilot** authenticates with a **GitHub token** (`COPILOT_GITHUB_TOKEN` / `GH_TOKEN` / `GITHUB_TOKEN`) under the `api-key` type, not an LLM-provider key.
    * ² **Antigravity**'s `oauth-token` default type is a **file-based** OAuth token (`AGY_TOKEN` at `~/.gemini/antigravity-cli/antigravity-oauth-token`), captured under the auth-file capability — it does not accept a raw injected OAuth token the way Claude does.
