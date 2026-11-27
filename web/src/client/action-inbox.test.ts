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

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  ActionInboxApiError,
  buildActionInboxQuery,
  buildActionInboxUrlSearch,
  fetchActionInbox,
  fetchActionInboxProbe,
  getCachedActionInboxProbe,
  getLastKnownActionInboxAvailability,
  parseActionInboxUrlState,
  resetActionInboxProbeCacheForTests,
} from './action-inbox.js';
import { stateManager } from './state.js';

describe('action-inbox client helpers', () => {
  beforeEach(() => {
    resetActionInboxProbeCacheForTests();
  });

  afterEach(() => {
    resetActionInboxProbeCacheForTests();
    vi.unstubAllGlobals();
  });

  it('parses and serializes URL state with defaults and sanitization', () => {
    expect(parseActionInboxUrlState('')).toEqual({
      tab: 'open',
      project: '',
      agent: '',
      q: '',
      sort: 'recent',
      cursor: '',
    });

    const parsed = parseActionInboxUrlState(
      '?tab=resolved&sort=project&project=p1&agent=a2&q=TOKEN&cursor=cur-1'
    );
    expect(parsed).toEqual({
      tab: 'resolved',
      project: 'p1',
      agent: 'a2',
      q: 'TOKEN',
      sort: 'project',
      cursor: 'cur-1',
    });

    expect(buildActionInboxUrlSearch(parsed)).toBe(
      'tab=resolved&sort=project&project=p1&agent=a2&q=TOKEN&cursor=cur-1'
    );
    expect(buildActionInboxUrlSearch({ tab: 'open', sort: 'recent' })).toBe('');
    expect(parseActionInboxUrlState('?tab=bogus&sort=weird&project=all&q=bad\u0007')).toEqual({
      tab: 'open',
      project: '',
      agent: '',
      q: '',
      sort: 'recent',
      cursor: '',
    });
  });

  it('builds API query strings with optional limit', () => {
    expect(
      buildActionInboxQuery({ tab: 'open', sort: 'recent', project: 'p1' }, { limit: 0 })
    ).toBe('?tab=open&sort=recent&project=p1&limit=0');
  });

  it('caches ?limit=0 probes per kind and invalidates on message-action-updated', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(
      new Response(JSON.stringify({ counts: { answerable: 3, open: 4 } }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
    vi.stubGlobal('fetch', fetchMock);

    const first = await fetchActionInboxProbe('test_kind', '/api/v1/users/me/test-kind');
    expect(first).toEqual({ available: true, answerable: 3, open: 4 });
    expect(getCachedActionInboxProbe('test_kind')).toEqual(first);
    expect(getLastKnownActionInboxAvailability('test_kind')).toBe(true);

    // Cached second call does not re-fetch.
    const second = await fetchActionInboxProbe('test_kind', '/api/v1/users/me/test-kind');
    expect(second).toEqual(first);
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // Unrelated kind event does not invalidate 'test_kind'.
    stateManager.dispatchEvent(
      new CustomEvent('message-action-updated', { detail: { data: { kind: 'other_kind' } } })
    );
    expect(getCachedActionInboxProbe('test_kind')).toEqual(first);

    // Matching kind event marks cache dirty while preserving last-known availability.
    stateManager.dispatchEvent(
      new CustomEvent('message-action-updated', { detail: { data: { kind: 'test_kind' } } })
    );
    expect(getCachedActionInboxProbe('test_kind')).toBeNull();
    expect(getLastKnownActionInboxAvailability('test_kind')).toBe(true);
  });

  it('fetchActionInbox throws ActionInboxApiError on 404 and records unavailable probe', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn<typeof fetch>().mockResolvedValue(
        new Response(JSON.stringify({ error: { code: 'not_found', message: 'disabled' } }), {
          status: 404,
          headers: { 'Content-Type': 'application/json' },
        })
      )
    );

    await expect(
      fetchActionInbox('/api/v1/users/me/test-kind', {}, { kind: 'test_kind' })
    ).rejects.toBeInstanceOf(ActionInboxApiError);
    expect(getLastKnownActionInboxAvailability('test_kind')).toBe(false);
  });
});
