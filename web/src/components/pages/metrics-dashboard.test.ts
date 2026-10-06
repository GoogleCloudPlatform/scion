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
