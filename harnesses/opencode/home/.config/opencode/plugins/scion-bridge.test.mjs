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

// Node built-in test runner (Node >= 18; the opencode harness image has
// node available, since its Dockerfile installs opencode-ai with `npm
// install -g`). Run with: node --test scion-bridge.test.mjs
//
// Drives the real, shipped scion-bridge.js against
// pkg/sciontool/hooks/dialects/testdata/opencode/bus-events-1.18.33.json --
// a real capture. See pkg/sciontool/hooks/dialects/testdata/opencode/README.md
// for the fixture's full provenance (opencode-ai version, the mock model
// server, each run's scenario, every filter and scrub applied); the sibling
// hook-payloads-1.18.33.jsonl fixture's own provenance is documented in
// opencode_dialect_test.go. This is the "JS unit test for the bridge" design
// §9 phase 3b asks for, exercising exactly the code this harness ships, not
// a reimplementation of it.

import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import test from 'node:test';

import { createBridgeState, route, sessionErrorText, toolExecuteBeforeData, toolExecuteAfterData } from './scion-bridge.js';

const HERE = path.dirname(fileURLToPath(import.meta.url));
// harnesses/opencode/home/.config/opencode/plugins -> repo root is 6 levels up.
const REPO_ROOT = path.resolve(HERE, '..', '..', '..', '..', '..', '..');
const FIXTURE_PATH = path.join(
  REPO_ROOT, 'pkg', 'sciontool', 'hooks', 'dialects', 'testdata', 'opencode', 'bus-events-1.18.33.json'
);

function loadFixture() {
  const raw = JSON.parse(readFileSync(FIXTURE_PATH, 'utf8'));
  const byRun = new Map();
  for (const rec of raw) {
    if (!byRun.has(rec.run)) byRun.set(rec.run, []);
    byRun.get(rec.run).push(rec);
  }
  return byRun;
}

function emissionsFor(records, name) {
  const state = createBridgeState();
  const emissions = [];
  for (const rec of records) {
    if (rec.kind !== 'event') continue;
    for (const emission of route(state, rec.payload)) {
      emissions.push(emission);
    }
  }
  return name ? emissions.filter((e) => e.name === name) : emissions;
}

test('run2: N completed steps give exactly N model-end emissions', () => {
  const run2 = loadFixture().get('run2');
  assert.ok(run2 && run2.length > 0, 'fixture must contain run2 records');

  const stepFinishes = emissionsFor(run2, 'message.part.updated.step-finish');
  assert.equal(stepFinishes.length, 3, 'expected exactly 3 model-end emissions for the 3 real step-finish parts');
});

test('run2: fork replay contributes zero model-end emissions', () => {
  const run2 = loadFixture().get('run2');
  const sessionCreated = run2.filter((r) => r.payload.type === 'session.created');
  assert.equal(sessionCreated.length, 2, 'expected an original session and its fork');
  const originalSessionID = sessionCreated[0].payload.properties.info.id;
  const forkedSessionID = sessionCreated[1].payload.properties.info.id;
  assert.notEqual(originalSessionID, forkedSessionID);

  const stepFinishes = emissionsFor(run2, 'message.part.updated.step-finish');
  for (const emission of stepFinishes) {
    assert.equal(emission.data.session_id, originalSessionID, 'every model-end must belong to the original session, never the fork');
  }
});

test('run2: token math matches the captured step-finish parts, output includes reasoning', () => {
  const run2 = loadFixture().get('run2');
  const stepFinishes = emissionsFor(run2, 'message.part.updated.step-finish');
  assert.equal(stepFinishes.length, 3);

  // Real captured values (see fixture provenance): step1 has reasoning=5,
  // which must be summed into output_tokens (design §3.2); steps 2 and 3
  // have no reasoning and a cache read instead.
  assert.deepEqual(
    { input_tokens: stepFinishes[0].data.input_tokens, output_tokens: stepFinishes[0].data.output_tokens, reasoning_tokens: stepFinishes[0].data.reasoning_tokens, cached_tokens: stepFinishes[0].data.cached_tokens },
    { input_tokens: 400, output_tokens: 30, reasoning_tokens: 5, cached_tokens: undefined }
  );
  assert.deepEqual(
    { input_tokens: stepFinishes[1].data.input_tokens, output_tokens: stepFinishes[1].data.output_tokens, cached_tokens: stepFinishes[1].data.cached_tokens, reasoning_tokens: stepFinishes[1].data.reasoning_tokens },
    { input_tokens: 516, output_tokens: 25, cached_tokens: 384, reasoning_tokens: undefined }
  );
  assert.deepEqual(
    { input_tokens: stepFinishes[2].data.input_tokens, output_tokens: stepFinishes[2].data.output_tokens, cached_tokens: stepFinishes[2].data.cached_tokens },
    { input_tokens: 148, output_tokens: 18, cached_tokens: 1152 }
  );
  for (const e of stepFinishes) {
    assert.equal(e.data.cache_write_tokens, undefined, 'this mock model never reports a cache write, so the field must be omitted, not zero');
  }
});

