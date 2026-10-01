#!/usr/bin/env node
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
 * large-project-bench.mjs -- reproducible browser benchmark for the
 * project grid/list/graph views at large agent counts.
 *
 * Third stage of the perf/2393-large-project-bench harness
 * (ptone/scion#2393, #2374, #2367), after perf/bench/seed and
 * perf/bench/apibench.
 *
 * A standalone Node script (not a `playwright test` spec) so it can be
 * pointed at any already-running hub+seed combination and produce a single
 * JSON report, following the existing convention in
 * web/test-scripts/realtime-lifecycle-test.js. It lives under web/ (rather
 * than perf/bench/) specifically so `@playwright/test` resolves from
 * web/node_modules without a second install -- see the harness README for
 * why. Pure/testable logic lives in lib.mjs (see lib.test.mjs, run with
 * `node --test e2e-perf/lib.test.mjs`).
 *
 * Session auth reuses the hub's --enable-test-login endpoint the same way
 * web/e2e/harness/auth.ts does (find-or-create-by-email, so it logs in as
 * the exact non-admin project-member perf/bench/seed already created and
 * bound a project-member role to, rather than a fresh user).
 *
 * Usage:
 *   cd web && node e2e-perf/large-project-bench.mjs \
 *     --hub http://127.0.0.1:18080 \
 *     --seed /tmp/scion-bench/seed-25.json \
 *     --out /tmp/scion-bench/browser-25.json \
 *     --runs 5
 *
 * Requires the hub to be started with --enable-web --enable-test-login
 * --web-assets-dir <built web/dist/client>, against the same --db and
 * --session-secret perf/bench/seed used (see the harness README).
 *
 * bench-rev-2 fixes (see gs://scion-xproject-exchange/slow-list/reviews/
 * bench-rev-2.md) on top of bench-rev-1's:
 * - R1: the network matcher now matches on parsed pathname so it actually
 *   fires for the standalone graph's unscoped `/api/v1/agents` fetch (it
 *   previously required a `projectId=` query param that page never sends).
 * - R2: consecutive burst runs target a rotation offset by run index, and
 *   each run's restore is polled to confirm it actually reached the DOM
 *   before the next run starts -- a prior run's stale, not-yet-restored
 *   badge could otherwise be miscounted as the next run's settle.
 * - NB1: restores both phase and activity, not phase alone.
 * - NB2: settle time is measured per agent from that agent's own POST
 *   completion, not from a shared burst-start timestamp (which folded in
 *   POST latency as if it were SSE latency).
 * - NB3: the first run of each scenario uses a fresh ("cold") browser
 *   context; subsequent runs share one ("warm") context. Reported
 *   separately.
 * - NB4: the report records the harness's git commit and effective
 *   timeouts.
 * - NB5: network failures/non-2xx responses record when they happened
 *   (elapsed ms from navigation start), so WriteTimeout attribution is
 *   measured, not inferred from wall-clock totals alone.
 * - NB6: a 2xx response that never renders is now `loaded-not-rendered`,
 *   distinct from `still-loading` (no response observed at all).
 * - Graph interaction timing (pan/zoom/hover) is now measured for both
 *   graph scenarios, not deferred.
 */

import { chromium } from '@playwright/test';
import * as fs from 'node:fs';
import { execFileSync } from 'node:child_process';
import * as path from 'node:path';
import { fileURLToPath } from 'node:url';

import {
  apiMatcherFor,
  classifyOutcome,
  median,
  minMax,
  stddev,
  summarizeLongTasks,
  summarizeScenario,
  generateTestLoginToken,
  pickBurstTarget,
} from './lib.mjs';

// ---- CLI args --------------------------------------------------------

function parseArgs(argv) {
  const out = {};
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (!a.startsWith('--')) continue;
    const key = a.slice(2);
    const next = argv[i + 1];
    if (next === undefined || next.startsWith('--')) {
      out[key] = true;
    } else {
      out[key] = next;
      i++;
    }
  }
  return out;
}

const args = parseArgs(process.argv.slice(2));

function required(name) {
  if (!args[name]) {
    console.error(`missing required --${name}`);
    process.exit(2);
  }
  return args[name];
}

