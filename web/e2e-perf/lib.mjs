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

/**
 * lib.mjs -- pure, Playwright-free functions from large-project-bench.mjs,
 * split out so they can be unit-tested directly (see lib.test.mjs).
 *
 * A bug like the standalone-graph network matcher never firing is exactly
 * the kind of thing a two-line unit test against the real URLs each page
 * fetches catches; splitting these out makes them independently-callable,
 * testable units rather than only reachable through a full browser run.
 */

import * as crypto from 'node:crypto';

// ---- stats ----------------------------------------------------------------

export function median(xs) {
  if (!xs.length) return null;
  const s = [...xs].sort((a, b) => a - b);
  const mid = Math.floor(s.length / 2);
  return s.length % 2 ? s[mid] : (s[mid - 1] + s[mid]) / 2;
}

export function minMax(xs) {
  if (!xs.length) return { min: null, max: null };
  return { min: Math.min(...xs), max: Math.max(...xs) };
}

export function stddev(xs) {
  if (xs.length < 2) return 0;
  const mean = xs.reduce((a, b) => a + b, 0) / xs.length;
  const sumSq = xs.reduce((a, b) => a + (b - mean) * (b - mean), 0);
  return Math.sqrt(sumSq / (xs.length - 1));
}

export function summarizeLongTasks(entries) {
  if (!entries.length) return { count: 0, totalMs: 0, maxMs: 0 };
  let total = 0;
  let max = 0;
  for (const e of entries) {
    total += e.duration;
    if (e.duration > max) max = e.duration;
  }
  return { count: entries.length, totalMs: total, maxMs: max };
}

// ---- network-outcome classification ----------------------------------------
//
// The hub's default WriteTimeout is 60s (pkg/config/hub_config.go:851,
// pkg/hub/web.go:2913). A handler that has not started writing its response
// by then gets its connection forcibly closed: the browser's fetch sees a
// network-level failure (an empty reply), the page's data-load promise
// rejects, and the page falls to its error/empty state -- which renders
// almost no agent cards and looks, from a "did the selector count reach N"
// check alone, identical to "still loading". Watching the actual network
// outcome of the load-bearing API request is the only way to tell those
// apart.

/**
 * apiMatcherFor returns a predicate matching the specific request each
 * scenario's data load depends on, per project-detail.ts / agent-graph.ts.
 *
 * The standalone-graph page (web/src/components/pages/agent-graph.ts:115)
 * calls `apiFetch('/api/v1/agents')` with NO query string at all -- it
 * fetches every agent and filters client-side -- so a matcher requiring
 * `projectId=<id>` in the URL, as an earlier version of this function did,
 * can never fire for it. Match on the parsed pathname instead, with any or
 * no query string.
 */
export function apiMatcherFor(scenarioKey, projectId) {
  if (scenarioKey === 'standalone-graph') {
    return (url) => {
      try {
        return new URL(url).pathname === '/api/v1/agents';
      } catch {
        return false;
      }
    };
  }
  // project-grid / project-list / project-graph-embedded all load via
  // project-detail.ts's loadData(), which fetches both the project and its
  // agents in parallel; the agents call is the one whose cost scales with
  // agent count.
  const wantPath = `/api/v1/projects/${projectId}/agents`;
  return (url) => {
    try {
      return new URL(url).pathname === wantPath;
    } catch {
      return false;
    }
  };
}

/**
 * classifyOutcome turns (populated?, network state) into one of:
 * "populated" | "load-failed(<reason>)" | "loaded-not-rendered" |
 * "still-loading".
 *
 * A 2xx response that nonetheless never reaches the expected rendered
 * count is a *render* failure, not a *load* failure -- distinct from
 * "still-loading" (no response observed at all within the timeout, i.e.
 * genuinely still in flight or the matcher never fired).
 */
export function classifyOutcome(populatedOk, netState) {
  if (populatedOk) return 'populated';
  if (netState.failed) return `load-failed(network:${netState.failed})`;
  if (netState.status != null && (netState.status < 200 || netState.status >= 300)) {
    return `load-failed(http:${netState.status})`;
  }
  if (netState.status != null) return 'loaded-not-rendered';
  return 'still-loading';
}

