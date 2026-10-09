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
 * Browser counter budgets for the project agent page: grid, list and graph
 * views at 100 generated agents (fixture.mjs, see mock-api.ts).
 *
 * Per view: one warm-up load (not measured; it lets the dev server
 * transform and cache the modules), then LOADS measured loads, each in a
 * fresh browser context. A load is measured once the view has rendered its
 * agents (25 cards or rows on the first page of the grid and list, 100
 * graph nodes) and the deep DOM count has stopped changing.
 *
 * Counters:
 * - domElements: every element in the document, including inside shadow
 *   roots (the countAllDeep walk from large-project-bench.mjs). The
 *   fixture has fixed IDs and times and the page clock is pinned
 *   (FIXED_NOW), so this is exact for a given web build and fixture;
 *   every measured load must stay within the limit.
 * - longTasks: main-thread tasks over 50 ms (PerformanceObserver
 *   'longtask') from navigation until the measurement. This is the one
 *   host-sensitive counter here: it depends on runner CPU and load. The
 *   gate uses the median over the LOADS loads and a wide margin, so one
 *   noisy load cannot fail CI.
 *
 * Baselines were measured on BASELINE_COMMIT, Chromium headless shell,
 * one worker:
 * - domElements: identical on all 6 measured loads (2 runs of 3) per view.
 * - longTasks: the median of the same 6 loads, rounded up (grid 2-5, list
 *   2-5, graph 3-5 per load).
 * Limits:
 * - domElements: baseline + DOM_MARGIN elements. One extra element per
 *   rendered card or row adds 25 (grid, list) or 100 (graph) and fails.
 * - longTasks: baseline + max(3, 50% of baseline, rounded up). Eight added
 *   120 ms tasks at startup measured medians of 11-13 and fail.
 *
 * Updating a budget for an intended change: run
 *   npm run test:e2e:perf-budgets
 * read the "measured" lines, set the baseline below and say why in the
 * commit message (docs-site/src/content/docs/contributing/perf-tracing.md).
 */

import { expect, test, type Browser, type Page } from '@playwright/test';

import { loadFixture, setupBudgetMocks } from './mock-api.js';

const BASELINE_COMMIT = 'main 00d36ed';
const LOADS = 3;
const DOM_MARGIN = 10;
// The page's clock, a few minutes after the fixture's agent times, so
// relative times and any age-based rendering do not drift with the date.
const FIXED_NOW = new Date('2026-10-01T12:05:00Z');

interface ViewBudget {
  view: 'grid' | 'list' | 'graph';
  /** Rendered once this many elements match. */
  selector: string;
  expected: number;
  baseline: { domElements: number; longTasks: number };
}

const VIEWS: ViewBudget[] = [
  {
    view: 'grid',
    selector: '.agent-card',
    expected: 25,
    baseline: { domElements: 2439, longTasks: 4 },
  },
  {
    view: 'list',
    selector: '.agent-table-container tbody tr',
    expected: 25,
    baseline: { domElements: 2360, longTasks: 4 },
  },
  {
    view: 'graph',
    selector: '.node-wrapper',
    expected: 100,
    baseline: { domElements: 2934, longTasks: 5 },
  },
];

function limits(b: ViewBudget['baseline']): ViewBudget['baseline'] {
  return {
    domElements: b.domElements + DOM_MARGIN,
    longTasks: b.longTasks + Math.max(3, Math.ceil(b.longTasks / 2)),
  };
}

function median(xs: number[]): number {
  const s = [...xs].sort((a, b) => a - b);
  const m = Math.floor(s.length / 2);
  return s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2;
}

async function countDeep(page: Page, selector: string): Promise<number> {
  return page.evaluate((sel) => {
    function walk(root: Document | ShadowRoot): number {
      let n = root.querySelectorAll(sel).length;
      for (const el of root.querySelectorAll('*')) {
        if (el.shadowRoot) n += walk(el.shadowRoot);
      }
      return n;
    }
    return walk(document);
  }, selector);
}

const fixture = loadFixture();

interface LoadResult {
  domElements: number;
  longTasks: number;
  unexpected: string[];
}

async function measureLoad(browser: Browser, v: ViewBudget): Promise<LoadResult> {
  const context = await browser.newContext();
  try {
    const page = await context.newPage();
    await page.clock.setFixedTime(FIXED_NOW);
    const unexpected = await setupBudgetMocks(page, fixture);
    await page.addInitScript((view) => {
      localStorage.setItem('scion-view-project-agents', view);
      const w = window as unknown as { __budgetLongTasks: number };
      w.__budgetLongTasks = 0;
      new PerformanceObserver((list) => {
        w.__budgetLongTasks += list.getEntries().length;
      }).observe({ type: 'longtask', buffered: true });
    }, v.view);
    await page.goto(`/projects/${encodeURIComponent(fixture.projectId)}`);

    await expect
      .poll(() => countDeep(page, v.selector), {
        timeout: 30_000,
        message: `${v.view}: rendered items`,
      })
      .toBe(v.expected);

    // Settled: the deep element count is unchanged across two reads 500 ms
    // apart (Shoelace and Lit render their shadow roots asynchronously).
    let last = -1;
    let domElements = await countDeep(page, '*');
    for (let i = 0; i < 20 && domElements !== last; i++) {
      await page.waitForTimeout(500);
      last = domElements;
      domElements = await countDeep(page, '*');
    }
    expect(domElements, `${v.view}: DOM count did not settle`).toBe(last);
    const longTasks = await page.evaluate(
      () => (window as unknown as { __budgetLongTasks: number }).__budgetLongTasks
    );
    return { domElements, longTasks, unexpected };
  } finally {
    await context.close();
  }
}

for (const v of VIEWS) {
  test(`project ${v.view} view stays within its DOM and long-task budget`, async ({ browser }) => {
    await measureLoad(browser, v); // warm-up, not measured
    const loads: LoadResult[] = [];
    for (let i = 0; i < LOADS; i++) loads.push(await measureLoad(browser, v));

    const dom = loads.map((l) => l.domElements);
    const lt = loads.map((l) => l.longTasks);
    const limit = limits(v.baseline);
    console.log(
      `measured ${v.view}: domElements=${dom.join(',')} longTasks=${lt.join(',')} (median ${median(lt)}); ` +
        `baseline ${v.baseline.domElements}/${v.baseline.longTasks} on ${BASELINE_COMMIT}; ` +
        `limit ${limit.domElements}/${limit.longTasks}`
    );

    expect(
      [...new Set(loads.flatMap((l) => l.unexpected))],
      'requests with no generated response in fixture.mjs (add them; see perf-tracing.md)'
    ).toEqual([]);
    for (const n of dom) {
      expect(
        n,
        `${v.view}: DOM elements over budget (baseline ${v.baseline.domElements})`
      ).toBeLessThanOrEqual(limit.domElements);
    }
    expect(
      median(lt),
      `${v.view}: median long tasks over budget (baseline ${v.baseline.longTasks})`
    ).toBeLessThanOrEqual(limit.longTasks);
  });
}
