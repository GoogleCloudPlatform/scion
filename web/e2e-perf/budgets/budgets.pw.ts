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
 * transform and cache the modules), then LOADS measured loads, then one
 * load with the CPU slowed SLOW_CPU_RATE times, each in a fresh browser
 * context. A load is measured once the view has written its readiness mark
 * (READY_MARK, turned on in the mocked page data), has rendered its agents
 * (25 cards or rows on the first page of the grid and list, 100 graph
 * nodes), its lazily mounted Files section has been scrolled into view
 * (otherwise whether it mounts depends on load speed), and its deep DOM
 * count is the same on three reads 500 ms apart. The page's Date is pinned
 * to FIXED_NOW (its timers and animation frames are left alone; the
 * readiness marks need them).
 * The slowed load must settle on the same DOM count as the normal ones, so
 * the gate cannot depend on how fast the runner is.
 *
 * Counters:
 * - domElements: every element in the document, including inside shadow
 *   roots (the countAllDeep walk from large-project-bench.mjs). The
 *   fixture has fixed IDs and times and the page's Date is pinned
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
 * - domElements: identical on all loads per view, slowed ones included
 *   (12 loads on main 7049f53, 9 more on main 48a6715).
 * - longTasks: the median of the 6 normal loads of 2 runs on main
 *   48a6715, rounded up (grid 3-7, list 3-7, graph 4-9 per load),
 *   so that no run median (up to 7 in every view) sits within 1 of its
 *   limit.
 * Limits:
 * - domElements: baseline + DOM_MARGIN elements. One extra element per
 *   rendered card or row adds 25 (grid, list) or 100 (graph) and fails.
 * - longTasks: baseline + max(3, 50% of baseline, rounded up). Eight added
 *   120 ms tasks at startup measured medians of 13-15 and fail.
 *
 * Updating a budget for an intended change: run
 *   npm run test:e2e:perf-budgets
 * read the "measured" lines, set the baseline below and say why in the
 * commit message (docs-site/src/content/docs/contributing/perf-tracing.md).
 */

import { expect, test, type Browser, type Page } from '@playwright/test';

import { loadFixture, setupBudgetMocks } from './mock-api.js';

const BASELINE_COMMIT = 'main 48a6715';
const LOADS = 3;
const DOM_MARGIN = 10;
// The page's Date, a few minutes after the fixture's agent times, so
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
    baseline: { domElements: 2448, longTasks: 7 },
  },
  {
    view: 'list',
    selector: '.agent-table-container tbody tr',
    expected: 25,
    baseline: { domElements: 2369, longTasks: 6 },
  },
  {
    view: 'graph',
    selector: '.node-wrapper',
    expected: 100,
    baseline: { domElements: 2971, longTasks: 7 },
  },
];

// Long-task margin: max(3, 50% of baseline). It is wide on purpose: long
// tasks are the one host-sensitive counter, and GitHub-runner variance is
// not yet measured, so revisit the margin (and the baselines) against the
// first 3 CI runs of this job at PR time. Observed spread per view, normal
// loads only, 3 runs of 3 loads each:
//   main 48a6715: grid per load 2-7, run medians 3-7; list 3-7, medians
//                 5-7; graph 4-9, medians 5-7.
//   main 7049f53: grid per load 3-7, run medians 4-6; list 4-8, medians
//                 5-6; graph 3-7, medians 4-6.
//   main ece808d: one run; run medians grid 3, list 4, graph 5.
// Across all these runs the run medians were about 3 to 7 in every view;
// that is the basis for the limits 11 (grid), 9 (list) and 11 (graph).
// The DOM margin (+10) has no such spread: DOM counts were identical on
// every load, slowed loads included.
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

// The readiness mark each view writes once it has rendered its first page
// (grid, list) or laid out and fitted the graph (web/src/client/readiness-marks.ts).
const READY_MARK: Record<ViewBudget['view'], string> = {
  grid: 'scion:ready:rows-grid',
  list: 'scion:ready:rows-list',
  graph: 'scion:ready:graph',
};

// CPU slowdown for the timing check load (Chrome DevTools throttling).
const SLOW_CPU_RATE = 4;