test('run2: streaming deltas and repeated message.updated add nothing', () => {
  const run2 = loadFixture().get('run2');
  const deltaRecords = run2.filter((r) => r.payload.type === 'message.part.delta');
  assert.ok(deltaRecords.length > 0, 'fixture must include at least one message.part.delta to prove it is ignored');

  const messageUpdatedRecords = run2.filter((r) => r.payload.type === 'message.updated');
  assert.ok(messageUpdatedRecords.length > 3, 'fixture must include repeated message.updated events (more than one per message)');

  // route() on a state that has ONLY ever seen delta and message.updated
  // events (no step-finish parts at all) must emit nothing.
  const state = createBridgeState();
  const emissions = [];
  for (const rec of [...deltaRecords, ...messageUpdatedRecords]) {
    emissions.push(...route(state, rec.payload));
  }
  assert.deepEqual(emissions, []);
});

test('a repeated delivery of the same step-finish part is deduped', () => {
  const run2 = loadFixture().get('run2');

  // Feed the whole run2 sequence through ONE shared state twice, simulating
  // a retried delivery.
  const sharedState = createBridgeState();
  const combined = [];
  for (const rec of run2) {
    if (rec.kind !== 'event') continue;
    combined.push(...route(sharedState, rec.payload));
  }
  const secondPassCombined = [];
  for (const rec of run2) {
    if (rec.kind !== 'event') continue;
    secondPassCombined.push(...route(sharedState, rec.payload));
  }
  assert.equal(combined.filter((e) => e.name === 'message.part.updated.step-finish').length, 3);
  assert.equal(secondPassCombined.filter((e) => e.name === 'message.part.updated.step-finish').length, 0, 'the second delivery of the same (sessionID, messageID, part.id) must be deduped');
});

test('session and agent-end events route through the event hook', () => {
  const run2 = loadFixture().get('run2');
  const sessionCreatedEmissions = emissionsFor(run2, 'session.created');
  assert.equal(sessionCreatedEmissions.length, 2, 'both the original session and the fork fire session.created');

  const idleEmissions = emissionsFor(run2, 'session.idle');
  assert.equal(idleEmissions.length, 1);
});

test('agent-end count matches the number of real prompt cycles, for every real run', () => {
  // Each run below is one real prompt cycle from the user's perspective, so
  // each expects exactly one agent-end, for a different reason each time:
  const expected = {
    // run2: a 3-step tool loop (bash, read, final answer) is ONE turn --
    // session.status goes busy repeatedly across the three steps (the raw
    // capture has 7 busy records for this one run), but the gate is a set
    // membership check, not a counter, so that collapses to one armed
    // session, and session.idle fires once at the end. The fork never
    // receives its own session.idle in this capture (it is a separate,
    // later-forked session; its own eventual idle, whenever it happens,
    // would be counted independently for that session), so this run's
    // total is still 1.
    run2: 1,
    // run3: one failed prompt, with several real provider retries.
    // OpenCode's own processor publishes session.idle twice for this one
    // cycle (see that test below for the full trace); busy/retry fire
    // repeatedly before the first idle, but the gate is a set membership
    // check, not a counter, so the first idle consumes it once and the
    // second idle -- with nothing setting busy again in between -- is
    // dropped.
    run3: 1,
    // run4: one prompt that requests a tool the user denies permission for,
    // then finishes with an error message and a single session.idle.
    run4: 1,
    // run5: a task-tool call. The child session's own session.idle is
    // filtered entirely (it is a subagent, not the top-level agent -- see
    // the child-session tests below), so only the parent's one real idle
    // is left, and it correctly consumes the parent's own busy signal.
    run5: 1,
  };
  const byRun = loadFixture();
  for (const [run, want] of Object.entries(expected)) {
    const idleEmissions = emissionsFor(byRun.get(run), 'session.idle');
    assert.equal(idleEmissions.length, want, `${run}: agent-end count = ${idleEmissions.length}, want ${want}`);
  }
});

test('run3: session.error emits nothing by itself, and the number of agent-ends equals the number of real prompt cycles', () => {
  const run3 = loadFixture().get('run3');
  assert.ok(run3 && run3.length > 0, 'fixture must contain run3 records');

  const errorEmissions = emissionsFor(run3, 'session.error');
  assert.deepEqual(errorEmissions, [], 'session.error must never be routed to any lifecycle event');

  // The raw capture has two session.idle records for this one failed
  // prompt (OpenCode's own retry/cleanup behavior: session.error, then
  // session.idle, then one more message.updated as the assistant message
  // finalizes, then session.idle again). session.status goes busy/retry
  // several times before the first idle (this run does hit real provider
  // retries), but the gate only tracks set membership, not a count, so the
  // first idle consumes it regardless of how many times it fired. Nothing
  // sets busy again between the two idles, so the second idle finds
  // nothing to consume. Net: exactly 1 agent-end, matching the 1 real
  // prompt cycle in this run.
  const idleEmissions = emissionsFor(run3, 'session.idle');
  assert.equal(idleEmissions.length, 1, 'run3 is one real prompt cycle (a failed one), so exactly one agent-end is expected');
});

