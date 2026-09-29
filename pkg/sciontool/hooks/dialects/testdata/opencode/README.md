# OpenCode hook-usage fixtures

Both files here are built from real, live captures of `opencode-ai@1.18.33`
(installed fresh from npm), driven against a local, credential-free mock
model server implementing the OpenAI-compatible chat-completions streaming
API (`@ai-sdk/openai-compatible`, custom `baseURL`). No real model provider
or credentials were used for either file.

## `bus-events-1.18.33.json`

Raw OpenCode bus events (`{id, type, properties}`, the shape delivered to
`scion-bridge.js`'s `event` plugin hook), captured via a throwaway logging
plugin that recorded every `event` hook delivery verbatim, one JSON object
per line as `{kind: "event", t: <capture timestamp>, payload: <the bus
event>}` (or `{kind: "tool.execute.before"|"tool.execute.after", t, payload:
{input, output}}` for the two real keyed hooks). Four real capture runs:

- **run2** — `opencode serve` plus an `@opencode-ai/sdk` client script, one
  isolated opencode process: creates a session, sends one prompt that drives
  the mock model through a scripted 3-step tool loop (`bash`, then `read`,
  then a final answer with no more tool calls), then calls
  `POST /session/{id}/fork` at the last assistant message. Provides the
  multi-step tool loop, streaming deltas, repeated `message.updated` events,
  and a real session fork (2 of the 3 `step-finish` parts get replayed under
  new session/message/part IDs). No records were dropped from this run's raw
  capture beyond the type filter and delta cap described below.
- **run3** — `opencode run` against a second mock provider that always
  returns HTTP 500, one isolated opencode process. Captures a real
  `session.error` after OpenCode's internal retries are exhausted, followed
  by two real `session.idle` events for that same session (OpenCode's own
  retry behaviour, not a fixture artifact). No records dropped beyond the
  type filter and delta cap.
- **run4** — `opencode run` against the working mock model with
  `"permission": {"bash": "ask"}` configured, headless (no interactive
  responder, so the request is auto-rejected), one isolated opencode
  process. Captures a real `permission.asked` followed by
  `permission.replied` (`reply: "reject"`). No records dropped beyond the
  type filter and delta cap.
- **run5** — `opencode run` with the `task` tool: the mock model calls
  `task` with `subagent_type: "general"`, which OpenCode executes as a real
  child session. Captures a real `session.created` for the child with
  `info.parentID` set to the parent session's ID, and the fact that the
  child's `session.idle` fires before the parent's, plus real child-session
  `step-finish` usage (the subagent's own model calls). **Unlike run2-4,
  the preserved raw file for this run is not the full capture**: this run
  reused a shared, not-yet-isolated capture sink (a leftover from an earlier
  sanity check, before per-invocation capture output was set up), so the raw
  file was first filtered down to only the two session IDs belonging to this
  run (the parent's and the child's) before anything else was applied. That
  extraction step could not be re-verified against a fuller, unfiltered
  capture, because one was never preserved. It is unlikely to have dropped
  anything the bridge consumes, since every event either carries one of
  those two session IDs in `properties.sessionID`/`properties.info.id` (bus
  events) or `input.sessionID` (the two keyed tool hooks) — but the
  extraction script only matched on `kind == "event"` and did not check
  `tool.execute.before`/`tool.execute.after` records at all, so every
  `tool.execute.*` record for run5 was dropped outright, regardless of its
  session ID, even though the `task` tool call itself demonstrably ran. This
  is a real gap in this one run's raw file, disclosed rather than smoothed
  over: run5 contains no tool-hook coverage.

### Filtering

Beyond run5's session-ID pre-extraction above, every run then goes through
the same two steps to build the committed fixture:

1. **Type filter.** Only these bus event types are kept (the ones
   `scion-bridge.js`'s `route()` or the JS test actually reads, plus
   `message.part.delta`, kept deliberately to prove it's ignored):
   `session.created`, `session.idle`, `session.error`, `session.status`,
   `message.updated`, `message.part.updated`, `message.part.delta`,
   `permission.asked`, `permission.replied`. Every other bus event type
   present in the raw capture (`session.updated`, `session.diff`,
   `plugin.added`, `catalog.updated`, `reference.updated`,
   `integration.updated`) is dropped — none of these is consumed by the
   bridge. `tool.execute.before`/`tool.execute.after` records are kept
   (except in run5, per above).
2. **Delta cap.** `message.part.delta` is capped at 2 kept records per run
   (the raw captures have many more; the bridge ignores every one, so only
   enough are kept to exercise that). `session.status` has no such cap: it
   is kept in full, because the busy/retry gate (`scion-bridge.js`'s
   `routeSessionStatus`) needs the real sequence and count of these per run,
   not just one example.
3. **Record wrapper.** Each kept record's `{kind, t, payload}` wrapper (see
   above) becomes `{run, kind, payload}` in the committed fixture: `t` (the
   capture-side timestamp, never used by any consumer) is dropped, and `run`
   (one of `run2`/`run3`/`run4`/`run5`, added by the build step) identifies
   which of the four runs above the record came from.

An early, uncaptured-isolation sanity check (`opencode run` before
per-invocation capture output was set up, the same leftover capture sink
run5 partly reused) is otherwise **not** included as its own run: on its
own it interleaves `session.created` for four unrelated opencode
invocations and was never used to build this fixture directly.

### Scrubs

Exactly three substitutions are applied, verbatim, everywhere they occur in
every run, and nothing else is changed:

- the capture host's scratch project directory path → a placeholder repo
  path;
- a git snapshot hash that OpenCode records on every step-start/step-finish
  part → a placeholder hash string;
- the capture's internal OpenCode project ID (a per-machine identifier
  OpenCode generates for the working directory, unrelated to any Scion
  project) → a placeholder ID string.

The real values are not reproduced here (they identify only a throwaway
capture sandbox, not anything sensitive, but there is no reason to publish
them either); grep the fixture for the placeholder strings
(`/workspace/project`, `<git-snapshot-hash>`, `<project-id>`) to see exactly
where each substitution landed.

## `hook-payloads-1.18.33.jsonl`

The real stdin `scion-bridge.js` wrote to a stub `sciontool` binary at
capture time, while replaying run2's exact session live (a separate run from
the one that produced `bus-events-1.18.33.json`'s run2 records, same scripted
3-step-then-fork scenario). One scrub: the `read` tool's captured
`tool_input` path (the same capture-host scratch directory as above) →
the same placeholder repo path. Nothing else differs from the raw capture.

`scion-bridge.js` has changed since this file was captured (activity-gated
agent-end, unmapped `session.error`, task-tool child-session filtering).
Re-verified equivalent: replaying `bus-events-1.18.33.json`'s own run2 records
through the current `route()` reproduces the same emission sequence and the
same token/tool values as this file, modulo session and message IDs (the two
captures are separate runs, so their IDs differ).
