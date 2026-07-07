---
title: Supported Agent Harnesses
---

Scion supports multiple LLM agent "harnesses". A harness is an adapter that allows Scion to manage the lifecycle, authentication, and configuration of a specific agent tool.

## 1. Gemini CLI (`gemini`)

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
- **System Prompt**: `~/.gemini/system_prompt.md` is automatically seeded if `system_prompt` is provided in the agent config.

### Known Limitations
- The `gemini` CLI tool must be installed in the container image (included in default images).

---

## 2. Claude Code (`claude`)

A harness for Anthropic's "Claude Code" agent.

### Authentication
Claude supports two authentication methods (auto-detected in this order):
- **API Key** (`api-key`): Set `ANTHROPIC_API_KEY` in your host environment. Scion propagates this to the agent and pre-approves it in `.claude.json` so Claude Code does not prompt for confirmation.
- **Vertex AI** (`vertex-ai`): Uses Google Cloud's Vertex AI endpoint with ADC, `GOOGLE_CLOUD_PROJECT`, and `GOOGLE_CLOUD_REGION`.

Auth type can be explicitly set via `auth_selectedType` in your Scion settings profile. See [Agent Credentials](/scion/local/agent-credentials/) for details.

### Configuration
- **scion-agent.yaml**: Can be configured via `agent_instructions` and `system_prompt` fields in the template.
- **Config File**: `~/.claude.json`. Scion manages project-specific settings in this file to ensure the agent respects the workspace boundaries.
- **Projects**: Scion automatically configures the current workspace as a project in `.claude.json`.

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

## Feature Capability Matrix

The following table summarizes the capabilities supported by each agent harness within Scion.

| Capability | Gemini | Claude | OpenCode | Codex |
| :--- | :---: | :---: | :---: | :---: |
| **Resume** | ✅ | ✅ | ✅ | ✅ |
| With Prompt | ✅ | ✅ | ✅ | ❌ |
| Custom Session ID | ❌ | ✅ | ❌ | ❌ |
| **Interject** | ✅ | ✅ | ✅ | ✅ |
| Interupt Key | C-c | C-c | Esc / C-c | C-c |
| **Enqueue** | ✅ | ✅ | ✅ | ✅ |
| **Hooks** | ✅ | ✅ | ❌ | ❌ |
| Support | ✅ | ✅ | ❌ | ❌ |
| **OpenTelemetry** | ✅ | ✅  | ❌ | ✅  |
| **System Prompt Override** | ✅ | ✅ | ❌ | ❌ |

* **Resume with Prompt**: Ability to provide a new task/prompt when resuming an existing session.
* **Interject** (pending feature): Key used to interrupt the agent (e.g., stop generation).
* **Enqueue**: Ability to send messages to the agent while it's running (supported via the built-in Tmux session).
* **Hooks**: Support for lifecycle hooks (e.g., `SessionStart`, `AfterTool`).
* **OpenTelemetry**: Specific events vary by harness and native emitter schema.
* **System Prompt Override**: Support for providing a custom system prompt to the agent (e.g. via `system_prompt.md`).
