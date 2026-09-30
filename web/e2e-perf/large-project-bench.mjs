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
const settleQuietMs = parseInt(args['settle-quiet-ms'] || '500', 10);
const settleTimeoutMs = parseInt(args['settle-timeout-ms'] || '30000', 10);
const notes = args.notes || '';

const seed = JSON.parse(fs.readFileSync(seedPath, 'utf8'));

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
    selector: '.agent-table-container tr',
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
  const runsOut = [];
  for (let i = 0; i < runs; i++) {
    const page = await context.newPage();
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
    let navToPopulatedMs = null;
    if (!navError) {
      populated = await waitForCount(page, scenario.selector, expectedCount, populateTimeoutMs);
      navToPopulatedMs = Date.now() - navStart;
    }

    const domCount = navError ? null : await countAllDeep(page);
    const longTasks = navError ? [] : await page.evaluate(() => window.__benchLongTasks || []);

    runsOut.push({
      run: i,
      navError,
      navToPopulatedMs,
      populatedCount: populated.count,
      expectedCount,
      timedOut: populated.timedOut,
      domElementCount: domCount,
      longTasks: summarizeLongTasks(longTasks),
    });

    await page.close();
  }
  return runsOut;
}

function median(xs) {
  const s = [...xs].sort((a, b) => a - b);
  if (!s.length) return null;
  const mid = Math.floor(s.length / 2);
  return s.length % 2 ? s[mid] : (s[mid - 1] + s[mid]) / 2;
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

async function runBurstScenario(context) {
  const page = await context.newPage();
  await page.addInitScript(() => {
    localStorage.setItem('scion-view-project-agents', 'grid');
  });
  await page.goto(hubBase + projectPath, { waitUntil: 'domcontentloaded', timeout: navTimeoutMs });
  await waitForCount(page, '.agent-card', seed.agentCount, populateTimeoutMs);

  // Fetch a real page of agent IDs to update, authenticated as the seeded
  // project owner (the seeded member may not carry agent.update -- ordinary
  // project members are not expected to; using the owner to *drive* the
  // burst is fine, since the thing under measurement is how the *viewer's*
  // browser session settles after a burst of SSE updates, not who sent them).
  const listResp = await fetch(
    `${hubBase}/api/v1/projects/${seed.projectId}/agents?limit=${burstCount}`,
    { headers: { Authorization: `Bearer ${seed.ownerToken}` } }
  );
  if (!listResp.ok) {
    throw new Error(`fetching agents for burst failed: ${listResp.status}`);
  }
  const listData = await listResp.json();
  const agentIds = (listData.agents || []).slice(0, burstCount).map((a) => a.id);

  // NOTE: a plain `MutationObserver` on document.body (even with
  // subtree:true) does NOT see mutations inside shadow roots -- it does not
  // cross shadow boundaries at all, and this app's agent cards live several
  // shadow roots deep (scion-page-project-detail > ... > scion-status-badge).
  // An earlier version of this script tried exactly that as a secondary
  // "DOM mutation count" signal and it always read zero, even for updates
  // independently confirmed to have rendered. Rather than attach a separate
  // observer per shadow root (fragile: would need to know every relevant
  // custom element's tag name up front), this harness measures live-update
  // responsiveness the direct way below: poll each burst-updated agent's own
  // rendered status badge for its new value. That is ground truth for
  // "the live update reached the DOM", which is what actually matters here.

  // Pick a target status per agent that is guaranteed to differ from its
  // current rendered label, so "badge now shows the target" is unambiguous
  // evidence of a live update having been applied (not a false-positive
  // match against a status the card already happened to show).
  // store.AgentStatusUpdate (pkg/store/store.go) has no top-level "status"
  // field -- only "phase"/"activity"/etc. POSTing {"status": ...} decodes
  // successfully (readJSON ignores unknown keys) but silently changes
  // nothing, which looks identical to "the update never arrived" from the
  // browser side. Use "phase" with a value from pkg/agent/state.Phase.
  //
  // Deliberately excludes "running": web/src/shared/types.ts's
  // getAgentDisplayStatus() renders a running agent's *activity* instead of
  // the literal phase string whenever activity is non-empty, so "running"
  // would only be unambiguous for agents that happen to have no activity
  // set -- every other target here renders as the phase string verbatim.
  const rotation = ['stopped', 'error', 'suspended', 'stopping'];
  const targets = new Map();
  for (let idx = 0; idx < agentIds.length; idx++) {
    const id = agentIds[idx];
    const current = (await getStatusBadgeLabelDeep(page, id)) || '';
    let target = rotation[idx % rotation.length];
    if (target.toLowerCase() === current.toLowerCase()) {
      target = rotation[(idx + 1) % rotation.length];
    }
    targets.set(id, target);
  }

  const burstStart = Date.now();
  await Promise.all(
    agentIds.map((id) =>
      fetch(`${hubBase}/api/v1/agents/${id}/status`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${seed.ownerToken}` },
        body: JSON.stringify({ phase: targets.get(id) }),
      })
    )
  );
  const burstSentMs = Date.now() - burstStart;

  // Poll every burst-updated agent's badge until all show their target
  // status (ground truth for "the live update reached the DOM"), or until
  // settleTimeoutMs elapses.
  const deadline = Date.now() + settleTimeoutMs;
  const settledAtByAgent = new Map();
  for (;;) {
    for (const id of agentIds) {
      if (settledAtByAgent.has(id)) continue;
      const label = await getStatusBadgeLabelDeep(page, id);
      if (label && label.toLowerCase() === targets.get(id).toLowerCase()) {
        settledAtByAgent.set(id, Date.now());
      }
    }
    if (settledAtByAgent.size === agentIds.length) break;
    if (Date.now() > deadline) break;
    await page.waitForTimeout(100);
  }
  const settledCount = settledAtByAgent.size;
  const lastSettledAt = settledCount ? Math.max(...settledAtByAgent.values()) : null;
  const settledAllMs = lastSettledAt != null ? lastSettledAt - burstStart : null;

  await page.close();
  return {
    agentCount: agentIds.length,
    burstSentMs,
    settledCount,
    timedOut: settledCount < agentIds.length,
    // Null when not every agent's badge updated within settleTimeoutMs --
    // reported explicitly as a timeout rather than a misleadingly small
    // number, per "state which gates ran and which didn't".
    settledAllMs,
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
    args: ['--no-sandbox', '--disable-setuid-sandbox'],
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
    notes,
    scenarios: {},
  };

  for (const scenario of scenarios) {
    console.log(`running scenario ${scenario.key} (${runs} runs)...`);
    const results = await runScenario(context, scenario, seed.agentCount);
    const navTimes = results
      .filter((r) => r.navToPopulatedMs != null)
      .map((r) => r.navToPopulatedMs);
    const domCounts = results
      .filter((r) => r.domElementCount != null)
      .map((r) => r.domElementCount);
    report.scenarios[scenario.key] = {
      selector: scenario.selector,
      runs: results,
      medianNavToPopulatedMs: median(navTimes),
      medianDomElementCount: median(domCounts),
      anyTimedOut: results.some((r) => r.timedOut),
    };
    console.log(
      `  median nav->populated: ${report.scenarios[scenario.key].medianNavToPopulatedMs}ms, ` +
        `median DOM count: ${report.scenarios[scenario.key].medianDomElementCount}, ` +
        `timed out: ${report.scenarios[scenario.key].anyTimedOut}`
    );
  }

  console.log('running SSE burst-update responsiveness scenario...');
  report.liveUpdateBurst = await runBurstScenario(context);
  console.log(
    `  burst sent in ${report.liveUpdateBurst.burstSentMs}ms, ` +
      `${report.liveUpdateBurst.settledCount}/${report.liveUpdateBurst.agentCount} agents settled ` +
      `${report.liveUpdateBurst.settledAllMs}ms after burst start (timedOut=${report.liveUpdateBurst.timedOut})`
  );

  await browser.close();

  fs.writeFileSync(outPath, JSON.stringify(report, null, 2));
  console.log(`report written to ${outPath}`);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
