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
 * Tests for the hub-scope project capability helper (design §5.F): one
 * GET /api/v1/projects?limit=1, cached per page load, fail-closed.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

import {
  fetchHubProjectCapabilities,
  seedHubProjectCapabilities,
  resetHubProjectCapabilitiesCache,
} from './hub-capabilities.js';
import { can } from '../shared/types.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('fetchHubProjectCapabilities', () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    resetHubProjectCapabilitiesCache();
    fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('reads _capabilities from GET /api/v1/projects?limit=1', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ projects: [], _capabilities: { actions: ['list', 'create'] } })
    );

    const caps = await fetchHubProjectCapabilities();

    expect(caps).toEqual({ actions: ['list', 'create'] });
    expect(can(caps, 'create')).toBe(true);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(String(fetchMock.mock.calls[0][0])).toContain('/api/v1/projects?limit=1');
  });

  it('returns viewer capabilities without create', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ projects: [], _capabilities: { actions: ['list'] } })
    );

    const caps = await fetchHubProjectCapabilities();

    expect(can(caps, 'create')).toBe(false);
  });

  it('caches a successful result for the page load', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ projects: [], _capabilities: { actions: ['create'] } })
    );

    await fetchHubProjectCapabilities();
    await fetchHubProjectCapabilities();

    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it('shares one request between concurrent callers', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ projects: [], _capabilities: { actions: ['create'] } })
    );

    const [a, b] = await Promise.all([
      fetchHubProjectCapabilities(),
      fetchHubProjectCapabilities(),
    ]);

    expect(a).toEqual(b);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it('fails closed on an HTTP error and does not cache the failure', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ error: { code: 'internal' } }, 500));

    const first = await fetchHubProjectCapabilities();
    expect(first).toBeUndefined();
    expect(can(first, 'create')).toBe(false);

    fetchMock.mockResolvedValueOnce(
      jsonResponse({ projects: [], _capabilities: { actions: ['create'] } })
    );
    const second = await fetchHubProjectCapabilities();
    expect(can(second, 'create')).toBe(true);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('fails closed on a network error', async () => {
    fetchMock.mockRejectedValue(new TypeError('Failed to fetch'));

    const caps = await fetchHubProjectCapabilities();

    expect(caps).toBeUndefined();
  });

  it('fails closed when the response has no _capabilities', async () => {
    fetchMock.mockResolvedValue(jsonResponse({ projects: [] }));

    expect(await fetchHubProjectCapabilities()).toBeUndefined();
  });

  it('fails closed on a malformed _capabilities value', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ projects: [], _capabilities: { actions: 'create' } })
    );

    expect(await fetchHubProjectCapabilities()).toBeUndefined();
  });

  it('fails closed on a legacy bare-array response', async () => {
    fetchMock.mockResolvedValue(jsonResponse([]));

    expect(await fetchHubProjectCapabilities()).toBeUndefined();
  });

  it('uses seeded capabilities without a request', async () => {
    seedHubProjectCapabilities({ actions: ['create'] });

    const caps = await fetchHubProjectCapabilities();

    expect(can(caps, 'create')).toBe(true);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('ignores an undefined seed', async () => {
    seedHubProjectCapabilities(undefined);
    fetchMock.mockResolvedValue(jsonResponse({ projects: [], _capabilities: { actions: [] } }));

    await fetchHubProjectCapabilities();

    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});