/**
 * summarizeScenario computes spread stats -- median/min/max/stddev, not
 * just a median -- over SUCCESSFUL ("populated") runs only, and reports
 * success/failure counts and an outcome tally separately so a reader can
 * see at a glance whether a scenario's numbers are "5/5 populated" or
 * "1/5 populated, 4 load-failed".
 */
export function summarizeScenario(scenario, results) {
  const populatedRuns = results.filter((r) => r.outcome === 'populated');
  const navTimes = populatedRuns.map((r) => r.navToPopulatedMs);
  const domCounts = populatedRuns.map((r) => r.domElementCount).filter((n) => n != null);
  const longTaskTotals = populatedRuns.map((r) => r.longTasks.totalMs);

  const outcomeCounts = {};
  for (const r of results) {
    outcomeCounts[r.outcome] = (outcomeCounts[r.outcome] || 0) + 1;
  }

  const navMinMax = minMax(navTimes);
  const domMinMax = minMax(domCounts);
  const longTaskMinMax = minMax(longTaskTotals);

  const coldRuns = results.filter((r) => r.cold);
  const warmRuns = results.filter((r) => !r.cold);
  const coldPopulated = coldRuns.filter((r) => r.outcome === 'populated');
  const warmPopulated = warmRuns.filter((r) => r.outcome === 'populated');

  return {
    selector: scenario.selector,
    runs: results,
    successCount: populatedRuns.length,
    failureCount: results.length - populatedRuns.length,
    outcomeCounts,
    // Cold (first, fresh-context) vs warm (subsequent, shared-context)
    // runs reported separately, since a cold run's navToPopulatedMs is
    // measurably slower and averaging it into one median without saying
    // so is misleading.
    coldRunCount: coldRuns.length,
    warmRunCount: warmRuns.length,
    medianNavToPopulatedMsCold: median(coldPopulated.map((r) => r.navToPopulatedMs)),
    medianNavToPopulatedMsWarm: median(warmPopulated.map((r) => r.navToPopulatedMs)),
    medianNavToPopulatedMs: median(navTimes),
    minNavToPopulatedMs: navMinMax.min,
    maxNavToPopulatedMs: navMinMax.max,
    stddevNavToPopulatedMs: stddev(navTimes),
    medianDomElementCount: median(domCounts),
    minDomElementCount: domMinMax.min,
    maxDomElementCount: domMinMax.max,
    medianLongTaskTotalMs: median(longTaskTotals),
    minLongTaskTotalMs: longTaskMinMax.min,
    maxLongTaskTotalMs: longTaskMinMax.max,
  };
}

/**
 * expectedFirstPageCount is how many cards or rows the first page of a
 * paged project grid or list must render: one full page, or every agent
 * when there are fewer. `total` is the pager's total when known (a number);
 * otherwise the seeded agent count is used. A missing or non-positive page
 * size (no pager rendered) means the view is not paged, so every agent is
 * expected, as before paging existed.
 */
export function expectedFirstPageCount(pageSize, agentCount, total) {
  const all = typeof total === 'number' && total >= 0 ? total : agentCount;
  if (!(typeof pageSize === 'number' && pageSize > 0)) return all;
  return Math.min(pageSize, all);
}

/** pageCountFor is the number of pages a paged view has for `total` items. */
export function pageCountFor(pageSize, total) {
  if (!(typeof pageSize === 'number' && pageSize > 0)) return null;
  if (!(typeof total === 'number' && total >= 0)) return null;
  return Math.max(1, Math.ceil(total / pageSize));
}

/**
 * summarizePageChanges reports page-change latency (Next clicked to the
 * next page rendered) over every completed change of every populated run.
 * A change that did not complete within its timeout is counted in
 * pageChangeFailureCount and excluded from the timing stats.
 */
