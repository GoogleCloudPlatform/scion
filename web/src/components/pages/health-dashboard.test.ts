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
 * Health dashboard page: the rendered cards, and the broker heartbeat age.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

vi.mock('../../client/api.js', async (orig) => ({
  ...(await orig<typeof import('../../client/api.js')>()),
  apiFetch: vi.fn(),
}));

import { apiFetch } from '../../client/api.js';
import { formatHeartbeatAge, ScionPageHealthDashboard } from './health-dashboard.js';
import { elementStyleRules } from './__fixtures__/css-rules.js';

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('scion-page-health-dashboard cards', () => {
  let el: ScionPageHealthDashboard;

  beforeEach(() => {
    vi.mocked(apiFetch).mockImplementation(async (url: string) => {
      if (url === '/api/v1/admin/health/summary') {
        return json({
          status: 'healthy',
          hub: {
            status: 'healthy',
            version: 'v1',
            uptime: '1h',
            connected_brokers: 0,
            active_agents: 0,
            projects: 0,
          },
          database: {
            status: 'healthy',
            pool_active: 0,
            pool_max: 10,
            pool_wait_count_total: 0,
            pool_idle: 0,
          },
          brokers: [],
          agents: { total: 0, active: 0, errored: 0, considered: 0, by_phase: [], problems: [] },
          dispatch: null,
        });
      }
      throw new Error(`unexpected request ${url}`);
    });
    el = new ScionPageHealthDashboard();
    document.body.appendChild(el);
  });

  afterEach(() => {
    el.remove();
    vi.mocked(apiFetch).mockReset();
  });

  async function rendered(): Promise<string> {
    await vi.waitFor(() => {
      expect(el.shadowRoot?.querySelector('scion-health-dispatch-card')).not.toBeNull();
    });
    await el.updateComplete;
    return el.shadowRoot?.textContent ?? '';
  }

  it('renders no stall settings card and no recent alerts card', async () => {
    const text = await rendered();
    expect(el.shadowRoot?.querySelector('scion-health-hub-card')).not.toBeNull();
    expect(text).not.toContain('Stall Detection');
    expect(text).not.toContain('Auto-Suspend');
    expect(text).not.toContain('Recent Alerts');
    expect(text).not.toContain('Cloud Monitoring');
    expect(el.shadowRoot?.querySelector('a[href*="console.cloud.google.com"]')).toBeNull();
  });

  it('never reads or writes the server config', async () => {
    await rendered();
    // One manual refresh cycle, as the Refresh button and the poll timer run it.
    await (el as unknown as { fetchData(): Promise<void> }).fetchData();
    await el.updateComplete;
    const summaryCalls = vi
      .mocked(apiFetch)
      .mock.calls.filter(([url]) => url === '/api/v1/admin/health/summary');
    expect(summaryCalls.length).toBeGreaterThanOrEqual(2);
    const urls = vi.mocked(apiFetch).mock.calls.map(([url]) => url);
    expect(urls).not.toContain('/api/v1/admin/server-config');
  });
});

describe('formatHeartbeatAge', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it('shows never for a null, undefined or empty heartbeat', () => {
    expect(formatHeartbeatAge(null)).toBe('never');
    expect(formatHeartbeatAge(undefined)).toBe('never');
    expect(formatHeartbeatAge('')).toBe('never');
  });

  it('shows never for the Go zero time', () => {
    expect(formatHeartbeatAge('0001-01-01T00:00:00Z')).toBe('never');
    expect(formatHeartbeatAge('1970-01-01T00:00:00Z')).toBe('never');
    expect(formatHeartbeatAge('1969-12-31T23:59:59Z')).toBe('never');
  });

  it('still formats a recent heartbeat as a relative age', () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-10-06T12:00:00Z'));
    expect(formatHeartbeatAge('2026-10-06T11:59:30Z')).toBe('30s ago');
    expect(formatHeartbeatAge('2026-10-06T11:55:00Z')).toBe('5m ago');
    expect(formatHeartbeatAge('2026-10-06T09:00:00Z')).toBe('3h ago');
    expect(formatHeartbeatAge('2026-10-05T12:00:00Z')).toBe('yesterday');
    expect(formatHeartbeatAge('2026-10-04T12:00:00Z')).toBe('2d ago');
  });

  it('shows just now for a future instant and unknown for garbage', () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-10-06T12:00:00Z'));
    expect(formatHeartbeatAge('2026-10-06T12:01:00Z')).toBe('just now');
    expect(formatHeartbeatAge('not-a-date')).toBe('unknown');
  });
});

