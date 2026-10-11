---
title: Using the scion CLI from a coding agent
description: Run the scion CLI from a coding agent on your own machine with a scoped token, machine-readable output and no interactive prompts.
---

This page is for a coding agent that runs on your own machine (for example Claude Code or
Gemini CLI in your terminal) and drives `scion` against a Hub on your behalf: listing agents,
starting them, sending messages, reading logs. The agent calls the CLI through its shell tool,
so there is no terminal for prompts and its output has to be machine-readable.

This is different from agents that Scion itself starts. Those authenticate with their own
agent token and need none of the setup below.

## 1. Create a scoped token

Give the coding agent a [user access token](/scion/hosted/user/personal-access-tokens/) that
covers only what it needs, rather than your interactive login. The token only takes effect
where no interactive login is stored; see [step 2](#2-run-the-cli-with-scion_hub_token).
Create it from your own signed-in shell:

```bash
scion hub token create \
  --project my-project \
  --name "coding-agent" \
  --scopes project:read,agent:list,agent:read,agent:create,agent:message \
  --expires 30d
```

Most CLI commands that run in a project look the project up on the Hub first, which needs
`project:read`, so include it in every token. Scopes common CLI flows need:

| Flow | Scopes |
|------|--------|
| Any command run in a project | `project:read` |
| `scion list` | `project:read`, `agent:list` |
| `scion look`, `scion logs` | `project:read`, `agent:read` |
| `scion start` / `scion create` | `project:read`, `agent:create`, `agent:read` |
| `scion message` | `project:read`, `agent:message` |
| `scion attach` | `project:read`, `agent:attach` |
| `scion stop`, `scion suspend`, `scion resume`, `scion restore` | `project:read`, `agent:lifecycle` |
| `scion delete` | `project:read`, `agent:delete` |

Run `scion hub token scopes --project my-project` to see which scopes you can select.

Leave out scopes for actions you do not want the agent to take. When the CLI runs under the
token (see step 2), token scopes limit what the agent can do on the Hub, and the CLI flags in
[step 3](#3-choose-how-confirmations-are-answered) do not change that: for example, a token
without `agent:delete` cannot delete agents through the Hub.

:::caution[Token scopes do not limit local actions]
Local actions, such as `scion clean` and any command run with `--no-hub` (for example
`scion delete --no-hub`), act on the local machine with the file permissions of the user the
CLI runs as, and token scopes don't limit them. To keep the agent away from local projects and
agents, run it as a dedicated OS user or with an isolated `HOME` (see step 2).
:::

If the token lacks `project:read`, the Hub answers the project lookup with `404 Not Found`. The
CLI reports the likely missing `project:read` scope and stops. A user access token cannot
register a new project, so the CLI does not try. Link the project once with your interactive
login (`scion hub link`) before you hand the token to the agent.

## 2. Run the CLI with `SCION_HUB_TOKEN`

Pass the token to the coding agent's environment as `SCION_HUB_TOKEN`:

```bash
export SCION_HUB_TOKEN="scion_pat_..."
scion list --format json
```

:::caution[A stored login takes precedence]
A stored interactive login (from `scion hub auth login`) takes precedence over
`SCION_HUB_TOKEN`. To run the CLI under a scoped token, use an environment with no stored login:
a dedicated OS user, an isolated `HOME`, or log out first (`scion hub auth logout`).
:::

Run `scion hub status` in the agent's environment to check which credential the CLI is
using.

## 3. Choose how confirmations are answered

Have the agent pass `--format json` so it gets a result it can parse. Prompts, auto-confirm
notes and progress messages go to stderr, so stdout holds only the JSON.

:::danger[`--yes` and `--non-interactive` confirm everything]
`--yes` answers **Yes** to every confirmation, including destructive ones whose interactive
default is No, such as deleting a hub project or Runtime Broker, `scion clean`, deregistering a
broker, withdrawing it from a project or unlinking a project. `--non-interactive` implies
`--yes`, so it does the same. Some destructive commands, such as `scion delete`, do not ask
for confirmation at all. Neither flag makes a destructive action safe. Token scopes limit what
the agent can do on the Hub, and only when the CLI runs in an environment with no stored login.
They do not limit local actions: `scion clean` removes the local `.scion` directory and
`scion delete --no-hub` deletes local agents, limited only by what the CLI's OS user can reach.
:::

Pick one of these setups:

- **Recommended: no `--yes`.** Leave out `--yes` and `--non-interactive`. Without a terminal,
  every confirmation answers No, the command stops, and stderr names `--yes`. The agent reports
  this and you decide whether to run the command yourself or let the agent re-run it with
  `--yes` for that one action. This does not cover commands that do not ask, such as
  `scion delete`. Leave their scopes out of the token to stop them on the Hub; with `--no-hub`
  they act locally, where token scopes don't apply.
- **`--non-interactive`, with a narrow token and an isolated environment.** Pass
  `--non-interactive` only when the token lacks the scopes for Hub actions you would not confirm
  yourself, for example no `agent:delete`, and the CLI runs with no stored login. Every Hub
  confirmation the token allows is then answered Yes. Local confirmations are answered Yes too:
  local actions (`scion clean`, and commands with `--no-hub`) act on the local machine with the
  user's file permissions, and token scopes don't limit them. So use this setup only where the
  agent's OS user or `HOME` holds no local project or agents it must not remove, such as the
  dedicated OS user or isolated `HOME` from step 2. A prompt with no single answer, such as
  several Hub projects with the same name, is still an error rather than a guess.

## What the CLI does without a terminal

The CLI decides how to prompt from whether stdin is a terminal, not from the CLI mode:

- **No stdin reads.** When stdin is not a terminal, the CLI does not read answers from it,
  so an idle open stdin (as many tool runners provide) cannot hang a command.
- **Safe defaults.** Without `--yes` or `--non-interactive`, a yes/no confirmation answers No
  and says on stderr that `--yes` confirms, and a choice with no safe default fails with an
  error that names the flag to use. Without those flags, destructive and registration actions
  never proceed on a default.
- **Prompts on stderr.** Prompt text and auto-confirm notes such as `auto-confirmed Yes` go
  to stderr, never stdout.
- **No colour codes.** ANSI colour is used only when the output is a terminal, and never when
  the `NO_COLOR` environment variable is set to a non-empty value.

## Limiting what the coding agent can do

For Hub actions, the token's scopes and the flags above limit what the coding agent can do,
provided the CLI runs in an environment with no stored login (see the caution in step 2).
Local actions are limited by the OS user and `HOME` the CLI runs with, not by the token.

The CLI no longer has a separate `assistant` mode. If `SCION_CLI_MODE` or the `cli.mode`
setting is still set to `assistant`, the CLI prints one warning to stderr, ignores it, and
runs with the full command set.

## What's next

Agent-bound tokens are planned. They will tie a token to the coding agent that uses it rather
than to your user, and they will replace the user access token setup on this page.
