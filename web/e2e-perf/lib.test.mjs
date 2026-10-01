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
  displayStatusLabel,
  computePreStaleIds,
  summarizeBurstScenario,
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

// ---- pickBurstTarget (bench-rev-2 R2, bench-rev-3 RR1) -----------------------
//
// bench-rev-3 RR1(c): the real caller (runBurstOnce in large-project-bench.mjs)
// always passes the agent's PRE-BURST phase as `currentPhase` -- the SAME
// value on every run, since it is restored between runs -- never the
// previous run's own target. bench-rev-2's original test fed the previous
// *target* back in as `currentPhase`, a model the real caller never
// follows, so reverting pickBurstTarget to the old idx-only rotation still
// passed it (21/21). The tests below hold `currentPhase` fixed across runs,
// as the real caller does, and track `previousRunTarget` as its own,
// separate argument -- the only shape that can actually catch RR1(a).

test('pickBurstTarget never returns the current phase', () => {
  for (let idx = 0; idx < 20; idx++) {
    for (let run = 0; run < 10; run++) {
      for (const current of BURST_TARGET_ROTATION) {
        const picked = pickBurstTarget(idx, run, current, null);
        assert.notEqual(picked.toLowerCase(), current.toLowerCase());
      }
    }
  }
});

test('pickBurstTarget: holding currentPhase fixed (as the real caller does), consecutive runs never repeat the same target for the same agent', () => {
  for (let idx = 0; idx < 20; idx++) {
    for (const currentPhase of [...BURST_TARGET_ROTATION, 'running']) {
      let previousRunTarget = null;
      for (let run = 0; run < 10; run++) {
        const picked = pickBurstTarget(idx, run, currentPhase, previousRunTarget);
        assert.notEqual(picked.toLowerCase(), currentPhase.toLowerCase());
        if (previousRunTarget !== null) {
          assert.notEqual(
            picked.toLowerCase(),
            previousRunTarget.toLowerCase(),
            `idx=${idx} run=${run} currentPhase=${currentPhase}: repeated ${picked} from the previous run`
          );
        }
        previousRunTarget = picked;
      }
    }
  }
});

// bench-rev-4 N3: an earlier version of this file had a "mutation check"
// test here that ran a hand-copied `idxOnlyPickBurstTarget` function
// against ITSELF and asserted that copy repeats -- it never called the
// real, exported `pickBurstTarget`, so it was tautological: it could never
// fail because of anything this file's actual code did. The real
// regression protection is the test above ("holding currentPhase fixed...")
// -- bench-rev-3 confirmed by temporarily reverting the real
// `pickBurstTarget` to the bench-rev-2 idx-only form and bench-rev-4
// independently reconfirmed it by replaying the exact round-3 false-settle
// sequence through the real `runBurstOnce`/`runBurstScenario`: the test
// above fails against both the bench-rev-2 and the original idx-only
// implementations. That mutation check lives in review evidence, not here,
// specifically so this suite does not carry a test that asserts something
// about a copy of the code instead of the code.

test('pickBurstTarget still returns a valid candidate when currentPhase equals previousRunTarget', () => {
  for (let idx = 0; idx < 10; idx++) {
    for (let run = 0; run < 10; run++) {
      const picked = pickBurstTarget(idx, run, 'stopped', 'stopped');
      assert.ok(BURST_TARGET_ROTATION.map((p) => p.toLowerCase()).includes(picked.toLowerCase()));
    }
  }
});

test('pickBurstTarget only ever returns values from BURST_TARGET_ROTATION', () => {
  for (let idx = 0; idx < 5; idx++) {
    for (let run = 0; run < 5; run++) {
      const picked = pickBurstTarget(idx, run, 'running', null);
      assert.ok(BURST_TARGET_ROTATION.map((p) => p.toLowerCase()).includes(picked.toLowerCase()));
    }
  }
});