describe('scion-page-health-dashboard runtime brokers (ptone/scion#3582)', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    vi.mocked(apiFetch).mockReset();
  });

  it('renders runtime_brokers as one table row per broker', async () => {
    vi.mocked(apiFetch).mockImplementation(async () =>
      json({
        status: 'healthy',
        hub: { status: 'healthy', version: 'v1', uptime: '1h', connected_brokers: 2 },
        database: { status: 'healthy', pool_active: 1, pool_max: 10, pool_idle: 1 },
        runtime_brokers: {
          items: [
            {
              id: 'b1',
              name: 'zulu',
              version: '1.0.0',
              status: 'online',
              last_heartbeat: null,
              runtime: null,
              workspace_storage: { backend: 'nfs', nfs_healthy: true },
              agents: { running: 0, attention: 0 },
            },
            {
              id: 'b2',
              name: 'alpha',
              version: '1.0.0',
              status: 'offline',
              last_heartbeat: null,
              runtime: { type: 'docker', profile: 'docker' },
              workspace_storage: { backend: 'local' },
              agents: { running: 0, attention: 0 },
            },
          ],
          total: 5,
          truncated: true,
        },
        agents: { total: 0, active: 0, errored: 0, considered: 0, by_phase: [], problems: [] },
        dispatch: null,
        stall_config: { threshold_seconds: 300, auto_suspend: false },
      })
    );
    const page = document.createElement('scion-page-health-dashboard') as ScionPageHealthDashboard;
    document.body.appendChild(page);
    await (page as unknown as { fetchData(): Promise<void> }).fetchData();
    await page.updateComplete;

    const table = page.shadowRoot?.querySelector('scion-health-broker-table');
    expect(table).not.toBeNull();
    await (table as LitLike).updateComplete;
    const rows = [...(table!.shadowRoot?.querySelectorAll('tbody tr') ?? [])];
    expect(rows.map((r) => (r as HTMLElement).dataset.brokerId)).toEqual(['b2', 'b1']);
    expect(table!.shadowRoot?.querySelector('.note')?.textContent?.trim()).toBe('Showing 2 of 5');
    expect(page.shadowRoot?.querySelector('.broker-card')).toBeNull();
  });
});

describe('scion-page-health-dashboard agents (ptone/scion#3587)', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    vi.mocked(apiFetch).mockReset();
  });

  it('hands the agents block to the agents card and shows no stall data', async () => {
    vi.mocked(apiFetch).mockImplementation(async () =>
      json({
        status: 'degraded',
        hub: { status: 'healthy', version: 'v1', uptime: '1h', connected_brokers: 0 },
        database: { status: 'healthy', pool_active: 0, pool_max: 10, pool_idle: 0 },
        runtime_brokers: { items: [], total: 0, truncated: false },
        agents: {
          total: 3,
          active: 2,
          errored: 1,
          considered: 3,
          by_phase: [
            { phase: 'running', count: 2 },
            { phase: 'error', count: 1 },
          ],
          problems: [
            {
              kind: 'errored',
              count: 1,
              items: [
                { id: 'ag1', name: 'w', project_id: 'p', project_slug: 'proj', broker_id: '' },
              ],
            },
          ],
        },
        dispatch: null,
      })
    );
    const page = document.createElement('scion-page-health-dashboard') as ScionPageHealthDashboard;
    document.body.appendChild(page);
    await (page as unknown as { fetchData(): Promise<void> }).fetchData();
    await page.updateComplete;

    const card = page.shadowRoot?.querySelector('scion-health-agents-card');
    expect(card).not.toBeNull();
    await (card as LitLike).updateComplete;
    const link = card!.shadowRoot?.querySelector('.group a');
    expect(link?.getAttribute('href')).toBe('/agents/ag1');
    expect(link?.textContent?.trim()).toBe('proj / w');
    expect(page.shadowRoot?.textContent ?? '').not.toMatch(/stall/i);
    expect(card!.shadowRoot?.textContent ?? '').not.toMatch(/stall/i);
  });

  it('shows agents as not reported, not as all clear, when agents is null', async () => {
    vi.mocked(apiFetch).mockImplementation(async () =>
      json({
        status: 'degraded',
        hub: { status: 'healthy', version: 'v1', uptime: '1h', connected_brokers: 0 },
        database: { status: 'healthy', pool_active: 0, pool_max: 10, pool_idle: 0 },
        runtime_brokers: { items: [], total: 0, truncated: false },
        agents: null,
        dispatch: null,
      })
    );
    const page = document.createElement('scion-page-health-dashboard') as ScionPageHealthDashboard;
    document.body.appendChild(page);
    await (page as unknown as { fetchData(): Promise<void> }).fetchData();
    await page.updateComplete;

    const card = page.shadowRoot?.querySelector('scion-health-agents-card');
    expect(card).not.toBeNull();
    await (card as LitLike).updateComplete;
    const text = card!.shadowRoot?.textContent ?? '';
    expect(text).toContain('Agent data not available');
    expect(text).not.toContain('No agents need attention');
  });
});