export function summarizePageChanges(results) {
  const changes = [];
  let attempted = 0;
  for (const r of results) {
    if (r.outcome !== 'populated' || !Array.isArray(r.pageChanges)) continue;
    for (const c of r.pageChanges) {
      attempted++;
      if (c.ok) changes.push(c.ms);
    }
  }
  const mm = minMax(changes);
  const sizes = [...new Set(results.map((r) => r.pageSize).filter((n) => n != null))];
  const counts = [...new Set(results.map((r) => r.pageCount).filter((n) => n != null))];
  return {
    pageSize: sizes.length === 1 ? sizes[0] : sizes.length === 0 ? null : sizes,
    pageCount: counts.length === 1 ? counts[0] : counts.length === 0 ? null : counts,
    pageChangeAttemptCount: attempted,
    pageChangeSuccessCount: changes.length,
    pageChangeFailureCount: attempted - changes.length,
    medianPageChangeMs: median(changes),
    minPageChangeMs: mm.min,
    maxPageChangeMs: mm.max,
    stddevPageChangeMs: stddev(changes),
  };
}

// ---- test-login session (mirrors web/e2e/harness/auth.ts) -----------------

export const USER_TOKEN_ISSUER = 'scion-hub';
export const TEST_LOGIN_AUDIENCE = 'scion-test-login';
export const USER_SIGNING_KEY_NAME = 'user_signing_key';

export function deriveSigningKey(secret, keyName) {
  return crypto.createHash('sha256').update(`scion-hub-signing-key:${keyName}:${secret}`).digest();
}

export function base64url(buf) {
  return buf.toString('base64url');
}

export function signJWT(payload, signingKey) {
  const header = base64url(Buffer.from(JSON.stringify({ alg: 'HS256', typ: 'JWT' })));
  const body = base64url(Buffer.from(JSON.stringify(payload)));
  const sig = crypto.createHmac('sha256', signingKey).update(`${header}.${body}`).digest();
  return `${header}.${body}.${base64url(sig)}`;
}

export function generateTestLoginToken(secret, subject = 'perf-bench') {
  const key = deriveSigningKey(secret, USER_SIGNING_KEY_NAME);
  const now = Math.floor(Date.now() / 1000);
  return signJWT(
    {
      iss: USER_TOKEN_ISSUER,
      sub: subject,
      aud: TEST_LOGIN_AUDIENCE,
      iat: now,
      nbf: now,
      exp: now + 300,
      jti: crypto.randomBytes(16).toString('base64url'),
    },
    key
  );
}

// ---- SSE burst target selection --------------------------------------------
//
// store.AgentStatusUpdate (pkg/store/store.go) has no top-level "status"
// field -- only "phase"/"activity"/etc. Use "phase" with a value from
// pkg/agent/state.Phase.
//
// Deliberately excludes "suspended" as a *target* -- updateAgentStatus's
// Guard 0 (pkg/hub/handlers_agent_lifecycle.go) silently drops any
// phase/activity update sent to an agent that is *currently* suspended,
// and still returns 200. Agents that are already suspended are also
// skipped as *sources* by the caller, for the same reason.
//
// Also excludes "running": web/src/shared/types.ts's getAgentDisplayStatus()
// renders a running agent's *activity* instead of the literal phase string
// whenever activity is non-empty, so "running" would only be unambiguous
// for agents that happen to have no activity set.
export const BURST_TARGET_ROTATION = ['stopped', 'error', 'stopping'];

/**
 * displayStatusLabel mirrors web/src/shared/types.ts's getAgentDisplayStatus:
 * a `running` agent with a non-empty activity displays its activity string
 * instead of its literal phase. Anything expecting to compare against what
 * the UI actually renders -- the restore-wait check below, in particular --
 * must use this, not the raw phase, or it can never match for such agents.
 *
 * Comparing the restore-wait against the literal pre-burst phase, when
 * that agent's displayed label is actually its activity, would mean the
 * check could never succeed for those agents -- not a harness bug in the
 * sense of miscounting, but it would burn the full settle timeout every
 * run waiting on a check that could not pass, since that is the wrong
 * predicate to gate on.
 */
export function displayStatusLabel(phase, activity) {
  if (phase === 'running' && activity) return activity;
  return phase;
}