test('BURST_TARGET_ROTATION never includes suspended or running', () => {
  const lower = BURST_TARGET_ROTATION.map((p) => p.toLowerCase());
  assert.equal(lower.includes('suspended'), false);
  assert.equal(lower.includes('running'), false);
});

// ---- displayStatusLabel (bench-rev-3 RR1(b)) ---------------------------------
// Mirrors web/src/shared/types.ts's getAgentDisplayStatus exactly -- the
// restore-wait check must use this, not the literal phase, to have any
// chance of matching a running-with-activity agent's rendered badge.

test('displayStatusLabel returns activity for a running agent with non-empty activity', () => {
  assert.equal(displayStatusLabel('running', 'compiling'), 'compiling');
});

test('displayStatusLabel returns phase when running with empty activity', () => {
  assert.equal(displayStatusLabel('running', ''), 'running');
});

test('displayStatusLabel returns the literal phase for any non-running phase, regardless of activity', () => {
  assert.equal(displayStatusLabel('stopped', 'leftover-activity'), 'stopped');
  assert.equal(displayStatusLabel('error', ''), 'error');
});

// ---- computePreStaleIds (bench-rev-3 RR1 guard 2, bench-rev-4 N12) -----------
// Extracted from large-project-bench.mjs's runBurstOnce so the pre-fire
// staleness guard has its own direct test, not only review-time simulation
// against a fake hub and DOM.

test('computePreStaleIds excludes an agent whose badge already shows its target', () => {
  const preFireLabels = new Map([
    ['a1', 'error'],
    ['a2', 'stopped'],
  ]);
  const targetPhase = new Map([
    ['a1', 'error'], // already showing the target -- pre-stale
    ['a2', 'stopping'], // showing something else -- fine
  ]);
  const result = computePreStaleIds(['a1', 'a2'], preFireLabels, targetPhase);
  assert.deepEqual([...result], ['a1']);
});

test('computePreStaleIds is case-insensitive', () => {
  const preFireLabels = new Map([['a1', 'ERROR']]);
  const targetPhase = new Map([['a1', 'error']]);
  const result = computePreStaleIds(['a1'], preFireLabels, targetPhase);
  assert.deepEqual([...result], ['a1']);
});

test('computePreStaleIds treats a missing or null badge label as not stale', () => {
  const preFireLabels = new Map([['a1', null]]);
  const targetPhase = new Map([['a1', 'error']]);
  const result = computePreStaleIds(['a1'], preFireLabels, targetPhase);
  assert.equal(result.size, 0);
});

test('computePreStaleIds returns an empty set when nothing is stale', () => {
  const preFireLabels = new Map([
    ['a1', 'stopped'],
    ['a2', 'running'],
  ]);
  const targetPhase = new Map([
    ['a1', 'error'],
    ['a2', 'stopping'],
  ]);
  const result = computePreStaleIds(['a1', 'a2'], preFireLabels, targetPhase);
  assert.equal(result.size, 0);
});

// ---- summarizeBurstScenario (bench-rev-3 RR1(b), bench-rev-4 R4/N12) ---------
// Extracted from runBurstScenario's aggregation so the invalid-run
// exclusion -- the guard that actually stopped bench-rev-3's round-3
// false-settle sequence in bench-rev-4's replay -- has its own direct test.

function makeRun({
  invalid = false,
  medianSettleMs = null,
  minSettleMs = null,
  maxSettleMs = null,
  timedOut = false,
  restoreFullyConfirmed = true,
  preStaleExcludedCount = 0,
} = {}) {
  return {
    invalid,
    medianSettleMs,
    minSettleMs,
    maxSettleMs,
    timedOut,
    restoreFullyConfirmed,
    preStaleExcludedCount,
  };
}