describe('scion-page-health-dashboard dispatch (ptone/scion#3589)', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    vi.mocked(apiFetch).mockReset();
  });

  it('hands the dispatch block to the dispatch card, with no placeholder prose', async () => {
    vi.mocked(apiFetch).mockImplementation(async () =>
      json({
        status: 'degraded',
        hub: { status: 'healthy', version: 'v1', uptime: '1h', connected_brokers: 0 },
        database: { status: 'healthy', pool_active: 0, pool_max: 10, pool_idle: 0 },
        runtime_brokers: { items: [], total: 0, truncated: false },
        agents: null,
        dispatch: { stuck_messages: 2, stuck_broker_dispatch: 1, failed_broker_dispatch_1h: 4 },
      })
    );
    const page = document.createElement('scion-page-health-dashboard') as ScionPageHealthDashboard;
    document.body.appendChild(page);
    await (page as unknown as { fetchData(): Promise<void> }).fetchData();
    await page.updateComplete;

    const card = page.shadowRoot?.querySelector('scion-health-dispatch-card');
    expect(card).not.toBeNull();
    await (card as LitLike).updateComplete;
    const values = [...(card!.shadowRoot?.querySelectorAll('li .value') ?? [])].map((v) =>
      v.textContent?.trim()
    );
    expect(values).toEqual(['2', '1', '4']);
    const text = `${page.shadowRoot?.textContent ?? ''} ${card!.shadowRoot?.textContent ?? ''}`;
    expect(text).not.toMatch(/not yet available|future update/i);
  });
});

// ---------------------------------------------------------------------------
// Header, Needs attention and layout (ptone/scion#3595)
// ---------------------------------------------------------------------------

function brokerItem(i: number, status = 'online') {
  return {
    id: `b${i}`,
    name: `broker-${String(i).padStart(2, '0')}`,
    version: '1.0.0',
    status,
    last_heartbeat: '2026-10-09T11:59:00Z',
    runtime: { type: 'docker', profile: 'docker' },
    workspace_storage: { backend: 'local' },
    health: null,
    agents: { running: 1, attention: 0 },
  };
}

function summaryBody(over: Record<string, unknown> = {}) {
  return {
    status: 'healthy',
    generated_at: '2026-10-09T12:34:56Z',
    attention: [],
    hub: {
      status: 'healthy',
      instance_id: 'hub-7f3a',
      version: 'v1',
      uptime: '1h',
      connected_brokers: 1,
      active_agents: 21,
      projects: 1,
      checks: { database: 'healthy', workspace_storage: 'healthy' },
    },
    database: {
      status: 'healthy',
      pool_active: 3,
      pool_max: 25,
      pool_idle: 2,
      pool_wait_count_total: 0,
    },
    runtime_brokers: { items: [brokerItem(0)], total: 1, truncated: false },
    integrations: [],
    integrations_detail: true,
    integration_counts: { total: 0, healthy: 0, degraded: 0, unhealthy: 0, unknown: 0 },
    agents: { total: 21, active: 21, errored: 0, considered: 21, by_phase: [], problems: [] },
    dispatch: { stuck_messages: 0, stuck_broker_dispatch: 0, failed_broker_dispatch_1h: 0 },
    ...over,
  };
}

async function mountPage(body: unknown): Promise<ScionPageHealthDashboard> {
  vi.mocked(apiFetch).mockImplementation(async () => json(body));
  const page = document.createElement('scion-page-health-dashboard') as ScionPageHealthDashboard;
  document.body.appendChild(page);
  await (page as unknown as { fetchData(): Promise<void> }).fetchData();
  await page.updateComplete;
  for (const child of page.shadowRoot?.querySelectorAll('*') ?? []) {
    if ('updateComplete' in child) await (child as LitLike).updateComplete;
  }
  return page;
}

function pill(page: ScionPageHealthDashboard): HTMLElement {
  return page.shadowRoot!.querySelector('[data-role="overall-status"]') as HTMLElement;
}

function attentionRows(page: ScionPageHealthDashboard): HTMLElement[] {
  const panel = page.shadowRoot!.querySelector('scion-health-attention')!;
  return [...(panel.shadowRoot?.querySelectorAll('li') ?? [])] as HTMLElement[];
}