test('run3: the captured session.error is carried onto the one agent-end as a bounded error string', () => {
  const run3 = loadFixture().get('run3');
  const idleEmissions = emissionsFor(run3, 'session.idle');
  assert.equal(idleEmissions.length, 1);
  // The capture's error is {name: "APIError", data: {message: "mock upstream
  // failure", responseBody, responseHeaders, metadata.url, ...}}; only the
  // name and message are carried.
  assert.equal(idleEmissions[0].data.error, 'APIError: mock upstream failure');
  assert.deepEqual(Object.keys(idleEmissions[0].data).sort(), ['error', 'session_id']);
});

test('run2/run4/run5: turns without a session.error carry no error on their agent-end', () => {
  const byRun = loadFixture();
  for (const run of ['run2', 'run4', 'run5']) {
    for (const emission of emissionsFor(byRun.get(run), 'session.idle')) {
      assert.equal(emission.data.error, undefined, `${run}: unexpected error on agent-end`);
    }
  }
});

test('a pending error is attached once, then cleared, so the next turn ends clean', () => {
  const sessionID = 'ses_synthetic_err_clear';
  const state = createBridgeState();
  route(state, { type: 'session.created', properties: { info: { id: sessionID } } });

  route(state, { type: 'session.status', properties: { sessionID, status: { type: 'busy' } } });
  route(state, { type: 'session.error', properties: { sessionID, error: { name: 'APIError', data: { message: 'boom' } } } });
  const first = route(state, { type: 'session.idle', properties: { sessionID } });
  assert.equal(first.length, 1);
  assert.equal(first[0].data.error, 'APIError: boom');
  // OpenCode's own duplicate idle after the error: dropped, as before.
  assert.deepEqual(route(state, { type: 'session.idle', properties: { sessionID } }), []);
  assert.equal(state.pendingErrors.has(sessionID), false);

  route(state, { type: 'session.status', properties: { sessionID, status: { type: 'busy' } } });
  const second = route(state, { type: 'session.idle', properties: { sessionID } });
  assert.equal(second.length, 1);
  assert.equal(second[0].data.error, undefined, 'the next, successful turn must not inherit the previous error');
});

test('a session.error outside an armed turn, or for a child session, is not recorded', () => {
  const state = createBridgeState();
  const parentID = 'ses_synthetic_err_parent';
  const childID = 'ses_synthetic_err_child';
  route(state, { type: 'session.created', properties: { info: { id: parentID } } });
  route(state, { type: 'session.created', properties: { info: { id: childID, parentID } } });

  // Not armed: belongs to no turn.
  route(state, { type: 'session.error', properties: { sessionID: parentID, error: { name: 'APIError' } } });
  assert.equal(state.pendingErrors.has(parentID), false);
  // Child: its idle is never emitted, so its error would never be consumed.
  route(state, { type: 'session.status', properties: { sessionID: childID, status: { type: 'busy' } } });
  route(state, { type: 'session.error', properties: { sessionID: childID, error: { name: 'APIError' } } });
  assert.equal(state.pendingErrors.has(childID), false);
  // No sessionID at all (OpenCode can publish a global session.error).
  assert.deepEqual(route(state, { type: 'session.error', properties: { error: { name: 'APIError' } } }), []);
  assert.equal(state.pendingErrors.size, 0);

  // The parent's next real turn ends clean.
  route(state, { type: 'session.status', properties: { sessionID: parentID, status: { type: 'busy' } } });
  const idle = route(state, { type: 'session.idle', properties: { sessionID: parentID } });
  assert.equal(idle.length, 1);
  assert.equal(idle[0].data.error, undefined);
});

test('a user abort (MessageAbortedError) is not reported as a failed turn', () => {
  const sessionID = 'ses_synthetic_abort_err';
  const state = createBridgeState();
  route(state, { type: 'session.created', properties: { info: { id: sessionID } } });
  route(state, { type: 'session.status', properties: { sessionID, status: { type: 'busy' } } });
  route(state, { type: 'session.error', properties: { sessionID, error: { name: 'MessageAbortedError', data: { message: 'aborted' } } } });
  const idle = route(state, { type: 'session.idle', properties: { sessionID } });
  assert.equal(idle.length, 1);
  assert.equal(idle[0].data.error, undefined);
});

