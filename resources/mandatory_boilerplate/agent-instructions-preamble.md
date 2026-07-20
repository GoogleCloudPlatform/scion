# Agent Status Signals

You must explicitly signal your state to the orchestration system using `sciontool status`. These signals prevent false stall detection, enable notification routing, and keep users and other agents informed.

## Waiting for Input

Before asking a user or coordinator a question, signal that you are waiting:

```bash
sciontool status ask_user "<question>"
```

Then proceed to ask the question.

Do not use any built in "ask_user" type tool as this can present interactive tooling that can interfere with orchestration.

## Blocked (Intentionally Waiting)

When you are intentionally waiting for something — such as a child agent to complete, a scheduled event, or a user reply — signal that you are blocked:

```bash
sciontool status blocked "<reason>"
```

Example: `sciontool status blocked "Waiting for agent deploy-frontend to complete"`

This prevents the system from falsely marking you as stalled. The status clears automatically when you resume work (e.g. when you receive a message or start a new task).

Do NOT use the sleep bash command, or enter into a polling loop to wait check on another agent. You may use your own strategies for waiting for things like long running bash commands, programs you run, build jobs etc that are running inside your system, but for interaction with other agents or users you should signal a "blocked" status


## Completed Task

Once you have completed your current task, summarize and report back as you normally would, then signal completion:

```bash
sciontool status task_completed "<task title>"
```

Do not follow this with "What would you like to do now?" or similar — just stop.

**Important:** An agent  must call `sciontool status blocked` or `sciontool status task_completed "<task title>"` in order to avoid being marked as stalled due to inactivity.
