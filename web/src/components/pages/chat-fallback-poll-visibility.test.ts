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
 * Chat page: the 60-second fallback poll (unread DM state, human membership)
 * pauses while the tab is hidden and catches up with one poll when the tab
 * is shown again (C5b). The poll's loaders are stubbed so each poll counts
 * as one call of loadUnreadDMPeers, which every poll makes first; the last
 * test keeps the DM loader real and counts /chat/dms requests instead.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { apiFetch } from '../../client/api.js';
import { chatDMsLoad, chatSpacesLoad } from '../../client/chat-list-cache.js';
import { FakeEventSource } from '../../client/__fixtures__/agent-store-harness.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../client/main.js', () => import('../../client/__fixtures__/main-stub.js'));

vi.mock('../../client/api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/api.js')>();
  return {
    ...actual,
    apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
  };
});

const POLL_MS = 60_000;
const TEN_MINUTES = 10 * 60_000;

/** The loaders one fallback poll calls, stubbed on the instance. */
const POLL_LOADERS = [
  'loadUnreadDMPeers',
  'loadV2Members',
  'loadHubMembers',
  'refreshHubMemberPresence',
] as const;

describe('chat page — fallback poll in a hidden tab', () => {
  let hidden = false;
  let pages: any[] = [];

  function setHidden(value: boolean): void {
    hidden = value;
    document.dispatchEvent(new Event('visibilitychange'));
  }

  function dmListRequests(): number {
    return vi.mocked(apiFetch).mock.calls.filter(([path]) => path === '/api/v1/chat/dms').length;
  }

  /**
   * A connected page whose startup has settled, with the poll's loaders
   * stubbed afterwards (all of them, or all but the DM loader), so only
   * polls are counted. It never renders: these tests are about the poll,
   * not the view.
   */
  async function startedPage({ realDMLoader = false } = {}): Promise<any> {
    const el = document.createElement('scion-page-chat') as any;
    el.pageData = { user: { id: 'user-me' } };
    el.shouldUpdate = (): boolean => false;
    const startups: Promise<void>[] = [];
    const initV2 = el.initV2;
    if (typeof initV2 !== 'function') {
      throw new Error('initV2 is not a function on the chat page. Was it renamed or removed?');
    }
    // Deliberately shadows the private initV2 on this instance only.
    el.initV2 = function (this: unknown): Promise<void> {
      const startup = initV2.call(this);
      startups.push(startup);
      return startup;
    };
    window.history.replaceState({}, '', '/chat');
    document.body.appendChild(el);
    pages.push(el);
    await Promise.all(startups);
    expect(startups).toHaveLength(1);
    for (const name of POLL_LOADERS) {
      if (realDMLoader && name === 'loadUnreadDMPeers') continue;
      // Deliberately shadows the private method on this instance only.
      el[name] = vi.fn(() => Promise.resolve(true));
    }
    vi.mocked(apiFetch).mockClear();
    return el;
  }

  function polls(el: any): number {
    return el.loadUnreadDMPeers.mock.calls.length;
  }

  beforeAll(async () => {
    await import('./chat.js');
    await Promise.all([
      import('../shared/chat/chat-space-rail.js'),
      import('../shared/chat/chat-members.js'),
    ]);
  });

  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    chatDMsLoad.invalidate();
    chatSpacesLoad.invalidate();
    // Only the interval and the clock: startup awaits imports and requests.
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'Date'] });
    hidden = false;
    pages = [];
    vi.spyOn(document, 'hidden', 'get').mockImplementation(() => hidden);
  });

  afterEach(() => {
    for (const el of pages) el.remove();
    vi.useRealTimers();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('polls every 60 seconds while visible', async () => {
    const el = await startedPage();
    vi.advanceTimersByTime(POLL_MS * 3);
    expect(polls(el)).toBe(3);
  });

  it('issues no polls while hidden, then exactly one on becoming visible', async () => {
    const el = await startedPage();

    setHidden(true);
    vi.advanceTimersByTime(TEN_MINUTES);
    expect(polls(el)).toBe(0);

    setHidden(false);
    expect(polls(el)).toBe(1);
    // Back on the normal cadence afterwards.
    vi.advanceTimersByTime(POLL_MS);
    expect(polls(el)).toBe(2);
  });

  it('issues no polls when started in a hidden tab, then one on becoming visible', async () => {
    hidden = true;
    const el = await startedPage();

    vi.advanceTimersByTime(TEN_MINUTES);
    expect(polls(el)).toBe(0);

    setHidden(false);
    expect(polls(el)).toBe(1);
  });

  it('stops polling on disconnect, including on a later visibility change', async () => {
    const el = await startedPage();
    el.remove();
    expect(el._stopFallbackPoll).toBeNull();

    vi.advanceTimersByTime(TEN_MINUTES);
    setHidden(true);
    setHidden(false);
    vi.advanceTimersByTime(TEN_MINUTES);
    expect(polls(el)).toBe(0);
  });

  it('a regular tick fetches the DM list fresh', async () => {
    const el = await startedPage({ realDMLoader: true });
    vi.advanceTimersByTime(POLL_MS);
    vi.advanceTimersByTime(POLL_MS);
    expect(dmListRequests()).toBe(2);
    el.remove();
  });
});
