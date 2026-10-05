// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Scion hook bridge plugin for OpenCode.
//
// Session lifecycle, model usage and permission events are OpenCode *bus*
// events, not plugin hook keys: they only reach a plugin through the single
// generic `event` hook (`Hooks.event`, `{event: {id, type, properties}}`),
// never through a same-named keyed hook. Subscribing to keyed hooks named
// "session.created", "message.updated", etc. (as an earlier version of this
// file did) never fires. `tool.execute.before` and `tool.execute.after` are
// the only real keyed hooks this bridge uses.
//
// Usage (model-end) is derived from `message.part.updated` events carrying a
// `step-finish` part, one per completed LLM step, instead of debouncing
// `message.updated` (which fires for every streaming delta and does not
// correspond 1:1 with model calls). See routeMessagePartUpdated below and
// `.design/hosted/usage-telemetry.md` §3.7 for the mapping this implements.
//
// `message.part.delta` fires once per streamed token; every check in this
// file that could match it is a cheap, synchronous JS comparison performed
// *before* any `execSync` call, so a busy stream never spawns a process per
// chunk.

import { execSync } from 'node:child_process';

const HOOK_TIMEOUT_MS = 5000;

function emitHookEvent(eventName, data) {
  try {
    const payload = JSON.stringify({
      hook_event_name: eventName,
      ...data,
    });
    execSync('sciontool hook --dialect=opencode', {
      input: payload,
      stdio: ['pipe', 'ignore', 'ignore'],
      timeout: HOOK_TIMEOUT_MS,
    });
  } catch (err) {
    // Best-effort — never crash the plugin.
    if (process.env.SCION_HOOK_DEBUG) {
      console.error(`[scion-bridge] ${eventName}: ${err.message}`);
    }
  }
}

function numberOrZero(v) {
  return typeof v === 'number' && Number.isFinite(v) ? v : 0;
}

// MAX_CACHE_ENTRIES bounds every per-session/per-message cache below. A
// long-lived `opencode serve` process never gets a natural point to forget
// old sessions or messages, so without a cap these grow for the life of the
// process (roughly 150-250 bytes per step; unbounded, though harmless for a
// normal-length session). Eviction is FIFO by insertion order (Map/Set
// preserve it, and re-adding an existing key does not move it), not by
// completion time: the capture shows a step-finish part can arrive some
// time after its message's first `message.updated`, and a PATCH re-emit of
// an already-handled part must still be deduped, so nothing here is safe to
// evict "as soon as done". One accepted consequence: if a task-tool child
// session is evicted from childSessionIds before that same child later goes
// idle or errors, its event is treated as top-level (a spurious
// session-start/agent-end pair). That needs more than MAX_CACHE_ENTRIES
// other child sessions created in between -- for example a task resumed by
// `task_id` much later in a very long-lived `opencode serve` process.
const MAX_CACHE_ENTRIES = 4096;

// evictOldest trims a Map or Set (both expose .size, .keys(), .delete()) down
// to maxEntries by removing entries in insertion order, oldest first.
function evictOldest(collection, maxEntries) {
  while (collection.size > maxEntries) {
    const oldestKey = collection.keys().next().value;
    collection.delete(oldestKey);
  }
}

// ---------------------------------------------------------------------------
// Pure event-routing core.
//
// Exported so a JS unit test can drive it directly against captured (or
// synthetic) bus events without spawning sciontool. No I/O happens here;
// `route()` only returns the list of {name, data} hook emissions a caller
// should make for one bus event, given the bridge's running state.
// ---------------------------------------------------------------------------

// createBridgeState returns the mutable, per-plugin-instance state route()
// needs across calls: which assistant messages have been observed live (as
// opposed to replayed by a fork), which model-end parts have already been
// emitted, the provider/model pair for each assistant message, which
// session IDs are task-tool subagent children (see routeSessionCreated), and
// which sessions have been busy (or retrying) since their last emitted
// agent-end (see routeSessionIdle), and the pending error text for an armed
// session whose turn failed (see routeSessionError).
export function createBridgeState() {
  return {
    liveMessageIds: new Set(),
    seenModelEnds: new Set(),
    modelByMessage: new Map(),
    childSessionIds: new Set(),
    busySessionIds: new Set(),
    pendingErrors: new Map(),
  };
}