test('sessionErrorText reads only name and message, with fallbacks, bounded in length', () => {
  assert.equal(sessionErrorText({ name: 'APIError', data: { message: 'boom', responseBody: 'secret' } }), 'APIError: boom');
  assert.equal(sessionErrorText({ name: 'ProviderAuthError' }), 'ProviderAuthError');
  assert.equal(sessionErrorText({ data: { message: 'only a message' } }), 'only a message');
  assert.equal(sessionErrorText(undefined), 'session error');
  assert.equal(sessionErrorText({ name: 42, data: { message: {} } }), 'session error');
  const long = sessionErrorText({ name: 'APIError', data: { message: 'x'.repeat(1000) } });
  assert.equal(long.length, 256);
  assert.ok(long.startsWith('APIError: xxx'));
});

test('run4: permission.asked and permission.replied route through the event hook', () => {
  // Real capture correction (see the fixture's provenance/README): the
  // actual runtime event names are permission.asked/permission.replied, not
  // the installed @opencode-ai/sdk package's declared "permission.updated".
  const run4 = loadFixture().get('run4');
  assert.ok(run4 && run4.length > 0, 'fixture must contain run4 records');

  const asked = emissionsFor(run4, 'permission.asked');
  assert.equal(asked.length, 1);
  assert.equal(typeof asked[0].data.message, 'string');
  assert.ok(asked[0].data.message.length > 0);

  const replied = emissionsFor(run4, 'permission.replied');
  assert.equal(replied.length, 1);
});

test('tool.execute.before/after: real field shapes, args from output not input', () => {
  const run2 = loadFixture().get('run2');
  const before = run2.find((r) => r.kind === 'tool.execute.before');
  const after = run2.find((r) => r.kind === 'tool.execute.after');
  assert.ok(before && after, 'fixture must contain a tool.execute.before/after pair');

  const beforeData = toolExecuteBeforeData(before.payload.input, before.payload.output);
  assert.equal(beforeData.tool_name, before.payload.input.tool);
  assert.notEqual(beforeData.tool_input, '{}', 'tool_input must come from output.args, not be the old fixed "{}"');
  assert.deepEqual(JSON.parse(beforeData.tool_input), before.payload.output.args);

  const afterData = toolExecuteAfterData(after.payload.input);
  assert.equal(afterData.success, true);
});

test('all-zero tokens with no total still count the call but emit no token fields', () => {
  const state = createBridgeState();
  const messageID = 'msg_synthetic_zero';
  const sessionID = 'ses_synthetic_zero';
  // Synthetic (not from a capture): a step-finish whose tokens are all zero
  // and `total` is absent, which design §3.7 treats as unknown usage
  // ("All-zero tokens with total undefined mean unknown: count the call,
  // emit no tokens"). This exact shape was not observed in any real
  // capture, so it is exercised here as a labelled synthetic case.
  route(state, {
    type: 'message.updated',
    properties: { info: { id: messageID, role: 'assistant', providerID: 'p', modelID: 'm', time: { created: 1 } } },
  });
  const emissions = route(state, {
    type: 'message.part.updated',
    properties: {
      part: {
        id: 'prt_synthetic_zero', sessionID, messageID, type: 'step-finish', reason: 'stop',
        tokens: { input: 0, output: 0, reasoning: 0, cache: { read: 0, write: 0 } },
      },
    },
  });
  assert.equal(emissions.length, 1, 'the call itself is still counted');
  assert.deepEqual(Object.keys(emissions[0].data).sort(), ['model', 'session_id'].sort(), 'no token fields when usage is unknown, only session_id and the joined model');
});

test('a synthetic non-zero cache_write step-finish maps through the bridge', () => {
  // Synthetic (not from a capture): the real capture only ever has
  // cache.write=0, because an OpenAI-compatible mock model has no
  // cache-write concept. This pins the mapping path itself
  // (tokens.cache.write -> data.cache_write_tokens) at a non-zero value, so
  // a typo in either key would fail this test even though the real fixture
  // alone could not catch it.
  const state = createBridgeState();
  const messageID = 'msg_synthetic_cache_write';
  const sessionID = 'ses_synthetic_cache_write';
  route(state, {
    type: 'message.updated',
    properties: { info: { id: messageID, role: 'assistant', providerID: 'p', modelID: 'm', time: { created: 1 } } },
  });
  const emissions = route(state, {
    type: 'message.part.updated',
    properties: {
      part: {
        id: 'prt_synthetic_cache_write', sessionID, messageID, type: 'step-finish', reason: 'stop',
        tokens: { total: 107, input: 100, output: 0, reasoning: 0, cache: { read: 0, write: 7 } },
      },
    },
  });
  assert.equal(emissions.length, 1);
  assert.equal(emissions[0].data.cache_write_tokens, 7);
});

