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
import {
  resetServerFlagStateForTests,
  setFeatureFlag,
  TOKENOMICS_USAGE_LEDGER_FLAG,
} from '../../utils/feature-flags.js';

interface ChartDataset {
  label: string;
  data: (number | null)[];
  spanGaps?: boolean;
  pointStyle?: string | string[];
  pointRadius?: number | number[];
  partialPoints?: boolean[];
}

interface ChartConfig {
  type: string;
  data: { labels: string[]; datasets: ChartDataset[] };
  options: {
    scales: { x: { title: { display: boolean; text: string } } };
    plugins?: {
      tooltip?: {
        callbacks?: {
          label?: (ctx: {
            dataIndex: number;
            dataset: { partialPoints?: boolean[] };
            parsed: { y: number | null };
          }) => string;
        };
      };
    };
  };
}

const chartConfigs: ChartConfig[] = [];

vi.mock('chart.js', () => {
  class Chart {
    static register(): void {}
    config: ChartConfig;
    data: ChartConfig['data'];
    options: ChartConfig['options'];
    constructor(_canvas: unknown, config: ChartConfig) {
      this.config = config;
      this.data = config.data;
      this.options = config.options;
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

/** Per-view overrides of `reportedZone`. */
let reportedZoneByView: Record<string, string> = {};

/**
 * When set, the next request's response waits for this promise. The
 * response body (and its zone) is still fixed when the request is made.
 */
let holdNextResponse: Promise<void> | null = null;

/** When true, the next request fails with HTTP 500. */
let failNextResponse = false;

/** Returns a promise and the function that resolves it. */
function gate(): { promise: Promise<void>; release: () => void } {
  let release!: () => void;
  const promise = new Promise<void>((resolve) => {
    release = resolve;
  });
  return { promise, release };
}

/** The visible state the request-sequencing guards protect. */
function spinnerShown(el: MetricsPage): boolean {
  return !!el.shadowRoot?.querySelector(`sl-tab-panel[name="${el.activeTab}"] sl-spinner`);
}

function errorAlert(el: MetricsPage): string | null {
  return el.shadowRoot?.querySelector('sl-alert')?.textContent?.trim() ?? null;
}

async function mountOnTab(tab: string): Promise<MetricsPage> {
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string | URL | Request) => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.href : url.url;
      const params = new URL(path, 'http://localhost').searchParams;
      const view = params.get('view') ?? '';
      requestedZones.push(params.get('tz'));
      const zone =
        reportedZoneByView[view] ??
        (reportedZone === 'echo' ? (params.get('tz') ?? undefined) : reportedZone);
      const body = { ...((VIEW_BODIES[view] ?? SUMMARY) as object), timeZone: zone };
      const hold = holdNextResponse ?? Promise.resolve();
      holdNextResponse = null;
      const fail = failNextResponse;
      failNextResponse = false;
      return hold.then(() =>
        fail
          ? new Response(JSON.stringify({ error: { message: 'stale failure' } }), {
              status: 500,
              headers: { 'Content-Type': 'application/json' },
            })
          : new Response(JSON.stringify(body), {
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

/**
 * Chart headings in one tab panel (default: the active tab). Every panel is
 * in the DOM at once, so an unscoped query would mix tabs.
 */
function headings(el: MetricsPage, panel: string = el.activeTab): (string | undefined)[] {
  return [
    ...(el.shadowRoot?.querySelectorAll(`sl-tab-panel[name="${panel}"] .chart-section-title`) ??
      []),
  ].map((h) => h.textContent?.trim());
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
    reportedZoneByView = {};
    holdNextResponse = null;
    failNextResponse = false;
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

  it("labels each tab with its own view's reported zone, not the last-loaded view's", async () => {
    reportedZoneByView = { sessions: 'America/Chicago', 'model-calls': 'Asia/Kathmandu' };
    element = await mountOnTab('sessions');
    expect(headings(element)).toEqual([
      'Daily Sessions (America/Chicago)',
      'Active Agents per Day (America/Chicago)',
    ]);

    // Load another view in a different zone, then show the sessions tab
    // again without refetching: its labels still follow its own data.
    await element.loadView('model-calls');
    element.activeTab = 'sessions';
    await settle(element);

    expect(headings(element)).toEqual([
      'Daily Sessions (America/Chicago)',
      'Active Agents per Day (America/Chicago)',
    ]);
    // Both panels are rendered at once, each with its own zone.
    expect(headings(element, 'model-calls')).toEqual([
      'Daily API Calls by Model (Asia/Kathmandu)',
      'Daily API Calls by Harness (Asia/Kathmandu)',
    ]);
    expect(chartConfigs.length).toBeGreaterThan(0);
    for (const config of chartConfigs) {
      expect(config.options.scales.x.title.text).toBe('Day (America/Chicago)');
    }
    // Reloading the same view in another zone updates the existing charts'
    // axis title, not just their data.
    const chartsBefore = chartConfigs.length;
    reportedZoneByView = { sessions: 'Asia/Tokyo' };
    await element.loadView('sessions');
    await settle(element);
    expect(chartConfigs.length).toBe(chartsBefore);
    expect(headings(element)).toEqual([
      'Daily Sessions (Asia/Tokyo)',
      'Active Agents per Day (Asia/Tokyo)',
    ]);
    for (const config of chartConfigs) {
      expect(config.options.scales.x.title.text).toBe('Day (Asia/Tokyo)');
    }
  });

  it('drops a stale response that arrives after a newer one for the same view', async () => {
    element = await mountOnTab('sessions');
    expect(headings(element)).toEqual(['Daily Sessions (UTC)', 'Active Agents per Day (UTC)']);

    // An old-zone request is held, then the zone changes and the refetch
    // completes first; the late old-zone response must not win.
    let release!: () => void;
    holdNextResponse = new Promise<void>((resolve) => {
      release = resolve;
    });
    const stale = element.loadView('sessions');
    setPreferredTimeZone('Asia/Kathmandu');
    await settle(element);
    expect(headings(element)).toEqual([
      'Daily Sessions (Asia/Kathmandu)',
      'Active Agents per Day (Asia/Kathmandu)',
    ]);

    release();
    await stale;
    await settle(element);
    expect(requestedZones.slice(-2)).toEqual(['UTC', 'Asia/Kathmandu']);
    expect(headings(element)).toEqual([
      'Daily Sessions (Asia/Kathmandu)',
      'Active Agents per Day (Asia/Kathmandu)',
    ]);
  });

  it('keeps the spinner when an older request resolves after a newer one is sent', async () => {
    element = await mountOnTab('sessions');
    expect(spinnerShown(element)).toBe(false);

    const older = gate();
    holdNextResponse = older.promise;
    const olderLoad = element.loadView('model-calls');
    const newer = gate();
    holdNextResponse = newer.promise;
    const newerLoad = element.loadView('sessions');
    await settle(element);
    expect(spinnerShown(element)).toBe(true);

    // The older request finishing must not end the loading state while the
    // newer one is still in flight.
    older.release();
    await olderLoad;
    await settle(element);
    expect(spinnerShown(element)).toBe(true);

    newer.release();
    await newerLoad;
    await settle(element);
    expect(spinnerShown(element)).toBe(false);
  });

  it("does not surface an older request's error after a newer request for the same view", async () => {
    element = await mountOnTab('sessions');
    expect(errorAlert(element)).toBeNull();

    const older = gate();
    holdNextResponse = older.promise;
    failNextResponse = true;
    const olderLoad = element.loadView('sessions');
    await element.loadView('sessions');
    await settle(element);
    expect(errorAlert(element)).toBeNull();

    older.release();
    await olderLoad;
    await settle(element);
    expect(errorAlert(element)).toBeNull();
    expect(spinnerShown(element)).toBe(false);
  });

  it('control: a failing newest request does surface its error', async () => {
    element = await mountOnTab('sessions');
    failNextResponse = true;
    await element.loadView('sessions');
    await settle(element);
    expect(errorAlert(element)).toContain('stale failure');
  });

  it('does not surface a failure from a view that is not the active tab', async () => {
    element = await mountOnTab('sessions');
    failNextResponse = true;
    await element.loadView('model-calls');
    await settle(element);
    expect(errorAlert(element)).toBeNull();

    // The active view failing still shows its error.
    failNextResponse = true;
    await element.loadView('sessions');
    await settle(element);
    expect(errorAlert(element)).toContain('stale failure');
  });

  it('does not surface a failure that lands after the user left that tab', async () => {
    element = await mountOnTab('sessions');
    const held = gate();
    holdNextResponse = held.promise;
    failNextResponse = true;
    const sessionsLoad = element.loadView('sessions');
    element.activeTab = 'tokens';
    await element.loadView('tokens');
    held.release();
    await sessionsLoad;
    await settle(element);
    expect(errorAlert(element)).toBeNull();
  });

  it("does not let a background request clear the active tab's error", async () => {
    element = await mountOnTab('sessions');
    failNextResponse = true;
    await element.loadView('sessions');
    await settle(element);
    expect(errorAlert(element)).toContain('stale failure');

    await element.loadView('model-calls');
    await settle(element);
    expect(errorAlert(element)).toContain('stale failure');
  });

  it('switching tabs through the tab event clears and shows errors for the new tab', async () => {
    element = await mountOnTab('sessions');
    failNextResponse = true;
    await element.loadView('sessions');
    await settle(element);
    expect(errorAlert(element)).toContain('stale failure');

    const tabGroup = element.shadowRoot?.querySelector('sl-tab-group');
    expect(tabGroup).toBeTruthy();
    const showTab = (name: string): void => {
      tabGroup!.dispatchEvent(new CustomEvent('sl-tab-show', { detail: { name } }));
    };

    // A successful switch clears the error the previous tab was showing.
    showTab('model-calls');
    await settle(element);
    expect(element.activeTab).toBe('model-calls');
    expect(errorAlert(element)).toBeNull();

    // A failing switch shows the new tab's error.
    failNextResponse = true;
    showTab('tokens');
    await settle(element);
    expect(element.activeTab).toBe('tokens');
    expect(errorAlert(element)).toContain('stale failure');
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

  it('renders a warning alert when summary includes telemetryHint', async () => {
    const hintText =
      'Cloud telemetry is enabled for GCP, but the default metadata mode is block and the scion-telemetry-gcp-credentials secret is not configured.';
    const origFetch = globalThis.fetch;
    globalThis.fetch = vi.fn(() =>
      Promise.resolve(
        new Response(JSON.stringify({ ...SUMMARY, telemetryHint: hintText }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      )
    ) as unknown as typeof fetch;
    try {
      const el = document.createElement('scion-page-metrics') as MetricsPage;
      document.body.appendChild(el);
      element = el;
      await settle(el);
      const alert = el.shadowRoot?.querySelector('sl-alert[variant="warning"]');
      expect(alert).toBeTruthy();
      expect(alert?.textContent).toContain(hintText);
    } finally {
      globalThis.fetch = origFetch;
    }
  });
});

describe('scion-page-metrics — HubRollupSource tokenomics UI & CM fallback', () => {
  let element: MetricsPage | null = null;

  beforeAll(async () => {
    await import('./metrics-dashboard.js');
  }, 60_000);

  afterEach(() => {
    element?.remove();
    element = null;
    chartConfigs.length = 0;
    requestedZones.length = 0;
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  function stubResponses(responses: Record<string, unknown>): void {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string | URL | Request) => {
        const path = typeof url === 'string' ? url : url instanceof URL ? url.href : url.url;
        const params = new URL(path, 'http://localhost').searchParams;
        const view = params.get('view') ?? 'summary';
        const body = responses[view] ?? SUMMARY;
        return Promise.resolve(
          new Response(JSON.stringify(body), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      })
    );
  }

  it('renders canonical tokens, cache hit rate KPI, measurement badge, and source/granularity on Summary', async () => {
    setFeatureFlag(TOKENOMICS_USAGE_LEDGER_FLAG, true);
    stubResponses({
      summary: {
        periodDays: 7,
        timeZone: 'America/Chicago',
        source: 'hub_rollups',
        granularity: 'hour',
        totalSessions: 4,
        totalApiCalls: 12,
        totalTokens: 54_500,
        uniqueAgents: 3,
        tokens: {
          input: 12_000,
          output: 4_500,
          cacheRead: 36_000,
          cacheWrite: 2_000,
          cacheWrite1h: null,
          reasoning: null,
          summableTokens: 54_500,
          cacheHitRate: 0.72,
        },
        cacheHitRate: 0.72,
        measurement: {
          status: 'measured',
          unmeasuredAgents: 0,
          staleAgents: 0,
          lastReceiptAt: '2026-11-27T00:05:00Z',
        },
      },
    });

    element = document.createElement('scion-page-metrics') as MetricsPage;
    document.body.appendChild(element);
    await settle(element);

    const root = element.shadowRoot!;
    expect(
      root.querySelector('[data-testid="metrics-source-indicator"]')?.textContent?.trim()
    ).toBe('Hub rollups · Local-day buckets');
    expect(root.querySelector('[data-testid="metrics-summary-tokens"]')?.textContent?.trim()).toBe(
      '54.5K'
    );
    expect(
      root.querySelector('[data-testid="metrics-summary-cache-hit-rate"]')?.textContent?.trim()
    ).toBe('72.0%');

    const badge = root.querySelector(
      '[data-testid="metrics-summary-measurement"] scion-measurement-status-badge'
    );
    expect(badge).not.toBeNull();
    expect(
      badge?.shadowRoot
        ?.querySelector('[data-testid="measurement-status-pill"]')
        ?.textContent?.trim()
    ).toBe('Measured');
    expect(root.querySelector('[data-testid="metrics-summary-measurement-partial"]')).toBeNull();
  });

  it('renders null tokens and null cacheHitRate as em dash (never 0) and marks partial coverage', async () => {
    setFeatureFlag(TOKENOMICS_USAGE_LEDGER_FLAG, true);
    stubResponses({
      summary: {
        periodDays: 7,
        timeZone: 'UTC',
        source: 'hub_rollups',
        granularity: 'day',
        totalSessions: 2,
        totalApiCalls: 5,
        totalTokens: 0,
        uniqueAgents: 3,
        tokens: {
          input: null,
          output: null,
          cacheRead: null,
          cacheWrite: null,
          cacheWrite1h: null,
          reasoning: null,
          summableTokens: null,
          cacheHitRate: null,
        },
        cacheHitRate: null,
        measurement: {
          status: 'stale',
          unmeasuredAgents: 1,
          staleAgents: 2,
        },
      },
    });

    element = document.createElement('scion-page-metrics') as MetricsPage;
    document.body.appendChild(element);
    await settle(element);

    const root = element.shadowRoot!;
    expect(
      root.querySelector('[data-testid="metrics-source-indicator"]')?.textContent?.trim()
    ).toBe('Hub rollups · Daily (UTC)');
    expect(root.querySelector('[data-testid="metrics-summary-tokens"]')?.textContent?.trim()).toBe(
      '—'
    );
    expect(
      root.querySelector('[data-testid="metrics-summary-cache-hit-rate"]')?.textContent?.trim()
    ).toBe('—');

    const badge = root.querySelector(
      '[data-testid="metrics-summary-measurement"] scion-measurement-status-badge'
    );
    expect(
      badge?.shadowRoot
        ?.querySelector('[data-testid="measurement-status-counts"]')
        ?.textContent?.trim()
    ).toBe('1 unmeasured · 2 stale');
    expect(
      root.querySelector('[data-testid="metrics-summary-measurement-partial"]')
    ).not.toBeNull();
  });

  it('renders cacheRead, cacheWrite, and dailyCacheHitRate charts with null gaps and UTC fallback axis', async () => {
    setFeatureFlag(TOKENOMICS_USAGE_LEDGER_FLAG, true);
    stubResponses({
      summary: SUMMARY,
      tokens: {
        periodDays: 14,
        timeZone: 'UTC',
        source: 'hub_rollups',
        granularity: 'day',
        input: [{ label: 'claude-sonnet', points: [{ timestamp: '2026-11-25', value: 1000 }] }],
        output: [{ label: 'claude-sonnet', points: [{ timestamp: '2026-11-25', value: 500 }] }],
        cacheRead: [{ label: 'claude-sonnet', points: [{ timestamp: '2026-11-25', value: 3000 }] }],
        cacheWrite: [{ label: 'claude-sonnet', points: [{ timestamp: '2026-11-25', value: 200 }] }],
        totals: {
          input: 1000,
          output: 500,
          cacheRead: 3000,
          cacheWrite: 200,
          cacheWrite1h: null,
          reasoning: null,
          summableTokens: 4700,
          cacheHitRate: 0.7143,
        },
        dailyCacheHitRate: [
          { timestamp: '2026-11-24', value: null },
          { timestamp: '2026-11-25', value: 0 },
          { timestamp: '2026-11-26', value: 0.7143 },
        ],
        measurement: {
          status: 'partial',
          unmeasuredAgents: 1,
          staleAgents: 0,
        },
      },
    });

    element = document.createElement('scion-page-metrics') as MetricsPage;
    document.body.appendChild(element);
    await settle(element);
    element.activeTab = 'tokens';
    await element.loadView('tokens');
    await settle(element);

    const root = element.shadowRoot!;
    expect(
      root.querySelector('[data-testid="metrics-source-indicator"]')?.textContent?.trim()
    ).toBe('Hub rollups · Daily (UTC)');
    expect(root.querySelector('[data-testid="metrics-tokens-summable"]')?.textContent?.trim()).toBe(
      '≥ 4.7K'
    );
    expect(
      root.querySelector('[data-testid="metrics-tokens-cache-hit-rate-total"]')?.textContent?.trim()
    ).toBe('≈ 71.4%');

    expect(headings(element, 'tokens')).toEqual([
      'Daily Input Tokens by Model (UTC)',
      'Daily Output Tokens by Model (UTC)',
      'Daily Cache Read Tokens by Model (UTC)',
      'Daily Cache Write Tokens by Model (UTC)',
      'Daily Cache Hit Rate (UTC)',
    ]);

    // 5 charts: input, output, cacheRead, cacheWrite, dailyCacheHitRate
    expect(chartConfigs).toHaveLength(5);
    const hitRateChart = chartConfigs[4];
    expect(hitRateChart.type).toBe('line');
    expect(hitRateChart.data.labels).toEqual(['2026-11-24', '2026-11-25', '2026-11-26']);
    expect(hitRateChart.data.datasets[0].data).toEqual([null, 0, 71.4]);
    expect(hitRateChart.data.datasets[0].spanGaps).toBe(false);
    expect(hitRateChart.options.scales.x.title.text).toBe('Day (UTC)');
  });

  it('keeps working with Cloud Monitoring source (tokenomics fields absent) and gates UI when experiment is off', async () => {
    // Experiment ON, but Cloud Monitoring response has no tokens/cacheHitRate/measurement/cacheRead/cacheWrite.
    setFeatureFlag(TOKENOMICS_USAGE_LEDGER_FLAG, true);
    stubResponses({
      summary: { ...SUMMARY, source: 'cloud_monitoring', timeZone: 'UTC' },
      tokens: { ...TOKENS, source: 'cloud_monitoring', timeZone: 'UTC' },
    });

    element = document.createElement('scion-page-metrics') as MetricsPage;
    document.body.appendChild(element);
    await settle(element);

    const root = element.shadowRoot!;
    expect(
      root.querySelector('[data-testid="metrics-source-indicator"]')?.textContent?.trim()
    ).toBe('Cloud Monitoring');
    expect(root.querySelector('[data-testid="metrics-summary-cache-hit-rate"]')).toBeNull();
    expect(root.querySelector('[data-testid="metrics-summary-measurement"]')).toBeNull();

    element.activeTab = 'tokens';
    await element.loadView('tokens');
    await settle(element);
    expect(root.querySelector('[data-testid="metrics-tokens-cache-charts"]')).toBeNull();
    expect(root.querySelector('[data-testid="metrics-tokens-hit-rate-section"]')).toBeNull();
    expect(chartConfigs).toHaveLength(2);

    // Experiment OFF hides tokenomics UI even when HubRollupSource fields exist.
    setFeatureFlag(TOKENOMICS_USAGE_LEDGER_FLAG, false);
    await settle(element);
    expect(root.querySelector('[data-testid="metrics-source-indicator"]')).toBeNull();
  });

  it('uses null gaps on HubRollupSource bar series, distinguishes unknown empty cache charts, marks partial totals on null buckets, and applies saturation only to Summable Total', async () => {
    setFeatureFlag(TOKENOMICS_USAGE_LEDGER_FLAG, true);
    stubResponses({
      summary: SUMMARY,
      tokens: {
        periodDays: 7,
        timeZone: 'UTC',
        source: 'hub_rollups',
        granularity: 'day',
        input: [
          {
            label: 'model-a',
            points: [
              { timestamp: '2026-11-24', value: 1000 },
              { timestamp: '2026-11-25', value: 2000 },
            ],
          },
          {
            label: 'model-b',
            points: [{ timestamp: '2026-11-25', value: 500 }],
          },
        ],
        output: [
          {
            label: 'model-a',
            points: [{ timestamp: '2026-11-24', value: 400 }],
          },
        ],
        cacheRead: [],
        cacheWrite: [],
        totals: {
          input: 3500,
          output: 400,
          cacheRead: 0,
          cacheWrite: null,
          cacheWrite1h: null,
          reasoning: null,
          summableTokens: 3900,
          cacheHitRate: null,
          saturated: true,
        },
        measurement: {
          status: 'measured',
          unmeasuredAgents: 0,
          staleAgents: 0,
        },
      },
    });

    element = document.createElement('scion-page-metrics') as MetricsPage;
    document.body.appendChild(element);
    await settle(element);
    element.activeTab = 'tokens';
    await element.loadView('tokens');
    await settle(element);

    const root = element.shadowRoot!;
    // O3 + N2: summableTokens is marked partial (≥) because cacheWrite is null, and saturated (+) applies only to Summable Total.
    expect(root.querySelector('[data-testid="metrics-tokens-summable"]')?.textContent?.trim()).toBe(
      '≥ 3.9K+'
    );
    expect(
      root.querySelector('[data-testid="metrics-tokens-input-total"]')?.textContent?.trim()
    ).toBe('3.5K');
    expect(
      root.querySelector('[data-testid="metrics-tokens-cache-write-total"]')?.textContent?.trim()
    ).toBe('—');

    // O2: measured 0 cacheRead says "No cache read tokens", while null cacheWrite says "not reported".
    expect(
      root.querySelector('[data-testid="metrics-cache-read-empty"]')?.textContent?.trim()
    ).toBe('No cache read tokens in this period.');
    expect(
      root.querySelector('[data-testid="metrics-cache-write-empty"]')?.textContent?.trim()
    ).toBe('Cache write tokens not reported in this period.');

    // O4: multi-series hub_rollups bar chart uses null (not 0) for omitted dates on model-b.
    const inputChart = chartConfigs[0];
    expect(inputChart.data.labels).toEqual(['2026-11-24', '2026-11-25']);
    expect(inputChart.data.datasets[0].data).toEqual([1000, 2000]);
    expect(inputChart.data.datasets[1].data).toEqual([null, 500]);
  });

  it('shows not-reported text on empty cacheRead chart when totals.cacheRead is null', async () => {
    setFeatureFlag(TOKENOMICS_USAGE_LEDGER_FLAG, true);
    stubResponses({
      summary: SUMMARY,
      tokens: {
        periodDays: 7,
        timeZone: 'UTC',
        source: 'hub_rollups',
        granularity: 'day',
        input: [{ label: 'model-a', points: [{ timestamp: '2026-11-25', value: 1000 }] }],
        output: [{ label: 'model-a', points: [{ timestamp: '2026-11-25', value: 500 }] }],
        cacheRead: [],
        cacheWrite: [],
        totals: {
          input: 1000,
          output: 500,
          cacheRead: null,
          cacheWrite: 0,
          cacheWrite1h: null,
          reasoning: null,
          summableTokens: 1500,
          cacheHitRate: null,
        },
        measurement: { status: 'measured', unmeasuredAgents: 0, staleAgents: 0 },
      },
    });

    element = document.createElement('scion-page-metrics') as MetricsPage;
    document.body.appendChild(element);
    await settle(element);
    element.activeTab = 'tokens';
    await element.loadView('tokens');
    await settle(element);

    const root = element.shadowRoot!;
    expect(
      root.querySelector('[data-testid="metrics-cache-read-empty"]')?.textContent?.trim()
    ).toBe('Cache read tokens not reported in this period.');
    expect(
      root.querySelector('[data-testid="metrics-cache-write-empty"]')?.textContent?.trim()
    ).toBe('No cache write tokens in this period.');
  });

  it('renders truncated notices on sessions/model-calls/tokens, shows generic partialFailure copy without leaking raw errors, and logs details to console', async () => {
    setFeatureFlag(TOKENOMICS_USAGE_LEDGER_FLAG, true);
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    stubResponses({
      summary: {
        ...SUMMARY,
        timeZone: 'UTC',
        source: 'hub_rollups',
        granularity: 'day',
        totalSessions: null,
        partialFailure: true,
        partialErrors: ['daily sessions: sql: driver internal error'],
      },
      sessions: {
        periodDays: 7,
        timeZone: 'UTC',
        source: 'hub_rollups',
        granularity: 'day',
        dailyCounts: [],
        activeAgents: [{ timestamp: '2026-11-25', value: 2 }],
        truncated: true,
        partialFailure: true,
        partialErrors: ['daily sessions: sql: driver internal error'],
      },
      'model-calls': {
        ...MODEL_CALLS,
        timeZone: 'UTC',
        source: 'hub_rollups',
        granularity: 'day',
        truncated: true,
      },
      tokens: {
        ...TOKENS,
        timeZone: 'UTC',
        source: 'hub_rollups',
        granularity: 'day',
        truncated: true,
      },
    });

    element = document.createElement('scion-page-metrics') as MetricsPage;
    document.body.appendChild(element);
    await settle(element);

    const root = element.shadowRoot!;
    // Summary: totalSessions=null renders Unavailable (never 0) + generic partial-failure alert (no raw SQL/driver text).
    expect(
      root.querySelector('[data-testid="metrics-summary-sessions"]')?.textContent?.trim()
    ).toBe('Unavailable');
    const summaryAlert = root.querySelector('[data-testid="metrics-summary-partial-failure"]');
    expect(summaryAlert).not.toBeNull();
    expect(summaryAlert?.textContent).toContain('Some metrics could not be loaded');
    expect(summaryAlert?.textContent).not.toContain('sql: driver internal error');
    expect(warnSpy).toHaveBeenCalledWith('[metrics-dashboard] partial failure on view=summary:', [
      'daily sessions: sql: driver internal error',
    ]);

    // Sessions: partialFailure with empty dailyCounts shows unavailable placeholder + truncated notice.
    element.activeTab = 'sessions';
    await element.loadView('sessions');
    await settle(element);
    expect(root.querySelector('[data-testid="metrics-sessions-unavailable"]')).not.toBeNull();
    expect(root.querySelector('[data-testid="metrics-sessions-partial-failure"]')).not.toBeNull();
    expect(
      root.querySelector('[data-testid="metrics-sessions-partial-failure"]')?.textContent
    ).not.toContain('sql: driver internal error');
    expect(root.querySelector('[data-testid="metrics-sessions-truncated"]')?.textContent).toContain(
      'earliest boundary day was omitted'
    );

    // Model calls & Tokens: truncated notice shown.
    element.activeTab = 'model-calls';
    await element.loadView('model-calls');
    await settle(element);
    expect(
      root.querySelector('[data-testid="metrics-model-calls-truncated"]')?.textContent
    ).toContain('daily granularity');

    element.activeTab = 'tokens';
    await element.loadView('tokens');
    await settle(element);
    expect(root.querySelector('[data-testid="metrics-tokens-truncated"]')?.textContent).toContain(
      'earliest boundary day was omitted'
    );
  });

  it('marks cacheHitRatePartial and dailyCacheHitRate partial points as upper bounds (≤ with tooltip), keeps null as —, and handles absent source when experiment is off', async () => {
    setFeatureFlag(TOKENOMICS_USAGE_LEDGER_FLAG, true);
    stubResponses({
      summary: {
        periodDays: 7,
        timeZone: 'America/Chicago',
        source: 'hub_rollups',
        granularity: 'hour',
        totalSessions: 2,
        totalApiCalls: 6,
        totalTokens: 50_000,
        uniqueAgents: 1,
        tokens: {
          input: 12_000,
          output: 2_000,
          cacheRead: 36_000,
          cacheWrite: null,
          cacheWrite1h: null,
          reasoning: null,
          summableTokens: 50_000,
          cacheHitRate: 0.75,
          cacheHitRatePartial: true,
        },
        cacheHitRate: 0.75,
        cacheHitRatePartial: true,
        measurement: { status: 'measured', unmeasuredAgents: 0, staleAgents: 0 },
      },
      tokens: {
        periodDays: 7,
        timeZone: 'UTC',
        source: 'hub_rollups',
        granularity: 'day',
        input: [{ label: 'm1', points: [{ timestamp: '2026-11-25', value: 1000 }] }],
        output: [{ label: 'm1', points: [{ timestamp: '2026-11-25', value: 500 }] }],
        cacheRead: [{ label: 'm1', points: [{ timestamp: '2026-11-25', value: 3000 }] }],
        cacheWrite: [],
        totals: {
          input: 1000,
          output: 500,
          cacheRead: 3000,
          cacheWrite: null,
          cacheWrite1h: null,
          reasoning: null,
          summableTokens: 4500,
          cacheHitRate: 0.75,
          cacheHitRatePartial: true,
        },
        dailyCacheHitRate: [
          { timestamp: '2026-11-24', value: null, partial: false },
          { timestamp: '2026-11-25', value: 0.75, partial: true },
          { timestamp: '2026-11-26', value: 0.5, partial: false },
        ],
        measurement: { status: 'measured', unmeasuredAgents: 0, staleAgents: 0 },
      },
    });

    element = document.createElement('scion-page-metrics') as MetricsPage;
    document.body.appendChild(element);
    await settle(element);

    const root = element.shadowRoot!;
    const summaryHitRate = root.querySelector('[data-testid="metrics-summary-cache-hit-rate"]');
    expect(summaryHitRate?.textContent?.trim()).toBe('≤ 75.0%');
    expect(summaryHitRate?.getAttribute('title')).toContain('Upper bound');

    element.activeTab = 'tokens';
    await element.loadView('tokens');
    await settle(element);

    const tokensHitRate = root.querySelector('[data-testid="metrics-tokens-cache-hit-rate-total"]');
    expect(tokensHitRate?.textContent?.trim()).toBe('≤ 75.0%');
    expect(tokensHitRate?.getAttribute('title')).toContain('Upper bound');
    expect(root.querySelector('[data-testid="metrics-hit-rate-partial-note"]')).not.toBeNull();

    // Hit rate chart marks partial point with triangle and upper-bound tooltip, null stays null.
    const hitRateChart = chartConfigs[chartConfigs.length - 1];
    expect(hitRateChart.data.datasets[0].data).toEqual([null, 75, 50]);
    expect(hitRateChart.data.datasets[0].pointStyle).toEqual(['circle', 'triangle', 'circle']);
    const tooltipLabel = hitRateChart.options.plugins?.tooltip?.callbacks?.label;
    expect(tooltipLabel).toBeDefined();
    expect(
      tooltipLabel!({
        dataIndex: 0,
        dataset: hitRateChart.data.datasets[0],
        parsed: { y: null },
      })
    ).toBe('Cache Hit Rate: —');
    expect(
      tooltipLabel!({
        dataIndex: 1,
        dataset: hitRateChart.data.datasets[0],
        parsed: { y: 75 },
      })
    ).toContain('≤ 75.0%');
    expect(
      tooltipLabel!({
        dataIndex: 2,
        dataset: hitRateChart.data.datasets[0],
        parsed: { y: 50 },
      })
    ).toBe('Cache Hit Rate: 50.0%');

    // Experiment OFF with `source` absent on response works cleanly.
    setFeatureFlag(TOKENOMICS_USAGE_LEDGER_FLAG, false);
    stubResponses({
      summary: { ...SUMMARY, timeZone: 'UTC' },
      tokens: { ...TOKENS, timeZone: 'UTC' },
    });
    await element.loadView('summary');
    await settle(element);
    expect(root.querySelector('[data-testid="metrics-source-indicator"]')).toBeNull();
    expect(root.querySelector('[data-testid="metrics-summary-cache-hit-rate"]')).toBeNull();
  });
});
