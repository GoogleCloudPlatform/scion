More advanced configuration options for Codex local clients

Use these options when you need more control over providers, policies, and
integrations. For a quick start, see [Config
basics](https://developers.openai.com/codex/config-basic){:.external}.

For background on project guidance, reusable capabilities, custom slash
commands, multi-agent workflows, and integrations, see
[Customization](https://developers.openai.com/codex/concepts/customization){:.external}.
For configuration keys, see [Configuration
Reference](https://developers.openai.com/codex/config-reference){:.external}.

## Profiles {:#profiles}

Profiles let you save named sets of configuration values and switch between them
from the CLI.

Profiles are experimental and may change or be removed in future releases.

Profiles are not currently supported in the Codex IDE extension.

Define profiles under [`profiles.<name>`] in `config.toml`, then run `codex
--profile <name>`:

To make a profile the default, add `profile = "deep-review`" at the top level of
`config.toml`. Codex loads that profile unless you override it on the command
line.

Profiles can also override `model_catalog_json`. When both the top level and the
selected profile set `model_catalog_json`, Codex prefers the profile value.

## One-off overrides from the CLI {:#one-off-overrides}

In addition to editing `~/.codex/config.toml`, you can override configuration
for a single run from the CLI:

- Prefer dedicated flags when they exist (for example, --`model`).
- Use -`c` / --`config` when you need to override an arbitrary key.

Examples:

Notes:

- Keys can use dot notation to set nested values (for example,
  `mcp_servers.context7.enabled=false`).
- --`config` values are parsed as TOML. When in doubt, quote the value so your
  shell doesn't split it on spaces.
- If the value can't be parsed as TOML, Codex treats it as a string.

## Config and state locations {:#config-and}

Codex stores its local state under `CODEX_HOME` (defaults to `~/.codex`).

Common files you may see there:

- `config.toml` (your local configuration)
- `auth.json` (if you use file-based credential storage) or your OS
  keychain/keyring
- `history.jsonl` (if history persistence is enabled)
- Other per-user state such as logs and caches

For authentication details (including credential storage modes), see
[Authentication](https://developers.openai.com/codex/auth){:.external}. For the
full list of configuration keys, see [Configuration
Reference](https://developers.openai.com/codex/config-reference){:.external}.

For shared defaults, rules, and skills checked into repos or system paths, see
[Team
Config](https://developers.openai.com/codex/enterprise/admin-setup#team-config){:.external}.

If you just need to point the built-in OpenAI provider at an LLM proxy, router,
or data-residency enabled project, set environment variable `OPENAI_BASE_URL`
instead of defining a new provider. This overrides the default OpenAI endpoint
without a `config.toml` change.

## Project config files (.codex/config.toml) {:#project-config}

In addition to your user config, Codex reads project-scoped overrides from
.`codex/config.toml` files inside your repo. Codex walks from the project root
to your current working directory and loads every .`codex/config.toml` it finds.
If multiple files define the same key, the closest file to your working
directory wins.

For security, Codex loads project-scoped config files only when the project is
trusted. If the project is untrusted, Codex ignores .`codex/config.toml` files
in the project.

Relative paths inside a project config (for example,
`experimental_instructions_file`) are resolved relative to the .`codex/` folder
that contains the `config.toml`.

## Agent roles ([agents] in config.toml) {:#agent-roles}

For multi-agent role configuration ([`agents`] in `config.toml`), see
[Multi-agents](https://developers.openai.com/codex/multi-agent){:.external}.

## Project root detection {:#project-root}

Codex discovers project configuration (for example, .`codex/` layers and
`AGENTS.md`) by walking up from the working directory until it reaches a project
root.

By default, Codex treats a directory containing .`git` as the project root. To
customize this behavior, set `project_root_markers` in `config.toml`:

Set `project_root_markers =` [] to skip searching parent directories and treat
the current working directory as the project root.

## Custom model providers {:#custom-model}

A model provider defines how Codex connects to a model (base URL, wire API, and
optional HTTP headers).

Define additional providers and point `model_provider` at them:

Add request headers when needed:

## OSS mode (local providers) {:#oss-mode}

Codex can run against a local "open source" provider (for example, Ollama or LM
Studio) when you pass --`oss`. If you pass --`oss` without specifying a
provider, Codex uses `oss_provider` as the default.

## Azure provider and per-provider tuning {:#azure-provider}

## ChatGPT customers using data residency {:#chatgpt-customers}

Projects created with [data
residency](https://help.openai.com/en/articles/9903489-data-residency-and-inference-residency-for-chatgpt){:.external}
enabled can create a model provider to update the base_url with the [correct
prefix](https://platform.openai.com/docs/guides/your-data#which-models-and-features-are-eligible-for-data-residency){:.external}.

## Model reasoning, verbosity, and limits {:#model-reasoning,}

`model_verbosity` applies only to providers using the Responses API. Chat
Completions providers will ignore the setting.

## Approval policies and sandbox modes {:#approval-policies}

Pick approval strictness (affects when Codex pauses) and sandbox level (affects
file/network access).

For operational details that are easy to miss while editing `config.toml`, see
[Common sandbox and approval
combinations](https://developers.openai.com/codex/agent-approvals-security#common-sandbox-and-approval-combinations){:.external},
[Protected paths in writable
roots](https://developers.openai.com/codex/agent-approvals-security#protected-paths-in-writable-roots){:.external},
and [Network
access](https://developers.openai.com/codex/agent-approvals-security#network-access){:.external}.

You can also use a granular reject policy (`approval_policy = { reject = { ...
`} }) to auto-reject only selected prompt categories, such as sandbox approvals,
`execpolicy` rule prompts, or MCP input requests (`mcp_elicitations`), while
keeping other prompts interactive.

Need the complete key list (including profile-scoped overrides and requirements
constraints)? See [Configuration
Reference](https://developers.openai.com/codex/config-reference){:.external} and
[Managed
configuration](https://developers.openai.com/codex/enterprise/managed-configuration){:.external}.

In workspace-write mode, some environments keep .`git/` and .`codex/` read-only
even when the rest of the workspace is writable. This is why commands like `git
commit` may still require approval to run outside the sandbox. If you want Codex
to skip specific commands (for example, block `git commit` outside the sandbox),
use [rules](https://developers.openai.com/codex/rules){:.external}.

Disable sandboxing entirely (use only if your environment already isolates
processes):

## Shell environment policy {:#shell-environment}

`shell_environment_policy` controls which environment variables Codex passes to
any subprocess it launches (for example, when running a tool-command the model
proposes). Start from a clean start (`inherit = "none`") or a trimmed set
(`inherit = "core`"), then layer on excludes, includes, and overrides to avoid
leaking secrets while still providing the paths, keys, or flags your tasks need.

Patterns are case-insensitive globs (`*, ?, [A-Z]); ignore_default_excludes =
false` keeps the automatic KEY/SECRET/TOKEN filter before your includes/excludes
run.

## MCP servers {:#mcp-servers}

See the dedicated [MCP
documentation](https://developers.openai.com/codex/mcp){:.external} for
configuration details.

## Observability and telemetry {:#observability-and}

Enable OpenTelemetry (OTel) log export to track Codex runs (API requests,
SSE/events, prompts, tool approvals/results). Disabled by default; opt in via
[`otel`]:

Choose an exporter:

If `exporter = "none`" Codex records events but sends nothing. Exporters batch
asynchronously and flush on shutdown. Event metadata includes service name, CLI
version, env tag, conversation id, model, sandbox/approval settings, and
per-event fields (see [Config
Reference](https://developers.openai.com/codex/config-reference){:.external}).

### What gets emitted {:#gets-emitted}

Codex emits structured log events for runs and tool usage. Representative event
types include:

- `codex.conversation_starts` (model, reasoning settings, sandbox/approval
  policy)
- `codex.api_request` (attempt, status/success, duration, and error details)
- `codex.sse_event` (stream event kind, success/failure, duration, plus token
  counts on `response.completed`)
- `codex.websocket_request` and `codex.websocket_event` (request duration plus
  per-message kind/success/error)
- `codex.user_prompt` (length; content redacted unless explicitly enabled)
- `codex.tool_decision` (approved/denied and whether the decision came from
  config vs user)
- `codex.tool_result` (duration, success, output snippet)

### OTel metrics emitted {:#otel-metrics}

When the OTel metrics pipeline is enabled, Codex emits counters and duration
histograms for API, stream, and tool activity.

Each metric below also includes default metadata tags: `auth_mode, originator,
session_source, model`, and `app.version`.

<table>
  <tr>
    <td><p><strong>Metric</strong></p></td>
    <td><p><strong>Type</strong></p></td>
    <td><p><strong>Fields</strong></p></td>
    <td><p><strong>Description</strong></p></td>
  </tr>
  <tr>
    <td><p><code>codex.api_request</code></p></td>
    <td><p>counter</p></td>
    <td><p><code>status, success</code></p></td>
    <td><p>API request count by HTTP status and success/failure.</p></td>
  </tr>
  <tr>
    <td><p><code>codex.api_request.duration_ms</code></p></td>
    <td><p>histogram</p></td>
    <td><p><code>status, success</code></p></td>
    <td><p>API request duration in milliseconds.</p></td>
  </tr>
  <tr>
    <td><p><code>codex.sse_event</code></p></td>
    <td><p>counter</p></td>
    <td><p><code>kind, success</code></p></td>
    <td><p>SSE event count by event kind and success/failure.</p></td>
  </tr>
  <tr>
    <td><p><code>codex.sse_event.duration_ms</code></p></td>
    <td><p>histogram</p></td>
    <td><p><code>kind, success</code></p></td>
    <td><p>SSE event processing duration in milliseconds.</p></td>
  </tr>
  <tr>
    <td><p><code>codex.websocket.request</code></p></td>
    <td><p>counter</p></td>
    <td><p><code>success</code></p></td>
    <td><p>WebSocket request count by success/failure.</p></td>
  </tr>
  <tr>
    <td><p><code>codex.websocket.request.duration_ms</code></p></td>
    <td><p>histogram</p></td>
    <td><p><code>success</code></p></td>
    <td><p>WebSocket request duration in milliseconds.</p></td>
  </tr>
  <tr>
    <td><p><code>codex.websocket.event</code></p></td>
    <td><p>counter</p></td>
    <td><p><code>kind, success</code></p></td>
    <td><p>WebSocket message/event count by type and success/failure.</p></td>
  </tr>
  <tr>
    <td><p><code>codex.websocket.event.duration_ms</code></p></td>
    <td><p>histogram</p></td>
    <td><p><code>kind, success</code></p></td>
    <td><p>WebSocket message/event processing duration in milliseconds.</p></td>
  </tr>
  <tr>
    <td><p><code>codex.tool.call</code></p></td>
    <td><p>counter</p></td>
    <td><p><code>tool, success</code></p></td>
    <td><p>Tool invocation count by tool name and success/failure.</p></td>
  </tr>
  <tr>
    <td><p><code>codex.tool.call.duration_ms</code></p></td>
    <td><p>histogram</p></td>
    <td><p><code>tool, success</code></p></td>
    <td><p>Tool execution duration in milliseconds by tool name and outcome.</p></td>
  </tr>
</table>

For more security and privacy guidance around telemetry, see
[Security](https://developers.openai.com/codex/agent-approvals-security#monitoring-and-telemetry){:.external}.

### Metrics {:#metrics}

By default, Codex periodically sends a small amount of anonymous usage and
health data back to OpenAI. This helps detect when Codex isn't working correctly
and shows what features and configuration options are being used, so the Codex
team can focus on what matters most. These metrics don't contain any personally
identifiable information (PII). Metrics collection is independent of OTel
log/trace export.

If you want to disable metrics collection entirely across Codex surfaces on a
machine, set the analytics flag in your config:

Each metric includes its own fields plus the default context fields below.

#### Default context fields (applies to every event/metric)

- `auth_mode: swic` | `api` | `unknown`.
- `model`: name of the model used.
- `app.version`: Codex version.

#### Metrics catalog

Each metric includes the required fields plus the default context fields above.
Every metric is prefixed by `codex`.. If a metric includes the `tool` field, it
reflects the internal tool used (for example, `apply_patch` or `shell`) and
doesn't contain the actual shell command or patch `codex` is trying to apply.

<table>
  <tr>
    <td><p><strong>Metric</strong></p></td>
    <td><p><strong>Type</strong></p></td>
    <td><p><strong>Fields</strong></p></td>
    <td><p><strong>Description</strong></p></td>
  </tr>
  <tr>
    <td><p><code>feature.state</code></p></td>
    <td><p>counter</p></td>
    <td><p><code>feature, value</code></p></td>
    <td><p>Feature values that differ from defaults (emit one row per non-default).</p></td>
  </tr>
  <tr>
    <td><p><code>thread.started</code></p></td>
    <td><p>counter</p></td>
    <td><p><code>is_git</code></p></td>
    <td><p>New thread created.</p></td>
  </tr>
  <tr>
    <td><p><code>thread.fork</code></p></td>
    <td><p>counter</p></td>
    <td></td>
    <td><p>New thread created by forking an existing thread.</p></td>
  </tr>
  <tr>
    <td><p><code>thread.rename</code></p></td>
    <td><p>counter</p></td>
    <td></td>
    <td><p>Thread renamed.</p></td>
  </tr>
  <tr>
    <td><p><code>task.compact</code></p></td>
    <td><p>counter</p></td>
    <td><p><code>type</code></p></td>
    <td><p>Number of compactions per type (<code>remote</code> or <code>local</code>), including manual and auto.</p></td>
  </tr>
  <tr>
    <td><p><code>task.user_shell</code></p></td>
    <td><p>counter</p></td>
    <td></td>
    <td><p>Number of user shell actions (<code>!</code> in the TUI for example).</p></td>
  </tr>
  <tr>
    <td><p><code>task.review</code></p></td>
    <td><p>counter</p></td>
    <td></td>
    <td><p>Number of reviews triggered.</p></td>
  </tr>
  <tr>
    <td><p><code>task.undo</code></p></td>
    <td><p>counter</p></td>
    <td></td>
    <td><p>Number of undo actions triggered.</p></td>
  </tr>
  <tr>
    <td><p><code>approval.requested</code></p></td>
    <td><p>counter</p></td>
    <td><p><code>tool, approved</code></p></td>
    <td><p>Tool approval request result (<code>approved, approved_with_amendment, approved_for_session, denied, abort</code>).</p></td>
  </tr>
  <tr>
    <td><p><code>conversation.turn.count</code></p></td>
    <td><p>counter</p></td>
    <td></td>
    <td><p>User/assistant turns per thread, recorded at the end of the thread.</p></td>
  </tr>
  <tr>
    <td><p><code>turn.e2e_duration_ms</code></p></td>
    <td><p>histogram</p></td>
    <td></td>
    <td><p>End-to-end time for a full turn.</p></td>
  </tr>
  <tr>
    <td><p><code>mcp.call</code></p></td>
    <td><p>counter</p></td>
    <td><p><code>status</code></p></td>
    <td><p>MCP tool invocation result (<code>ok</code> or error string).</p></td>
  </tr>
  <tr>
    <td><p><code>model_warning</code></p></td>
    <td><p>counter</p></td>
    <td></td>
    <td><p>Warning sent to the model.</p></td>
  </tr>
  <tr>
    <td><p><code>tool.call</code></p></td>
    <td><p>counter</p></td>
    <td><p><code>tool, success</code></p></td>
    <td><p>Tool invocation result (<code>success: true</code> or <code>false</code>).</p></td>
  </tr>
  <tr>
    <td><p><code>tool.call.duration_ms</code></p></td>
    <td><p>histogram</p></td>
    <td><p><code>tool, success</code></p></td>
    <td><p>Tool execution time.</p></td>
  </tr>
  <tr>
    <td><p><code>remote_models.fetch_update.duration_ms</code></p></td>
    <td><p>histogram</p></td>
    <td></td>
    <td><p>Time to fetch remote model definitions.</p></td>
  </tr>
  <tr>
    <td><p><code>remote_models.load_cache.duration_ms</code></p></td>
    <td><p>histogram</p></td>
    <td></td>
    <td><p>Time to load the remote model cache.</p></td>
  </tr>
  <tr>
    <td><p><code>shell_snapshot</code></p></td>
    <td><p>counter</p></td>
    <td><p><code>success</code></p></td>
    <td><p>Whether taking a shell snapshot succeeded.</p></td>
  </tr>
  <tr>
    <td><p><code>shell_snapshot.duration_ms</code></p></td>
    <td><p>histogram</p></td>
    <td><p><code>success</code></p></td>
    <td><p>Time to take a shell snapshot.</p></td>
  </tr>
  <tr>
    <td><p><code>db.init</code></p></td>
    <td><p>counter</p></td>
    <td><p><code>status</code></p></td>
    <td><p>State DB initialization outcomes (<code>opened, created, open_error, init_error</code>).</p></td>
  </tr>
  <tr>
    <td><p><code>db.backfill</code></p></td>
    <td><p>counter</p></td>
    <td><p><code>status</code></p></td>
    <td><p>Initial state DB backfill results (<code>upserted, failed</code>).</p></td>
  </tr>
  <tr>
    <td><p><code>db.backfill.duration_ms</code></p></td>
    <td><p>histogram</p></td>
    <td><p><code>status</code></p></td>
    <td><p>Duration of the initial state DB backfill, tagged with <code>success, failed</code>, or <code>partial_failure</code>.</p></td>
  </tr>
  <tr>
    <td><p><code>db.error</code></p></td>
    <td><p>counter</p></td>
    <td><p><code>stage</code></p></td>
    <td><p>Errors during state DB operations (for example, <code>extract_metadata_from_rollout, backfill_sessions, apply_rollout_items</code>).</p></td>
  </tr>
  <tr>
    <td><p><code>db.compare_error</code></p></td>
    <td><p>counter</p></td>
    <td><p><code>stage, reason</code></p></td>
    <td><p>State DB discrepancies detected during reconciliation.</p></td>
  </tr>
</table>

### Feedback controls {:#feedback-controls}

By default, Codex lets users send feedback from `/feedback`. To disable feedback
collection across Codex surfaces on a machine, update your config:

When disabled, `/feedback` shows a disabled message and Codex rejects feedback
submissions.

### Hide or surface reasoning events {:#hide-or}

If you want to reduce noisy "reasoning" output (for example in CI logs), you can
suppress it:

If you want to surface raw reasoning content when a model emits it:

Enable raw reasoning only if it's acceptable for your workflow. Some
models/providers (like `gpt-oss`) don't emit raw reasoning; in that case, this
setting has no visible effect.

## Notifications {:#notifications}

Use `notify` to trigger an external program whenever Codex emits supported
events (currently only `agent-turn-complete`). This is handy for desktop toasts,
chat webhooks, CI updates, or any side-channel alerting that the built-in TUI
notifications don't cover.

Example `notify.py` (truncated) that reacts to `agent-turn-complete`:

The script receives a single JSON argument. Common fields include:

- `type` (currently `agent-turn-complete`)
- `thread-id` (session identifier)
- `turn-id` (turn identifier)
- `cwd` (working directory)
- `input-messages` (user messages that led to the turn)
- `last-assistant-message` (last assistant message text)

Place the script somewhere on disk and point `notify` to it.

#### notify vs tui.notifications

- `notify` runs an external program (good for webhooks, desktop notifiers, CI
  hooks).
- `tui.notifications` is built in to the TUI and can optionally filter by event
  type (for example, `agent-turn-complete` and `approval-requested`).
- `tui.notification_method` controls how the TUI emits terminal notifications
  (`auto, osc9`, or `bel`).

In `auto` mode, Codex prefers OSC 9 notifications (a terminal escape sequence
some terminals interpret as a desktop notification) and falls back to BEL
(`\x07`) otherwise.

See [Configuration
Reference](https://developers.openai.com/codex/config-reference){:.external} for
the exact keys.

## History persistence {:#history-persistence}

By default, Codex saves local session transcripts under `CODEX_HOME` (for
example, `~/.codex/history.jsonl`). To disable local history persistence:

To cap the history file size, set `history.max_bytes`. When the file exceeds the
cap, Codex drops the oldest entries and compacts the file while keeping the
newest records.

## Clickable citations {:#clickable-citations}

If you use a terminal/editor integration that supports it, Codex can render file
citations as clickable links. Configure `file_opener` to pick the URI scheme
Codex uses:

Example: a citation like `/home/user/project/main.py:42` can be rewritten into a
clickable `vscode://file/...:42` link.

## Project instructions discovery {:#project-instructions}

Codex reads `AGENTS.md` (and related files) and includes a limited amount of
project guidance in the first turn of a session. Two knobs control how this
works:

- `project_doc_max_bytes`: how much to read from each `AGENTS.md` file
- `project_doc_fallback_filenames`: additional filenames to try when `AGENTS.md`
  is missing at a directory level

For a detailed walkthrough, see [Custom instructions with
AGENTS.md](https://developers.openai.com/codex/guides/agents-md){:.external}.

## TUI options {:#tui-options}

Running `codex` with no subcommand launches the interactive terminal UI (TUI).
Codex exposes some TUI-specific configuration under [`tui`], including:

- `tui.notifications`: enable/disable notifications (or restrict to specific
  types)
- `tui.notification_method`: choose `auto, osc9`, or `bel` for terminal
  notifications
- `tui.animations`: enable/disable ASCII animations and shimmer effects
- `tui.alternate_screen`: control alternate screen usage (set to `never` to keep
  terminal scrollback)
- `tui.show_tooltips`: show or hide onboarding tooltips on the welcome screen

`tui.notification_method` defaults to `auto`. In `auto` mode, Codex prefers OSC 9
notifications (a terminal escape sequence some terminals interpret as a desktop
notification) when the terminal appears to support them, and falls back to BEL
(`\x07`) otherwise.

See [Configuration
Reference](https://developers.openai.com/codex/config-reference){:.external} for
the full key list.

