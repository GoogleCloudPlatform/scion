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
 * Notable behavior of this harness:
 * - The network matcher matches on parsed pathname, so it fires correctly
 *   for the standalone graph's unscoped `/api/v1/agents` fetch (that page
 *   never sends a `projectId=` query param).
 * - `pickBurstTarget` excludes both the agent's pre-burst phase and its
 *   actual previous-run target, each target's badge is read immediately
 *   before posting so any agent already showing its about-to-be-requested
 *   value is excluded from that run's settle tracking, and a run is marked
 *   invalid -- excluded from the scenario's settle statistics -- whenever
 *   the previous run's restore was not fully confirmed in the DOM before
 *   this run started. This bound does not block the next run; see
 *   runBurstOnce for the mark-invalid mechanism. Without these guards, a
 *   prior run's stale, not-yet-restored badge could be miscounted as the
 *   next run's settle. The restore-wait's expected value is computed the
 *   way the UI renders it (`displayStatusLabel`), not the literal
 *   pre-burst phase.
 * - Restores both phase and activity, not phase alone.
 * - Settle time is measured per agent from that agent's own POST
 *   completion, observed by a polling loop dedicated to that agent and
 *   started the instant that agent's own POST resolves -- not from a
 *   shared burst-start timestamp or only after every agent's POST has
 *   resolved, either of which would inflate settle time by folding in POST
 *   latency or the spread between POST completions.
 * - The first run of each scenario uses a fresh ("cold") browser context;
 *   subsequent runs share one ("warm") context. Reported separately.
 * - The report records the harness's git commit (from the binary's own
 *   build-time VCS stamp, not `git rev-parse HEAD` in the caller's cwd,
 *   which could silently record the wrong commit), a dirty-tree flag, the
 *   hub's own version (`GET /health`), and effective timeouts.
 * - Network failures/non-2xx responses record when they happened (elapsed
 *   ms from navigation start), so WriteTimeout attribution is measured,
 *   not inferred from wall-clock totals alone.
 * - A 2xx response that never renders is `loaded-not-rendered`, distinct
 *   from `still-loading` (no response observed at all).
 * - Graph interaction timing (pan/zoom/hover) is measured for both graph
 *   scenarios.
 */

import { chromium } from '@playwright/test';
import * as fs from 'node:fs';
import { execFileSync } from 'node:child_process';
import * as os from 'node:os';
import * as path from 'node:path';
import { fileURLToPath } from 'node:url';