/**
 * pickBurstTarget chooses a target phase for the agent at position `idx`
 * (0-based, stable across runs for a given database) on burst run number
 * `runIndex` (0-based), guaranteed to differ from BOTH `currentPhase` (the
 * agent's live/pre-burst phase) AND `previousRunTarget` (the phase this same
 * agent was targeted with on the previous run it took part in, or null on
 * its first run).
 *
 * A `runIndex`-offset rotation alone is not sufficient to guarantee
 * consecutive runs never request the same phase twice in a row for the
 * same agent: the caller always passes the *pre-burst* phase as
 * `currentPhase` (the same value on every run, since it is restored
 * between runs), not the previous run's target, so excluding only
 * `currentPhase` does not prevent `pick(idx, r)` and `pick(idx, r+1)` from
 * coinciding whenever the pre-burst phase is itself in the rotation (12 of
 * 15 agents in the 25-agent seed) -- 13 of 60 consecutive-run pairs
 * repeated in practice. Excluding both `currentPhase` and the actual
 * `previousRunTarget` closes this: with a 3-entry rotation, excluding at
 * most 2 distinct values always leaves at least one candidate.
 */
export function pickBurstTarget(idx, runIndex, currentPhase, previousRunTarget) {
  const n = BURST_TARGET_ROTATION.length;
  const exclude = new Set(
    [currentPhase, previousRunTarget].filter(Boolean).map((p) => p.toLowerCase())
  );
  for (let i = 0; i < n; i++) {
    const candidate = BURST_TARGET_ROTATION[(idx + runIndex + i) % n];
    if (!exclude.has(candidate.toLowerCase())) return candidate;
  }
  // Unreachable with a 3-entry rotation and at most 2 excluded values, but
  // fail loudly rather than silently return a colliding target.
  throw new Error(
    `pickBurstTarget: no candidate in [${BURST_TARGET_ROTATION}] excludes ` +
      `{${[...exclude]}} for idx=${idx} runIndex=${runIndex}`
  );
}

/**
 * computePreStaleIds returns the set of agent ids, among `ids`, whose badge
 * label (from `preFireLabels`, read immediately before posting) already
 * equals the phase about to be requested (from `targetPhase`). Such agents
 * cannot have a later matching poll result attributed to THIS run's own
 * POST -- it could be a stale badge left over from a restore that silently
 * failed to reach the DOM -- so the caller excludes them from that run's
 * settle tracking entirely. Extracted as a pure function so this guard is
 * directly unit-testable, rather than only existing inline in
 * large-project-bench.mjs.
 *
 * Case-insensitive, matching the comparison large-project-bench.mjs's badge
 * reads and `pickBurstTarget`'s rotation both use.
 */
export function computePreStaleIds(ids, preFireLabels, targetPhase) {
  const preStaleIds = new Set();
  for (const id of ids) {
    const label = preFireLabels.get(id);
    const target = targetPhase.get(id);
    if (label && target && label.toLowerCase() === target.toLowerCase()) {
      preStaleIds.add(id);
    }
  }
  return preStaleIds;
}

/**
 * resolveBatchTickSettled compares one tick's freshly-read badge labels
 * against each still-pending agent's target phase, for the shared
 * burst-settle poller in large-project-bench.mjs's runBurstOnce. That
 * poller reads every currently-pending agent's badge in a single
 * page.evaluate call per tick (instead of one independent poll loop per
 * agent) and uses this pure function to decide which of them just settled,
 * so the match logic -- case-insensitive, only ids present in `pending`
 * are considered -- is unit-testable without a real page.
 *
 * `pending` is a Map<id, targetPhase>; `labels` is a Map<id, label|null>
 * read in that same tick. Returns the ids (a subset of pending's keys)
 * whose label now matches their target; the caller removes them from
 * `pending` and computes their settle time against their OWN POST
 * completion timestamp, not this tick's -- batching the DOM read must not
 * change which moment a given agent's settle time is measured from.
 */
export function resolveBatchTickSettled(pending, labels) {
  const settledIds = [];
  for (const [id, target] of pending) {
    const label = labels.get(id);
    if (label && target && label.toLowerCase() === target.toLowerCase()) {
      settledIds.push(id);
    }
  }
  return settledIds;
}

