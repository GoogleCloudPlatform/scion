# Multi-Agent Collaboration Tests

## Purpose

Test that scion can orchestrate multiple agents and subagents, while delegating and communicating correctly.
There are two ways this happens:

1. orchestrator passes task when starting the agent
2. orchestrator starts an agent and sends task as message

However, there is a known bug in the hub and messaging system.
When an agent immediately starts and then sends a message,
the recipient agent is not ready in time and the message is lost.

From this, we can create three test cases:

1. orchestrator passes task when starting the agent
2a. orchestrator starts an agent and sends task as message, without extra instructions
2b. orchestrator is given extra instructions to check if subagent is ready before sending message

## Test Overview

Your goal is to run and monitor testing across the three scenarios.
For each test case, you will start a new scion agent named `orchestrator`, which will then start another agent `code-reviewer`.
You should never interact with the agents directly, only start, monitor passively, and cleanup as needed.

**IMPORTANT** Run each test case sequentially, we only have one GPU.

```
you (test-runner)
└── orchestrator
    └── code-reviewer
```

For each of the three test cases, you start the orchestrator with the `scion start` command:

```bash
scion start orchestrator -y -t default --harness opencode --harness-auth none "${ORCHESTRATOR_INSTRUCTIONS}"
```


Now that the `orchestrator` has started, your job is to monitor the progress:

1. Did the `orchestrator` follow instructions for the test case and perform the steps in order?
2. Did the `code-reviewer` get the task by the method for the test case and follow its instructions?
3. Did the test case proceed as expected? Was the correct pattern used?
4. Did the agents set their status correctly as the progressed through processing?
5. Where there any hiccups and recoveries in the test case?

Do this for each test case in order. Once all three are done, write a final report to the user.

**Only report results as a message to the user.** You are running tests on behalf of a user who wants to know the summary of _what_ happened, _not_. Do not explore why, do not try to debug, do not try to work around system failures. Your only job is to run the tests and report the results.

We expect test case (1) and (2b) to pass without issue. Test case (2a) should fail if the `orchrestrator` follows instructions and does not try to recover. You should wait long enough that the system notifies the `orchestrator` that it's `code-reviewer` agent has stalled, at which point it may try to resend the message.



### Instructions for `orchestrator`

The `orchestrator` needs slightly different instructions for each test case,
such that it performs the correct sequence of steps required by the test case.
The template instructions are:

```
You are the `orchestrator` agent. Your job is to coordinate a code review process.

Follow this sequence of steps to start a `code-reviewer` agent

<<insert instructions for test case>>

Once your message has been sent, follow this sequence of steps to finalize the review process:

1. Set blocked status: `sciontool status blocked "Waiting for code-reviewer analysis"`
   - **CRITICAL: Immediately end your turn after setting blocked. Do NOT execute any further steps, do NOT poll for messages, do NOT continue working.** The blocked status is your signal that you are idle and waiting for a harness notification. Any work you do after setting blocked is wasted — you will not see the notification until your next turn.
2. Once you receive the report, create `/workspace/review-summary.md` containing:
   - A header section
   - code-reviewer's full report (copy their findings)
   - Your own brief assessment section at the end evaluating whether the functions are production-ready
3. Mark yourself complete: `sciontool status task_completed "Multi-agent code review complete"`


The task for the code-reviewer is:

<insert code-reviewer task here>
```

### Test Case 1: orchestrator passes task when starting the agent

1. Start a code-reviewer agent with the full task embedded in its initial message:
   `scion start code-reviewer -y -t default --harness opencode --harness-auth none "${TASK}"`

### Test Case 2a: orchestrator starts an agent and sends task as message, without extra instructions

1. Start a `code-reviewer` agent (no task, just boot it): `scion start code-reviewer -y -t default --harness opencode --harness-auth none`
2. Send the task as a message to the agent `scion message code-reviewer "${TASK}"`. Do not monitor, proceed to your blocked state.

### Test Case 2b: orchestrator is given extra instructions to check if subagent is ready before sending message

1. Start a `code-reviewer` agent (no task, just boot it): `scion start code-reviewer -y -t default --harness opencode --harness-auth none`
2. Wait until code-reviewer's phase is `running` (poll with `scion list --format json`), THEN verify it is actually ready by running `scion look code-reviewer --full --plain` and confirming it shows the agent prompt (not still initializing/booting), then send it this task:
3. Send the task as a message to the agent `scion message code-reviewer "${TASK}"`


### Instructions for `code-reviewer`

The `code-reviewer` subagent is the same in every case and should be given the following task by the `orchestrator`.

```
Analyze these three function specifications and provide a detailed review:

FUNCTION 1: calculateSum(numbers)
- Description: Sums all numbers in a list
- Input: array of integers
- Output: integer sum
- Edge cases: empty list returns 0, negative numbers allowed

FUNCTION 2: findMax(numbers)
- Description: Finds the maximum value in a list
- Input: array of integers
- Output: maximum integer
- Edge cases: empty list returns null, single element returns itself

FUNCTION 3: reverseString(s)
- Description: Reverses a string
- Input: string
- Output: reversed string
- Edge cases: empty string returns empty, single char returns itself

Provide:
(1) Time and space complexity for each (O notation)
(2) Whether each edge case is handled correctly and if any are missing
(3) One actionable improvement suggestion per function

After completing your analysis, you MUST send your report back to me using the scion CLI. Run this exact command:
   scion message orchestrator "YOUR COMPLETE REPORT HERE WITH ALL THREE SECTIONS"
   - This is mandatory. Do not just print your report in your response. You must execute the `scion message` command to deliver it through the Hub.
   - The command sends the message to the orchestrator agent by name. Use the exact format above with your full report as the message body. Send only a single message.
After sending the report, mark your task as complete:
   sciontool status task_completed "Multi-agent code review complete"
- **CRITICAL: Immediately end your turn after setting complete. Do NOT execute any further steps, do NOT continue working.** Setting complete is your signal that you are done.
```

## Cleanup

Always use `scion delete`, not `scion stop` — stop leaves stale state.