// route computes the hook emissions for one raw opencode bus event
// ({id, type, properties}). It never throws and returns [] for anything not
// relevant to Scion telemetry.
export function route(state, event) {
  if (!event || typeof event.type !== 'string') return [];

  switch (event.type) {
    case 'message.updated':
      return routeMessageUpdated(state, event);
    case 'message.part.updated':
      return routeMessagePartUpdated(state, event);
    case 'session.created':
      return routeSessionCreated(state, event);
    case 'session.idle':
      return routeSessionIdle(state, event);
    case 'session.status':
      return routeSessionStatus(state, event);
    // session.error never emits by itself: see routeSessionIdle's doc
    // comment for why a session's turn must end exactly once, on
    // session.idle, and never a second time here. Its error text is only
    // remembered, and carried on that turn's single agent-end.
    case 'session.error':
      return routeSessionError(state, event);
    // Real runtime event names, confirmed against a live capture (npm
    // opencode-ai 1.18.33): "permission.asked" / "permission.replied", each
    // shaped as {properties: {id, sessionID, permission, patterns, ...}} /
    // {properties: {sessionID, requestID, reply}}. The published
    // @opencode-ai/sdk 1.18.33 type declarations instead describe
    // "permission.updated" with a differently-shaped payload
    // ({permissionID, response}); that schema was never observed on the
    // wire and is not handled here.
    case 'permission.asked':
      return [{
        name: 'permission.asked',
        data: {
          message: event.properties?.metadata?.command || event.properties?.permission || 'Permission requested',
        },
      }];
    case 'permission.replied':
      return [{ name: 'permission.replied', data: {} }];
    default:
      return [];
  }
}

// routeSessionCreated excludes task-tool subagent child sessions from
// session-start: OpenCode's `task` tool spawns a full child session with
// `info.parentID` set to the invoking session, confirmed against a real
// capture of a task-tool call (see the fixture's provenance). Counting a
// child session as its own agent-level session would inflate session counts
// relative to what the user sees; a fork has no `parentID` at all (it is a
// sibling copy of history, not a child), so this filter never touches
// fork-derived sessions.
function routeSessionCreated(state, event) {
  const info = event.properties?.info;
  if (!info || !info.id) return [];
  if (info.parentID) {
    state.childSessionIds.add(info.id);
    evictOldest(state.childSessionIds, MAX_CACHE_ENTRIES);
    return [];
  }
  return [{ name: 'session.created', data: { session_id: info.id } }];
}

// routeSessionIdle maps a session's session.idle to one agent-end, gated two
// ways:
//
// 1. Child-session exclusion: `session.idle` carries only `{sessionID}`, no
//    `parentID`, so the child-ness has to be remembered from that session's
//    own `session.created` event. A child session going idle only means
//    that subagent's turn ended, not the parent agent's turn -- signalling
//    agent-end for it would be wrong while the parent may still be working
//    (confirmed in the capture: the child's session.idle fires before the
//    parent's).
//
// 2. Busy gating: OpenCode's own session.idle is not 1:1 with a real turn.
//    Traced through OpenCode v1.18.33's SessionProcessor (process/halt/
//    cleanup); confirmed against the captures for an errored turn, and
//    against OpenCode's source for an aborted turn (the bridge's own
//    abort tests are labelled synthetic): either publishes session.idle
//    once right after the error or abort (from `halt`), then runs
//    `cleanup`, which rewrites every in-flight text/reasoning/tool part
//    via message.part.updated, then publishes session.idle a
//    second time once the runner actually finishes. Arming on message or
//    part activity would not work here: cleanup's own part updates would
//    re-arm the gate, so a mid-response abort or error would still produce
//    two agent-ends. Arming on session.status{type:"busy"|"retry"} instead
//    avoids this: OpenCode publishes busy from the run loop and processor
//    on each step or attempt of an active run (including each retry
//    attempt), but never from halt, cleanup, SessionSummary, or an
//    idle-only cancel. Every agent-end increments a turn counter
//    (hooks/handlers/limits.go's max_turns, the hub turn count, and the
//    aggregator's RecordTurn), so counting a turn more than once per real
//    prompt cycle can trip max_turns on a single error or user abort.
//    session.idle only produces agent-end, and clears the busy flag, the
//    first time it fires after a busy/retry was seen; a second idle with
//    nothing new in between (OpenCode's own duplicate idle, or the idle
//    that always follows a session.error) is silently dropped.
//
// A pending error recorded by routeSessionError is attached to that one
// gated emission as `error`, then cleared, so a failed turn's agent-end
// carries its error status while still counting the turn exactly once.
function routeSessionIdle(state, event) {
  const sessionID = event.properties?.sessionID;
  if (!sessionID) return [];
  if (state.childSessionIds.has(sessionID)) return [];
  if (!state.busySessionIds.has(sessionID)) return [];
  state.busySessionIds.delete(sessionID);
  const data = { session_id: sessionID };
  const error = state.pendingErrors.get(sessionID);
  if (error) data.error = error;
  state.pendingErrors.delete(sessionID);
  return [{ name: 'session.idle', data }];
}

