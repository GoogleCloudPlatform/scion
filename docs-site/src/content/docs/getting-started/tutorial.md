---
title: Tutorial
---

Once you have installed and configured Scion, you can start using it to run LLM-based code agents. This brief tutorial will guide you through the basics of starting and managing an agent.

## 1. Start an Agent

To start an agent, navigate to your project directory (where you ran `scion init`) and use the `scion start` command. You provide a name for the agent and the objective you want it to accomplish.

```bash
scion start my-first-agent "Write a python script that prints Hello World"
```

The agent will be launched in the background. If you are in a git repo, it will automatically create a new git worktree and branch for its workspace, ensuring your main working directory remains clean.

## 2. List Agents

You can see all agents currently managed by Scion in your project using the `scion list` command:

```bash
scion list
```

This will display a table showing the agent's name, its current status (e.g., `STARTING`, `THINKING`, `EXECUTING`, `COMPLETED`), and other details like its runtime and the LLM harness it is using.

## 3. Check Agent Progress

If you want to see what an agent is doing, you can view its logs:

```bash
scion logs my-first-agent
```

If the agent needs your input or confirmation (its status will be `WAITING_FOR_INPUT`), you can attach to its terminal session:

```bash
scion attach my-first-agent
```

When you are done interacting, you can detach from the session to leave the agent running in the background. This is done with tmux control keys, Cntrl-b, then d.

See more
<!-- TODO link to tmux doc -->


You can start and attach to an agent in one go with 

```bash
scion start --attach my-other-agent
```

## 4. Clean Up

Once the agent has completed its task, you can review the changes it made in its dedicated branch. When you no longer need the agent, you can delete it:

```bash
scion delete my-first-agent
```

This will stop the agent container and clean up its resources. By default, its git branch is removed, so be sure to merge any changes you want to keep before deleting the agent!

This gives you the very basics of the command, you can use `scion --help` and `scion <cmd> --help` to learn more about each of the commands in the tool.

## Next steps

Want to talk to your agents from your phone? Follow [Setting Up Telegram](/scion/getting-started/telegram/) to message agents and receive their replies over Telegram.