async function measureLoad(browser: Browser, v: ViewBudget, slow = false): Promise<LoadResult> {
  const context = await browser.newContext();
  try {
    const page = await context.newPage();
    // Pin the page's Date (not its timers or animation frames, which the
    // readiness marks need) to FIXED_NOW, then let it run on from there.
    await page.addInitScript((fixedMs) => {
      const RealDate = Date;
      const offset = fixedMs - RealDate.now();
      // Must be called with new: plain Date() (a string) would throw; web/src has no such calls.
      class PinnedDate extends RealDate {
        constructor(...args: unknown[]) {
          if (args.length === 0) super(RealDate.now() + offset);
          else super(...(args as ConstructorParameters<typeof Date>));
        }
        static now(): number {
          return RealDate.now() + offset;
        }
      }
      (window as unknown as { Date: DateConstructor }).Date = PinnedDate as DateConstructor;
    }, FIXED_NOW.getTime());
    const unexpected = await setupBudgetMocks(page, fixture);
    await page.addInitScript((view) => {
      localStorage.setItem('scion-view-project-agents', view);
      const w = window as unknown as { __budgetLongTasks: number };
      w.__budgetLongTasks = 0;
      new PerformanceObserver((list) => {
        w.__budgetLongTasks += list.getEntries().length;
      }).observe({ type: 'longtask', buffered: true });
    }, v.view);
    if (slow) {
      const cdp = await context.newCDPSession(page);
      await cdp.send('Emulation.setCPUThrottlingRate', { rate: SLOW_CPU_RATE });
    }
    await page.goto(`/projects/${encodeURIComponent(fixture.projectId)}`);

    // Settled on a defined signal, not a delay: the view's readiness mark,
    // then the rendered items, then a deep element count that is unchanged
    // across three reads 500 ms apart (Shoelace and Lit render their
    // shadow roots asynchronously).
    await expect
      .poll(
        () =>
          page.evaluate((name) => performance.getEntriesByName(name).length, READY_MARK[v.view]),
        { timeout: 60_000, message: `${v.view}: readiness mark ${READY_MARK[v.view]}` }
      )
      .toBeGreaterThan(0);
    await expect
      .poll(() => countDeep(page, v.selector), {
        timeout: 30_000,
        message: `${v.view}: rendered items`,
      })
      .toBe(v.expected);
    // The Files section mounts lazily once its placeholder nears the
    // viewport, and stays mounted. Whether an intermediate layout brought it
    // near depends on load speed, so reveal it on every load: the counted
    // page is then the same whether the load was fast or slow.
    const placeholder = await page.evaluateHandle(() => {
      const find = (root: Document | ShadowRoot): Element | null => {
        const el = root.querySelector('.files-section-placeholder');
        if (el) return el;
        for (const n of root.querySelectorAll('*')) {
          const f = n.shadowRoot ? find(n.shadowRoot) : null;
          if (f) return f;
        }
        return null;
      };
      return find(document);
    });
    const hadPlaceholder = await placeholder.evaluate((el) => {
      if (!el) return false;
      el.scrollIntoView({ block: 'center' });
      return true;
    });
    if (hadPlaceholder) {
      await expect
        .poll(() => countDeep(page, '.files-section-placeholder'), {
          timeout: 30_000,
          message: `${v.view}: Files section revealed`,
        })
        .toBe(0);
    }
    // Mounted either way (with no placeholder, a slowed load had already
    // revealed it), so a renamed placeholder class cannot silently skip
    // this step. Eager mounting below the fold is guarded by the
    // project-files-tabs e2e suite, not by this gate.
    await expect
      .poll(() => countDeep(page, 'scion-file-browser'), {
        timeout: 30_000,
        message: `${v.view}: Files section not mounted (placeholder class renamed?)`,
      })
      .toBeGreaterThanOrEqual(1);
    const reads = [await countDeep(page, '*')];
    for (let i = 0; i < 40; i++) {
      const n = reads.length;
      if (n >= 3 && reads[n - 1] === reads[n - 2] && reads[n - 2] === reads[n - 3]) break;
      await page.waitForTimeout(500);
      reads.push(await countDeep(page, '*'));
    }
    const domElements = reads[reads.length - 1];
    expect(reads.slice(-3), `${v.view}: DOM count did not settle`).toEqual([
      domElements,
      domElements,
      domElements,
    ]);
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
    // Timing check: the same load with the CPU slowed down must settle on
    // the same DOM. Not counted for long tasks.
    const slowLoad = await measureLoad(browser, v, true);

    const dom = loads.map((l) => l.domElements);
    const lt = loads.map((l) => l.longTasks);
    const limit = limits(v.baseline);
    console.info(
      `measured ${v.view}: domElements=${dom.join(',')} (slowed ${slowLoad.domElements}) ` +
        `longTasks=${lt.join(',')} (median ${median(lt)}); ` +
        `baseline ${v.baseline.domElements}/${v.baseline.longTasks} on ${BASELINE_COMMIT}; ` +
        `limit ${limit.domElements}/${limit.longTasks}`
    );

    expect(
      [...new Set([...loads, slowLoad].flatMap((l) => l.unexpected))],
      'requests with no generated response in fixture.mjs (add them; see perf-tracing.md)'
    ).toEqual([]);
    expect(
      [...dom, slowLoad.domElements],
      `${v.view}: the DOM count must not depend on load speed (normal loads, then a ${SLOW_CPU_RATE}x slowed load)`
    ).toEqual(Array(LOADS + 1).fill(dom[0]));
    for (const n of dom) {
      expect(
        n,
        `${v.view}: DOM elements over budget (baseline ${v.baseline.domElements})`
      ).toBeLessThanOrEqual(limit.domElements);
    }
    // Non-fatal: a count well under its baseline means the baseline is
    // stale, and the old headroom would let a later regression through.
    if (dom[0] < v.baseline.domElements - DOM_MARGIN) {
      console.warn(
        `note: ${v.view} DOM elements ${dom[0]} are below baseline ${v.baseline.domElements} by more than the margin; consider lowering the baseline`
      );
    }
    const ltMargin = limit.longTasks - v.baseline.longTasks;
    if (median(lt) < v.baseline.longTasks - ltMargin) {
      console.warn(
        `note: ${v.view} median long tasks ${median(lt)} are below baseline ${v.baseline.longTasks} by more than the margin; consider lowering the baseline`
      );
    }
    expect(
      median(lt),
      `${v.view}: median long tasks over budget (baseline ${v.baseline.longTasks})`
    ).toBeLessThanOrEqual(limit.longTasks);
  });
}