describe('scion-page-health-dashboard header (ptone/scion#3595)', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    vi.mocked(apiFetch).mockReset();
  });

  it('shows the pill with a theme tone, the as-of time and the serving instance', async () => {
    const page = await mountPage(summaryBody());
    expect(pill(page).textContent?.trim()).toBe('healthy');
    expect(pill(page).classList.contains('tone-ok')).toBe(true);
    // TZ is pinned to UTC in vitest.config.ts.
    const asOf = page.shadowRoot!.querySelector('[data-role="as-of"]');
    expect(asOf?.textContent?.trim()).toBe('as of 12:34:56');
    const instance = page.shadowRoot!.querySelector('[data-role="instance"]');
    expect(instance?.textContent?.trim()).toBe('this instance: hub-7f3a');
  });

  it('maps degraded and unhealthy to the warning and danger tones', async () => {
    for (const [status, tone] of [
      ['degraded', 'tone-warn'],
      ['unhealthy', 'tone-bad'],
      ['unknown', 'tone-neutral'],
    ] as const) {
      const page = await mountPage(summaryBody({ status }));
      expect(pill(page).classList.contains(tone)).toBe(true);
      page.remove();
    }
  });

  it('styles the page with theme tokens only, with no hex fallbacks', () => {
    for (const body of elementStyleRules('scion-page-health-dashboard').values()) {
      expect(body).not.toMatch(/#[0-9a-f]{3,8}\b|rgba?\(/i);
      for (const m of body.matchAll(/var\((--[\w-]+)/g)) expect(m[1]).toMatch(/^--scion-/);
    }
  });
});

describe('scion-page-health-dashboard needs attention (ptone/scion#3595)', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    vi.mocked(apiFetch).mockReset();
  });

  it('shows the panel first, with Nothing needs attention when the list is empty', async () => {
    const page = await mountPage(summaryBody());
    const sections = [...page.shadowRoot!.children].filter((c) => c.tagName !== 'STYLE');
    // Header, then the attention panel's container.
    expect(sections[1]?.querySelector('scion-health-attention')).not.toBeNull();
    const panel = page.shadowRoot!.querySelector('scion-health-attention')!;
    expect(panel.shadowRoot?.textContent).toContain('Nothing needs attention.');
  });

  it('keeps the pill healthy and shows a linked warning for an agent-only problem below 5%', async () => {
    const page = await mountPage(
      summaryBody({
        status: 'healthy',
        attention: [
          {
            severity: 'warning',
            kind: 'agents',
            subject: { type: 'agent', id: 'ag1', name: 'w', project_id: 'p' },
            message: 'Agent proj / w is in the error phase',
          },
        ],
        agents: {
          total: 21,
          active: 20,
          errored: 1,
          considered: 21,
          by_phase: [],
          problems: [
            {
              kind: 'errored',
              count: 1,
              items: [
                { id: 'ag1', name: 'w', project_id: 'p', project_slug: 'proj', broker_id: 'b0' },
              ],
            },
          ],
        },
      })
    );
    expect(pill(page).textContent?.trim()).toBe('healthy');
    expect(pill(page).classList.contains('tone-ok')).toBe(true);
    const rows = attentionRows(page);
    expect(rows).toHaveLength(1);
    expect(rows[0]!.querySelector('sl-icon')?.getAttribute('label')).toBe('Warning');
    const link = rows[0]!.querySelector('a');
    expect(link?.getAttribute('href')).toBe('/agents/ag1');
    expect(link?.textContent?.trim()).toBe('Agent proj / w is in the error phase');
  });

  it('degrades the pill and links the broker for an offline broker', async () => {
    const page = await mountPage(
      summaryBody({
        status: 'degraded',
        attention: [
          {
            severity: 'warning',
            kind: 'broker_offline',
            subject: { type: 'runtime_broker', id: 'b1', name: 'broker-01' },
            message: 'Runtime broker broker-01 is offline',
          },
        ],
        runtime_brokers: {
          items: [brokerItem(0), brokerItem(1, 'offline')],
          total: 2,
          truncated: false,
        },
      })
    );
    expect(pill(page).textContent?.trim()).toBe('degraded');
    expect(pill(page).classList.contains('tone-warn')).toBe(true);
    const link = attentionRows(page)[0]!.querySelector('a');
    expect(link?.getAttribute('href')).toBe('/brokers/b1');
  });

  it('does not link an integration item when integrations_detail is false', async () => {
    const item = {
      severity: 'warning',
      kind: 'integration',
      subject: { type: 'integration', id: 'chat', name: 'chat' },
      message: 'Integration chat is unhealthy',
    };
    const restricted = await mountPage(
      summaryBody({ status: 'degraded', attention: [item], integrations_detail: false })
    );
    expect(attentionRows(restricted)[0]!.querySelector('a')).toBeNull();
    restricted.remove();

    const full = await mountPage(
      summaryBody({ status: 'degraded', attention: [item], integrations_detail: true })
    );
    expect(attentionRows(full)[0]!.querySelector('a')?.getAttribute('href')).toBe(
      '/admin/integrations/chat'
    );
  });

  it('does not link the hub item for a broker list that could not be read', async () => {
    const page = await mountPage(
      summaryBody({
        attention: [
          {
            severity: 'warning',
            kind: 'hub_check',
            subject: { type: 'hub', id: 'hub-7f3a' },
            message: 'Runtime broker data not available',
          },
        ],
        runtime_brokers: { items: [], total: 0, truncated: false, not_reported: true },
      })
    );
    const rows = attentionRows(page);
    expect(rows[0]!.textContent?.trim()).toBe('Runtime broker data not available');
    expect(rows[0]!.querySelector('a')).toBeNull();
  });
});

