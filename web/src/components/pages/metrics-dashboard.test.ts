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
 * The hub buckets dashboard series by calendar day in the zone the page asks
 * for (UTC by default) and echoes that zone, and the day-bucket labels must
 * name it. These tests pin the chart x-axis title and the per-day chart
 * headings on every day-bucketed tab (sessions, model-calls, tokens).
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { setPreferredTimeZone } from '../../utils/time.js';

// The labels follow the browser's time zone; pin it to UTC so these
// assertions don't depend on the machine running the tests.
function pinBrowserTimeZone(timeZone: string): void {
  vi.spyOn(Intl.DateTimeFormat.prototype, 'resolvedOptions').mockReturnValue({
    timeZone,
  } as Intl.ResolvedDateTimeFormatOptions);
}

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

function requestParams(url: string | URL | Request): URLSearchParams {
  const path = typeof url === 'string' ? url : url instanceof URL ? url.href : url.url;
  return new URL(path, 'http://localhost').searchParams;
}

/** Response body for a view, echoing the bucketing zone like the hub does. */
function viewBody(params: URLSearchParams, acceptedZone?: string): unknown {
  const view = params.get('view') ?? '';
  const body = VIEW_BODIES[view];
  if (!body) return SUMMARY;
  return { ...body, zone: acceptedZone ?? params.get('tz') ?? 'UTC' };
}