// routeSessionError never emits a hook event by itself (routing it to any
// lifecycle event would count a failed turn twice; see routeSessionIdle).
// It records a bounded error string for the session, which the next gated
// session.idle attaches to its agent-end and clears. Recorded only when:
//
// - the session is a top-level session (a child's idle is never emitted,
//   so its error would never be consumed);
// - the session is armed (busy/retry seen since its last agent-end): an
//   error outside an active turn belongs to no turn, and keeping it would
//   mislabel the next, unrelated one;
// - the error is not a user abort (MessageAbortedError): an abort is the
//   user ending the turn, not the turn failing.
//
// routeSessionStatus clears a pending error when the session goes busy or
// retries again afterwards: the run continued past the error (OpenCode's
// context-overflow auto-compaction path publishes session.error with no
// idle and keeps running), so the turn did not end on it.
function routeSessionError(state, event) {
  const sessionID = event.properties?.sessionID;
  if (!sessionID) return [];
  if (state.childSessionIds.has(sessionID)) return [];
  if (!state.busySessionIds.has(sessionID)) return [];
  const err = event.properties?.error;
  if (err?.name === 'MessageAbortedError') return [];
  state.pendingErrors.set(sessionID, sessionErrorText(err));
  evictOldest(state.pendingErrors, MAX_CACHE_ENTRIES);
  return [];
}

// ERROR_NAME_RE accepts an error class name (e.g. "APIError",
// "ProviderAuthError"): an identifier, bounded in length. Anything else in
// the `name` field is not carried.
const ERROR_NAME_RE = /^[A-Za-z][A-Za-z0-9_.]{0,63}$/;

// sessionErrorText renders an OpenCode session.error `error` object
// ({name, data: {message, statusCode, ...}}) for turn-end telemetry (span
// status and log). It carries only structured fields: the error class name
// and, when present, the numeric HTTP status -- e.g. "APIError (status
// 429)" or "ProviderAuthError". Free-text fields (data.message, response
// body, headers, URL) are never carried: provider messages can echo
// credentials or account details, and no pattern list can reliably scrub
// them. The full error remains in opencode's own log. The result is bounded
// by construction (a 64-character name plus a 3-digit status).
export function sessionErrorText(err) {
  const rawName = typeof err?.name === 'string' ? err.name.trim() : '';
  const name = ERROR_NAME_RE.test(rawName) ? rawName : 'session error';
  const status = Number(err?.data?.statusCode);
  if (Number.isInteger(status) && status >= 100 && status <= 599) {
    return `${name} (status ${status})`;
  }
  return name;
}

// routeSessionStatus never emits a hook event by itself. It only remembers
// that a session has been busy (or is retrying, which OpenCode treats the
// same way as busy for the duration of that attempt) since its last
// emitted agent-end -- see routeSessionIdle for how that gate is consumed.
// A child session's own busy signal is skipped: the child's session.idle is
// already unconditionally filtered in routeSessionIdle, so arming for it
// would never be consulted, just wasted cache space.
function routeSessionStatus(state, event) {
  const sessionID = event.properties?.sessionID;
  const statusType = event.properties?.status?.type;
  if (!sessionID || (statusType !== 'busy' && statusType !== 'retry')) return [];
  if (state.childSessionIds.has(sessionID)) return [];
  state.busySessionIds.add(sessionID);
  evictOldest(state.busySessionIds, MAX_CACHE_ENTRIES);
  // The run kept going after a recorded error, so the turn did not end on
  // it (see routeSessionError).
  state.pendingErrors.delete(sessionID);
  return [];
}

// routeMessageUpdated never emits a hook event by itself. It only updates
// bridge state: which provider/model an assistant message belongs to (for
// the model-end join in routeMessagePartUpdated), and whether the message
// was seen "live" — i.e. observed by this process while still in progress.
//
// Liveness is the fork-replay discriminator (design §3.7, "fork replays are
// excluded by counting only parts whose message was seen live"), confirmed
// against a real capture of `Session.fork`: a live assistant message always
// gets at least one `message.updated` with `time.completed` still unset
// before the one that completes it, because streaming starts before the
// response finishes. A forked session republishes every historical message
// under new IDs, but — since nothing is actually streaming — each forked
// message gets exactly one `message.updated`, and it already carries
// `time.completed`. So "was any update for this message ever incomplete"
// reliably separates the two cases; a replayed `step-start` part looks
// byte-for-byte identical to a live one and cannot be used for this (both
// carry the same fields), so this bridge does not treat step-start as a
// liveness signal.
function routeMessageUpdated(state, event) {
  const info = event.properties?.info;
  if (!info || info.role !== 'assistant' || !info.id) return [];

  if (info.providerID && info.modelID) {
    state.modelByMessage.set(info.id, { providerID: info.providerID, modelID: info.modelID });
    evictOldest(state.modelByMessage, MAX_CACHE_ENTRIES);
  }

  if (!info.time || !info.time.completed) {
    state.liveMessageIds.add(info.id);
    evictOldest(state.liveMessageIds, MAX_CACHE_ENTRIES);
  }

  return [];
}

