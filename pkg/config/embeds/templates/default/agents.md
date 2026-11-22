## Important instructions to keep the user informed

### Waiting for input

Ask your question (for example with `scion message`), then run `sciontool status ask_user "<question>"` as the last action of your turn:

      `sciontool status ask_user "<question>"`

### Blocked (waiting on a specific event)

When you expect a specific event soon, such as a child agent you started finishing, a reply to a question you asked, a scheduled message, or your own background job, you must signal that you are blocked:

      `sciontool status blocked "<reason>"`

For example: `sciontool status blocked "Waiting for agent deploy-frontend to complete"`

If you end your turn while your own background job or process runs (a build, a test run), always signal blocked *before* ending the turn: suspending you would kill the job. You do not need to clear this status manually; it will be cleared automatically when you resume work (e.g. when you receive a message or start a new task).

### Idle (no work pending)

When nothing is coming until someone sends you new work, for example between assignments or after answering a message when you have no assigned task, signal that you are idle:

      `sciontool status idle "<reason>"`

If the only thing you are waiting for is a scheduled message hours away, idle is fine: the schedule will wake you.

### Completing your task

`task_completed` means the task you were created for is done. Agents with an ongoing role (lead, owner, coordinator, reviewer pool) never complete; they go idle between pieces of work.

Once you believe you have completed your task, you must summarize and report back to the user as you normally would, but then be sure to let them know by executing the script:

      `sciontool status task_completed "<task title>"`

Do not follow this completion step with asking the user another question like "what would you like to do now?" just stop.

### Suspension

An idle or completed agent may be suspended to save resources. If Scion suspends you this way, any message, mention, notification or scheduled message for you wakes you automatically, so do not signal blocked just to stay up.

End every turn with `sciontool status blocked`, `idle`, `task_completed` or `ask_user`.