test('bridge caches (liveMessageIds, modelByMessage) evict oldest entries once MAX_CACHE_ENTRIES is exceeded', () => {
  const state = createBridgeState();
  const cap = 4096;
  for (let i = 0; i < cap + 10; i++) {
    route(state, {
      type: 'message.updated',
      properties: { info: { id: `msg_${i}`, role: 'assistant', providerID: 'p', modelID: 'm', time: { created: i } } },
    });
  }
  assert.ok(state.liveMessageIds.size <= cap, `liveMessageIds.size = ${state.liveMessageIds.size}, want <= ${cap}`);
  assert.ok(state.modelByMessage.size <= cap, `modelByMessage.size = ${state.modelByMessage.size}, want <= ${cap}`);
  // The oldest entries (msg_0, msg_1, ...) must be the ones evicted, not
  // arbitrary ones -- FIFO by insertion order.
  assert.equal(state.liveMessageIds.has('msg_0'), false);
  assert.equal(state.liveMessageIds.has(`msg_${cap + 9}`), true);
});

test('run5: a task-tool subagent child session does not signal session-start or agent-end', () => {
  const run5 = loadFixture().get('run5');
  assert.ok(run5 && run5.length > 0, 'fixture must contain run5 records (a real task-tool subagent capture)');

  const sessionCreatedRecords = run5.filter((r) => r.payload.type === 'session.created');
  assert.equal(sessionCreatedRecords.length, 2, 'expected a parent session and its task-tool child');
  const parentInfo = sessionCreatedRecords[0].payload.properties.info;
  const childInfo = sessionCreatedRecords[1].payload.properties.info;
  assert.equal(parentInfo.parentID, undefined, 'the parent session must have no parentID');
  assert.equal(childInfo.parentID, parentInfo.id, 'the child session must be parented to the real parent session (real capture, not synthetic)');

  const sessionStartEmissions = emissionsFor(run5, 'session.created');
  assert.equal(sessionStartEmissions.length, 1, 'only the parent session fires session-start; the child is filtered');
  assert.equal(sessionStartEmissions[0].data.session_id, parentInfo.id);

  const idleEmissions = emissionsFor(run5, 'session.idle');
  assert.equal(idleEmissions.length, 1, 'only the parent session fires agent-end; the child going idle must not signal it');
  assert.equal(idleEmissions[0].data.session_id, parentInfo.id);
});

test('a session.created with no parentID (e.g. a fork) still fires session-start', () => {
  // A fork has no parentID at all (confirmed by run2's fork session.created,
  // see the earlier fork tests above) -- this pins that the child-session
  // filter is keyed specifically on parentID, not on "any session that
  // isn't the very first one".
  const state = createBridgeState();
  const emissions = route(state, {
    type: 'session.created',
    properties: { info: { id: 'ses_no_parent' } },
  });
  assert.equal(emissions.length, 1);
  assert.equal(emissions[0].data.session_id, 'ses_no_parent');
});

test('session.error is unmapped for both a child session and a top-level session', () => {
  // Labelled synthetic (not from a capture): a session's turn ends exactly
  // once, on session.idle (routeSessionIdle's busy/retry gate); routing
  // session.error to any lifecycle event, for a child or a top-level
  // session, would count that same turn a second time. See dialect.yaml's
  // comment above its mappings block for the full reasoning.
  const state = createBridgeState();
  const parentID = 'ses_synthetic_parent';
  const childID = 'ses_synthetic_child';

  route(state, { type: 'session.created', properties: { info: { id: parentID } } });
  route(state, { type: 'session.created', properties: { info: { id: childID, parentID } } });

  const childErrorEmissions = route(state, {
    type: 'session.error',
    properties: { sessionID: childID, error: { data: { message: 'child provider error' } } },
  });
  assert.deepEqual(childErrorEmissions, [], 'a child session error must not be routed');

  const parentErrorEmissions = route(state, {
    type: 'session.error',
    properties: { sessionID: parentID, error: { data: { message: 'parent provider error' } } },
  });
  assert.deepEqual(parentErrorEmissions, [], 'a top-level session error must not be routed either');
});