const hubBase = (args.hub || 'http://127.0.0.1:18080').replace(/\/$/, '');
const seedPath = required('seed');
const outPath = required('out');
const runs = parseInt(args.runs || '5', 10);
const navTimeoutMs = parseInt(args['nav-timeout-ms'] || '120000', 10);
const populateTimeoutMs = parseInt(args['populate-timeout-ms'] || '120000', 10);
const burstCount = parseInt(args['burst-count'] || '15', 10);
const burstRuns = parseInt(args['burst-runs'] || String(runs), 10);
const settleTimeoutMs = parseInt(args['settle-timeout-ms'] || '30000', 10);
const notes = args.notes || '';

const seed = JSON.parse(fs.readFileSync(seedPath, 'utf8'));

// bench-rev-2 NB4: record enough provenance that a report can be matched
// back to the exact harness version and settings that produced it, without
// relying on wall-clock proximity to a commit (which bench-rev-2 had to ask
// about directly for the round-1 data).
const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
function gitHeadSha() {
  try {
    return execFileSync('git', ['rev-parse', 'HEAD'], { cwd: repoRoot, encoding: 'utf8' }).trim();
  } catch {
    return null;
  }
}

// Incremental output (bench-rev-1 N5): written after every scenario and
// every burst run, so a Chromium crash mid-benchmark loses at most the
// in-flight run, not the whole report.
function writeReportSoFar(report) {
  fs.writeFileSync(outPath, JSON.stringify(report, null, 2));
}

async function createSessionCookies(baseURL, secret, email, role, displayName) {
  const challenge = generateTestLoginToken(secret);
  const res = await fetch(`${baseURL}/api/v1/auth/test-login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${challenge}` },
    body: JSON.stringify({ email, role, displayName }),
  });
  if (!res.ok) {
    throw new Error(`test-login failed (${res.status}): ${await res.text()}`);
  }
  const data = await res.json();

  const url = new URL(baseURL);
  const setCookieHeaders =
    typeof res.headers.getSetCookie === 'function'
      ? res.headers.getSetCookie()
      : (res.headers.get('set-cookie') || '').split(/,(?=\s*\w+=)/);

  const cookies = [];
  for (const header of setCookieHeaders) {
    if (!header.trim()) continue;
    const [nameValue, ...attrs] = header.split(';').map((s) => s.trim());
    const eq = nameValue.indexOf('=');
    if (eq === -1) continue;
    const cookie = {
      name: nameValue.slice(0, eq),
      value: nameValue.slice(eq + 1),
      domain: url.hostname,
      path: '/',
    };
    for (const attr of attrs) {
      const [k, v] = attr.split('=');
      switch (k.toLowerCase()) {
        case 'path':
          cookie.path = v;
          break;
        case 'domain':
          cookie.domain = v;
          break;
        case 'secure':
          cookie.secure = true;
          break;
        case 'samesite':
          cookie.sameSite = v === 'Strict' ? 'Strict' : v === 'Lax' ? 'Lax' : 'None';
          break;
      }
    }
    // The hub only sets Secure cookies over TLS; this harness runs the hub
    // over plain http:// locally, so a Secure attribute (if present) would
    // make Playwright silently refuse to send it back.
    if (cookie.secure && url.protocol === 'http:') cookie.secure = false;
    if (!cookie.sameSite) cookie.sameSite = 'Lax';
    cookies.push(cookie);
  }
  return { cookies, user: data.user };
}

let sessionCookies = null;

async function newCookiedContext(browser) {
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
  await context.addCookies(sessionCookies);
  return context;
}

// ---- page helpers -------------------------------------------------------

async function countAllDeep(page) {
  return page.evaluate(() => {
    function walk(root) {
      let n = 0;
      const all = root.querySelectorAll('*');
      n += all.length;
      for (const el of all) {
        if (el.shadowRoot) n += walk(el.shadowRoot);
      }
      return n;
    }
    return walk(document);
  });
}

async function countSelectorDeep(page, selector) {
  return page.evaluate((sel) => {
    function walk(root) {
      let n = root.querySelectorAll(sel).length;
      for (const el of root.querySelectorAll('*')) {
        if (el.shadowRoot) n += walk(el.shadowRoot);
      }
      return n;
    }
    return walk(document);
  }, selector);
}

