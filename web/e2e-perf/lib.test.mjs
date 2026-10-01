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

// Run with: node --test e2e-perf/lib.test.mjs  (from web/)

import { test } from 'node:test';
import assert from 'node:assert/strict';

import {
  apiMatcherFor,
  classifyOutcome,
  median,
  minMax,
  stddev,
  summarizeScenario,
  pickBurstTarget,
  BURST_TARGET_ROTATION,
} from './lib.mjs';

// ---- apiMatcherFor: the bench-rev-2 R1 regression test ---------------------
// Match against the REAL URLs each page actually fetches, not an assumed
// shape -- this is exactly the test that would have caught R1 before it
// shipped.

test('apiMatcherFor(standalone-graph) matches agent-graph.ts real unscoped fetch', () => {
  const matches = apiMatcherFor('standalone-graph', 'proj-123');
  // web/src/components/pages/agent-graph.ts:115 -- apiFetch('/api/v1/agents'),
  // no query string at all.
  assert.equal(matches('http://127.0.0.1:18080/api/v1/agents'), true);
});

test('apiMatcherFor(standalone-graph) also matches if a query string is present', () => {
  const matches = apiMatcherFor('standalone-graph', 'proj-123');
  assert.equal(matches('http://127.0.0.1:18080/api/v1/agents?limit=500'), true);
});

test('apiMatcherFor(standalone-graph) does not match the project-scoped endpoint', () => {
  const matches = apiMatcherFor('standalone-graph', 'proj-123');
  assert.equal(matches('http://127.0.0.1:18080/api/v1/projects/proj-123/agents'), false);
});

test('apiMatcherFor(project-grid) matches the project-scoped agents fetch', () => {
  const matches = apiMatcherFor('project-grid', 'proj-123');
  assert.equal(matches('http://127.0.0.1:18080/api/v1/projects/proj-123/agents'), true);
});

test('apiMatcherFor(project-grid) does not match a different project id', () => {
  const matches = apiMatcherFor('project-grid', 'proj-123');
  assert.equal(matches('http://127.0.0.1:18080/api/v1/projects/other-project/agents'), false);
});

test('apiMatcherFor(project-grid) does not match the unscoped global endpoint', () => {
  const matches = apiMatcherFor('project-grid', 'proj-123');
  assert.equal(matches('http://127.0.0.1:18080/api/v1/agents'), false);
});

test('apiMatcherFor never throws on an unparsable URL', () => {
  const matches = apiMatcherFor('standalone-graph', 'proj-123');
  assert.equal(matches('not a url'), false);
});

// ---- classifyOutcome --------------------------------------------------------

test('classifyOutcome: populated wins regardless of network state', () => {
  assert.equal(classifyOutcome(true, { status: null, failed: null }), 'populated');
});

test('classifyOutcome: network failure -> load-failed(network:...)', () => {
  const got = classifyOutcome(false, { status: null, failed: 'net::ERR_EMPTY_RESPONSE' });
  assert.equal(got, 'load-failed(network:net::ERR_EMPTY_RESPONSE)');
});

test('classifyOutcome: non-2xx status -> load-failed(http:...)', () => {
  assert.equal(classifyOutcome(false, { status: 500, failed: null }), 'load-failed(http:500)');
  assert.equal(classifyOutcome(false, { status: 403, failed: null }), 'load-failed(http:403)');
});

test('classifyOutcome: 2xx observed but not populated -> loaded-not-rendered (NB6)', () => {
  assert.equal(classifyOutcome(false, { status: 200, failed: null }), 'loaded-not-rendered');
});

test('classifyOutcome: nothing observed at all -> still-loading', () => {
  assert.equal(classifyOutcome(false, { status: null, failed: null }), 'still-loading');
});

// ---- stats ------------------------------------------------------------------

test('median: empty/single/odd/even/unsorted', () => {
  assert.equal(median([]), null);
  assert.equal(median([5]), 5);
  assert.equal(median([3, 1, 2]), 2);
  assert.equal(median([1, 2, 3, 4]), 2.5);
  assert.equal(median([5, 1, 5, 1, 3]), 3);
});