test('session.error emits nothing even while the session is armed, with no idle in between (context-overflow auto-compaction path)', () => {
  // Labelled synthetic: OpenCode's context-overflow-with-auto-compaction
  // path publishes session.error with NO session.idle following it (the
  // run loop continues instead of stopping), unlike every other
  // session.error case this bridge has been tested against, which are all
  // followed by an idle. This is exactly the shape that would defeat a
  // mapping that routed session.error through the same gate as
  // session.idle: busy, then error (already armed, no idle to consume the
  // arm), then busy again for the continuation, then one real idle. If
  // session.error consumed or itself produced an agent-end here, the
  // total would be 2 for this one turn; it must stay 1, and the error
  // itself must produce no emission at all.
  const sessionID = 'ses_synthetic_overflow';
  const state = createBridgeState();

  route(state, { type: 'session.created', properties: { info: { id: sessionID } } });
  route(state, { type: 'session.status', properties: { sessionID, status: { type: 'busy' } } });

  const errorEmissions = route(state, {
    type: 'session.error',
    properties: { sessionID, error: { data: { message: 'context overflow' } } },
  });
  assert.deepEqual(errorEmissions, [], 'session.error must emit nothing, even while its session is already armed');

  route(state, { type: 'session.status', properties: { sessionID, status: { type: 'busy' } } });
  const idleEmissions = route(state, { type: 'session.idle', properties: { sessionID } });

  assert.equal(idleEmissions.length, 1, 'the one real idle at the end of this turn must give exactly one agent-end');
  // The run went busy again after the error (it recovered), so the turn did
  // not end on that error and its agent-end carries none.
  assert.equal(idleEmissions[0].data.error, undefined, 'a recovered error must not be attached to the eventual agent-end');
});

// These scenarios reproduce OpenCode v1.18.33's actual SessionProcessor
// event order (process/halt/cleanup, read from source), not a guessed one.
// A user abort or provider error publishes session.error, then
// session.status{idle} and session.idle once from `halt`; `cleanup` then
// rewrites every in-flight text/reasoning/tool part via
// message.part.updated (and completes the assistant message); the runner
// then publishes session.status{idle} and session.idle a second time once
// it actually finishes. Arming on message or part activity would not work
// here: cleanup's own part updates would re-arm the gate, giving 2
// agent-ends for a single aborted or errored turn. Arming only on
// session.status{busy|retry} avoids this: OpenCode publishes busy from the
// run loop and processor on each step or attempt of an active run, but
// never from halt, cleanup, SessionSummary, or an idle-only cancel.
function driveScenario(events) {
  const state = createBridgeState();
  const emissions = [];
  for (const event of events) {
    emissions.push(...route(state, event));
  }
  return emissions.filter((e) => e.name === 'session.idle').length;
}

test('a user abort while a tool call is in flight gives exactly one agent-end', () => {
  // Labelled synthetic: no captured MessageAbortedError payload exists (see
  // the fixture provenance -- run3 only proves this order for a provider
  // failure with no part in flight, not a mid-response abort).
  const sessionID = 'ses_synthetic_abort_tool';
  const messageID = 'msg_synthetic_abort_tool';
  const count = driveScenario([
    { type: 'session.created', properties: { info: { id: sessionID } } },
    { type: 'message.updated', properties: { info: { id: 'msg_u', sessionID, role: 'user', time: { created: 1 } } } },
    { type: 'session.status', properties: { sessionID, status: { type: 'busy' } } },
    { type: 'message.updated', properties: { info: { id: messageID, sessionID, role: 'assistant', time: { created: 2 } } } },
    { type: 'message.part.updated', properties: { part: { id: 'prt_tool', sessionID, messageID, type: 'tool', state: { status: 'running' } } } },
    // halt(): session.error, then status idle -> idle #1.
    { type: 'session.error', properties: { sessionID, error: { name: 'MessageAbortedError' } } },
    { type: 'session.status', properties: { sessionID, status: { type: 'idle' } } },
    { type: 'session.idle', properties: { sessionID } },
    // cleanup(): the in-flight tool part is rewritten to an error state,
    // and the assistant message is completed. Neither sets busy.
    { type: 'message.part.updated', properties: { part: { id: 'prt_tool', sessionID, messageID, type: 'tool', state: { status: 'error' } } } },
    { type: 'message.updated', properties: { info: { id: messageID, sessionID, role: 'assistant', time: { created: 2, completed: 3 } } } },
    // The runner finishes: status idle -> idle #2.
    { type: 'session.status', properties: { sessionID, status: { type: 'idle' } } },
    { type: 'session.idle', properties: { sessionID } },
  ]);
  assert.equal(count, 1, 'one aborted prompt cycle must give exactly one agent-end, not two');
});

test('a user abort while text is streaming gives exactly one agent-end', () => {
  const sessionID = 'ses_synthetic_abort_text';
  const messageID = 'msg_synthetic_abort_text';
  const count = driveScenario([
    { type: 'session.created', properties: { info: { id: sessionID } } },
    { type: 'message.updated', properties: { info: { id: 'msg_u', sessionID, role: 'user', time: { created: 1 } } } },
    { type: 'session.status', properties: { sessionID, status: { type: 'busy' } } },
    { type: 'message.updated', properties: { info: { id: messageID, sessionID, role: 'assistant', time: { created: 2 } } } },
    { type: 'message.part.updated', properties: { part: { id: 'prt_text', sessionID, messageID, type: 'text', text: 'partial' } } },
    { type: 'session.error', properties: { sessionID, error: { name: 'MessageAbortedError' } } },
    { type: 'session.status', properties: { sessionID, status: { type: 'idle' } } },
    { type: 'session.idle', properties: { sessionID } },
    // cleanup() finalizes the in-flight text part -- another part update
    // that must not re-arm the gate.
    { type: 'message.part.updated', properties: { part: { id: 'prt_text', sessionID, messageID, type: 'text', text: 'partial (finalized)' } } },
    { type: 'message.updated', properties: { info: { id: messageID, sessionID, role: 'assistant', time: { created: 2, completed: 3 } } } },
    { type: 'session.status', properties: { sessionID, status: { type: 'idle' } } },
    { type: 'session.idle', properties: { sessionID } },
  ]);
  assert.equal(count, 1, 'one aborted prompt cycle must give exactly one agent-end, not two');
});

