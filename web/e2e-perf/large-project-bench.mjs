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
 * why.
 *
 * Session auth reuses the hub's --enable-test-login endpoint the same way
 * web/e2e/harness/auth.ts does (find-or-create-by-email, so it logs in as
 * the exact non-admin project-member perf/bench/seed already created and
 * bound a project-member role to, rather than a fresh user). The JWT/cookie
 * logic is duplicated here in ~30 lines rather than imported from
 * e2e/harness/auth.ts, which hardcodes a fixed e2e-only session secret --
 * this script needs to use whatever --session-secret perf/bench/seed's
 * metadata says the hub was actually started with.
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
 * bench-rev-1 fixes (see gs://scion-xproject-exchange/slow-list/reviews/
 * bench-rev-1.md): a run that never populates within its timeout is now
 * classified as populated / load-failed(<reason>) / still-loading by
 * watching the page's network activity for the load-bearing API request,
 * instead of being lumped in with genuine "still rendering" cases (B2).
 * Timed-out/failed runs are excluded from the median/min/max/stddev
 * (B3), which are now computed over successes only and reported alongside
 * successCount/failureCount. The SSE burst scenario now runs
 * --burst-runs times (default 5, not 1), skips agents that are already
 * `suspended` (a silently-dropped update target -- see
 * pkg/hub/handlers_agent_lifecycle.go's Guard 0), never targets
 * `suspended` either, checks every POST's status, and restores every
 * updated agent to its pre-burst phase afterward so repeated runs do not
 * accumulate stuck agents (B1).
 */

import { chromium } from '@playwright/test';
import * as crypto from 'node:crypto';
import * as fs from 'node:fs';

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
// bench-rev-1 B3: the burst scenario now runs multiple times like the other
// scenarios, not once (n=1 against a brief requiring at least 5 runs).
const burstRuns = parseInt(args['burst-runs'] || String(runs), 10);
const settleTimeoutMs = parseInt(args['settle-timeout-ms'] || '30000', 10);
const notes = args.notes || '';

const seed = JSON.parse(fs.readFileSync(seedPath, 'utf8'));

// Incremental output (bench-rev-1 N5): written after every scenario and
// every burst run, so a Chromium crash mid-benchmark (observed on this
// container's default /dev/shm size -- see the --disable-dev-shm-usage
// launch arg below) loses at most the in-flight run, not the whole report.
function writeReportSoFar(report) {
  fs.writeFileSync(outPath, JSON.stringify(report, null, 2));
}

// ---- test-login session (mirrors web/e2e/harness/auth.ts) -------------

const USER_TOKEN_ISSUER = 'scion-hub';
const TEST_LOGIN_AUDIENCE = 'scion-test-login';
const USER_SIGNING_KEY_NAME = 'user_signing_key';

function deriveSigningKey(secret, keyName) {
  return crypto.createHash('sha256').update(`scion-hub-signing-key:${keyName}:${secret}`).digest();
}

function base64url(buf) {
  return buf.toString('base64url');
}

function signJWT(payload, signingKey) {
  const header = base64url(Buffer.from(JSON.stringify({ alg: 'HS256', typ: 'JWT' })));
  const body = base64url(Buffer.from(JSON.stringify(payload)));
  const sig = crypto.createHmac('sha256', signingKey).update(`${header}.${body}`).digest();
  return `${header}.${body}.${base64url(sig)}`;
}

function generateTestLoginToken(secret, subject = 'perf-bench') {
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
    // make Playwright silently refuse to send it back. Strip it here rather
    // than requiring a local TLS setup purely for the benchmark.
    if (cookie.secure && url.protocol === 'http:') cookie.secure = false;
    if (!cookie.sameSite) cookie.sameSite = 'Lax';
    cookies.push(cookie);
  }
  return { cookies, user: data.user };
}

// ---- page helpers -------------------------------------------------------

// countAllDeep counts every element in the document AND every open shadow
// root reachable from it, matching how ptone/scion#2367's original
// investigation reported "DOM elements including shadow roots" for a Lit
// app where nearly all structure lives inside shadow DOM.
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

function summarizeLongTasks(entries) {
  if (!entries.length) return { count: 0, totalMs: 0, maxMs: 0 };
  let total = 0;
  let max = 0;
  for (const e of entries) {
    total += e.duration;
    if (e.duration > max) max = e.duration;
  }
  return { count: entries.length, totalMs: total, maxMs: max };
}