import {
  apiMatcherFor,
  classifyOutcome,
  median,
  minMax,
  summarizeLongTasks,
  summarizeScenario,
  generateTestLoginToken,
  pickBurstTarget,
  displayStatusLabel,
  computePreStaleIds,
  resolveBatchTickSettled,
  summarizeBurstScenario,
  expectedFirstPageCount,
  pageCountFor,
  summarizePageChanges,
  walkEndReason,
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
// How many Next clicks each populated paged-view run times (fewer when the
// view has fewer pages).
const pageChanges = parseInt(args['page-changes'] || '3', 10);
const notes = args.notes || '';
// Lets a targeted re-measurement (e.g. re-running only the SSE burst after
// a burst-logic-only change) skip the four view scenarios, which can take
// most of a run's wall-clock time at 500 agents and whose numbers such a
// change would not affect.
const burstOnly = Boolean(args['burst-only']);

const seed = JSON.parse(fs.readFileSync(seedPath, 'utf8'));

// Records enough provenance that a report can be matched back to the exact
// harness version and settings that produced it, without relying on
// wall-clock proximity to a commit.
const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
// `stdio: ['ignore', 'pipe', 'ignore']` suppresses git's
// `fatal: not a git repository` going straight to the console when this is
// run outside a checkout -- the caller already handles a null return.
function gitHeadSha() {
  try {
    return execFileSync('git', ['rev-parse', 'HEAD'], {
      cwd: repoRoot,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
    }).trim();
  } catch {
    return null;
  }
}

// A dirty working tree means the running script may not exactly match
// harnessCommit's committed tree -- recorded alongside it rather than
// silently assumed clean. `repoRoot` is resolved from import.meta.url
// (this file's own location), not the caller's cwd, so this is correct
// regardless of where the script is invoked from.
//
// This counts UNTRACKED files as dirty (plain `git status --porcelain`, no
// `--untracked-files=no`), so a stray untracked file anywhere in the repo
// marks the harness dirty even with zero tracked changes. This is
// intentional, not an oversight: Go's OWN VCS auto-stamping -- the thing
// `harnessCommit`/`harnessCommitDirty` on the Go side, and the hub's own
// `hubScionVersion`, are both built on -- determines `vcs.modified` via
// exactly `git status --porcelain` with no `--untracked-files=no` either
// (see the Go toolchain's own `cmd/go/internal/vcs/vcs.go`, the `git
// status --porcelain` call used for dirty detection; confirmed empirically
// too: a tree with every tracked file committed and exactly one new
// untracked file present still VCS-stamps `vcs.modified=true`). So this
// function already matches Go's real semantics exactly. Switching to
// `--untracked-files=no` would make the JS and Go sides of this harness
// DISAGREE about what "dirty" means, not agree more closely -- kept as-is
// instead. See perf/bench/README.md and measurements.md's section 3 for
// the same reasoning stated for a reader of the published numbers.
function gitIsDirty() {
  try {
    const out = execFileSync('git', ['status', '--porcelain'], {
      cwd: repoRoot,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
    });
    return out.trim().length > 0;
  } catch {
    return null;
  }
}

// Identifies the hub binary under test via its own unauthenticated GET
// /health (pkg/hub/handlers_health.go), mirroring apibench's
// fetchHubVersion. Best-effort: never throws, returns null on any failure.
//
// Returns ONLY scionVersion. /health's `version` field is a hard-coded
// `"0.1.0"` literal in hub source (pkg/hub/handlers_health.go:86, marked
// `// TODO: Get from build info`), constant regardless of which hub commit
// is actually running -- recording it under a name like `hubVersion` would
// look like real provenance but carry none, the same reasoning applied to
// the harness's own commit field above. `scionVersion`
// (`pkg/version.Short()`) is real provenance when the hub binary is built
// correctly (a regular clone, not a `git worktree`; see README) and
// "unknown" otherwise -- never a placeholder presented as if it were real.
async function fetchHubScionVersion(baseURL) {
  try {
    const res = await fetch(`${baseURL.replace(/\/$/, '')}/health`);
    if (!res.ok) return null;
    const body = await res.json();
    return body.scionVersion ?? null;
  } catch {
    return null;
  }
}

// Incremental output: written after every scenario and every burst run, so
// a Chromium crash mid-benchmark loses at most the in-flight run, not the
// whole report.
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

// Returns bounding boxes (in viewport coordinates) for up to `limit`
// matched elements, piercing shadow roots, so the graph-interaction step
// below can hover/drag over real node positions instead of guessing
// coordinates.
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

// readPagerDeep returns the first scion-agent-pager's state (piercing
// shadow roots), or null when none is rendered. The page size is read from
// the pager itself rather than assumed, so a persisted or future default
// page size is measured as the user would see it.
async function readPagerDeep(page) {
  return page.evaluate(() => {
    function find(root) {
      const el = root.querySelector('scion-agent-pager');
      if (el) return el;
      for (const n of root.querySelectorAll('*')) {
        if (n.shadowRoot) {
          const f = find(n.shadowRoot);
          if (f) return f;
        }
      }
      return null;
    }
    const p = find(document);
    if (!p) return null;
    const total = typeof p.total === 'number' ? p.total : null;
    return {
      pageSize: p.pageSize,
      pageIndex: p.pageIndex,
      rowsOnPage: p.rowsOnPage,
      total,
      hasNext: p.hasNext,
      loading: p.loading,
    };
  });
}

// clickPagerNextDeep clicks the first pager's Next button; returns whether
// an enabled Next button was found.
async function clickPagerNextDeep(page) {
  return page.evaluate(() => {
    function find(root) {
      const el = root.querySelector('scion-agent-pager');
      if (el) return el;
      for (const n of root.querySelectorAll('*')) {
        if (n.shadowRoot) {
          const f = find(n.shadowRoot);
          if (f) return f;
        }
      }
      return null;
    }
    const p = find(document);
    const buttons = p?.shadowRoot ? [...p.shadowRoot.querySelectorAll('sl-button')] : [];
    const next = buttons.find((b) => /next/i.test(b.textContent || ''));
    if (!next || next.disabled) return false;
    next.click();
    return true;
  });
}

// firstItemKeyDeep identifies the first rendered card or row (its agent
// link), so a page change is only counted once different items render.
async function firstItemKeyDeep(page, selector) {
  return page.evaluate((sel) => {
    function walk(root) {
      const el = root.querySelector(sel);
      if (el) return el;
      for (const n of root.querySelectorAll('*')) {
        if (n.shadowRoot) {
          const f = walk(n.shadowRoot);
          if (f) return f;
        }
      }
      return null;
    }
    const el = walk(document);
    if (!el) return null;
    const a = el.querySelector('a[href^="/agents/"]') || el.closest('a[href^="/agents/"]');
    return a ? a.getAttribute('href') : (el.textContent || '').trim().slice(0, 80);
  }, selector);
}

// waitForFirstPage waits until a paged grid or list renders its first page:
// min(page size, total) items, with the pager idle. Falls back to every
// agent when no pager renders (an unpaged view).
async function waitForFirstPage(page, selector, agentCount, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  let last = { count: 0, pager: null, expected: agentCount };
  while (Date.now() < deadline) {
    const [count, pager] = await Promise.all([
      countSelectorDeep(page, selector),
      readPagerDeep(page),
    ]);
    // A pager can render briefly with total 0 before the first response;
    // trust its total only once it is positive.
    const total = pager && pager.total > 0 ? pager.total : agentCount;
    const expected = expectedFirstPageCount(pager ? pager.pageSize : null, agentCount, total);
    last = { count, pager, expected };
    if (count >= 1 && count >= expected && !(pager && pager.loading)) {
      return { ...last, timedOut: false };
    }
    // A seed with no agents renders the project's empty state, not a
    // pager: populated once that empty state is on screen (not while the
    // page is still loading).
    if (agentCount === 0 && !pager && (await countSelectorDeep(page, '.empty-state')) > 0) {
      return { ...last, timedOut: false };
    }
    await page.waitForTimeout(100);
  }
  return { ...last, timedOut: true };
}

// measurePageChanges clicks Next up to `maxChanges` times and times each
// change: from the click until the pager shows the next page index, is
// idle, and the rendered items match its rowsOnPage with a different first
// item than before.
async function measurePageChanges(page, selector, maxChanges, timeoutMs) {
  const out = [];
  let stopReason = maxChanges > 0 ? null : 'none-requested';
  for (let i = 0; i < maxChanges; i++) {
    // Start each change from an idle pager.
    let before = await readPagerDeep(page);
    const idleDeadline = Date.now() + timeoutMs;
    while (before && before.loading && Date.now() < idleDeadline) {
      await page.waitForTimeout(50);
      before = await readPagerDeep(page);
    }
    if (!before) {
      stopReason = 'no-pager';
      break;
    }
    if (before.loading) {
      stopReason = 'pager-busy';
      break;
    }
    if (!before.hasNext) {
      // Next is disabled. On the last page that is the normal end of the
      // view; before it (the pager's own total says more pages exist) the
      // walk ended early, which is a product behaviour worth surfacing.
      stopReason = walkEndReason(before);
      break;
    }
    const beforeKey = await firstItemKeyDeep(page, selector);
    const toPageIndex = before.pageIndex + 1;
    const t0 = Date.now();
    if (!(await clickPagerNextDeep(page))) {
      stopReason = 'next-disabled';
      break;
    }
    const deadline = t0 + timeoutMs;
    let ok = false;
    let rows = null;
    while (Date.now() < deadline) {
      const [pager, count, key] = await Promise.all([
        readPagerDeep(page),
        countSelectorDeep(page, selector),
        firstItemKeyDeep(page, selector),
      ]);
      if (
        pager &&
        pager.pageIndex === toPageIndex &&
        !pager.loading &&
        pager.rowsOnPage > 0 &&
        count === pager.rowsOnPage &&
        key !== beforeKey
      ) {
        ok = true;
        rows = count;
        break;
      }
      await page.waitForTimeout(20);
    }
    out.push({ toPageIndex, ok, ms: ok ? Date.now() - t0 : null, rows });
    if (!ok) {
      stopReason = 'timed-out';
      break;
    }
  }
  // The pager as it stood when the run stopped, so an early stop (for
  // example no Next on a short page) can be told apart from a short view.
  const stopPager = await readPagerDeep(page);
  if (stopReason === null && walkEndReason(stopPager) === 'next-unavailable-before-last-page') {
    // Every requested change completed, but the last one landed on a page
    // with Next disabled before the last page: still an early stop.
    stopReason = 'next-unavailable-before-last-page';
  }
  return { changes: out, stopReason: stopReason ?? 'completed', stopPager };
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
    // The project grid and list render one page of agents at a time:
    // populated means the first page rendered, and page changes are timed.
    paged: true,
  },
  {
    key: 'project-list',
    url: projectPath,
    viewMode: 'list',
    selector: '.agent-table-container tbody tr',
    isGraph: false,
    paged: true,
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
    // This view fetches the *unscoped* `/api/v1/agents`
    // (web/src/components/pages/agent-graph.ts:115), not a
    // project-filtered request -- the `project` query param only drives
    // client-side filtering (`:135`). The URL itself is correct (it is
    // what a user would navigate to); apiMatcherFor (lib.mjs) matches the
    // real, unscoped request this page sends.
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
  let pager = null;
  if (!navError) {
    if (scenario.paged) {
      populated = await waitForFirstPage(page, scenario.selector, expectedCount, populateTimeoutMs);
      pager = populated.pager;
      expectedCount = populated.expected;
    } else {
      populated = await waitForCount(page, scenario.selector, expectedCount, populateTimeoutMs);
    }
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

  // Stop watching the load-bearing request before any page change: Next
  // fetches hit the same endpoint and would otherwise overwrite the first
  // load's networkStatus, networkFailed and networkObservedAtMs.
  netWatch.detach();

  // Paged views: the page size and page count as the pager reports them,
  // and the Next-click latency for a few pages.
  let pagedFields = {};
  if (scenario.paged) {
    const pageSize = pager ? pager.pageSize : null;
    const pageTotal = pager && pager.total > 0 ? pager.total : null;
    const measured =
      isPopulated && pager
        ? await measurePageChanges(page, scenario.selector, pageChanges, populateTimeoutMs)
        : { changes: [], stopReason: 'not-populated', stopPager: null };
    pagedFields = {
      agentCount: seed.agentCount,
      pageSize,
      pageTotal,
      pageCount: pageCountFor(pageSize, pageTotal),
      pageChanges: measured.changes,
      // Why the run timed fewer than pageChangesPerRun changes, if it did:
      // completed, none-requested (--page-changes 0), no-next-page (the
      // last page was reached), next-unavailable-before-last-page (Next
      // disabled although the pager's total says more pages exist),
      // next-disabled, pager-busy, timed-out, no-pager or not-populated.
      pageChangesStopReason: measured.stopReason,
      pageChangesStopPager: measured.stopPager,
    };
  }

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
    // When the load-bearing request's outcome was observed, in elapsed ms
    // from navigation start -- so a WriteTimeout attribution is a
    // measurement, not an inference from the run's total wall-clock time.
    networkObservedAtMs: netWatch.state.atMs,
    consoleErrorCount: consoleErrors.length,
    consoleErrorsSample: consoleErrors.slice(0, 5),
    ...pagedFields,
  };
}

// Run 0 uses a fresh ("cold") context; runs 1..N-1 share one ("warm")
// context.
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

// getStatusBadgeLabelsBatch is getStatusBadgeLabelDeep's multi-id form: ONE
// page.evaluate call walks the document (and its shadow roots) once and
// looks up EVERY id in `agentIds` during that single traversal, instead of
// one full document walk per id. Used by the burst-settle poller below so
// N concurrently-unsettled agents cost one DOM traversal per tick rather
// than N.
async function getStatusBadgeLabelsBatch(page, agentIds) {
  const result = await page.evaluate((ids) => {
    const out = {};
    const remaining = new Set(ids);
    function walk(root) {
      for (const id of remaining) {
        const link = root.querySelector(`a[href="/agents/${id}"]`);
        if (link) {
          const card = link.closest('.agent-card');
          const badge = card ? card.querySelector('scion-status-badge') : null;
          out[id] = badge ? badge.getAttribute('label') : null;
        }
      }
      for (const id of Object.keys(out)) remaining.delete(id);
      if (remaining.size === 0) return;
      for (const el of root.querySelectorAll('*')) {
        if (el.shadowRoot) {
          walk(el.shadowRoot);
          if (remaining.size === 0) return;
        }
      }
    }
    walk(document);
    return out;
  }, agentIds);
  return new Map(Object.entries(result));
}

// createBurstSettlePoller returns a shared poller used by runBurstOnce to
// watch multiple agents' status badges for their respective target phases
// concurrently, with ONE page.evaluate (getStatusBadgeLabelsBatch) per tick
// covering every currently-pending agent, instead of each agent running its
// own independent page.evaluate + page.waitForTimeout(100) loop. 15
// concurrent per-agent loops each doing a full shadow-DOM traversal measured
// at ~68ms median (p90 98ms) per poll versus ~14ms for a single poller (see
// the comment on waitFor below) -- this collapses that back toward the
// single-poller cost regardless of --burst-count.
//
// Each caller's own POST-completion timestamp remains the anchor for its
// settle time (passed into waitFor as `deadline`, and the caller itself
// computes settleMs against its own postCompletedAt) -- batching the DOM
// read must not change which moment a given agent's settle time is measured
// from, only how many browser round-trips it costs to observe it.
function createBurstSettlePoller(page) {
  const pending = new Map(); // id -> target phase
  const resolvers = new Map(); // id -> resolve function
  let looping = false;

  async function tick() {
    if (pending.size === 0) return;
    const labels = await getStatusBadgeLabelsBatch(page, [...pending.keys()]);
    const settledNow = Date.now();
    for (const id of resolveBatchTickSettled(pending, labels)) {
      const resolve = resolvers.get(id);
      pending.delete(id);
      resolvers.delete(id);
      resolve(settledNow);
    }
  }

  async function loop() {
    if (looping) return;
    looping = true;
    try {
      while (pending.size > 0) {
        await tick();
        if (pending.size === 0) break;
        await page.waitForTimeout(100);
      }
    } finally {
      looping = false;
    }
  }

  return {
    // waitFor registers (id, target) with the shared poller and resolves
    // with the Date.now() at which its badge was observed to match, or null
    // if `deadlineMs` passes first. Registering immediately (re-)triggers
    // the shared loop, so a newly-added id is covered by the very next
    // tick -- including one already in flight, if it hasn't read the DOM
    // yet -- rather than waiting for its own freshly-started timer.
    waitFor(id, target, deadlineMs) {
      return new Promise((resolve) => {
        let done = false;
        const finish = (value) => {
          if (done) return;
          done = true;
          pending.delete(id);
          resolvers.delete(id);
          resolve(value);
        };
        pending.set(id, target);
        resolvers.set(id, finish);
        loop();
        const remaining = deadlineMs - Date.now();
        if (remaining <= 0) {
          finish(null);
        } else {
          setTimeout(() => finish(null), remaining);
        }
      });
    },
  };
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

async function runBurstOnce(page, runIndex, targetHistory, invalidateDueToPriorRestore) {
  // Targets must be agents whose cards are on screen: the grid is paged, so
  // an agent on another page has no badge to settle. Read the agent links
  // of the rendered cards, then their current state from the API.
  const visibleIds = await page.evaluate(() => {
    const ids = [];
    function walk(root) {
      for (const card of root.querySelectorAll('.agent-card')) {
        const a = card.querySelector('a[href^="/agents/"]');
        const m = a && a.getAttribute('href').match(/^\/agents\/([^/?#]+)$/);
        if (m && !ids.includes(m[1])) ids.push(m[1]);
      }
      for (const n of root.querySelectorAll('*')) if (n.shadowRoot) walk(n.shadowRoot);
    }
    walk(document);
    return ids;
  });
  const visibleAgents = (await Promise.all(visibleIds.map((id) => getAgent(id)))).filter(Boolean);
  const candidates = visibleAgents.filter((a) => a.phase !== 'suspended');
  const targets = candidates.slice(0, burstCount);
  if (targets.length < burstCount) {
    console.warn(`  warning: only ${targets.length}/${burstCount} non-suspended agents available`);
  }

  // Capture activity too, so the restore below can put agents back exactly
  // as they were, not just their phase.
  const preBurst = new Map(
    targets.map((a) => [a.id, { phase: a.phase, activity: a.activity || '' }])
  );
  // pickBurstTarget must exclude BOTH the agent's current (pre-burst)
  // phase AND the phase it was targeted with on its previous run -- the
  // pre-burst phase alone is constant across runs (it is restored every
  // time), so excluding only it does not stop run r and run r+1 from
  // requesting the same target. targetHistory persists across calls for
  // the lifetime of one scenario (one Map per runBurstScenario call) so
  // "previous run's target" means exactly that, not "previous idx-only
  // rotation slot".
  const targetPhase = new Map();
  targets.forEach((a, idx) => {
    const previousRunTarget = targetHistory.get(a.id) ?? null;
    const phase = pickBurstTarget(idx, runIndex, preBurst.get(a.id).phase, previousRunTarget);
    targetPhase.set(a.id, phase);
    targetHistory.set(a.id, phase);
  });

  // Make staleness impossible to miscount, independent of whether the
  // rotation or the restore-gating below are themselves correct. Read
  // every target's badge BEFORE posting anything; any agent
  // whose badge already shows the phase we are about to request cannot
  // have that match attributed to THIS run's POST (it could be a restore
  // that silently failed to reach the DOM, or any other stale state), so
  // it is excluded from this run's settle tracking rather than risking a
  // false "settled".
  const preFireLabels = new Map(
    await Promise.all(targets.map(async (a) => [a.id, await getStatusBadgeLabelDeep(page, a.id)]))
  );
  const preStaleIds = computePreStaleIds(
    targets.map((a) => a.id),
    preFireLabels,
    targetPhase
  );
  if (preStaleIds.size > 0) {
    console.warn(
      `  warning: run ${runIndex}: ${preStaleIds.size} agent(s) already show their target ` +
        `phase before posting; excluded from this run's settle count`
    );
  }

  // Poll each agent's badge independently, starting as soon as THAT
  // agent's own POST resolves -- not after Promise.all over every agent's
  // POST. An agent whose POST resolved early would otherwise only be
  // *first checked* once the slowest of the other 14 POSTs had also
  // returned, inflating its recorded settle time by up to the spread
  // between POST completions (0.24-1.40s observed in one capture -- the
  // same magnitude as the reported medians). Anchoring AND observing per
  // agent removes that inflation entirely, rather than merely measuring
  // around it. The DOM read itself is shared across agents (see
  // createBurstSettlePoller) -- what stays per-agent is the anchor
  // (postCompletedAt) each one's own settleMs is measured against, and the
  // per-agent deadline passed into waitFor.
  //
  // This is a SAMPLING INTERVAL, not a floor -- the first poll happens
  // immediately after the POST resolves, so values well under 170ms are
  // common (per-agent minimums of 11-12ms have been observed), and "floor"
  // would wrongly imply they can't occur. What is true: each sample can
  // LAG the true DOM update by up to one poll interval -- `page.
  // waitForTimeout(100)` plus a deep shadow-DOM badge read, which used to
  // measure at ~14ms with one poller but ~68ms median (p90 98ms) with
  // `--burst-count` (default 15) concurrent pollers sharing one page before
  // createBurstSettlePoller collapsed those into one shared evaluate call
  // per tick. Differences smaller than that combined interval (roughly
  // 170-200ms) cannot be used to rank hub speed, even though many
  // individual samples will themselves read well below it; see
  // measurements.md section 3 for this caveat applied to actual numbers.
  const burstStartedAt = Date.now();
  const settlePoller = createBurstSettlePoller(page);
  const perAgent = await Promise.all(
    targets.map(async (a) => {
      const id = a.id;
      const target = targetPhase.get(id);
      const postRes = await postAgentStatus(id, { phase: target });
      const postCompletedAt = Date.now();
      if (!postRes.ok) {
        return { id, ok: false, status: postRes.status, preStale: preStaleIds.has(id) };
      }
      if (preStaleIds.has(id)) {
        // Accepted server-side, but we cannot distinguish a real update
        // from the pre-existing stale badge -- do not poll, do not count
        // toward settled/timed-out either way.
        return { id, ok: true, status: postRes.status, preStale: true, postCompletedAt };
      }
      const deadline = postCompletedAt + settleTimeoutMs;
      const settledAt = await settlePoller.waitFor(id, target, deadline);
      return {
        id,
        ok: true,
        status: postRes.status,
        preStale: false,
        postCompletedAt,
        settled: settledAt != null,
        settleMs: settledAt != null ? settledAt - postCompletedAt : null,
      };
    })
  );
  // A single combined field would conflate two different things once
  // polling happens inside the same per-agent `Promise.all` as the POST:
  // "time to POST every agent's update" (the fan-out only, a few hundred
  // ms) versus the whole per-agent POST-plus-poll sequence (883-1330ms at
  // 25 agents). Recorded as two separate, correctly-named fields instead:
  // `postFanOutMs` (POST-only spread, comparable across captures) and
  // `burstWallClockMs` (the whole run including polling, for context only).
  const postCompletedAts = perAgent.map((r) => r.postCompletedAt).filter((v) => v != null);
  const postFanOutMs = postCompletedAts.length
    ? Math.max(...postCompletedAts) - burstStartedAt
    : null;
  const burstWallClockMs = Date.now() - burstStartedAt;

  const rejected = perAgent.filter((r) => !r.ok);
  const accepted = perAgent.filter((r) => r.ok);
  const tracked = accepted.filter((r) => !r.preStale);
  const settled = tracked.filter((r) => r.settled);
  const perAgentSettleMs = settled.map((r) => r.settleMs);
  const settleMM = minMax(perAgentSettleMs);
  const acceptedIds = accepted.map((r) => r.id);

  // Distinguish "the server never applied the update" from "the UI did not
  // settle": GET each targeted agent's server-side phase independently of
  // the DOM-badge check above.
  const serverAppliedCount = (
    await Promise.all(
      acceptedIds.map(async (id) => (await getAgent(id))?.phase === targetPhase.get(id))
    )
  ).filter(Boolean).length;

  // Restore every targeted agent to its pre-burst phase AND activity, then
  // WAIT for the restore to be confirmed in the DOM before returning. The
  // expected label must match what the UI actually renders
  // (web/src/shared/types.ts's getAgentDisplayStatus): a `running` agent
  // with a non-empty activity displays its activity, not its literal
  // phase, so comparing against the literal phase could never succeed for
  // those agents regardless of how long this waited.
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
      const pb = preBurst.get(id);
      const expected = displayStatusLabel(pb.phase, pb.activity);
      const result = await waitForBadgeValue(page, id, expected, settleTimeoutMs);
      return { id, ...result };
    })
  );
  const restoreSettledCount = restoreSettled.filter((r) => r.settled).length;
  const restoreSettleMs = restoreSettled
    .filter((r) => r.settled)
    .map((r) => r.atMs - restoreStartedAt);
  const restoreFullyConfirmed = restoreSettledCount === restoreAcceptedIds.length;

  return {
    runIndex,
    // The restore-wait is bounded, not blocking; the next run starts
    // regardless. What actually has an effect is this run's OWN
    // restoreFullyConfirmed result -- see runBurstScenario, which passes
    // it as the NEXT run's invalidateDueToPriorRestore. A run fired while
    // the previous run's restore was not confirmed in the DOM within that
    // bound cannot be trusted to have started from the expected pre-burst
    // state, so it is marked invalid rather than silently mixed into the
    // scenario's settle statistics.
    invalid: invalidateDueToPriorRestore === true,
    invalidReason: invalidateDueToPriorRestore
      ? 'previous run restore was not fully confirmed in the DOM before this run started'
      : null,
    requestedCount: targets.length,
    acceptedCount: acceptedIds.length,
    rejectedCount: rejected.length,
    rejectedSample: rejected.slice(0, 3),
    postFanOutMs,
    burstWallClockMs,
    preStaleExcludedCount: preStaleIds.size,
    trackedCount: tracked.length,
    settledCount: settled.length,
    serverAppliedCount,
    timedOut: settled.length < tracked.length,
    // Per-agent settle time (this agent's own badge update minus this SAME
    // agent's own POST completion, observed by a polling loop dedicated to
    // that agent, not started only after every other agent's POST also
    // returned).
    medianSettleMs: median(perAgentSettleMs),
    minSettleMs: settleMM.min,
    maxSettleMs: settleMM.max,
    restoreFailureCount: restoreFailures.length,
    restoreSettledCount,
    restoreTotalCount: restoreAcceptedIds.length,
    restoreFullyConfirmed,
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
  // The grid is paged: the burst only needs its first page on screen, and
  // its targets are taken from the agents shown there (see runBurstOnce).
  const populated = await waitForFirstPage(page, '.agent-card', seed.agentCount, populateTimeoutMs);
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

  // targetHistory persists per-agent-id across every run in this scenario
  // (pickBurstTarget needs the REAL previous target, not just an
  // idx-derived guess); priorRestoreConfirmed gates the NEXT run.
  const targetHistory = new Map();
  let priorRestoreConfirmed = true;
  const results = [];
  for (let i = 0; i < burstRuns; i++) {
    const invalidate = !priorRestoreConfirmed;
    const r = await runBurstOnce(page, i, targetHistory, invalidate);
    results.push(r);
    priorRestoreConfirmed = r.restoreFullyConfirmed;
    console.log(
      `  burst run ${i}${r.invalid ? ' [INVALID: ' + r.invalidReason + ']' : ''}: ` +
        `${r.acceptedCount}/${r.requestedCount} accepted ` +
        `(${r.preStaleExcludedCount} pre-stale excluded), ` +
        `${r.settledCount}/${r.trackedCount} settled (median ${r.medianSettleMs}ms), ` +
        `${r.serverAppliedCount}/${r.acceptedCount} server-applied, ` +
        `restore ${r.restoreSettledCount}/${r.restoreTotalCount} confirmed (timedOut=${r.timedOut})`
    );
  }
  await context.close();

  return { skipped: false, ...summarizeBurstScenario(results) };
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

  const hubScionVersion = await fetchHubScionVersion(hubBase);
  // Record load average at start/end, matching apibench's convention --
  // settle numbers are environment-sensitive (a lower-load single-hub run
  // measured roughly 4x lower settle times at the same agent counts), and
  // a report with no load figure gives a reader no way to tell environment
  // noise from a real difference.
  const [loadAvg1, loadAvg5, loadAvg15] = os.loadavg();
  const report = {
    generatedAt: new Date().toISOString(),
    harnessCommit: gitHeadSha(),
    harnessCommitDirty: gitIsDirty(),
    hubScionVersion,
    hubBaseUrl: hubBase,
    seedProjectId: seed.projectId,
    seedProjectSlug: seed.projectSlug,
    agentCount: seed.agentCount,
    runs,
    burstRuns,
    effectiveTimeouts: { navTimeoutMs, populateTimeoutMs, settleTimeoutMs, burstCount },
    pageChangesPerRun: pageChanges,
    notes,
    machine: { loadAvg1, loadAvg5, loadAvg15 },
    scenarios: {},
  };
  writeReportSoFar(report);

  try {
    if (burstOnly) {
      console.log('--burst-only set: skipping the four view scenarios');
    } else {
      for (const scenario of scenarios) {
        console.log(
          `running scenario ${scenario.key} (${runs} runs: 1 cold + ${runs - 1} warm)...`
        );
        const results = await runScenario(browser, scenario, seed.agentCount);
        report.scenarios[scenario.key] = {
          ...summarizeScenario(scenario, results),
          ...(scenario.paged ? { paged: true, ...summarizePageChanges(results) } : {}),
        };
        const s = report.scenarios[scenario.key];
        console.log(
          `  ${s.successCount}/${results.length} populated; outcomes=${JSON.stringify(s.outcomeCounts)}; ` +
            `median nav->populated: ${s.medianNavToPopulatedMs}ms (cold=${s.medianNavToPopulatedMsCold}ms, warm=${s.medianNavToPopulatedMsWarm}ms); ` +
            `median DOM count: ${s.medianDomElementCount} [${s.minDomElementCount}, ${s.maxDomElementCount}]`
        );
        if (scenario.paged) {
          console.log(
            `  paged: page size ${JSON.stringify(s.pageSize)}, pages ${JSON.stringify(s.pageCount)}; ` +
              `page changes ${s.pageChangeSuccessCount}/${s.pageChangeAttemptCount} completed, ` +
              `${s.pageWalkEarlyStopCount} walk(s) stopped before the last page, ` +
              `median ${s.medianPageChangeMs}ms [${s.minPageChangeMs}, ${s.maxPageChangeMs}]`
          );
        }
        writeReportSoFar(report);
      }
    }

    console.log(`running SSE burst-update responsiveness scenario (${burstRuns} runs)...`);
    report.liveUpdateBurst = await runBurstScenario(browser);
    if (report.liveUpdateBurst.skipped) {
      console.log(`  skipped: ${report.liveUpdateBurst.skipReason}`);
    } else {
      // fullySettledRunCount/fullyRestoredRunCount are counted over valid
      // runs only (see summarizeBurstScenario), so they are reported
      // against validRunCount, not runsAttempted -- dividing by
      // runsAttempted would make any invalid run read like a settle
      // failure even when every valid run settled 15/15. Invalid and
      // pre-stale counts are both surfaced explicitly rather than only
      // affecting the denominator silently.
      const b = report.liveUpdateBurst;
      console.log(
        `  median settle time: ${b.medianSettleMs}ms ` +
          `[${b.perAgentMinSettleMs}, ${b.perAgentMaxSettleMs}] true per-agent range ` +
          `(range of the ${b.medianOfRunMediansN} per-run medians themselves: ` +
          `[${b.runMedianMinMs}, ${b.runMedianMaxMs}]); ` +
          `${b.fullySettledRunCount}/${b.validRunCount} valid runs fully settled, ` +
          `${b.fullyRestoredRunCount}/${b.runsAttempted} fully restored, ` +
          `${b.invalidRunCount}/${b.runsAttempted} invalid (prior restore unconfirmed), ` +
          `${b.preStaleExcludedTotal} total pre-stale-excluded agent(s)`
      );
    }
    [report.machine.loadAvg1AtEnd, report.machine.loadAvg5AtEnd, report.machine.loadAvg15AtEnd] =
      os.loadavg();
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
