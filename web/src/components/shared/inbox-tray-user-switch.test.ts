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
 * Inbox tray behaviour when the signed-in user changes.
 *
 * Every list request is answered through a deferred response, so each test
 * decides exactly when a response lands relative to the user change: a
 * response for a previous user (or one arriving after sign-out) must be
 * dropped, and the next user only ever sees their own messages.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

/* eslint-disable @typescript-eslint/no-explicit-any */

interface Pending {
  url: string;
  method: string;
  resolve: (body: unknown) => void;
}

const pending: Pending[] = [];

vi.mock('../../client/api.js', () => ({
  apiFetch: vi.fn(
    (url: string, init?: RequestInit) =>
      new Promise<Response>((resolve) => {
        pending.push({
          url,
          method: init?.method ?? 'GET',
          resolve: (body) => resolve(new Response(JSON.stringify(body), { status: 200 })),
        });
      })
  ),
}));

vi.mock('../../client/state.js', () => ({
  stateManager: new EventTarget(),
}));

const { stateManager } = await import('../../client/state.js');

const POLL_INTERVAL_MS = 5 * 60_000;

function user(id: string): any {
  return { id, email: `${id}@example.com`, displayName: id };
}

function msg(id: string): any {
  return {
    id,
    sender: 'agent:helper',
    msg: `${id} body`,
    type: 'instruction',
    createdAt: new Date().toISOString(),
  };
}

function page(...items: any[]): any {
  return { items };
}

/** Lets pending promise chains (fetch, json, Lit update) settle. */
async function flush(): Promise<void> {
  for (let i = 0; i < 5; i++) await new Promise((r) => setTimeout(r, 0));
}

/** Takes the single outstanding list request. */
function takeList(): Pending {
  const lists = pending.filter((p) => p.method === 'GET');
  expect(lists).toHaveLength(1);
  pending.splice(pending.indexOf(lists[0]), 1);
  return lists[0];
}

async function respond(req: Pending, body: unknown): Promise<void> {
  req.resolve(body);
  await flush();
}

async function setUser(tray: any, u: any): Promise<void> {
  tray.user = u;
  await tray.updateComplete;
  await flush();
}

function ids(tray: any): string[] {
  return tray.messages.map((m: any) => m.id);
}

/** An attached tray signed in as u whose initial fetch returned body. */
async function trayAs(u: any, body: any): Promise<any> {
  const tray: any = document.createElement('scion-inbox-tray');
  tray.user = u;
  document.body.appendChild(tray);
  await tray.updateComplete;
  await flush();
  // Mounting can start more than one list request; answer them all.
  expect(pending.length).toBeGreaterThan(0);
  for (const req of pending.splice(0)) await respond(req, body);
  return tray;
}

/** The unread count shown on the envelope badge, or 0 when hidden. */
function badgeCount(tray: any): number {
  const badge = tray.shadowRoot?.querySelector('.badge');
  return badge ? Number(badge.textContent?.trim()) : 0;
}