// ---- network-outcome classification (bench-rev-1 B2) ---------------------
//
// The hub's default WriteTimeout is 60s (pkg/config/hub_config.go's
// HubServerConfig.WriteTimeout, and pkg/hub/web.go's WebServer.Start,
// hard-coded). A handler that has not started writing its response by then
// gets its connection forcibly closed: the browser's fetch/XHR sees a
// network-level failure (an empty reply), loadData's Promise.all rejects,
// and the page falls to its error/empty state -- which renders almost no
// agent cards and looks, from a "did the selector count reach N" check
// alone, identical to "still loading". Watching the actual network outcome
// of the load-bearing API request is the only way to tell those apart.

// apiMatcherFor returns a predicate matching the specific request each
// scenario's data load depends on, per project-detail.ts / agent-graph.ts.
function apiMatcherFor(scenarioKey, projectId) {
  if (scenarioKey === 'standalone-graph') {
    return (url) => url.includes('/api/v1/agents') && url.includes(`projectId=${projectId}`);
  }
  // project-grid / project-list / project-graph-embedded all load via
  // project-detail.ts's loadData(), which fetches both the project and its
  // agents in parallel; the agents call is the one whose cost scales with
  // agent count.
  return (url) => url.includes(`/api/v1/projects/${projectId}/agents`);
}

function attachNetworkWatch(page, matches) {
  const state = { status: null, failed: null, url: null };
  const onResponse = (resp) => {
    if (matches(resp.url())) {
      state.status = resp.status();
      state.url = resp.url();
    }
  };
  const onRequestFailed = (req) => {
    if (matches(req.url())) {
      state.failed = req.failure()?.errorText || 'unknown';
      state.url = req.url();
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

// classifyOutcome turns (populated?, network state) into one of:
// "populated" | "load-failed(<reason>)" | "still-loading".
function classifyOutcome(populatedOk, netState) {
  if (populatedOk) return 'populated';
  if (netState.failed) return `load-failed(network:${netState.failed})`;
  if (netState.status != null && (netState.status < 200 || netState.status >= 300)) {
    return `load-failed(http:${netState.status})`;
  }
  return 'still-loading';
}

// ---- scenario definitions ------------------------------------------------

// The web app's project-detail route reads the path segment verbatim and
// passes it straight to `GET /api/v1/projects/{id}` (pkg/hub/
// handlers_projects_core.go's getProject), which does a raw-ID store lookup
// with no slug resolution (unlike the agent-list endpoints' projectId query
// param, which does resolve slugs). The URL must carry the project UUID.
const projectPath = `/projects/${encodeURIComponent(seed.projectId)}`;
const scenarios = [
  {
    key: 'project-grid',
    url: projectPath,
    viewMode: 'grid',
    selector: '.agent-card',
  },
  {
    key: 'project-list',
    url: projectPath,
    viewMode: 'list',
    // bench-rev-1 Nit1: ".agent-table-container tr" also matches the header
    // row, so readiness fired one row early (e.g. 26/25, 501/500 in raw
    // data). Scope to the body rows.
    selector: '.agent-table-container tbody tr',
  },
  {
    key: 'project-graph-embedded',
    url: projectPath,
    viewMode: 'graph',
    selector: '.node-wrapper',
  },
  {
    key: 'standalone-graph',
    url: `/agents/graph?project=${encodeURIComponent(seed.projectId)}`,
    viewMode: null,
    selector: '.node-wrapper',
  },
];

async function runScenario(context, scenario, expectedCount) {
  const apiMatches = apiMatcherFor(scenario.key, seed.projectId);
  const runsOut = [];
  for (let i = 0; i < runs; i++) {
    const page = await context.newPage();
    const consoleErrors = [];
    page.on('console', (msg) => {
      if (msg.type() === 'error') consoleErrors.push(msg.text());
    });
    const netWatch = attachNetworkWatch(page, apiMatches);

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
        // longtask not supported in this engine build; long-task metrics
        // will just read as zero for this run rather than failing it.
      }
    });
    if (scenario.viewMode) {
      const vm = scenario.viewMode;
      await page.addInitScript((viewMode) => {
        try {
          localStorage.setItem('scion-view-project-agents', viewMode);
        } catch {
          // localStorage unavailable pre-navigation in some engine states;
          // the page falls back to its own default view in that case.
        }
      }, vm);
    }

    const navStart = Date.now();
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
    netWatch.detach();

    runsOut.push({
      run: i,
      outcome,
      // navToPopulatedMs is set ONLY for a genuinely populated run
      // (bench-rev-1 B3): a load-failed or still-loading run's elapsed
      // wall-clock time is not a rendering duration and must never be
      // averaged into medianNavToPopulatedMs as if it were one.
      navToPopulatedMs: isPopulated ? elapsedMs : null,
      elapsedMs,
      populatedCount: populated.count,
      expectedCount,
      domElementCount: domCount,
      longTasks: summarizeLongTasks(longTasks),
      networkStatus: netWatch.state.status,
      networkFailed: netWatch.state.failed,
      consoleErrorCount: consoleErrors.length,
      consoleErrorsSample: consoleErrors.slice(0, 5),
    });

    await page.close();
  }
  return runsOut;
}