// bench-rev-2 NB6/R1-adjacent: returns bounding boxes (in viewport
// coordinates) for up to `limit` matched elements, piercing shadow roots,
// so the graph-interaction step below can hover/drag over real node
// positions instead of guessing coordinates.
async function elementRectsDeep(page, selector, limit) {
  return page.evaluate(
    ({ sel, lim }) => {
      const rects = [];
      function walk(root) {
        for (const el of root.querySelectorAll(sel)) {
          if (rects.length >= lim) return;
          const r = el.getBoundingClientRect();
          if (r.width > 0 && r.height > 0) {
            rects.push({ x: r.x + r.width / 2, y: r.y + r.height / 2 });
          }
        }
        for (const el of root.querySelectorAll('*')) {
          if (rects.length >= lim) return;
          if (el.shadowRoot) walk(el.shadowRoot);
        }
      }
      walk(document);
      return rects;
    },
    { sel: selector, lim: limit }
  );
}

async function waitForCount(page, selector, expected, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  let last = 0;
  while (Date.now() < deadline) {
    last = await countSelectorDeep(page, selector);
    if (last >= expected) return { count: last, timedOut: false };
    await page.waitForTimeout(200);
  }
  return { count: last, timedOut: true };
}

function attachNetworkWatch(page, matches, navStart) {
  const state = { status: null, failed: null, url: null, atMs: null };
  const onResponse = (resp) => {
    if (matches(resp.url())) {
      state.status = resp.status();
      state.url = resp.url();
      state.atMs = Date.now() - navStart;
    }
  };
  const onRequestFailed = (req) => {
    if (matches(req.url())) {
      state.failed = req.failure()?.errorText || 'unknown';
      state.url = req.url();
      state.atMs = Date.now() - navStart;
    }
  };
  page.on('response', onResponse);
  page.on('requestfailed', onRequestFailed);
  return {
    state,
    detach() {
      page.off('response', onResponse);
      page.off('requestfailed', onRequestFailed);
    },
  };
}

// ---- scenario definitions ------------------------------------------------

// The web app's project-detail route reads the path segment verbatim and
// passes it straight to `GET /api/v1/projects/{id}` (pkg/hub/
// handlers_projects_core.go's getProject), which does a raw-ID store lookup
// with no slug resolution. The URL must carry the project UUID.
const projectPath = `/projects/${encodeURIComponent(seed.projectId)}`;
const scenarios = [
  {
    key: 'project-grid',
    url: projectPath,
    viewMode: 'grid',
    selector: '.agent-card',
    isGraph: false,
  },
  {
    key: 'project-list',
    url: projectPath,
    viewMode: 'list',
    selector: '.agent-table-container tbody tr',
    isGraph: false,
  },
  {
    key: 'project-graph-embedded',
    url: projectPath,
    viewMode: 'graph',
    selector: '.node-wrapper',
    isGraph: true,
  },
  {
    key: 'standalone-graph',
    // bench-rev-2 R1 (related, non-blocking): this view fetches the
    // *unscoped* `/api/v1/agents` (web/src/components/pages/
    // agent-graph.ts:115), not a project-filtered request -- the `project`
    // query param only drives client-side filtering (`:135`). The URL
    // itself is correct (it is what a user would navigate to); it is
    // apiMatcherFor (lib.mjs) that had to change to match the real,
    // unscoped request this page sends.
    url: `/agents/graph?project=${encodeURIComponent(seed.projectId)}`,
    viewMode: null,
    selector: '.node-wrapper',
    isGraph: true,
  },
];

// performGraphInteraction drives a short pan/zoom/hover sequence against a
// populated graph view and measures the long-task cost specifically
// attributable to it (delta against a snapshot taken immediately before).
// Previously listed in the README as a #2393 acceptance gap "not attempted
// here for lack of time" -- no source change is needed, so there was no
// genuine constraint; implemented now.
async function performGraphInteraction(page) {
  const before = await page.evaluate(() => (window.__benchLongTasks || []).length);
  const rects = await elementRectsDeep(page, '.node-wrapper', 5);

  const interactionStart = Date.now();

  // Hover across up to 5 nodes.
  for (const r of rects) {
    await page.mouse.move(r.x, r.y);
    await page.waitForTimeout(50);
  }

  // Zoom: wheel over the canvas center.
  const box = rects[0] || { x: 720, y: 500 };
  await page.mouse.move(box.x, box.y);
  await page.mouse.wheel(0, -200); // zoom in
  await page.waitForTimeout(100);
  await page.mouse.wheel(0, 200); // zoom back out
  await page.waitForTimeout(100);

  // Pan: drag from one point to another.
  await page.mouse.move(box.x, box.y);
  await page.mouse.down();
  await page.mouse.move(box.x + 80, box.y + 40, { steps: 10 });
  await page.mouse.move(box.x, box.y, { steps: 10 });
  await page.mouse.up();

  // Let any triggered re-render/long tasks finish.
  await page.waitForTimeout(300);

  const interactionMs = Date.now() - interactionStart;
  const allLongTasks = await page.evaluate(() => window.__benchLongTasks || []);
  const deltaLongTasks = allLongTasks.slice(before);

  return {
    nodesInteracted: rects.length,
    interactionMs,
    longTasks: summarizeLongTasks(deltaLongTasks),
  };
}

