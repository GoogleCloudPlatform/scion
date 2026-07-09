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

`claude` and `gemini` are installed by default. `opencode`, `codex`, `copilot`, `hermes`, and
`antigravity` are opt-in bundles you add via a [harness-config](/scion/reference/harness-settings/#managing-harness-configs).
:::

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
Antigravity uses **OAuth** (auth type `oauth-token`), with an optional **Vertex AI**
(`vertex-ai`) mode for enterprise/GCP deployments. It does not use API keys.

- **OAuth token** (`oauth-token`): provide a JSON file secret named `AGY_TOKEN` containing a `refresh_token`. Scion stages it at `~/.gemini/antigravity-cli/antigravity-oauth-token` and injects it into the container's gnome-keyring at launch.
- **Vertex AI** (`vertex-ai`): requires `AGY_TOKEN` plus `GOOGLE_CLOUD_PROJECT` and `GOOGLE_CLOUD_LOCATION` (or `GOOGLE_CLOUD_REGION`). Tried before OAuth when configured.

If no token is available, run `agy` interactively to log in, then capture the credential with
the Antigravity bundle's `capture_auth.py` (which can also extract the token from gnome-keyring).

### Configuration
- **Config directory**: `~/.gemini/antigravity-cli/`.
- **Instructions**: `agent_instructions` and `system_prompt` are projected into `~/.gemini/GEMINI.md` (system prompt *prepended*).
- **MCP**: `~/.gemini/config/mcp_config.json`.
- **Hooks**: Antigravity ships a hook dialect (`dialect.yaml`) mapping `agy` events to Scion lifecycle events. Hooks fire **project-locally** (wired via `/workspace/.agents/hooks.json`).
- **Runtime**: requires gnome-keyring and D-Bus in the container (provided by the base image); a generated wrapper script bootstraps the keyring and injects the token before launching `agy`.
- **Default model**: `Gemini 3.5 Flash` (override via `AGY_MODEL`).

### Known Limitations
- **System Prompt**: approximated via `GEMINI.md` (no native override).
- **No OpenTelemetry**: `agy` has no native OTLP export; enterprise mode explicitly disables telemetry.
- **Hooks fire project-locally only**.
- **Runtime dependencies**: requires gnome-keyring/D-Bus and the `jq` tool inside the container.

---

## Feature Capability Matrix

The following table summarizes the capabilities supported by each agent harness within Scion.

| Capability | Gemini | Claude | OpenCode | Codex | Copilot | Hermes | Antigravity |
| :--- | :---: | :---: | :---: | :---: | :---: | :---: | :---: |
| **Resume** | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| With Prompt | ✅ | ✅ | ✅ | ❌ | ✅ | ✅ | ✅ |
| Custom Session ID | ❌ | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ |
| **Interject** | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| Interrupt Key | C-c | C-c | Esc / C-c | C-c | C-c | C-c | C-c |
| **Enqueue** | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| **Hooks** | ✅ | ✅ | ❌ | ❌ | ❌ | ❌ | ✅ |
| Support | ✅ | ✅ | ❌ | ❌ | ❌ | ❌ | ✅ |
| **OpenTelemetry** | ✅ | ✅  | ❌ | ✅  | ❌ | ❌ | ❌ |
| **System Prompt Override** | ✅ | ✅ | ❌ | ❌ | ◐ | ◐ | ◐ |

* **Resume with Prompt**: Ability to provide a new task/prompt when resuming an existing session.
* **Interject** (pending feature): Key used to interrupt the agent (e.g., stop generation).
* **Enqueue**: Ability to send messages to the agent while it's running (supported via the built-in Tmux session).
* **Hooks**: Support for lifecycle hooks (e.g., `SessionStart`, `AfterTool`).
* **OpenTelemetry**: Specific events vary by harness and native emitter schema.
* **System Prompt Override**: Support for providing a custom system prompt to the agent (e.g. via `system_prompt.md`). The `gemini` harness has full support via `~/.gemini/system_prompt.md`. ◐ = *partial* — the harness has no native system-prompt flag, so Scion prepends the system prompt to the harness's instructions file: `AGENTS.md` for Hermes, `GEMINI.md` for Antigravity, and `copilot-instructions.md` for Copilot.