/**
 * summarizeBurstScenario reduces one scenario's per-run burst results
 * (`runBurstOnce`'s return values, one per run) into the scenario-level
 * statistics large-project-bench.mjs's report publishes. Extracted as a
 * pure function so the invalid-run exclusion is directly testable with
 * synthetic run objects, no fake hub or DOM required.
 *
 * A run marked `invalid` (fired while the previous run's restore was not
 * yet confirmed in the DOM) is excluded from every statistic below except
 * `invalidRunCount` itself and `results` -- it is reported for
 * transparency, but describing it as "settled" or folding its median into
 * the scenario's would describe a run known not to have started from the
 * expected pre-burst state.
 *
 * `medianSettleMs` is the median OF THE PER-RUN MEDIANS (one sample per
 * valid run -- `n` of them, given by `validRunCount` below), NOT a median
 * over every individual agent's settle time; it is a median of medians, a
 * coarser but more outlier-resistant statistic.
 *
 * The scenario-level min/max are named `perAgentMinSettleMs`/
 * `perAgentMaxSettleMs` -- deliberately NOT `minSettleMs`/`maxSettleMs` --
 * to avoid a field name silently changing what it means: those generic
 * names could easily be reused for the range of the five per-run medians
 * (e.g. "51-227ms") in one report and the true per-agent range across all
 * valid runs (e.g. "11-373ms" for the same data) in another, which would
 * make comparing a `minSettleMs` field across two reports compare two
 * different statistics without any visible warning. `runMedianMinMs`/
 * `runMedianMaxMs` are provided alongside for the (coarser, matching
 * `medianSettleMs`'s own granularity) range-of-medians statistic, so both
 * are available under names that only ever mean one thing.
 */
export function summarizeBurstScenario(results) {
  const validResults = results.filter((r) => !r.invalid);
  const medianSettleValues = validResults.map((r) => r.medianSettleMs).filter((v) => v != null);
  const perRunMins = validResults.map((r) => r.minSettleMs).filter((v) => v != null);
  const perRunMaxes = validResults.map((r) => r.maxSettleMs).filter((v) => v != null);
  // Surfaced so a reader (and the console summary) can see "fully settled,
  // but N agents pre-stale-excluded" rather than only a
  // trackedCount < requestedCount buried inside each run's own object.
  const preStaleExcludedTotal = results.reduce((sum, r) => sum + (r.preStaleExcludedCount || 0), 0);

  return {
    runsAttempted: results.length,
    validRunCount: validResults.length,
    invalidRunCount: results.length - validResults.length,
    results,
    preStaleExcludedTotal,
    // n for medianSettleMs/stddevSettleMs: one sample per valid run.
    medianOfRunMediansN: medianSettleValues.length,
    medianSettleMs: median(medianSettleValues),
    // True per-agent range across all valid runs (the min of each run's
    // own min and the max of each run's own max).
    perAgentMinSettleMs: perRunMins.length ? Math.min(...perRunMins) : null,
    perAgentMaxSettleMs: perRunMaxes.length ? Math.max(...perRunMaxes) : null,
    // The coarser range-of-the-per-run-medians statistic, at the same
    // granularity as medianSettleMs itself.
    runMedianMinMs: medianSettleValues.length ? Math.min(...medianSettleValues) : null,
    runMedianMaxMs: medianSettleValues.length ? Math.max(...medianSettleValues) : null,
    stddevSettleMs: stddev(medianSettleValues),
    // `timedOut` is `settledCount < trackedCount`, which is vacuously false
    // when `trackedCount` is 0 (every target excluded as pre-stale) -- a
    // run with nothing to track would otherwise count as "fully settled"
    // despite confirming nothing. Require `trackedCount > 0` too, so an
    // all-pre-stale run is excluded from this count instead of silently
    // inflating it.
    fullySettledRunCount: validResults.filter((r) => !r.timedOut && r.trackedCount > 0).length,
    fullyRestoredRunCount: results.filter((r) => r.restoreFullyConfirmed).length,
  };
}