async function runOneScenarioAttempt(page, scenario, expectedCount, runIndex, cold) {
  const apiMatches = apiMatcherFor(scenario.key, seed.projectId);
  const consoleErrors = [];
  page.on('console', (msg) => {
    if (msg.type() === 'error') consoleErrors.push(msg.text());
  });

  await page.addInitScript(() => {
    window.__benchLongTasks = [];
    try {
      const po = new PerformanceObserver((list) => {
        for (const e of list.getEntries()) {
          window.__benchLongTasks.push({ startTime: e.startTime, duration: e.duration });
        }
      });
      po.observe({ type: 'longtask', buffered: true });
    } catch {
      // longtask not supported in this engine build.
    }
  });
  if (scenario.viewMode) {
    const vm = scenario.viewMode;
    await page.addInitScript((viewMode) => {
      try {
        localStorage.setItem('scion-view-project-agents', viewMode);
      } catch {
        // localStorage unavailable pre-navigation in some engine states.
      }
    }, vm);
  }

  const navStart = Date.now();
  const netWatch = attachNetworkWatch(page, apiMatches, navStart);
  let navError = null;
  try {
    await page.goto(hubBase + scenario.url, {
      waitUntil: 'domcontentloaded',
      timeout: navTimeoutMs,
    });
  } catch (err) {
    navError = String(err);
  }

  let populated = { count: 0, timedOut: true };
  let elapsedMs = Date.now() - navStart;
  if (!navError) {
    populated = await waitForCount(page, scenario.selector, expectedCount, populateTimeoutMs);
    elapsedMs = Date.now() - navStart;
  }

  const outcome = navError
    ? `load-failed(nav:${navError})`
    : classifyOutcome(!populated.timedOut, netWatch.state);
  const isPopulated = outcome === 'populated';

  const domCount = navError ? null : await countAllDeep(page);
  const longTasks = navError ? [] : await page.evaluate(() => window.__benchLongTasks || []);

  let graphInteraction = null;
  if (isPopulated && scenario.isGraph) {
    graphInteraction = await performGraphInteraction(page);
  }

  netWatch.detach();

  return {
    run: runIndex,
    cold,
    outcome,
    // navToPopulatedMs is set ONLY for a genuinely populated run: a
    // load-failed/loaded-not-rendered/still-loading run's elapsed
    // wall-clock time is not a rendering duration.
    navToPopulatedMs: isPopulated ? elapsedMs : null,
    elapsedMs,
    populatedCount: populated.count,
    expectedCount,
    domElementCount: domCount,
    longTasks: summarizeLongTasks(longTasks),
    graphInteraction,
    networkStatus: netWatch.state.status,
    networkFailed: netWatch.state.failed,
    // bench-rev-2 NB5: when the load-bearing request's outcome was
    // observed, in elapsed ms from navigation start -- so a WriteTimeout
    // attribution is a measurement, not an inference from the run's total
    // wall-clock time.
    networkObservedAtMs: netWatch.state.atMs,
    consoleErrorCount: consoleErrors.length,
    consoleErrorsSample: consoleErrors.slice(0, 5),
  };
}

