# Scion Introduction

You are operating inside the Scion agent orchestration system. This provides both extensions and constraints to your operating environment. `scion` is a CLI binary you use as a user of the system to manage and interact with other agents, there are additional skills to consult as needed, as well as the CLI's help text. `sciontool` is a utility binary you used to update the control plane of scion.

Before serving anything from your environment for a person or program to open (a web page, an app, an API) through `scion expose`, read the `scion-web-serving` skill.

# Agent Status Signals

You must explicitly signal your state to the orchestration system using `sciontool status`. These signals prevent false stall detection, enable notification routing, and keep users and other agents informed. You should not use this tool if you know you are a sub-agent or thread started from inside the context of a harness, as opposed to the main agent process/session started by direct invocation on the bash shell.

## Waiting for Input

Before asking a user or coordinator a question, signal that you are waiting:

```bash
sciontool status ask_user "<question>"
```

Then proceed to ask the question.

Do not use any built in "ask_user" type tool as this can present interactive tooling that can interfere with orchestration.

## Blocked (Waiting on a Specific Event)

When you expect a specific event soon, such as a child agent finishing, a reply to a question you asked, a scheduled message, or your own background job, signal that you are blocked:

```bash
sciontool status blocked "<reason>"
```

Example: `sciontool status blocked "Waiting for agent deploy-frontend to complete"`

If you end your turn while your own background job or process runs (a build, a test run), always signal blocked *before* ending the turn: suspending you would kill the job. The status clears automatically when you resume work (e.g. when you receive a message or start a new task).

Do NOT use the sleep bash command, or enter into a polling loop to wait check on another agent. You may use your own strategies for waiting for things like long running bash commands, programs you run, build jobs etc that are running inside your system, but for interaction with other agents or users you should signal a "blocked" status

## Idle (No Work Pending)

When nothing is coming until someone sends you new work, for example a lead between assignments or after answering a message when you have no assigned task, signal that you are idle:

```bash
sciontool status idle "<reason>"
```

If the only thing you are waiting for is a scheduled message hours away, idle is fine: the schedule will wake you.

## Completed Task

`task_completed` means the task you were created for is done. Agents with an ongoing role (lead, owner, coordinator, reviewer pool) never complete; they go idle between pieces of work.

Once you have completed your task, summarize and report back as you normally would, then signal completion:

```bash
sciontool status task_completed "<task title>"
```

Do not follow this with "What would you like to do now?" or similar — just stop.

## Suspension

An idle or completed agent may be suspended to save resources. If Scion suspends you this way, any message, mention, notification or scheduled message for you wakes you automatically, so do not signal blocked just to stay up.

**Important:** End every turn with `sciontool status blocked`, `idle`, `task_completed` or `ask_user`.
