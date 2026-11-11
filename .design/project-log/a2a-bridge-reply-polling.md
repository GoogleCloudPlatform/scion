# A2A bridge: agent replies read from the caller's own conversation

Tracked in upstream issue 4603. Bridge-only change on upstream main `163f4e8f0c37`; no Hub or hubclient change.

## Problem

An agent's reply to a message the A2A bridge sent carries a channel (reply affinity, else the `web` default for direct conversations). The Hub's fan-out bus hands a message with a channel only to the in-process bus and the spoke of that channel, and the bridge's broker spoke has none, so the reply never reached the bridge and the A2A call timed out, in plugin and standalone mode alike.

## What changed

- **Reply poller** (`internal/bridge/reply_poller.go`). While an executor call waits, the bridge reads the reply from the caller's direct conversation with the agent through the Hub API, with the caller's own credential: list the caller's direct conversations (no `limit`; the Hub truncates after sorting) and pick the one whose `dmPeer` is the agent; read the anchor (the `message_id` of the send) once for its time; read newest-first pages after `anchor - 1s`, following `nextCursor` up to 4 pages. Only messages above the anchor on the pages read count (the page order is the Hub's keyset order); the oldest agent message of a reply type (`instruction`, `reply`, `chat`, `group-set`, empty) or question type (`input-needed` with a thread ID) is the reply. Notification rows (`input-needed` or `state-change` with no thread ID) and `state-change`, `system`, `mention`, `assistant-reply` rows are skipped.
- **One emit path.** The poller appends the reply with dedup key `poll:<hub message ID>`; for a call with a live poller the wait accepts only `poll:` events and a canceled final event (`waitOptions.acceptEvent`). The push path never produces a `poll:` key (an incoming `msgId` with that prefix is ignored). Push status is agent-wide (Hub subscriber notifications), so it never ends such a call.
- **Preflight and refusals.** Before the send, the caller's list and (for an existing conversation) a one-row read run the same checks as a poll. 401 and 403 are terminal everywhere; 404 on the anchor or a page, 5xx and transport errors are retried until the deadline; a conversation not listed after a send that returned an anchor is terminal. Never an admin fallback.
- **Same-caller check** in each bridge process: one waiting call per (reader, agent). Reads are capped per process (`reply_poll.concurrency`, 64).
- **Push path.** The task is marked before the send; pushed messages for a marked task are dropped. Plugin-mode correlation now accepts a message only from the task's agent, on a user topic of the task's project and caller (admin topic for legacy calls and agent callers), from the local task cache only; the legacy-store fallbacks are gone. The admin subscription uses `hub.user_id` when set. Push and broker log lines carry reason codes and topic or sender kinds only; the accepted-message line logs type, text length and sender kind, never text.
- **Plugin continuation**: the wait starts after the task's last event.
- **Rollback**: `reply_poll.enabled: false` restores waiting on pushes only.

## Notes for later work

- Real-hub tests need callers that may message the agent: project membership alone does not grant `agent.message`; the fixtures record callers in the agent's ancestry.
- Standalone push correlation compares the topic's project ID with the route's project key, so the standalone fixtures expose the project by its ID (the existing slug/ID limit in standalone correlation).
- The executor stops and joins the poller and clears the mark before it yields the final event; the poller's append fence (standalone: SDK snapshot not terminal, lease held) closes the window after that.
- Follow-ups: durable same-caller check and duplicate push webhooks across standalone replicas; attachments on polled replies; removal of the test-only `Bridge.SendMessage`, `sendFollowUp` and `SendStreamingMessage` paths; standalone push-path correlation.
- Follow-up changes: the same-caller check now runs before the task is registered for push correlation; the real-hub log check scans only the bridge's logger (the hub's own buses log subscription topics); the standalone append fence has a PostgreSQL test; a reply the bridge's task store fails to record is not read again (README limit); a real-hub test with a project-restricted user access credential is still to do.
- Second follow-up: once the append fence passes, a polled reply is appended on a bounded context that stopping the poller does not cancel (stop waits for the append), so the wait returning on the reply's artifact event no longer cuts off its message event and push dispatches. The PostgreSQL fence test (`TestReplyPoller_StandaloneAppendFence`, `./internal/bridge`) needs `TEST_DATABASE_URL` and is not run by the PostgreSQL CI job, which runs `./integration` only; adding it there is a follow-up.
