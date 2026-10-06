# OpenCode Harness Bundle

Scion harness configuration for [OpenCode](https://opencode.ai), an open-source
AI coding assistant.

## Install

From a repository checkout:

```sh
scion harness-config install harnesses/opencode
```

Or directly from GitHub:

```sh
scion harness-config install github.com/GoogleCloudPlatform/scion/tree/main/harnesses/opencode
```

## Auth Modes

| Mode | Env / File | Notes |
|------|-----------|-------|
| `api-key` (default) | `ANTHROPIC_API_KEY` or `OPENAI_API_KEY` | Anthropic key takes precedence |
| `auth-file` | `~/.local/share/opencode/auth.json` | OpenCode native auth file |

## Bundle Layout

```
opencode/
  config.yaml       # Harness configuration (provisioner, capabilities, auth)
  provision.py       # Container-side provisioner (pre-start hook)
  Dockerfile         # Image build (FROM scion-base)
  cloudbuild.yaml    # Cloud Build configuration
  home/
    .config/opencode/opencode.json   # OpenCode client settings
```

## Telemetry

OpenCode has no usable native OTel usage signal (it emits `ai.streamText.doStream`
spans only when `experimental.openTelemetry` is explicitly enabled and only to
an externally configured OTLP endpoint), so model calls and tokens are
published from hooks instead. `provision.py` declares
`SCION_USAGE_SOURCE=hooks` (see `.design/hosted/usage-telemetry.md` §3.7 and
[the metrics docs](../../docs-site/src/content/docs/hosted/single-node/metrics.md)
for the full contract).

`home/.config/opencode/plugins/scion-bridge.js` subscribes to OpenCode's
generic `event` bus hook — not same-named keyed hooks, which never fire for
session, permission or model-usage events — and emits one `model-end` per
completed LLM step (a `step-finish` bus part), deduped on
`(sessionID, messageID, part.id)` with fork replays excluded. `dialect.yaml`
maps that event, plus `session.created`/`session.idle`/`permission.asked`/
`permission.replied`, onto the normalized Scion event grammar.
`tool.execute.before`/`tool.execute.after` remain real, directly subscribed
keyed hooks.

Known undercount: a model call that produces no `step-finish` part (a failed
or retried attempt, an abort, title generation, or agent generation) is not
counted.

**Task-tool subagent sessions are excluded from session-start and
agent-end.** OpenCode's `task` tool spawns a full child session with
`info.parentID` set to the invoking session (confirmed against a real
capture). The bridge tracks which session IDs are children from their
`session.created` event and filters that event and the child's later
`session.idle` — a subagent's idle turn is not the parent agent's, and must
not stand in for the parent's status. A forked session (`Session.fork`) has
no `parentID` at all and is unaffected by this filter; only task-tool
children are. Child-session model usage (`step-finish` parts) is still
counted normally — only the session-lifecycle signals are filtered.

**`session.error` emits nothing by itself; its error rides on the turn's
`agent-end`.** A
session's turn ends exactly once, on `session.idle` — routing
`session.error` to any lifecycle event as well (even a non-terminal one)
would count that same turn a second time, since `session.idle` follows a
`session.error` in the captured error path and, per OpenCode's source, in the
abort path (the one known path without an idle, context-overflow
auto-compaction, continues the turn rather than ending it). That matters
because every agent-end increments a turn counter (`max_turns`), so
double-counting could shut a working agent down on a single recoverable error
or user abort. Instead, the bridge remembers the error name and HTTP status
only (for example `APIError (status 429)`; never the message, response body,
headers or URL; see `sessionErrorText`) for the armed session and attaches
it as `error` to that turn's one gated `session.idle` emission, then clears
it, so the failed turn's `agent.turn.end` span and log carry error status.
A user abort (`MessageAbortedError`) is not recorded, an error on a session
that is not mid-turn or is a task-tool child is ignored, and a run that goes
busy again after the error (auto-compaction) clears it. See `dialect.yaml`'s
comment for the full reasoning.

**`agent-end` is gated on a prior `session.status` busy (or retry), not on
message/part activity.** OpenCode's own `session.idle` is not 1:1 with a real
turn: an errored or aborted turn publishes it once right after the error,
then again after OpenCode's own cleanup finishes rewriting every in-flight
part. Arming on message or part activity would not work here: that cleanup
step *is* a part update, so it would re-arm the gate and produce two
`agent-end`s for one aborted turn. `session.status{type:"busy"}` (or
`"retry"`, during a provider retry) is published from the run loop and
processor on each step or attempt of an active run, but never from the
error/cleanup path, from `SessionSummary`, or from cancelling an
already-idle session — so the bridge instead remembers, per session,
whether a busy or retry was seen since the session's last emitted
`agent-end`, and turns a `session.idle` into `agent-end` only if so,
clearing the flag either way. This makes `agent-end` — and therefore turn
counts — track real prompt cycles even across a mid-response abort or a
retried provider call.

## Build the Image

```sh
# Local Docker build
docker build --build-arg BASE_IMAGE=scion-base:latest -t scion-opencode:latest -f Dockerfile .

# Cloud Build
gcloud builds submit --config cloudbuild.yaml .
```
