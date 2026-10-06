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
 * Notification tray behaviour when the signed-in user changes.
 *
 * Every list request is answered through a deferred response, so each test
 * decides exactly when a response lands relative to the user change: a
 * response for a previous user (or one arriving after sign-out) must be
 * dropped, and the next user's first fetch must behave like a first load.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import { PUSH_STORAGE_KEY } from '../../client/push-preference.js';

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

let popups: string[] = [];

class FakeNotification {
  static permission: NotificationPermission = 'granted';
  static requestPermission = vi.fn(async (): Promise<NotificationPermission> => 'granted');
  constructor(_title: string, options: NotificationOptions = {}) {
    popups.push(options.tag ?? '');
  }
}

function user(id: string): any {
  return { id, email: `${id}@example.com`, displayName: id };
}

function notif(id: string): any {
  return {
    id,
    status: 'COMPLETED',
    message: `${id} done`,
    agentId: 'agent-1',
    createdAt: new Date().toISOString(),
  };
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
  return tray.notifications.map((n: any) => n.id);
}

/** An attached tray signed in as u1 whose initial fetch returned items. */
async function trayAs(u: any, items: any[]): Promise<any> {
  const tray: any = document.createElement('scion-notification-tray');
  tray.user = u;
  document.body.appendChild(tray);
  await tray.updateComplete;
  await flush();
  // Mounting can start more than one list request; answer them all.
  expect(pending.length).toBeGreaterThan(0);
  for (const req of pending.splice(0)) await respond(req, items);
  return tray;
}

