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

function getErrorString(err, defaultString = "") {
  if (!err) return defaultString;
  if (typeof err === 'string') return err;
  if (typeof err.message === 'string') return err.message;
  if (err.data && typeof err.data.message === 'string') return err.data.message;
  return JSON.stringify(err);
}

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
// emitted, and the provider/model pair for each assistant message.
export function createBridgeState() {
  return {
    liveMessageIds: new Set(),
    seenModelEnds: new Set(),
    modelByMessage: new Map(),
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
      return routeSessionCreated(event);
    case 'session.idle':
      return [{ name: 'session.idle', data: { session_id: event.properties?.sessionID } }];
    case 'session.error':
      return [{
        name: 'session.error',
        data: {
          session_id: event.properties?.sessionID,
          error: getErrorString(event.properties?.error, 'Unknown error'),
          reason: 'error',
        },
      }];
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

function routeSessionCreated(event) {
  const info = event.properties?.info;
  return [{ name: 'session.created', data: { session_id: info?.id } }];
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
  }

  if (!info.time || !info.time.completed) {
    state.liveMessageIds.add(info.id);
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

  const tokens = part.tokens || {};
  const input = numberOrZero(tokens.input);
  const reasoning = numberOrZero(tokens.reasoning);
  // Canonical output includes reasoning (design §3.2); OpenCode's own
  // `output` field is exclusive of it, so the sum happens here, once, and
  // nowhere else in this file or in dialect.yaml.
  const output = numberOrZero(tokens.output) + reasoning;
  const cacheRead = numberOrZero(tokens.cache && tokens.cache.read);
  const cacheWrite = numberOrZero(tokens.cache && tokens.cache.write);
  const hasKnownUsage = tokens.total !== undefined || input > 0 || output > 0 || cacheRead > 0 || cacheWrite > 0;

  const data = { session_id: sessionID };

  const model = state.modelByMessage.get(messageID);
  if (model) {
    // No current Scion consumer reads this field: hook-sourced usage
    // (pkg/sciontool/hooks/handlers/telemetry.go) attributes `model` from
    // the SCION_MODEL environment variable for every harness, the same as
    // it does for Claude, Gemini and Codex hook events. It is included here
    // so the payload already carries the joined value if a future change
    // adds per-event model attribution.
    data.model = `${model.providerID}/${model.modelID}`;
  }

  // design §3.7: all-zero tokens with `total` undefined mean unknown usage —
  // count the call (the model-end event itself does that), but emit no
  // token fields at all, rather than a misleading all-zero response. Only
  // fields greater than zero are ever included below, matching the
  // dialect's own token-field semantics (dialects/common.go's extractTokens
  // and dialects/mapping.go's applyFieldPath both treat "present and > 0"
  // as the only way a value is recorded).
  if (hasKnownUsage) {
    if (input > 0) data.input_tokens = input;
    if (output > 0) data.output_tokens = output;
    if (cacheRead > 0) data.cached_tokens = cacheRead;
    if (cacheWrite > 0) data.cache_write_tokens = cacheWrite;
    if (reasoning > 0) data.reasoning_tokens = reasoning;
  }

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
// *absence* of this event. Today's dialect.yaml exposed a fabricated
// `success: !output?.error` that was always true anyway, since
// `output.error` never exists on this hook's output.
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
