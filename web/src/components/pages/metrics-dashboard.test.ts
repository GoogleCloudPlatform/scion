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
 * The page asks the hub to bucket the dashboard series by calendar day in
 * the viewer's effective display zone, and the day-bucket labels name the
 * zone the hub reports back (ptone/scion#3370; the "(UTC)" labels came from
 * tz-refactor task 7, ptone/scion#2500). These tests pin the `tz` request
 * parameter, the chart x-axis title and the per-day chart headings on every
 * day-bucketed tab (sessions, model-calls, tokens).
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { DISPLAY_TIMEZONE_CHANGED_EVENT, setPreferredTimeZone } from '../../utils/time.js';

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
  activeAgents: [
    { timestamp: '2026-03-11', value: 1 },
    { timestamp: '2026-03-10', value: 1 },
  ],
};

const GROUPED = [
  {
    label: 'm1',
    points: [
      { timestamp: '2026-03-11', value: 4 },
      { timestamp: '2026-03-10', value: 3 },
    ],
  },
];

const MODEL_CALLS = { periodDays: 7, byModel: GROUPED, byHarness: GROUPED };
const TOKENS = { periodDays: 7, input: GROUPED, output: GROUPED };

const VIEW_BODIES: Record<string, unknown> = {
  sessions: SESSIONS,
  'model-calls': MODEL_CALLS,
  tokens: TOKENS,
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

/** The `tz` parameter of every metrics request the page made, in order. */
const requestedZones: (string | null)[] = [];

/**
 * The zone the fake hub reports as `timeZone`; `undefined` omits the field
 * (an older hub), `'echo'` reports whatever the page asked for.
 */
let reportedZone: string | undefined = 'echo';

async function mountOnTab(tab: string): Promise<MetricsPage> {
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string | URL | Request) => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.href : url.url;
      const params = new URL(path, 'http://localhost').searchParams;
      const view = params.get('view') ?? '';
      requestedZones.push(params.get('tz'));
      const zone = reportedZone === 'echo' ? (params.get('tz') ?? undefined) : reportedZone;
      const body = { ...((VIEW_BODIES[view] ?? SUMMARY) as object), timeZone: zone };
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
  el.activeTab = tab;
  await el.loadView(tab);
  await settle(el);
  return el;
}

/** Makes the browser report `zone` as its resolved time zone. */
function stubBrowserZone(zone: string): void {
  const original = Intl.DateTimeFormat.prototype.resolvedOptions;
  vi.spyOn(Intl.DateTimeFormat.prototype, 'resolvedOptions').mockImplementation(function (
    this: Intl.DateTimeFormat
  ) {
    return { ...original.call(this), timeZone: zone };
  });
}

function headings(el: MetricsPage): (string | undefined)[] {
  return [...(el.shadowRoot?.querySelectorAll('.chart-section-title') ?? [])].map((h) =>
    h.textContent?.trim()
  );
}

const TABS = [
  { tab: 'sessions', titles: ['Daily Sessions', 'Active Agents per Day'] },
  { tab: 'model-calls', titles: ['Daily API Calls by Model', 'Daily API Calls by Harness'] },
  { tab: 'tokens', titles: ['Daily Input Tokens by Model', 'Daily Output Tokens by Model'] },
];

describe('scion-page-metrics — day-bucket zone', () => {
  let element: MetricsPage | null = null;

  beforeAll(async () => {
    await import('./metrics-dashboard.js');
  }, 60_000);

  afterEach(() => {
    element?.remove();
    element = null;
    chartConfigs.length = 0;
    requestedZones.length = 0;
    reportedZone = 'echo';
    setPreferredTimeZone('');
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('sends the browser zone as tz when no display-zone preference is set', async () => {
    stubBrowserZone('America/Chicago');
    element = await mountOnTab('sessions');
    expect(requestedZones.length).toBeGreaterThan(0);
    expect(new Set(requestedZones)).toEqual(new Set(['America/Chicago']));
  });

  it('sends the display-zone preference as tz when one is set', async () => {
    stubBrowserZone('America/Chicago');
    setPreferredTimeZone('Asia/Kathmandu');
    element = await mountOnTab('sessions');
    expect(new Set(requestedZones)).toEqual(new Set(['Asia/Kathmandu']));
  });

  it.each(TABS)('labels the $tab charts with the zone the hub reports', async ({ tab, titles }) => {
    stubBrowserZone('America/Chicago');
    element = await mountOnTab(tab);

    expect(headings(element)).toEqual(titles.map((t) => `${t} (America/Chicago)`));
    expect(chartConfigs).toHaveLength(2);
    for (const config of chartConfigs) {
      expect(config.options.scales.x.title).toMatchObject({
        display: true,
        text: 'Day (America/Chicago)',
      });
      expect(config.data.labels).toEqual(['2026-03-10', '2026-03-11']);
    }
  });

  it('labels with the reported zone, not the requested one, when the hub falls back to UTC', async () => {
    stubBrowserZone('America/Chicago');
    reportedZone = 'UTC';
    element = await mountOnTab('sessions');
    expect(requestedZones).toContain('America/Chicago');
    expect(headings(element)).toEqual(['Daily Sessions (UTC)', 'Active Agents per Day (UTC)']);
    for (const config of chartConfigs) {
      expect(config.options.scales.x.title.text).toBe('Day (UTC)');
    }
  });

  it('labels UTC when an older hub omits timeZone', async () => {
    stubBrowserZone('America/Chicago');
    reportedZone = undefined;
    element = await mountOnTab('tokens');
    expect(headings(element)).toEqual([
      'Daily Input Tokens by Model (UTC)',
      'Daily Output Tokens by Model (UTC)',
    ]);
  });

  it('refetches with the new zone when the display-zone preference changes', async () => {
    element = await mountOnTab('model-calls');
    expect(requestedZones.at(-1)).toBe('UTC');
    requestedZones.length = 0;

    setPreferredTimeZone('Asia/Kathmandu');
    await settle(element);

    expect(requestedZones).toEqual(['Asia/Kathmandu']);
    expect(headings(element)).toEqual([
      'Daily API Calls by Model (Asia/Kathmandu)',
      'Daily API Calls by Harness (Asia/Kathmandu)',
    ]);
  });

  it('stops listening for zone changes once disconnected', async () => {
    element = await mountOnTab('sessions');
    element.remove();
    requestedZones.length = 0;
    window.dispatchEvent(new Event(DISPLAY_TIMEZONE_CHANGED_EVENT));
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(requestedZones).toEqual([]);
    element = null;
  });
});