describe('notification tray: user switch', () => {
  beforeAll(async () => {
    await import('./notification-tray.js');
  });

  beforeEach(() => {
    pending.length = 0;
    popups = [];
    FakeNotification.permission = 'granted';
    (window as unknown as { Notification: unknown }).Notification = FakeNotification;
    localStorage.setItem(PUSH_STORAGE_KEY, 'true');
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.useRealTimers();
    localStorage.clear();
  });

  it('drops a response for a previous user from a fetch started by a user change', async () => {
    const tray: any = document.createElement('scion-notification-tray');
    document.body.appendChild(tray);
    await tray.updateComplete;
    expect(pending).toHaveLength(0);

    await setUser(tray, user('u1'));
    const u1Req = takeList();

    await setUser(tray, user('u2'));
    const u2Req = takeList();

    await respond(u1Req, [notif('u1-a')]);
    expect(ids(tray)).toEqual([]);

    await respond(u2Req, [notif('u2-a')]);
    expect(ids(tray)).toEqual(['u2-a']);
    // The dropped response did not count as a first load, so u2's first
    // fetch still suppresses push.
    expect(popups).toEqual([]);
  });

  it('drops a poll response for a previous user', async () => {
    const tray = await trayAs(user('u1'), [notif('u1-a')]);

    vi.advanceTimersByTime(POLL_INTERVAL_MS);
    const pollReq = takeList();

    await setUser(tray, user('u2'));
    const u2Req = takeList();

    await respond(pollReq, [notif('u1-a'), notif('u1-b')]);
    expect(ids(tray)).toEqual([]);

    await respond(u2Req, [notif('u2-a')]);
    expect(ids(tray)).toEqual(['u2-a']);
  });

  it('drops an event-triggered response for a previous user', async () => {
    const tray = await trayAs(user('u1'), [notif('u1-a')]);

    stateManager.dispatchEvent(new Event('notification-created'));
    const sseReq = takeList();

    await setUser(tray, user('u2'));
    const u2Req = takeList();

    // The new user's response landing first must not be overwritten.
    await respond(u2Req, [notif('u2-a')]);
    expect(ids(tray)).toEqual(['u2-a']);

    await respond(sseReq, [notif('u1-a'), notif('u1-b')]);
    expect(ids(tray)).toEqual(['u2-a']);
    // Push is armed for u2 here, yet the dropped response pushes nothing.
    expect(popups).toEqual([]);

    // The dropped response did not mark u1's items as seen for u2: the next
    // fetch pushes only the item that is new to u2.
    stateManager.dispatchEvent(new Event('notification-created'));
    await respond(takeList(), [notif('u2-a'), notif('u2-b')]);
    expect(ids(tray)).toEqual(['u2-a', 'u2-b']);
    expect(popups).toEqual(['u2-b']);
  });

  it('drops a refresh-on-open response for a previous user', async () => {
    const tray = await trayAs(user('u1'), [notif('u1-a')]);

    tray.toggle();
    const openReq = takeList();

    await setUser(tray, user('u2'));
    const u2Req = takeList();

    await respond(openReq, [notif('u1-a')]);
    expect(ids(tray)).toEqual([]);

    await respond(u2Req, [notif('u2-a')]);
    expect(ids(tray)).toEqual(['u2-a']);
  });

  it('does not show the previous user list while the next user fetch is in flight', async () => {
    const tray = await trayAs(user('u1'), [notif('u1-a')]);
    expect(ids(tray)).toEqual(['u1-a']);

    await setUser(tray, user('u2'));
    expect(ids(tray)).toEqual([]);

    await respond(takeList(), [notif('u2-a')]);
    expect(ids(tray)).toEqual(['u2-a']);
  });

  it('keeps the list cleared when a response lands after sign-out', async () => {
    const tray = await trayAs(user('u1'), [notif('u1-a')]);

    stateManager.dispatchEvent(new Event('notification-created'));
    const sseReq = takeList();

    await setUser(tray, null);
    expect(ids(tray)).toEqual([]);

    await respond(sseReq, [notif('u1-a'), notif('u1-b')]);
    expect(ids(tray)).toEqual([]);
    expect(popups).toEqual([]);
  });

  it('keeps the list cleared when a poll response lands after sign-out', async () => {
    const tray = await trayAs(user('u1'), [notif('u1-a')]);

    vi.advanceTimersByTime(POLL_INTERVAL_MS);
    const pollReq = takeList();

    await setUser(tray, null);
    await respond(pollReq, [notif('u1-a')]);
    expect(ids(tray)).toEqual([]);
  });

  it('treats the next user first fetch as a first load', async () => {
    const tray = await trayAs(user('u1'), [notif('shared-a')]);

    await setUser(tray, user('u2'));
    // Same id as u1's item plus one u1 never saw: neither pushes, because
    // this is u2's first fetch.
    await respond(takeList(), [notif('shared-a'), notif('u2-b')]);
    expect(ids(tray)).toEqual(['shared-a', 'u2-b']);
    expect(popups).toEqual([]);

    // Later new items for u2 do push.
    stateManager.dispatchEvent(new Event('notification-created'));
    await respond(takeList(), [notif('shared-a'), notif('u2-b'), notif('u2-c')]);
    expect(popups).toEqual(['u2-c']);
  });

  it('resets seen notifications on user change', async () => {
    const tray = await trayAs(user('u1'), [notif('u1-a'), notif('u1-b')]);
    expect([...tray.seenIds]).toEqual(['u1-a', 'u1-b']);

    await setUser(tray, user('u2'));
    expect([...tray.seenIds]).toEqual([]);
    expect(tray.initialFetchDone).toBe(false);
  });

  it('suppresses push on the first fetch after signing back in', async () => {
    const tray = await trayAs(user('u1'), [notif('u1-a')]);

    await setUser(tray, null);
    await setUser(tray, user('u1'));
    await respond(takeList(), [notif('u1-a'), notif('u1-b')]);

    expect(ids(tray)).toEqual(['u1-a', 'u1-b']);
    expect(popups).toEqual([]);
  });

  it('applies responses for the current user in every path', async () => {
    const tray = await trayAs(user('u1'), [notif('a')]);
    expect(ids(tray)).toEqual(['a']);
    expect(popups).toEqual([]);

    vi.advanceTimersByTime(POLL_INTERVAL_MS);
    await respond(takeList(), [notif('a'), notif('b')]);
    expect(ids(tray)).toEqual(['a', 'b']);

    stateManager.dispatchEvent(new Event('notification-created'));
    await respond(takeList(), [notif('a'), notif('b'), notif('c')]);
    expect(ids(tray)).toEqual(['a', 'b', 'c']);

    tray.toggle();
    await respond(takeList(), [notif('a'), notif('b'), notif('c'), notif('d')]);
    expect(ids(tray)).toEqual(['a', 'b', 'c', 'd']);

    expect(popups).toEqual(['b', 'c', 'd']);
  });

  it('keeps state when the same user is handed over as a new object', async () => {
    const tray = await trayAs(user('u1'), [notif('a')]);

    await setUser(tray, user('u1'));
    expect(ids(tray)).toEqual(['a']);
    expect([...tray.seenIds]).toEqual(['a']);
    expect(tray.initialFetchDone).toBe(true);

    // A refetch for the same user may or may not start; answer it if it does.
    for (const req of pending.splice(0)) await respond(req, [notif('a')]);
    expect(ids(tray)).toEqual(['a']);

    stateManager.dispatchEvent(new Event('notification-created'));
    await respond(takeList(), [notif('a'), notif('b')]);
    expect(ids(tray)).toEqual(['a', 'b']);
    expect(popups).toEqual(['b']);
  });

  it('drops a response that starts with no user signed in', async () => {
    const tray: any = document.createElement('scion-notification-tray');
    document.body.appendChild(tray);
    await tray.updateComplete;
    expect(pending).toHaveLength(0);

    tray.toggle();
    await respond(takeList(), [notif('a')]);
    expect(ids(tray)).toEqual([]);
    expect(popups).toEqual([]);
  });

  it('never renders the next user with the previous user list', async () => {
    const tray = await trayAs(user('u1'), [notif('u1-a')]);
    const renders: { userId: string | null; ids: string[] }[] = [];
    const render = tray.render.bind(tray);
    tray.render = () => {
      renders.push({ userId: tray.user?.id ?? null, ids: ids(tray) });
      return render();
    };

    await setUser(tray, user('u2'));
    await respond(takeList(), [notif('u2-a')]);

    const u2Renders = renders.filter((r) => r.userId === 'u2');
    expect(u2Renders.length).toBeGreaterThan(0);
    for (const r of u2Renders) expect(r.ids.filter((id) => id.startsWith('u1-'))).toEqual([]);
  });

  it('drops acknowledge responses for a previous user', async () => {
    const tray = await trayAs(user('u1'), [notif('u1-a')]);

    tray.ackAll();
    const ackAllReq = pending.splice(0, 1)[0];
    expect(ackAllReq.url).toBe('/api/v1/notifications/ack-all');

    await setUser(tray, user('u2'));
    await respond(takeList(), [notif('u2-a'), notif('u2-b')]);

    await respond(ackAllReq, {});
    expect(ids(tray)).toEqual(['u2-a', 'u2-b']);
  });

  it('drops a single acknowledge response for a previous user', async () => {
    const tray = await trayAs(user('u1'), [notif('shared-a')]);

    tray.ackOne('shared-a');
    const ackReq = pending.splice(0, 1)[0];
    expect(ackReq.url).toBe('/api/v1/notifications/shared-a/ack');

    await setUser(tray, user('u2'));
    await respond(takeList(), [notif('shared-a')]);

    await respond(ackReq, {});
    expect(ids(tray)).toEqual(['shared-a']);
  });

  it('applies acknowledge responses for the current user', async () => {
    const tray = await trayAs(user('u1'), [notif('a'), notif('b'), notif('c')]);

    tray.ackOne('a');
    await respond(pending.splice(0, 1)[0], {});
    expect(ids(tray)).toEqual(['b', 'c']);

    tray.ackAll();
    await respond(pending.splice(0, 1)[0], {});
    expect(ids(tray)).toEqual([]);
  });
});
