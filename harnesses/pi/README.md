# pi Harness Bundle

Scion harness configuration for [pi](https://pi.dev), a multi-provider terminal
coding agent (npm package `@earendil-works/pi-coding-agent`, pinned to 1.3.0).

> Status: the bundle is covered by unit tests for the provisioner and the
> bridge extension. It has not been exercised against a live pi under Scion;
> see [Known limits](#known-limits).

## Install

From a repository checkout:

```sh
scion harness-config install harnesses/pi
```

## Auth Modes

| Mode | Env / File | Notes |
|------|-----------|-------|
| `api-key` (default) | `ANTHROPIC_API_KEY`, or any other pi provider key variable listed in `config.yaml` | Anthropic is listed first |
| `auth-file` | `~/.pi/agent/auth.json` (secret `PI_AUTH`) | Written 0600 and **never overwritten** if present, because pi refreshes OAuth tokens in place |

With no credentials the agent starts a shell with instructions (`no_auth`);
run `pi` and use `/login` interactively.

## Configuration

Everything has a safe default. `provision.py` reads the first four env
variables below at every start and writes `~/.pi/agent/settings.json`; the
bridge extension reads `SCION_PI_NO_AUTO_COMPLETE` and `SCION_HOOK_DEBUG`
(and also `SCION_PI_PROJECT_TRUST`) from pi's environment at runtime, so
`settings.json` cannot set those two.
Other pi settings can also be edited in `settings.json` directly, except the
keys provisioning overwrites at every start: `defaultProjectTrust` (always,
from `SCION_PI_PROJECT_TRUST`), `defaultProvider` and `defaultModel` (when
`SCION_PI_PROVIDER` or a concrete `SCION_MODEL` is set), and any key set in
`SCION_PI_SETTINGS`.

| Setting | Env var | Default | Notes |
|---------|---------|---------|-------|
| Project trust | `SCION_PI_PROJECT_TRUST` = `never` \| `ask` \| `always` | `never` | Wins over `SCION_PI_SETTINGS` and over a value left in `settings.json`; recomputed on every start (unset means `never`). Invalid values fail provisioning |
| Provider | `SCION_PI_PROVIDER` | unset | Sets `defaultProvider` in `settings.json`, over a provider taken from `SCION_MODEL`. A `--model provider/id` names its own provider and overrides it; with no `--model`, `defaultProvider` applies. How pi treats a provider-less `--model` value (a fuzzy ID match) is pi's own resolution; this bundle does not pin it |
| Model | `SCION_MODEL` | unset | A `provider/id[:thinking]` value sets `defaultModel` to `id` (a trailing pi thinking-level suffix, `:off` to `:max`, is not written; it takes effect through `--model`. Any other colon is part of the ID and is kept, e.g. `ollama/qwen3:8b`) and `defaultProvider` to `provider` unless `SCION_PI_PROVIDER` is set. Scion resolves size tiers (`small`/`medium`/`large`/`extra-large`) through `model_aliases` in `config.yaml` before the agent starts, so the resolved pattern (e.g. `anthropic/sonnet`) is written the same way. Scion also starts pi with `--model <resolved>` whenever the agent has an effective model: the Hub-resolved model, a `SCION_MODEL` in the start request or in the settings harness-config env, or the configuration's model (`resolveEffectiveModel` in pkg/agent/run.go). pi uses that instead of `defaultModel`. When pi starts without `--model` (no effective model, for example a `SCION_MODEL` set only in a harness-config directory's `config.yaml` `env:`, which the effective model does not read), `defaultProvider`/`defaultModel` decide the model. A template cannot set `SCION_MODEL`: `SCION_*` keys in a template's `env:` are ignored with a warning |
| Other pi settings | `SCION_PI_SETTINGS` (JSON object) | unset | Deep-merged into `settings.json`. Keys that look like credentials are rejected; it cannot change auth handling |
| Auto task completion | `SCION_PI_NO_AUTO_COMPLETE=1` | off | Bridge stops signalling `task_completed` on settle |
| Bridge debug | `SCION_HOOK_DEBUG=1` | off | Logs bridge failures to stderr |

Existing user values in `~/.pi/agent/settings.json` are preserved unless an
env setting writes the same key: `defaultProjectTrust` always;
`defaultProvider`/`defaultModel` when `SCION_PI_PROVIDER` or a concrete
`SCION_MODEL` is set; and any key in `SCION_PI_SETTINGS`. The bundle's own
defaults (`quietStartup`, `enableInstallTelemetry`) are only added when
missing. A second provisioning run with the same environment changes
nothing.

Scion's `system_prompt` is written to `~/.pi/agent/APPEND_SYSTEM.md`, which pi
appends to its default system prompt (it does not replace it; pi's separate
`SYSTEM.md` replacement is not used). `agent_instructions` go to
`~/.pi/agent/AGENTS.md`.

### Project trust

pi can load settings, MCP servers, extensions, skills, prompts, themes and
system-prompt files from a repository's `.pi/` directory. Those run with the
agent's privileges, so the default is `never`. Set
`SCION_PI_PROJECT_TRUST=always` only for repositories you control.

What the policy does and does not cover (pi v1.3.0,
[security.md](https://github.com/earendil-works/pi/blob/v1.3.0/packages/coding-agent/docs/security.md)):

- pi decides in this order: command-line `--approve`/`--no-approve`; the
  `project_trust` event of a user-level extension; a saved decision in
  `~/.pi/agent/trust.json` (closest directory wins); then `defaultProjectTrust`.
- Scion enforces the policy above the saved decisions: the bridge extension
  answers `project_trust` from `SCION_PI_PROJECT_TRUST` (`never` = no,
  `always` = yes, `ask` = leave to pi), so a saved `/trust` decision cannot
  override `never`. `provision.py` also writes `defaultProjectTrust` on every
  start from the env alone, so removing the env variable returns to `never` and
  `SCION_PI_SETTINGS` cannot change it (it is dropped with a warning).
- With `never`, protected project resources (`.pi/settings.json`,
  `.pi/mcp.json`, `.pi/extensions|skills|prompts|themes`, `.pi/SYSTEM.md`,
  `.pi/APPEND_SYSTEM.md`, project `.agents/skills`) are skipped. The user-level
  agent directory (`~/.pi/agent`) is unaffected.
- Not covered: `AGENTS.md`/`CLAUDE.md` context files load regardless of trust
  and are untrusted input; the project `sessionDir` setting is read before
  trust is resolved; trust does not restrict what tools can do. pi has no
  sandbox: the container is the security boundary.
- With `SCION_PI_PROJECT_TRUST=always`, a repository's `.pi/APPEND_SYSTEM.md`
  takes precedence over the agent-level file, so the repository can override
  Scion's system prompt.
- A direct `pi --approve` on the command line overrides everything.
- `SCION_PI_SETTINGS` is an operator-controlled surface: it can set
  code-loading keys (`packages`, `extensions`, `shellCommandPrefix`,
  `npmCommand`) in the user-level settings. Its credential-name filter
  (`apiKey`, `token`, `secret`, ...) is best-effort, not a guarantee.
- `~/.pi/agent/mcp.json` may carry `env`/`headers` secrets from Scion's MCP
  config; it is written `0600`. Servers removed from Scion are not pruned.

## Lifecycle signals

`home/.pi/agent/extensions/scion-bridge.js` is a user-level pi extension.

| pi event | Scion event |
|----------|-------------|
| `session_start` | `session-start` |
| `input` (human/RPC) | `prompt-submit` |
| `tool_execution_start` / `_end` | `tool-start` / `tool-end` |
| `agent_end` | `agent-end` (carries a provider error) |
| `agent_settled`, not aborted, no error, and the agent has not run `sciontool status` `ask_user`/`blocked`/`limits_exceeded`/`task_completed` itself since the last human/RPC input | `sciontool status task_completed -- <summary>` |

`ask_user` and `blocked` are not pi events. Instruct the agent to run
`sciontool status ask_user "<question>"` or `sciontool status blocked "<reason>"`
through its bash tool; the bridge sees that command on `tool_execution_start`
and skips the automatic `task_completed` until the next human/RPC input.
The same applies when the agent runs `sciontool status task_completed "<summary>"`
itself: the bridge skips its own call, so the agent's summary is kept.
The call must be in command position: at the start of the command or of a
line, or after `;`, `&` (so `&&`), `|` (so `||`), `(` (so `$(`), `{` or a
backtick, optionally after `then`, `do` or `else`, and optionally with a
path (`/usr/local/bin/sciontool`). A command that only mentions it, such as
`grep "sciontool status blocked"`, does not count. The bridge matches text,
not parsed shell, so:

- False positives (count although nothing ran, so the automatic
  `task_completed` is skipped and the coordinator is not notified): one of
  those characters inside quotes, e.g. `git commit -m "fix (sciontool status
  blocked handling)"` or `echo "x; sciontool status blocked"`; a heredoc
  or multi-line string with a line that starts with a status call; and a
  status call in command position that does not run or fails (for example
  after a failing `&&` step), because the bridge sets the flag when the
  tool starts, not when the call succeeds.
- False negatives (a real call that does not count, so the bridge's
  `task_completed` overwrites it): wrapped calls such as
  `env X=1 sciontool status blocked ...`, `timeout 10 sciontool ...`,
  `sudo sciontool ...` or `bash -c 'sciontool ...'`.

## Bundle Layout

```
pi/
  agent-plugins.yaml # Native Agent Plugins support (no); kept out of config.yaml
  config.yaml        # Harness configuration (provisioner, capabilities, auth)
  provision.py       # Container-side provisioner (pre-start hook)
  scion_harness.py   # Generated copy of ../scion_harness.py (go generate ./harnesses/)
  capture_auth.py    # Captures ~/.pi/agent/auth.json as the PI_AUTH secret
  dialect.yaml       # Hook dialect for the bridge's events
  Dockerfile         # Image build (FROM scion-base), pinned pi version
  cloudbuild.yaml    # Cloud Build configuration
  scion-bridge.test.mjs, provision_test.py
  home/.pi/agent/
    settings.json                  # Safe defaults
    extensions/scion-bridge.js     # Scion lifecycle bridge
```

## Build the Image

```sh
# Local Docker build
docker build --build-arg BASE_IMAGE=scion-base:latest -t scion-pi:latest -f Dockerfile .

# Cloud Build
gcloud builds submit --config cloudbuild.yaml .
```

The pi version is the `PI_VERSION` build arg (default `1.3.0`); never use a
floating tag. The install uses `--ignore-scripts`; transitive dependencies are
not pinned by npm's global install.

## Tests

```sh
cd harnesses/pi
python3 -m unittest provision_test
node --test scion-bridge.test.mjs
# also drive a locally built sciontool (go build ./cmd/sciontool):
SCIONTOOL_BIN=/path/to/sciontool node --test scion-bridge.test.mjs
```

## Known limits

- **MCP:** pi supports stdio and streamable HTTP; SSE servers are skipped
  with a warning.
- **Usage/tokens:** no token mapping, so `SCION_USAGE_SOURCE` is not set.
- **In-session `/new`, `/resume`, `/fork` are new sessions; `/reload` is in
  place:** the bridge forwards pi's `session_start` reason as the
  session-start source. Scion classifies `reload` as an in-place restart,
  like `clear` and `compact`: it has no readiness effect and keeps the
  current status (it releases only a notification-set wait). `new`,
  `resume` and `fork` are new sessions: no readiness effect under
  `ready: delay`, but a completed or waiting status (including an agent-set
  `ask_user`/`blocked`) is replaced by working until the next run settles.
- **Synchronous bridge calls:** the bridge runs `sciontool` with
  `execFileSync`, so pi's event loop (and its TUI) waits for each call: up to
  5 s per hook event (session start, human/RPC input, each tool start and
  end, agent end; sciontool bounds its own Hub calls for a hook at 3 s) and
  up to 15 s for the `task_completed` status call, which waits on the Hub.
  A slow or unreachable Hub makes the pause visible. This matches how
  Claude's command hooks block; switching to async calls is a possible
  follow-up.
- **Provider errors:** the bridge suppresses `task_completed` when the last
  assistant message has an error stop reason. This is untested against a live
  provider.
- **Not verified against a live pi:** terminal paste/Enter delivery, Escape
  interrupt, `agent_settled` fidelity, OAuth refresh.
  `ready` uses the repo-default `delay 8s` and `extra_submits: 2`; hook-based
  readiness and fewer extra Enters (pi steers on Enter while busy) are
  follow-ups once tested against a live pi.
