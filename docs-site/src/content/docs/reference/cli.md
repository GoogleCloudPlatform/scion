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
- `--hub <url>`: Hub API endpoint URL (overrides `SCION_HUB_ENDPOINT`).
- `--no-hub`: Disable Hub integration for this invocation (local-only mode).
- `-y, --yes`: Skip confirmation prompts.
- `--non-interactive`: Full non-interactive mode (implies `--yes`, errors on ambiguous prompts).
- `--debug`: Enable verbose debug output.
- `--tz <IANA zone>`: Show times in this time zone, for example `America/New_York`. Defaults to the local zone (which honors `TZ`). `Local` and invalid names are rejected.
- `--utc`: Show times in UTC. Takes precedence over `--tz`.

Human-readable times use a 24-hour clock and always include a zone. `--tz` and `--utc` only change human-readable output: JSON output (`--format json`) keeps the API's UTC values.

**Project resolution order.** The CLI picks the project in this order:

1. The `-g` / `--project` or `--global` flag.
2. Inside a Hub-connected agent container only: the agent's own project (`SCION_PROJECT_ID`).
3. The project `.scion` directory found from the current directory.
4. The global project (`~/.scion`).

A context is Hub-connected when `SCION_HUB_ENDPOINT`, `SCION_HUB_URL` or `SCION_PROJECT_ID` is set
in the environment, as in an agent container. Enabling the Hub in a workstation's settings does not
make it a Hub-connected context: there, `--global` uses the local global directory.

An explicit flag wins over `SCION_PROJECT_ID`. This applies to commands that go through the Hub
pre-flight check, and to the `conversation`, `notifications` and `messages` commands.
`SCION_PROJECT` does not select a project; it is used only to detect a send within the same
project.

In a Hub-connected context, `--global` (or `-g global`) targets the Hub's Global project (slug
`global`) when the local global directory is not linked to a Hub project. If the Hub has no Global
project, or you do not have access to it, the command fails with an error that names
`--project <slug|id>` as the alternative.

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

**In Hub mode**, starting an existing **stopped** agent restarts it in place
(printed as "Restarting", with a fresh session), the same as `scion resume`.
Starting an **error**-phase agent still fails with a conflict: use
`scion resume --force` or delete it first. When an existing agent is reused in
place, the Hub applies only the task and `--attach`. Other configuration flags
you set explicitly (for example `--type`, `--image`, `--harness-config`,
`--broker`, `--label` or `--no-auth`) are not applied, and the CLI prints a
warning that names them. `--no-auth` on an existing agent is tracked in
ptone/scion#1855.

**Usage:** `scion start <agent-name> [task] [flags]`

- **Arguments:**
    - `<agent-name>`: Unique name for the agent instance.
    - `[task]`: (Optional) The initial instruction/task for the agent.