// routeMessagePartUpdated emits one model-end per completed LLM step, from a
// `step-finish` part. Every other part type (text, tool, step-start,
// snapshot, ...) and every `message.part.delta` streaming chunk is ignored
// here as a plain object-shape check, with no process spawned.
function routeMessagePartUpdated(state, event) {
  const part = event.properties?.part;
  if (!part || part.type !== 'step-finish') return [];

  const sessionID = part.sessionID;
  const messageID = part.messageID;
  const partID = part.id;
  if (!sessionID || !messageID || !partID) return [];

  // Fork-replay exclusion (see routeMessageUpdated's doc comment).
  if (!state.liveMessageIds.has(messageID)) return [];

  // Dedupe on (sessionID, messageID, part.id): a PATCH re-emit of the same
  // part, or any other repeat delivery, is ignored after the first.
  const dedupeKey = `${sessionID}:${messageID}:${partID}`;
  if (state.seenModelEnds.has(dedupeKey)) return [];
  state.seenModelEnds.add(dedupeKey);
  evictOldest(state.seenModelEnds, MAX_CACHE_ENTRIES);

  const tokens = part.tokens || {};
  const input = numberOrZero(tokens.input);
  const reasoning = numberOrZero(tokens.reasoning);
  // Canonical output includes reasoning (design §3.2); OpenCode's own
  // `output` field is exclusive of it, so the sum happens here, once, and
  // nowhere else in this file or in dialect.yaml.
  const output = numberOrZero(tokens.output) + reasoning;
  const cacheRead = numberOrZero(tokens.cache && tokens.cache.read);
  const cacheWrite = numberOrZero(tokens.cache && tokens.cache.write);

  const data = { session_id: sessionID };

  const model = state.modelByMessage.get(messageID);
  if (model) {
    // dialect.yaml maps this onto the event's model, which hook-sourced
    // usage (pkg/sciontool/hooks/handlers/telemetry.go) prefers over
    // SCION_MODEL for the model label (design §3.2).
    data.model = `${model.providerID}/${model.modelID}`;
  }

  // design §3.7: all-zero tokens with `total` undefined mean unknown usage —
  // count the call (the model-end event itself does that), but emit no
  // token fields at all, rather than a misleading all-zero response. This
  // falls out of the guards below with no separate check: every field is
  // included only when it's greater than zero, matching the dialect's own
  // token-field semantics (dialects/common.go's extractTokens and
  // dialects/mapping.go's applyFieldPath both treat "present and > 0" as
  // the only way a value is recorded), so an all-zero step-finish already
  // produces none of these fields regardless of `total`.
  if (input > 0) data.input_tokens = input;
  if (output > 0) data.output_tokens = output;
  if (cacheRead > 0) data.cached_tokens = cacheRead;
  if (cacheWrite > 0) data.cache_write_tokens = cacheWrite;
  if (reasoning > 0) data.reasoning_tokens = reasoning;

  // "message.part.updated.step-finish" is a bridge-internal name, not an
  // OpenCode wire event: message.part.updated covers every part type, and
  // dialect.yaml needs a distinct mapping key for the step-finish case.
  return [{ name: 'message.part.updated.step-finish', data }];
}

// toolExecuteBeforeData and toolExecuteAfterData are pure (no execSync), so
// the JS unit test can check the field-extraction fix directly.

// The tool name is `input.tool` (there is no `input.name`), and the
// arguments are on the *second* parameter, `output.args` — not
// `input.args`, which is always undefined.
export function toolExecuteBeforeData(input, output) {
  return {
    tool_name: input?.tool || "unknown",
    tool_input: typeof output?.args === 'string' ? output.args : JSON.stringify(output?.args || {}),
  };
}

// This hook fires only when the tool call succeeded (OpenCode never invokes
// it on failure), and its output carries no error field. So success is
// unconditionally true here; a failed tool call is visible only as the
// *absence* of this event. The previous bridge computed
// `success: !output?.error`, which was always true because this hook's
// output never has `error`.
export function toolExecuteAfterData(input) {
  return {
    tool_name: input?.tool || "unknown",
    success: true,
  };
}

export const ScionBridge = async (ctx) => {
  const state = createBridgeState();

  return {
    event: async (input) => {
      for (const { name, data } of route(state, input?.event)) {
        emitHookEvent(name, data);
      }
    },

    "tool.execute.before": async (input, output) => {
      emitHookEvent("tool.execute.before", toolExecuteBeforeData(input, output));
    },
    "tool.execute.after": async (input) => {
      emitHookEvent("tool.execute.after", toolExecuteAfterData(input));
    },
  };
};
