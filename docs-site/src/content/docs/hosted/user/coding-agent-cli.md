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
covers only what it needs, rather than your interactive login. Create it from your own
signed-in shell:

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
| `scion stop`, `scion resume`, `scion restart` | `project:read`, `agent:lifecycle` |
| `scion delete` | `project:read`, `agent:delete` |

Run `scion hub token scopes --project my-project` to see which scopes you can select.

If the token lacks `project:read`, the Hub answers the project lookup with `404 Not Found`. The
CLI reports the likely missing `project:read` scope and stops. A user access token cannot
register a new project, so the CLI does not try. Link the project once with your interactive
login (`scion hub link`) before you hand the token to the agent.

## 2. Run the CLI with `SCION_HUB_TOKEN`

Pass the token to the coding agent's environment as `SCION_HUB_TOKEN`:

```bash
export SCION_HUB_TOKEN="scion_pat_..."
scion list --non-interactive --format json
```

:::caution[A stored login takes precedence]
A stored interactive login (from `scion hub auth login`) takes precedence over
`SCION_HUB_TOKEN`. To run the CLI under a scoped token, use an environment with no stored login:
a dedicated OS user, an isolated `HOME`, or log out first (`scion hub auth logout`).
:::

Run `scion hub status` in the agent's environment to check which credential the CLI is
using.

## 3. Use non-interactive flags

Have the agent pass these flags:

- `--non-interactive`: never prompt. It implies `--yes`, and a prompt with no single
  safe answer, such as several Hub projects with the same name, is an error rather than a guess.
- `--format json`: print a result the agent can parse. Prompts, auto-confirm notes and
  progress messages go to stderr, so stdout holds only the JSON.
- `--yes`: on its own, accepts the default of each confirmation. `--non-interactive` already
  implies it. Ask for `--yes` explicitly only for the actions you want the agent to confirm
  unattended. Without it, a confirmation outside a terminal answers No.

## What the CLI does without a terminal

The CLI decides how to prompt from whether stdin is a terminal, not from the CLI mode:

- **No stdin reads.** When stdin is not a terminal, the CLI does not read answers from it,
  so an idle open stdin (as many tool runners provide) cannot hang a command.
- **Safe defaults.** A yes/no confirmation without `--yes` answers No and says on stderr that
  `--yes` confirms. A choice with no safe default fails with an error that names the flag to use.
  Destructive and registration actions never proceed on a default.
- **Prompts on stderr.** Prompt text and auto-confirm notes such as `auto-confirmed Yes` go
  to stderr, never stdout.
- **No colour codes.** ANSI colour is used only when the output is a terminal, and never when
  the `NO_COLOR` environment variable is set to a non-empty value.

## Assistant mode

The CLI also has an `assistant` mode (`SCION_CLI_MODE=assistant`), which limits the command
set. This page does not rely on it: the token's scopes and the flags above are enough.

## What's next

Agent-bound tokens are planned. They will tie a token to the coding agent that uses it rather
than to your user, and they will replace the user access token setup on this page.