test('a turn, then a user-message republish, then an idle-only cancel gives exactly one agent-end', () => {
  // Labelled synthetic: models OpenCode's SessionSummary republishing an
  // existing user message (via summary.diffs) after a real turn's idle,
  // followed by SessionRunState.cancel on an already-idle session (which
  // itself publishes only session.status{idle}/session.idle, no busy).
  const sessionID = 'ses_synthetic_republish';
  const messageID = 'msg_synthetic_republish_assistant';
  const count = driveScenario([
    { type: 'session.created', properties: { info: { id: sessionID } } },
    { type: 'session.status', properties: { sessionID, status: { type: 'busy' } } },
    { type: 'message.updated', properties: { info: { id: 'msg_u', sessionID, role: 'user', time: { created: 1 } } } },
    { type: 'message.updated', properties: { info: { id: messageID, sessionID, role: 'assistant', providerID: 'p', modelID: 'm', time: { created: 2 } } } },
    { type: 'message.part.updated', properties: { part: { id: 'prt_sf', sessionID, messageID, type: 'step-finish', tokens: { total: 1, input: 1, output: 1, reasoning: 0, cache: { read: 0, write: 0 } } } } },
    { type: 'message.updated', properties: { info: { id: messageID, sessionID, role: 'assistant', time: { created: 2, completed: 3 } } } },
    { type: 'session.status', properties: { sessionID, status: { type: 'idle' } } },
    { type: 'session.idle', properties: { sessionID } },
    // SessionSummary republishes the existing user message; no busy is set.
    { type: 'message.updated', properties: { info: { id: 'msg_u', sessionID, role: 'user', time: { created: 1 } } } },
    // An idle-only cancel: no busy was set since the last agent-end.
    { type: 'session.idle', properties: { sessionID } },
  ]);
  assert.equal(count, 1, 'the republish and the idle-only cancel must add nothing to the one real turn');
});

test('two consecutive prompts in one session give two agent-ends', () => {
  const sessionID = 'ses_synthetic_two_prompts';
  const count = driveScenario([
    { type: 'session.created', properties: { info: { id: sessionID } } },
    { type: 'session.status', properties: { sessionID, status: { type: 'busy' } } },
    { type: 'session.idle', properties: { sessionID } },
    { type: 'session.status', properties: { sessionID, status: { type: 'busy' } } },
    { type: 'session.idle', properties: { sessionID } },
  ]);
  assert.equal(count, 2, 'two real prompt cycles must give two agent-ends');
});

test('a retry within one run still gives exactly one agent-end', () => {
  const sessionID = 'ses_synthetic_retry';
  const count = driveScenario([
    { type: 'session.created', properties: { info: { id: sessionID } } },
    { type: 'session.status', properties: { sessionID, status: { type: 'busy' } } },
    { type: 'session.status', properties: { sessionID, status: { type: 'retry', attempt: 1, message: 'rate limited', next: 1000 } } },
    { type: 'session.status', properties: { sessionID, status: { type: 'busy' } } },
    { type: 'session.idle', properties: { sessionID } },
  ]);
  assert.equal(count, 1, 'a retried run is still one prompt cycle, so one agent-end');
});

test('a retry with no preceding busy still arms the gate on its own', () => {
  // Pins that "retry" is checked in routeSessionStatus's arm condition, not
  // just "busy": a session.idle following only a retry (no busy record at
  // all first) must still give one agent-end.
  const sessionID = 'ses_synthetic_retry_only';
  const count = driveScenario([
    { type: 'session.created', properties: { info: { id: sessionID } } },
    { type: 'session.status', properties: { sessionID, status: { type: 'retry', attempt: 1, message: 'rate limited', next: 1000 } } },
    { type: 'session.idle', properties: { sessionID } },
  ]);
  assert.equal(count, 1, 'retry alone must arm the gate, with no busy record required first');
});

