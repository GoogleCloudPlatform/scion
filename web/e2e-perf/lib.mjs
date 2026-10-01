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
 * bench-rev-2 R1's root cause (the standalone-graph network matcher never
 * firing) is exactly the kind of bug a two-line unit test against the real
 * URLs each page fetches would have caught; these functions did not exist
 * as independently-callable, testable units until this split.
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

// ---- network-outcome classification (bench-rev-1 B2, bench-rev-2 R1/NB6) --
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
 * bench-rev-2 R1: the standalone-graph page
 * (web/src/components/pages/agent-graph.ts:115) calls
 * `apiFetch('/api/v1/agents')` with NO query string at all -- it fetches
 * every agent and filters client-side -- so a matcher requiring
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
 * bench-rev-2 NB6: a 2xx response that nonetheless never reaches the
 * expected rendered count is a *render* failure, not a *load* failure --
 * distinct from "still-loading" (no response observed at all within the
 * timeout, i.e. genuinely still in flight or the matcher never fired).
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
 * summarizeScenario computes bench-rev-1 B3's required spread stats --
 * median/min/max/stddev, not just a median -- over SUCCESSFUL ("populated")
 * runs only, and reports success/failure counts and an outcome tally
 * separately so a reader can see at a glance whether a scenario's numbers
 * are "5/5 populated" or "1/5 populated, 4 load-failed".
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
    // bench-rev-2 NB3: cold (first, fresh-context) vs warm (subsequent,
    // shared-context) runs reported separately, since a cold run's
    // navToPopulatedMs is measurably slower and averaging it into one
    // median without saying so is misleading.
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
// bench-rev-1 B1: deliberately excludes "suspended" as a *target* --
// updateAgentStatus's Guard 0 (pkg/hub/handlers_agent_lifecycle.go) silently
// drops any phase/activity update sent to an agent that is *currently*
// suspended, and still returns 200. Agents that are already suspended are
// also skipped as *sources* by the caller, for the same reason.
//
// Also excludes "running": web/src/shared/types.ts's getAgentDisplayStatus()
// renders a running agent's *activity* instead of the literal phase string
// whenever activity is non-empty, so "running" would only be unambiguous
// for agents that happen to have no activity set.
export const BURST_TARGET_ROTATION = ['stopped', 'error', 'stopping'];

/**
 * pickBurstTarget chooses a target phase for the agent at position `idx`
 * (0-based, stable across runs for a given database) on burst run number
 * `runIndex` (0-based), guaranteed to differ from `currentPhase`.
 *
 * bench-rev-2 R2: an earlier version used `idx % N` only, so every run
 * picked the *same* target for a given agent. Combined with not waiting for
 * the inter-run restore to reach the DOM, run i+1's first poll could see
 * run i's stale (not-yet-restored) badge value, which equals run i+1's own
 * target under the same idx-only rotation -- a false "settled" before
 * run i+1's own SSE update could possibly have arrived. Mixing `runIndex`
 * into the rotation offset means consecutive runs targeting the same agent
 * never request the same phase twice in a row, so a stale badge can never
 * match the new target by coincidence. This still does not make waiting
 * for the restore unnecessary -- see waitForBadgeValue in
 * large-project-bench.mjs -- it is a second, independent guard.
 */
export function pickBurstTarget(idx, runIndex, currentPhase) {
  const n = BURST_TARGET_ROTATION.length;
  let phase = BURST_TARGET_ROTATION[(idx + runIndex) % n];
  if (phase.toLowerCase() === (currentPhase || '').toLowerCase()) {
    phase = BURST_TARGET_ROTATION[(idx + runIndex + 1) % n];
  }
  return phase;
}