test('median does not mutate its input', () => {
  const xs = [3, 1, 2];
  median(xs);
  assert.deepEqual(xs, [3, 1, 2]);
});

test('minMax: empty/single/unsorted', () => {
  assert.deepEqual(minMax([]), { min: null, max: null });
  assert.deepEqual(minMax([7]), { min: 7, max: 7 });
  assert.deepEqual(minMax([3, 1, 4, 1, 5, 9, 2, 6]), { min: 1, max: 9 });
});

test('stddev: empty/single/two-equal/textbook', () => {
  assert.equal(stddev([]), 0);
  assert.equal(stddev([5]), 0);
  assert.equal(stddev([3, 3]), 0);
  assert.ok(Math.abs(stddev([2, 4, 4, 4, 5, 5, 7, 9]) - 2.138089935299394) < 1e-9);
});

// ---- summarizeScenario -------------------------------------------------------

test('summarizeScenario excludes non-populated runs from median/min/max and splits cold/warm', () => {
  const results = [
    {
      outcome: 'populated',
      navToPopulatedMs: 1000,
      domElementCount: 10,
      longTasks: { totalMs: 5 },
      cold: true,
    },
    {
      outcome: 'populated',
      navToPopulatedMs: 2000,
      domElementCount: 20,
      longTasks: { totalMs: 10 },
      cold: false,
    },
    {
      outcome: 'load-failed(network:x)',
      navToPopulatedMs: null,
      domElementCount: null,
      longTasks: { totalMs: 0 },
      cold: false,
    },
    {
      outcome: 'still-loading',
      navToPopulatedMs: null,
      domElementCount: null,
      longTasks: { totalMs: 0 },
      cold: false,
    },
  ];
  const s = summarizeScenario({ selector: '.x' }, results);
  assert.equal(s.successCount, 2);
  assert.equal(s.failureCount, 2);
  assert.equal(s.coldRunCount, 1);
  assert.equal(s.warmRunCount, 3);
  assert.equal(s.medianNavToPopulatedMsCold, 1000);
  assert.equal(s.medianNavToPopulatedMsWarm, 2000);
  assert.equal(s.medianNavToPopulatedMs, 1500);
  assert.deepEqual(s.outcomeCounts, {
    populated: 2,
    'load-failed(network:x)': 1,
    'still-loading': 1,
  });
});

// ---- pickBurstTarget (bench-rev-2 R2) ----------------------------------------

test('pickBurstTarget never returns the current phase', () => {
  for (let idx = 0; idx < 20; idx++) {
    for (let run = 0; run < 10; run++) {
      for (const current of BURST_TARGET_ROTATION) {
        const picked = pickBurstTarget(idx, run, current);
        assert.notEqual(picked.toLowerCase(), current.toLowerCase());
      }
    }
  }
});

test('pickBurstTarget never repeats the same phase for the same agent on consecutive runs', () => {
  // Simulate: agent at a fixed idx, restored to whatever the previous run
  // targeted, then targeted again next run. The R2 bug was exactly this
  // sequence producing the same phase twice in a row.
  const idx = 3;
  let phase = 'running'; // arbitrary pre-burst seed phase
  for (let run = 0; run < 10; run++) {
    const next = pickBurstTarget(idx, run, phase);
    assert.notEqual(next.toLowerCase(), phase.toLowerCase());
    phase = next; // simulate: this run's target becomes "current" for the next
  }
});

test('pickBurstTarget only ever returns values from BURST_TARGET_ROTATION', () => {
  for (let idx = 0; idx < 5; idx++) {
    for (let run = 0; run < 5; run++) {
      const picked = pickBurstTarget(idx, run, 'running');
      assert.ok(BURST_TARGET_ROTATION.map((p) => p.toLowerCase()).includes(picked.toLowerCase()));
    }
  }
});

test('BURST_TARGET_ROTATION never includes suspended or running', () => {
  const lower = BURST_TARGET_ROTATION.map((p) => p.toLowerCase());
  assert.equal(lower.includes('suspended'), false);
  assert.equal(lower.includes('running'), false);
});