test('summarizeBurstScenario excludes invalid runs from medianSettleMs/min/max/stddev', () => {
  const results = [
    makeRun({ medianSettleMs: 100, minSettleMs: 50, maxSettleMs: 150 }),
    // This run is invalid: even though its own numbers are wildly
    // different, they must not affect the scenario's statistics at all.
    makeRun({ invalid: true, medianSettleMs: 99999, minSettleMs: 99999, maxSettleMs: 99999 }),
    makeRun({ medianSettleMs: 200, minSettleMs: 80, maxSettleMs: 220 }),
  ];
  const s = summarizeBurstScenario(results);
  assert.equal(s.runsAttempted, 3);
  assert.equal(s.validRunCount, 2);
  assert.equal(s.invalidRunCount, 1);
  assert.equal(s.medianOfRunMediansN, 2);
  assert.equal(s.medianSettleMs, 150); // median of [100, 200], NOT influenced by 99999
  assert.equal(s.minSettleMs, 50); // true per-agent min across valid runs only
  assert.equal(s.maxSettleMs, 220); // true per-agent max across valid runs only
});

test('summarizeBurstScenario: minSettleMs/maxSettleMs are the true per-agent range, not the range of per-run medians', () => {
  // bench-rev-4 R4: an earlier version computed minMax(medianSettleValues),
  // which for these three runs would give [100, 110] (the range of the
  // medians) -- very different from the true per-agent range, which must
  // reach down to each run's own min (10) and up to each run's own max (900).
  const results = [
    makeRun({ medianSettleMs: 100, minSettleMs: 10, maxSettleMs: 300 }),
    makeRun({ medianSettleMs: 105, minSettleMs: 20, maxSettleMs: 900 }),
    makeRun({ medianSettleMs: 110, minSettleMs: 15, maxSettleMs: 250 }),
  ];
  const s = summarizeBurstScenario(results);
  assert.equal(s.minSettleMs, 10);
  assert.equal(s.maxSettleMs, 900);
  assert.notEqual(s.minSettleMs, 100); // not the range-of-medians bug
  assert.notEqual(s.maxSettleMs, 110);
});

test('summarizeBurstScenario: fullySettledRunCount only counts valid, non-timed-out runs', () => {
  const results = [
    makeRun({ timedOut: false }),
    makeRun({ timedOut: true }),
    makeRun({ invalid: true, timedOut: false }), // invalid: must not count even though not timed out
  ];
  const s = summarizeBurstScenario(results);
  assert.equal(s.fullySettledRunCount, 1);
  assert.equal(s.validRunCount, 2);
});

test('summarizeBurstScenario: fullyRestoredRunCount is counted over ALL runs, including invalid ones', () => {
  // restoreFullyConfirmed describes THAT run's own restore, independent of
  // whether a later run was invalidated because of it -- unlike the settle
  // statistics, this is not restricted to valid runs.
  const results = [
    makeRun({ restoreFullyConfirmed: true }),
    makeRun({ invalid: true, restoreFullyConfirmed: true }),
    makeRun({ restoreFullyConfirmed: false }),
  ];
  const s = summarizeBurstScenario(results);
  assert.equal(s.fullyRestoredRunCount, 2);
});

test('summarizeBurstScenario sums preStaleExcludedTotal across all runs', () => {
  const results = [
    makeRun({ preStaleExcludedCount: 2 }),
    makeRun({ preStaleExcludedCount: 0 }),
    makeRun({ invalid: true, preStaleExcludedCount: 1 }),
  ];
  const s = summarizeBurstScenario(results);
  assert.equal(s.preStaleExcludedTotal, 3);
});

test('summarizeBurstScenario handles an all-invalid scenario without crashing', () => {
  const results = [makeRun({ invalid: true }), makeRun({ invalid: true })];
  const s = summarizeBurstScenario(results);
  assert.equal(s.validRunCount, 0);
  assert.equal(s.medianSettleMs, null);
  assert.equal(s.minSettleMs, null);
  assert.equal(s.maxSettleMs, null);
});
