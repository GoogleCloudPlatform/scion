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
    expect(text).toContain('Hub Status');
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

type LitLike = Element & { updateComplete: Promise<unknown> };