test('bridge caches (seenModelEnds, childSessionIds, busySessionIds, pendingErrors) evict oldest entries once MAX_CACHE_ENTRIES is exceeded', () => {
  const cap = 4096;

  const modelEndState = createBridgeState();
  for (let i = 0; i < cap + 10; i++) {
    const sessionID = `ses_${i}`;
    const messageID = `msg_${i}`;
    route(modelEndState, {
      type: 'message.updated',
      properties: { info: { id: messageID, role: 'assistant', providerID: 'p', modelID: 'm', time: {} } },
    });
    route(modelEndState, {
      type: 'message.part.updated',
      properties: {
        part: { id: `prt_${i}`, sessionID, messageID, type: 'step-finish', reason: 'stop', tokens: { total: 1, input: 1, output: 0, reasoning: 0, cache: { read: 0, write: 0 } } },
      },
    });
  }
  assert.ok(modelEndState.seenModelEnds.size <= cap, `seenModelEnds.size = ${modelEndState.seenModelEnds.size}, want <= ${cap}`);

  const childState = createBridgeState();
  for (let i = 0; i < cap + 10; i++) {
    route(childState, {
      type: 'session.created',
      properties: { info: { id: `ses_child_${i}`, parentID: 'ses_parent' } },
    });
  }
  assert.ok(childState.childSessionIds.size <= cap, `childSessionIds.size = ${childState.childSessionIds.size}, want <= ${cap}`);
  assert.equal(childState.childSessionIds.has('ses_child_0'), false);
  assert.equal(childState.childSessionIds.has(`ses_child_${cap + 9}`), true);

  const busyState = createBridgeState();
  for (let i = 0; i < cap + 10; i++) {
    route(busyState, {
      type: 'session.status',
      properties: { sessionID: `ses_busy_${i}`, status: { type: 'busy' } },
    });
  }
  assert.ok(busyState.busySessionIds.size <= cap, `busySessionIds.size = ${busyState.busySessionIds.size}, want <= ${cap}`);
  assert.equal(busyState.busySessionIds.has('ses_busy_0'), false);
  assert.equal(busyState.busySessionIds.has(`ses_busy_${cap + 9}`), true);

  // A known child session is skipped when arming, so it never grows
  // busySessionIds at all -- its session.idle is unconditionally filtered
  // anyway (routeSessionIdle), so there would be nothing to consult.
  const childBusyState = createBridgeState();
  route(childBusyState, { type: 'session.created', properties: { info: { id: 'ses_child_busy', parentID: 'ses_parent_busy' } } });
  route(childBusyState, { type: 'session.status', properties: { sessionID: 'ses_child_busy', status: { type: 'busy' } } });
  assert.equal(childBusyState.busySessionIds.has('ses_child_busy'), false, 'a known child session must not be armed');

  // pendingErrors is bounded the same way: arm and fail many sessions
  // without any of them going idle.
  const errorState = createBridgeState();
  for (let i = 0; i < cap + 10; i++) {
    const sessionID = `ses_err_${i}`;
    route(errorState, { type: 'session.status', properties: { sessionID, status: { type: 'busy' } } });
    route(errorState, { type: 'session.error', properties: { sessionID, error: { name: 'APIError' } } });
  }
  assert.ok(errorState.pendingErrors.size <= cap, `pendingErrors.size = ${errorState.pendingErrors.size}, want <= ${cap}`);
  assert.equal(errorState.pendingErrors.has('ses_err_0'), false);
  assert.equal(errorState.pendingErrors.has(`ses_err_${cap + 9}`), true);
});

test('run5: child-session step-finish usage is still counted (not silently dropped)', () => {
  const run5 = loadFixture().get('run5');
  const stepFinishes = emissionsFor(run5, 'message.part.updated.step-finish');
  assert.equal(stepFinishes.length, 4, 'expected 2 model-ends from the parent and 2 from the child');

  const sessionCreatedRecords = run5.filter((r) => r.payload.type === 'session.created');
  const parentID = sessionCreatedRecords[0].payload.properties.info.id;
  const childID = sessionCreatedRecords[1].payload.properties.info.id;

  const byInputTokens = (a, b) => a.data.input_tokens - b.data.input_tokens;
  const childEmissions = stepFinishes.filter((e) => e.data.session_id === childID).sort(byInputTokens);
  const parentEmissions = stepFinishes.filter((e) => e.data.session_id === parentID).sort(byInputTokens);
  assert.equal(childEmissions.length, 2, 'expected 2 model-ends from the child session');
  assert.equal(parentEmissions.length, 2, 'expected 2 model-ends from the parent session');

  // Real captured values (the task mock's scripted usage): 200/10 then 250/8,
  // for both the child (the subagent's own two steps) and the parent (its
  // own two steps, driving the task tool then finishing).
  assert.deepEqual(
    childEmissions.map((e) => [e.data.input_tokens, e.data.output_tokens]),
    [[200, 10], [250, 8]]
  );
  assert.deepEqual(
    parentEmissions.map((e) => [e.data.input_tokens, e.data.output_tokens]),
    [[200, 10], [250, 8]]
  );
});