- **Flags:**
    - `-b, --branch <string>`: Target branch for the agent workspace.
    - `-t, --type <string>`: Template to use (default "gemini").
    - `-i, --image <string>`: Override container image.
    - `-a, --attach`: Attach to the agent immediately after starting.
    - `--no-auth`: Disable authentication propagation (also sent to the Hub in Hub mode; applies when the agent is created).
    - `-d, --detached`: Run in detached mode (default true).
    - `--config <path>`: Path to inline agent config file (YAML/JSON) for Just-In-Time (JIT) overrides, or `-` for stdin.
    - `--harness-config <string>`: Named harness configuration to use.
    - `--thinking-level <value>`: Thinking level to inject into the agent config: an integer from 0 to 100, or a case-insensitive shorthand: `low` (25), `medium` (50), `high` (75), `max` (100). The level is stored as an integer; each harness maps it to its own tiers (see [Thinking Level Map](/scion/reference/harness-settings/#thinking-level-map-thinking)).
    - `--harness-auth <string>`: Override auth method for the harness. Universal types: `api-key`, `oauth-token`, `vertex-ai`, `auth-file` (each harness accepts a subset — see [Harness Authentication](/scion/local/agent-credentials/)).
    - `--broker <string>`: Preferred runtime broker ID, name, or slug for execution. In Hub mode, a broker that does not exist fails with `runtime_broker_not_found` (404), and the message lists the brokers you can use.
    - `--message-mode <mode>`: Set the agent's initial message mode (`project`, `branch`, `lineage`, `none`, or `hub`). Defaults to `project`. See [Message Authorization & Modes](/scion/hosted/user/messaging/#message-authorization--modes).
    - `--notify`: Get notified via the browser or system when the spawned agent reaches a terminal state.
    - `--no-wait`: *(Hub mode)* Return as soon as the Hub accepts the launch instead of waiting for the agent to reach `running`. Ignored with `--attach`.
    - `--wait-timeout <duration>`: *(Hub mode)* How long to wait for the agent to start (for example `10m`); must not be negative. The default is the Hub's remaining launch budget plus 30 seconds, or 5 minutes when the Hub does not advertise one. Raise it if the Hub's launch timeout has been raised.

In Hub mode, `start` waits for the agent to reach `running` when the Hub
launches it asynchronously, after a workspace upload, or with `--attach`.
Otherwise (for example when the Hub does not launch asynchronously) it returns
as soon as the Hub answers, as before, possibly while the agent is still
provisioning. While waiting, each launch step is printed to stderr (nothing
extra under `--format json`). If the wait times out, or you press Ctrl-C, only
the wait stops: the launch continues on the Hub, and re-running
`scion start <agent-name>` resumes waiting. Ctrl-C exits with status 130 and
SIGTERM with 143; a failed launch or a timeout exits 1. Network errors and
Hub answers of 5xx, 408 or 429 are retried while waiting; any other 4xx (for
example 401 or 403) stops the wait at once with the Hub's error, and the launch
continues on the Hub. If the agent's create did not
complete (for example the image could not be pulled), the error shows the
stored template and task. Delete the agent and create it again
(`scion delete <agent-name>`, then `scion start` with the same template and
task). If soft-delete retention is enabled on the Hub, the name stays reserved
until the agent is deleted with force=true or purged; until then, use a new
name. With `--format json`, `--attach` after a workspace upload attaches
without printing the JSON result.

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

In Hub mode, `scion list` and the Web Dashboard's status badges show such an agent
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
    - `--harness-config <string>` (alias `--harness`): Named harness configuration to use.
    - `--harness-auth <string>`: Override auth method for the harness (`api-key`, `oauth-token`, `auth-file`, `vertex-ai`).
    - `--broker <string>`: Preferred runtime broker ID or name.
    - `--label <key=value>`: Label for the agent (repeatable).
    - `--role <string>`: Agent role for Hub API access (`none`, `readonly`, `baseline`, `full`).
    - `--message-mode <mode>`: Set the agent's initial message mode (`project`, `branch`, `lineage`, `none`, or `hub`). See [Message Authorization & Modes](/scion/hosted/user/messaging/#message-authorization--modes).
    - `--service-account <string>`: GCP service account ID to assign (Hub mode).
    - `--upload-template`, `--no-upload`, `--template-scope <scope>`: Template upload behavior in Hub mode.

### `scion stop`

Stops a running agent. This is a graceful shutdown (`SIGTERM`); the agent's
phase becomes `stopped` and the next `start` runs a fresh session.

In Hub mode, stopping an agent whose Runtime Broker is offline queues the stop: the CLI prints
"stop queued via Hub", and the Runtime Broker applies the stop when it reconnects, unless a newer
start or stop was recorded first. With `--rm`, a queued stop does not remove the agent; run
`scion delete` once the Runtime Broker is back. Any warnings the Hub returns are printed.

**Usage:** `scion stop <agent-name> [flags]`

- **Flags:**
    - `--rm`: Remove the agent after stopping. In Hub mode the command waits for the Hub to
      confirm the removal, as [`scion delete`](#scion-delete-or-rm) does, and prints that removal
      is in progress if the Hub answers `202`. Once the removal is confirmed, the agent's local
      files (agent directory and worktree) are removed too, as with `scion delete`; its git branch
      is kept. If the Hub accepts the removal but cannot confirm it, or the removal fails, local
      files are left in place. If the local cleanup itself fails, the command still succeeds and
      prints a warning suggesting `scion --no-hub delete --preserve-branch <agent-name>`.
    - `-a, --all`: Stop all running agents in the current project. If any agent fails to stop
      (or, with `--rm`, to be removed), the command exits 1. With `--format json` the result
      object (`"status": "partial"`) is still printed on stdout, and no separate error message is added.

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
    - `-a, --all`: Suspend all running agents in the current project. Agents
      whose harness does not support resume are skipped.

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
    - `--no-wait`, `--wait-timeout <duration>`: *(Hub mode)* Same as for [`scion start`](#scion-start-or-run).

### `scion attach`

Connects to the interactive session of a running agent.

When connecting to a Hub behind Google Identity-Aware Proxy (IAP), `scion attach` automatically attempts to resolve transport-layer authentication (Google OIDC ID tokens) *before* evaluating the application-level access token gate. If transport auth can be successfully established (e.g., using your local Google Cloud SDK identity or GKE Workload Identity), the application token check is bypassed, enabling seamless attachment in proxy-auth/IAP mode.

If the agent is stopped, the attach ends immediately rather than waiting and retrying (see [PTY close codes](/scion/reference/api/#pty-close-codes)).

**Usage:** `scion attach <agent-name>`

- **Key Bindings:**
    - `Ctrl-b`, then `d`: Detach from the session without stopping the agent (the tmux detach key; see [Interactive Sessions with Tmux](/scion/local/tmux/)). The container runtime's default `Ctrl-p Ctrl-q` detach sequence is not used, so `Ctrl-p` reaches the agent. Podman's detach keys are off. Docker's are moved to `Ctrl-\` then `Ctrl-^`: a single `Ctrl-\` is delayed until the next key, and the full sequence ends the attach while the agent keeps running (see [Interactive Sessions with Tmux](/scion/local/tmux/#basic-operations)).
- **Requires a terminal:** `scion attach`, `scion start --attach` and `scion resume --attach` need an interactive terminal on both stdin and stdout. Without one (for example from a script or a coding harness) they fail at once with `attach requires an interactive terminal` and a non-zero exit, before starting anything. Use `scion look` to view a session and `scion message` to send input instead.
- **Not running:** if the Hub reports the agent as not running, `scion attach` names the next step for the agent's phase: `scion resume <agent> --attach` for a stopped or suspended agent, waiting and then resuming for a stopping agent, `scion logs <agent>` and then resuming for an agent in the `error` phase, and `scion start <agent> --attach` for anything else. An agent whose runtime doesn't support attach gets that error first, whatever its phase.
- **No reconnect:** in Hub mode `scion attach` does not reconnect. When the session ends for any reason other than a detach, the command exits non-zero with a message that says what happened and what to run next, based on the [PTY close code](/scion/reference/api/#pty-close-codes). For example, a dropped runtime broker (`4503`) suggests running `scion attach` again, and an ended session (`4410`) suggests `scion resume`. `scion start --attach` and `scion resume --attach` use the same attach flow and print the same messages.
- **Hub URL with a path prefix:** a Hub served under a path (for example `https://example.com/scion`) works for attach as it does for other commands.

See [Attaching to a remote agent](/scion/hosted/user/hosted-user/#attaching-to-a-remote-agent) for Hub-mode details such as who can attach.

### `scion message` (or `msg`)

Sends a message to a running agent or user.

**Usage:** `scion message <recipient> <message> [flags]`

- **Recipients:**
    - `<agent-name>`: Send to an agent (default, same as `agent:<name>`).
    - `agent:<name>`: Send to an agent explicitly.
    - `user:<name>`: Send to a user's inbox. *(Hub mode only)*
    - `group[a,b,...]`: Send to multiple recipients. *(Hub mode only)*
    - `@<agent-name>`: Send to an agent's conversation (preferred).
    - `@<email>`: Send to a user by email (global DM).
    - `conv:<uuid>`: Send to a conversation by ID. *(Hub mode only)*
    - `#<thread>`: Send to a named thread. *(Hub mode only)*

- **Arguments:**
    - `<recipient>`: The recipient (see above).
    - `<message>`: The text to send. Pass `-` to read the body from stdin.
- **Flags:**
    - `-i, --interrupt`: Interrupt the harness before sending the message.
    - `-w, --wake`: Resume a suspended agent before delivering the message. Requires the permission to start the agent (`agent.lifecycle`); without it the send fails with `403` and the agent stays suspended.
    - `--body-file <path>`: Read the message body from a file instead of passing it inline. Useful for long messages and scripted workflows. `--body-file -` reads the body from stdin, like a `-` message argument. Mutually exclusive with the inline `<message>` argument.
    - `--attach <path>`: Attach one or more file paths (repeatable). File paths must be within allowed roots (`/workspace` or `/scion-volumes`), where relative paths resolve against `/workspace`.
        - **Constraints:** Cannot be combined with `--in` or `--at`.
        - **Requirements:** Requires Hub mode (`scion hub enable`). If run in local mode, the command will fail with an error suggesting you include file contents directly in the message text. If the file is not a regular file (e.g., is a directory) or is outside allowed roots, the command will fail.
    - `--cc <agents>`: *(Deprecated — will be removed.)* Carbon copy additional agents. This flag is **repeatable** and also accepts a **comma-separated list** of agent names (e.g., `--cc dev-agent,qa-agent --cc test-agent`). Use `group[...]` addressing or body `@mentions` instead.
    - `--notify`: *(Deprecated — use `scion notifications subscribe` instead.)* Get notified when the target agent(s) respond or reach a terminal state after receiving the message.
    - `--plain`: *(Deprecated — will be removed.)*  Mark for plain-text delivery.
    - `--channel <channel>`: *(Deprecated — address the conversation with `conv:<uuid>` instead, or use `@<name>` to message an agent.)* Target a specific message channel (e.g., `telegram`, `gchat`, `teams`, `web`).
    - `--thread-id <id>`: *(Deprecated — address the conversation with `conv:<uuid>` instead; `scion conversation list` shows conversation IDs.)* Target a specific thread ID within the channel. For `user:` recipients on the web channel, the thread must already exist as a conversation: the Hub rejects an unknown or deleted thread ID (HTTP 422) instead of creating a new conversation, and the command exits 1. External channels (for example Slack, Teams, or Google Chat) deliver to the thread ID themselves and are not checked. On success, a `user:` send prints the conversation the message was recorded in.
    - `--raw`: *(Removed — use `scion keys` instead.)* Raw keystroke delivery through messages has been removed. Any use of `--raw` (with any value, target, or mode) fails before anything is sent, with an error pointing to `scion keys`. The Hub likewise rejects a message request carrying the retired `raw` field with `422 raw_input_removed`.
    - `--in <duration>`: *(Deprecated — use `scion schedule create --in` instead.)* Schedule message delivery after a duration.
    - `--at <time>`: *(Deprecated — use `scion schedule create --at` instead.)* Schedule message delivery at an absolute time.

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

### `scion keys`

Sends literal terminal input to an agent's tmux session via `send-keys`, with no trailing Enter and no envelope. It is a distinct operation from messaging, authorized like terminal attach rather than like `scion message`.

**Usage:** `scion keys <agent-name> <keys>`

**One argument, one tmux argument — not a sequence.** The entire `<keys>` string becomes exactly one `send-keys` argument, in local mode and Hub mode alike. tmux recognizes a small set of named keys (`Enter`, `Escape`, `C-c`, arrow names, etc.; `Enter` is the canonical spelling) only when the *whole* argument matches one name exactly; anything else — including a string containing spaces — is typed character by character. `scion keys my-agent "Up Up Enter"` therefore sends the eleven literal characters `U`, `p`, ` `, `U`, `p`, ` `, `E`, `n`, `t`, `e`, `r`, not three key presses. The command takes exactly two arguments, so leaving the keys unquoted (e.g. `scion keys my-agent Up Up Enter`, four arguments) is rejected outright with an argument-count error rather than silently reinterpreted — quote any `<keys>` value containing spaces. To send multiple key presses, issue separate `scion keys` calls, one per press — there is no atomic multi-key sequence form.

```bash
scion keys my-agent "Escape"
scion keys my-agent "C-c"
scion keys my-agent "Enter"
```

**Project isolation.** When run by an agent, `scion keys` can only target agents in the agent's own project: cross-project targets are refused, both by the CLI itself and by the Hub's `/keys` authorization. A human operator using `--project` can target other projects — there is no blanket human cross-project ban — but in Hub mode needs attach authority on the target (the same authority as `scion attach`), not message authority. Local mode has no Hub authorization.

**In Hub mode**, `scion keys` calls the Hub's dedicated keys operation described in [API Reference](/scion/reference/api/#agents-apiv1agents) (the project-scoped `/keys` route), never the message path. That means: input validation and bounding to 4096 UTF-8 bytes before dispatch (an empty `<keys>` string is rejected before any request is sent); authorization mirroring `agent.attach` (a human operator needs the same authority as opening a terminal — closed/`none` message mode does not block it, and message-only authority does not grant it; an agent caller additionally needs a live attach relationship on the target — see [Permissions & Policy](/scion/hosted/ha/permissions/)); a `409` rather than a wake/start for a stopped or suspended target, and a `503` (safe to retry only after fixing the route — never a `502`/`504`, which must never be retried automatically) for an unreachable broker or no immediate route; single-attempt delivery with no automatic retry, even if the client is configured with retries; no message, conversation, or terminal-content record, only content-free audit; and a `422 keys_unsupported` error for a managed-runtime target or an un-upgraded Runtime Broker — see [API Reference](/scion/reference/api/#agents-apiv1agents) for the full outcome table. A Hub predating the `/keys` route itself (rather than just an old broker behind an up-to-date Hub) answers with that Hub's own generic `404`. The CLI reports any `404`/`405` without an `operation_id` — an old Hub, or a project the Hub does not recognize — as a rejection (`hub_unsupported`), and never falls back to the message path.

**Outcome reporting.** Every attempt resolves to one of three outcomes: `dispatched` (the Runtime Broker acknowledged terminal injection — not that the harness acted on it), `rejected` (definitely not delivered; safe to correct and retry), or `unknown` (may or may not have reached the terminal; check with `scion look` before resending). With `--format json`, the CLI prints exactly one result object on success or failure, including the outcome, the machine outcome code when known, and the Hub's `operation_id` when one was returned. The outcome code is never empty or a generic placeholder: if a proxy or gateway answers for the Hub (for example a bare `502` with no body, or any response with no keys outcome code and no `operation_id`) and the status does not prove the keys were not delivered, the CLI reports outcome `unknown` with code `keys_outcome_unknown` and a message naming the HTTP status, and exits non-zero.

**In local mode**, `scion keys` uses the local keys primitive with identical tmux-argument semantics and the same input validation (including rejecting an empty string). It works for projects linked to a Hub project and for purely local projects that never ran `scion hub enable`; the target is resolved within the selected project only, and an ambiguous match fails rather than guessing. There is no Hub authorization, rate limit, or audit record, since no Hub is involved.

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

Manages conversations — the surface-agnostic containers for message threads. Requires Hub mode. Running `scion conversation` without a subcommand defaults to `list`. It also works inside Hub-connected agent containers, as does `scion notifications`; this requires a harness image built from this release or later.

Conversations are referenced using one of three forms:

- `conv:<uuid>` — by conversation ID.
- `@<agent-name>` — resolves the direct conversation with the named agent.
- `#<thread-name>` — resolves a named group conversation.

**Usage:** `scion conversation [command] [flags]`

- **Commands:**
    - `list` (default): List conversations you participate in.
    - `get <conversation-ref>`: Show conversation details.
    - `get-message <conversation-ref> <message-id>`: Retrieve a single message by its ID from a conversation. Authorization is participant-based — only participants of the conversation can retrieve its messages.
    - `messages <conversation-ref>`: View messages in a conversation.
    - `create <name>`: Create a new group conversation. The group appears as a thread in the project's web chat space. Names must start with a letter or digit, contain only letters, digits, spaces, `_`, or `-`, and be at most 100 characters (returns `400 Bad Request` otherwise). A name already used in the project returns `409` (name conflict).
    - `set-default <conversation-ref> <agent-id>`: Set the default agent for a conversation.
    - `participants <conversation-ref>`: List participants in a conversation.
    - `join <conversation-ref> <principal-kind> <principal-id>`: Add a participant to a conversation.
    - `leave <conversation-ref>`: Leave a conversation.
    - `catch-up <conversation-ref>`: Show recent messages in a conversation.
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
    - `--json`: Output in JSON format.
- **Flags (on `catch-up`):**
    - `--since <duration>`: Show messages from this duration ago, e.g. `30m`, `2h` (default `1h`).
    - `--json`: Output in JSON format.
- **Flags (on `get-message`):**
    - `--json`: Output in JSON format.

### `scion logs`

Displays the logs of an agent.

**Usage:** `scion logs <agent-name> [flags]`

- **Flags:**
    - `-f, --follow`: Stream logs.

### `scion list` (or `ps`)

Lists all agents and their status.

**Usage:** `scion list [flags]`

`scion list` takes no positional arguments; passing one is an error. To name a reference agent for `--descendants`, `--ancestors`, or `--lineage`, use `=` (for example, `--descendants=foo`, not `--descendants foo`).

- **Flags:**
    - `-a, --all`: Show all agents (including stopped ones).
    - `-r, --running`: Filter for active (running) agents.
    - `--phase <phase>`, `--activity <activity>`, `--template <name>`, `--label <key=value>` (repeatable): Filter by attribute. Combine with each other using AND.
    - `--owner <user>` (Hub mode only): Filter by owner — a user ID, name, email, or the reserved value `me` (a user whose display name is literally "me" cannot be matched by name; use their ID or email). Owner is the direct creator (`createdBy`), which for an agent-created agent is another **agent**, not the human at the root of the tree — `--owner alice` does not include agents created by alice's agents, and `--owner me` when authenticated with an agent token means "agents I directly created."
    - `--broker <name|id>` (Hub mode only): Filter by runtime broker name or ID.
    - `--harness <harness-config name>` (Hub mode only): Filter by harness-config name.
    - `--descendants[=<agent>]` (Hub mode only): List every agent descended from the reference. With no value, the reference is the calling agent in agent mode, or the calling user otherwise (a user's ID is recorded as the creator in its directly-created agents' ancestry, so this still works).
    - `--ancestors[=<agent>]` (Hub mode only): List the agents named in the reference's ancestry chain (entries that name a user rather than an agent are skipped). Same reference-resolution rule as `--descendants`. A user reference has no ancestry, so this returns an empty list when the reference defaults to the calling user.
    - `--lineage[=<agent>]` (Hub mode only): List the reference's **creation-tree neighborhood** — the reference's direct parent agent, plus everything created (at any depth) from that parent — bounded to the reference's own project. The exact root rule: it roots at itself when it was created directly by a user, has no recorded parent at all, **or its only recorded parent is an agent you cannot list** (without `--all`, that includes a parent that is merely in a different project — the lookup for a length-one ancestry goes through the current project's endpoint, not the global one). For a deeper ancestry (two or more recorded ancestors), the direct parent is always used as the root even if you cannot see it yourself, so its other children (your siblings) that you *can* see are still listed — this is safe because the ancestry length alone already proves that entry is an agent, not a user. Same reference-resolution rule as `--descendants`. For a user reference (e.g. bare `--lineage` in human/assistant mode with no agent to infer), there is no project to bound to, so the result is identical to `--descendants` for that user: every agent they've created, at any depth, across whatever project scope is already in effect. **This is a creation-tree query, not a messaging-permission query** — it does not reflect who the reference agent is allowed to message under its message mode (`scion set-message-mode`), which can be a different and smaller set.
    - `--descendants`, `--ancestors`, and `--lineage` are mutually exclusive with each other. All of the above combine with `--phase`/`--activity`/`--template`/`--label` using AND.
    - Without `--all`, every Hub-mode filter above (including `--descendants`/`--ancestors`/`--lineage`) is scoped to the **current project** — a reference agent's ancestors/descendants/lineage in a different project will not appear. From a human or assistant shell, `--all` searches across every project you can see. **When authenticated with an agent token, `--all` cannot be combined with `--descendants`/`--ancestors`/`--lineage`**: an agent token can only list agents in its own project, so the command fails with a clear error instead of silently returning nothing. This is keyed on the credential actually in use, not on CLI mode — running inside an agent container while authenticated as a user (OAuth login, or dev auth against a localhost Hub) is not affected.

### `scion delete` (or `rm`)

Deletes an agent, removing its container, home directory, and worktree.

You can name several agents. If any of them cannot be deleted, the command exits 1 after
handling the rest. With `--format json` the result object (`"status": "partial"`, with an entry
per agent) is still printed on stdout, and no separate error message is added.

**Usage:** `scion delete <agent-name>... [flags]`

- **Flags:**
    - `-b, --preserve-branch`: Preserve the git branch associated with the worktree (default: deleted).
    - `--stopped`: Delete all agents with stopped containers.
    - `-f, --force`: Remove the agent from the Hub even when its Runtime Broker cannot be reached or cannot resolve it. A forced delete is permanent: it skips soft-delete retention, so the agent cannot be restored. Runtime resources on the Runtime Broker (containers, worktrees) may need separate cleanup on that Runtime Broker. `--force` does not purge an agent that is already soft-deleted. Applies to every named agent; it cannot be combined with `--stopped` (name the agents to force-delete instead). In local mode (no Hub), `--force` has no effect and the CLI prints a warning; the local delete already removes the container.

In Hub mode the Hub may answer `202` when teardown is still running (see [`DELETE /agents/:id`](/scion/reference/api/)). The CLI then polls the agent every 2 seconds for up to 180 seconds. It removes the local worktree only after the Hub confirms the delete:
- **Delete failed:** the worktree is kept and the command exits non-zero.
- **Poll timed out or the agent can't be read (for example a `403`):** the worktree and the sync state are kept. The command prints that removal is pending and exits 0.
- With `--format json`, a pending delete reports `"status": "accepted"` and `"worktreeKept": true` (`scion stop --rm` reports `"removalPending": true`).

### `scion sync`

Synchronizes the agent workspace between the host and the container.

**Usage:** `scion sync [to|from] <agent-name> [flags]`

- **Flags:**
    - `--dry-run`: Preview changes without syncing.
    - `--exclude <glob>`: Exclude files matching the pattern.

### `scion reset-auth`

Injects a fresh Hub token into a **running** agent's container and signals it to reload, without
restarting the agent. Use this to recover an agent whose token expired and cannot self-refresh
(e.g. after a Hub signing-key rotation). Requires a Hub connection. The same action is available
as a **Reset Auth** button in the web UI. The token is passed to the container over stdin, not on the
command line, so it does not appear in the host's process list.

**Usage:** `scion reset-auth <agent-name>`

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

Reincarnation works for agents in clone-per-agent, shared-workspace (shared-plain), and
Hub-managed workspaces. For a shared-workspace agent, the agent record, identity, and shared
checkout are preserved, and sibling agents sharing the checkout are not restarted. Agents in
worktree-per-agent projects and agents in empty-per-agent projects are not yet supported; the Hub
rejects the request with `400 Bad Request`. Reincarnating another agent requires the `agent.lifecycle` permission (the same
as stop, start, and restart); an agent can always reincarnate itself.

**Usage:** `scion reincarnate [agent-name] [flags]`

- **Flags:**
    - `--handoff-file <path>`: File whose content becomes the new generation's first task. Required for self-migration.
    - `--handoff-template`: Print the handoff template and exit. Ignores other flags and arguments, and does not contact the Hub.
    - `--dry-run`: Print the resolved plan (old → new template, image, harness config, model, env key names, and branch) without migrating anything.
    - `--broker <name|id>`: Move the agent to another Runtime Broker (see [Moving to another Runtime Broker](#moving-to-another-runtime-broker) below). Both Runtime Brokers must mount the same NFS export, so the workspace moves without being copied. The CLI dry-runs the move first and stops if it is refused; add `--dry-run` to only check it.
    - `--service-account <id>`: Patch the GCP service account of the new generation. Gets the same access checks as `scion create`.
    - `--role <role>`: Patch the agent role of the new generation: `none`, `readonly`, `baseline`, or `full`. Gets the same access checks as `scion create`; an agent reincarnating itself can lower its own role but not raise it.
    - `--model <model>`: Patch the model of the new generation. Model aliases are accepted, as with `scion start`.
    - `--harness-auth <method>`: Patch the harness auth method of the new generation: `api-key`, `oauth-token`, `auth-file`, or `vertex-ai`.
    - `--image <image>`, `-i`: Patch the container image of the new generation.
    - `--thinking-level <value>`: Patch the thinking level of the new generation. Accepts the same values as `scion start`: an integer from 0 to 100, or a case-insensitive shorthand: `low` (25), `medium` (50), `high` (75), `max` (100). The Hub receives the integer. Without the flag, the thinking level is not patched. An invalid value fails before contacting the Hub.

**Patch flags.** `--service-account`, `--role`, `--model`, `--thinking-level`, `--harness-auth`, and
`--image` change that setting on the new generation, and later reincarnations keep the new value.
`--dry-run` shows the old and new value of each patched setting. For a user, patch flags need
permission to update the agent (`agent.update`) as well as `agent.lifecycle`, so a user access token
cannot use them; sign in with `scion hub auth login` instead. An agent caller, including an agent
patching itself, needs the agent lifecycle permission. Before a real patched reincarnation the CLI dry-runs
the same request, and stops if the Hub does not apply the patch (upgrade the Hub). Patch flags combine
with `--broker`: the move's checks judge the patched configuration, for example the patched service
account.

Reincarnating another principal's agent makes you its recorded delegator, so you must be able to
delegate the agent's role. A caller who cannot, for example a non-admin reincarnating an agent with a
privileged role, gets `403` from this authority check. It runs before the workspace and capability
checks (`400`, `412`), so expect the `403` first.

#### Moving to another Runtime Broker

`scion reincarnate <agent> --broker <name|id>` moves the agent to another Runtime Broker that mounts
the same NFS export, keeping its ID, slug, generation chain, and workspace. The full requirements,
permissions, and operator notes are in
[Moving an agent to another Runtime Broker](/scion/hosted/ha/multi-broker/#moving-an-agent-to-another-runtime-broker).

The CLI first sends the same request as a dry run. The Hub runs nine checks in this order: workspace
mode, workspace storage reported by both Runtime Brokers, same NFS export, workspace on the export,
target profile, target health, access, agent-move capability (on both Runtime Brokers, with the source
online), and capacity. The first failing check decides the answer, with `400`, `403`, `409`, `412`,
`429`, or `503`, and the CLI prints every check as passed, failed, or not evaluated. A refused move
stops there, before anything changes. With `--dry-run` the CLI only prints the verdict and plan.

When the checks pass, the Hub accepts the move with `202 Accepted` and runs it in the background. It
stops the agent on the source, moves its quota to the target, provisions it on the target (which first
confirms through its own mount that the workspace is there), starts the new generation with the
preamble and handoff, and then removes the agent's local state from the source. A failure before the
new generation is running on the target rolls the move back: the agent is left on the source, not
running, with its previous configuration and workspace, and you can start it again or retry.

A target you cannot see is reported as not found (`404 runtime_broker_not_found`), and a name that
matches more than one Runtime Broker returns `409 runtime_broker_ambiguous` (use the ID). A target that
is the agent's current Runtime Broker is a plain reincarnation. If the CLI says the Hub does not
support `--broker`, upgrade the Hub.

A self-migration with `--broker` follows the same contract as any self-migration: the CLI prints that
the container will be stopped shortly, and does not change the agent's status. An agent can move itself
only to a Runtime Broker that already serves its project.

## Configuration & Workspace

### `scion project`

Manages the Scion workspace (Project).

- `scion project init`: Initialize a new project. By default, creates a `.scion` directory in the current directory or the root of the current git repository.
    - Flags:
        - `--global`: Initialize the global project in the home directory.
        - `--machine`: Perform full machine-level setup (seeds harness-configs, templates, settings).
        - `--image-registry <string>`: Configure the container image registry path (e.g., `ghcr.io/myorg`).
    - **Note:** If you are in a git repository, add `.scion/agents` to your `.gitignore` to avoid issues with nested git worktrees: `echo ".scion/agents" >> .gitignore`
    - **Hub Integration:** If a Hub endpoint is configured, `init` will prompt to register the new project with the Hub.
- `scion project list` (alias `ls`): List all projects known to Scion on this machine, including their type, agent count, status, and workspace path.
- `scion project prune`: Detect and remove project configurations whose workspace directories no longer exist. This stops any running containers associated with orphaned projects before cleaning up.
- `scion project status [project]` (alias `health`): Show agent status for a project from the Hub: counts per lifecycle phase and per activity, a per-agent table (template, harness, phase, activity), and troubleshooting hints for blocked, stalled, or errored agents. Uses the current project when no name is given.
    - Flags: `--all` (report across all projects on the Hub), `--json` (JSON output).
- `scion project service-accounts` (alias `sa`): Manage GCP service accounts registered for the project.
    - `add <email>`: Register an existing GCP service account.
        - Flags: `--gcp-project <id>` (required, the GCP project ID), `--name <string>` (display name).
    - `mint`: Create a new service account in the Hub's GCP project (the account ID is prefixed with `scion-`). Flags: `--account-id`, `--name`.
    - `list` (alias `ls`): List registered service accounts. Flags: `--json`.
    - `verify <id>`: Verify that the Hub can impersonate the service account.
    - `remove <id>` (aliases `rm`, `delete`): Remove a service account registration.
    - `add`, `mint`, `verify` and `list` print the Hub's warnings to stderr, for example a service account that no Kubernetes broker profile of the project maps (see [early warning for unmapped service accounts](/scion/hosted/ha/kubernetes/#gcp-identity-mode-assign-workload-identity-mapping)). Warnings never change the exit status. With `--json`, `add`, `mint` and `verify` also include the warnings in the JSON document as `warnings`; `list --json` prints only the account list.
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

### `scion user`

Manages per-user Hub settings.

- `scion user skills`: Manage auto-injected skills for your user across all projects.
    - `list` (alias `ls`): List your personal auto-injected skills.
    - `add <uri>`: Add a skill URI to your personal auto-injected list.
        - Flags: `--as <alias>` (alias under which to mount the skill), `--optional` (continue provisioning if resolution fails), `--from-directory <url>` (discover and batch-add all skills from a GitHub repository directory).
    - `remove <id|uri>` (aliases `rm`, `delete`): Remove an auto-injected skill entry from your personal list by its ID or full URI.

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
- `scion secret list`: List metadata (key, type, version, updated time) for all project-scoped secrets. Secret values are never returned.

### `scion clean`

Removes the scion project configuration from the current project or global location.

**Usage:** `scion clean [flags]`

- **Flags:**
    - `--skip-hub-check`: Skip Hub connectivity check before removing.

### `scion config`

View and modify configuration settings.

- `list`: List all effective settings.
- `get <key>`: Get a specific configuration value.
- `set <key> <value>`: Set a configuration value.
- `validate`: Validate settings files against the schema.
- `migrate`: Migrate configuration to the latest versioned format.
- `dir`: Print the path to the active configuration directory.

`config get` supports the top-level settings keys (`active_profile`, `default_template`,
`default_harness_config`, `workspace_path`, `image_registry`, `project_id`, `cli.autohelp`,
`hub.enabled`, `hub.linked`, `hub.endpoint`, `hub.local_only`, `hub.brokerId`,
`hub.brokerToken`, `hub.brokerNickname` — note that `hub.brokerToken` prints its value,
as it always has), plus dotted paths into a named entry of the `profiles` or `runtimes`
maps: `profiles.<name>.<field>` and `runtimes.<name>.<field>`, where `<field>` is one of
that entry's scalar settings named by its `settings.yaml` key (e.g.
`profiles.local.runtime`, `runtimes.kubernetes.namespace`). Within a profiles/runtimes
entry, structured fields (maps, lists, nested objects — e.g. `env`, `volumes`,
`secrets`, `harness_overrides`) and credential-like field names are not supported and
return an error rather than a partial, reformatted, or unmasked value.
`harness_configs.<name>.<field>` is not supported: harness configs are normally
resolved from on-disk harness-config directories, not merged into `settings.yaml`;
use `scion harness-config` to inspect them instead.

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

- `list`: List available templates (local, and Hub when connected), grouped by scope.
- `show <name>`: Show a template's resolved configuration.
    - Flags: `--local` (search local only), `--hub` (search Hub only).
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
- `upgrade [name]`: Add missing support files and metadata without clobbering user values. Flags: `--dry-run`, `--activate-script`, `--force`. With no name, upgrades all configs in the global directory.
- `delete <name>`: Delete a config from the Hub (does not remove local files). The web UI additionally offers an "Also delete stored files" option.

## Hub Integration

### `scion hub`

Manages connection to and interaction with a Scion Hub. Authentication lives under `scion hub auth` (there is no top-level `scion auth` command).

- `scion hub auth`: Manage Hub authentication.
    - `login`: Authenticate with Hub server (opens a browser; supports `--no-browser` for device flow and `--provider github`).
    - `logout`: Clear stored credentials.
- `scion hub token`: Manage user access tokens (scoped, revocable bearer tokens for CI/CD and automation).
    - `create`: Create a new token.
        - Flags:
            - `--project <string>`: Project ID or name to scope the token to (required).
            - `--name <string>`: Token name/label (required).
            - `--scopes <scopes>`: Scopes to grant (required). This flag is **repeatable** and also accepts a **comma-separated list** of scopes (e.g., `--scopes agent:read,agent:create --scopes agent:lifecycle`). Strict empty-value validation is enforced.
            - `--expires <duration>`: Expiry duration (e.g., 30d, 90d, 1y, default: 90d).
            - `--purpose <text>`: Optional bounded description of what the token is for (≤128 bytes, single line, no control characters). Immutable after issuance — there is no update command.
            - `--label <key=value>`: Optional bounded label (repeatable). Keys are lowercase `[a-z][a-z0-9_.-]*` (≤32 bytes); values are ≤64 bytes from a restricted charset. A set of attribution-shaped keys (e.g. `user_id`, `agent`, `actor_binding`) are reserved and rejected. Immutable after issuance.
    - `scopes`: List every scope accepted by `create --scopes`. With `--project <string>`, also report which scopes you may currently select for a token scoped to that project and, for each one you cannot, why. Supports `--json`.
    - `list`: List your access tokens.
    - `revoke <token-id>`: Revoke a token (remains visible in listings as revoked).
    - `delete <token-id>`: Permanently delete a token.
- `scion hub status`: Show the current Hub connection status.
- `scion hub notifications`: **Deprecated**. This command has been moved to the top-level `scion notifications` command group.
- `scion hub link`: Link the current local project to the Hub.
- `scion hub unlink`: Unlink the current project from the Hub locally.
- `scion hub projects` (alias `project`): List all projects registered on the Hub.
    - `create [git-url]`: Create a project on the Hub. With a git URL, the project is anchored to that repository. Without one, a Hub-managed project without git is created; `--name` is then required and `--branch` is not allowed.
        - Flags: `--slug`, `--name`, `--branch`, `--workspace-mode`, `--json`
        - `--workspace-mode <shared|per-agent|worktree-per-agent>`: The [workspace sharing mode](/scion/local/workspaces-and-sharing/#setting-the-mode-on-a-hub-project). On a project without git, `per-agent` gives each agent an empty private directory (empty-per-agent). The mode is set at create time only and cannot be changed later. The Hub validates the value: unknown values, and `worktree-per-agent` without a git URL, fail with the Hub's `400` message.
    - `info [project-name]`: Show details for a project, including its providers. Each provider shows its Runtime Broker's capacity as `(agents: count/limit)`, or `(agents: count)` when that Runtime Broker has no limit. `(not enforced)` is appended when the hub-wide switch for Runtime Broker quota enforcement is off.
- `scion hub brokers`: List all runtime brokers registered on the Hub.
- `scion hub secret`: Manage write-only secrets on the Hub.
    - `set <key> <value>`: Set a secret (supports `--allow-progeny` for user-scoped secrets).
    - `get [key]`: Get secret metadata.
    - `clear <key>`: Remove a secret.
    - `migrate`: Move existing secrets from the Hub database to GCP Secret Manager.
        - Flags: `--gcp-project <id>` (required, the GCP project ID), `--credentials <path>` (GCP credentials JSON), `--dry-run`, `--force` (re-migrate secrets that already reference Secret Manager), `--hub-id <id>` (Hub instance ID used to namespace secrets). Works from any directory; no project is required.
    - `migrate-names`: Rename legacy (pre hub-prefix) GCP Secret Manager secrets to the hub-prefixed `scion-<12-hex hub hash>-…` scheme. Idempotent; run a plain pass (or `--dry-run`) first, then a separate `--delete-legacy` pass. Does not require a project directory. See [Secrets](/scion/hosted/user/secrets/) for the IAM and rollout ordering.
        - Flags: `--gcp-project <id>` (required), `--credentials <path>`, `--dry-run`, `--delete-legacy` (delete each legacy secret after verifying its hub-prefixed copy), `--hub-id <id>` (defaults to the resolved server hub ID), `--timeout <duration>` (default `5m`), `-c, --config <path>` (server config file; must match the running hub's so hub ID resolution agrees).
- `scion hub env`: Manage environment variables on the Hub.
    - `set <key>=<value>`: Set a variable.
    - `get [key]`: Get variable values.
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

## Artifacts

### `scion artifact`

Publishes files as artifacts and fetches them by reference (`scion://artifact/<id>[@<seq>]`). Requires Hub mode and the `hub.artifacts` experiment (off by default). Available in agent mode. See [Artifacts](/scion/reference/artifacts/) for access rules and the API.

- `scion artifact publish <file>`: Publish one file as a new artifact in the current project; prints its reference and web page URL.
    - Flags: `--title <title>` (default: the file name).
- `scion artifact get <ref>`: Write an artifact's entry file to stdout.
    - Flags: `--out`, `-o <path>` (write to a file, or into an existing directory under the file's name).

## Notification Management

### `scion notifications`

Manages notifications and notification subscriptions. Requires Hub mode.

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

**Broker port.** `start` runs the broker on `--port`, else on `server.broker.port` from the global settings, else on 9800. While the broker runs, `start` keeps a record of that port (removed by `stop`). `register`, `deregister`, `status`, `stop`, `restart` and `hubs` use their own `--port` if given, else the recorded port, else the settings port, else 9800.

- `scion runtime-broker status`: Show status of the local broker server, including the projects it provides for. Providers added with `--auto-provide` are listed right away.
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
- `scion runtime-broker register`: Register this host as a Runtime Broker with the Hub. The local broker server must be running. Requires the `broker.create` permission (see [Broker Registration Permission](/scion/hosted/ha/runtime-broker/#broker-registration-permission)). Credentials are saved to `~/.scion/hub-credentials/<name>.json`.
    - `--name <name>`: Name for this Hub connection. Default: derived from the Hub endpoint.
    - `--force`: Register again even if already registered. This also issues a new broker secret.
    - `--auto-provide`: Automatically add this broker as a provider for new projects.
    - `--transport-mode <iap|cloudrun_invoker>`, `--transport-audience <audience>`: Transport auth for a Hub behind IAP or Cloud Run, saved to the credentials file (see [Transport Auth for IAP-Protected Hubs](/scion/hosted/ha/runtime-broker/#transport-auth-for-iap-protected-hubs)).
    - `--port <port>`: Port of the local broker.
- `scion runtime-broker deregister`: Remove this broker's registration from the Hub, which also removes it from every project it provides for. Deletes the local credentials for that Hub connection. Once no Hub connection remains, it also clears this broker's ID and token from global settings; other settings are kept.
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
- `scion runtime-broker withdraw`: Remove this broker as a provider from a project.

### `scion server`

Manages Scion server components (Hub and Broker).

- `scion server start`: Start one or more server components.
    - Flags:
        - `--enable-hub`: Enable the Hub server component.
        - `--enable-runtime-broker`: Enable the Runtime Broker component.
        - `--port <int>`: Port to listen on.
        - `--db <string>`: Database driver/connection.
        - `--dev-auth`: Enable dev-auth authentication.
        - `--admin-emails <emails>`: Email addresses to auto-promote to the administrator role. This flag is **repeatable** and also accepts a **comma-separated list** (e.g. `--admin-emails admin1@example.com,admin2@example.com --admin-emails admin3@example.com`). Strict empty-value validation is enforced.
- `scion server backfill`: Scan historical messages that predate the conversation model and assign them to conversations based on their thread, sender, and recipient metadata.
    - **Safety Default (Dry-Run):** By default, the command runs in DRY-RUN mode — scanning and reporting what would change without modifying the database. You must explicitly pass `--execute` to apply changes.
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

Runs host-side diagnostics: checks Git, tmux, the active container runtime, and runtime-specific
health (Docker/Podman daemon, or Kubernetes cluster/namespace/RBAC/CSI access). Supports
`--format json`.

**Usage:** `scion doctor [flags]`

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

### `scion version`

Prints the Scion version information and optionally checks for available updates.

**Usage:** `scion version [flags]`

- **Flags:**
    - `--check`: Query the release manifest (`LATEST.json`) for available updates across release channels (stable, preview, nightly). Outputs update availability in plain text by default, or structured JSON when combined with `--format json`.