describe('scion-page-health-dashboard layout (ptone/scion#3595)', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    vi.mocked(apiFetch).mockReset();
  });

  for (const n of [1, 2, 13]) {
    it(`puts Dispatch next to Hub and no card alone at full width with ${n} broker(s)`, async () => {
      const items = Array.from({ length: n }, (_, i) => brokerItem(i));
      const page = await mountPage(
        summaryBody({ runtime_brokers: { items, total: n, truncated: false } })
      );
      const root = page.shadowRoot!;
      const pair = root.querySelector('.grid-2')!;
      expect([...pair.children].map((c) => c.tagName.toLowerCase())).toEqual([
        'scion-health-hub-card',
        'scion-health-dispatch-card',
      ]);
      expect(elementStyleRules('scion-page-health-dashboard').get('.grid-2')).toMatch(
        /grid-template-columns:\s*1fr 1fr/
      );
      // Full-width rows hold the attention panel and the tables, never a
      // lone summary card.
      for (const full of root.querySelectorAll('.grid-full')) {
        expect(full.querySelector('scion-health-hub-card, scion-health-dispatch-card')).toBeNull();
      }
      // The broker section is one table with a row per broker.
      const table = root.querySelector('scion-health-broker-table')!;
      expect(table.shadowRoot?.querySelectorAll('table')).toHaveLength(1);
      expect(table.shadowRoot?.querySelectorAll('tbody tr')).toHaveLength(n);
    });
  }

  it('renders no empty full-width wrapper when the Integrations section is hidden', async () => {
    for (const over of [
      { integrations: [], integrations_detail: true },
      {
        integrations: [],
        integrations_detail: false,
        integration_counts: { total: 0, healthy: 0, degraded: 0, unhealthy: 0, unknown: 0 },
      },
    ]) {
      const page = await mountPage(summaryBody(over));
      const root = page.shadowRoot!;
      expect(root.querySelector('scion-health-integrations')).toBeNull();
      const full = [...root.querySelectorAll('.grid-full')];
      for (const f of full) expect(f.children.length).toBeGreaterThan(0);
      // Attention, brokers and agents only.
      expect(full).toHaveLength(3);
      page.remove();
    }
  });

  it('wraps the Integrations section when it has content', async () => {
    const page = await mountPage(
      summaryBody({
        integrations: [],
        integrations_detail: false,
        integration_counts: { total: 2, healthy: 2, degraded: 0, unhealthy: 0, unknown: 0 },
      })
    );
    const section = page.shadowRoot!.querySelector('scion-health-integrations');
    expect(section?.parentElement?.classList.contains('grid-full')).toBe(true);
  });

  it('has no separate Database card: the pool is inside the Hub card', async () => {
    const page = await mountPage(summaryBody());
    const hub = page.shadowRoot!.querySelector('scion-health-hub-card')!;
    expect(hub.shadowRoot?.querySelector('[data-role="pool"]')?.textContent?.trim()).toBe(
      'Pool 3/25 in use, 2 idle'
    );
    const cardTitles = [...page.shadowRoot!.querySelectorAll('*')]
      .flatMap((c) => [...(c.shadowRoot?.querySelectorAll('.card-title') ?? [])])
      .map((t) => t.textContent?.trim());
    expect(cardTitles).not.toContain('Database');
  });
});

type LitLike = Element & { updateComplete: Promise<unknown> };