// bench-rev-2 NB3: run 0 uses a fresh ("cold") context; runs 1..N-1 share
// one ("warm") context. The README's previous reason for deferring this
// ("not done here for time") was not a real constraint -- it is a few
// lines, implemented here.
async function runScenario(browser, scenario, expectedCount) {
  const runsOut = [];

  const coldContext = await newCookiedContext(browser);
  try {
    const coldPage = await coldContext.newPage();
    runsOut.push(await runOneScenarioAttempt(coldPage, scenario, expectedCount, 0, true));
    await coldPage.close();
  } finally {
    await coldContext.close();
  }

  if (runs > 1) {
    const warmContext = await newCookiedContext(browser);
    try {
      for (let i = 1; i < runs; i++) {
        const page = await warmContext.newPage();
        runsOut.push(await runOneScenarioAttempt(page, scenario, expectedCount, i, false));
        await page.close();
      }
    } finally {
      await warmContext.close();
    }
  }

  return runsOut;
}

// ---- live-update (SSE burst) responsiveness ------------------------------

async function getStatusBadgeLabelDeep(page, agentId) {
  return page.evaluate((id) => {
    function walk(root) {
      const link = root.querySelector(`a[href="/agents/${id}"]`);
      if (link) {
        const card = link.closest('.agent-card');
        const badge = card ? card.querySelector('scion-status-badge') : null;
        if (badge) return badge.getAttribute('label');
      }
      for (const el of root.querySelectorAll('*')) {
        if (el.shadowRoot) {
          const found = walk(el.shadowRoot);
          if (found !== null) return found;
        }
      }
      return null;
    }
    return walk(document);
  }, agentId);
}

