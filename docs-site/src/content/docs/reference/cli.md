---
title: Scion CLI Reference
---

The Scion CLI is the primary interface for managing agents, projects, and server components.

## Global Flags

These flags are available on all commands:

- `-g, --project <string>`: Project identifier: path, slug (with Hub), or git URL (with Hub).
- `--global`: Use the global project (equivalent to `--project global`).
- `-p, --profile <name>`: Configuration profile to use.
- `--format <string>`: Output format (`json` or `plain`).
- `--hub <url|name>`: Hub API endpoint URL, or the nickname of a Hub in the hub list (`scion hub list`). Overrides `SCION_HUB_ENDPOINT`. An unknown nickname is an error that lists the registered names. Under `scion server` and `scion runtime-broker` a nickname is resolved too, but nothing else comes from the hub list there (no current Hub and no current project). Inside agents nicknames are not resolved, so a URL is needed there. See [Working with Several Hubs](/scion/hosted/user/multiple-hubs/).
- `--hub-profile <name>`: Use a target-hub profile (overrides `SCION_HUB_PROFILE`). See `scion hub profile` and [Test identities on another test hub](/scion/hosted/user/test-identity-target-hubs/).
- `-y, --yes`: Answer Yes to every confirmation prompt, including destructive ones whose interactive default is No. Required to confirm when stdin is not a terminal.
- `--non-interactive`: Full non-interactive mode (implies `--yes`, so it also answers Yes to every confirmation; errors on ambiguous prompts).
- `--debug`: Enable verbose debug output for this command. Agents it starts are not affected; use `--agent-log-level` on `scion start` or `scion resume` to debug an agent.
- `--tz <IANA zone>`: Show times in this time zone, for example `America/New_York`. Defaults to the local zone (which honors `TZ`). `Local` and invalid names are rejected.
- `--utc`: Show times in UTC. Takes precedence over `--tz`.

Every command that manages agents goes through a hub. Flags and commands that ran agents without one were removed; the [migration page](/scion/reference/local-mode-removal/) lists them with their replacements. The removed `--no-hub` flag stays hidden for one release and fails with `--no-hub was removed together with local-only mode; scion always uses a hub. See https://googlecloudplatform.github.io/scion/reference/local-mode-removal/`. With no hub configured, commands that need one fail with `No hub selected. Add one with 'scion hub add <name> <url>', start a local hub with 'scion server start', or pick one with 'scion use --hub <name>'.`

**Without a terminal.** When stdin is not a terminal, the CLI never reads prompt answers from it: a yes/no confirmation without `--yes` answers No, and a choice with no safe default fails with an error naming the flag to use. Prompts and auto-confirm notes go to stderr, so `--format json` output on stdout stays parseable. ANSI colour is used only on a terminal and never when `NO_COLOR` is set. To drive the CLI from a coding agent, see [Using the scion CLI from a coding agent](/scion/hosted/user/coding-agent-cli/).

**Behaviour change:** confirmations run without a terminal used to take their default, often Yes. They now answer No. Scripts that relied on this, for example auto-linking a project from cron or CI, must pass `--yes`.

Human-readable times use a 24-hour clock and always include a zone. `--tz` and `--utc` only change human-readable output: JSON output (`--format json`) keeps the API's UTC values.

**Debug output.** `SCION_LOG_LEVEL` controls the CLI's `[DEBUG]` and tagged lines such as `[hubsync]`; see [Controlling the Log Level](/scion/hosted/single-node/observability/#controlling-the-log-level). Inside an agent container (agent CLI mode), the CLI ignores the deprecated `SCION_DEBUG`, which older brokers set in every agent, so use `SCION_LOG_LEVEL=debug` or `--debug` there. Best-effort Hub sync steps that fail without stopping the command are reported as `Warning:` lines on stderr whatever the log level.

**CLI modes.** The CLI runs in `human` mode, with every command available, unless `SCION_CLI_MODE` (or the `cli.mode` setting) selects `agent` mode. Agent containers set `SCION_CLI_MODE=agent`, which limits the CLI to the commands an agent may use. An earlier `assistant` mode has been removed: if `SCION_CLI_MODE` or `cli.mode` is still set to `assistant`, the CLI prints one warning to stderr and runs in `human` mode.

**Project resolution order.** The CLI picks the project in this order:

1. The `-g` / `--project` or `--global` flag.
2. Inside a Hub-connected agent container only: the agent's own project (`SCION_PROJECT_ID`).
3. The project `.scion` directory found from the current directory.
4. The project last selected with [`scion use`](#scion-project) on the effective Hub (the `--hub` flag, `SCION_HUB_ENDPOINT`/`SCION_HUB_URL`, or the current Hub), for commands that need a project and for `scion tunnel`, `ssh`, `ssh-config` and `vscode`. Not used by `scion config` or inside agent containers.
5. The global project (`~/.scion`).

**Hub resolution order.** The `--hub` flag, then `SCION_HUB_ENDPOINT`, then `SCION_HUB_URL`.
Inside a scion project directory: the project's link entry (from `scion hub link`, or when a
command registers the project on the current Hub). Then the current Hub selected with `scion use`.
While either environment variable is set, link entries and the current Hub are not used. The CLI
does not read `hub.endpoint` from settings files; a project linked before link entries existed is
recorded as a link entry the first time the CLI resolves it (see
[Linked directories](/scion/hosted/user/multiple-hubs/#linked-directories)). Inside a scion
project directory, commands keep targeting that project whatever `scion use` selected. The hub list and
current context live in the per-user file `~/.scion/context.yaml` (mode `0600`); agent
containers never read it. See
[Which hub and project a command targets](/scion/hosted/user/multiple-hubs/#which-hub-and-project-a-command-targets).

A context is Hub-connected when `SCION_HUB_ENDPOINT`, `SCION_HUB_URL` or `SCION_PROJECT_ID` is set
in the environment, as in an agent container. A hub endpoint in a workstation's settings does not
make it a Hub-connected context: there, `--global` uses the local global directory.

An explicit flag wins over `SCION_PROJECT_ID`. This applies to commands that go through the Hub
pre-flight check, and to the `conversation`, `notifications` and `messages` commands.
`SCION_PROJECT` does not select a project; it is used only to detect a send within the same
project.

In a Hub-connected context, `--global` (or `-g global`) targets the Hub's Global project (slug
`global`) when the local global directory is not linked to a Hub project. If the Hub has no Global
project, or you do not have access to it, the command fails with an error that names
`--project <slug|id>` as the alternative.

A `--project` value that is a Hub project slug, name, ID or git URL selects that existing Hub
project; nothing is created or registered for it, and an unknown slug or name fails with
`project '<value>' not found on hub`. A directory of the same name in the current directory takes
precedence over a slug. Outside agent containers, a `--project` path that does not exist is refused
instead of being created. With `--global`, these commands do not link the global project to the Hub
or register this machine's Runtime Broker: `harness-config install`, `sync`, `push`, `update`,
`delete`, `pull`, `show` and `list`, `templates sync` and `push`, and the Hub sync after
`scion build`.

**Commands that need no project.** Commands that act on the Hub connection, a login, this machine's
Runtime Broker, or the server run outside a project without `--global` or `--project`:
`scion use`, `scion hub status`, `scion hub add`, `scion hub list`, `scion hub rename`,
`scion hub remove`, the `scion hub auth` commands,
`scion runtime-broker register`, `deregister`, `start`, `stop`, `restart`, `status`, `hubs` and
`join`, and the `server`, `admin` and `project` commands. Outside a project, the `scion hub`
connection commands read and write the global settings, as with `--global`. The hub list
commands (`add`, `list`, `rename`, `remove`) act on `~/.scion/context.yaml`.
`scion runtime-broker provide` and `withdraw` act on a project and still need one.

:::note[Agents creating agents in other projects]
Inside an agent container, `-g` / `--project` changes which project the CLI addresses, but the Hub
refuses an agent-created agent outside the calling agent's own project. This is intended: an
agent can only start agents in its own project.
:::

The legacy hidden `--grove` flag has been removed from every command; passing it fails with
`unknown flag: --grove`. Use `--project`.

:::caution[`--gcp-project` on GCP commands]
On `scion project service-accounts add` and `scion hub secret migrate`, the GCP project ID is passed
with `--gcp-project`. On those commands `--project` is the global Scion project selector, and using
it for the GCP project ID fails with a hint pointing to `--gcp-project`.
:::

:::tip[Decluttered CLI Help]
To keep subcommand help output clean and readable, global flags are hidden from default subcommand help outputs. You can view the full list of global flags anytime by running:

```bash
scion help global-flags
```
:::

## Agent Lifecycle

### `scion start` (or `run`)

Starts a new agent or resumes an existing one. Starting a **suspended** agent
implicitly resumes its harness session (continuing the prior conversation);
starting a **stopped** or **error** agent runs a fresh session. See
[`scion suspend`](#scion-suspend) and [`scion resume`](#scion-resume).

Starting an existing **stopped** agent restarts it in place
(printed as "Restarting", with a fresh session), the same as `scion resume`.
Starting an **error**-phase agent still fails with a conflict: use
`scion resume --force` or delete it first. When an existing agent is reused in
place, the Hub applies only the task and `--attach`. Other configuration flags
you set explicitly (for example `--type`, `--image`, `--harness-config`,
`--broker`, `--label` or `--no-auth`) are not applied, and the CLI prints a
warning that names them.

**Usage:** `scion start <agent-name> [task] [flags]`

- **Arguments:**
    - `<agent-name>`: Unique name for the agent instance.
    - `[task]`: (Optional) The initial instruction/task for the agent.
- **Flags:**
    - `-b, --branch <string>`: Target branch for the agent workspace.
    - `-t, --type <string>`: Template to use (default "gemini").
    - `-i, --image <string>`: Override container image.
    - `-a, --attach`: Attach to the agent immediately after starting.
    - `--no-auth`: Disable authentication propagation (also sent to the Hub; applies when the agent is created).
    - `-d, --detached`: Run in detached mode (default true).
    - `--config <path>`: Path to inline agent config file (YAML/JSON) for Just-In-Time (JIT) overrides, or `-` for stdin.
    - `--task-file <path>`: Read the task from a file, or `-` for stdin, up to 96 KiB. See [Long tasks and task files](#long-tasks-and-task-files).
    - `--harness-config <string>`: Named harness configuration to use.
    - `--thinking-level <value>`: Thinking level to inject into the agent config: an integer from 0 to 100, or a case-insensitive shorthand: `low` (25), `medium` (50), `high` (75), `max` (100). The level is stored as an integer; each harness maps it to its own tiers (see [Thinking Level Map](/scion/reference/harness-settings/#thinking-level-map-thinking)).
    - `--harness-auth <string>`: Override auth method for the harness. Universal types: `api-key`, `oauth-token`, `vertex-ai`, `auth-file` (each harness accepts a subset — see [Harness Authentication](/scion/local/agent-credentials/)).
    - `--broker <string>`: Preferred runtime broker ID, name, or slug for execution. A broker that does not exist fails with `runtime_broker_not_found` (404), and the message lists the brokers you can use.
    - `--agent-log-level <spec>`: Set `SCION_LOG_LEVEL` in the agent's environment, for example `debug` or `info,hubsync=debug` (same syntax as [`SCION_LOG_LEVEL`](/scion/hosted/single-node/observability/#debugging-an-agent)). An invalid value fails before anything starts. Agents never inherit `--debug` or the server's debug setting. The value is saved in the agent's applied config and kept on later restarts (remove it by deleting and re-creating the agent); when the Hub reuses an existing agent, the flag is not applied and `scion start` or `scion resume` prints a warning.
    - `--message-mode <mode>`: Set the agent's initial message mode (`project`, `branch`, `lineage`, `none`, or `hub`). Defaults to `project`. See [Message Authorization & Modes](/scion/hosted/user/messaging/#message-authorization--modes).
    - `--notify`: Get notified via the browser or system when the spawned agent reaches a terminal state.
    - `--no-wait`: Return as soon as the Hub accepts the launch instead of waiting for the agent to reach `running`. Ignored with `--attach`.
    - `--wait-timeout <duration>`: How long to wait for the agent to start (for example `10m`); must not be negative. The default is the Hub's remaining launch budget plus 30 seconds, or 5 minutes when the Hub does not advertise one. Raise it if the Hub's launch timeout has been raised.
    - `--wait-ready`: After the agent is running, also wait until its harness accepts input (its activity is no longer `initializing`, shown as "starting"). `--wait-timeout`, if set, bounds the whole wait; otherwise the readiness wait is limited to 2 minutes. Prints `Agent '<name>' is ready.` when it is; JSON output adds `"ready": true|false` to `details`. If the agent is not ready in time the command exits with status 3 and the agent is left running. Cannot be combined with `--no-wait`.

`start` waits for the agent to reach `running` when the Hub
launches it asynchronously, after a workspace upload, or with `--attach`.
Otherwise (for example when the Hub does not launch asynchronously) it returns
as soon as the Hub answers, as before, possibly while the agent is still
provisioning. A synchronous launch runs on the Hub independently of the CLI
request: if the CLI gives up or you press Ctrl-C mid-dispatch, the Hub keeps
going instead of rolling the agent back. Each dispatch attempt is bounded at
120 seconds, so a slow cold start (for example on Kubernetes) can still fail on
a synchronous launch; enable [asynchronous agent create](/scion/reference/server-config/#asynchronous-agent-create) for those. While waiting, each launch step is printed to stderr with a readable label, for example
`my-agent: provisioning (waiting to be scheduled)` (nothing extra under `--format json`). If the
launch fails, the error gives the failure code (with its label, unless the message already says
it), the step and the message, for example `agent 'my-agent' create did not complete
(image_pull_failed) while pulling the image: ...`. A create, including `scion start` of a new
agent, reports `create did not complete`; a start or resume of an existing agent reports `did not
start`. A launch that ended with an error ends the wait whatever phase the agent reports. If the wait times out, or you press Ctrl-C, only
the wait stops: the launch continues on the Hub, and re-running `scion start
<agent-name>` resumes waiting. When the Hub accepted the launch and the wait
times out, `start` reports the agent as accepted and launching, says why it
stopped following the launch, suggests `scion list` to check on it, and exits
0, so a caller does not retry a launch that is under way (under `--format json`
the result has status `success` and a `launchNote` detail). With `--attach`,
which needs a running agent, a timeout exits 1. Ctrl-C exits with status 130
and SIGTERM with 143; a rejected create, a failed launch or an agent deleted
while launching exits 1. Network errors and Hub answers of 5xx, 408 or 429 are
retried while waiting; any other 4xx (for example 401 or 403) stops the wait at
once with the Hub's error, and the launch continues on the Hub. One exception:
until the agent's status has been read once, a 403 or 404 is retried for up to
5 seconds while the Hub is still launching the agent (an asynchronous launch,
or a start after a workspace upload). If an agent launcher still cannot read
the new agent's status after that, `start` reports that the launch was accepted
and its status is not readable with this credential's scope, and exits 0 like a
timeout (1 with `--attach`); the launch continues on the Hub. When the Hub has
already finished the start and you wait with `--attach`, there is no retry: an
agent launcher's 404 gives the same not-readable report at once, and exits 1.
An agent launcher's 404 after the status was read is retried for 5 seconds (or
until the wait ends, if that is sooner). If it lasts, `start` reports that the
status is no longer readable and that the agent may have been deleted or the
Hub refused the read: the Hub answers an agent with 404 both for an agent that
is gone and for a read it refuses, so a launcher cannot tell the two apart.
This exits 0 when the Hub accepted the launch, and 1 with `--attach` or when
it did not. The agent is reported as deleted only for a user's login: on a 404
after its status was read that persists through the 5-second retry (or until
the wait ends, if that is sooner), or on a 404 before that, after the 5-second
retry while the Hub is still launching the agent, and at once otherwise. If the
agent's create did not complete (for example the image could not be pulled), the
error shows the stored template and task. Delete the agent and create it again
(`scion delete <agent-name>`, then `scion start` with the same template and
task). If soft-delete retention is enabled on the Hub, the name stays reserved
until the agent is deleted with force=true or purged; until then, use a new
name. With `--format json`, `--attach` after a workspace upload attaches without
printing the JSON result.

#### Long tasks and task files

`--task-file <path>` on `scion start` and `scion create` reads the task from a
file (`-` reads stdin). Use it for long briefs that are too big for a command
line or a message. Any `[task]` arguments come first, followed by a blank line
and then the file. The file must be UTF-8 text and not empty or whitespace
only. The whole task, arguments included, can be at most 96 KiB (98304 bytes).
The CLI checks this before it contacts the Hub, and anything larger is
rejected. The Hub forwards the create request to the runtime broker as JSON in
a control channel message of at most 1 MiB. Even a task made only of characters
that JSON escapes to six bytes, such as `<`, `>` and `&`, fits in that message
with room for the rest of the request. The task goes in the create request like
any other task. The Hub stores it with the agent, so this works for agents a
Hub runs on any runtime, including Kubernetes agents without access to a
storage bucket. It also works when an agent launches another agent: the agent's
CLI reads the file from the agent's own filesystem.

Whenever an agent starts with a task, from arguments, `--task-file` or
`prompt.md`, the full text is written to `~/.scion/task.md` in the agent's home
directory (mode 0644). Each start that has a task replaces that file; a
symbolic link at that path is replaced, never written through, and the start
fails if `~/.scion` is a symbolic link. The task starts the harness inside a
tmux command, and tmux rejects commands larger than about 16 KB. The harness
gets a task inline, as before, when it takes at most 8 KiB (8192 bytes) in that
command once quoted for the shell. Each single quote (`'`) takes 13 bytes
there, so a task with many quotes is passed as a file at a smaller size. A
larger task is replaced by a short task that names `~/.scion/task.md` and its
size and asks the agent to read the whole file.

### `scion create`

Provisions a new agent without starting it. Scion writes the agent's
directory, workspace and `prompt.md`, and the agent stays in phase `created`
with no container. It is not started even when you pass a task. Run
`scion start <agent-name>` to start it.

The output says that the agent is provisioned but not started, and gives the
`scion start` command. If a Hub created the agent record but the runtime broker
could not provision it, the output shows the provisioning warning and says the
agent was not fully provisioned; `scion start` retries provisioning and starts
the agent. With `--format json`, `details.started` is `false`,
`details.provisioned` says whether the agent was provisioned, and
`details.startCommand` holds the start command.

`scion list` and the Web Dashboard's status badges show such an agent
as `created (not started)`, with a hint to start it, so it does not look like a
failed start.

**Usage:** `scion create <agent-name> [task] [flags]`

- **Arguments:**
    - `<agent-name>`: Unique name for the agent instance.
    - `[task]`: (Optional) The task, written to `prompt.md` for when the agent starts.
- **Flags:**
    - `-t, --type <string>`: Template to use.
    - `-i, --image <string>`: Override container image.
    - `-b, --branch <string>`: Git branch to use for the agent workspace.
    - `-w, --workspace <string>`: Host path or project-relative subdirectory to mount as `/workspace`.
    - `--config <path>`: Path to inline agent config file (YAML/JSON), or `-` for stdin.
    - `--task-file <path>`: Read the task from a file, or `-` for stdin, up to 96 KiB. See [Long tasks and task files](#long-tasks-and-task-files).
    - `--harness-config <string>` (alias `--harness`): Named harness configuration to use.
    - `--harness-auth <string>`: Override auth method for the harness (`api-key`, `oauth-token`, `auth-file`, `vertex-ai`).
    - `--broker <string>`: Preferred runtime broker ID or name.
    - `--label <key=value>`: Label for the agent (repeatable).
    - `--role <string>`: Agent role for Hub API access (`none`, `readonly`, `baseline`, `full`).
    - `--message-mode <mode>`: Set the agent's initial message mode (`project`, `branch`, `lineage`, `none`, or `hub`). See [Message Authorization & Modes](/scion/hosted/user/messaging/#message-authorization--modes).
    - `--service-account <string>`: GCP service account to assign: its id, email or display name. An email matches the project's own account before a hub-wide one; a display name that matches more than one account returns `identity_ambiguous` with the candidates' ids, and you retry with an id.
    - `--upload-template`, `--no-upload`, `--template-scope <scope>`: Template upload behavior.

### `scion stop`

Stops a running agent. This is a graceful shutdown (`SIGTERM`); the agent's
phase becomes `stopped` and the next `start` runs a fresh session.

Stopping an agent whose Runtime Broker is offline queues the stop: the CLI prints
"stop queued via Hub", and the Runtime Broker applies the stop when it reconnects, unless a newer
start or stop was recorded first. With `--rm`, a queued stop does not remove the agent; run
`scion delete` once the Runtime Broker is back. Any warnings the Hub returns are printed.

**Usage:** `scion stop <agent-name> [flags]`

- **Flags:**
    - `--rm`: Remove the agent after stopping. The command waits for the Hub to
      confirm the removal, as [`scion delete`](#scion-delete-or-rm) does, and prints that removal
      is in progress if the Hub answers `202`. Once the removal is confirmed, the agent's local
      files (agent directory and worktree) are removed too, as with `scion delete`; its git branch
      is kept. If the Hub accepts the removal but cannot confirm it, or the removal fails, local
      files are left in place. If the local cleanup itself fails, the command still succeeds and
      prints a warning suggesting `scion delete --cleanup-only --preserve-branch <agent-name>`.
    - `-a, --all`: Stop all running agents in the current project, at most six at a time. If any agent fails to stop
      (or, with `--rm`, to be removed), the command exits 1. With `--format json` the result
      object (`"status": "partial"`) is still printed on stdout, and no separate error message is added.
    - `--self`: With `--rm`, allow removing the current agent. Inside an agent container, `scion stop --rm` naming the agent the CLI runs in is refused before anything is stopped, and exits 1 with a message that `--self` is required; re-run the same command with `--self` added to go ahead. `--all --rm` skips the current agent, prints a note on stderr that it was skipped and that `--self` includes it, and stops and removes the other agents as usual. With `--format json` the result object lists the skipped agent under `"skipped"`. If the current agent was the only running agent, the command prints "No other running agents to stop (current agent skipped; add --self to include it)" instead of "No running agents found." The current agent is matched by name, slug or agent ID within its own project. A stop without `--rm` does not need `--self`. Outside an agent container `--self` has no effect.

### `scion suspend`

Suspends a running agent, preserving its harness session for a later resume.
Unlike `stop`, suspending sets the agent's phase to `suspended`, and the next
`start` (or `resume`) **continues** the prior conversation instead of starting
fresh.

Only running agents can be suspended, and the agent's harness must support
session resume (Claude Code and Gemini CLI do; the generic harness does not —
use `stop` instead). See [Agent Lifecycle](/scion/local/agent-lifecycle/).

**Usage:** `scion suspend <agent-name> [flags]`

- **Flags:**
    - `-a, --all`: Suspend all running agents in the current project, at most six at a
      time. Agents whose harness does not support resume are skipped.

### `scion resume`

Resumes an existing agent. For a **suspended** agent, the harness session is
continued (Claude Code receives `--continue`, Gemini CLI `--resume`, etc.). For
a **stopped** agent, there is no session to continue, so a fresh session is
started.

A plain `scion resume <agent-name>` (no task) simply **continues** the prior
session — the agent's original creation task is *not* re-injected. If you pass an
explicit prompt, it is sent as a **new message** on top of the continued
session.

**Usage:** `scion resume <agent-name> [task] [flags]`

- **Flags:**
    - `-a, --attach`: Attach to the agent immediately.
    - `-f, --force`: Force resume an agent in the `error` phase. This attempts an in-place restart of a crashed or interrupted session, preserving the prior harness conversation state instead of starting fresh.
    - `--no-wait`, `--wait-timeout <duration>`: Same as for [`scion start`](#scion-start-or-run).
    - `--agent-log-level <spec>`: Same as for [`scion start`](#scion-start-or-run). Resume reuses the existing agent, so the flag is not applied and `scion resume` prints a warning; the value saved when the agent was created stays in effect.

### `scion attach`

Connects to the interactive session of a running agent.

When connecting to a Hub behind Google Identity-Aware Proxy (IAP), `scion attach` automatically attempts to resolve transport-layer authentication (Google OIDC ID tokens) *before* evaluating the application-level access token gate. If transport auth can be successfully established (e.g., using your local Google Cloud SDK identity or GKE Workload Identity), the application token check is bypassed, enabling seamless attachment in proxy-auth/IAP mode.

If the agent is stopped, the attach ends immediately rather than waiting and retrying (see [PTY close codes](/scion/reference/api/#pty-close-codes)).

**Usage:** `scion attach <agent-name>`

- **Key Bindings:**
    - `Ctrl-b`, then `d`: Detach from the session without stopping the agent (the tmux detach key; see [Interactive Sessions with Tmux](/scion/local/tmux/)). The container runtime's default `Ctrl-p Ctrl-q` detach sequence is not used, so `Ctrl-p` reaches the agent. Podman's detach keys are off. Docker's are moved to `Ctrl-\` then `Ctrl-^`: a single `Ctrl-\` is delayed until the next key, and the full sequence ends the attach while the agent keeps running (see [Interactive Sessions with Tmux](/scion/local/tmux/#basic-operations)).
- **Requires a terminal:** `scion attach`, `scion start --attach` and `scion resume --attach` need an interactive terminal on both stdin and stdout. Without one (for example from a script or a coding harness) they fail at once with `attach requires an interactive terminal` and a non-zero exit, before starting anything. Use `scion look` to view a session and `scion message` to send input instead.
- **Not running:** if the Hub reports the agent as not running, `scion attach` names the next step for the agent's phase: `scion resume <agent> --attach` for a stopped or suspended agent, waiting and then resuming for a stopping agent, `scion logs <agent>` and then resuming for an agent in the `error` phase, and `scion start <agent> --attach` for anything else. A managed agent is refused before the phase check, whatever its phase; any other agent gets the Hub preflight (below) once it is running.
- **Preflight:** `scion attach` asks the Hub whether it can reach the agent's terminal (a plain `GET` of the agent's `/pty` endpoint) before the first connection and before each automatic reconnect. The Hub picks the path: the Runtime Broker, or the agent's own session when the broker's runtime has no attach. A refusal (for example `503`) ends the command with the Hub's reason and is not retried; if the Hub cannot be reached before a reconnect, the reconnect is retried after the usual backoff, within the limit of 3 short attempts in a row; when the Hub has no path at all (`runtime_attach_unsupported`), the message says so. `scion start --attach` and `scion resume --attach` use the same preflight.
- **Reconnect:** `scion attach` reconnects by itself, once per close, when the session closes with `4503` (after a random delay of up to 5 seconds) or `4504` or `1011` (after the normal backoff), and the screen redraws. It stops after 3 reconnects in a row whose sessions each ended within a minute, and Ctrl-C during the wait stops it. It does not reconnect after any other close. When the session ends for any other reason than a detach, a reconnect fails, or the CLI stops reconnecting after 3 short sessions in a row, the command exits non-zero with a message that says what happened and what to run next, based on the [PTY close code](/scion/reference/api/#pty-close-codes). For example, a dropped runtime broker (`4503`) whose reconnect also failed suggests running `scion attach` again, and an ended session (`4410`) suggests `scion resume`. `scion start --attach` and `scion resume --attach` use the same attach flow and print the same messages.
- **Hub URL with a path prefix:** a Hub served under a path (for example `https://example.com/scion`) works for attach as it does for other commands.

See [Attaching to a remote agent](/scion/hosted/user/hosted-user/#attaching-to-a-remote-agent) for Hub-mode details such as who can attach.

### `scion message` (or `msg`)

Sends a message to a running agent or user.

**Usage:** `scion message <recipient> <message> [flags]`

- **Recipients:**
    - `<agent-name>`: Send to an agent (default, same as `agent:<name>`).
    - `agent:<name>`: Send to an agent explicitly.
    - `user:<name>`: Send to a user's inbox.
    - `group[a,b,...]`: Send to multiple recipients.
    - `@<agent-name>`: Send to an agent's conversation (preferred).
    - `@<email>`: Send to a user by email (global DM).
    - `conv:<uuid>`: Send to a conversation by ID.
    - `#<thread>`: Send to a named thread.

- **Arguments:**
    - `<recipient>`: The recipient (see above).
    - `<message>`: The text to send. Pass `-` to read the body from stdin.
- **Flags:**
    - `-i, --interrupt`: Interrupt the harness before sending the message.
    - `-w, --wake`: Resume a suspended agent before delivering the message. Requires the permission to start the agent (`agent.lifecycle`); without it the send fails with `403` and the agent stays suspended. If the agent is still starting (activity `initializing`) when the wake wait ends, the message is delivered and held until its harness is ready, and `--interrupt` is delivered as an ordinary message instead of interrupting.
    - `--body-file <path>`: Read the message body from a file instead of passing it inline. Useful for long messages and scripted workflows. `--body-file -` reads the body from stdin, like a `-` message argument. Mutually exclusive with the inline `<message>` argument.
    - `--attach <path>`: Attach one or more file paths (repeatable). File paths must be within allowed roots (`/workspace` or `/scion-volumes`), where relative paths resolve against `/workspace`.
        - **Constraints:** Cannot be combined with `--in` or `--at`.
        - **Requirements:** If the file is not a regular file (e.g., is a directory) or is outside allowed roots, the command will fail.
        - **Undelivered attachments:** When an agent sends the message and the Hub cannot read an attached file (for example, it was staged on a different Runtime Broker's host than the Hub's), the message is still delivered without it, and the command prints `Warning: attachment <path> was not delivered: <reason>` on stderr for each such file. With `--format json`, the warnings are in the result's `attachment_warnings` field instead. Messages sent by a human from the CLI do not report undelivered attachments yet.
    - `--artifact <ref>`: Reference a published artifact, `scion://artifact/<id>` or `scion://artifact/<id>@<seq>` (repeatable, at most 10). The reference is added to the message text, and an agent recipient's message ends with one line per artifact showing how to fetch it (`Artifact: v2 - scion artifact get <ref>`, or `current` for an unpinned reference). The Hub attaches only artifacts the sender can read and prints a warning for the rest; a recipient still needs its own access to read an artifact. See [Artifacts](/scion/reference/artifacts/#artifacts-in-messages).
        - **Constraints:** Cannot be combined with `--in` or `--at`, or used with `group[...]` recipients.
        - **Requirements:** Requires the `hub.artifacts` experiment on.
    - `--cc <agents>`: *(Deprecated — will be removed.)* Carbon copy additional agents. This flag is **repeatable** and also accepts a **comma-separated list** of agent names (e.g., `--cc dev-agent,qa-agent --cc test-agent`). Use `group[...]` addressing or body `@mentions` instead.
    - `--notify`: *(Deprecated — use `scion notifications subscribe` instead.)* Get notified when the target agent(s) respond or reach a terminal state after receiving the message.
    - `--plain`: *(Deprecated — will be removed.)*  Mark for plain-text delivery.
    - `--channel <channel>`: *(Deprecated — address the conversation with `conv:<uuid>` instead, or use `@<name>` to message an agent.)* Target a specific message channel (e.g., `telegram`, `gchat`, `teams`, `web`).
    - `--thread-id <id>`: *(Deprecated — address the conversation with `conv:<uuid>` instead; `scion conversation list` shows conversation IDs.)* Target a specific thread ID within the channel. For `user:` recipients on the web channel, the thread must already exist as a conversation: the Hub rejects an unknown or deleted thread ID (HTTP 422) instead of creating a new conversation, and the command exits 1. External channels (for example Slack, Teams, or Google Chat) deliver to the thread ID themselves and are not checked. On success, a `user:` send prints the conversation the message was recorded in.
    - `--raw`: *(Removed — use `scion keys` instead.)* Raw keystroke delivery through messages has been removed. Any use of `--raw` (with any value, target, or mode) fails before anything is sent, with an error pointing to `scion keys`. The Hub likewise rejects a message request carrying the retired `raw` field with `422 raw_input_removed`.
    - `--in <duration>`: *(Deprecated — use `scion schedule create --in` instead.)* Schedule message delivery after a duration.
    - `--at <time>`: *(Deprecated — use `scion schedule create --at` instead.)* Schedule message delivery at an absolute time.

- **Output:**
  A successful send ends with a confirmation that names the message the Hub created, as a `(message <id>)` suffix, for example `Message accepted for agent 'reviewer' (message 0f3c…).` or `Message sent to user:alice@example.com via Hub (conversation 7b1e…, message 0f3c…).` A group send prints the ID on each recipient's line (`  Accepted: agent:a (message 0f3c…)`), because every recipient gets its own message. The ID is the one agents see as `message_id` in the delivered message. For a message recorded in a conversation, it is also the `<message-id>` that `scion conversation get-message conv:<conversation-id> <message-id>` takes; a message with no conversation (for example a broadcast) cannot be fetched that way. A Hub that does not report an ID gets the confirmation without the suffix. `scion broadcast` prints no ID. With `--format json`, a single send's result has the ID in `message_id`, and a group send's `results` entries carry `message_id` for each accepted (`delivered`), deferred or `unknown` (ambiguous) recipient; a failed recipient has none.

- **What the confirmation means:**
  The Hub reports a status for each agent send, and the CLI words its confirmation by what that status actually confirms. "Accepted" means the Hub handed the message to the agent's runtime, which buffered it as input. It does not confirm that the agent has read or acted on it. No send status currently confirms that.

  | Hub status | Meaning | CLI confirmation |
  |---|---|---|
  | `dispatched`, `delivered` | The runtime accepted and buffered the message. | `Message accepted for agent 'x' (message <id>).` |
  | `delivered_not_persisted` | Accepted as above, but the Hub could not save it to history, so there is no message ID. | `Message accepted for agent 'x'.` |
  | `ambiguous` (HTTP `202`) | The Hub could not tell whether the runtime took the message. Check before you resend. | `Message accepted for agent 'x' (delivery unconfirmed) (message <id>).` |
  | `deduplicated` | An agent sent an `@mention` notification that repeats one the Hub sent moments earlier for the same message, so it was not sent again. The ID is the earlier message's. | `Message accepted for agent 'x' (duplicate of an earlier send) (message <id>).` |
  | `deferred` (HTTP `202`) | The agent is reincarnating. The message was saved to history and not sent yet. The new generation sees it on catch-up. | `agent x is reincarnating; message saved to history and will be seen on catch-up (message <id>).` |
  | failed (an error response) | The message was not sent. | An error, and no confirmation. |

  `scion broadcast` prints a `Broadcast accepted (…)` line, followed by a note when some targeted agents are still starting (the message is held until they are ready), or `No running agents …` when it reached none. With `--all` it prints `Broadcasting message to <n> agents...`, then `Message accepted for agent 'x' via Hub.` for each agent it reached, whatever the status, and a warning for each send that failed. With `--format json`, a single `scion message` send prints the Hub's response, with its raw `status` value, except a human's `@<agent-name>` send, which prints nothing on success; group sends report the statuses described below.

- **Group sends and exit codes:**
  A `group[...]` send delivers to at most six recipients at a time, and reports each recipient's outcome: accepted (`delivered` in JSON), `deferred` (saved while the agent reincarnates), `failed` (with the reason), or `unknown` (no definite answer, for example a timeout, a gateway error, or the Hub reporting delivery as `ambiguous`, so the message may have been delivered). When not every recipient was reached, the output lists the accepted and failed recipients and a recipient argument naming only the failed ones: the bare recipient (for example `agent:b`) when one failed, or `group[...]` when several did, since `group[...]` needs at least two recipients. With `--format json` the output is an object:
  ```json
  {
    "group_id": "…",
    "total": 3, "delivered": 1, "deferred": 0, "failed": 1, "unknown": 1,
    "results": [
      {"recipient": "agent:a", "status": "delivered", "message_id": "…"},
      {"recipient": "agent:b", "status": "failed", "error": "…"},
      {"recipient": "agent:c", "status": "unknown", "error": "…"}
    ],
    "retry_recipient": "agent:b"
  }
  ```
  Earlier versions printed only a bare array of results; that array is now the `results` field (`jq '.results[]'` instead of `jq '.[]'`).
  `scion message` exits `0` when the message was sent (for a group, every recipient was accepted or deferred), `1` when it was not sent (for a group, no recipient received it, so the whole send can be retried), and `3` when a group send partly succeeded. On exit `3`, do not resend to the whole group: retry only `retry_recipient` (pass it back as the recipient argument), and check `unknown` recipients before you resend to them. An interrupt (Ctrl-C or `SIGTERM`) while the group sends are in progress cancels them and still prints the results: sends already in flight are `unknown`, sends not yet started are `failed` (not sent), and `@mention`/`--cc` notifications are skipped. Outside that window an interrupt exits immediately.

- **Message Body Formatting:**
  The command delivers the `<message>` argument **verbatim** — it performs no escape expansion, no markdown rendering, and no character substitution. Whatever bytes you pass are exactly what the recipient receives.
  
  To include newlines, use real newlines inside shell quoted strings or heredocs. Do **not** use JSON-encoded bodies or literal backslash-n (`\n`) sequences — those will appear as literal characters in the delivered message.

  * **Correct (real newlines in a quoted string):**
    ```bash
    scion message --non-interactive @reviewer "PR #42 is ready for review.

    Branch: fix/auth-bug
    CI: all green"
    ```

  * **Correct (heredoc for longer messages):**
    ```bash
    scion message --non-interactive @reviewer "$(cat <<'EOF'
    PR #42 is ready for review.

    Branch: fix/auth-bug
    CI: all green
    EOF
    )"
    ```

  * **Wrong (JSON-encoded body with literal `\n`):**
    ```bash
    # BAD: literal \n chars appear in the delivered message
    scion message --non-interactive @reviewer "PR #42 is ready for review.\n\nBranch: fix/auth-bug\nCI: all green"
    ```

**Message body input:** for `--body-file` and stdin (`-` or `--body-file -`), trailing CR/LF characters are trimmed and everything else, including interior newlines, is sent exactly as read. An inline `<message>` is sent as given. An empty body is an error.

:::caution[Backticks and `$(...)` in double-quoted messages]
Your shell expands backticks and `$(...)` inside double-quoted arguments **before** `scion` runs: it executes the command and splices its output into the message. `scion` cannot detect or undo this. To send code or shell snippets verbatim, use `--body-file`, or stdin with a quoted heredoc (`<<'EOF'`):

```bash
scion message my-agent - <<'EOF'
Run `make test`, then check $(git rev-parse HEAD).
EOF
```
:::

### `scion broadcast`

Sends a message to all running agents in the current project (or across all projects).

**Usage:** `scion broadcast <message> [flags]`

This command replaces the removed `--broadcast` / `--all` flags on `scion message`.

Like a group send, a broadcast (including `--all`) sends to at most six agents at a time.

### `scion keys`

Sends literal terminal input to an agent's tmux session via `send-keys`, with no trailing Enter and no envelope. It is a distinct operation from messaging, authorized like terminal attach rather than like `scion message`.

**Usage:** `scion keys <agent-name> <keys>`

**One argument, one tmux argument — not a sequence.** The entire `<keys>` string becomes exactly one `send-keys` argument. tmux recognizes a small set of named keys (`Enter`, `Escape`, `C-c`, arrow names, etc.; `Enter` is the canonical spelling) only when the *whole* argument matches one name exactly; anything else — including a string containing spaces — is typed character by character. `scion keys my-agent "Up Up Enter"` therefore sends the eleven literal characters `U`, `p`, ` `, `U`, `p`, ` `, `E`, `n`, `t`, `e`, `r`, not three key presses. The command takes exactly two arguments, so leaving the keys unquoted (e.g. `scion keys my-agent Up Up Enter`, four arguments) is rejected outright with an argument-count error rather than silently reinterpreted — quote any `<keys>` value containing spaces. To send multiple key presses, issue separate `scion keys` calls, one per press — there is no atomic multi-key sequence form.

```bash
scion keys my-agent "Escape"
scion keys my-agent "C-c"
scion keys my-agent "Enter"
```

**Project isolation.** When run by an agent, `scion keys` can only target agents in the agent's own project: cross-project targets are refused, both by the CLI itself and by the Hub's `/keys` authorization. A human operator using `--project` can target other projects — there is no blanket human cross-project ban — but needs attach authority on the target (the same authority as `scion attach`), not message authority.

`scion keys` calls the Hub's dedicated keys operation described in [API Reference](/scion/reference/api/#agents-apiv1agents) (the project-scoped `/keys` route), never the message path. That means: input validation and bounding to 4096 UTF-8 bytes before dispatch (an empty `<keys>` string is rejected before any request is sent); authorization mirroring `agent.attach` (a human operator needs the same authority as opening a terminal — closed/`none` message mode does not block it, and message-only authority does not grant it; an agent caller additionally needs a live attach relationship on the target — see [Permissions & Policy](/scion/hosted/ha/permissions/)); a `409` rather than a wake/start for a stopped or suspended target, and a `503` (safe to retry only after fixing the route — never a `502`/`504`, which must never be retried automatically) for an unreachable broker or no immediate route; single-attempt delivery with no automatic retry, even if the client is configured with retries; no message, conversation, or terminal-content record, only content-free audit; and a `422 keys_unsupported` error for a managed-runtime target or an un-upgraded Runtime Broker — see [API Reference](/scion/reference/api/#agents-apiv1agents) for the full outcome table. A Hub predating the `/keys` route itself (rather than just an old broker behind an up-to-date Hub) answers with that Hub's own generic `404`. The CLI reports any `404`/`405` without an `operation_id` — an old Hub, or a project the Hub does not recognize — as a rejection (`hub_unsupported`), and never falls back to the message path.

**Outcome reporting.** Every attempt resolves to one of three outcomes: `dispatched` (the Runtime Broker acknowledged terminal injection — not that the harness acted on it), `rejected` (definitely not delivered; safe to correct and retry), or `unknown` (may or may not have reached the terminal; check with `scion look` before resending). With `--format json`, the CLI prints exactly one result object on success or failure, including the outcome, the machine outcome code when known, and the Hub's `operation_id` when one was returned. The outcome code is never empty or a generic placeholder: if a proxy or gateway answers for the Hub (for example a bare `502` with no body, or any response with no keys outcome code and no `operation_id`) and the status does not prove the keys were not delivered, the CLI reports outcome `unknown` with code `keys_outcome_unknown` and a message naming the HTTP status, and exits non-zero.

`scion keys` replaces the removed `scion message --raw` flag. `scion message --raw` now fails locally, before any request is sent, with guidance naming `scion keys`; update scripts and skills to call `scion keys` directly. See [Migrating from raw message delivery](/scion/reference/raw-message-removal/).

### `scion set-message-mode`

Sets the message mode for an agent, controlling which users and agents can send messages to it. Full-role agents can also call this command programmatically.

**Usage:** `scion set-message-mode <agent-name> <mode>`

- **Arguments:**
    - `<agent-name>`: The target agent.
    - `<mode>`: One of `project` (default), `branch`, `lineage`, `none`, or `hub`.

See [Message Authorization & Modes](/scion/hosted/user/messaging/#message-authorization--modes) for details on each mode. The `hub` mode enables [cross-project messaging](/scion/hosted/user/messaging/#cross-project-messaging).

### `scion messages` (aliases: `msgs`, `inbox`)

Manages bidirectional communication and persistent messages sent by agents to humans.

**Usage:** `scion messages [command] [flags]`

- **Commands:**
    - `list` (default): View unread messages.
    - `read <message-id>`: Mark a specific message as read.
    - `read-all`: Mark all messages as read.
- **Flags:**
    - `--agent <string>`: Filter messages by a specific agent.
    - `--all`: Show all messages, including those already marked as read.

### `scion conversation` (alias: `conv`)

Manages conversations — the surface-agnostic containers for message threads. Running `scion conversation` without a subcommand defaults to `list`. It also works inside Hub-connected agent containers, as does `scion notifications`; this requires a harness image built from this release or later.

Conversations are referenced using one of three forms:

- `conv:<uuid>` — by conversation ID.
- `@<agent-name>` — resolves the direct conversation with the named agent.
- `#<thread-name>` — resolves a named group conversation.

**Usage:** `scion conversation [command] [flags]`

- **Commands:**
    - `list` (default): List conversations you participate in. The NAME column shows `DM:<name>` for a direct conversation (the other participant), otherwise the conversation's display name or thread name.
    - `get <conversation-ref>`: Show conversation details.
    - `get-message <conversation-ref> <message-id>`: Retrieve a single message by its ID from a conversation. Authorization is participant-based — only participants of the conversation can retrieve its messages.
    - `messages <conversation-ref>`: View messages in a conversation.
    - `create <name>`: Create a new group conversation. The group appears as a thread in the project's web chat space. Names must start with a letter or digit, contain only letters, digits, spaces, `_`, or `-`, and be at most 100 characters (returns `400 Bad Request` otherwise). A name already used in the project returns `409` (name conflict).
    - `set-default <conversation-ref> <agent-id>`: Set the default agent for a conversation.
    - `participants <conversation-ref>`: List participants in a conversation.
    - `join <conversation-ref> <principal-kind> <principal-id>`: Add a participant to a conversation.
    - `leave <conversation-ref>`: Leave a conversation.
    - `catch-up <conversation-ref>`: Show recent messages in a conversation.
    - `group <command>`: Manage the project's thread groups. See [`scion conversation group`](#scion-conversation-group) below.
- **Flags (on `list`):**
    - `--kind <string>`: Filter by kind (`direct`, `group`).
    - `--surface <string>`: Filter by surface (`native`, `discord`, `slack`, etc.).
    - `--project <string>`: Filter by project ID.
    - `--limit <int>`: Maximum number of conversations to show (default 50).
    - `--json`: Output in JSON format.
- **Flags (on `messages`):**
    - `--limit <int>`: Maximum number of messages to show (default 25).
    - `--before <time>`: Show messages before this time (RFC 3339).
    - `--after <time>`: Show messages after this time (RFC 3339).
    - `--json`: Output in JSON format.
- **Flags (on `create`):**
    - `--project <string>`: Project ID. Defaults to the Hub-linked project, then the local project. If no project resolves, the Hub falls back to the calling agent's project; a user caller with no project gets `400 projectId is required`.
    - `--group <group>`: File the new conversation into a thread group of the same project: a group ID, or a group name resolved within the project as `scion conversation group` resolves it. The group is checked before the conversation is created, so an unknown or ambiguous group creates nothing. Requires the `hub.chat_thread_groups` experiment.
    - `--json`: Output in JSON format.
- **Flags (on `catch-up`):**
    - `--since <duration>`: Show messages from this duration ago, e.g. `30m`, `2h` (default `1h`).
    - `--json`: Output in JSON format.
- **Flags (on `get-message`):**
    - `--json`: Output in JSON format.

### `scion conversation group`

Manages a project's **thread groups**: named sets of the project's group conversations (threads), shared by every member of the project, user or agent. A thread is in at most one group, so adding it to another group moves it. Requires the `hub.chat_thread_groups` [experiment](/scion/reference/experiments/) (default off). On a Hub with the experiment off, every subcommand fails with `thread groups are not enabled on this hub (experiment hub.chat_thread_groups)`.

A `<group>` argument is a group ID, or a group name resolved within the project (`--project <id>`, defaulting to the current project) through the project's group list. Names are compared as the Hub compares them: trimmed and case-insensitive. A name that matches no group is an error. A name that matches more than one group (including an ID that is also another group's name) is an error that lists each match by name and ID; use the group ID then. An ID that is not in the project's list is looked up directly, and the Hub decides whether you can read it. A `<conversation-ref>` takes the same forms as the other `scion conversation` commands.

**Usage:** `scion conversation group [command] [flags]`

- **Commands:**
    - `list`: List the project's thread groups, sorted by name. Text output has the columns `NAME`, `THREADS` (the number of member threads) and `ID`.
    - `get <group>`: Show a thread group and its member threads (name, conversation ID and when it was added).
    - `create <name>`: Create a thread group. A name is 1 to 100 characters after trimming, with no control characters. A name already used in the project, compared case-insensitively, returns `409`. A project can have at most 100 groups (`422`).
    - `rename <group> <new-name>`: Rename a thread group. The name rules of `create` apply.
    - `delete <group>`: Delete a thread group. Its threads become ungrouped; no thread or message is deleted. Asks for confirmation; without a terminal it needs `--yes`. Only users may delete a thread group: the command is not available in agent mode, and the Hub answers `403` to an agent.
    - `add <group> <conversation-ref>`: Add a thread to a thread group, moving it out of any other group of the project. Adding a thread that is already in the group changes nothing. The thread must be a group conversation of the group's project; direct conversations and the project's `#general` thread are refused. A group holds at most 500 threads (`422`).
    - `remove <group> <conversation-ref>`: Remove a thread from a thread group. The thread is not deleted. Removing a thread that is not in the group changes nothing.
- **Flags (on every subcommand):**
    - `--project <string>`: Project ID: the project to list or create groups in, and the project a `<group>` name is resolved in. Defaults to the Hub-linked project, then the local project, as for `scion conversation create`. If no project resolves, the Hub falls back to the calling agent's project; a user caller with no project gets `400 projectId is required`. As on `scion conversation create`, this local flag takes a project ID and replaces the global `-g/--project` for these commands.
- **Flags (on `list`, `get` and `create`):**
    - `--json`: Output in JSON format. `list` prints `{"threadGroups": [...]}`, the Hub's list body; `get` and `create` print one thread group: `id`, `projectId`, `name`, `createdBy`, `createdByKind` (`user` or `agent`), `createdAt`, `updatedAt` and `threads`, where each thread has `conversationId`, `threadId` (the web chat thread ID, empty for a conversation with no web thread), `name`, `addedBy` and `addedAt`. `threads` is always a list, never `null`.
- **Flags (on `delete`):**
    - `-y, --yes`: Answer Yes to the confirmation prompt.

In agent mode (inside agent containers), every subcommand except `delete` is available.

### `scion logs`

Displays the logs of an agent.

**Usage:** `scion logs <agent-name> [flags]`

- **Flags:**
    - `-n, --tail <lines>`: Number of lines from the end (default 100).

`--follow` was removed: it only worked for local agents, which were removed with local mode.

### `scion look`

Shows an agent's current terminal output.

**Usage:** `scion look <agent> [flags]`

- **Flags:**
    - `--plain`: Strip ANSI escape sequences from the output.
    - `--full`: Capture the full scrollback history.
    - `-n, --num-lines <n>`: Capture the last `n` lines of scrollback. Cannot be combined with `--full`.

**GCP identity header.** When the Hub has recorded a GCP identity for the agent, `scion look` first prints one line naming it: the metadata mode, the assigned service account and the agent's profile, for example `GCP identity: assign as "Build worker" (profile: gke)`. The account is shown by its registered display name, or by its email when it has no display name or you cannot read the project's service account registrations. `block` and `passthrough` show the mode only. The line goes to stderr, so stdout still carries only the terminal output for scripts that parse it. No line is printed when no identity is recorded or the agent cannot be read.

### `scion list` (or `ps`)

Lists all agents and their status.

**Usage:** `scion list [flags]`

**GCP identity.** With `--format json`, each agent carries a `gcpIdentity` object when the Hub has recorded one: `mode` (`block`, `passthrough` or `assign`) and, for `assign`, `serviceAccountId`, `serviceAccountEmail` and `displayName` (the registered display name, omitted when you cannot read the project's service account registrations). The table output has no identity column.

`scion list` takes no positional arguments; passing one is an error. To name a reference agent for `--descendants`, `--ancestors`, or `--lineage`, use `=` (for example, `--descendants=foo`, not `--descendants foo`).

- **Flags:**
    - `-a, --all`: Show all agents (including stopped ones).
    - `-r, --running`: Filter for active (running) agents.
    - `--phase <phase>`, `--activity <activity>`, `--template <name>`, `--label <key=value>` (repeatable): Filter by attribute. Combine with each other using AND.
    - `--owner <user>`: Filter by owner — a user ID, name, email, or the reserved value `me` (a user whose display name is literally "me" cannot be matched by name; use their ID or email). Owner is the direct creator (`createdBy`), which for an agent-created agent is another **agent**, not the human at the root of the tree — `--owner alice` does not include agents created by alice's agents, and `--owner me` when authenticated with an agent token means "agents I directly created."
    - `--broker <name|id>`: Filter by runtime broker name or ID.
    - `--harness <harness-config name>`: Filter by harness-config name.
    - `--descendants[=<agent>]`: List every agent descended from the reference. With no value, the reference is the calling agent in agent mode, or the calling user otherwise (a user's ID is recorded as the creator in its directly-created agents' ancestry, so this still works).
    - `--ancestors[=<agent>]`: List the agents named in the reference's ancestry chain (entries that name a user rather than an agent are skipped). Same reference-resolution rule as `--descendants`. A user reference has no ancestry, so this returns an empty list when the reference defaults to the calling user.
    - `--lineage[=<agent>]`: List the reference's **creation-tree neighborhood** — the reference's direct parent agent, plus everything created (at any depth) from that parent — bounded to the reference's own project. The exact root rule: it roots at itself when it was created directly by a user, has no recorded parent at all, **or its only recorded parent is an agent you cannot list** (without `--all`, that includes a parent that is merely in a different project — the lookup for a length-one ancestry goes through the current project's endpoint, not the global one). For a deeper ancestry (two or more recorded ancestors), the direct parent is always used as the root even if you cannot see it yourself, so its other children (your siblings) that you *can* see are still listed — this is safe because the ancestry length alone already proves that entry is an agent, not a user. Same reference-resolution rule as `--descendants`. For a user reference (e.g. bare `--lineage` in human mode with no agent to infer), there is no project to bound to, so the result is identical to `--descendants` for that user: every agent they've created, at any depth, across whatever project scope is already in effect. **This is a creation-tree query, not a messaging-permission query** — it does not reflect who the reference agent is allowed to message under its message mode (`scion set-message-mode`), which can be a different and smaller set.
    - `--descendants`, `--ancestors`, and `--lineage` are mutually exclusive with each other. All of the above combine with `--phase`/`--activity`/`--template`/`--label` using AND.
    - Without `--all`, every Hub-mode filter above (including `--descendants`/`--ancestors`/`--lineage`) is scoped to the **current project** — a reference agent's ancestors/descendants/lineage in a different project will not appear. From a human shell, `--all` searches across every project you can see. **When authenticated with an agent token, `--all` cannot be combined with `--descendants`/`--ancestors`/`--lineage`**: an agent token can only list agents in its own project, so the command fails with a clear error instead of silently returning nothing. This is keyed on the credential actually in use, not on CLI mode — running inside an agent container while authenticated as a user (OAuth login, or dev auth against a localhost Hub) is not affected.

### `scion delete` (or `rm`)

Deletes an agent, removing its container, home directory, and worktree.

You can name several agents. If any of them cannot be deleted, the command exits 1 after
handling the rest. With `--format json` the result object (`"status": "partial"`, with an entry
per agent) is still printed on stdout, and no separate error message is added.

**Usage:** `scion delete <agent-name>... [flags]`

- **Flags:**
    - `-b, --preserve-branch`: Preserve the git branch associated with the worktree (default: deleted).
    - `--stopped`: Delete all agents with stopped containers.
    - `--cleanup-only`: Remove only the agent's local files: its worktree, its agent directory and, unless `--preserve-branch` is given, its git branch. Use it when a delete went through on the Hub but the local cleanup failed. It refuses while the Hub still has the agent (run `scion delete <agent-name>` instead); an agent the Hub no longer has, or has only soft-deleted, is cleaned up. When the Hub cannot be checked (unreachable, or no Hub in use) it refuses unless `--force` is given, which removes the local files without checking. Cannot be combined with `--stopped`. When a Hub delete succeeds but its local cleanup fails, `scion delete` prints the `--cleanup-only` command to retry.
    - `--self`: Allow deleting the current agent. Inside an agent container, if one of the named agents is the agent the CLI runs in, `scion delete` refuses, deletes none of the named agents, and exits 1 with a message that `--self` is required. That agent is matched by name, slug or agent ID within its own project. Deleting the current agent removes its files and branch and ends its session. With `--self` the delete goes ahead as usual; to use it after a refusal, re-run the same command with `--self` added. Other agents are deleted as usual. Outside an agent container `--self` has no effect. Also applies with `--cleanup-only`.
    - `-f, --force`: Remove the agent from the Hub even when its Runtime Broker cannot be reached or cannot resolve it. A forced delete is permanent: it skips soft-delete retention, so the agent cannot be restored. Runtime resources on the Runtime Broker (containers, worktrees) may need separate cleanup on that Runtime Broker. `--force` does not purge an agent that is already soft-deleted. Applies to every named agent; it cannot be combined with `--stopped` (name the agents to force-delete instead). `--force` does not override a Runtime Broker that holds a different run of the agent than the Hub recorded: the Hub refuses that delete with `409 conflict` and keeps the agent (see [`DELETE /agents/:id`](/scion/reference/api/)).

The Hub may answer `202` when teardown is still running (see [`DELETE /agents/:id`](/scion/reference/api/)). The CLI then polls the agent every 2 seconds for up to 180 seconds. It removes the local worktree only after the Hub confirms the delete:
- **Delete failed:** the worktree is kept and the command exits non-zero.
- **Poll timed out or the agent can't be read (for example a `403`):** the worktree and the sync state are kept. The command prints that removal is pending and exits 0.
- With `--format json`, a pending delete reports `"status": "accepted"` and `"worktreeKept": true` (`scion stop --rm` reports `"removalPending": true`).

### `scion sync`

Synchronizes the agent workspace between the host and the container.

**Usage:** `scion sync to|from <agent-name> [flags]` (agent workspace; the direction is required), or `scion sync [push|pull]` (project workspace on the Hub)

- **Flags:**
    - `--dry-run`: Preview changes without syncing.
    - `--exclude <glob>`: Exclude files matching the pattern.

### `scion reset-auth`

Injects a fresh Hub token into a **running** agent's container and signals it to reload, without
restarting the agent. Use this to recover an agent whose token expired and cannot self-refresh
(e.g. after a Hub signing-key rotation). Requires a Hub connection. The same action is available
as a **Reset Auth** button in the web UI. The token is passed to the container over stdin, not on the
command line, so it does not appear in the host's process list.

**Usage:** `scion reset-auth <agent-name>` or `scion reset-auth --all --reissue-scopes [--apply]`

- **Flags:**
    - `--reissue-scopes`: Instead of only refreshing the token, re-issue the agent's role and scopes from
      its delegating agent or user's current authority. The agent's delegation is re-recorded through the
      same checks agent creation applies, scopes the delegator no longer holds are removed, the agent's
      current credentials are revoked, and a running agent receives a new token. Requires a Hub
      super-admin session; the operation is audited. Descendant agents are not changed: re-issue them
      one by one, parents first, or use `--all`.
    - `--dry-run`: With `--reissue-scopes` for one agent, show the change without applying it.
    - `--all`: With `--reissue-scopes`, re-issue every agent on the Hub, parents before
      children within each project. Takes no agent argument. A failure affects only that agent, and
      re-running is safe: agents already re-issued are unchanged. This is a dry run that prints a summary
      unless `--apply` is given.
    - `--apply`: With `--all --reissue-scopes`, apply the re-issue instead of a dry run.

```bash
# Preview, then apply, a re-issue for one agent
scion reset-auth my-agent --reissue-scopes --dry-run
scion reset-auth my-agent --reissue-scopes

# Review the Hub-wide summary first, then apply it
scion reset-auth --all --reissue-scopes
scion reset-auth --all --reissue-scopes --apply
```

- **Flags:**
    - `--reissue-scopes`: Re-issue the agent's role scopes from its delegator's current authority
      instead: the agent's delegation record is re-recorded, its credentials are revoked, and a new
      token is pushed to a running agent. Hub super-admins only; audited.
    - `--dry-run`: With `--reissue-scopes`, show the change without applying it.
    - `--all`: With `--reissue-scopes`, re-issue every agent on the Hub, parents first (no agent
      argument). A dry run unless `--apply` is given.
    - `--apply`: With `--all --reissue-scopes`, apply the re-issue.

See [Reset Auth and Scope Re-issue](/scion/reference/reset-auth/).

### `scion reincarnate`

Migrates an agent to a fresh **generation**: it stops the agent, re-resolves its configuration
against the current template and harness-config catalog (template, image, harness config, model,
env keys), and starts it again with the **same** agent ID and slug. The new generation's first task
is a Hub-built preamble plus the handoff you provide. Requires a Hub connection.

The Hub accepts the request with `202 Accepted` and completes the migration in the background. If
the new generation cannot be provisioned, the Hub restores the previous generation's configuration.

While the migration is in progress, messages to the agent are saved to its history rather than
delivered or dropped (the send returns `202` with status `deferred`); scheduled messages fail
instead. The new generation's preamble tells it to catch up with `scion conversation catch-up`.

Run it with no argument inside an agent container to migrate the agent itself (self-migration).
Self-migration requires `--handoff-file`, because there is no one else to describe the work in
progress. When migrating another agent, the handoff is optional. Run
`scion reincarnate --handoff-template` to print the expected handoff sections (role charter,
active work, canonical files, live conversations, child agents, pending waits, and so on); this
flag is local and offline, so it works without a Hub connection and from inside an agent container.

The new generation's preamble names who requested the migration. While a migration is in progress,
the agent's status message reads "migrating to generation N".

An interrupted migration is not resumed. For example, if the Hub restarts in the middle of a
migration, the Hub marks it failed and puts the agent in the `error` phase with the reason in its
status message. Run `scion reincarnate` again with the same flags: the Hub does not keep a failed
migration's patch flags. A migration that had not started yet fails within about 10 minutes with
"reincarnation failed: did not start, so no changes were applied; run reincarnate again". None of
its patch flags are applied. A migration that had started fails after 30 minutes without progress.
The Hub restores the previous configuration unless the new generation had already been provisioned.
Until the migration fails, a new request for the agent is refused with `409`.

Reincarnation works for agents in clone-per-agent, shared-workspace (shared-plain), and
Hub-managed workspaces. For a shared-workspace agent, the agent record, identity, and shared
checkout are preserved, and sibling agents sharing the checkout are not restarted. Agents in
worktree-per-agent projects are not yet supported; the Hub rejects the request with
`400 Bad Request`. An agent in an empty-per-agent project can be reincarnated on its current
Runtime Broker when it runs on a local-disk runtime (Docker, Podman, or Apple `container`): the new
generation reuses the agent's private workspace directory in place, with its content. Reincarnation
never creates or recreates that directory; if it is missing, or is not a real directory, the
Runtime Broker refuses the reprovision, the reincarnation fails, and the agent stays stopped. On Kubernetes or any other runtime the Hub rejects the
request with `400 Bad Request`, and a Runtime Broker too old to reuse the workspace gets
`412 Precondition Failed`; in both cases the agent is not stopped, and `--dry-run` reports the same
answer. Reincarnating another agent requires the `agent.lifecycle` permission (the same
as stop, start, and restart); an agent can always reincarnate itself.

**Usage:** `scion reincarnate [agent-name] [flags]`

- **Flags:**
    - `--handoff-file <path>`: File whose content becomes the new generation's first task. Required for self-migration.
    - `--handoff-template`: Print the handoff template and exit. Ignores other flags and arguments, and does not contact the Hub.
    - `--dry-run`: Print the resolved plan (old → new template, image, harness config, model, env key names, and branch; for legacy agents created before `createInputs` was recorded, any remaining inline env keys other than `SCION_AUTO_EXPOSE_PORTS` and adopted `TZ` are kept as explicit with a warning in the plan) without migrating anything.
    - `--broker <name|id>`: Move the agent to another Runtime Broker (see [Moving to another Runtime Broker](#moving-to-another-runtime-broker) below). Both Runtime Brokers must mount the same NFS export, so the workspace moves without being copied (`transferMode: "export"`). The CLI dry-runs the move first and stops if it is refused; add `--dry-run` to only check it.
    - `--force`: With `--broker <name|id>`, check a forced move that recreates a clone-per-agent or empty-per-agent workspace on the target (`transferMode: "recreate"`) when the workspace cannot travel through a shared NFS export, waiving the export checks and printing what is kept, carried, recreated, and lost. Currently available with `--dry-run` only (executing a forced move that recreates the workspace returns `501 Not Implemented`).
    - `--service-account <id|email|name>`: Patch the GCP service account of the new generation. Accepts the same forms and gets the same access checks as `scion create`.
    - `--role <role>`: Patch the agent role of the new generation: `none`, `readonly`, `baseline`, or `full`. Gets the same access checks as `scion create`; an agent reincarnating itself can lower its own role but not raise it.
    - `--model <model>`: Patch the model of the new generation. Model aliases are accepted, as with `scion start`.
    - `--harness-auth <method>`: Patch the harness auth method of the new generation: `api-key`, `oauth-token`, `auth-file`, or `vertex-ai`.
    - `--image <image>`, `-i`: Patch the container image of the new generation.
    - `--thinking-level <value>`: Patch the thinking level of the new generation. Accepts the same values as `scion start`: an integer from 0 to 100, or a case-insensitive shorthand: `low` (25), `medium` (50), `high` (75), `max` (100). The Hub receives the integer. Without the flag, the thinking level is not patched. An invalid value fails before contacting the Hub.
    - `--shared-dir-backend <name>=<backend>`: Change the recorded [storage backend](/scion/reference/server-config/#changing-an-agents-shared-directory-to-nfs) of the agent's shared directory `<name>`: `nfs` moves it from `local` to `nfs`, and `local` [moves it back](/scion/reference/server-config/#changing-an-agents-shared-directory-back-to-local). Repeatable. Only the agent's record changes; copy the data first. An agent cannot use this on itself, and it cannot be combined with a move to another Runtime Broker. Before a real reincarnation the CLI asks the Hub for the plan and stops if the Hub does not echo the change; if it says the Hub does not support `--shared-dir-backend`, upgrade the Hub. If the next start refuses an empty directory, copy the data and run `scion start` again.
    - `--allow-empty-shared-dir`: With `--shared-dir-backend`, let the agent start even if the directory on the new backend is empty while the previous one is not.

**Patch flags.** `--service-account`, `--role`, `--model`, `--thinking-level`, `--harness-auth`, and
`--image` change that setting on the new generation, and later reincarnations keep the new value.
`--dry-run` shows the old and new value of each patched setting. For a user, patch flags need
permission to update the agent (`agent.update`) as well as `agent.lifecycle`, so a user access token
needs the `agent:update` scope for them; otherwise sign in with `scion hub auth login`. An agent caller, including an agent
patching itself, needs the agent lifecycle permission. Before a real patched reincarnation the CLI dry-runs
the same request, and stops if the Hub does not apply the patch (upgrade the Hub). Patch flags combine
with `--broker`: the move's checks judge the patched configuration, for example the patched service
account.

To reincarnate another principal's agent you must be able to delegate the agent's role. The agent
keeps its existing delegator unless you change its role with `--role`; then you become its recorded
delegator (refused with `403` if you descend from the agent, since that would close a delegation
loop). If you are an agent, the agent then depends on your delegation chain, so prefer a user for role
changes on long-lived agents.

One exception repairs agents left without recorded provenance, for example after a hub upgrade. When
a user reincarnates an agent without changing its role, and the agent's own delegation edge is
missing or has no recorded provenance, or an edge further up its chain has no recorded provenance
and every edge between it and the agent is accepted on this hub, the user becomes the agent's
recorded delegator. This clears the
`ceiling_unrecorded` denial, which blocks service-account assignment among other actions. If the
agent has an assigned GCP service account, the same reincarnation also records the user as the source
of that assignment, provided the user passes the same access checks as `scion create --service-account`
for the account. If the user does not, the reincarnation repairs the delegation all the same, the agent
keeps its existing assignment, and the plan carries a warning. The
**Reincarnate** button in the web UI does the same. A reincarnation by the agent itself or by another
agent keeps the edge, and so does a user's reincarnation of an agent whose chain is fully recorded,
or whose first problem walking up from the agent is an edge this hub does not accept (for example
one recorded with local development credentials on a hub without dev auth); recreate such an agent.
An agent whose chain has a missing ancestor edge or a loop is suspended, and a reincarnation of it is
refused with `409`.

An agent whose delegator was changed by an earlier reincarnation by another agent (for example, one
that now gets `403` when creating agents) is not repaired by this rule, because its chain is
recorded; recreate it. Two role changes by a signed-in user (for example to `baseline` and back) also
re-point its edge to that user, but recreating is recommended. A caller who cannot delegate the role, for
example a non-admin reincarnating an agent with a privileged role, gets `403` from this authority
check. It runs before the workspace and capability checks (`400`, `412`), so expect the `403` first.

#### Moving to another Runtime Broker

`scion reincarnate <agent> --broker <name|id>` moves the agent to another Runtime Broker that mounts
the same NFS export, keeping its ID, slug, generation chain, and workspace. The full requirements,
permissions, and operator notes are in
[Moving an agent to another Runtime Broker](/scion/hosted/ha/multi-broker/#moving-an-agent-to-another-runtime-broker).

The CLI first sends the same request as a dry run. The Hub runs eleven checks in this order:
`runtime_target`, `workspace_mode`, `workspace_storage_reported`, `same_export`, `workspace_on_export`,
`workspace_recreatable`, `target_profile`, `target_health`, `access`, `capability` (on both Runtime
Brokers, with the source online), and `capacity`. The first failing check decides the answer, with `400`, `403`, `409`, `412`,
`429`, or `503`, and the CLI prints the transfer mode of an eligible move (`export` or `recreate`), every check (`passed`, `failed`, `not_evaluated`, or `waived`), and the move's `consequences` (`kept`, `carried`, `recreated`, and `lost`). When a move without `--force` fails only because the workspace cannot travel through a shared export and a forced move onto the target would apply, the CLI also prints a hint pointing at `--dry-run --force`. A refused move
stops there, before anything changes. With `--dry-run` (or `--dry-run --force`) the CLI only prints the verdict and plan.

When the checks pass, the Hub accepts the move with `202 Accepted` and runs it in the background. It
stops the agent on the source, moves its quota to the target, provisions it on the target (which first
confirms through its own mount that the workspace is there), starts the new generation with the
preamble and handoff, and then removes the agent's local state from the source. A failure in these
steps does not come back to the CLI, which already has its `202`: the agent goes to the `error`
phase, and its status message gives the reason. A failure after the agent is assigned to the target
and before the new generation is running rolls the move back: the agent is left stopped on the
source, in the `error` phase, with its previous configuration and workspace, and you can start it
again or retry. If the target's start fails in a way that may have left a container, the agent stays
on the target in the `error` phase instead.

A target you cannot see is reported as not found (`404 runtime_broker_not_found`), and a name that
matches more than one Runtime Broker returns `409 runtime_broker_ambiguous` (use the ID). A target that
is the agent's current Runtime Broker is a plain reincarnation. If the CLI says the Hub does not
support `--broker`, upgrade the Hub.

A self-migration with `--broker` follows the same contract as any self-migration: the CLI prints that
the container will be stopped shortly, and does not set a status such as `blocked`. An agent can move itself
only to a Runtime Broker that already serves its project.

## Configuration & Workspace

### `scion project`

Manages the Scion workspace (Project). `scion projects` is an alias of the group (as is `scion group`), so `scion projects list` and `scion projects use` work too.

- `scion project init`: Initialize a new project. By default, creates a `.scion` directory in the current directory or the root of the current git repository.
    - Flags:
        - `--global`: Initialize the global project in the home directory.
        - `--machine`: Perform full machine-level setup (seeds harness-configs, templates, settings).
        - `--image-registry <string>`: Configure the container image registry path (e.g., `ghcr.io/myorg`).
    - **Note:** If you are in a git repository, add `.scion/agents` to your `.gitignore` to avoid issues with nested git worktrees: `echo ".scion/agents" >> .gitignore`
    - **Hub Integration:** When a hub answers, `init` links the new project to it: without a prompt on the local workstation hub, after a prompt on a remote hub. When no hub is configured or the local server is not running, `init` still succeeds and prints a note pointing at `scion server start`; the project then links on its first hub command.
- `scion project use [project]`: Show or select the current project: a project on a Hub that commands target when they run outside a scion project directory. `scion use` is an alias with the same flags. The selection is saved per user in `~/.scion/context.yaml`; flags, environment variables and the project of the current directory take precedence over it. See [Working with Several Hubs](/scion/hosted/user/multiple-hubs/).
    - `scion use`: Show the effective Hub and project, where each comes from (`--hub flag`, `SCION_HUB_ENDPOINT` (or `SCION_HUB_URL`), `directory link`, `current context`; for the project `--project flag`, `directory: <path>`, `current context`, `global project`; or `none`), and how the CLI would authenticate. When the current directory overrides the saved context, a `Saved context (applies outside scion project directories): <hub> / <project>` line follows. Reads local state only. With no Hub it prints `No hub selected. Add one with 'scion hub add <name> <url>' or start a local hub with 'scion server start'.` and exits 0. `--format json` prints `effective` (`hub`, `project`, `auth`), `context` and `overridden`.
    - `scion use <project>`: Select a project and the Hub it lives on; that Hub becomes the current Hub. `<project>` is a slug, name, UUID or git URL, as for `--project`. It is looked up in the known-project list first, and on the registered Hubs when it is not known there or with `--refresh` (5 seconds per Hub; with `--hub`, or with `SCION_HUB_ENDPOINT`/`SCION_HUB_URL` set, only that Hub). When several Hubs have the project, an interactive run asks you to choose; without a terminal, with `--non-interactive` or in agent mode, the command fails with `project '<ref>' exists on several hubs; choose one with --hub:` and one `scion use <ref> --hub <hub>` line per candidate (JSON: `{"error": "ambiguous", "candidates": [...]}`). `--yes` never picks one. When no Hub has it, the error is `project '<ref>' not found on hubs: <hubs>`, or, when some Hubs could not be searched, `project '<ref>' not found on the hubs searched (<hubs>); it may be on the hub that could not be searched (<hub>: not logged in)`, followed by `Log in with: scion hub auth login --hub <hub>` for each Hub you are not logged in to. Inside a project directory, the selection is saved and a note says the directory's project takes precedence there.
    - `scion use --hub <hub>`: Make `<hub>` (a nickname or URL) the current Hub and restore the project last selected on it. A URL not in the hub list is added with a nickname derived from its host. No network calls.
    - `scion use --clear`: Forget the current project on the current Hub; the Hub stays current.
    - Flags: `--refresh` (look the project up on the Hubs even if the known-project list has it), `--clear`, `--force` (replace an unreadable `~/.scion/context.yaml`).
    - Not available inside an agent.
- `scion project list` (alias `ls`): List the projects on all registered hubs, each with its hub, merged with the project directories on this machine (a linked directory shows its local path; an unlinked one shows hub `-`). Columns: `CURRENT HUB PROJECT NAME LOCAL PATH LAST USED`; `CURRENT` marks the project selected with `scion use`. Offline unless `--refresh` is given.
    - Flags: `--hub <hub>` (only that hub's projects; a nickname or URL), `--refresh` (fetch the project list of each logged-in hub, or of `--hub`, first; hubs that are not logged in or do not answer keep their cached rows and are named in one `Warning: not refreshed:` line), `--local` (list the project directories on this machine instead, with their type, agent count, status and workspace path; cannot be combined with `--hub` or `--refresh`).
    - Scripts that parse `scion project list` should use `scion project list --local`, which keeps the earlier output, JSON included.
- `scion project use [project]` (alias `scion use`): Show or select the current project, the Hub project that commands target when they run outside a scion project directory.
    - `scion use` shows the effective Hub and project and where each came from.
    - `scion use <project>` selects the project and the Hub it lives on. `<project>` is a slug, name, UUID or git URL, as for `--project`. It is looked up in the known-project list (`scion project list`) first, and on the registered Hubs when it is not known there or with `--refresh`. Only Hubs with a usable login are searched. The others are named in a warning, or in the error when no searched Hub has the project (in a terminal, when exactly one Hub was skipped for want of a usable login, you are offered a login). When `--hub`, `SCION_HUB_ENDPOINT` or `SCION_HUB_URL` is set, only that Hub is searched. When several Hubs have the project, you pick one; without a terminal, or with `--non-interactive`, the command fails and lists the `--hub` value of each candidate.
    - `scion use <project> --hub <hub>` selects the project on that Hub. `scion use --hub <hub>` makes the Hub current (a nickname or URL) and restores the project last selected there. A Hub URL that is not in the hub list yet is added with a nickname derived from its host (`local` for the local workstation Hub).
    - `scion use --clear` forgets the current project; the Hub stays current.
    - Flags: `--clear`, `--refresh`, `--force` (replace an unreadable `~/.scion/context.yaml`).
    - The selection is saved per user in `~/.scion/context.yaml`. Flags, environment variables and the project of the current directory take precedence over it.
- `scion project prune`: Detect and remove project configurations whose workspace directories no longer exist, or whose recorded workspace is itself a scion `.scion` directory. This stops any running containers associated with orphaned projects before cleaning up. A git project configuration with no agents and no recorded workspace path shows as `unverified` in `scion project list --local` and is never removed.
- `scion project status [project]` (alias `health`): Show agent status for a project from the Hub: counts per lifecycle phase and per activity, a per-agent table (template, harness, phase, activity), and troubleshooting hints for blocked, stalled, or errored agents. Uses the current project when no name is given.
    - Flags: `--all` (report across all projects on the Hub), `--json` (JSON output).
- `scion project service-accounts` (alias `sa`): Manage GCP service accounts registered for the project.
    - `add <email>`: Register an existing GCP service account.
        - Flags: `--gcp-project <id>` (required, the GCP project ID), `--name <string>` (display name).
    - `mint`: Create a new service account in the Hub's GCP project (the account ID is prefixed with `scion-`). Flags: `--account-id`, `--name`.
    - `list` (alias `ls`): List registered service accounts. The `MAPPED` column shows how many of the project's Kubernetes broker profiles map the account, out of those whose broker reported its mappings (for example `1/2`), or `-` when no profile reported. Flags: `--json`.
    - `show <id|email|name>` (aliases `get`, `describe`, `status`): Show the account as seen from this project, in sections: identity, verification (with its age), mapping per Kubernetes broker profile (`mapped`; `not mapped`, only on a complete, current-version report under 15 minutes old; `unknown` with the reason (report stale, report incomplete, report too old a version, or no stored report); or `not reported`; followed, when the broker reports them, by the Kubernetes service account, namespace and source (`mapped` or `discovered`), an `ambiguous` marker, a `report incomplete` marker with its reason, and the report's age; mappings are owned by the broker and shown read-only), Workload Identity binding (always `unknown (not checked)`: the Hub does not read IAM policy here), the defaults that point at it (project, per-profile, Hub), the agents using it that you may see, and the next step (not verified, not mapped on a named profile, or nothing missing). The account is resolved as for `scion create --service-account`; use the id or the email for a name that contains `/`. Available in agent mode (the other `project service-accounts` subcommands are not). Flags: `--json`.
    - `verify <id>`: Verify that the Hub can impersonate the service account.
    - `remove <id>` (aliases `rm`, `delete`): Remove a service account registration. The account is not deleted in GCP, and a minted account is kept there. If a project or per-profile default points at the account, the Hub refuses. The command then prints what references the account (defaults, agents, broker profiles that map it) and exits non-zero. If the hub default points at it, a hub admin must change that default first. Agents that reference the account do not block removal; they fail at their next start. On success the command prints the defaults it cleared and the cleanup the Hub cannot do: the broker mapping, the Kubernetes ServiceAccount, the IAM bindings, and for a minted account the account in GCP. The report names only agents and projects you can see; others appear as counts. A default in a project you cannot change also blocks removal until an admin of that project changes it. `--json` prints the Hub's result or report instead.
        - Flags: `--force` (clear the project and per-profile defaults you may change that point at the account, then remove it; a cleared `assign` default becomes `block`; the hub default is never cleared).
    - `add`, `mint`, `verify` and `list` print the Hub's warnings to stderr, for example a service account that no Kubernetes broker profile of the project maps (see [early warning for unmapped service accounts](/scion/hosted/ha/kubernetes/#gcp-identity-mode-assign-workload-identity-mapping)). Warnings never change the exit status. With `--json`, `add`, `mint` and `verify` also include the warnings in the JSON document as `warnings`; `list --json` prints only the account list.
- `scion project identity default`: Show or change the default GCP identity the Hub gives a new agent when the create request names none. Not available in agent mode.
    - `show`: Print the ladder the Hub walks, in order, with where each rung is set: the request (`--service-account` at create), the per-profile defaults (one line per broker profile), the project default, the hub default (`set`, `not set`, or `unknown` when the Hub cannot tell), and the broker's runtime default (`block`, or `passthrough` on Kubernetes). Accounts are shown by id, with the display name when visible. Flags: `--json`.
    - `set <id|email|name|passthrough|block>`: Set the project default. An account sets the mode to `assign`; `passthrough` or `block` sets that mode and clears the stored account. A bare `assign` is refused, because it needs an account. To name an account whose display name is `passthrough`, `block` or `assign`, use its id or email.
        - Flags: `--profile <name>` (set the default account for agents running under that broker profile instead; the value must be an account, and other profiles' entries are kept). Per-profile writes read the stored map and write it back whole, and the Hub has no conditional write, so two people changing per-profile defaults at the same time are last-writer-wins.
    - `unset`: Clear the project default (mode and account), so new agents fall through to the hub default.
        - Flags: `--profile <name>` (remove only that profile's default account).
    - An account may be named by id, email or display name. The Hub resolves it as for `scion create --service-account`, and the command stores the account id, so the default keeps working after a rename. A reference that matches more than one account (the error lists the candidate ids and scopes) or none is refused before anything is written. Each write sends only the identity fields it changes, so every other project setting keeps its stored value. The Hub checks the account as for any project default: it must be available in the project and verified. Changing a default needs the `project.update` permission (the `project-owner` and `project-admin` roles); on a 403 the command prints the Hub's message and the role needed. `set` and `unset` print the stored identity defaults after the write (`--json` for JSON).
- `scion project reconnect <new-workspace-path>`: Reconnect a moved workspace to its externalized project configuration. This fixes projects that show as "orphaned" after being relocated.
- `scion project skills`: Manage auto-injected skills for the project.
    - `list [project]` (alias `ls`): List auto-injected skills configured for the current project (or a specified project).
    - `add [project] <uri>`: Add a skill URI to the project's auto-injected list.
        - Flags: `--as <alias>` (alias under which to mount the skill), `--optional` (continue provisioning if resolution fails), `--from-directory <url>` (discover and batch-add all skills from a GitHub repository directory).
    - `remove [project] <id|uri>` (aliases `rm`, `delete`): Remove an auto-injected skill entry from the project by its ID or full URI.
- `scion project hook` (alias `psh`): Manage project-scoped pre-start hooks. These shell scripts run inside the container during agent initialization, and abort agent startup on failure.
    - `list [project]` (alias `ls`): List pre-start hooks for a project.
    - `show <id-or-slug> [project]`: Show details and script content of a pre-start hook.
    - `create [project]`: Create a new hook (archives the current active hook).
        - Flags: `--name` (required, human-readable name), `--script` (required, path to shell script, or `-` for stdin), `--slug` (url-safe identifier), `--description` (optional description).
    - `update <id-or-slug> [project]`: Update an existing pre-start hook.
        - Flags: `--name`, `--script`, `--description`.
    - `activate <id-or-slug> [project]`: Mark an archived hook as active (archives any currently active hook).
    - `delete <id-or-slug> [project]` (alias `rm`, `remove`): Delete an archived hook. Active hooks cannot be deleted.
- `scion project messaging`: Manage cross-project messaging policy for a project.
    - `set [project]`: Set the inbound messaging policy.
        - Flags: `--policy <string>` (one of `none`, `members`, `all`), `--revision <int>` (required, optimistic concurrency revision).
    - `get [project]`: Show the current messaging policy and revision.
- `scion project visitors`: Grant users of a trusted issuer visitor access to a project by email or by email domain (experimental: needs the `federation.project_visitors` experiment, an issuer with `allow_visitor_grants`, project owner or admin or hub admin, and an interactive session; granting and extending also need the `project-visitor` permissions on the project). Not available in agent mode.
    - `list`: List the project's visitors with email, issuer, status (`pending` until the visitor's first request, then `active`, or `suspended`; marked `(issuer not accepted)` while the Hub does not accept visitors from that issuer), source (`email`, or the domain of the visitor rule that added them) and expiry, then the visitor rules with how many visitors each has added. `--json` prints `{"visitors": [...], "rules": [...]}`.
        - Flags: `--json`.
    - `add`: Grant visitor access by email, or with `--domain` add a visitor rule: every user of the issuer whose verified email is in exactly that domain (no subdomains) becomes a visitor on their first request. A rule replaces an expired or not yet started grant by email, never an active one; a person who is a visitor through a rule is granted by email only after `remove`.
        - Flags: `--issuer <url>` (required, a trusted issuer URL), `--email <email>` or `--domain <domain>` (exactly one), `--expires <duration>` (optional, for example `30d`), `--json`.
    - `remove <email or id>` or `remove --domain <domain>`: Revoke a visitor's access (the next request is refused; a visitor a rule added is not added again by that rule), or remove a visitor rule and every visitor it added (a rule can also be named by the rule ID `list` shows). Visitors granted by email keep their access when a rule is removed.
        - Flags: `--issuer <url>` (when the email is a visitor, or the domain has rules, from more than one issuer), `--domain <domain>`.
    - `extend <email or id>`: Set a new expiry.
        - Flags: `--expires <duration>` (required), `--issuer <url>`, `--json`.
    - Flags: `--project <id or slug>`.

### `scion service-accounts`

Manage GCP service accounts at an explicit scope: the current project's accounts by default, or hub-scoped accounts with `--global`. Subcommands: `list`, `show <id>`, `verify <id>`, `add <email>`, and `remove <id>`.

- `remove <id>` (aliases `rm`, `delete`): Remove a registration, with the same impact report, refusal and `--force` behaviour as [`scion project service-accounts remove`](#scion-project). Any project's default can block removal of a hub-scoped account. `--force` clears those defaults but never the hub default.

### `scion user`

Manages per-user Hub settings.

- `scion user skills`: Manage auto-injected skills for your user across all projects.
    - `list` (alias `ls`): List your personal auto-injected skills.
    - `add <uri>`: Add a skill URI to your personal auto-injected list.
        - Flags: `--as <alias>` (alias under which to mount the skill), `--optional` (continue provisioning if resolution fails), `--from-directory <url>` (discover and batch-add all skills from a GitHub repository directory).
    - `remove <id|uri>` (aliases `rm`, `delete`): Remove an auto-injected skill entry from your personal list by its ID or full URI.
    - Changing your injected skills with a user access token requires the `user_skill_injection:update` scope on the token.

### `scion secret`

Manages project-scoped secrets from the host or within an agent container. This command mirrors the `sciontool secret` commands used in agent containers, using the `hubclient` Secrets service to operate on the active project.

Unlike `scion hub secret` (which supports managing secrets at any scope: user, hub, project, or broker), `scion secret` is streamlined for project-level secrets within your current project context.

- `scion secret set KEY VALUE`: Store a project-scoped secret in the Hub.
    - If `VALUE` starts with `@`, the remainder is treated as a file path. The file contents are read and base64-encoded, and `--type` defaults to `file`.
    - Flags:
        - `--type <string>`: Secret type: `environment` (default), `variable`, or `file`.
        - `--target <string>`: Injection target path (defaults to key for env, required for file-type secrets).
        - `--allow-progeny`: Allow child agents (progeny) to inherit this secret.
- `scion secret get KEY`: Retrieve the metadata of a project-scoped secret. Secret values are never returned to protect security.
- `scion secret list`: List metadata (key, type, version, updated time) for all project-scoped secrets. Secret values are never returned. With `--json` or `--format json`, prints the same list shape as `scion hub secret list`.

### `scion clean`

Removes the scion project configuration from the current project or global location.

**Usage:** `scion clean [flags]`

- **Flags:**
    - `--skip-hub-check`: Skip Hub connectivity check before removing.

In a git repository, `scion clean` removes the repository's `.scion` but leaves the project's external configuration under `~/.scion`. Once that configuration has no agent directories left (delete the agents first with `scion delete`), it shows as orphaned in `scion project list --local` and `scion project prune` removes it. A configuration that still holds agent directories is never pruned. The `.gitignore` entries that scion added are left in place.

### `scion config`

View and modify configuration settings.

- `list`: List all effective settings and where each comes from.
- `get <key>`: Get a specific configuration value (`--source` adds where it comes from).
- `set <key> <value>`: Set a configuration value.
- `unset <key>`: Remove a setting so it takes its value from the layer below.
- `validate`: Validate settings files against the schema.
- `migrate`: Migrate configuration to the latest versioned format.
- `dir`: Print the path to the active configuration directory.

`config list` shows every key `config set` accepts, the hub transport settings
(`hub.transport.mode`, `hub.transport.audience`) and each hub connection's endpoint
(`hub_connections.<name>.endpoint`), with the effective value and its source: `default`
(the embedded defaults), `global <file>`, `project <file>`, `env <VARIABLE>` or
`project-id file <file>`. A setting with no value shows `<unset>`. `config get <key>`
reads the same settings and gives the same value and source. The value of a
credential-like key (for example `hub.brokerToken`) is masked in both. With
`--format json`, `config list` prints an object keyed by setting
(`{"<key>": {"value": "...", "source": "..."}}`) and `config get` prints
`{"key": "...", "value": "...", "source": "..."}`.

`config get` supports those keys (including `cli.mode`) plus dotted paths into a named
entry of the `profiles` or `runtimes` maps: `profiles.<name>.<field>` and `runtimes.<name>.<field>`, where `<field>` is one of
that entry's scalar settings named by its `settings.yaml` key (e.g.
`profiles.local.runtime`, `runtimes.kubernetes.namespace`). Within a profiles/runtimes
entry, structured fields (maps, lists, nested objects — e.g. `env`, `volumes`,
`secrets`, `harness_overrides`) and credential-like field names are not supported and
return an error rather than a partial, reformatted, or unmasked value.
`harness_configs.<name>.<field>` is not supported: harness configs are normally
resolved from on-disk harness-config directories, not merged into `settings.yaml`;
use `scion harness-config` to inspect them instead.

`config set` writes the project's own settings, or the global settings
(`~/.scion/settings.yaml`) with `--global`. It refuses a key the settings file
would accept but never read, and leaves the file unchanged:
`hub.token`, `hub.apiKey`, `hub.lastSyncedAt`, `bucket.provider`, `bucket.name`,
`bucket.prefix` and `hub_connections.<name>.<field>` at either layer, and
`server.auth.display_name`, `server.auth.email` and `server.auth.username` at the
project layer (they are read only from the global settings). The error names the key
and does not include the value. After a successful write, the value of a
credential-like key (for example `hub.brokerToken`) is shown masked. `cli.mode`
accepts `human` or `agent`. Agent mode removes the `config` commands, so after
`scion config set cli.mode agent` the way back is
`SCION_CLI_MODE=human scion config unset [--global] cli.mode` (or removing `mode` from
the `cli` section of the settings file by hand); `config set` prints this when it writes
`agent`.

`config unset <key>` removes the key from the project's own settings, or from the
global settings with `--global`, so the setting takes its value from the layers below
instead of, for example, a boolean being written `false`. It accepts the keys
`config set` accepts, under the same rules. A key that is not in the file changes
nothing (a legacy-format file is then not migrated). A YAML file is edited in place,
and a section left empty is removed with the key. An environment variable that sets
the key still applies, and the command warns about it. When a file of the edited
layer still sets the key afterwards (a spelling `config unset` does not edit, or the
in-repo `.scion/settings.yaml` of a project whose settings are kept in its external
settings directory), the command names that file, and `--format json` adds it as
`still_set_in`. `config unset` is available in human mode
only, like `config set`.

### `scion cd-config`

Open a new shell in the active Scion configuration directory.

**Usage:** `scion cd-config`

### `scion cd-project`

Open a new shell in the active project's workspace directory.

**Usage:** `scion cd-project`

### `scion cdw`

Change directory to the workspace of an agent.

**Usage:** `scion cdw <agent-name>`

### `scion shared-dir`

Manages shared directories for agents within a project.

- `list`: List shared directories in the current project.
- `create <name>`: Create a new shared directory.
- `info <name>`: View details about a specific shared directory.
- `remove <name>`: Remove a shared directory (permanently deletes contents).

## Template Management

### `scion templates`

Manages agent templates. `scion template` (singular) is an accepted alias. Scope defaults to the project; add the root `--global` flag to target global templates.

- `list`: List available templates from the Hub and the filesystem, grouped by scope. A failed global-scope Hub list is an error; a failed project or user list is a warning and the rest is still shown.
    - Flags: `--local` (list filesystem templates only; makes no Hub request, so it works offline).
- `show <name>`: Show a template's resolved configuration.
    - Flags: `--local` (search local only), `--hub` (search Hub only). Without `--local`, a Hub error is shown rather than falling back to the filesystem; the same holds for `clone` and `delete`.
- `create <name>`: Create a new template (seeded from the `default` template).
- `clone <src> <dest>`: Clone an existing template (local or Hub source) to a new local one.
    - Flags: `--local`, `--hub` (restrict where the source is searched).
- `delete <name>` (alias `rm`): Delete a template.
    - Flags: `--local`, `--hub`.
- `import <source>`: Import agent definitions (Claude/Gemini sub-agents or Scion templates) into your templates directory.
    - Flags: `--all` (import every discovered agent), `-H, --harness <type>` (force `claude`/`gemini`), `--name <name>` (rename a single import), `--force` (overwrite), `--dry-run` (preview).
- `update-default`: Update the global default template with the latest from the binary.
    - Flags:
        - `--force`: Overwrite the existing default template if it already exists.

Hub-only commands (require an enabled Hub):

- `sync [template]` (alias `push`): Create or update a template in the Hub; only changed files are uploaded. Use `--all` to sync every local template, or `--name <name>` to sync under a different Hub name.
- `pull <name>`: Download a template from the Hub to the local filesystem. Use `--to <path>` for a custom destination.
- `status`: Show the sync status of templates relative to the Hub.

See [Templates & Roles](/scion/local/templates/) for the full guide.

## Skill Bank

### `scion skills`

Manages skills in the Hub skill bank — reusable, versioned instruction snippets referenced by URI (`scion skill`, singular, is an alias). See [Skills — Authoring & Publishing](/scion/local/skills/) for the full guide. All subcommands except `create` require a Hub connection.

- `list`: List available skills.
    - Flags: `--scope <core|global|project|user>`, `--search <text>`, `--tags <a,b>` (repeatable or comma-separated, AND semantics).
- `show <name-or-id>`: Show a skill's details and versions.
- `create <name>`: Scaffold a new local skill directory with a starter `SKILL.md` (local-only; does not publish).
- `publish <path>`: Publish a local skill directory to the Hub. Limits: 50 files, 10 MB/file, 50 MB total.
    - Flags: `--version <semver>` (required), `--scope <core|global|project|user>` (default `global` for new skills), `--skill-id <id>`.
- `versions <name-or-id>`: List all versions of a skill.
- `resolve <uri>`: Resolve a skill URI to a concrete version, content hash, and file manifest.
- `deprecate <name-or-id>`: Mark a published version as deprecated.
    - Flags: `--version <version>` (required), `--message <text>` (required), `--replacement <uri>`.
- `delete <name-or-id>` (alias `rm`): Soft-delete a skill (archived, retained for history).

### `scion skills registries`

Manages external skill registries for [federation](/scion/hosted/single-node/skill-registry/). Admin operations.

- `list`: List configured registries.
- `add <name>`: Register an external skill registry.
    - Flags: `--endpoint <url>` (required, HTTPS), `--trust <trusted|pinned>` (default `pinned`), `--type <hub|gcp>` (default `hub`), `--description <text>`, `--auth-token <token>`, `--resolve-path <path>`.
- `show <name-or-id>`: Show registry details.
- `update <name-or-id>`: Update a registry. Flags: `--endpoint`, `--trust`, `--status <active|disabled>`, `--description`, `--auth-token`, `--resolve-path` (only changed flags are applied).
- `remove <name-or-id>`: Remove a registry.
- `pin <name-or-id> <skill-uri>`: Pin a content hash for a pinned-trust registry. Flag: `--hash <sha256:...>` (required).

## Harness Configuration

### `scion harness-config` (alias `hc`)

Manages harness-config bundles — the named, versioned definitions of each harness (config,
image, capabilities, auth, and supporting files). See
[Harness-Specific Settings](/scion/reference/harness-settings/#managing-harness-configs) for the
full lifecycle.

- `list`: List local harness-configs. Flags: `--hub` (also include Hub-registered configs).
- `show <name>`: Show config details (local path/image, or Hub ID, image status, and source URL).
- `install <source>`: Install a config from a GitHub URL, local path, rclone URI, or archive. The name is `--name`, else the config's `name` field, else its `harness` field, else the source directory name. Flags: `--name` (override derived name), `--force` (overwrite an existing config with the same name in the target scope, locally or on the Hub; without it install refuses), `--global` (install globally / register at global scope on the Hub; default is the current project).
- `update [name]`: Re-import (refresh) a config from its stored source URL. Flags: `--url <url>` (override/set the stored source URL for one config), `--all` (re-import every config that has a stored source URL). `--url` and `--all` are mutually exclusive; requires a Hub connection.
- `sync <name>` (alias `push`): Upload a local config to the Hub (changed files only), creating it or updating the same-named config in the target scope. Scope is the current project by default, or global with `--global`; the output shows the scope used. Flags: `--name` (publish under a different Hub name).
- `pull <name>`: Download a config from the Hub. Flags: `--to <path>` (destination; defaults to the global dir).
- `reset <name>`: Restore a config to the binary's embedded defaults.
- `upgrade [name]`: Add missing support files and metadata to the local copy without clobbering user values. Flags: `--dry-run`, `--activate-script`, `--force` (overwrite every file that ships in the embedded defaults, discarding local edits to them; files you added are kept). With no name, upgrades all configs in the global directory. It does not change the Hub's copy or existing agents; see [Changing a harness config affects new agents only](/scion/reference/harness-settings/#changing-a-harness-config-affects-new-agents-only).
- `delete <name>`: Delete a config from the Hub (does not remove local files). The web UI additionally offers an "Also delete stored files" option.

## Hub Integration

### `scion hub`

Manages connection to and interaction with a Scion Hub. Authentication lives under `scion hub auth` (there is no top-level `scion auth` command).

- `scion hub auth`: Manage Hub authentication.
    - `login`: Authenticate with Hub server (opens a browser; supports `--no-browser` for device flow and `--provider github`).
        - **Hub URL.** Taken from, in order: the root `--hub` flag (a hub nickname or URL), the `SCION_HUB_ENDPOINT` environment variable, then `SCION_HUB_URL`, then the directory's link entry, then the current Hub. This is the order every Hub command uses (see [Hub resolution order](#global-flags)). `--hub-url` is a hidden, deprecated alias of `--hub` that wins over it; it prints a deprecation notice.
        - **Hub list.** A hub URL given with `--hub` or `--hub-url` that is not in the hub list yet is added to it after a successful login, with a nickname derived from its host. A login never makes a Hub current; when the Hub is not the current Hub, the command prints how to select it (`scion use --hub <name>`).
        - **Settings are never written.** When the Hub came from the current context or from the directory's link entry, the command prints `Logged in to <url> (current hub, selected with 'scion use').` or `Logged in to <url> (the hub this directory is linked to).` A Hub taken from an environment variable is not added to the hub list.
    - `logout`: Clear stored credentials. Uses the same Hub URL order. The hub list is not changed.
    - `federation-token`: Obtain a short-lived user access credential that names you to a peer hub, signed with the Hub's OIDC key, and print it. Requires the `federation.project_visitors` [experiment](/scion/reference/experiments/) on the Hub (the Hub answers 404 while it is off or while its OIDC provider is not enabled). Human mode only. A user access token used for this command needs the `federation_token:mint` scope on a hub boundary.
        - Flags:
            - `--audience <url>`: Issuer URL of the peer hub the credential is for (required). It must be listed in the Hub's `server.oidc.user_token_audiences` setting; the credential expires after `server.oidc.user_token_lifetime` (15 minutes by default). See [Server Configuration](/scion/reference/server-config/).
            - `--json`: Print the credential and its expiry as JSON instead of the credential alone.
- `scion hub token`: Manage user access tokens (scoped, revocable bearer tokens for CI/CD and automation).
    - `create`: Create a new token.
        - Flags:
            - `--project <string>`: Project ID or name to scope the token to (required).
            - `--name <string>`: Token name/label (required).
            - `--scopes <scopes>`: Scopes to grant (required). This flag is **repeatable** and also accepts a **comma-separated list** of scopes (e.g., `--scopes agent:read,agent:create --scopes agent:lifecycle`). Strict empty-value validation is enforced.
            - `--expires <duration>`: Expiry: a positive duration in minutes (90m), hours (2h), days (30d) or years (1y, always 365 days), or an RFC 3339 date (2026-12-31T00:00:00Z) (default: 90d). `m` means minutes; there is no month unit (use 30d or 1y for longer).
            - `--purpose <text>`: Optional bounded description of what the token is for (≤128 bytes, single line, no control characters). Immutable after issuance — there is no update command.
            - `--label <key=value>`: Optional bounded label (repeatable). Keys are lowercase `[a-z][a-z0-9_.-]*` (≤32 bytes); values are ≤64 bytes from a restricted charset. A set of attribution-shaped keys (e.g. `user_id`, `agent`, `actor_binding`) are reserved and rejected. Immutable after issuance.
    - `scopes`: List every scope accepted by `create --scopes`. With `--project <string>`, also report which scopes you may currently select for a token scoped to that project and, for each one you cannot, why. Supports `--json`.
    - `list`: List your access tokens.
    - `revoke <token-id>`: Revoke a token (remains visible in listings as revoked).
    - `delete <token-id>`: Permanently delete a token.
- `scion hub status`: Show the current Hub connection status: the Hub's nickname from the hub list (`-` when it has none), the endpoint and where it comes from (the same labels as `scion use`, for example `current context` or `directory link`), the project (the current project when the context supplied it), whether it answers, authentication and where the bearer comes from (`Source`: `OAuth`, `agent-token`, `env`, `dev`, `transport` or `none`; `bearerSource` in `--json`), whether it is the local workstation hub (`Local workstation: yes/no`), and the project's link state (linked, never linked, or unlinked). When a settings file still sets the deprecated `hub.enabled` or `hub.local_only` keys, it prints a note that they are ignored. With a hub profile selected it shows the profile instead: endpoint, bearer source (`profile:issuer` or `profile:<alias>`), the identity (email, kind, tier, expiry) and the transport principal and audience, all decoded locally (no token is printed). It probes the target with the profile credential and exits non-zero when the request sent did not carry the profile's bearer (or carried another credential), when the transport principal is not the profile's IAP service account, or when the target refuses the credential. In agent mode it runs only with a target-hub profile.
- `scion hub notifications`: **Deprecated**. This command has been moved to the top-level `scion notifications` command group.
- `scion hub add <name> <url>`: Add a Hub to the hub list under a nickname (lowercase letters, digits and `-`, starting with a letter or digit, at most 63 characters). Each Hub URL can be added once. A Hub URL must be a plain `http://` or `https://` URL with a host: one with user info, a query or a fragment is refused (`invalid hub URL ...`), and the CLI prints Hub URLs with the credential in any user info replaced by `xxxxx`. The nickname then works anywhere `--hub` takes a URL, including `scion server` and `scion runtime-broker` commands (but not inside agents). The Hub's health is checked, but an unreachable Hub is still added with a warning. Prints the login command when you are not logged in. `--use` also makes it the current Hub. The first `scion` command that reads the hub list imports a `hub.endpoint` from your global settings into it once (named `local` for the local workstation Hub, otherwise after its host), and makes it current when no Hub is current. See [Working with Several Hubs](/scion/hosted/user/multiple-hubs/).
- `scion hub list`: List the Hubs in the hub list, marking the current Hub. Columns: `CURRENT NAME ENDPOINT AUTH`. `AUTH` shows how the CLI would authenticate to each Hub, from local state only: `oauth (<user>)`, `dev` (the workstation dev token, counted only for a Hub on a loopback address), `env token`, or `none`. With no Hubs it prints `No hubs registered. Add one with 'scion hub add <name> <url>'.` `--check` also reports whether each Hub responds (`REACHABLE`) and accepts the CLI's credential for it (`VERIFIED`: `yes`, `no`, or `-` when not checked); each Hub is sent only its own credential, and tokens from the environment are never sent.
- `scion hub rename <old> <new>`: Rename a Hub in the hub list. The current Hub and the projects known on that Hub follow the new name; logins are unaffected.
- `scion hub remove <name>`: Remove a Hub from the hub list, together with the projects known on it. If it was the current Hub, no Hub is current afterwards. The stored login is kept unless you pass `--logout`.
    - `add`, `rename` and `remove` accept `--force` to replace an unreadable `~/.scion/context.yaml`. The hub list commands are not available inside an agent.
    - A background workstation `scion server start` (with the web server on) adds the local Hub to the hub list as `local` (`local-2` and so on if that name is taken; `--foreground` does not add it), and makes it the current Hub when no Hub is current and the global settings name no other Hub. A successful `scion hub auth login` to a Hub URL given with `--hub-url` or `--hub` adds that Hub with a nickname derived from its host (`local` for the local workstation Hub).
- `scion hub link`: Link the current local project to the Hub that applies in that directory (for example the current Hub selected with `scion use`). The project is created on the Hub if it does not exist, or, after a prompt, linked to an existing project with the same name (the repository name from the git remote, or the directory name). With no Hub configured, the command fails with `No hub selected. Add one with 'scion hub add <name> <url>', start a local hub with 'scion server start', or pick one with 'scion use --hub <name>'.` On the local workstation Hub the link confirmation is skipped; a remote Hub still asks (or needs `--yes`). On the local workstation Hub a newly registered git project gets the `worktree-per-agent` mode, so its agents get worktrees of this checkout (see [Linked git projects](/scion/local/workspace/#linked-git-projects)). In a git repository without its own `.scion`, when the project would otherwise fall back to the global project (no `--global` or `--project`), the command refuses: run `scion init` there first, or pass `--global` to link the global project. A `.scion`-less repository inside another scion project links that parent project instead. The prompt to pick among matching projects appears on both. When the project has agents left from before the hub was required that the Hub does not know, `hub link` offers to import them; see [what the import does with each agent](/scion/reference/local-mode-removal/#what-the-import-does-with-each-agent). After linking, the command records a link entry (the hub, the hub project and the directory) in the hub list, so the directory keeps resolving to that Hub whichever Hub is current, and the project appears in `scion project list` with its local path. A Hub that was not in the hub list is added. No `hub.endpoint` is written to the project's settings.
- `scion hub unlink`: Unlink the current project from the Hub locally. It marks the project unlinked (`hub.linked: false`) and clears its Hub project ID; it does not remove the project or its agents from the Hub. Agent commands in an unlinked project refuse with a pointer to `scion hub link`. It also removes the directory's link entry from the hub list; the current context is not changed. Agents that already exist keep running on their broker; manage them with `--project <hub-slug>`, or re-link with `scion hub link`.
- `scion hub projects` (alias `project`): List all projects registered on the Hub. For the projects on all your hubs, use `scion project list`.
    - `create [git-url]`: Create a project on the Hub. With a git URL, the project is anchored to that repository. Without one, a Hub-managed project without git is created; `--name` is then required and `--branch` is not allowed.
        - Flags: `--slug`, `--name`, `--branch`, `--workspace-mode`, `--json`
        - `--workspace-mode <shared|per-agent|worktree-per-agent>`: The [workspace sharing mode](/scion/local/workspaces-and-sharing/#setting-the-mode-on-a-hub-project). On a project without git, `per-agent` gives each agent an empty private directory (empty-per-agent). The mode is set at create time only and cannot be changed later. The Hub validates the value: unknown values, and `worktree-per-agent` without a git URL, fail with the Hub's `400` message.
    - `info [project-name-or-id]`: Show details for a project, given by name or project ID (UUID), including its providers. Each provider shows its Runtime Broker's capacity as `(agents: count/limit)`, or `(agents: count)` when that Runtime Broker has no limit. `(not enforced)` is appended when the hub-wide switch for Runtime Broker quota enforcement is off.
    - `delete [project-name-or-id]`: Delete a project from the Hub, given by name or project ID (UUID), with its broker provider associations. The Hub refuses while the project still has agents that are not deleted (including agents whose delete failed) and lists them; delete those agents first. `--force` deletes the project anyway: its agent records are removed and the Runtime Brokers are asked to delete the agents best-effort. With `--format json` a refusal is reported as `{status: "error", details: {projectId, projectName, agents, truncated}}`. Asks for confirmation unless `--yes` or `--non-interactive` is set. The project is resolved the same way as for `info`, so `info` shows the project that `delete` would remove.
        - Flags: `--yes`, `--non-interactive`, `--force`
- `scion hub shadow` and `scion hub unshadow` have been removed. `scion hub shadow` fails with `'scion hub shadow' was removed; run 'scion use <project>' to work with a hub project from any directory`. Directories it created keep working and print a cleanup notice; see [Moving from `scion hub shadow`](/scion/hosted/user/multiple-hubs/#moving-from-scion-hub-shadow).
- `scion hub brokers`: List all runtime brokers registered on the Hub.
    - `join-token create <broker-name>`: Create the broker if it does not exist, and a single-use join token for it, to redeem on the broker host with `scion runtime-broker join` (see [Headless Registration with a Join Token](/scion/hosted/ha/runtime-broker/#headless-registration-with-a-join-token)). The token is printed on stdout; the instructions go to stderr. For an existing broker, only its owner or a super-admin can create a token, and the new token replaces any unused earlier one. The broker's settings are not changed. Requires the `broker.create` permission, with a sign-in or a hub-boundary user access token carrying `broker:create`; a token issues a token only for a broker its user created.
        - `--ttl <duration>`: How long the token is valid, from `5m` to `24h`. Default: the Hub's default, `1h`.
        - `--json`: Print `brokerId`, `brokerName`, `joinToken`, `expiresAt`, `hubEndpoint` and `reissued` as JSON.
- `scion hub secret`: Manage write-only secrets on the Hub.
    - `set <key> <value>`: Set a secret (supports `--allow-progeny` for user-scoped secrets).
    - `get [key]`: Get secret metadata. Honors `--format json` (as well as `--json`).
    - `clear <key>`: Remove a secret.
    - `migrate`: Move existing secrets from the Hub database to GCP Secret Manager.
        - Flags: `--gcp-project <id>` (required, the GCP project ID), `--credentials <path>` (GCP credentials JSON), `--dry-run`, `--force` (re-migrate secrets that already reference Secret Manager), `--hub-id <id>` (Hub instance ID used to namespace secrets). Works from any directory; no project is required.
    - `migrate-names`: Rename legacy (pre hub-prefix) GCP Secret Manager secrets to the hub-prefixed `scion-<12-hex hub hash>-…` scheme. Idempotent; run a plain pass (or `--dry-run`) first, then a separate `--delete-legacy` pass. Does not require a project directory. See [Secrets](/scion/hosted/user/secrets/) for the IAM and rollout ordering.
        - Flags: `--gcp-project <id>` (required), `--credentials <path>`, `--dry-run`, `--delete-legacy` (delete each legacy secret after verifying its hub-prefixed copy), `--hub-id <id>` (defaults to the resolved server hub ID), `--timeout <duration>` (default `5m`), `-c, --config <path>` (server config file; must match the running hub's so hub ID resolution agrees).
- `scion hub users`: Administer hub users. Not available in agent mode.
    - `provision <email>`: Pre-register a user (status `invited`, the same record an admin invite creates). The person still signs in through a configured sign-in provider; the role is assigned at first sign-in. Running it again with the same details is safe and reports that the user is already pre-registered. Requires the `user.invite` permission (hub admins hold it) and an interactive sign-in; it is not available on a Hub running with dev auth. See `POST /api/v1/users` in the [API reference](/scion/reference/api/).
        - Flags: `--display-name <string>`, `--note <string>`, `--json`.
- `scion hub profile`: Manage target-hub profiles: named, non-secret descriptions of another test hub, stored in `~/.scion/hub-profiles/<name>.yaml` (mode `0600`). Select one with `--hub-profile <name>` or `SCION_HUB_PROFILE`. With a profile selected, the bearer comes only from the profile, never from a login, `~/.scion/scion-token`, `SCION_AUTH_TOKEN` or `SCION_HUB_TOKEN`; a command with no applicable profile credential fails. Only these commands run with a profile: `hub status`, `hub profile`, `hub test-identity` (except `delegate`), `hub projects` (list, `info`, `create`, `delete`; name the project explicitly), `artifact publish`, `get`, `versions` and `delete` (`publish` needs `--project <name or id>` on the target), and `create`, `start`, `list`, `stop`, `delete`, `look` and `logs` with `--project <name or id>` on the target (they act as the `--as` identity through the target's API only: no local project, settings, hub sync, agent directory, worktree or local template is used, no environment value is gathered from the local environment (`--config` and `--task-file` content is sent as given), and `--attach`, `--workspace`, `--upload-template`, `delete --cleanup-only`, `list --all` and the `list` relationship filters are refused). `message` is not available with a profile. Every other command is refused, `--hub` with a profile fails with `--hub cannot be used with a hub profile: the profile names the hub`, and `--no-hub` fails with its removal error first. A profile whose endpoint names this agent's own hub (`SCION_HUB_ENDPOINT` or `SCION_HUB_URL`) is refused; when either is set but is not an absolute URL, every profile is refused. The check compares parsed host names and ports (lowercase, without a trailing dot, IP literals in canonical form, loopback and container bridge host names treated as one host; paths ignored). It does not resolve DNS aliases, so a profile naming another name of the same hub is not detected; the target hub's own delegation remains the boundary. In agent mode, `profile add`, `show` and `remove` apply the same check. `--as <alias>` and `--run <label>` (or `SCION_HUB_RUN`) are flags of the `hub` and `artifact` commands and of the agent commands above that apply only with a profile. See [Test identities on another test hub](/scion/hosted/user/test-identity-target-hubs/).
    - `add <name>`: Add a profile. Flags: `--endpoint <url>` (required; https, or http on a loopback host), `--transport <iap|none>` (default `iap`), `--iap-audience <client-id>` and `--uat-sa <email>` (the target's IAP service account; both required for `iap`), `--issuer-sa <email>` (the target's issuer service account), `--admin-issuer-sa <email>` (the target's admin issuer service account; required with an admin delegation and different from `--issuer-sa`), `--issuer-delegation <tier>=<id>` (tiers `member`, `hub-admin` and `super-admin`; repeat the flag or separate entries with commas), `--replace`.
    - `list`, `show <name>`: List profiles; show one and its cached runs. Flag: `--json`.
    - `remove <name>`: Remove a profile. Refused while it has cached runs, unless `--remove-cache`.
- `scion hub test-identity`: Manage hub test identities: short-lived synthetic member or viewer users for testing on a test hub. Without a hub profile it uses your usual Hub credential and is not available in agent mode. With a hub profile it acts as the profile's issuer delegation, keeps issued identities in the run cache (`~/.scion/hub-profiles/<profile>/run-<label>/<alias>.json`, mode `0600`, created exclusively, symlinks refused), and also runs in agent mode (`issue`, `issue-admin`, `token`, `list`, `delete`, `teardown`, `web-code`, `cleanup`, `provenance`; not `delegate`), but only against a profile for another hub. The Hub must run with `--test-mode=member:<hub id>` or `--test-mode=admin:<hub id>` (otherwise every subcommand gets `404`; see "Test mode (operators)" in the [API reference](/scion/reference/api/)), and you need the `test_identity.issue` permission: an admin sign-in, or a hub-bound access token carrying the `test_identity:issue` scope (set it in `SCION_HUB_TOKEN` for CI). Without a hub profile, a test identity's access token is written **only** to the file named by `--out`: the path must not exist yet (an existing file or a symlink, even a dangling one, is refused before any request is sent), and the file is created with mode `0600`. The token is never printed; standard output and standard error carry only the identity's ID, email, role, expiry and the token file path. See `/api/v1/test-identities` in the [API reference](/scion/reference/api/).
    - `issue --out <file>`: Issue a new test identity and write its token to `<file>`. With a hub profile, `issue --as <alias> --run <label>` stores it in the run cache instead (no `--out`); the alias must not exist in the run yet.
        - Flags:
            - `--out <file>`: File to write the token to (required without a hub profile).
            - `--role <member|viewer>`: Role of the identity (default `member`). For an admin role use `--admin-role`.
            - `--admin-role <hub-admin|super-admin>`: Issue a test identity holding the real `hub-admin` or `super-admin` role, on a hub started with `--test-mode=admin:<hub id>` (`super-admin:<hub id>` for `super-admin`). The issuer must be a live human super-admin. `--purpose` (8 to 200 characters) and `--out` are required, `--role` does not apply, and a path under a symlinked directory is refused too. Token lifetime: `hub-admin` 15m by default, at most 30m; `super-admin` 10m by default, at most 15m. Identity lifetime: at most 1h for `hub-admin` and 30m for `super-admin`. The token is never re-issued. Not available in agent mode or with a hub profile: this is the human operator's command; a runner acting through an admin issuer delegation uses `issue-admin`. See [Test Identities with Admin Roles](/scion/contributing/test-admin-identities/).
            - `--ttl <duration>`: Token lifetime, in whole seconds or more (for example `30m`). Default: the Hub's, `30m`. The token never outlives the identity.
            - `--lifetime <duration>`: Identity lifetime (for example `2h`). Default: the Hub's, `1h`; the Hub refuses more than `8h`. After it, the identity's tokens stop working.
            - `--purpose <text>`: Short label recorded on the identity and in the audit log (at most 200 characters).
            - `--json`: Print `identity`, `tokenFile` and `tokenExpiresAt` as JSON (never the token).
    - `issue-admin --admin-role <hub-admin|super-admin> --purpose <text> --as <alias> --run <label>`: The runner's command, acting through an admin issuer delegation (a human operator on their own sign-in uses `issue --admin-role` instead). With a hub profile only, in human and agent mode (in agent mode only for a profile for another hub). Issue a test hub-admin or test super-admin on a target running the test admin tier (a systemd test hub), through the profile's admin delegation of `--admin-role`, with an issuer ID token of the profile's `--admin-issuer-sa`, and store it in the run cache as `--as` (mode `0600`; the token is never printed). Flags: `--admin-role` (required), `--purpose` (required by the Hub), `--lifetime <duration>` (default the kind's; at most 60 minutes for hub-admin, 30 for super-admin), `--ttl <duration>` (default the kind's; at most 30 minutes for hub-admin, 15 for super-admin), `--json` (never the token). A delegation cannot list or delete admin identities: `delete --as <alias>` is refused for one, and `teardown` reports it `left-to-expire` and removes its cache file. See [Admin-tier delegations](/scion/hosted/user/test-identity-target-hubs/#admin-tier-delegations-systemd-test-hubs).
    - `token <id> --out <file>`: Issue a new token for a live test identity you issued (an admin sign-in may do it for any), for runs longer than one token. Flags: `--out <file>` (required), `--ttl <duration>`, `--json`. With a hub profile, `token --as <alias> --run <label>` re-issues the cached identity's token into the cache; an admin identity (`issue-admin`) is re-issued only through the profile's admin delegation that issued it, within the kind's token limit and never past the identity's expiry. Cached tokens are also re-issued automatically when a command uses `--as` within 5 minutes of the token's expiry; an expired identity is refused ("issue a new one").
    - `web-code <id> --out <file>`: Mint a single-use web-login code for a live test identity you issued, for a browser session as the identity (see [Browser Sessions as Test Identities](/scion/contributing/test-identity-browser-sessions/)). The code is valid for 5 minutes and can be redeemed once at `POST /auth/test-identity/redeem`; a new code replaces an unredeemed earlier one. The Hub must run with `--test-mode=member:<hub id>` or above (`admin:<hub id>`, `super-admin:<hub id>`) and carry the test infrastructure marker (`scion server test-infra mark`); otherwise the Hub answers `404`. `--out` must not exist yet: an existing file, a symlink, or a path under a symlinked directory is refused before any request is sent. The file is created with mode `0600` and holds only the code, which is never printed. Flags: `--out <file>` (required), `--json` (prints `identityId`, `codeId`, `expiresAt` and `codeFile`, never the code). With a hub profile, the code is minted on the target through the profile's issuer delegation, for an identity issued through it (`--as` does not apply), and still goes only to `--out`; `--admin-role <hub-admin|super-admin>` mints it through the profile's admin delegation of that tier instead, for an admin identity that delegation issued (only when the target runs with `server.hub.test_identities.admin_web_sessions` on; `--admin-role` needs a profile). In agent mode it runs only with a profile for another hub.
    - `list`: List the test identities you issued (every identity for an admin sign-in). Only live identities are listed unless `--include-expired` is set. Flags: `--limit <n>` (default: the Hub's, 100; at most 500), `--include-expired`, `--json`.
    - `delete <id>`: Delete a test identity. Its role bindings and group memberships go with it, and its tokens stop working at once. The delete is refused while the identity still owns agents, or is the last owner of a project; the command lists them, and you delete them first. Deleting an identity that does not exist (for example, deleted already) succeeds with a notice on standard error, so a teardown step can run twice. A plain delete does not remove a test identity with an admin role: the Hub answers it as not found, and without a hub profile, in human mode, the notice points to `delete <id> --purge`.
        - With a hub profile, `delete --as <alias> --run <label>` deletes the cached identity and removes it from the cache.
        - `--purge`: Also remove what the identity created: its agents (with their files on the broker), schedules, groups and projects. Only resources the Hub recorded as created under the identity are removed. A project that has another owner, or resources someone else created, is kept, and the identity's access to it is removed. Agents the identity owns without that record are listed as unproven and never removed. The command prints what it removed and kept. If the purge cannot finish (for example a broker is offline) it lists what is left and exits non-zero; run it again to finish.
    - `teardown --run <label>`: With a hub profile only. Delete every test identity of the run (each cached alias, and any identity the profile's delegation lists whose purpose names the run), then remove the run's cache. An identity already gone counts as done; if anything is left, the command lists it and exits non-zero (run it again). Flags: `--purge` (purge each identity instead of deleting it), `--json`.
    - `cleanup`: Remove what your expired or deleted test identities created: their agents (with their files on the broker), schedules, groups and projects, and the expired identities themselves. The Hub selects the targets; the command names none. Only resources the Hub recorded, at creation, as created under a test identity you issued are removed, and only for identities that have expired or been deleted: a live identity is never touched, nor is another issuer's. An admin sign-in reaches every issuer. With a hub profile it acts as the profile's issuer delegation and reaches only the identities issued through that delegation. A project with another owner, or resources someone else created, is kept. The command prints each identity it reached (`expired` or `absent`) and each target with its outcome. If the run cannot finish (for example a broker is offline) it lists what is left and exits non-zero; run it again to finish. Test identities, the admin roles included, never clean up; the Hub refuses them and the command names the refusal. In agent mode it runs only with a profile for another hub. See [Cleaning Up After Test Identities](/scion/contributing/test-identity-cleanup/).
        - Flags:
            - `--dry-run`: List what a run would remove; change nothing.
            - `--expect-absent <file>`: Check resources the run expects to be gone, one `<type>:<id>` per line (`agent`, `project`, `schedule` or `group`; blank lines and lines starting with `#` are ignored; an invalid line refuses the file before anything is sent). Each one's provenance is checked before the cleanup. A proven one is reported `cleaned` when the run removed it (`would be cleaned` with `--dry-run`), `absent (proven by deletion record <id>)` when it is already gone and the Hub proves it by the deletion record the expiry reaper, a purge or a cleanup wrote, and `NOT cleaned` with the reason otherwise (for example its identity is still live). An unproven one is printed with `proven:false`, the reason and `real-admin action required`, and is never reported as cleaned. The command exits non-zero when any expected resource is unproven or not cleaned.
            - `--json`: Print `{report, expectAbsent}` as JSON.
    - `provenance <type>:<id>`: Show whether the Hub recorded one resource (`agent`, `project`, `schedule` or `group`) as created under a test identity in your issuer scope (identities you issued, those of the profile's delegation with a hub profile, or every issuer's for an admin sign-in). A proven resource shows the identity, its issuer and its state (`live`, `expired` or `absent`); `cleanup` removes it once that identity has expired or been deleted. A resource that is already gone is shown as deleted, with the correlation ID of the deletion record that proves it. An unproven resource shows only the reason (the Hub never reports its owner or creator, and a resource that does not exist, or another issuer's, gets the same answer); cleanup never removes it, and a real administrator must decide. Flag: `--json`. In agent mode it runs only with a profile for another hub.
    - `residue <id>`: Print the residue report of one test identity with an admin role, live or purged: the grants and resources stamped with its issuance (a purge removes them) and the changes it made to rows without its stamp (reported, never reverted). It holds IDs and audit facts only. One identity per call. Only a human super-admin signed in interactively may read it; other callers get a named error. Flag: `--json`. Not available in agent mode or with a hub profile.
    - `revoke-all --reason <text>`: Emergency stop: revoke every test identity with an admin role on the Hub. Each is deleted, or suspended with its role bindings removed when it owns agents or is a project's last owner; its tokens stop working at once. Then restart the Hub without `--test-mode=admin` or `super-admin`. Only a human super-admin signed in interactively may run it; other callers get a named error. Flags: `--reason <text>` (required; printed with the result as the operator's record only, the Hub does not receive or store it), `--dry-run` (reads only the admin health summary and prints whether the admin tier is on and how many such identities are live; changes nothing, and does not check your super-admin authority), `--json`. If it fails partway (for example `503`), some identities may already be revoked; check with `--dry-run` and run it again (it is safe to repeat). Not available in agent mode or with a hub profile. See [Test Identities with Admin Roles](/scion/contributing/test-admin-identities/).
    - `delegate`: Manage workload issuer delegations on a test hub: the human consent step that lets the target's issuer service account issue member and viewer identities (or, on a Hub running the test admin tier, admin test identities) within your own live authority. Takes your interactive sign-in on that Hub; never an access token, an agent token or a hub profile. Not available in agent mode.
        - `create --workload-sub <unique id>`: Create a delegation pinned to the numeric unique id of the target's issuer service account (it must be in the Hub's startup allowlist `server.hub.test_identities.workload_issuers`). Flags: `--scope member|hub-admin|super-admin` (default `member`; the admin scopes need a Hub running `--test-mode=admin` or `super-admin`, a live super-admin sign-in and an allowlist entry with `max_tier: admin`), `--ttl <duration>` (member: default 8h, at most 7 days; hub-admin: default 4h, at most 8h; super-admin: default 1h, at most 2h), `--member-cap <1-5>` (member scope), `--admin-cap <n>` (hub-admin 1-2, super-admin 1), `--label <text>`, `--json`. The printed delegation id is not a secret; give it to the runner's profile with `--issuer-delegation <scope>=<id>` (an admin delegation also needs the profile's `--admin-issuer-sa`); the runner then uses `issue` or `issue-admin` (see [Test identities on another test hub](/scion/hosted/user/test-identity-target-hubs/#admin-tier-delegations-systemd-test-hubs)).
        - `list`: List your delegations. Flag: `--json`.
        - `revoke <id>`: Revoke a delegation; issuance through it stops at once.

```bash
# CI: issue a member identity, run tests with its token, then tear it down
scion hub test-identity issue --role member --ttl 30m --purpose "e2e" --out ./fixture.token --json > fixture.json
FIXTURE_ID=$(jq -r .identity.id fixture.json)
./run-e2e.sh --token-file ./fixture.token   # your test runner reads the token from the file
scion hub test-identity delete "$FIXTURE_ID" --purge
```
- `scion hub env`: Manage environment variables on the Hub.
    - `set <key>=<value>`: Set a variable.
    - `get [key]`: Get variable values. `get` and `list` honor `--format json` (as well as `--json`).
    - `clear <key>`: Remove a variable.
- `scion hub hook` (alias `psh`): Manage hub-scoped (baseline) pre-start hooks. Requires administrator privileges.
    - `list` (alias `ls`): List hub-scoped pre-start hooks.
    - `show <id-or-slug>`: Show details and script content of a hub-scoped hook.
    - `create`: Create a new hook (archives the current active hook).
        - Flags: `--name` (required, human-readable name), `--script` (required, path to shell script, or `-` for stdin), `--slug` (url-safe identifier), `--description` (optional description).
    - `update <id-or-slug>`: Update an existing hook.
        - Flags: `--name`, `--script`, `--description`.
    - `activate <id-or-slug>`: Mark an archived hook as active (archives any currently active hub-scoped hook).
    - `delete <id-or-slug>` (alias `rm`, `remove`): Delete an archived hook. Active hooks cannot be deleted.
- `scion hub messaging`: Manage hub-wide cross-project messaging settings. Requires administrator privileges.
    - `set`: Set hub-wide messaging settings.
        - Flags: `--cross-project-enabled <bool>` (enable or disable cross-project messaging), `--revision <int>` (required, optimistic concurrency revision).
    - `get`: Show the current hub-wide messaging settings and revision.

### `scion service-accounts`

Manage GCP service accounts registered with the Hub (aliases `service-account`, `sas`). Scope comes from the root `--global` flag: with `--global` the commands address hub-scoped accounts, which belong to no project and are assignable from every project; without it they address the current project's accounts.

- `add <email>`: Register an existing GCP service account. Any current hub member may register a hub-scoped account (`--global`). A hub-scoped account can be assigned only while `gcp_iam_check_mode` is `enforce`; see [Hub-Scoped Service Accounts](/scion/hosted/ha/permissions/#hub-scoped-service-accounts).
    - Flags: `--gcp-project <id>` (required), `--name <string>` (display name).
- `list` (alias `ls`): List accounts at the selected scope. Flags: `--assignable` (the accounts assignable to an agent in this project: its own plus every hub-scoped account; not valid with `--global`), `--json`.
- `show <id>` (aliases `get`, `describe`): Show one account, including whether the Hub's impersonation check passed and, if not, why.
- `verify <id>`: Re-run the Hub's check that its identity holds `roles/iam.serviceAccountTokenCreator` on the account.
- `remove <id>` (aliases `rm`, `delete`): Remove the registration from the Hub. The GCP service account itself is not deleted. The Hub refuses (`409 sa_in_use`) while a default points at the account (a project default, a per-profile default or the Hub default) and the command prints what references it. Agents that reference the account do not block removal; they fail at their next start. On success the command lists the cleanup the Hub cannot do: the Runtime Broker mapping, the Kubernetes ServiceAccount, the IAM bindings and, for a minted account, the account itself, which is kept in GCP.
    - `--force`: Clear the project and per-profile defaults that point at the account (those you may update), then remove it. A cleared `assign` default becomes `block`. The Hub default is never cleared: a Hub admin must change it.

### `scion tunnel`

Open a tunnel from your machine to a port inside an agent, or to the agent's SSH service, through the Hub. Requires the `hub.conduit` [experiment](/scion/reference/experiments/) on the Hub and an agent with a Conduit session. See [Tunnels from your machine](/scion/hosted/user/port-forwarding/#tunnels-from-your-machine-scion-tunnel).

**Usage:** `scion tunnel <agent> (--port N [--local-port L] | --ssh) [--stdio]`

- **Flags:**
    - `--port <int>`: Agent-local TCP port to reach (on the agent's loopback address).
    - `--local-port <int>`: Local port to listen on. Default: a random free port.
    - `--ssh`: Reach the agent's SSH service instead of a port.
    - `--stdio`: Carry the tunnel on stdin and stdout instead of a local listener, for use as an ssh `ProxyCommand`.
    - `--known-hosts <path>`: With `--ssh`, the absolute path of the known-hosts file holding the agent's pinned host key, for the host-key hint only. Requires `--host-key-alias`. Written into Host blocks by `scion vscode`.
    - `--host-key-alias <host>`: With `--ssh`, the name of the pin in `--known-hosts` (the Host block's `HostKeyAlias`). Requires `--known-hosts`.

By default the tunnel listens on `127.0.0.1` and prints the address it forwards to stderr. `--hub` and `--project` make the command independent of the working directory. The command never prompts. Exit codes: `0` success, `1` other error, `2` usage error, `3` no usable credential or identity changed, `4` agent not found, `5` not permitted, `6` Hub unreachable or unavailable, `7` tunnel closed by policy, `8` tunnel limit reached, `9` agent not running, `10` agent unreachable, `11` Hub does not support tunnels, `130` interrupted, `143` terminated. A listener that gets `3`, `4`, `5` or `11` for a new connection stops accepting connections. It exits with that code once its open connections have ended. A stopped agent (`9`) refuses only that connection, and the listener keeps listening. With `--ssh`, the command prints a one-line hint when the agent's host key no longer matches its pin. Without `--known-hosts` it reads the `scion ssh-config` pins. The hint never changes host-key verification: ssh's `StrictHostKeyChecking` and the Host block's `UserKnownHostsFile` remain the only check.

```bash
scion tunnel my-agent --port 5173
scion tunnel my-agent --port 5173 --local-port 8080
scion tunnel --stdio --ssh --hub https://hub.example.com --project my-project my-agent
```

- `scion tunnel status [--json]`: Show the open tunnels of your running `scion tunnel` processes: the Hub endpoint, the credential identity (kind, subject and scope, never the secret), the session IDs and the open tunnels. With `--hub`, only tunnels to that Hub are shown. Reads local state only and never prompts.

### `scion ssh`

Open an SSH session to an agent with your system `ssh` client. The connection runs through `scion tunnel --ssh --stdio` as the `ProxyCommand`, and the agent's host key is taken from the Hub, so no keys or `known_hosts` entries need to be set up first. Same requirements as [`scion tunnel`](#scion-tunnel) with `--ssh`. Not available in agent mode.

**Usage:** `scion ssh [flags] <agent> [<command>...]`

- `<agent>` is an agent in the current project, `<project>/<agent>`, or the only agent with that name across all projects. The Hub and project are resolved as for `scion tunnel`, including the project selected with `scion use`.
- Flags for `scion ssh` (such as `--hub` and `--project`) go before the agent. Everything after the agent is the remote command and is passed to `ssh` unchanged, flags included (a `--` right after the agent is dropped).
- The command never prompts for authentication, registration or sync. It returns the remote command's exit status, or `255` for a local or connection failure (including a usage or flag error), with the reason on one stderr line.

```bash
scion ssh my-agent
```

```bash
scion ssh my-agent git status
```

### `scion ssh-config`

Print or install SSH Host blocks for reaching agents through `scion tunnel`, for example for VS Code Remote-SSH or plain `ssh`. Not available in agent mode.

**Usage:** `scion ssh-config [flags] [<agent>]`

- Flags: `--write`; `--hub` and `--project` work as for `scion tunnel`.

- Each agent gets a Host named `scion.<hub-label>.<project>.<agent>` whose `ProxyCommand` runs `scion tunnel` with the Hub and project pinned, and whose host key is pinned in a known-hosts file kept by Scion, where `<hub-label>` is derived from the Hub's host name (not its hub-list nickname).
- With no agent, every agent the Hub lists across all projects is included. With an agent, only that agent's block is updated.
- Without `--write`, the files are printed to stdout and nothing is written. With `--write`, the blocks are installed in `~/.ssh/config.d/scion-<hub-label>`, the host keys in `~/.scion/ssh/known_hosts-<hub-label>`, and one `Include` line is added at the top of `~/.ssh/config`. Regenerating without an agent removes the blocks of agents that are no longer listed; if the listing fails or is incomplete, nothing is written.
- Exit codes: `0` success, `1` other error, `2` usage error (bad flags or arguments, or an agent name found in several projects), `3` no usable or refused credential, `4` agent not found, `5` not permitted, `6` Hub unreachable or unavailable.

Tunnel streams opened this way are re-checked with the same authorization as when the tunnel was opened, and close when your access is withdrawn.

### `scion vscode`

Open VS Code Remote-SSH on an agent's `/workspace` through the Hub, without editing `~/.ssh/config`. It sits alongside `scion ssh-config` and does not replace it. Requires the `hub.conduit` [experiment](/scion/reference/experiments/) on the Hub. Not available in agent mode.

**Usage:** `scion vscode <agent> [--path <dir>] [--prune]`

- **Flags:**
    - `--path <dir>`: Remote folder to open. Default: `/workspace`.
    - `--prune`: Also remove the Host blocks and pins of this Hub's agents that no longer exist, from a complete listing only. The agent named is still written and opened; with an incomplete listing nothing is changed and VS Code is not started.

The Hub and project are resolved as for `scion tunnel`, including the project selected with `scion use`. Each run writes the agent's Host block and pinned host key into `~/.scion/vscode/ssh_config` and `~/.scion/vscode/known_hosts` and keeps every other agent's entries. Its ProxyCommand passes `--known-hosts` and `--host-key-alias`, so a recreated agent is reported with the command that refreshes it: `scion vscode --hub <endpoint> --project <project> <agent>`. It checks the agent with one SSH tunnel, then runs `code --user-data-dir ~/.scion/vscode/user-data --remote ssh-remote+scion.<hub-label>.<project>.<agent> <path>`, where `<hub-label>` is the label a `scion ssh-config` include file already records for this Hub, or else one derived from the Hub's host name that no other Hub's entries use. That user data directory's `settings.json` sets `remote.SSH.configFile`, which is application-scoped and cannot be set per profile; your own VS Code settings are never changed, and extensions are shared. The command never prompts. Exit codes are those of `scion tunnel`; `1` when `code` is missing or fails to start, or when the user data `settings.json` has comments and does not already hold the managed values (the error names the file).

Terminals in these windows open in the agent's tmux session: the default `Scion agent` profile joins it with the `agent` window selected, `Scion shell` selects the `shell` window, and `Container shell` is a plain login shell; VS Code's auto-detected `tmux`, `bash`, `zsh` and `sh` profiles are hidden. Switch windows with `Ctrl-b n`. Closing a terminal leaves no extra tmux session behind, and switching windows does not move the web terminal. If the agent has no tmux session, the Scion profiles open a login shell instead. The scion profiles in that `settings.json` are reset on every run; a default terminal or keybinding choice you make there is kept.

```bash
scion vscode my-agent
scion vscode my-project/my-agent --path /workspace/web
```

#### Troubleshooting: new files do not appear in the explorer

**Symptom:** in a `scion vscode` window, files created outside the explorer (from a terminal, or by the agent) do not show up until you click Refresh in the explorer.

**Cause:** VS Code's file watcher runs inside the agent container and needs an inotify instance. With Docker or Podman, every agent container runs as the Runtime Broker's host user (no user-namespace remapping), so all of them share that user's `fs.inotify.max_user_instances` limit (often 128). On a busy Runtime Broker host the limit can run out, and the watcher then stops without a visible error. `max_user_watches` is usually already high, but it can run out too on workspaces with many directories.

**Confirm:** in the VS Code Output panel, the "Log (Remote Server)" or "File Watcher" channel shows inotify errors such as `EMFILE` ("Too many open files") or "unable to watch", and Refresh brings the files back. `ENOSPC` means `max_user_watches` ran out instead. On the Runtime Broker host, `scion doctor` reports the limits and their current use as its `inotify-limits` check.

**Fix:** a host administrator raises both limits on the Runtime Broker host, for example `sudo sysctl -w fs.inotify.max_user_instances=8192 fs.inotify.max_user_watches=524288`, and keeps them in a file under `/etc/sysctl.d/` (see [Scaling a Broker Host: inotify Limits](/scion/hosted/ha/runtime-broker/#scaling-a-broker-host-inotify-limits)). No restart is needed. Then reload the VS Code window (**Developer: Reload Window**). No `scion vscode` setting affects file watching: the command sets only `remote.SSH.configFile` and the terminal profiles, and sets the default terminal profile and `terminal.integrated.sendKeybindingsToShell` only when they are unset.

### `scion expose` / `scion unexpose`

Register or remove an agent-local HTTP port with the Hub's port forwarding proxy. Run these inside an agent container: they act as the calling agent, using its own Hub identity, and fail with `must be run inside an agent container with a hub connection` anywhere else. Available in agent mode. They need no local project. See [Manual Port Management](/scion/hosted/user/port-forwarding/#manual-port-management).

**Usage:** `scion expose <port> [--label <text>] [--host <addr>]`, `scion expose --list`, `scion unexpose <port>`

- **Flags (`expose`):**
    - `--label <string>`: Optional label to identify the service.
    - `--host <string>`: Local host to forward to. Default: `127.0.0.1`.
    - `--list`: List the agent's exposed ports (port, label, proxy URL) and the auto-expose status instead of exposing one.

Both commands are idempotent: exposing a port that is already exposed, or unexposing one that is not, succeeds. They replace `sciontool expose` and `sciontool unexpose`, which now print a message naming the `scion` command and exit `1`.

```bash
scion expose 8080 --label my-web-app
scion expose --list
scion unexpose 8080
```

## Artifacts

### `scion artifact`

Publishes files as artifacts and fetches them by reference (`scion://artifact/<id>[@<seq>]`). Requires the `hub.artifacts` experiment (off by default). Available in agent mode, except `share`, `grants` and `delete`. An unknown subcommand is an error, also with `--help`: `scion artifact nosuch --help` exits non-zero with `unknown command`, as do `scion hub nosuch --help` and `scion config nosuch --help`. See [Artifacts](/scion/reference/artifacts/) for access rules and the API.

- `scion artifact publish <file|folder>`: Publish a file or folder in the current project; prints its reference, version and web page URL.
    - Flags: `--title <title>` (set when the artifact is created; default: the entry file's name), `--key <key>` (publishing again under the key adds a version), `--note <text>`, `--entry <path>` (a folder's entry file).
    - With `--review <ref>`, when the Hub refuses the review because it changes text outside its marks, the CLI prints each differing region of each file. A region that differs only in whitespace is labelled `(whitespace only)` and shows tabs as `→`, trailing spaces as `·` and line ends as `⏎`; a change to the final newline is named, and an empty side prints `(no lines)`. See [Reviews](/scion/reference/artifacts/#reviews).
- `scion artifact get <ref>`: Write an artifact's entry file to stdout, or with `--out` the file or the whole bundle.
    - Flags: `--out`, `-o <path>` (a file, an existing directory for a single file, or the directory a bundle is written into), `--force` (replace files that already exist under `--out`).
- `scion artifact versions <ref>`: List an artifact's versions, newest first; the current one is marked `*`. Discarded versions (a rejected or stale review, or an upload not finished in time) are listed among them by number with their reason, so gaps in the numbering are explained.
- `scion artifact list`: List the artifacts you can read, newest first: those you own, those shared with you, and those homed in projects whose artifacts you may read. Each line shows the reference, title, current version, why you see it (`owned`, `project` or `shared`) and when it last changed; one page is printed unless `--all` is given.
    - Flags: `--mine` (only artifacts you own), `--project-id <id>` (only artifacts homed in, or shared with, that project), `--shared` (only those shared with you, or with `--project-id` with that project, from elsewhere), `--review-pending`, `--query <text>` (title or key), `--limit <n>` (page size), `--all` (follow every page), `--cursor <c>` (start at the page a previous `--format json` list returned as `nextCursor`).
- `scion artifact view <ref>`: Print a short-lived link that shows a version whose entry is HTML, the way the web UI shows it, and when it expires (30 minutes unless the Hub says otherwise). It never opens a browser. A version whose entry is not HTML has no view. In agent mode it prints the version and the expiry but not the link; `--show-link` prints the link too. In human mode the link is always printed.
    - Flags: `--show-link` (print the link in agent mode too), `--check` (also read the version's HTML files and report every `href` or `src` that names no file of the version, including links that start with `/` or climb above the bundle's top folder, and exit non-zero if there is one; links with a scheme, to another host or to a fragment of the same page are not checked, and a link that cannot be parsed is reported; a `<base href>` is not taken into account).
- `scion artifact share <ref>`: Create a share link and print it, with its expiry and the command that revokes it. The link is shown only once. Only the artifact's owner or a user with an admin grant can share it; not available in agent mode, and the Hub refuses agents. See [Share links](/scion/reference/artifacts/#share-links).
    - Flags: `--ttl <n>h|<n>d` (link lifetime; default: the Hub's, 7 days unless changed; a lifetime above the Hub's maximum, 30 days unless changed, is rejected, not shortened), `--list` (list the active links instead), `--revoke <link-id>` (revoke one instead).
- `scion artifact grants <ref>`: List an artifact's grants (id, subject, permission, created), or add, change or remove one. Only the artifact's owner or a user with an admin grant can see or change its grants. On an artifact owned by an agent, the user who delegated to the agent and anyone holding `project.manage` in its home project (the project owners and admins, and super-admins) may also add a review grant for a user who can already read it, and nothing else; they cannot list the grants. Not available in agent mode, and the Hub refuses agents. See [Grants](/scion/reference/artifacts/#grants).
    - Flags: `--add <subject>` with `--permission read|review|write|admin` (subjects `user:<id>`, `agent:<id>` or `project:<id>`; review is for users only; a subject that already has a grant gets the new permission), `--remove <grant-id>`.
- `scion artifact delete <ref>`: Delete an artifact after confirming (`--yes` skips the prompt; required when stdin is not a terminal). Its grants and share links end with it and its files are removed after the hub's grace period; there is no undelete. The artifact's owner, a user with an admin grant on it, or an admin of its home project can delete it; not available in agent mode, and the Hub refuses agents. See [Deletion](/scion/reference/artifacts/#deletion).

## Notification Management

### `scion notifications`

Manages notifications and notification subscriptions.

- `scion notifications`: List your recent unacknowledged notifications.
    - Flags: `--all` (include acknowledged notifications), `--json` (format output as JSON).
- `scion notifications ack [id]`: Acknowledge one or all notifications.
    - Flags: `--all` (acknowledge all unacknowledged notifications).
- `scion notifications subscribe`: Subscribe to notifications for a specific agent or all agents in a project.
    - Flags:
        - `--agent <name-or-id>`: Subscribe to specific agent.
        - `--project <name>`: Specify project (inferred from context if omitted).
        - `--triggers <triggers>`: Trigger activities to subscribe to. This flag is **repeatable** and also accepts a **comma-separated list** of triggers (e.g., `--triggers COMPLETED,WAITING_FOR_INPUT --triggers LIMITS_EXCEEDED`). Default: `COMPLETED,WAITING_FOR_INPUT,LIMITS_EXCEEDED`. Strict empty-value validation is enforced.
- `scion notifications unsubscribe <id>`: Remove a subscription.
    - Flags: `--all` (remove all subscriptions in the project), `--project <name>`.
- `scion notifications update <id>`: Update a subscription's trigger activities.
    - Flags:
        - `--triggers <triggers>`: Trigger activities to update (required). This flag is **repeatable** and also accepts a **comma-separated list** of triggers (e.g., `--triggers COMPLETED,WAITING_FOR_INPUT --triggers LIMITS_EXCEEDED`). Strict empty-value validation is enforced.
- `scion notifications subscriptions`: List your active notification subscriptions.
    - Flags: `--project <name>` (filter by project), `--json` (format output as JSON).

## Infrastructure

### `scion runtime-broker`

Manages the local host as a Runtime Broker. The old name `scion broker` still works as a deprecated alias.

**Broker port.** `start` runs the broker on `--port`, else on `server.broker.port` from the global settings, else on 9800. While the broker runs, `start` keeps a record of that port (removed by `stop`). `register`, `deregister`, `status`, `stop`, `restart` and `hubs` use their own `--port` if given, else the recorded port, else the settings port, else 9800. When `scion server start` runs the broker, `start`, `stop`, `restart` and `scion server status` look for it on the server's `--runtime-broker-port`, else on `server.broker.port`, else on 9800.

**Broker name.** A broker registers on the Hub under its hostname unless `register --broker-name` gives another name. The Hub reuses an existing broker with the same name, so when you run several brokers on one host, give each its own `--broker-name` (and its own `HOME`, so each has its own settings, broker ID and credentials). The name is saved in the global settings as `server.broker.broker_nickname`, where `register`, `join`, `provide`, `withdraw`, `status`, project registration and the broker server itself read it instead of the hostname. An existing `server.broker.broker_name` or `server.broker.broker_nickname` setting is used the same way, so `register`, `join` and project registration send that name rather than the hostname, matching the name `scion server start` already gives the broker. If such a host was registered under its hostname, a `register --force`, or a register after its credentials and broker ID are lost, no longer matches the hostname-named broker on the Hub and creates a new one.

- `scion runtime-broker status`: Show status of the local broker server, including the projects it provides for. Providers added with `--auto-provide` are listed right away. Right after a broker start or restart (a daemon started under 30 seconds ago, or a broker uptime under 30 seconds), `status` waits for the broker to serve and for its Hub connections to report. All of this waiting shares one 5-second budget. A connection the running broker has not reported yet is shown as `pending` (it connects on its first heartbeat). `unknown` means no broker answered on the port.
    - `--json`: Output in JSON format.
    - `--broker <id>`: Show the status of another broker as the Hub sees it, instead of the local one.
    - `--port <port>`: Port of the local broker.
- `scion runtime-broker start`: Start the broker server as a background daemon.
    - `--foreground`: Run in the current process instead of daemonizing. Use this as the `ExecStart` of a systemd `Type=simple` unit.
    - `--port <port>`: Listen on a custom port (default: `server.broker.port` from settings, else 9800).
    - `--auto-provide`: Automatically add this broker as a provider for new projects.
    - `--debug`: Enable debug logging.
- `scion runtime-broker stop`: Stop the broker daemon. A broker running in the foreground is stopped with Ctrl+C instead.
    - `--port <port>`: Port of the local broker (used to detect a foreground broker).
- `scion runtime-broker restart`: Stop the broker daemon and start it again with the current `scion` binary, for example after an upgrade. It does not restart a foreground broker. The new daemon keeps the `--port`, `--auto-provide` and `--debug` values the running daemon was started with, unless you pass them again.
    - `--port <port>`, `--auto-provide`, `--debug`: Override the values the daemon was started with.
- `scion runtime-broker register`: Register this host as a Runtime Broker with the Hub. The local broker server must be running. Registering from the global context (`--global`, or outside any project) never offers to link the `global` pseudo-project or add the broker as its provider, so no `global` project is created on the Hub; use `provide --project <name>` to provide for a Hub project. Requires the `broker.create` permission (see [Broker Registration Permission](/scion/hosted/ha/runtime-broker/#broker-registration-permission)). Credentials are saved to `~/.scion/hub-credentials/<name>.json`.
    - `--name <name>`: Name for this Hub connection. Default: derived from the Hub endpoint.
    - `--broker-name <name>`: Name this broker registers under on the Hub, saved to global settings. Default: the saved name, else the hostname. The Hub keeps the name of a broker that is already registered; to change it, deregister first. Restart the broker afterwards so it uses the new name locally.
    - `--force`: Register again even if already registered. This also issues a new broker secret.
    - `--auto-provide`: Automatically add this broker as a provider for new projects.
    - `--transport-mode <iap|cloudrun_invoker>`, `--transport-audience <audience>`: Transport auth for a Hub behind IAP or Cloud Run, saved to the credentials file (see [Transport Auth for IAP-Protected Hubs](/scion/hosted/ha/runtime-broker/#transport-auth-for-iap-protected-hubs)).
    - `--port <port>`: Port of the local broker.
    - `--instance <key>`: Register the configured [flat Runtime Broker](/scion/hosted/ha/multi-broker/#flat-runtime-brokers-experimental) instance with this key (from `server.broker.instances` in the global `settings.yaml`) instead of this host. Uses your Hub login, does not need the instance to be running, and saves the instance's own credentials only after the Hub accepts the registration; the host checks them at its next start.
- `scion runtime-broker join`: Join this host to the Hub as an existing broker, using a join token from `scion hub brokers join-token create`. Sends no Hub user credential. Saves credentials to `~/.scion/hub-credentials/<name>.json` and the broker ID to global settings. Does not provide the broker to any project.
    - `--broker-id <id>`: The broker to join as. Default: `SCION_BROKER_ID`.
    - `--token-file <path|->`: Read the token from a file, or from stdin with `-`. Without it, the token is read from `SCION_BROKER_JOIN_TOKEN`. There is no flag that takes the token itself. A file with any group or other permissions is still used, with a warning.
    - `--force`: Replace existing credentials for this Hub connection, or a different broker ID in global settings. Without it, `join` stops in either case.
    - `--name <name>`, `--transport-mode`, `--transport-audience`: As for `register`.
    - `--port <port>`: Port of the local broker. If no broker is running there, `join` warns and continues.
- `scion runtime-broker deregister`: Remove this broker's registration from the Hub, which also removes it from every project it provides for. Deletes the local credentials for that Hub connection. Once no Hub connection remains, it also clears this broker's ID and token from global settings and removes the empty `~/.scion/hub-credentials/` directory; other settings are kept.
    - **Local state left behind.** Without `--purge-local`, deregister lists the broker-local state it leaves in place: the broker daemon log (`~/.scion/broker.log`), the broker's state directory (`<server.broker.state_dir>/<broker-id>/`, by default `~/.scion/runtime-broker-state/<broker-id>/`) and the broker template cache (`~/.scion/cache/templates/`). The `~/.scion/hub-id` file belongs to a local Hub, not to the broker, and is always kept.
    - `--purge-local`: Also remove that broker-local state. It runs only once the last Hub connection is gone, the connections could be listed, and no broker is running (stop it with `scion runtime-broker stop` first). Otherwise the purge is skipped: the command lists the paths left in place and exits non-zero with the reason, even when the deregistration itself succeeded. A credentials file without a broker ID cannot be deregistered: when it is the connection you selected, it does not block the purge and its path is printed; any other such file is named in the error, to be removed by hand. The state directory follows `server.broker.state_dir` as read from the global settings and environment of the shell that runs `deregister`. If that setting cannot be read or is invalid, the purge removes nothing and exits non-zero; without `--purge-local`, deregister warns and lists the default state directory instead. A broker run with a custom template cache directory keeps that cache. Before it removes anything, it checks every entry against its expected location: the daemon log path, `<state root>/<broker-id>` and `<scion home>/cache/templates`. An entry is refused when it is not exactly that path, when a symlinked parent directory resolves outside its root, when the daemon log path is a directory, when its root (the state root, or the scion home for the template cache) is relative, the filesystem root or the home directory, or when the state root is the scion home or a parent of the home directory or the scion home. If any entry is refused, the command prints `Not an expected broker state location, not removed:` with each path and reason, removes nothing and exits non-zero. A symlink at the entry itself is removed as a link; its target is kept. The purge never removes settings files, the `hub-id` file, another broker ID's state or other Hubs' credentials. On a host that is no longer registered, `deregister --purge-local` removes the local state only.
    - `--name <name>`: The Hub connection to deregister. Required when there is more than one (see `hubs`).
    - `--broker-only`: Accepted, but currently has no effect.
    - `--port <port>`: Port of the local broker.
- `scion runtime-broker hubs`: List this broker's Hub connections, with live connection status when the broker is running.
    - `--json`: Output in JSON format.
    - `--port <port>`: Port of the local broker.
- `scion runtime-broker provide`: Add this broker as a provider for a project.
    - `--project <name|id>`: The project to provide for. Without it, the project is resolved from the current directory.
    - `--path <path>`: The project path to register for this broker, resolved on this host. When `--broker` names another host's broker, give the absolute path to the project root (the directory containing `.scion`) on that host; it is sent as given, without checking this host's filesystem. For a remote broker, a path whose last element is `.scion` is refused; pass the directory that contains it. With `--project`, no path is sent unless `--path` is given: an existing provider path is kept, and otherwise the broker uses its Hub-managed project directory. The broker's global directory (`~/.scion`) is refused as the path of any project other than the global project.
    - `--make-default`: Make this broker the project's default Runtime Broker.
    - `--broker <name|id>`, `--hub <name>`: Operate on another broker or Hub connection.
    - Confirmation: `provide` asks before adding the provider. Without a terminal on stdin (a script, `ssh` without a TTY, a systemd unit), or at end of input, it fails with a message to re-run with `--yes` instead of waiting for an answer.
- `scion runtime-broker withdraw`: Remove this broker as a provider from a project.

### `scion server`

Manages Scion server components (Hub and Broker).

- `scion server start`: Start one or more server components.
    - Flags:
        - `--enable-hub`: Enable the Hub server component.
        - `--enable-runtime-broker`: Enable the Runtime Broker component.
        - `--port <int>`: Port to listen on.
        - `--host <addr>`: Listen address of the Hub API and web server (default `127.0.0.1` in workstation mode; in hosted mode, `server.hub.host`, which defaults to `0.0.0.0`). The Runtime Broker listener is set by `server.broker.host` instead; in workstation mode it defaults to `127.0.0.1`, and `0.0.0.0` counts as unset. In hosted mode, a server that runs only the Runtime Broker listens on `127.0.0.1`, even when `server.broker.host` is set, unless `--host` is given; it then uses `server.broker.host` (the `--host` value itself does not apply to the Runtime Broker).
        - `--db <string>`: Database driver/connection.
        - `--dev-auth`: Enable dev-auth authentication.
        - `--debug`: Set the server's default log level to `debug`. It changes logging only; see [Controlling the Log Level](/scion/hosted/single-node/observability/#controlling-the-log-level).
        - `--enable-debug-endpoints`: Serve diagnostic endpoints for local development. Off by default and independent of `--debug`. Not for hosted deployments: the server refuses to start in hosted mode (`--hosted`, `--production`, or server mode `hosted` or `production` from `settings.yaml` or `SCION_SERVER_MODE`) when this flag is set.
        - `--test-mode <level>:<hub id>`: Test identity level of a designated test hub: `off` (default), `member`, `admin` or `super-admin` (only on a binary whose test-admin policy classification is complete). Every level except `off` requires the hub id binding, an explicitly configured `server.hub.hub_id` (or `SCION_SERVER_HUB_HUBID`; on a hub that ran with a derived id, set it to its current id, see "Test mode (operators)" in the API reference), `server.hub.class: test`, a test infrastructure marker for this hub id (written offline with `scion server test-infra mark`) and no `--production`; `admin` adds further requirements. A level whose requirements are not all met refuses to start, and so does an unrecognised value (an empty value means `off`). Replaces `--enable-test-identities`, `--enable-test-admin-identities` and `--enable-test-super-admin-identities`, which now fail at startup. See "Test mode (operators)" in the [API reference](/scion/reference/api/).
        - `--admin-emails <emails>`: Email addresses to auto-promote to the administrator role. This flag is **repeatable** and also accepts a **comma-separated list** (e.g. `--admin-emails admin1@example.com,admin2@example.com --admin-emails admin3@example.com`). Strict empty-value validation is enforced.
- `scion server start` with no flags runs the **workstation server** (Hub, Runtime Broker and Web on 127.0.0.1, SQLite, dev auth) as a daemon. While the Hub's onboarding is not complete, it prints and opens the [onboarding wizard](/scion/getting-started/onboarding/). Without an `image_registry`, a workstation server starts the Hub and the web UI but not the Runtime Broker, and prints the fix; run `scion server restart` after setting the registry (a registry saved in the setup wizard counts).
- `scion server restart`, `scion server status`: Restart the daemon, or show whether it is running.
- `scion server install`: Print a systemd (Linux) or launchd (macOS) service file that runs the server at login. See [Workstation Server Mode](/scion/workstation/workstation-server/).
- `scion server stop`: Stop the server daemon. It sends an interrupt and waits up to 30 seconds for the process to exit; the PID file is removed only once it has. If the process is still running after 30 seconds, the command fails and keeps the PID file, so the process can still be found (for example by `scion server stop --force`). `--force` also stops any process listening on the server ports, even without a PID file. A server in the foreground stops on Ctrl-C. SIGTERM runs the same graceful shutdown as SIGINT. A Hub that fails to start exits with status 1 after flushing its logs.
- `scion server migrate`: Copy all Hub data, including agent session metrics, from a SQLite database into a PostgreSQL database. Parents are copied before children, rows whose primary key already exists in the destination are skipped (so a failed run can be restarted), the SQLite file is opened read-only (so a running SQLite Hub can stay up until you cut over), and row counts are compared after each table, stopping on any mismatch. A restart expects the destination to hold only rows from an earlier run; rows changed or deleted in the source since then are not reconciled, and a row-count mismatch stops the run. See [PostgreSQL](/scion/hosted/single-node/hub-server/#postgresql-production).
    - Flags:
        - `--from <dsn>` (required): Source SQLite database, for example `sqlite:///var/lib/scion/hub.db` (also `sqlite:`, `file://`, `file:` forms or a bare path).
        - `--to <dsn>` (required): Destination PostgreSQL connection string, for example `postgres://user:pass@host:5432/db?sslmode=require`.
        - `--keep-source`: Accepted for clarity; the source is always kept unless you pass `--drop-source`.
        - `--drop-source`: Delete the source SQLite file after a successful, verified migration.
        - `--batch-size <int>`: Maximum rows per bulk insert (`0`, the default, uses the built-in size).
- `scion server backfill`: Scan historical messages that predate the conversation model and assign them to conversations based on their thread, sender, and recipient metadata.
    - **Safety Default (Dry-Run):** By default, the command runs in DRY-RUN mode — scanning and reporting what would change; it removes or rewrites no rows but still applies the schema migration. You must explicitly pass `--execute` to apply changes.
    - **Idempotency:** The backfill is idempotent: messages already attributed to a conversation are skipped, making re-running entirely safe.
    - **Compound-Cursor Resumability (DEF-81):** Supports resuming interrupted runs via `--checkpoint`. The resume checkpoint uses a compound `(created, id)` keyset cursor (instead of a strictly-greater-than timestamp) to guarantee zero permanent row loss on resume, even for messages with identical timestamps.
    - Flags:
        - `--execute`: Apply changes (default: dry-run, safe).
        - `--project <string>`: Scope backfill to a specific project ID (default: all).
        - `--batch-size <int>`: Number of messages to process per batch (default: 100).
        - `--checkpoint <string>`: Resume from this pagination cursor (project-scoped).
        - `--db <string>`: Database DSN (overrides configuration/environment DSN).

## Administration

### `scion admin`

Administrative operations for emergency recovery scenarios. These commands connect directly to the database, bypassing the running server. They are intended for break-glass situations where normal admin access has been lost.

### `scion admin promote`

Promotes an existing user to the admin role by connecting directly to the database, bypassing the running Hub server. This is a break-glass recovery command for situations where all admin users have been removed or an organization has lost admin access.

The target user must already exist in the database — this command does not create new users.

**Usage:** `scion admin promote [flags]`

- **Flags:**
    - `--email <string>`: Email address of the user to promote (required).
    - `--db <string>`: Database URL or path (overrides the config-derived connection). Accepts Postgres connection strings (`postgres://...`) or SQLite file paths.
    - `--config <string>`: Path to server configuration file (defaults to the standard `settings.yaml` resolution).

**Examples:**

```bash
# Promote using the default config-derived database connection
scion admin promote --email user@example.com

# Promote with an explicit Postgres database URL
scion admin promote --email user@example.com --db postgres://user:pass@host:5432/db

# Promote using a specific config file
scion admin promote --email user@example.com --config /path/to/server.yaml
```

:::caution[Break-glass only]
This command modifies the database directly. Use it only when normal admin access through the Hub API or Web Dashboard is unavailable. Under normal operation, manage admin roles via the Web Dashboard Users list or the `admin_emails` server setting.
:::

## Miscellaneous

### `scion doctor`

Checks this machine and, when a Hub is configured, the Hub and the platform it manages. It is for
people checking that Scion is set up and healthy, or troubleshooting agents, from their own
machine. It is not a self-check for ordinary agents (see `sciontool doctor` below), and it is not
available inside agent containers.

**Usage:** `scion doctor [flags]`

**Flags:**

- `--verbose`: Add the per-runtime-broker and per-Hub-instance tables, agent counts by phase,
  and integration details to the Platform Health section. Text output only: JSON output carries
  the full summary under `healthSummary` instead.
- `--format json`: Print one JSON object and nothing else on stdout (see below).

**Sections:**

- **General**: Git and tmux on this machine.
- **Hub Health**: Hub reachability (`/healthz`), authentication, unmapped GCP service accounts of
  the linked project, and checks of this machine's settings (NFS mounts, Kubernetes profiles,
  the deprecated server hub endpoint source on hosts configured as a server, telemetry, container
  images, broker credentials).
- **Platform Health**: The Hub's health summary (`GET /api/v1/admin/health/summary`), the same
  data as the [health dashboard](/scion/workstation/dashboard/). It shows the overall status, then
  every "Needs attention" item in the Hub's order (critical items as failures, warnings as
  warnings), then one line each for the Hub fleet, runtime brokers, agents, dispatch and
  integrations, the service account check when it cannot run, and the test identity settings when
  any is on. Doctor shows the summary as the Hub computed it and does not recompute the status.
  - Reading the summary needs the `hub.health.read` permission (hub admins). Without it, the
    section shows `requires hub.health.read`, and Hub Health checks runtime brokers and agents
    from the lists you can see instead. That agent check uses the same rule as the dashboard:
    agents in error, crashed or offline count (stopped agents are ignored), and stalled agents do
    not.
  - The summary is read whenever the Hub answers, even if its `/healthz` reports it unhealthy.
    Text that comes from the Hub, such as runtime broker names and integration reasons, is shown
    with control characters removed.
  - If the Hub instance cannot read its database, the section shows "Health data not available
    (database unreachable from this hub instance)" and the list-based checks run.
- **Runtime**: The active container runtime: the Docker or Podman daemon, the Apple container CLI,
  or for Kubernetes and Cloud Run the runtime's own diagnostics (cluster connectivity, namespace
  access, RBAC permissions, CSI drivers).

**Exit status:** doctor reports check results and does not turn them into an exit status. It
exits zero whenever it ran, including when the Hub is unreachable or `/healthz` reports it
unhealthy, when the health summary status is `unhealthy`, or when a runtime diagnostic check
fails; read the results from the text output or from `status` in the JSON output. It exits
non-zero only when the command itself fails, for example on an invalid flag or when it cannot
write its output.

**JSON output:** `--format json` prints a single object for every runtime:

```json
{
  "status": "warn",
  "general": [{"name": "git", "status": "pass", "message": "git version 2.43.0"}],
  "hubChecks": [{"name": "hub-connectivity", "status": "pass", "message": "..."}],
  "platformHealth": [{"name": "health-summary", "status": "warn", "message": "Platform is degraded (1 item(s) need attention)"}],
  "healthSummary": {"status": "degraded", "attention": [], "...": "..."},
  "runtime": "docker",
  "checks": [{"name": "runtime", "status": "pass", "message": "Active runtime: docker"}]
}
```

`status` is `fail` when any check failed, `warn` when any check warned, and `pass` otherwise. It
does not affect the exit status. `healthSummary` is the Hub's summary response unchanged, and is
omitted when it could not be read. `runtime` is omitted when no project was found.

**Platform Health for an assistant agent:** an assistant agent that a hub admin has authorized
to monitor the platform runs `scion doctor` from an environment a person owns, not from inside an
agent container (agent containers do not offer the command, and the Hub accepts the health
summary only from user credentials). A hub admin mints a hub-boundary token and the environment
uses it with no stored login:

```bash
scion hub token create --boundary hub --name health-check \
  --scopes hub_health:read,broker:read,agent:list --expires 30d
SCION_HUB_TOKEN=scion_pat_... scion doctor --format json
```

`broker:read` and `agent:list` are for the list-based checks; add `hub_integrations:read` to see
integration names.

:::note[In-container diagnostics]
A separate **`sciontool doctor`** command runs *inside* an agent container and diagnoses the
agent's own health — environment variables, Hub token (presence/format/expiry), Hub reachability,
token validity (a read-only check that does not refresh or revoke the token), the GCP metadata server, and the GitHub App token. See
[Harness Authentication](/scion/local/agent-credentials/#diagnostics).
:::

### `scion whoami`

Prints identity details of the current agent container or system user.

- **Outside an agent container**: Gracefully falls back to the system `whoami` command (e.g., printing the current operating system user).
- **Inside an agent container**: Prints the agent's identity. By default, it prints the agent's slug in plain text.

**Usage:** `scion whoami [flags]`

- **Flags:**
    - `--full`: Enriches the output with live metadata from the Hub. 
        - When run in plain text, prints a human-readable multi-line summary of both local environment settings and live Hub details (e.g., agent phase, activity, etc.).
        - When run with `--format json`, includes both Tier 1 and Tier 2 fields in the JSON object.
        - If the Hub is unreachable, the command gracefully degrades to returning environment-only details, printing a warning to `stderr`.
    - `--format json` (persistent global flag): Returns a structured JSON output representing the agent's identity configuration and state.

#### Output Details (`--format json`)

The command populates a structured JSON schema divided into two latency tiers:

##### Tier 1 (Always populated, zero latency, derived from container environment variables)
- `slug`: The agent's slug identifier (falls back to its name if slug is absent).
- `name`: The agent's display name.
- `id`: The unique agent UUID.
- `project` / `projectId`: The assigned project's name and ID.
- `template`: The name of the template used to spawn the agent.
- `harness`: The underlying agent harness (e.g., `codex`, etc.).
- `model`: The model used by the agent.
- `creator`: The identity of the agent's creator.
- `brokerName` / `brokerId`: The running broker's name and ID.
- `cliMode`: The active CLI mode.
- `hubEndpoint`: The endpoint URL of the connecting Hub.
- `hubUrl`: The reconstructed direct URL pointing to the agent's resource page on the Hub (`{hubEndpoint}/agents/{id}`).

##### Tier 2 (Enriched, requires `--full` flag and Hub connectivity)
- `phase`: The current lifecycle phase of the agent (e.g., `running`, `suspended`, `error`).
- `activity`: The current runtime activity status of the agent (e.g., `thinking`, `stalled`, `offline`).
- `labels`: Metadata key-value pairs assigned to the agent.
- `annotations`: System key-value pairs attached to the agent.
- `ancestry`: A lineage array representing the agent's parent/child spawn relationships.
- `taskSummary`: A brief summary of active task execution.
- `messageMode`: The agent's effective [message mode](/scion/hosted/user/messaging/#message-authorization--modes) (`none`, `lineage`, `branch`, `project` or `hub`), shown as `Msg Mode:` in plain text.
- `experiments`: The hub experiments that are on now, read live from `GET /api/v1/experiments` (shown as `Experiments:` in plain text, `(none)` when none is on). It includes server-checked experiments that also have a web layer, such as `hub.artifacts`, so an agent can tell whether artifacts are available. The key is absent when the Hub could not be asked. The agent environment variable `SCION_HUB_EXPERIMENTS` lists only the experiments dispatched to agents, fixed when the agent starts, so it is not the full list.

##### Token scopes (requires `--full`; read locally, no Hub connectivity needed)
- `scopes`: The scopes of the agent's current Hub token, decoded from the token itself (shown as `Scopes:` in plain text). For example, an agent without `project:artifact:write` cannot publish artifacts; see [Artifacts](/scion/reference/artifacts/).

### `scion version`

Prints the Scion version information and optionally checks for available updates.

**Usage:** `scion version [flags]`

- **Flags:**
    - `--check`: Query the release manifest (`LATEST.json`) for available updates across release channels (stable, preview, nightly). Outputs update availability in plain text by default, or structured JSON when combined with `--format json`.


