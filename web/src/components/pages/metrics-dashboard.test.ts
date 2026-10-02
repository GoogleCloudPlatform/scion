/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/**
 * The hub buckets dashboard series by UTC calendar day, so the day-bucket
 * labels must say "(UTC)" (tz-refactor task 7, ptone/scion#2500). These
 * tests pin the chart x-axis title and the per-day chart headings.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

interface ChartConfig {
  type: string;
  data: { labels: string[] };
  options: { scales: { x: { title: { display: boolean; text: string } } } };
}

const chartConfigs: ChartConfig[] = [];

vi.mock('chart.js', () => {
  class Chart {
    static register(): void {}
    config: ChartConfig;
    data: ChartConfig['data'];
    constructor(_canvas: unknown, config: ChartConfig) {
      this.config = config;
      this.data = config.data;
      chartConfigs.push(config);
    }
    update(): void {}
    destroy(): void {}
  }
  return { Chart, registerables: [] };
});

const SUMMARY = {
  periodDays: 7,
  totalSessions: 3,
  totalApiCalls: 0,
  totalTokens: 0,
  uniqueAgents: 1,
};

const SESSIONS = {
  periodDays: 7,
  dailyCounts: [
    { timestamp: '2026-03-11', value: 2 },
    { timestamp: '2026-03-10', value: 1 },
  ],
  activeAgents: [{ timestamp: '2026-03-10', value: 1 }],
};

type MetricsPage = HTMLElement & {
  updateComplete: Promise<boolean>;
  activeTab: string;
  loadView(view: string): Promise<void>;
};

async function settle(el: MetricsPage): Promise<void> {
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 20));
  await el.updateComplete;
  // renderChart defers to requestAnimationFrame.
  await new Promise((resolve) => requestAnimationFrame(() => resolve(undefined)));
}

async function mountOnSessionsTab(): Promise<MetricsPage> {
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string | URL | Request) => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.href : url.url;
      const body = path.includes('view=sessions') ? SESSIONS : SUMMARY;
      return Promise.resolve(
        new Response(JSON.stringify(body), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    })
  );
  const el = document.createElement('scion-page-metrics') as MetricsPage;
  document.body.appendChild(el);
  await settle(el);
  el.activeTab = 'sessions';
  await el.loadView('sessions');
  await settle(el);
  return el;
}

describe('scion-page-metrics — UTC day-bucket labels', () => {
  let element: MetricsPage | null = null;
  let mod: typeof import('./metrics-dashboard.js');

  beforeAll(async () => {
    mod = await import('./metrics-dashboard.js');
  }, 60_000);

  afterEach(() => {
    element?.remove();
    element = null;
    chartConfigs.length = 0;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('exports the UTC day axis title', () => {
    expect(mod.DAY_BUCKET_AXIS_TITLE).toBe('Day (UTC)');
  });

  it('renders the sessions charts with UTC headings, UTC axis title and the hub date labels', async () => {
    element = await mountOnSessionsTab();

    const headings = [...(element.shadowRoot?.querySelectorAll('.chart-section-title') ?? [])].map(
      (h) => h.textContent?.trim()
    );
    expect(headings).toEqual(['Daily Sessions (UTC)', 'Active Agents per Day (UTC)']);

    expect(chartConfigs).toHaveLength(2);
    for (const config of chartConfigs) {
      expect(config.options.scales.x.title).toMatchObject({ display: true, text: 'Day (UTC)' });
    }
    // The tick labels are the hub's UTC day keys, unchanged by the viewer's zone.
    expect(chartConfigs[0].data.labels).toEqual(['2026-03-10', '2026-03-11']);
  });
});
