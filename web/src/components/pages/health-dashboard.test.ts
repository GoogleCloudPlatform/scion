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
          agents: { total: 0, by_phase: {}, stalled: [], crashed: [], errored: [] },
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
      expect(el.shadowRoot?.textContent ?? '').toContain('Dispatch Pipeline');
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
