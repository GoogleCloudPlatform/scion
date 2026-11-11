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
 * Status source for the test-hub banner (ptone/scion#4240, phase W).
 */

import { afterEach, describe, expect, it, vi } from 'vitest';

import {
  TEST_INFRA_STATUS_EVENT,
  TEST_INFRA_STATUS_URL,
  getTestInfraStatus,
  loadTestInfraStatus,
  parseTestInfraStatus,
  resetTestInfraStatusForTests,
} from './test-infra-status.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

afterEach(() => {
  resetTestInfraStatusForTests();
  vi.unstubAllGlobals();
});

describe('parseTestInfraStatus', () => {
  it('reads the three booleans', () => {
    expect(
      parseTestInfraStatus({ testIdentities: true, testHubAdmin: false, testSuperAdmin: false })
    ).toEqual({ testIdentities: true, testHubAdmin: false, testSuperAdmin: false });
  });

  it('only a literal true turns a gate on', () => {
    expect(
      parseTestInfraStatus({ testIdentities: 'true', testHubAdmin: 1, testSuperAdmin: null })
    ).toEqual({ testIdentities: false, testHubAdmin: false, testSuperAdmin: false });
    expect(parseTestInfraStatus({})).toEqual({
      testIdentities: false,
      testHubAdmin: false,
      testSuperAdmin: false,
    });
  });

  it('rejects a body that is not an object', () => {
    expect(parseTestInfraStatus(null)).toBeNull();
    expect(parseTestInfraStatus('yes')).toBeNull();
    expect(parseTestInfraStatus([true, true, true])).toBeNull();
  });
});

describe('loadTestInfraStatus', () => {
  it('fetches the status endpoint once and shares the result', async () => {
    const fetchMock = vi.fn(() =>
      Promise.resolve(
        jsonResponse({ testIdentities: true, testHubAdmin: false, testSuperAdmin: false })
      )
    );
    vi.stubGlobal('fetch', fetchMock);
    const seen: unknown[] = [];
    const onStatus = (e: Event): void => {
      seen.push((e as CustomEvent).detail);
    };
    window.addEventListener(TEST_INFRA_STATUS_EVENT, onStatus);

    const [a, b] = await Promise.all([loadTestInfraStatus(), loadTestInfraStatus()]);
    await loadTestInfraStatus();
    window.removeEventListener(TEST_INFRA_STATUS_EVENT, onStatus);

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0]).toEqual([TEST_INFRA_STATUS_URL, { credentials: 'include' }]);
    expect(a).toEqual({ testIdentities: true, testHubAdmin: false, testSuperAdmin: false });
    expect(b).toBe(a);
    expect(getTestInfraStatus()).toBe(a);
    expect(seen).toEqual([a]);
  });

  it('retries once at once after a 401 (a stale session the web layer clears)', async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse({ error: { code: 'session_expired' } }, 401))
      .mockResolvedValueOnce(
        jsonResponse({ testIdentities: true, testHubAdmin: false, testSuperAdmin: false })
      );
    vi.stubGlobal('fetch', fetchMock);

    expect(await loadTestInfraStatus()).toEqual({
      testIdentities: true,
      testHubAdmin: false,
      testSuperAdmin: false,
    });
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('retries a 401 only once per call', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse({}, 401)));
    vi.stubGlobal('fetch', fetchMock);
    expect(await loadTestInfraStatus()).toBeNull();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('does not cache a failure, so the next call retries', async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(new Response('nope', { status: 500 }))
      .mockRejectedValueOnce(new TypeError('network'))
      .mockResolvedValueOnce(
        jsonResponse({ testIdentities: false, testHubAdmin: false, testSuperAdmin: false })
      );
    vi.stubGlobal('fetch', fetchMock);

    expect(await loadTestInfraStatus()).toBeNull();
    expect(getTestInfraStatus()).toBeNull();
    expect(await loadTestInfraStatus()).toBeNull();
    expect(await loadTestInfraStatus()).toEqual({
      testIdentities: false,
      testHubAdmin: false,
      testSuperAdmin: false,
    });
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });
});