function median(xs) {
  if (!xs.length) return null;
  const s = [...xs].sort((a, b) => a - b);
  const mid = Math.floor(s.length / 2);
  return s.length % 2 ? s[mid] : (s[mid - 1] + s[mid]) / 2;
}

function minMax(xs) {
  if (!xs.length) return { min: null, max: null };
  return { min: Math.min(...xs), max: Math.max(...xs) };
}

function stddev(xs) {
  if (xs.length < 2) return 0;
  const mean = xs.reduce((a, b) => a + b, 0) / xs.length;
  const sumSq = xs.reduce((a, b) => a + (b - mean) * (b - mean), 0);
  return Math.sqrt(sumSq / (xs.length - 1));
}

// summarizeScenario computes bench-rev-1 B3's required spread stats --
// median/min/max/stddev, not just a median -- over SUCCESSFUL
// ("populated") runs only, and reports success/failure counts and an
// outcome tally separately so a reader can see at a glance whether a
// scenario's numbers are "5/5 populated" or "1/5 populated, 4 load-failed".
function summarizeScenario(scenario, results) {
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

  return {
    selector: scenario.selector,
    runs: results,
    successCount: populatedRuns.length,
    failureCount: results.length - populatedRuns.length,
    outcomeCounts,
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

// ---- live-update (SSE burst) responsiveness ------------------------------

// getStatusBadgeLabelDeep reads the rendered <scion-status-badge label=...>
// for a given agent's card, searching every shadow root. project-detail.ts's
// renderAgentCard (around line 2515) doesn't stamp a data-agent-id, but it
// does render `<a href="/agents/{id}">`, which is a stable enough anchor to
// find the enclosing .agent-card and its status badge from the outside.
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

// postAgentPhase POSTs a phase update and returns whether it was accepted
// (bench-rev-1 B1: "check every POST response" -- the original version
// never did, so a silently-dropped update to a `suspended` agent looked
// identical to a slow-but-successful one).
async function postAgentPhase(id, phase) {
  const res = await fetch(`${hubBase}/api/v1/agents/${id}/status`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${seed.ownerToken}` },
    body: JSON.stringify({ phase }),
  });
  return { ok: res.ok, status: res.status };
}

async function getAgentPhase(id) {
  const res = await fetch(`${hubBase}/api/v1/agents/${id}`, {
    headers: { Authorization: `Bearer ${seed.ownerToken}` },
  });
  if (!res.ok) return null;
  const data = await res.json();
  return data.phase ?? null;
}

// store.AgentStatusUpdate (pkg/store/store.go) has no top-level "status"
// field -- only "phase"/"activity"/etc. POSTing {"status": ...} decodes
// successfully (readJSON ignores unknown keys) but silently changes
// nothing, which looks identical to "the update never arrived" from the
// browser side. Use "phase" with a value from pkg/agent/state.Phase.
//
// bench-rev-1 B1: deliberately excludes "suspended" as a *target* --
// updateAgentStatus's Guard 0 (pkg/hub/handlers_agent_lifecycle.go) silently
// drops any phase/activity update sent to an agent that is *currently*
// suspended, and still returns 200. Targeting "suspended" therefore makes
// that agent permanently un-updatable for every subsequent run against the
// same database, which is what produced the originally-reported "6/15" and
// "12/15" settle results -- those were deterministic artifacts of
// previously-suspended agents, not SSE flakiness. Agents that are already
// suspended are also skipped as *sources* below, for the same reason.
//
// Also excludes "running": web/src/shared/types.ts's getAgentDisplayStatus()
// renders a running agent's *activity* instead of the literal phase string
// whenever activity is non-empty, so "running" would only be unambiguous
// for agents that happen to have no activity set -- every other target here
// renders as the phase string verbatim.
const BURST_TARGET_ROTATION = ['stopped', 'error', 'stopping'];

async function runBurstOnce(page) {
  // Fetch a page of real agents, authenticated as the seeded project owner
  // (the seeded member may not carry agent.update -- ordinary project
  // members are not expected to; using the owner to *drive* the burst is
  // fine, since the thing under measurement is how the *viewer's* browser
  // session settles after a burst of SSE updates, not who sent them).
  // Over-fetch beyond burstCount so there is room to skip already-suspended
  // agents (bench-rev-1 B1) without running out of candidates.
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
    console.warn(
      `  warning: only ${targets.length}/${burstCount} non-suspended agents available for the burst`
    );
  }

  const preBurstPhase = new Map(targets.map((a) => [a.id, a.phase]));
  const targetPhase = new Map();
  for (let idx = 0; idx < targets.length; idx++) {
    const id = targets[idx].id;
    let phase = BURST_TARGET_ROTATION[idx % BURST_TARGET_ROTATION.length];
    if (phase === preBurstPhase.get(id)) {
      phase = BURST_TARGET_ROTATION[(idx + 1) % BURST_TARGET_ROTATION.length];
    }
    targetPhase.set(id, phase);
  }

  const burstStart = Date.now();
  const postResults = await Promise.all(
    targets.map(async (a) => ({ id: a.id, ...(await postAgentPhase(a.id, targetPhase.get(a.id))) }))
  );
  const burstSentMs = Date.now() - burstStart;

  const rejected = postResults.filter((r) => !r.ok);
  const acceptedIds = postResults.filter((r) => r.ok).map((r) => r.id);

  // Poll every burst-updated agent's badge until all *accepted* updates
  // show their target status in the DOM (ground truth for "the live update
  // reached the DOM"), or until settleTimeoutMs elapses.
  const deadline = Date.now() + settleTimeoutMs;
  const settledAtByAgent = new Map();
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
  const lastSettledAt = settledCount ? Math.max(...settledAtByAgent.values()) : null;
  const settledAllMs = lastSettledAt != null ? lastSettledAt - burstStart : null;

  // bench-rev-1 B1: distinguish "the server never applied the update" from
  // "the UI did not settle" -- GET each targeted agent's server-side phase
  // and compare to what was requested, separately from the DOM-badge
  // settle check above. A GET-confirmed non-application (e.g. a rejected
  // POST, or a race with the restore below) must never be blamed on SSE.
  const serverAppliedCount = (
    await Promise.all(
      acceptedIds.map(async (id) => (await getAgentPhase(id)) === targetPhase.get(id))
    )
  ).filter(Boolean).length;

  // Restore every targeted agent to its pre-burst phase (bench-rev-1 B1:
  // "make the scenario non-destructive"), so a subsequent run -- in this
  // same process or a future invocation against the same database -- does
  // not inherit an ever-growing set of stuck/suspended agents. Restoring to
  // a phase that is itself never "suspended" (filtered above) means this
  // never re-triggers Guard 0 on the next run.
  const restoreResults = await Promise.all(
    targets.map(async (a) => ({
      id: a.id,
      ...(await postAgentPhase(a.id, preBurstPhase.get(a.id))),
    }))
  );
  const restoreFailures = restoreResults.filter((r) => !r.ok);
  if (restoreFailures.length > 0) {
    console.warn(
      `  warning: failed to restore ${restoreFailures.length}/${targets.length} agents to their pre-burst phase`
    );
  }

  return {
    requestedCount: targets.length,
    acceptedCount: acceptedIds.length,
    rejectedCount: rejected.length,
    rejectedSample: rejected.slice(0, 3),
    burstSentMs,
    settledCount,
    serverAppliedCount,
    timedOut: settledCount < acceptedIds.length,
    // Null when not every accepted agent's badge updated within
    // settleTimeoutMs -- reported explicitly as a timeout rather than a
    // misleadingly small number.
    settledAllMs,
    restoreFailureCount: restoreFailures.length,
  };
}

async function runBurstScenario(context) {
  const page = await context.newPage();
  await page.addInitScript(() => {
    localStorage.setItem('scion-view-project-agents', 'grid');
  });
  const apiMatches = apiMatcherFor('project-grid', seed.projectId);
  const netWatch = attachNetworkWatch(page, apiMatches);
  await page.goto(hubBase + projectPath, { waitUntil: 'domcontentloaded', timeout: navTimeoutMs });
  const populated = await waitForCount(page, '.agent-card', seed.agentCount, populateTimeoutMs);
  netWatch.detach();

  // bench-rev-1 N4: do not fire a burst at a page that never loaded --
  // runBurstScenario previously ignored waitForCount's timedOut entirely,
  // so at 500 agents it could (and did) fire the burst against an
  // error/empty page and then blame SSE for the resulting non-settle.
  if (populated.timedOut) {
    const outcome = classifyOutcome(false, netWatch.state);
    await page.close();
    return {
      skipped: true,
      skipReason: `grid did not populate before firing the burst (${outcome})`,
      runsAttempted: 0,
      results: [],
    };
  }

  const results = [];
  for (let i = 0; i < burstRuns; i++) {
    const r = await runBurstOnce(page);
    results.push(r);
    console.log(
      `  burst run ${i}: ${r.acceptedCount}/${r.requestedCount} accepted, ` +
        `${r.settledCount}/${r.acceptedCount} settled in DOM, ` +
        `${r.serverAppliedCount}/${r.acceptedCount} confirmed server-applied ` +
        `(settledAllMs=${r.settledAllMs}, timedOut=${r.timedOut})`
    );
  }
  await page.close();

  const settledAllMsValues = results.map((r) => r.settledAllMs).filter((v) => v != null);
  const mm = minMax(settledAllMsValues);
  return {
    skipped: false,
    runsAttempted: results.length,
    results,
    medianSettledAllMs: median(settledAllMsValues),
    minSettledAllMs: mm.min,
    maxSettledAllMs: mm.max,
    stddevSettledAllMs: stddev(settledAllMsValues),
    fullySettledRunCount: results.filter((r) => !r.timedOut).length,
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

  const browser = await chromium.launch({
    headless: true,
    // bench-rev-1 N5: this container's /dev/shm is 64MB, which crashes
    // Chromium (page.evaluate: Target crashed) partway through a large-DOM
    // run without this flag -- and the original script wrote no report at
    // all when that happened. --disable-dev-shm-usage makes Chromium use
    // /tmp instead.
    args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'],
  });
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
  await context.addCookies(cookies);

  const report = {
    generatedAt: new Date().toISOString(),
    hubBaseUrl: hubBase,
    seedProjectId: seed.projectId,
    seedProjectSlug: seed.projectSlug,
    agentCount: seed.agentCount,
    runs,
    burstRuns,
    notes,
    scenarios: {},
  };
  writeReportSoFar(report);

  try {
    for (const scenario of scenarios) {
      console.log(`running scenario ${scenario.key} (${runs} runs)...`);
      const results = await runScenario(context, scenario, seed.agentCount);
      report.scenarios[scenario.key] = summarizeScenario(scenario, results);
      const s = report.scenarios[scenario.key];
      console.log(
        `  ${s.successCount}/${results.length} populated; outcomes=${JSON.stringify(s.outcomeCounts)}; ` +
          `median nav->populated: ${s.medianNavToPopulatedMs}ms [${s.minNavToPopulatedMs}, ${s.maxNavToPopulatedMs}]; ` +
          `median DOM count: ${s.medianDomElementCount} [${s.minDomElementCount}, ${s.maxDomElementCount}]`
      );
      writeReportSoFar(report);
    }

    console.log(`running SSE burst-update responsiveness scenario (${burstRuns} runs)...`);
    report.liveUpdateBurst = await runBurstScenario(context);
    if (report.liveUpdateBurst.skipped) {
      console.log(`  skipped: ${report.liveUpdateBurst.skipReason}`);
    } else {
      console.log(
        `  median settle time: ${report.liveUpdateBurst.medianSettledAllMs}ms ` +
          `[${report.liveUpdateBurst.minSettledAllMs}, ${report.liveUpdateBurst.maxSettledAllMs}]; ` +
          `${report.liveUpdateBurst.fullySettledRunCount}/${report.liveUpdateBurst.runsAttempted} runs fully settled`
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