// postAgentStatus POSTs a status update (phase and/or activity) and returns
// whether it was accepted.
async function postAgentStatus(id, body) {
  const res = await fetch(`${hubBase}/api/v1/agents/${id}/status`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${seed.ownerToken}` },
    body: JSON.stringify(body),
  });
  return { ok: res.ok, status: res.status };
}

async function getAgent(id) {
  const res = await fetch(`${hubBase}/api/v1/agents/${id}`, {
    headers: { Authorization: `Bearer ${seed.ownerToken}` },
  });
  if (!res.ok) return null;
  return res.json();
}

async function waitForBadgeValue(page, id, expectedValue, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const label = await getStatusBadgeLabelDeep(page, id);
    if (label && label.toLowerCase() === expectedValue.toLowerCase()) {
      return { settled: true, atMs: Date.now() };
    }
    await page.waitForTimeout(100);
  }
  return { settled: false, atMs: null };
}

async function runBurstOnce(page, runIndex) {
  // Over-fetch beyond burstCount so there is room to skip already-suspended
  // agents without running out of candidates.
  const listResp = await fetch(
    `${hubBase}/api/v1/projects/${seed.projectId}/agents?limit=${burstCount * 3}`,
    { headers: { Authorization: `Bearer ${seed.ownerToken}` } }
  );
  if (!listResp.ok) {
    throw new Error(`fetching agents for burst failed: ${listResp.status}`);
  }
  const listData = await listResp.json();
  const candidates = (listData.agents || []).filter((a) => a.phase !== 'suspended');
  const targets = candidates.slice(0, burstCount);
  if (targets.length < burstCount) {
    console.warn(`  warning: only ${targets.length}/${burstCount} non-suspended agents available`);
  }

  // bench-rev-2 NB1: capture activity too, so the restore below can put
  // agents back exactly as they were, not just their phase.
  const preBurst = new Map(
    targets.map((a) => [a.id, { phase: a.phase, activity: a.activity || '' }])
  );
  // bench-rev-2 R2: offset the rotation by runIndex so consecutive runs
  // never request the same target phase for the same agent -- see
  // pickBurstTarget's doc comment in lib.mjs for why that matters.
  const targetPhase = new Map(
    targets.map((a, idx) => [a.id, pickBurstTarget(idx, runIndex, preBurst.get(a.id).phase)])
  );

  const postStartedAt = Date.now();
  const postResults = await Promise.all(
    targets.map(async (a) => {
      const r = await postAgentStatus(a.id, { phase: targetPhase.get(a.id) });
      return { id: a.id, ...r, postCompletedAtMs: Date.now() - postStartedAt };
    })
  );
  const burstSentMs = Date.now() - postStartedAt;

  const rejected = postResults.filter((r) => !r.ok);
  const accepted = postResults.filter((r) => r.ok);
  const postCompletedAt = new Map(accepted.map((r) => [r.id, postStartedAt + r.postCompletedAtMs]));

  // Poll every accepted agent's badge until it shows its target status in
  // the DOM (ground truth for "the live update reached the DOM"), or until
  // settleTimeoutMs elapses. bench-rev-2 NB2: record each agent's OWN
  // settle delta relative to ITS OWN POST completion, not a shared
  // burst-start timestamp (which folds in POST latency as if it were SSE
  // latency).
  const deadline = Date.now() + settleTimeoutMs;
  const settledAtByAgent = new Map();
  const acceptedIds = accepted.map((r) => r.id);
  for (;;) {
    for (const id of acceptedIds) {
      if (settledAtByAgent.has(id)) continue;
      const label = await getStatusBadgeLabelDeep(page, id);
      if (label && label.toLowerCase() === targetPhase.get(id).toLowerCase()) {
        settledAtByAgent.set(id, Date.now());
      }
    }
    if (settledAtByAgent.size === acceptedIds.length) break;
    if (Date.now() > deadline) break;
    await page.waitForTimeout(100);
  }
  const settledCount = settledAtByAgent.size;
  const perAgentSettleMs = acceptedIds
    .filter((id) => settledAtByAgent.has(id))
    .map((id) => settledAtByAgent.get(id) - postCompletedAt.get(id));
  const settleMM = minMax(perAgentSettleMs);

  // Distinguish "the server never applied the update" from "the UI did not
  // settle": GET each targeted agent's server-side phase independently of
  // the DOM-badge check above.
  const serverAppliedCount = (
    await Promise.all(
      acceptedIds.map(async (id) => (await getAgent(id))?.phase === targetPhase.get(id))
    )
  ).filter(Boolean).length;

  // Restore every targeted agent to its pre-burst phase AND activity
  // (bench-rev-2 NB1), then -- bench-rev-2 R2 -- WAIT for the restore to be
  // confirmed in the DOM before returning, so the *next* run never starts
  // against a stale, not-yet-restored badge. This doubles as a second,
  // independent SSE-delivery measurement.
  const restoreStartedAt = Date.now();
  const restoreResults = await Promise.all(
    targets.map(async (a) => ({ id: a.id, ...(await postAgentStatus(a.id, preBurst.get(a.id))) }))
  );
  const restoreFailures = restoreResults.filter((r) => !r.ok);
  if (restoreFailures.length > 0) {
    console.warn(`  warning: failed to restore ${restoreFailures.length}/${targets.length} agents`);
  }
  const restoreAcceptedIds = restoreResults.filter((r) => r.ok).map((r) => r.id);
  const restoreSettled = await Promise.all(
    restoreAcceptedIds.map(async (id) => {
      const expected = preBurst.get(id).phase;
      const result = await waitForBadgeValue(page, id, expected, settleTimeoutMs);
      return { id, ...result };
    })
  );
  const restoreSettledCount = restoreSettled.filter((r) => r.settled).length;
  const restoreSettleMs = restoreSettled
    .filter((r) => r.settled)
    .map((r) => r.atMs - restoreStartedAt);

  return {
    requestedCount: targets.length,
    acceptedCount: acceptedIds.length,
    rejectedCount: rejected.length,
    rejectedSample: rejected.slice(0, 3),
    burstSentMs,
    settledCount,
    serverAppliedCount,
    timedOut: settledCount < acceptedIds.length,
    // bench-rev-2 NB2: per-agent settle time (this agent's badge update
    // minus this agent's own POST completion), not a shared burst-start
    // delta. medianSettleMs/minSettleMs/maxSettleMs below describe THIS
    // run's distribution across its own settled agents.
    medianSettleMs: median(perAgentSettleMs),
    minSettleMs: settleMM.min,
    maxSettleMs: settleMM.max,
    restoreFailureCount: restoreFailures.length,
    restoreSettledCount,
    restoreTotalCount: restoreAcceptedIds.length,
    medianRestoreSettleMs: median(restoreSettleMs),
  };
}

async function runBurstScenario(browser) {
  const context = await newCookiedContext(browser);
  const page = await context.newPage();
  await page.addInitScript(() => {
    localStorage.setItem('scion-view-project-agents', 'grid');
  });
  const navStart = Date.now();
  const apiMatches = apiMatcherFor('project-grid', seed.projectId);
  const netWatch = attachNetworkWatch(page, apiMatches, navStart);
  await page.goto(hubBase + projectPath, { waitUntil: 'domcontentloaded', timeout: navTimeoutMs });
  const populated = await waitForCount(page, '.agent-card', seed.agentCount, populateTimeoutMs);
  netWatch.detach();

  if (populated.timedOut) {
    const outcome = classifyOutcome(false, netWatch.state);
    await context.close();
    return {
      skipped: true,
      skipReason: `grid did not populate before firing the burst (${outcome})`,
      runsAttempted: 0,
      results: [],
    };
  }

  const results = [];
  for (let i = 0; i < burstRuns; i++) {
    const r = await runBurstOnce(page, i);
    results.push(r);
    console.log(
      `  burst run ${i}: ${r.acceptedCount}/${r.requestedCount} accepted, ` +
        `${r.settledCount}/${r.acceptedCount} settled (median ${r.medianSettleMs}ms), ` +
        `${r.serverAppliedCount}/${r.acceptedCount} server-applied, ` +
        `restore ${r.restoreSettledCount}/${r.restoreTotalCount} confirmed (timedOut=${r.timedOut})`
    );
  }
  await context.close();

  const medianSettleValues = results.map((r) => r.medianSettleMs).filter((v) => v != null);
  const mm = minMax(medianSettleValues);
  return {
    skipped: false,
    runsAttempted: results.length,
    results,
    medianSettleMs: median(medianSettleValues),
    minSettleMs: mm.min,
    maxSettleMs: mm.max,
    stddevSettleMs: stddev(medianSettleValues),
    fullySettledRunCount: results.filter((r) => !r.timedOut).length,
    fullyRestoredRunCount: results.filter((r) => r.restoreSettledCount === r.restoreTotalCount)
      .length,
  };
}

// ---- main -----------------------------------------------------------------

async function main() {
  const { cookies } = await createSessionCookies(
    hubBase,
    seed.sessionSecret,
    seed.memberEmail,
    'member',
    'Bench Member'
  );
  sessionCookies = cookies;

  const browser = await chromium.launch({
    headless: true,
    args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'],
  });

  const report = {
    generatedAt: new Date().toISOString(),
    harnessCommit: gitHeadSha(),
    hubBaseUrl: hubBase,
    seedProjectId: seed.projectId,
    seedProjectSlug: seed.projectSlug,
    agentCount: seed.agentCount,
    runs,
    burstRuns,
    effectiveTimeouts: { navTimeoutMs, populateTimeoutMs, settleTimeoutMs, burstCount },
    notes,
    scenarios: {},
  };
  writeReportSoFar(report);

  try {
    for (const scenario of scenarios) {
      console.log(`running scenario ${scenario.key} (${runs} runs: 1 cold + ${runs - 1} warm)...`);
      const results = await runScenario(browser, scenario, seed.agentCount);
      report.scenarios[scenario.key] = summarizeScenario(scenario, results);
      const s = report.scenarios[scenario.key];
      console.log(
        `  ${s.successCount}/${results.length} populated; outcomes=${JSON.stringify(s.outcomeCounts)}; ` +
          `median nav->populated: ${s.medianNavToPopulatedMs}ms (cold=${s.medianNavToPopulatedMsCold}ms, warm=${s.medianNavToPopulatedMsWarm}ms); ` +
          `median DOM count: ${s.medianDomElementCount} [${s.minDomElementCount}, ${s.maxDomElementCount}]`
      );
      writeReportSoFar(report);
    }

    console.log(`running SSE burst-update responsiveness scenario (${burstRuns} runs)...`);
    report.liveUpdateBurst = await runBurstScenario(browser);
    if (report.liveUpdateBurst.skipped) {
      console.log(`  skipped: ${report.liveUpdateBurst.skipReason}`);
    } else {
      console.log(
        `  median settle time: ${report.liveUpdateBurst.medianSettleMs}ms ` +
          `[${report.liveUpdateBurst.minSettleMs}, ${report.liveUpdateBurst.maxSettleMs}]; ` +
          `${report.liveUpdateBurst.fullySettledRunCount}/${report.liveUpdateBurst.runsAttempted} runs fully settled, ` +
          `${report.liveUpdateBurst.fullyRestoredRunCount}/${report.liveUpdateBurst.runsAttempted} fully restored`
      );
    }
    writeReportSoFar(report);
  } finally {
    await browser.close();
  }

  console.log(`report written to ${outPath}`);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
