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
  resolveBatchTickSettled,
  expectedFirstPageCount,
  pageCountFor,
  summarizePageChanges,
  walkEndReason,
  summarizeBurstScenario,
  BURST_TARGET_ROTATION,
} from './lib.mjs';

// ---- apiMatcherFor: a regression test against the real page URLs ----------
// Match against the REAL URLs each page actually fetches, not an assumed
// shape -- this is exactly the kind of test that catches a matcher drifting
// out of sync with what a page actually requests.

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

test('classifyOutcome: 2xx observed but not populated -> loaded-not-rendered', () => {
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

// ---- pickBurstTarget ---------------------------------------------------------
//
// The real caller (runBurstOnce in large-project-bench.mjs) always passes
// the agent's PRE-BURST phase as `currentPhase` -- the SAME value on every
// run, since it is restored between runs -- never the previous run's own
// target. A test that instead fed the previous *target* back in as
// `currentPhase` would follow a model the real caller never does, and
// would pass even against a simpler idx-only rotation that does not
// actually prevent repeats. The tests below hold `currentPhase` fixed
// across runs, as the real caller does, and track `previousRunTarget` as
// its own, separate argument -- the only shape that actually exercises the
// repeat-prevention logic the real caller depends on.

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

// A "mutation check" test that runs a hand-copied `idxOnlyPickBurstTarget`
// function against ITSELF and asserts that copy repeats would never call
// the real, exported `pickBurstTarget`, so it would be tautological: it
// could never fail because of anything this file's actual code did. The
// real regression protection is the test above ("holding currentPhase
// fixed...") -- confirmed by temporarily reverting the real
// `pickBurstTarget` to a simpler idx-only form and replaying a known
// false-settle sequence through the real `runBurstOnce`/`runBurstScenario`:
// the test above fails against both an idx-only rotation and the original
// implementation it replaced. This suite intentionally does not carry a
// test that asserts something about a copy of the code instead of the
// code.

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

// ---- displayStatusLabel -------------------------------------------------------
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

// ---- computePreStaleIds -------------------------------------------------------
// Extracted from large-project-bench.mjs's runBurstOnce so the pre-fire
// staleness guard has its own direct test, rather than only being
// exercised indirectly against a fake hub and DOM.

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

// ---- resolveBatchTickSettled ------------------------------------------------
// Extracted from large-project-bench.mjs's shared burst-settle poller, which
// reads every currently-pending agent's badge in one page.evaluate call per
// tick instead of one independent poll loop per agent; this is the pure
// match-and-select step run against that one batched read.

test('resolveBatchTickSettled returns only ids whose label matches their target', () => {
  const pending = new Map([
    ['a1', 'error'],
    ['a2', 'stopped'],
  ]);
  const labels = new Map([
    ['a1', 'error'],
    ['a2', 'running'],
  ]);
  assert.deepEqual(resolveBatchTickSettled(pending, labels), ['a1']);
});

test('resolveBatchTickSettled is case-insensitive', () => {
  const pending = new Map([['a1', 'error']]);
  const labels = new Map([['a1', 'ERROR']]);
  assert.deepEqual(resolveBatchTickSettled(pending, labels), ['a1']);
});

test('resolveBatchTickSettled treats a missing or null label as not settled', () => {
  const pending = new Map([
    ['a1', 'error'],
    ['a2', 'stopped'],
  ]);
  const labels = new Map([
    ['a1', null],
    // a2 absent entirely, e.g. not yet found by the DOM walk this tick.
  ]);
  assert.deepEqual(resolveBatchTickSettled(pending, labels), []);
});

test('resolveBatchTickSettled ignores labels for ids not in pending', () => {
  const pending = new Map([['a1', 'error']]);
  const labels = new Map([
    ['a1', 'error'],
    ['a2', 'error'], // already resolved/removed from pending by the caller
  ]);
  assert.deepEqual(resolveBatchTickSettled(pending, labels), ['a1']);
});

test('resolveBatchTickSettled returns an empty list when nothing in the batch settled', () => {
  const pending = new Map([
    ['a1', 'error'],
    ['a2', 'stopped'],
  ]);
  const labels = new Map([
    ['a1', 'running'],
    ['a2', 'running'],
  ]);
  assert.deepEqual(resolveBatchTickSettled(pending, labels), []);
});

// ---- summarizeBurstScenario -----------------------------------------------
// Extracted from runBurstScenario's aggregation so the invalid-run
// exclusion -- the guard that stops a known false-settle sequence from
// contaminating the scenario's statistics -- has its own direct test.

function makeRun({
  invalid = false,
  medianSettleMs = null,
  minSettleMs = null,
  maxSettleMs = null,
  timedOut = false,
  restoreFullyConfirmed = true,
  preStaleExcludedCount = 0,
  // Default to a representative non-zero tracked count (a full 15-agent
  // burst with nothing pre-stale-excluded) so existing tests that don't
  // care about trackedCount aren't silently affected by the
  // `trackedCount > 0` guard on fullySettledRunCount.
  trackedCount = 15,
} = {}) {
  return {
    invalid,
    medianSettleMs,
    minSettleMs,
    maxSettleMs,
    timedOut,
    restoreFullyConfirmed,
    preStaleExcludedCount,
    trackedCount,
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
  assert.equal(s.perAgentMinSettleMs, 50); // true per-agent min across valid runs only
  assert.equal(s.perAgentMaxSettleMs, 220); // true per-agent max across valid runs only
});

test('summarizeBurstScenario: perAgentMin/MaxSettleMs are the true per-agent range; runMedianMin/MaxMs are the separate range-of-medians statistic', () => {
  // A field computing minMax(medianSettleValues) would, for these three
  // runs, give [100, 110] (the range of the medians) -- very different
  // from the true per-agent range, which must reach down to each run's own
  // min (10) and up to each run's own max (900). Both statistics are
  // available, under names that each only ever mean one thing:
  // perAgentMin/MaxSettleMs for the true per-agent range, runMedianMin/
  // MaxMs for the range of the five per-run medians.
  const results = [
    makeRun({ medianSettleMs: 100, minSettleMs: 10, maxSettleMs: 300 }),
    makeRun({ medianSettleMs: 105, minSettleMs: 20, maxSettleMs: 900 }),
    makeRun({ medianSettleMs: 110, minSettleMs: 15, maxSettleMs: 250 }),
  ];
  const s = summarizeBurstScenario(results);
  assert.equal(s.perAgentMinSettleMs, 10);
  assert.equal(s.perAgentMaxSettleMs, 900);
  assert.equal(s.runMedianMinMs, 100);
  assert.equal(s.runMedianMaxMs, 110);
  assert.notEqual(s.perAgentMinSettleMs, s.runMedianMinMs);
  assert.notEqual(s.perAgentMaxSettleMs, s.runMedianMaxMs);
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

test('summarizeBurstScenario: a run with every target pre-stale-excluded (trackedCount 0) does not count as fully settled', () => {
  // timedOut is `settledCount < trackedCount`, which is vacuously false
  // when trackedCount is 0 -- a run that tracked and confirmed NOTHING
  // must not be indistinguishable from a run that tracked and confirmed
  // all 15 agents.
  const nothingTracked = makeRun({ timedOut: false, trackedCount: 0, preStaleExcludedCount: 15 });
  const normalRun = makeRun({ timedOut: false, trackedCount: 15 });
  const s = summarizeBurstScenario([nothingTracked, normalRun]);
  assert.equal(s.validRunCount, 2);
  assert.equal(s.fullySettledRunCount, 1); // only normalRun, not nothingTracked
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
  assert.equal(s.perAgentMinSettleMs, null);
  assert.equal(s.perAgentMaxSettleMs, null);
  assert.equal(s.runMedianMinMs, null);
  assert.equal(s.runMedianMaxMs, null);
});

test('expectedFirstPageCount: one full page, or every agent when fewer', () => {
  assert.equal(expectedFirstPageCount(25, 100, 100), 25);
  assert.equal(expectedFirstPageCount(25, 10, 10), 10);
  assert.equal(expectedFirstPageCount(50, 500, 500), 50);
  // The pager total wins over the seeded count when known.
  assert.equal(expectedFirstPageCount(25, 100, 12), 12);
  // Unknown total falls back to the seeded count.
  assert.equal(expectedFirstPageCount(25, 100, null), 25);
  assert.equal(expectedFirstPageCount(25, 7, undefined), 7);
  // No agents: nothing to render.
  assert.equal(expectedFirstPageCount(25, 0, 0), 0);
});

test('expectedFirstPageCount: no pager (no page size) expects every agent', () => {
  assert.equal(expectedFirstPageCount(null, 100, null), 100);
  assert.equal(expectedFirstPageCount(0, 100, 100), 100);
  assert.equal(expectedFirstPageCount(undefined, 25, 25), 25);
});

test('pageCountFor', () => {
  assert.equal(pageCountFor(25, 100), 4);
  assert.equal(pageCountFor(25, 101), 5);
  assert.equal(pageCountFor(25, 25), 1);
  assert.equal(pageCountFor(25, 0), 1);
  assert.equal(pageCountFor(null, 100), null);
  assert.equal(pageCountFor(25, null), null);
});

test('summarizePageChanges: completed changes of populated runs only', () => {
  const results = [
    {
      outcome: 'populated',
      pageSize: 25,
      pageCount: 4,
      pageChanges: [
        { toPageIndex: 1, ok: true, ms: 100 },
        { toPageIndex: 2, ok: true, ms: 300 },
        { toPageIndex: 3, ok: false, ms: null },
      ],
    },
    {
      outcome: 'populated',
      pageSize: 25,
      pageCount: 4,
      pageChanges: [{ toPageIndex: 1, ok: true, ms: 200 }],
    },
    // A run that never populated contributes no page changes.
    { outcome: 'loaded-not-rendered', pageSize: 25, pageCount: 4, pageChanges: [] },
  ];
  const s = summarizePageChanges(results);
  assert.equal(s.pageSize, 25);
  assert.equal(s.pageCount, 4);
  assert.equal(s.pageChangeAttemptCount, 4);
  assert.equal(s.pageChangeSuccessCount, 3);
  assert.equal(s.pageChangeFailureCount, 1);
  assert.equal(s.medianPageChangeMs, 200);
  assert.equal(s.minPageChangeMs, 100);
  assert.equal(s.maxPageChangeMs, 300);
});

test('summarizePageChanges: a single page, no pager, or mixed page sizes', () => {
  const single = summarizePageChanges([
    { outcome: 'populated', pageSize: 25, pageCount: 1, pageChanges: [] },
  ]);
  assert.equal(single.pageChangeAttemptCount, 0);
  assert.equal(single.medianPageChangeMs, null);
  assert.equal(single.pageCount, 1);

  const none = summarizePageChanges([{ outcome: 'populated', pageSize: null, pageCount: null }]);
  assert.equal(none.pageSize, null);
  assert.equal(none.pageCount, null);

  const mixed = summarizePageChanges([
    { outcome: 'populated', pageSize: 25, pageCount: 4, pageChanges: [] },
    { outcome: 'populated', pageSize: 50, pageCount: 2, pageChanges: [] },
  ]);
  assert.deepEqual(mixed.pageSize, [25, 50]);
  assert.deepEqual(mixed.pageCount, [4, 2]);
});

test('summarizePageChanges: counts walks that stopped before the last page', () => {
  const results = [
    {
      outcome: 'populated',
      pageSize: 25,
      pageCount: 4,
      pageChangesStopReason: 'completed',
      pageChanges: [
        { toPageIndex: 1, ok: true, ms: 100 },
        { toPageIndex: 2, ok: true, ms: 100 },
        { toPageIndex: 3, ok: true, ms: 100 },
      ],
    },
    {
      outcome: 'populated',
      pageSize: 25,
      pageCount: 4,
      pageChangesStopReason: 'next-unavailable-before-last-page',
      pageChanges: [
        { toPageIndex: 1, ok: true, ms: 100 },
        { toPageIndex: 2, ok: true, ms: 100 },
      ],
    },
    // A legitimately short view ends with no-next-page and is not counted.
    {
      outcome: 'populated',
      pageSize: 25,
      pageCount: 1,
      pageChangesStopReason: 'no-next-page',
      pageChanges: [],
    },
  ];
  const s = summarizePageChanges(results);
  assert.equal(s.pageWalkEarlyStopCount, 1);
  assert.equal(s.pageChangeAttemptCount, 5);
  assert.equal(s.pageChangeFailureCount, 0);
  assert.equal(summarizePageChanges([]).pageWalkEarlyStopCount, 0);
});

test('walkEndReason: how a walk with Next disabled ended', () => {
  const pager = (o) => ({ pageSize: 25, pageIndex: 0, total: 100, hasNext: false, ...o });
  // Last page of 4: the normal end.
  assert.equal(walkEndReason(pager({ pageIndex: 3 })), 'no-next-page');
  // Page index 2 of 4 with Next disabled: an early stop.
  assert.equal(walkEndReason(pager({ pageIndex: 2 })), 'next-unavailable-before-last-page');
  // Unknown or capped total: no later page can be shown to exist.
  assert.equal(walkEndReason(pager({ pageIndex: 1, total: null })), 'no-next-page');
  assert.equal(
    walkEndReason(pager({ pageIndex: 1, total: { loaded: 2000, capped: true } })),
    'no-next-page'
  );
  // Total 0.
  assert.equal(walkEndReason(pager({ total: 0 })), 'no-next-page');
  // A one-page view.
  assert.equal(walkEndReason(pager({ total: 20 })), 'no-next-page');
  // Next still available, or no pager: not an end.
  assert.equal(walkEndReason(pager({ hasNext: true })), null);
  assert.equal(walkEndReason(null), null);
});
