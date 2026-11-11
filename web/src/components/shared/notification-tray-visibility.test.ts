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
 * Notification tray: the 5-minute fallback poll pauses while the tab is
 * hidden and catches up with one fetch when the tab is shown again (C5b).
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import { apiFetch } from '../../client/api.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('[]', { status: 200 }))),
}));

const LIST_URL = '/api/v1/notifications?acknowledged=false';
const TEN_MINUTES = 10 * 60_000;
const POLL_MS = 5 * 60_000;

describe('notification tray: polling in a hidden tab', () => {
  const fetchMock = vi.mocked(apiFetch);
  let hidden = false;
  let trays: any[] = [];

  function setHidden(value: boolean): void {
    hidden = value;
    document.dispatchEvent(new Event('visibilitychange'));
  }

  function listRequests(): number {
    return fetchMock.mock.calls.filter((c) => c[0] === LIST_URL).length;
  }

  async function mount(): Promise<any> {
    const tray = document.createElement('scion-notification-tray') as any;
    tray.user = { id: 'u1', email: 'u1@example.com', name: 'u1' };
    document.body.appendChild(tray);
    trays.push(tray);
    await tray.updateComplete;
    await vi.advanceTimersByTimeAsync(0);
    return tray;
  }

  beforeAll(async () => {
    await import('./notification-tray.js');
  });

  beforeEach(() => {
    vi.useFakeTimers();
    hidden = false;
    trays = [];
    vi.spyOn(document, 'hidden', 'get').mockImplementation(() => hidden);
    fetchMock.mockReset();
    fetchMock.mockImplementation(() => Promise.resolve(new Response('[]', { status: 200 })));
  });

  afterEach(() => {
    for (const t of trays) t.remove();
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it('issues no fetches while hidden, then exactly one on becoming visible', async () => {
    await mount();
    expect(listRequests()).toBe(1); // the mount load

    setHidden(true);
    await vi.advanceTimersByTimeAsync(TEN_MINUTES);
    expect(listRequests()).toBe(1);

    setHidden(false);
    await vi.advanceTimersByTimeAsync(0);
    expect(listRequests()).toBe(2);
  });

  it('issues no polls when mounted in a hidden tab, then one on becoming visible', async () => {
    hidden = true;
    await mount();
    expect(listRequests()).toBe(1); // the mount load still runs

    await vi.advanceTimersByTimeAsync(TEN_MINUTES);
    expect(listRequests()).toBe(1);

    setHidden(false);
    await vi.advanceTimersByTimeAsync(0);
    expect(listRequests()).toBe(2);
  });

  it('keeps polling every 5 minutes while visible', async () => {
    await mount();
    await vi.advanceTimersByTimeAsync(POLL_MS * 2);
    expect(listRequests()).toBe(3);
  });

  it('stops polling on disconnect, including on a later visibility change', async () => {
    const tray = await mount();
    tray.remove();
    expect(tray.stopPollTimer).toBeNull();

    await vi.advanceTimersByTimeAsync(TEN_MINUTES);
    setHidden(true);
    setHidden(false);
    await vi.advanceTimersByTimeAsync(TEN_MINUTES);
    expect(listRequests()).toBe(1);
  });
});
