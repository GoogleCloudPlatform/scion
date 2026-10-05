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
 * Hub clock offset (ptone/scion#2952), including the issue's acceptance:
 * with a simulated +90s browser skew, a renewed deleting view stays
 * "Deleting…", and the flip happens at the server-time lease.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import type { ReactiveController, ReactiveControllerHost } from 'lit';
import {
  _resetHubClock,
  hubClockOffsetMs,
  hubNow,
  recordHubDateHeader,
  recordHubTime,
} from './hub-clock.js';
import { DeletionLeaseController } from '../components/shared/deletion-badge.js';
import { apiFetch } from '../client/api.js';
import type { Agent, DeletionInfo } from './types.js';

const SERVER_T0 = Date.parse('2026-10-04T12:00:00Z');
const SKEW = 90_000; // the browser clock runs 90s ahead of the hub
const iso = (ms: number): string => new Date(ms).toISOString();

beforeEach(() => {
  _resetHubClock();
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  _resetHubClock();
});

describe('offset estimation', () => {
  it('is 0 with no samples', () => {
    expect(hubClockOffsetMs()).toBe(0);
  });

  it('uses the request midpoint', () => {
    recordHubTime(10_000, 1_000, 3_000); // midpoint 2_000
    expect(hubClockOffsetMs()).toBe(8_000);
  });

  it('reads a Date header as the middle of its second', () => {
    const browser = SERVER_T0 + SKEW;
    recordHubDateHeader(new Date(SERVER_T0).toUTCString(), browser, browser);
    expect(hubClockOffsetMs()).toBe(-SKEW + 500);
  });

  it('takes the median, so one stale (cached) Date does not move it', () => {
    for (let i = 0; i < 4; i++) recordHubTime(1_000 + i, 1_000, 1_000);
    recordHubTime(1_000 - 3_600_000, 1_000, 1_000); // an hour-old cached copy
    expect(Math.abs(hubClockOffsetMs())).toBeLessThan(10);
  });

  it('keeps only recent samples, so a corrected browser clock wins', () => {
    for (let i = 0; i < 10; i++) recordHubTime(0, 60_000, 60_000);
    expect(hubClockOffsetMs()).toBe(-60_000);
    for (let i = 0; i < 7; i++) recordHubTime(0, 0, 0);
    expect(hubClockOffsetMs()).toBe(0);
  });

  it('ignores missing or bad headers and slow or inverted requests', () => {
    recordHubDateHeader(null, 0, 0);
    recordHubDateHeader('not a date', 0, 0);
    recordHubTime(5_000, 0, 20_000); // 20s round trip
    recordHubTime(5_000, 10, 0); // received before sent
    recordHubTime(Number.NaN, 0, 0);
    expect(hubClockOffsetMs()).toBe(0);
  });

  it('apiFetch records the Date header of every response', async () => {
    vi.useFakeTimers({ toFake: ['Date'], now: SERVER_T0 + SKEW });
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response('{}', { status: 200, headers: { Date: new Date(SERVER_T0).toUTCString() } })
        )
      )
    );
    await apiFetch('/api/v1/agents');
    expect(hubClockOffsetMs()).toBe(-SKEW + 500);
    expect(hubNow()).toBe(SERVER_T0 + 500);
  });

  it('apiFetch tolerates a response without headers', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve({ ok: true, status: 200 } as Response))
    );
    await expect(apiFetch('/x')).resolves.toBeDefined();
    expect(hubClockOffsetMs()).toBe(0);
  });
});

class Host implements ReactiveControllerHost {
  controllers: ReactiveController[] = [];
  updates = 0;
  addController(c: ReactiveController): void {
    this.controllers.push(c);
  }
  removeController(): void {}
  requestUpdate(): void {
    this.updates++;
  }
  get updateComplete(): Promise<boolean> {
    return Promise.resolve(true);
  }
  updated(): void {
    for (const c of this.controllers) c.hostUpdated?.();
  }
}

describe('ptone/scion#2952 acceptance: +90s browser skew', () => {
  function deleting(leaseServerMs: number): DeletionInfo {
    return {
      state: 'deleting',
      soft: false,
      claim: 1,
      startedAt: iso(SERVER_T0),
      leaseExpiresAt: iso(leaseServerMs),
    };
  }

  it('a renewed deleting view stays Deleting…, and flips at the server-time lease', () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'], now: SERVER_T0 + SKEW });
    // The page's first API response carries the hub's Date.
    recordHubDateHeader(new Date(SERVER_T0).toUTCString(), Date.now(), Date.now());

    const agent: Pick<Agent, 'deletion'> = { deletion: deleting(SERVER_T0 + 60_000) };
    const host = new Host();
    const ctl = new DeletionLeaseController(host, () => [agent]); // default (hub) clock
    ctl.hostConnected();

    // Without the correction the browser (90s ahead) would already read
    // this 60s lease as lapsed.
    expect(Date.now()).toBeGreaterThan(SERVER_T0 + 60_000);
    expect(ctl.isDeleting(agent)).toBe(true);

    // Renewals every 20s push the lease out; the view stays Deleting….
    for (let renewal = 1; renewal <= 3; renewal++) {
      vi.advanceTimersByTime(20_000);
      agent.deletion = deleting(SERVER_T0 + renewal * 20_000 + 60_000);
      host.updated();
      expect(ctl.isDeleting(agent)).toBe(true);
      expect(ctl.view(agent)?.state).toBe('deleting');
    }

    // The engine dies: last lease is server T0+120s. The flip happens at
    // that server instant (allowing the Date header's ±0.5s), not 90s early.
    const lastLease = SERVER_T0 + 120_000;
    // Now at server T0+60s. One second before the lease (true server
    // time), still Deleting….
    vi.advanceTimersByTime(59_000 - 1_000);
    expect(Date.now() - SKEW).toBe(lastLease - 2_000);
    expect(ctl.isDeleting(agent)).toBe(true);
    const updatesBefore = host.updates;
    vi.advanceTimersByTime(2_000);
    expect(host.updates).toBe(updatesBefore + 1); // the lease timer fired
    expect(ctl.view(agent)).toMatchObject({ state: 'failed', code: 'abandoned' });
    expect(hubNow()).toBeGreaterThanOrEqual(lastLease - 500);
    expect(hubNow()).toBeLessThanOrEqual(lastLease + 1_000);
  });
});
