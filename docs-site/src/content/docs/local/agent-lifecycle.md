---
title: Agent Lifecycle — Suspend, Resume & Recovery
description: Pause and resume agents with their harness session intact, recover crashed agents, and understand auto-suspend of stalled agents.
---

Beyond the basic `start` / `stop` pair, Scion gives you finer control over an
agent's lifecycle: you can **suspend** an agent and later **resume** it with its
harness conversation intact, recover an agent that **crashed**, and rely on the
Hub to **auto-suspend** agents that have stalled in order to reclaim resources.

This page is for power users driving agents from the CLI. For the conceptual
model behind phases and activities, see [Core Concepts: Agent State Model](/scion/concepts/#agent-state-model).

## Stop vs. Suspend: two ways to wind down

Both `stop` and `suspend` tear down the agent's container, but they record very
different intent:

| | `scion stop` | `scion suspend` |
| :--- | :--- | :--- |
| **Phase after** | `stopped` | `suspended` |
| **Next `start`** | Fresh harness session | **Continues** the previous conversation |
| **Use when** | The task is done, or you want a clean slate | You'll come back and want the agent to pick up where it left off |
| **Harness requirement** | None | Harness must support session resume |

:::note
Suspend/resume relies on the agent's home directory persisting across the
container being torn down. On the Docker runtime, the agent home is a host
bind-mount, so this works with no extra configuration. Runtimes with an
ephemeral home (Kubernetes, Cloud Run) need durable home persistence (e.g. a
network filesystem); see [Runtime caveats](#runtime-caveats) below.
:::

## Suspend & Resume

### Suspending an agent

```bash
scion suspend <agent-name>
```

This stops the agent's container but marks its phase as `suspended` — a signal
that you intend to resume it later. Only a **running** agent can be suspended.

To suspend every running agent in the current project at once:

```bash
scion suspend --all
```

Suspend requires a harness that supports session resume. If the agent's harness
does not (for example, the `generic` harness), the command is rejected with an
error and you should use `scion stop` instead. When using `--all`, unsupported
agents are skipped rather than failing the whole batch.

**Suspend reasons and retained activities.** While an agent is in the `suspended` phase, the Hub records why it was suspended in `suspendReason` (cleared as soon as the agent leaves `suspended`):

- `user` — suspended explicitly by a user or agent (`scion suspend` or `POST /api/v1/agents/:id/suspend`).
- `idle` — suspended automatically by the Hub after going idle (see [Auto-Suspend of Idle Agents](#auto-suspend-of-idle-agents)).
- `stalled` — suspended automatically by the Hub after stalling (see [Auto-Suspend of Stalled Agents](#auto-suspend-of-stalled-agents)).

Suspending an agent also preserves a terminal or error-bearing activity (`completed`, `idle`, or `harness_error`) across the suspend instead of clearing it, so you can still tell whether a suspended agent finished its task, went idle, or hit a model error before it was suspended. `scion list` displays both next to `suspended` in the **PHASE** column, for example `suspended (idle)` or `suspended · completed (stalled)`, and the web shows the same label on the agent's status badge, with the reason explained in its tooltip and in a **Suspend reason** row on the agent page (`--activity` without `--phase` still matches active or terminal agents rather than suspended rows carrying a retained activity). For membership-loss holds (`suspension.held`), see [Agent Suspension When a Member Leaves a Project](/scion/reference/agent-suspension/).

**Auto-wake on new work (`lifecycle.idle_suspend`).** When the [`lifecycle.idle_suspend`](/scion/reference/experiments/#server-experiments-with-notes) experiment is enabled (alpha, off by default), an agent the Hub suspended on its own (`suspendReason` `idle` or `stalled`) is resumed automatically whenever a direct message, web chat message, `@`-mention, notification, or scheduled message arrives for it — no `--wake` flag or `agent.lifecycle` permission is required on the sender (messaging permission is enough). Direct messages, primary web chat targets, and scheduled messages wait for the resume and then deliver; `@`-mentions, secondary web chat targets, and notifications are accepted immediately and delivered after a background resume. Agents suspended explicitly (`suspendReason` `user`), agents suspended before `suspendReason` was recorded, and held agents are never woken automatically.

### Resuming an agent

```bash
scion resume <agent-name> [task]
```

`resume` re-launches the container and continues the prior harness conversation
by passing the harness-specific resume flag (`--continue` for Claude Code,
`--resume` for Gemini CLI, and so on). Any `[task]` arguments you supply are
appended to the resumed session as a new prompt, if the harness supports it.

| Flag | Description |
| :--- | :--- |
| `-a, --attach` | Attach to the agent's session immediately after resuming. |

### `start` and `resume` are intent-aware

You do not have to remember which command to use — Scion looks at the agent's
saved phase and does the right thing:

- **`scion start` on a *suspended* agent** performs an **implicit resume**: the
  harness session is continued, exactly as if you had run `scion resume`.
- **`scion resume` on a *stopped* agent** starts a **fresh** session — there is
  no prior conversation to continue, so it falls back to a clean start.

In other words, the *agent's phase* decides whether the session is continued or
started fresh; the command name is just a hint.

### Harness support

Session resume is a per-harness capability:

| Harness | Resume support |
| :--- | :--- |
| Claude Code | ✅ Yes (`--continue`) |
| Gemini CLI | ✅ Yes (`--resume`) |
| Generic | ❌ No — use `stop`/`start` |

## Crash Recovery: the `error` phase

When an agent's process or container exits **non-zero** — a real crash, an
out-of-memory kill, or a `SIGKILL` — the agent transitions to the `error` phase
with a descriptive message such as `Agent crashed with exit code 137`.

Scion is careful to distinguish a crash from an orderly shutdown. The harness
runs inside `tmux`, and `sciontool` recovers the real exit code when the session
ends, then classifies it:

| Outcome | Phase | Activity |
| :--- | :--- | :--- |
| Clean exit (code 0) | `stopped` | — |
| Limits reached (turns, model calls, or duration) | `stopped` | `limits_exceeded` |
| Crash / OOM / `SIGKILL` (non-zero) | `error` | — (cleared) |

A crash surfaces as the **`error` phase** — the activity is cleared, and the
crash detail is carried in the agent's message, with the last lines the
harness printed (up to five, on one line and redacted for credentials; the
whole message is cut to 300 characters by dropping the oldest output first),
e.g. `Agent crashed with exit code 1: Error: unknown option
'--frob'`. `scion list` shows the exit reason next to the phase when one is
recorded, e.g. `error (crashed)`. A harness that exits within its first moments
(a missing binary, a wrapper script that fails) is reported the same way, with
its own exit code; if the harness never started at all, the message reads
`Agent failed to start (exit code 1): …`. A failed pre-start hook or harness
provisioning step leaves the agent in `error` with the message
`pre-start hook failed: <reason>` on every runtime. On Kubernetes, a container killed for exceeding its memory limit
carries exit reason `oom_killed` and a memory-limit message instead (see
[Out-of-memory kills](/scion/hosted/ha/kubernetes/#out-of-memory-kills)). Two paths can set `error`: `sciontool` reports it from the recovered
exit code (the authoritative path), and the Hub also derives `error` from a
non-zero container exit reported in the broker heartbeat — which covers cases
where the container died before `sciontool` could report. When a start fails, the Runtime Broker records the failure for that run in a host-owned state file next to the agent's home directory, and `scion list` and the Runtime Broker heartbeat show the agent as `error` even if a leftover container from that attempt (for example a Kubernetes pod left `Pending`) still reports `starting`.

A third path covers a container that vanishes without reporting an exit (for
example, removed outside Scion). In Hub-connected setups, a `running` agent (or
a `stopping` agent whose final stop report never arrived) that is missing from
its online Runtime Broker's complete runtime inventory for longer than
`missing_agent_grace` (default **3 minutes**, see
[server configuration](/scion/reference/server-config/)) moves to `error` with
exit reason `container_missing`, instead of staying `running` while messages
to it are buffered. It can then be restarted like any other `error`-phase agent.

On Kubernetes, a preempted or evicted agent pod leaves `running` without
waiting for that grace period; see
[Preempted and evicted status](/scion/hosted/ha/kubernetes/#preempted-and-evicted-status).

:::note
A normal `scion stop` sends `SIGTERM`, which harnesses like Claude Code handle
gracefully and exit cleanly (code 0). Only a *genuine* crash or a hard kill
produces the `error` phase — stopping an agent never leaves it in `error`.
:::

The `error` phase is **restartable**. By default, starting the agent again clears the error and runs a **fresh** session:

```bash
scion start <agent-name>
```

Because the crash discarded the previous run, this is a clean start rather than a session continuation.

#### In-Place Session Resume after Crash

If the agent's harness supports session resume, you can force Scion to attempt to continue the prior conversation rather than starting fresh. This is useful for recovering a crashed or interrupted session:

```bash
scion resume <agent-name> --force
```

Using `scion resume --force` on an agent in the `error` phase permits an in-place restart, passing the harness-specific resume/continue flag so that the interrupted conversation is preserved.

In Hub-connected setups, the same recovery is available in the web UI as a **Resume (best effort)** action on an `error`-phase agent, which asks you to confirm first. It calls the agent's `start` action with `forceResume` set (see the [API reference](/scion/reference/api/#agents-apiv1agents)). The resume is best effort: if the harness session cannot be continued, the agent may still start fresh or fail again.

Only one start of an agent runs at a time. If you start, resume, or restart an agent while another start of it is still in progress (for example, automatic recovery or a reincarnation), the Hub refuses with `409 start_in_progress` and tells you who holds the start and when it will finish or be released; run `scion stop` to cancel it. A restart whose stop step fails is aborted with a retryable `503` instead of starting a second instance.

#### Reincarnating an Agent

To move an existing agent onto the current template, image, and harness config without losing its identity, use [`scion reincarnate`](/scion/reference/cli/#scion-reincarnate). It keeps the agent ID and slug, clears the previous run's `exitCode` and `exitReason` when the migration begins, starts a new generation with a freshly resolved config, and hands it the task you supply with `--handoff-file`. Use `--dry-run` to preview the changes first. Reincarnation requires a Hub, and works for agents in clone-per-agent, shared-workspace, and Hub-managed workspaces. An agent in an empty-per-agent project can be reincarnated on its current Runtime Broker when that Runtime Broker is up to date and uses a local-disk runtime (Docker, Podman, or Apple `container`) or shared (`nfs`) workspace storage. Agents in worktree-per-agent projects are not yet supported: the Hub rejects the request, and the Runtime Broker also refuses such a reprovision on its own.

Reincarnating is also how an existing agent picks up changed harness dotfiles (including hook registrations kept in them, such as Claude's `.claude/settings.json`) and changed template skills, which are otherwise copied only when the agent is created. Switching harness-config with `--harness` resets the home instead of overlaying it (see below). Harnesses whose provisioner writes its hook configuration at start, such as Copilot, Antigravity and Grok Build, pick up provisioner changes on the next start. See [Changing a harness config affects new agents only](/scion/reference/harness-settings/#changing-a-harness-config-affects-new-agents-only).

In a hosted setup with several Runtime Brokers, `scion reincarnate <agent> --broker <name|id>` also moves the agent to another Runtime Broker, keeping its workspace, when both Runtime Brokers mount the same NFS export. An agent in an empty-per-agent project can move only when its workspace is on that export (Kubernetes with NFS workspace storage); `--dry-run --force` previews a forced move that would recreate a clone-per-agent or empty-per-agent workspace on the target instead. See [Moving an agent to another Runtime Broker](/scion/hosted/ha/multi-broker/#moving-an-agent-to-another-runtime-broker).

##### Switching harness-config

With the **server-layer, alpha, default-off** [`hub.reincarnate_harness_switch`
experiment](/scion/reference/experiments/) enabled, use `--harness <harness-config>`
to select a named harness-config from the project's catalog, falling back to the
global catalog. This rebuilds the agent home, even when you name its current
harness-config. It keeps the same agent identity and workspace.

Write a handoff that captures the agent's active work, then **run `--dry-run`
first** to review the plan and its **HOME RESET** warning:

```bash
scion reincarnate my-agent --harness codex --handoff-file handoff.md --dry-run
scion reincarnate my-agent --harness codex --handoff-file handoff.md
```

The real request prints the same warning and proceeds without a confirmation
prompt. **There is no rollback.**

- **Lost:** the harness session and transcripts, harness memory edits,
  skills/tools/caches the agent installed in `$HOME`, shell history and dotfile edits.
- **Kept:** the workspace and branch, Hub conversations and history, and the
  handoff, which is the only continuity into the new generation.

Files the old harness left in the workspace are not removed. A template can also
seed old-harness files back into the fresh home; the plan warns about recognizable
files under another harness's config directory, but that check is best effort.

**Settings on a switch.** The old explicit image, model and harness auth selection
are cleared and resolved for the target harness-config. To pin them, pass `--image`,
`--model` and `--harness-auth` in the same request. Model aliases use the target
harness-config's aliases. Inline `config_dir`, `command_args`, `task_flag` and
`user` settings are also dropped; there are no replacement CLI flags for these
fields on reincarnate. Define the needed values in the target harness-config
before switching. The selected harness-config and these drops persist into later
plain reincarnations. The thinking level and other harness-neutral settings remain.
An explicit create-time no-auth choice is kept, with a warning.

**Refusals before the agent is stopped.** A dry run gets the same checks as a real
request:

| Condition | Response and next step |
| :--- | :--- |
| Experiment is off | `400 experiment_disabled`; enable `hub.reincarnate_harness_switch` on the Hub. |
| Unknown harness-config | `404 harness_config_not_found`; check its name and scope. |
| Auth method unsupported by the target | `400`; choose a method supported by the target harness-config. |
| `--harness` combined with `--broker` | `400` from the Hub (also rejected by the CLI); switch and move in separate requests. |
| Runtime lacks home-reset support, or the Runtime Broker does not advertise it | `412 unsupported_capability`; use a supported runtime and an up-to-date Runtime Broker. |
| Target credentials missing | `422 harness_credentials_missing`; the response names the missing environment keys or auth files, never their values. Supply credentials as Hub secrets or choose `--harness-auth`. |

A target with `no_auth.behavior: drop-to-shell` never silently switches to no-auth
when credentials are missing. Pass `--harness-auth none` deliberately to request
no credentials. Credentials present **only in the Runtime Broker's environment**
are invisible to the Hub preflight and can cause a false `422`; use Hub secrets.
The Runtime Broker also gathers required values before resetting the home.

**Runtime support for `--harness`:**

| Runtime | Home reset supported |
| :--- | :--- |
| `docker` | Yes |
| `podman` | Yes |
| `container` (Apple Container) | Yes |
| `cloudrun` | Yes |
| `cloudrun-instances` | Yes |
| `kubernetes` | Yes, with NFS or local agent homes |
| `cloudrun-sandbox` | No; refused |
| `substrate` | No; refused |

The existing workspace requirements still apply: **Agents in worktree-per-agent projects are
refused**, and this table does not add support for ephemeral workspaces. Kubernetes
NFS-home resets require an agent image whose `sciontool version --features` reports
`home-reset-v1`. An older image refuses the start with `home_prepare_failed` and
asks for an image with that feature; update the image before retrying.

**Failure recovery.** Before the reset, a failure restores the previous generation's
configuration and leaves the home intact. After the home has been reset (or its
state is uncertain), the Hub keeps the new harness-config and marks the migration
failed with the agent in `error`. Fix the reported cause and follow the Hub's hint:

- If rebuilding the home failed, run `scion reincarnate <agent>` to reprovision the
  new harness-config.
- If rebuilding succeeded but starting failed, run `scion start <agent>`.
- To select the old harness-config again, run
  `scion reincarnate <agent> --harness <old-config>`. This performs another reset;
  it does not recover the previous home's contents.

## Harness Errors: the harness is still running

A harness can fail without exiting. When the model provider rejects a request
(for example a `400` for a denied permission, an expired credential, or an
unknown model), most harnesses print the error in their terminal and return to
their prompt. The process keeps running, so there is no crash to report.

When the harness reports such a turn-ending error on its completion hook, the
agent stays in the `running` phase with the **`harness_error`** activity, and
the reason is in the agent's message, for example
`model API: APIError (status 400)`. The reason is put on one line, redacted
for credentials and cut to 300 characters.

- `scion list` shows it in **LAST ACTIVITY** as
  `harness error: model API: APIError (status 400), 2m ago`, and
  `scion list --format json` carries the reason in `detail.message`.
- Subscribers with the `HARNESS_ERROR` trigger get a notification with the
  reason. `HARNESS_ERROR` is in the default triggers. The `ERROR` trigger still
  matches only the `error` phase, a dead harness.
- The agent is not marked `stalled` while it shows `harness_error`, so the
  reason is not replaced by a stall. It is still marked `offline` if its
  heartbeats stop.
- The next sign that the harness is making progress (a new prompt, a model
  call or a tool call) clears it. Fix the cause, then send the agent a message
  to make it retry: `scion message <agent-name> "continue"`.

The web UI shows it as a red **harness error** badge; hover over the badge to
see the reason.

Whether a harness reports the error on a hook depends on the harness. This
was checked by running each harness CLI against a model endpoint that
answers with a `400`:

| Harness | Hook that reports the failed turn | Shows `harness_error` from its hook |
|---|---|---|
| Claude Code (2.1.325) | `StopFailure`, with the provider's message | Yes, also for a `401` (`authentication_failed: ...`) |
| Antigravity (1.5.9) | `Stop` with `error` | Yes |
| grok-build (1.0.77) | `StopFailure` with `errorDetails` | Yes |
| OpenCode (1.19.5) | `session.idle` with the bridge's error | Yes; the reason has the error name and status, not the provider's message |
| Pi | `agent_end` with `error` from the bridge | Yes (bridge test payloads) |
| Codex (0.169.0) | None: a failed turn fires no `Stop` and no notify | No; the agent stays `thinking`, and never `completed` |
| Gemini CLI (0.68.0) | None: `AfterAgent` fires without an error | No; the agent shows `working` |
| Copilot CLI (1.0.112) | `errorOccurred`, but it looks the same for an error Copilot then retries successfully, and no `agentStop` follows a failed turn | No; it is recorded as a failed model call, and the agent shows `working` |
| Muse Code (1.4.20) | Only the failed model call (`PostLLMCall` with `error`); no turn end | No |
| Hermes | No hooks | No |

For a harness marked No, the agent can still be marked `stalled` after the
stall threshold; `scion look <agent-name>` shows the error in its terminal.

### Errors that no completion hook reports

Some harnesses show a model error in their terminal without reporting it on a
completion hook, and some never get as far as the first turn. `sciontool init`
watches for both cases inside the agent container:

- **Model API errors seen in telemetry.** When the harness exports telemetry
  (Claude Code, Gemini CLI and Codex with telemetry enabled), Scion reads each
  model API attempt from it. The agent gets `harness_error` with the reason
  `model API <status>: <message>` when all of the following are true:
  - the latest attempt of the agent's main model failed with `400`, `401`,
    `403` or `404`. A failed call to another model, such as a sub-agent or a
    small background model, does not count;
  - the agent is `thinking` or `working`;
  - after the failure, nothing else happened for a while: no successful
    attempt, no new turn, no status change. The wait is 30 seconds once the
    harness has visibly ended its turn (it wrote `working` after the
    failure). It is 2 minutes while the turn has not visibly ended, because
    the harness may still be streaming a retry or the main model's answer.

  A retried transient error, such as a `503` followed by a success, does not
  count. A status change after the failure, for example the harness writing
  `working` when its turn ends, starts the wait again but never cancels the
  report.
- **The initial task did not start.** If the agent was started with a task and
  its harness reports turn events, Scion checks 3 minutes after the harness is
  ready. The agent gets `harness_error` with the reason
  `harness did not start the initial task: <detail>` in either case:
  - no turn started at all (for example the harness is stuck at a login or
    authorization screen);
  - the turn so far ended on a failed model call that the harness reported on a
    model hook.

  A started turn shows as a prompt, a model call or a tool call, or as a model
  step or turn that ended without an error. OpenCode and Pi report nothing
  when the initial prompt starts, so their first finished model step, first
  tool call or finished turn is what counts. If one of these arrives after the
  report, the report is withdrawn and the agent returns to `working`.

  The detail is that model error when there is one. Otherwise it is the last
  lines of the harness terminal, redacted and cut like every reason. Hermes
  reports no turn events and is not checked.

An error the harness reports on its completion hook takes precedence. Neither
check replaces it or reports it a second time.

Gaps that remain: without telemetry, a Codex turn that fails after the prompt
is still marked `stalled`. The same applies to a GitHub Copilot turn that
fails after the prompt, and to a model error after the first turn on a harness
with no completion-hook error (muse-code).

Some healthy first turns are briefly reported as not started:

- On OpenCode and Pi, a first model step that runs for more than 3 minutes
  before its first tool call, or before it finishes.
- On GitHub Copilot, a transient model error that Copilot retries
  successfully early in the first turn, when no tool call follows before the
  3-minute check. Copilot reports failed model calls but not successful ones.

Scion retracts such a report and returns the agent to `working` as soon as
the harness reports a finished step or turn, or a tool call. It never
retracts a harness error that the harness itself reported.

## Auto-Suspend of Stalled Agents

To reclaim resources from agents that are no longer making progress, the Hub can
automatically suspend agents that have **stalled**. Auto-suspend is **off by
default**; turn it on with `server.hub.auto_suspend_stalled` (or **Auto-suspend
stalled agents** in **Admin > Server Config**).

An agent is marked `stalled` by the platform when it is `running`, its heartbeat
arrived within the last 2 minutes (the process is alive), but no activity events
have arrived within the stall threshold (`server.hub.stalled_threshold`, default
**5 minutes**; a value under 2 minutes falls back to the default). The Hub
checks every 5 minutes. With auto-suspend on, the Hub suspends an agent in the
same check that marks it stalled, unless the agent's harness does not support
session resume. Agents already `stalled` when you turn auto-suspend on are not
suspended unless they become active and stall again.

A long build, a test run or a background job emits no activity events, so the
Hub also looks for **live work**. On each 30-second heartbeat, `sciontool init`
checks the container for two things:

- **A tool call in flight.** The activity is `executing` and a process the
  harness started after the tool call began is still running, even one that
  uses no CPU, such as `gh run watch`. MCP and language servers started earlier
  don't count.
- **CPU work.** A process used at least 1% of one core since the previous
  heartbeat.

The tmux server, the pane shells, the harness's own process, sciontool and its
services don't count. An agent whose container reported live work within the
stall threshold is not marked `stalled`, and a `stalled` agent whose container
reports live work returns to its previous activity. An idle agent, including one
with idle MCP or language servers, still stalls at the threshold.

A tool call holds off the stall for at most 60 minutes. It stops earlier when
its processes make no progress for 10 minutes: the same processes, with no new
CPU time and no reads or writes. A command waiting on a prompt or a dead
connection is flagged this way. CPU work is not capped. Either way, auto-suspend
skips an agent whose container reported live work in the last 90 seconds, so a
running tool call is never suspended.

Auto-suspend uses the same machinery as a manual `scion suspend`, setting the
agent's phase to `suspended` with `suspendReason: "stalled"`, preserving its
harness session (and any `completed`, `idle`, or `harness_error` activity), and
revoking its Hub credentials and active delegation grants. The agent resumes,
continuing where it left off, when someone starts it or sends it a message with
wake (`scion message --wake` or the web UI's wake prompt) — or automatically on
any incoming message, mention, notification, or scheduled message when the
[`lifecycle.idle_suspend`](/scion/reference/experiments/#server-experiments-with-notes)
experiment is enabled.

:::tip
If your agent is *intentionally* quiet, have it say why. Waiting on a specific
event (a child agent, a reply, a scheduled message, or its own background job)
is `sciontool status blocked "<reason>"`; having nothing to do until new work
arrives is `sciontool status idle "<reason>"`. Both are excluded from stalled
detection and therefore from stalled auto-suspend. See
[Ending a turn: blocked, idle, task_completed](/scion/concepts/#ending-a-turn-blocked-idle-task_completed).
:::

:::caution
Auto-suspend is a Hub-driven behavior and depends on the Hub's scheduler being
operational. For `auto_suspend_stalled` and `stalled_threshold`, see
[`server.hub`](/scion/reference/server-config/#hub-settings-serverhub).
:::

## Auto-Suspend of Idle Agents

An agent that reports it has nothing to do (`idle`) or that finished its task
(`completed`) still holds its container. The Hub can suspend such agents after a
grace period and wake them again when new work arrives for them.

Idle auto-suspend needs **both**:

- the [`lifecycle.idle_suspend`](/scion/reference/experiments/#server-experiments-with-notes)
  experiment (alpha, off by default), and
- `server.hub.auto_suspend_idle: true` (off by default; **Auto-suspend idle
  agents** in **Admin > Server Config**).

Every 5 minutes, the Hub suspends each `running` agent that meets all of these:

- Its activity is `idle`, `completed` or `harness_error`, and has been for
  longer than `server.hub.idle_suspend_after` (the grace period: default
  **15 minutes**, minimum **5 minutes**; a shorter value is ignored and the
  default applies). The grace is measured from when the activity last changed,
  not from the last hook event.
- Its heartbeat is fresh (an offline agent is left alone).
- Its container reported no live work (a tool call in flight or CPU work) in the
  last 90 seconds.
- Its harness supports session resume, judged by the harness type behind its
  harness config, so a custom-named config of a resumable harness qualifies.
- No other lifecycle operation is in progress for it.

An agent is normally suspended within about one sweep interval (5 minutes)
after its grace period ends; with a large backlog of candidates it may take
longer. Agents that are `blocked` or `waiting_for_input`, or that are working,
are never suspended by this policy.

The grace is measured from a timestamp the Hub records when the activity
changes. An agent that has been `idle` or `completed` since before the Hub
recorded it (that is, since before the `idle` status shipped) has no such
timestamp, and is not eligible until its activity next changes; there is no
backfill.

The suspend works like `scion suspend`: the container stops, the harness session
and the `idle`, `completed` or `harness_error` activity are kept, and the
suspend reason is `idle`. `scion list` shows such an agent as, for example,
`suspended · completed (idle)`.

### Interrupted suspends

The suspend is committed in two steps: the Hub records its intent to stop the
agent (with a short lease of a few minutes), stops the container, then records
`suspended` with reason `idle`. Failures between the steps are handled so an
agent is never left stopped without a suspend reason:

- **Retry.** The final write is retried with backoff. While it has not landed,
  the lease is kept.
- **Recovery.** If the write still fails, or the Hub restarts after the stop, the
  idle sweep records the suspension once the lease has expired and the agent
  reports stopping, stopped or error. This needs only the experiment, so turning
  `auto_suspend_idle` off does not strand an in-flight suspend. A user stop,
  stop-all, start or delete in the meantime always wins; the sweep never converts
  such an agent.
- **Unknown stop outcome.** A timeout or other error that leaves it unclear
  whether the container stopped does not revert the stop intent. If the
  container is still running, the Hub's stop-intent reconciliation stops it and
  the sweep then records the suspension. Only a provable refusal (for example
  the request never reached the broker) reverts the intent right away.
- **Fail fast, then abandon.** While a suspend is in progress, messages that need
  a running agent wait for it to settle (auto-wake paths) or are refused with a
  "suspending" error. If the lease has expired and the agent is still running,
  the stop never landed: deliveries are refused at once rather than waiting.
  After roughly 11 to 16 minutes (the sweep runs every 5 minutes) the sweep abandons the suspend: it reverts the intent,
  records a message on the agent, and logs a warning, so the agent is reachable
  again.
- **Backoff.** An abandoned agent is not idle-suspended again for about 15
  minutes, so a persistently failing broker is retried roughly every 30 minutes
  rather than every sweep.

### Auto-wake on new work

With the experiment on, an agent the Hub suspended (`idle` or `stalled`) is
resumed automatically when a direct message, web chat message, `@`-mention,
notification or scheduled message arrives for it, with no `--wake` and no
lifecycle permission needed on the sender (permission to message the agent is
enough). This is what makes it safe for an agent to go idle while it waits for
a child agent or a scheduled callback hours away. An agent suspended by a user
(reason `user`), one suspended before reasons were recorded, and a held agent
are never woken this way.

To keep a Hub-suspended agent down, suspend it again (`scion suspend <agent>`,
`POST /api/v1/agents/:id/suspend`, or **Keep suspended** on the agent page): with the experiment on, this changes its
reason from `idle` or `stalled` to `user` in place, without touching the
runtime, so new work no longer wakes it.

### What is gated

| Behavior | Gate |
|---|---|
| The `idle` activity, `sciontool status idle`, the `IDLE` notification trigger, stall and offline exclusion of `idle` | None (always on) |
| Retained activities and `suspendReason` on suspended agents, and their display | None (always on) |
| Stalled auto-suspend | `server.hub.auto_suspend_stalled` |
| Idle auto-suspend sweep | `lifecycle.idle_suspend` **and** `server.hub.auto_suspend_idle` |
| Auto-wake of `idle`- or `stalled`-suspended agents, and converting them to `user` with suspend | `lifecycle.idle_suspend` |

For the settings, see [`server.hub`](/scion/reference/server-config/#hub-settings-serverhub).

---

## Troubleshooting & Diagnostics

Most stuck, stalled, or failing agents are recoverable without deleting and recreating them. Recreating an agent should be a last resort, as it destroys unpushed working tree files and terminal history. 

Always start by running `scion look <agent-name>` to inspect the active screen state and `scion logs <agent-name>` to review internal telemetry before performing recovery.

### 1. Common Stuck States & Recovery (Triage Table)

| Symptom | Probable Cause | Correct Recovery Action |
|---|---|---|
| **`harness error`** in `scion list` | The harness reported that the model provider rejected its turn (the reason is in the message, for example `model API: APIError (status 400)`). The harness is still running. | Fix the cause (credentials, model name, permissions), then send `scion message <agent-name> "continue"` to retry. Do *not* recreate the agent. |
| **Transient API errors** in logs | LLM provider rate-limits, quotas, or minor timeouts. | Send a wake-up command: `scion message <agent-name> "continue"`. Do *not* recreate the agent. |
| **`LIMITS_EXCEEDED`** state | The agent reached its configured turn, model call, or duration ceiling. | Send a continue command: `scion message <agent-name> "continue"`. This clears the ceiling for another cycle. |
| **Cryptographic primitive error** | The Hub regenerated its signing keys (e.g., on restart) or there is a key mismatch in a multi-replica deployment. | Send `scion message <agent-name> "continue"`. Message delivery does not rely on the agent's own token. If looping, contact the operator: the Hub's `SharedSigningSecret` (`SCION_SERVER_SESSION_SECRET`) must be pinned in the deploy config. |
| **Deadlocked token refresh** (401 loops in logs) | The agent's token expired and its automatic refresh loop deadlocked. | Try sending `scion message <agent-name> "continue"`. If this has no effect, recreate the agent. |
| **Phase `created` / lastSeen zero** for 5+ minutes (not shown as `created (not started)`) | The agent creation timed out or failed to schedule. An agent made with `scion create` is shown as `created (not started)` instead; it is waiting for `scion start`, not stuck. | The system is likely under heavy resource pressure. Wait a few minutes. If still stuck, delete and recreate. **To prevent:** reduce concurrent agent starts. |
| **Start fails with `no_runtime_broker` (422)** | Temporary connection issue after a system restart or project reconnect. | Wait 30–60 seconds and try starting again. If persistent, verify Runtime Broker status with `scion runtime-broker status`. |
| **Split-Brain Configuration** (git project ignore settings) | Config files are loading incorrectly due to overlapping global vs. project settings. | Run `scion config dir` to see the effective config path. Ensure the merge chain matches: `defaults → global → in-repo → external → environment`. |
| **Interactive prompt blocking** | The agent's harness is stuck waiting for an unhandled prompt (e.g. yes/no query). | Send the dismissive keystroke to the terminal: `scion keys <agent-name> "Enter"` (or `"y"`, etc.). One key per call — there is no sequence syntax. |

---

## Deletion Authority & Hierarchical Teardown

`scion delete <agent-name> --non-interactive` immediately reclaims container resources. Since an agent's true deliverable is its **artifact** (pushed commits, opened PRs, files written to a shared volume), **deleting a completed agent is the default, recommended clean-up path.**

Teardown on the Runtime Broker can outlast the request. If it does, `scion delete` waits up to three minutes for the Hub to confirm the delete, and only then removes the local worktree. If the delete fails, the worktree is kept. Agent deletion and failed-start cleanup pass every removal through one check: with symlinks resolved, a path may be removed only if it lies inside one of the agent's own directories, and a path that is or contains a project workspace is refused. Only the agent's own git worktree entry is removed, never with a repository-wide prune, and a worktree path that resolves elsewhere through a symlink is left alone. If the Runtime Broker teardown is still unresolved, the agent also can't be started, restarted or woken (`409 delete_in_progress`) until you run `scion delete` again. If the Runtime Broker holds a different run of the agent than the Hub recorded, it deletes nothing. If that run is one of the agent's earlier runs, as a failed start can leave behind, the Hub then deletes it and removes the agent. Otherwise the Hub refuses the delete with `409 conflict` and keeps the agent, and `--force` deletes the run the Runtime Broker holds and then removes the agent. In both cases, if that delete fails, the Hub keeps the agent. See [`scion delete`](/scion/reference/cli/#scion-delete-or-rm) and [`DELETE /agents/:id`](/scion/reference/api/).

However, to prevent premature deletion of agents with active or pending tasks, strict teardown guidelines must be followed.

### 1. Who May Authorize Deletion

| Agent Role | Authorized to Delete | Allowed Timing |
|---|---|---|
| **Task-bounded agent** | The agent's creator or supervisor. | Immediately once the output has been accepted and verified. |
| **Agent with an open human question** | The agent's creator or supervisor. | **Only after all questions to humans are answered** and the conversation is explicitly concluded. |
| **Lead** — the first agent on a project or workstream, an agent coordinating that project or workstream, or an agent explicitly designated by a human as its lead | **Only on explicit human instruction naming the workstream** | It carries continuity from the initial request through closure, including non-code and non-repository projects |
| **Delegated supervisor** with a bounded assignment, not a lead | Its creator or authorized supervisor, subject to the assigned workflow; a human where that workflow requires it | After its deliverable is accepted, verified and durable, its subtree is torn down, and its human questions are resolved |

### 2. Hierarchical (Bottom-Up) Teardown

When cleaning up a complex multi-agent hierarchy, teardown must proceed **bottom-up** to avoid leaving orphaned containers running:

1. The parent orchestrator learns its goal is reached and notifies the child sub-agents.
2. The child sub-agents finish their current cycle, push their work, and signal completion.
3. The parent orchestrator deletes the child worker agents.
4. The parent orchestrator confirms its entire subtree is deleted and exits.
5. The human operator or higher-level parent deletes the parent orchestrator.

### 3. Safety Rules for Cleanup

1. **Completion is not Deletion**: A completed task means the work is finished, not that the agent should be deleted. The user or coordinator may want to assign a follow-up task.
2. **Unanswered Questions block Deletion**: If an agent has raised an open, unanswered question to a user, it **cannot** be deleted.
3. **Commit is not Push**: Never delete an agent with unpushed work. Always verify that all local commits have been pushed to the remote branch, and any local deliverables have been copied to shared directories or volumes.

---

## Runtime caveats

Session continuation works only when the agent's **home directory** — where the
harness stores its conversation state — survives the container being reclaimed.
Treat suspend/resume and auto-suspend *session continuation* as a Docker-proven
capability, with this caveat for other runtimes:

- **Docker** — the proven path. The agent home is a host bind-mount that
  survives the container being reclaimed, so suspend/resume and auto-suspend
  continue the harness session with no additional configuration. The same holds
  for any setup with a persistent or NFS-backed home.
- **Kubernetes / Cloud Run** — these runtimes can have an ephemeral home. Without
  durable home persistence, resume restarts the container but the harness session
  **may not continue**. Durable home persistence (for example, object storage
  such as GCS) is future work, and these runtimes are gated on NFS-style
  persistence regardless — so do not assume full suspend/resume parity here yet.

## See also

- [Core Concepts: Agent State Model](/scion/concepts/#agent-state-model)
- [CLI Reference: Agent Lifecycle](/scion/reference/cli/#agent-lifecycle)
- [Web Dashboard](/scion/workstation/dashboard/)