async function mountOnTab(tab: string, acceptedZone?: string): Promise<MetricsPage> {
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string | URL | Request) => {
      const body = viewBody(requestParams(url), acceptedZone);
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

describe('scion-page-metrics — default (UTC) day-bucket labels', () => {
  beforeEach(() => pinBrowserTimeZone('UTC'));

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

  it('titles the day axis in UTC when no zone resolves', () => {
    expect(mod.dayAxisTitle(undefined)).toBe('Day (UTC)');
  });

  it.each([
    { tab: 'sessions', headings: ['Daily Sessions (UTC)', 'Active Agents per Day (UTC)'] },
    {
      tab: 'model-calls',
      headings: ['Daily API Calls by Model (UTC)', 'Daily API Calls by Harness (UTC)'],
    },
    {
      tab: 'tokens',
      headings: ['Daily Input Tokens by Model (UTC)', 'Daily Output Tokens by Model (UTC)'],
    },
  ])(
    'renders the $tab charts with UTC headings, UTC axis title and the hub date labels',
    async ({ tab, headings }) => {
      element = await mountOnTab(tab);

      const rendered = [
        ...(element.shadowRoot?.querySelectorAll('.chart-section-title') ?? []),
      ].map((h) => h.textContent?.trim());
      expect(rendered).toEqual(headings);

      expect(chartConfigs).toHaveLength(2);
      for (const config of chartConfigs) {
        expect(config.options.scales.x.title).toMatchObject({ display: true, text: 'Day (UTC)' });
        // The tick labels are the hub's UTC day keys, unchanged by the viewer's zone.
        expect(config.data.labels).toEqual(['2026-03-10', '2026-03-11']);
      }
    }
  );
});

describe('scion-page-metrics — viewer time zone', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    setPreferredTimeZone('');
  });

  it('labels day buckets with the zone they are in', async () => {
    const mod = await import('./metrics-dashboard.js');
    expect(mod.dayZoneLabel(undefined)).toBe('UTC');
    expect(mod.dayZoneLabel('Etc/UTC')).toBe('UTC');
    expect(mod.dayZoneLabel('America/Chicago')).toBe('America/Chicago');
    expect(mod.dayAxisTitle('America/Chicago')).toBe('Day (America/Chicago)');
  });

  it('asks the hub to bucket days in the browser time zone when no preference is set', async () => {
    const mod = await import('./metrics-dashboard.js');
    pinBrowserTimeZone('America/Chicago');
    expect(mod.displayTimeZone()).toBe('America/Chicago');

    const fetchMock = vi.fn(() =>
      Promise.resolve(
        new Response(JSON.stringify(SUMMARY), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      )
    );
    vi.stubGlobal('fetch', fetchMock);

    const element = document.createElement('scion-page-metrics');
    document.body.appendChild(element);
    await new Promise((resolve) => setTimeout(resolve, 20));

    const urls = fetchMock.mock.calls.map((call) => String((call as unknown[])[0]));
    const dashboard = urls.find((u) => u.includes('view=summary'));
    expect(dashboard).toBeDefined();
    const params = new URL(dashboard!, 'http://localhost').searchParams;
    expect(params.get('tz')).toBe('America/Chicago');
    expect(params.get('period')).toBe('7');
  });

  async function dashboardRequests(): Promise<{ urls: () => string[] }> {
    const fetchMock = vi.fn(() =>
      Promise.resolve(
        new Response(JSON.stringify(SUMMARY), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      )
    );
    vi.stubGlobal('fetch', fetchMock);
    const element = document.createElement('scion-page-metrics');
    document.body.appendChild(element);
    await new Promise((resolve) => setTimeout(resolve, 20));
    return {
      urls: () =>
        fetchMock.mock.calls
          .map((call) => String((call as unknown[])[0]))
          .filter((u) => u.includes('view=summary')),
    };
  }

  it('uses the Display timezone preference over the browser zone', async () => {
    const mod = await import('./metrics-dashboard.js');
    pinBrowserTimeZone('America/Chicago');
    setPreferredTimeZone('Asia/Tokyo');
    expect(mod.displayTimeZone()).toBe('Asia/Tokyo');

    const { urls } = await dashboardRequests();
    const params = new URL(urls()[0], 'http://localhost').searchParams;
    expect(params.get('tz')).toBe('Asia/Tokyo');
  });

  it('names the Display timezone, not the browser zone, in headings, axis and request', async () => {
    pinBrowserTimeZone('America/Chicago');
    setPreferredTimeZone('Asia/Tokyo');
    const element = await mountOnTab('sessions');

    const rendered = [...(element.shadowRoot?.querySelectorAll('.chart-section-title') ?? [])].map(
      (h) => h.textContent?.trim()
    );
    expect(rendered).toEqual(['Daily Sessions (Asia/Tokyo)', 'Active Agents per Day (Asia/Tokyo)']);
    for (const config of chartConfigs.slice(-2)) {
      expect(config.options.scales.x.title).toMatchObject({ text: 'Day (Asia/Tokyo)' });
    }

    const fetchMock = globalThis.fetch as unknown as { mock: { calls: unknown[][] } };
    const sessionsUrl = fetchMock.mock.calls
      .map((call) => String(call[0]))
      .find((u) => u.includes('view=sessions'));
    expect(sessionsUrl).toBeDefined();
    expect(new URL(sessionsUrl!, 'http://localhost').searchParams.get('tz')).toBe('Asia/Tokyo');
    element.remove();
  });

  it('labels days UTC when the hub fell back to UTC for the requested zone', async () => {
    setPreferredTimeZone('Asia/Tokyo');
    const element = await mountOnTab('sessions', 'UTC');

    const rendered = [...(element.shadowRoot?.querySelectorAll('.chart-section-title') ?? [])].map(
      (h) => h.textContent?.trim()
    );
    expect(rendered).toEqual(['Daily Sessions (UTC)', 'Active Agents per Day (UTC)']);
    for (const config of chartConfigs.slice(-2)) {
      expect(config.options.scales.x.title).toMatchObject({ text: 'Day (UTC)' });
    }
    element.remove();
  });

  it('ignores a stale response that lands after a Display timezone change', async () => {
    setPreferredTimeZone('Asia/Tokyo');
    // Hold each sessions response until the test releases it.
    const pending: { tz: string | null; release: () => void }[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string | URL | Request) => {
        const params = requestParams(url);
        const respond = () =>
          new Response(JSON.stringify(viewBody(params)), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          });
        if (params.get('view') !== 'sessions') return Promise.resolve(respond());
        return new Promise<Response>((resolve) => {
          pending.push({ tz: params.get('tz'), release: () => resolve(respond()) });
        });
      })
    );
    const element = document.createElement('scion-page-metrics') as MetricsPage;
    document.body.appendChild(element);
    await settle(element);
    element.activeTab = 'sessions';
    const tokyoLoad = element.loadView('sessions');
    await settle(element);

    // The zone changes while the Tokyo request is in flight.
    setPreferredTimeZone('Europe/London');
    await settle(element);
    expect(pending.map((p) => p.tz)).toEqual(['Asia/Tokyo', 'Europe/London']);

    // The London response lands first, then the stale Tokyo one.
    pending[1].release();
    await settle(element);
    pending[0].release();
    await tokyoLoad;
    await settle(element);

    const rendered = [...(element.shadowRoot?.querySelectorAll('.chart-section-title') ?? [])].map(
      (h) => h.textContent?.trim()
    );
    expect(rendered).toEqual([
      'Daily Sessions (Europe/London)',
      'Active Agents per Day (Europe/London)',
    ]);
    expect(chartConfigs.at(-1)?.options.scales.x.title).toMatchObject({
      text: 'Day (Europe/London)',
    });
    // The applied data is the London response, not the stale Tokyo one.
    expect((element as unknown as { sessions: { zone?: string } }).sessions.zone).toBe(
      'Europe/London'
    );
    element.remove();
  });

  it('reloads the days when the Display timezone changes', async () => {
    pinBrowserTimeZone('America/Chicago');
    const { urls } = await dashboardRequests();
    expect(urls()).toHaveLength(1);

    setPreferredTimeZone('Europe/London');
    await new Promise((resolve) => setTimeout(resolve, 20));

    const all = urls();
    expect(all).toHaveLength(2);
    expect(new URL(all[1], 'http://localhost').searchParams.get('tz')).toBe('Europe/London');
  });
});
