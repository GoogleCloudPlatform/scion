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
 * Inbox tray loading: one request per signed-in user, not per render or per
 * user object, with the fallback poll, the SSE refetch and the refetch on open
 * kept as they were.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import { apiFetch } from '../../client/api.js';
import { stateManager } from '../../client/state.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../client/api.js', () => ({
  apiFetch: vi.fn(),
}));

const POLL_MS = 5 * 60_000;
const LIST_URL = '/api/v1/messages?unread=true';
const fetchMock = apiFetch as unknown as ReturnType<typeof vi.fn>;

let server: any[] = [];
let trays: any[] = [];

function user(id: string): any {
  return { id, email: `${id}@example.com`, name: id };
}

function message(id: string): any {
  return {
    id,
    sender: 'agent:helper',
    msg: `message ${id}`,
    type: 'instruction',
    createdAt: new Date().toISOString(),
  };
}

function listRequests(): number {
  return fetchMock.mock.calls.filter((c) => c[0] === LIST_URL).length;
}

async function mount(initialUser: any): Promise<any> {
  const tray: any = document.createElement('scion-inbox-tray');
  tray.user = initialUser;
  document.body.appendChild(tray);
  trays.push(tray);
  await tray.updateComplete;
  await vi.advanceTimersByTimeAsync(0);
  return tray;
}

async function setUser(tray: any, u: any): Promise<void> {
  tray.user = u;
  await tray.updateComplete;
  await vi.advanceTimersByTimeAsync(0);
}

async function messageEvent(): Promise<void> {
  stateManager.dispatchEvent(new CustomEvent('user-message-created', { detail: {} }));
  await vi.advanceTimersByTimeAsync(0);
}

describe('inbox tray: loading for the signed-in user', () => {
  beforeAll(async () => {
    await import('./inbox-tray.js');
  });

  beforeEach(() => {
    vi.useFakeTimers();
    server = [];
    trays = [];
    fetchMock.mockReset();
    fetchMock.mockImplementation(() =>
      Promise.resolve(new Response(JSON.stringify({ items: server }), { status: 200 }))
    );
  });

  afterEach(() => {
    for (const t of trays) t.remove();
    vi.useRealTimers();
  });

  it('sends one request when mounted with a user', async () => {
    await mount(user('u1'));

    expect(listRequests()).toBe(1);
    expect(vi.getTimerCount()).toBe(1);
    await messageEvent();
    expect(listRequests()).toBe(2);
  });

  it('sends one request when the user arrives after mount', async () => {
    const tray = await mount(null);
    expect(listRequests()).toBe(0);
    expect(vi.getTimerCount()).toBe(0);

    await setUser(tray, user('u1'));

    expect(listRequests()).toBe(1);
    expect(vi.getTimerCount()).toBe(1);
    await messageEvent();
    expect(listRequests()).toBe(2);
  });

  it('does not refetch for a new object of the same user', async () => {
    const tray = await mount(user('u1'));
    const timer = tray.pollTimer;

    await setUser(tray, user('u1'));

    expect(listRequests()).toBe(1);
    expect(tray.pollTimer).toBe(timer);
  });

  it('refetches once when a different user signs in', async () => {
    const tray = await mount(user('u1'));

    await setUser(tray, user('u2'));

    expect(listRequests()).toBe(2);
    expect(vi.getTimerCount()).toBe(1);
  });

  it('clears the list and stops loading when the user signs out', async () => {
    server = [message('m1')];
    const tray = await mount(user('u1'));
    expect(tray.messages).toHaveLength(1);

    await setUser(tray, null);

    expect(tray.messages).toEqual([]);
    expect(tray.pollTimer).toBeNull();
    expect(vi.getTimerCount()).toBe(0);
    await messageEvent();
    await vi.advanceTimersByTimeAsync(POLL_MS * 2);
    expect(listRequests()).toBe(1);
  });

  it('loads again after being removed and re-added', async () => {
    const tray = await mount(user('u1'));
    tray.remove();
    expect(vi.getTimerCount()).toBe(0);

    document.body.appendChild(tray);
    await tray.updateComplete;
    await vi.advanceTimersByTimeAsync(0);

    expect(listRequests()).toBe(2);
    expect(vi.getTimerCount()).toBe(1);
  });

  it('polls on the fallback interval', async () => {
    await mount(user('u1'));

    await vi.advanceTimersByTimeAsync(POLL_MS);
    expect(listRequests()).toBe(2);
    await vi.advanceTimersByTimeAsync(POLL_MS);
    expect(listRequests()).toBe(3);
  });

  it('refetches on a new message event and shows the result', async () => {
    const tray = await mount(user('u1'));
    expect(tray.messages).toEqual([]);

    server = [message('m1')];
    await messageEvent();

    expect(listRequests()).toBe(2);
    expect(tray.messages.map((m: any) => m.id)).toEqual(['m1']);
  });

  it('refetches when the panel opens', async () => {
    const tray = await mount(user('u1'));

    tray.toggle();
    await vi.advanceTimersByTimeAsync(0);

    expect(listRequests()).toBe(2);
  });
});