describe('inbox tray: user switch', () => {
  beforeAll(async () => {
    await import('./inbox-tray.js');
  });

  beforeEach(() => {
    pending.length = 0;
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.useRealTimers();
  });

  it('drops a response for a previous user from a fetch started by a user change', async () => {
    const tray: any = document.createElement('scion-inbox-tray');
    document.body.appendChild(tray);
    await tray.updateComplete;
    expect(pending).toHaveLength(0);

    await setUser(tray, user('u1'));
    const u1Req = takeList();

    await setUser(tray, user('u2'));
    const u2Req = takeList();

    await respond(u1Req, page(msg('u1-a')));
    expect(ids(tray)).toEqual([]);
    expect(badgeCount(tray)).toBe(0);

    await respond(u2Req, page(msg('u2-a')));
    expect(ids(tray)).toEqual(['u2-a']);
    expect(badgeCount(tray)).toBe(1);
  });

  it('drops a poll response for a previous user', async () => {
    const tray = await trayAs(user('u1'), page(msg('u1-a')));

    vi.advanceTimersByTime(POLL_INTERVAL_MS);
    const pollReq = takeList();

    await setUser(tray, user('u2'));
    const u2Req = takeList();

    await respond(pollReq, page(msg('u1-a'), msg('u1-b')));
    expect(ids(tray)).toEqual([]);

    await respond(u2Req, page(msg('u2-a')));
    expect(ids(tray)).toEqual(['u2-a']);
  });

  it('drops an event-triggered response for a previous user', async () => {
    const tray = await trayAs(user('u1'), page(msg('u1-a')));

    stateManager.dispatchEvent(new Event('user-message-created'));
    const sseReq = takeList();

    await setUser(tray, user('u2'));
    const u2Req = takeList();

    // The new user's response landing first must not be overwritten.
    await respond(u2Req, page(msg('u2-a')));
    expect(ids(tray)).toEqual(['u2-a']);

    await respond(sseReq, page(msg('u1-a'), msg('u1-b')));
    expect(ids(tray)).toEqual(['u2-a']);
    expect(badgeCount(tray)).toBe(1);
  });

  it('drops a refresh-on-open response for a previous user', async () => {
    const tray = await trayAs(user('u1'), page(msg('u1-a')));

    tray.toggle();
    const openReq = takeList();

    await setUser(tray, user('u2'));
    const u2Req = takeList();

    await respond(openReq, page(msg('u1-a')));
    expect(ids(tray)).toEqual([]);

    await respond(u2Req, page(msg('u2-a')));
    expect(ids(tray)).toEqual(['u2-a']);
  });

  it('does not show the previous user list while the next user fetch is in flight', async () => {
    const tray = await trayAs(user('u1'), page(msg('u1-a')));
    expect(ids(tray)).toEqual(['u1-a']);

    await setUser(tray, user('u2'));
    expect(ids(tray)).toEqual([]);
    expect(badgeCount(tray)).toBe(0);

    await respond(takeList(), page(msg('u2-a')));
    expect(ids(tray)).toEqual(['u2-a']);
  });

  it('keeps the list cleared when a response lands after sign-out', async () => {
    const tray = await trayAs(user('u1'), page(msg('u1-a')));

    stateManager.dispatchEvent(new Event('user-message-created'));
    const sseReq = takeList();

    await setUser(tray, null);
    expect(ids(tray)).toEqual([]);

    await respond(sseReq, page(msg('u1-a'), msg('u1-b')));
    expect(ids(tray)).toEqual([]);
  });

  it('keeps the list cleared when a poll response lands after sign-out', async () => {
    const tray = await trayAs(user('u1'), page(msg('u1-a')));

    vi.advanceTimersByTime(POLL_INTERVAL_MS);
    const pollReq = takeList();

    await setUser(tray, null);
    await respond(pollReq, page(msg('u1-a')));
    expect(ids(tray)).toEqual([]);
  });

  it('applies responses for the current user in every path', async () => {
    const tray = await trayAs(user('u1'), page(msg('a')));
    expect(ids(tray)).toEqual(['a']);

    vi.advanceTimersByTime(POLL_INTERVAL_MS);
    await respond(takeList(), page(msg('a'), msg('b')));
    expect(ids(tray)).toEqual(['a', 'b']);

    stateManager.dispatchEvent(new Event('user-message-created'));
    await respond(takeList(), page(msg('a'), msg('b'), msg('c')));
    expect(ids(tray)).toEqual(['a', 'b', 'c']);

    tray.toggle();
    await respond(takeList(), page(msg('a'), msg('b'), msg('c'), msg('d')));
    expect(ids(tray)).toEqual(['a', 'b', 'c', 'd']);
    expect(badgeCount(tray)).toBe(4);
  });

  it('keeps the list when the same user is handed over as a new object', async () => {
    const tray = await trayAs(user('u1'), page(msg('a')));

    await setUser(tray, user('u1'));
    expect(ids(tray)).toEqual(['a']);

    // A refetch for the same user may or may not start; answer it if it does.
    for (const req of pending.splice(0)) await respond(req, page(msg('a')));
    expect(ids(tray)).toEqual(['a']);

    stateManager.dispatchEvent(new Event('user-message-created'));
    await respond(takeList(), page(msg('a'), msg('b')));
    expect(ids(tray)).toEqual(['a', 'b']);
  });

  it('drops a response that starts with no user signed in', async () => {
    const tray: any = document.createElement('scion-inbox-tray');
    document.body.appendChild(tray);
    await tray.updateComplete;
    expect(pending).toHaveLength(0);

    tray.toggle();
    await respond(takeList(), page(msg('a')));
    expect(ids(tray)).toEqual([]);
    expect(badgeCount(tray)).toBe(0);
  });

  it('never renders the next user with the previous user list', async () => {
    const tray = await trayAs(user('u1'), page(msg('u1-a')));
    const renders: { userId: string | null; ids: string[] }[] = [];
    const render = tray.render.bind(tray);
    tray.render = () => {
      renders.push({ userId: tray.user?.id ?? null, ids: ids(tray) });
      return render();
    };

    await setUser(tray, user('u2'));
    await respond(takeList(), page(msg('u2-a')));

    const u2Renders = renders.filter((r) => r.userId === 'u2');
    expect(u2Renders.length).toBeGreaterThan(0);
    for (const r of u2Renders) expect(r.ids.filter((id) => id.startsWith('u1-'))).toEqual([]);
  });

  it('drops a mark-all-read response for a previous user', async () => {
    const tray = await trayAs(user('u1'), page(msg('u1-a')));

    tray.markAll();
    const markReq = pending.splice(0, 1)[0];
    expect(markReq.url).toBe('/api/v1/messages/read-all');

    await setUser(tray, user('u2'));
    await respond(takeList(), page(msg('u2-a'), msg('u2-b')));

    await respond(markReq, {});
    expect(ids(tray)).toEqual(['u2-a', 'u2-b']);
  });

  it('drops a mark-read response for a previous user', async () => {
    const tray = await trayAs(user('u1'), page(msg('shared-a')));

    tray.markOne('shared-a');
    const markReq = pending.splice(0, 1)[0];
    expect(markReq.url).toBe('/api/v1/messages/shared-a/read');

    await setUser(tray, user('u2'));
    await respond(takeList(), page(msg('shared-a')));

    await respond(markReq, {});
    expect(ids(tray)).toEqual(['shared-a']);
  });

  it('applies mark-read responses for the current user', async () => {
    const tray = await trayAs(user('u1'), page(msg('a'), msg('b'), msg('c')));

    tray.markOne('a');
    await respond(pending.splice(0, 1)[0], {});
    expect(ids(tray)).toEqual(['b', 'c']);

    tray.markAll();
    await respond(pending.splice(0, 1)[0], {});
    expect(ids(tray)).toEqual([]);
  });
});
