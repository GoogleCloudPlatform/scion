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
crash detail is carried in the agent's message (e.g. `Agent crashed with exit
code 137`). Two paths can set `error`: `sciontool` reports it from the recovered
exit code (the authoritative path), and the Hub also derives `error` from a
non-zero container exit reported in the broker heartbeat — which covers cases
where the container died before `sciontool` could report.

:::note
A normal `scion stop` sends `SIGTERM`, which harnesses like Claude Code handle
gracefully and exit cleanly (code 0). Only a *genuine* crash or a hard kill
produces the `error` phase — stopping an agent never leaves it in `error`.
:::

The `error` phase is **restartable**. Starting the agent again clears the error
and runs a **fresh** session:

```bash
scion start <agent-name>
```

Because the crash discarded the previous run, this is a clean start rather than
a session continuation.

## Auto-Suspend of Stalled Agents

To reclaim resources from agents that are no longer making progress, the Hub can
automatically suspend agents that have **stalled**.

An agent is marked `stalled` by the platform when its heartbeat is still being
received (the process is alive) but no activity events have arrived within the
stall threshold (default: **5 minutes**). After it remains stalled for an
additional grace period (a further **5 minutes**, so roughly **10 minutes** of
inactivity in total), the Hub auto-suspends it — provided that:

- the agent's harness supports session resume, and
- the container is still alive.

Auto-suspend uses the same machinery as a manual `scion suspend`, so the agent's
phase becomes `suspended` and its harness session is preserved. The agent is
**resumed automatically on the next message** sent to it, continuing right where
it left off.

:::tip
If your agent is *intentionally* idle — for example, waiting on a child agent or
a scheduled event — have it declare itself `blocked` (via
`sciontool status blocked "<reason>"`). Blocked agents are excluded from stalled
detection and therefore from auto-suspend.
:::

:::caution
The stall threshold and grace period are currently hardwired and not
user-configurable. Auto-suspend is a Hub-driven behavior and depends on the
Hub's scheduler being operational.
:::

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
- [Web Dashboard](/scion/hub-user/dashboard/)
