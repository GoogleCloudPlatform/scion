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
token (see step 2), the scopes are what limits the agent, not the CLI flags in
[step 3](#3-choose-how-confirmations-are-answered): for example, a token without
`agent:delete` cannot delete agents, whatever flags the agent passes.

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
for confirmation at all. Neither flag makes a destructive action safe: what the agent can do is
limited by the token's scopes, and only when the CLI runs in an environment with no stored login.
:::

Pick one of these setups:

- **Recommended: no `--yes`.** Leave out `--yes` and `--non-interactive`. Without a terminal,
  every confirmation answers No, the command stops, and stderr names `--yes`. The agent reports
  this and you decide whether to run the command yourself or let the agent re-run it with
  `--yes` for that one action. This does not cover commands that do not ask, such as
  `scion delete`; leave their scopes out of the token.
- **`--non-interactive`, with a narrow token.** Pass `--non-interactive` only when the token
  lacks the scopes for actions you would not confirm yourself, for example no `agent:delete`,
  and the CLI runs with no stored login. Every confirmation the token allows is then answered
  Yes. A prompt with no single answer, such as several Hub projects with the same name, is
  still an error rather than a guess.

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

## Assistant mode

The CLI also has an `assistant` mode (`SCION_CLI_MODE=assistant`), which limits the command
set. This page does not rely on it: the token's scopes and the flags above are enough, provided
the CLI runs in an environment with no stored login (see the caution in step 2).

## What's next

Agent-bound tokens are planned. They will tie a token to the coding agent that uses it rather
than to your user, and they will replace the user access token setup on this page.
