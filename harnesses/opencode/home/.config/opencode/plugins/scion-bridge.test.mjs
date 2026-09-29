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

import { createBridgeState, route, toolExecuteBeforeData, toolExecuteAfterData } from './scion-bridge.js';

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

test('run3: a real session.error is routed with a usable error string', () => {
  const run3 = loadFixture().get('run3');
  assert.ok(run3 && run3.length > 0, 'fixture must contain run3 records');
  const errorEmissions = emissionsFor(run3, 'session.error');
  assert.equal(errorEmissions.length, 1);
  assert.equal(errorEmissions[0].data.reason, 'error');
  assert.match(errorEmissions[0].data.error, /mock upstream failure/);
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

test('a child session error is excluded, but a top-level session error still emits', () => {
  // Labelled synthetic (not from a capture): pins that session.error is
  // filtered for a task-tool child session the same way session.created and
  // session.idle are, independent of dialect.yaml's own remapping of
  // session.error away from session-end.
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
  assert.equal(parentErrorEmissions.length, 1, 'a top-level session error must still be routed');
  assert.match(parentErrorEmissions[0].data.error, /parent provider error/);
});

test('bridge caches (seenModelEnds, childSessionIds) evict oldest entries once MAX_CACHE_ENTRIES is exceeded', () => {
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
