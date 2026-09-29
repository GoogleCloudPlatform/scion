# OpenCode hook-usage fixtures

Both files here are built from real, live captures of `opencode-ai@1.18.33`
(installed fresh from npm), driven against a local, credential-free mock
model server implementing the OpenAI-compatible chat-completions streaming
API (`@ai-sdk/openai-compatible`, custom `baseURL`). No real model provider
or credentials were used for either file.

## `bus-events-1.18.33.json`

Raw OpenCode bus events (`{id, type, properties}`, the shape delivered to
`scion-bridge.js`'s `event` plugin hook), captured via a throwaway logging
plugin that recorded every `event` hook delivery verbatim. Four real capture
runs, each a single, isolated opencode process:

- **run2** — `opencode serve` plus an `@opencode-ai/sdk` client script:
  creates a session, sends one prompt that drives the mock model through a
  scripted 3-step tool loop (`bash`, then `read`, then a final answer with no
  more tool calls), then calls `POST /session/{id}/fork` at the last
  assistant message. Provides the multi-step tool loop, streaming deltas,
  repeated `message.updated` events, and a real session fork (2 of the 3
  `step-finish` parts get replayed under new session/message/part IDs).
- **run3** — `opencode run` against a second mock provider that always
  returns HTTP 500. Captures a real `session.error` after OpenCode's
  internal retries are exhausted.
- **run4** — `opencode run` against the working mock model with
  `"permission": {"bash": "ask"}` configured, headless (no interactive
  responder, so the request is auto-rejected). Captures a real
  `permission.asked` followed by `permission.replied` (`reply: "reject"`).
- **run5** — `opencode run` with the `task` tool: the mock model calls
  `task` with `subagent_type: "general"`, which OpenCode executes as a full
  child session. Captures a real `session.created` for the child with
  `info.parentID` set to the parent session's ID, and the fact that the
  child's `session.idle` fires before the parent's.

### Filtering

Every record is one of these bus event types (the ones `scion-bridge.js`'s
`route()` or the JS test actually reads, plus `message.part.delta` kept
deliberately to prove it's ignored):
`session.created`, `session.idle`, `session.error`, `message.updated`,
`message.part.updated`, `message.part.delta`, `permission.asked`,
`permission.replied`. Every other bus event type present in the raw capture
(`session.updated`, `session.status`, `session.diff`, `plugin.added`,
`catalog.updated`, `reference.updated`, `integration.updated`) is dropped —
none of these is consumed by the bridge. `message.part.delta` is capped at
2 kept records per run (the raw captures have many more; the bridge ignores
every one, so only enough are kept to exercise that).

An early, uncaptured-isolation sanity check (`opencode run` before per-
invocation capture output was set up) is **not** included: that raw file
interleaves `session.created` for four unrelated opencode invocations and
was never used to build this fixture.

### Scrubs

Exactly three substitutions are applied, verbatim, everywhere they occur,
and nothing else is changed:

| Real value | Fixture value |
|---|---|
| `/tmp/opencode-cap/project` (the capture host's scratch project directory) | `/workspace/project` |
| `eca6650cd5d5edfaec4adf1553636efbf00ae290` (a git snapshot hash opencode records per step) | `<git-snapshot-hash>` |
| `0180e1f1645682e9cb47120769f22f7596baca86` (the capture's internal opencode project ID) | `<project-id>` |

## `hook-payloads-1.18.33.jsonl`

The real stdin the *actual, currently shipped* `scion-bridge.js` wrote to a
stub `sciontool` binary while replaying run2's exact session live (a separate
run from the one that produced `bus-events-1.18.33.json`'s run2 records, same
scripted 3-step-then-fork scenario). One scrub: the `read` tool's captured
`tool_input` path, `/tmp/opencode-cap/project/README.md` →
`/workspace/project/README.md`. Nothing else differs from the raw capture.
