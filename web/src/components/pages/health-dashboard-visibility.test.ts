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
 * Health dashboard: the 30-second auto-refresh pauses while the tab is
 * hidden and catches up with one fetch when the tab is shown again (C5b).
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

vi.mock('../../client/api.js', async (orig) => ({
  ...(await orig<typeof import('../../client/api.js')>()),
  apiFetch: vi.fn(),
}));

import { apiFetch } from '../../client/api.js';
import { ScionPageHealthDashboard } from './health-dashboard.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

const SUMMARY_URL = '/api/v1/admin/health/summary';
const TEN_MINUTES = 10 * 60_000;

describe('scion-page-health-dashboard auto-refresh in a hidden tab', () => {
  const fetchMock = vi.mocked(apiFetch);
  let hidden = false;
  let el: ScionPageHealthDashboard;

  function setHidden(value: boolean): void {
    hidden = value;
    document.dispatchEvent(new Event('visibilitychange'));
  }

  function summaryRequests(): number {
    return fetchMock.mock.calls.filter((c) => c[0] === SUMMARY_URL).length;
  }

  async function mount(): Promise<void> {
    el = new ScionPageHealthDashboard();
    document.body.appendChild(el);
    await el.updateComplete;
    await vi.advanceTimersByTimeAsync(0);
  }

  beforeEach(() => {
    vi.useFakeTimers();
    hidden = false;
    vi.spyOn(document, 'hidden', 'get').mockImplementation(() => hidden);
    fetchMock.mockReset();
    fetchMock.mockImplementation((url: string) =>
      url === SUMMARY_URL
        ? Promise.resolve(
            new Response(
              JSON.stringify({
                status: 'healthy',
                hub: {
                  status: 'healthy',
                  version: 'v1',
                  connected_brokers: 0,
                  active_agents: 0,
                  projects: 0,
                },
                brokers: [],
                agents: {
                  total: 0,
                  active: 0,
                  errored: 0,
                  considered: 0,
                  by_phase: [],
                  problems: [],
                },
                dispatch: null,
              }),
              { status: 200, headers: { 'Content-Type': 'application/json' } }
            )
          )
        : Promise.reject(new Error(`unexpected request ${url}`))
    );
  });

  afterEach(() => {
    el?.remove();
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it('issues no fetches while hidden, then exactly one on becoming visible', async () => {
    await mount();
    expect(summaryRequests()).toBe(1); // the mount load

    setHidden(true);
    await vi.advanceTimersByTimeAsync(TEN_MINUTES);
    expect(summaryRequests()).toBe(1);

    setHidden(false);
    await vi.advanceTimersByTimeAsync(0);
    expect(summaryRequests()).toBe(2);
  });

  it('issues no polls when mounted in a hidden tab, then one on becoming visible', async () => {
    hidden = true;
    await mount();
    expect(summaryRequests()).toBe(1);

    await vi.advanceTimersByTimeAsync(TEN_MINUTES);
    expect(summaryRequests()).toBe(1);

    setHidden(false);
    await vi.advanceTimersByTimeAsync(0);
    expect(summaryRequests()).toBe(2);
  });

  it('keeps refreshing every 30 seconds while visible', async () => {
    await mount();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(summaryRequests()).toBe(3);
  });

  it('stops refreshing on disconnect, including on a later visibility change', async () => {
    await mount();
    el.remove();
    expect((el as any).stopRefreshTimer).toBeNull();

    await vi.advanceTimersByTimeAsync(TEN_MINUTES);
    setHidden(true);
    setHidden(false);
    await vi.advanceTimersByTimeAsync(TEN_MINUTES);
    expect(summaryRequests()).toBe(1);
  });

  it('does not refresh, hidden or not, with auto-refresh turned off', async () => {
    await mount();
    (el as any).toggleAutoRefresh();
    expect((el as any).stopRefreshTimer).toBeNull();

    setHidden(true);
    await vi.advanceTimersByTimeAsync(TEN_MINUTES);
    setHidden(false);
    await vi.advanceTimersByTimeAsync(TEN_MINUTES);
    expect(summaryRequests()).toBe(1);
  });
});
