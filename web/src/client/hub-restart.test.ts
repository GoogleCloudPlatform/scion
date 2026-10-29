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

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

import { parseGoUptimeSeconds, waitForHubRestart, type HubRestartResult } from './hub-restart.js';

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

type HealthAnswer = { uptime?: string; scionVersion?: string } | 'down' | number;

/**
 * Stubs fetch so /healthz answers come from `answers` in order; the last
 * answer repeats. 'down' is a network error, a number is that HTTP status.
 */
function stubHealthz(answers: HealthAnswer[]): ReturnType<typeof vi.fn> {
  let i = 0;
  const fn = vi.fn((input: string | URL | Request, _init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    if (!url.endsWith('/healthz')) return Promise.resolve(json({}, 404));
    const a = answers[Math.min(i, answers.length - 1)];
    i++;
    if (a === 'down') return Promise.reject(new TypeError('Failed to fetch'));
    if (typeof a === 'number') return Promise.resolve(json({}, a));
    return Promise.resolve(json(a));
  });
  vi.stubGlobal('fetch', fn);
  return fn;
}

/** Starts waitForHubRestart; the getter returns its result once settled. */
function start(opts: Parameters<typeof waitForHubRestart>[0]): () => HubRestartResult | undefined {
  let result: HubRestartResult | undefined;
  void waitForHubRestart(opts).then((r) => (result = r));
  return () => result;
}

/** Runs waitForHubRestart under fake timers for advanceMs. */
async function run(
  opts: Parameters<typeof waitForHubRestart>[0],
  advanceMs: number
): Promise<HubRestartResult | undefined> {
  const result = start(opts);
  await vi.advanceTimersByTimeAsync(advanceMs);
  return result();
}

describe('parseGoUptimeSeconds', () => {
  it.each([
    ['3s', 3],
    ['3.12s', 3.12],
    ['1m5s', 65],
    ['2h3m0s', 7380],
    ['850ms', 0.85],
    ['0s', 0],
  ])('parses %s', (input, want) => {
    expect(parseGoUptimeSeconds(input)).toBeCloseTo(want);
  });

  it('returns null for missing or unrecognized values', () => {
    expect(parseGoUptimeSeconds(undefined)).toBeNull();
    expect(parseGoUptimeSeconds('')).toBeNull();
    expect(parseGoUptimeSeconds('soon')).toBeNull();
  });
});

describe('waitForHubRestart', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it('resolves once the hub is back on the expected version with a fresh uptime', async () => {
    const fetch = stubHealthz([
      'down',
      502,
      { uptime: '5h0m0s', scionVersion: 'nightly-20261022' },
      { uptime: '4s', scionVersion: 'nightly-20261023' },
    ]);
    const result = await run({ expectVersion: 'nightly-20261023' }, 20_000);
    expect(result).toEqual({
      ok: true,
      observed: { version: 'nightly-20261023', uptime: '4s', uptimeSeconds: 4 },
    });
    expect(fetch).toHaveBeenCalledTimes(4);
  });

  it('keeps waiting while the hub answers fresh on another version, until timeout', async () => {
    stubHealthz([{ uptime: '3s', scionVersion: 'nightly-20261022' }]);
    const result = start({ expectVersion: 'nightly-20261023', timeoutMs: 10_000 });
    await vi.advanceTimersByTimeAsync(9_000);
    expect(result()).toBeUndefined();
    await vi.advanceTimersByTimeAsync(5_000);
    expect(result()).toEqual({
      ok: false,
      observed: { version: 'nightly-20261022', uptime: '3s', uptimeSeconds: 3 },
    });
  });

  it('defaults to a five minute timeout', async () => {
    stubHealthz(['down']);
    const result = start({});
    await vi.advanceTimersByTimeAsync(298_000);
    expect(result()).toBeUndefined();
    await vi.advanceTimersByTimeAsync(5_000);
    expect(result()).toEqual({ ok: false, observed: null });
  });

  it('does not count a hub with an old uptime as restarted', async () => {
    stubHealthz([{ uptime: '10m0s', scionVersion: 'v1' }]);
    const result = await run({ timeoutMs: 5_000 }, 10_000);
    expect(result).toEqual({
      ok: false,
      observed: { version: 'v1', uptime: '10m0s', uptimeSeconds: 600 },
    });
  });

  it('without expectVersion, any fresh uptime counts', async () => {
    stubHealthz([{ uptime: '10m0s' }, { uptime: '1s', scionVersion: 'abc123' }]);
    const result = await run({}, 10_000);
    expect(result?.ok).toBe(true);
  });

  it('stops polling and resolves aborted when the signal aborts', async () => {
    const fetch = stubHealthz(['down']);
    const ctrl = new AbortController();
    const result = start({ signal: ctrl.signal });
    await vi.advanceTimersByTimeAsync(3_000);
    const callsBefore = fetch.mock.calls.length;
    expect(callsBefore).toBeGreaterThan(0);
    ctrl.abort();
    await vi.advanceTimersByTimeAsync(0);
    expect(result()).toEqual({ ok: false, observed: null, aborted: true });
    await vi.advanceTimersByTimeAsync(30_000);
    expect(fetch.mock.calls.length).toBe(callsBefore);
  });

  it('passes the signal to fetch', async () => {
    const fetch = stubHealthz([{ uptime: '1s' }]);
    const ctrl = new AbortController();
    await run({ signal: ctrl.signal }, 1_000);
    const init = fetch.mock.calls[0][1] as RequestInit | undefined;
    expect(init?.signal).toBe(ctrl.signal);
  });

  it('ends early when stopWhen says so, only for answers that do not count', async () => {
    stubHealthz(['down', { uptime: '2h0m0s', scionVersion: 'v1' }]);
    const stopWhen = vi.fn(() => Promise.resolve(true));
    const result = await run({ expectVersion: 'v2', stopWhen }, 5_000);
    expect(stopWhen).toHaveBeenCalledTimes(1);
    expect(result).toEqual({
      ok: false,
      observed: { version: 'v1', uptime: '2h0m0s', uptimeSeconds: 7200 },
      stopped: true,
    });
  });

  it('keeps waiting while stopWhen says no, and never asks once restarted', async () => {
    stubHealthz([
      { uptime: '2h0m0s', scionVersion: 'v1' },
      { uptime: '2s', scionVersion: 'v2' },
    ]);
    const stopWhen = vi.fn(() => Promise.resolve(false));
    const result = await run({ expectVersion: 'v2', stopWhen }, 5_000);
    expect(result?.ok).toBe(true);
    expect(stopWhen).toHaveBeenCalledTimes(1);
  });
});
