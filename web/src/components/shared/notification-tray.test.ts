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
 * Notification tray reactions to `notification-created` events: a
 * user-scoped event carrying the whole row is shown without a request, and
 * everything else is fetched, a burst coalesced into few requests.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import { PUSH_STORAGE_KEYS } from '../../client/push-preference.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

const { apiFetch } = vi.hoisted(() => ({ apiFetch: vi.fn() }));
vi.mock('../../client/api.js', () => ({ apiFetch }));

vi.mock('../../client/state.js', () => import('../../client/__fixtures__/state-stub.js'));

const { stateManager } = await import('../../client/state.js');
const { notificationFromEvent } = await import('./notification-tray.js');

let popups: string[] = [];

class FakeNotification {
  static permission: NotificationPermission = 'granted';
  static requestPermission = vi.fn(
    (): Promise<NotificationPermission> => Promise.resolve('granted')
  );
  constructor(_title: string, options: NotificationOptions = {}) {
    popups.push(options.tag ?? '');
  }
}

function notif(id: string): any {
  return {
    id,
    status: 'COMPLETED',
    message: `${id} done`,
    agentId: 'agent-1',
    createdAt: '2026-10-19T00:00:00.000Z',
  };
}

/** The payload the hub publishes on user.{id}.notification (UserNotificationEvent). */
function userEventPayload(id: string, subscriberId = 'u1'): Record<string, unknown> {
  return {
    id,
    agentId: 'agent-2',
    projectId: 'project-1',
    status: 'SCHEDULE_BLOCKED',
    message: `${id} blocked`,
    createdAt: '2026-10-19T00:01:00.000Z',
    subscriberId,
  };
}

/** The user-scoped path: notifyWithData wraps the payload as { state, data }. */
function emitUserScoped(data: unknown): void {
  stateManager.dispatchEvent(
    new CustomEvent('notification-created', { detail: { state: {}, data } })
  );
}

/** The bare notification.created path: notify() passes the state alone. */
function emitBare(): void {
  stateManager.dispatchEvent(new CustomEvent('notification-created', { detail: {} }));
}

function listFetches(): number {
  return apiFetch.mock.calls.filter(([url]) => String(url).startsWith('/api/v1/notifications?'))
    .length;
}

function ids(tray: any): string[] {
  return tray.notifications.map((n: any) => n.id);
}

/** An attached tray signed in as u1 whose initial fetch returned `items`. */
async function mountTray(items: any[]): Promise<any> {
  apiFetch.mockImplementation(() =>
    Promise.resolve(new Response(JSON.stringify(items), { status: 200 }))
  );
  const tray: any = document.createElement('scion-notification-tray');
  tray.user = { id: 'u1', email: 'u1@example.com', displayName: 'u1' };
  document.body.appendChild(tray);
  await tray.updateComplete;
  await vi.waitFor(() => expect(ids(tray)).toEqual(items.map((n) => n.id)));
  await vi.advanceTimersByTimeAsync(0);
  apiFetch.mockClear();
  return tray;
}

describe('notification tray: event reactions', () => {
  beforeAll(async () => {
    await import('./notification-tray.js');
  });

  beforeEach(() => {
    apiFetch.mockReset();
    popups = [];
    FakeNotification.permission = 'granted';
    (window as unknown as { Notification: unknown }).Notification = FakeNotification;
    localStorage.setItem(PUSH_STORAGE_KEYS.agent, 'true');
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval'] });
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.useRealTimers();
    localStorage.clear();
  });

  it('user-scoped notification with full payload is upserted without fetch', async () => {
    const tray = await mountTray([notif('old')]);

    emitUserScoped(userEventPayload('new'));
    await tray.updateComplete;
    await vi.advanceTimersByTimeAsync(5000);

    expect(listFetches()).toBe(0);
    // Newest first, as the endpoint orders the list.
    expect(ids(tray)).toEqual(['new', 'old']);
    // A genuinely new row pops, as a fetched one would.
    expect(popups).toEqual(['new']);

    // The row is rendered.
    tray.open = true;
    await tray.updateComplete;
    const messages = [...tray.shadowRoot.querySelectorAll('.notif-message')].map((el: Element) =>
      el.textContent?.trim()
    );
    expect(messages).toEqual(['new blocked', 'old done']);
  });

  it('a payload without an acknowledged field is upserted (a new row is unacknowledged)', async () => {
    const tray = await mountTray([]);
    const payload = userEventPayload('n1');
    expect('acknowledged' in payload).toBe(false);

    emitUserScoped(payload);
    await vi.advanceTimersByTimeAsync(5000);

    expect(listFetches()).toBe(0);
    expect(ids(tray)).toEqual(['n1']);
  });

  it('a repeated event replaces its row and pops once', async () => {
    const tray = await mountTray([]);

    emitUserScoped(userEventPayload('n1'));
    emitUserScoped({ ...userEventPayload('n1'), message: 'edited' });
    await vi.advanceTimersByTimeAsync(0);

    expect(ids(tray)).toEqual(['n1']);
    expect(tray.notifications[0].message).toBe('edited');
    expect(popups).toEqual(['n1']);
    expect(listFetches()).toBe(0);
  });

  it('a list fetch sent before an upsert keeps the upserted row and pops it once', async () => {
    const tray = await mountTray([notif('old')]);

    // A fetch is in flight when the event for a new row arrives.
    let release: () => void = () => {};
    apiFetch.mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          release = (): void =>
            resolve(new Response(JSON.stringify([notif('old')]), { status: 200 }));
        })
    );
    emitBare();
    expect(listFetches()).toBe(1);
    emitUserScoped(userEventPayload('new'));
    expect(ids(tray)).toEqual(['new', 'old']);

    // Its answer predates the new row: the row stays.
    release();
    await vi.advanceTimersByTimeAsync(0);
    expect(ids(tray)).toEqual(['new', 'old']);

    // A later fetch that lists it does not pop it again.
    apiFetch.mockImplementation(() =>
      Promise.resolve(
        new Response(JSON.stringify([userEventPayload('new'), notif('old')]), { status: 200 })
      )
    );
    await tray.fetchNotifications();
    expect(ids(tray)).toEqual(['new', 'old']);
    expect(popups).toEqual(['new']);

    // A fetch sent after the upsert is authoritative: acknowledged
    // elsewhere, the row goes.
    apiFetch.mockImplementation(() =>
      Promise.resolve(new Response(JSON.stringify([notif('old')]), { status: 200 }))
    );
    await tray.fetchNotifications();
    expect(ids(tray)).toEqual(['old']);
  });

  it('a stale list response landing after a newer one is ignored', async () => {
    const tray = await mountTray([notif('old')]);

    // Two held list requests: A, then (after X is upserted) B.
    const releases: Array<(items: unknown[]) => void> = [];
    apiFetch.mockImplementation(
      () =>
        new Promise<Response>((resolve) => {
          releases.push((items) => resolve(new Response(JSON.stringify(items), { status: 200 })));
        })
    );
    const a = tray.fetchNotifications();
    emitUserScoped(userEventPayload('x'));
    const b = tray.fetchNotifications();
    expect(releases).toHaveLength(2);

    // B lists X and lands first; A, sent before X existed, lands last.
    releases[1]([userEventPayload('x'), notif('old')]);
    await b;
    releases[0]([notif('old')]);
    await a;
    expect(ids(tray)).toEqual(['x', 'old']);

    // X stays seen: the next fetch does not pop it again.
    apiFetch.mockImplementation(() =>
      Promise.resolve(
        new Response(JSON.stringify([userEventPayload('x'), notif('old')]), { status: 200 })
      )
    );
    await tray.fetchNotifications();
    expect(popups).toEqual(['x']);
  });

  it('a user change drops the previous user queued fetch and the next event fetches at once', async () => {
    const tray = await mountTray([]);

    // u1: the first event fetches, the second is queued for a trailing fetch.
    emitBare();
    emitBare();
    expect(listFetches()).toBe(1);

    // Switch to u2 before the trailing run: u2's own first fetch.
    tray.user = { id: 'u2', email: 'u2@example.com', displayName: 'u2' };
    await tray.updateComplete;
    await vi.advanceTimersByTimeAsync(0);
    expect(listFetches()).toBe(2);

    // The queued u1 fetch never fires.
    await vi.advanceTimersByTimeAsync(5000);
    expect(listFetches()).toBe(2);

    // u2's first event fetches at once (leading).
    emitBare();
    expect(listFetches()).toBe(3);
  });

  it('bare notification.created burst fetches once', async () => {
    const tray = await mountTray([]);
    apiFetch.mockImplementation(() =>
      Promise.resolve(new Response(JSON.stringify([notif('n1')]), { status: 200 }))
    );

    // 5 events in 200 ms.
    for (let i = 0; i < 5; i++) {
      emitBare();
      await vi.advanceTimersByTimeAsync(40);
    }
    // The first fetches at once.
    expect(listFetches()).toBe(1);
    await vi.advanceTimersByTimeAsync(5000);

    // 1 leading + at most 1 trailing.
    expect(listFetches()).toBeLessThanOrEqual(2);
    expect(ids(tray)).toEqual(['n1']);
  });

  it('malformed payload falls back to fetch', async () => {
    await mountTray([]);

    // Missing createdAt.
    const { createdAt: _drop, ...noCreatedAt } = userEventPayload('n1');
    emitUserScoped(noCreatedAt);
    await vi.advanceTimersByTimeAsync(0);
    expect(listFetches()).toBe(1);
  });

  it('a payload addressed to another user falls back to fetch', async () => {
    const tray = await mountTray([]);

    emitUserScoped(userEventPayload('n1', 'u2'));
    await vi.advanceTimersByTimeAsync(0);

    expect(listFetches()).toBe(1);
    expect(ids(tray)).toEqual([]);
  });

  it('a retired chat row is not added', async () => {
    const tray = await mountTray([]);

    emitUserScoped({ ...userEventPayload('n1'), status: 'MENTION' });
    await vi.advanceTimersByTimeAsync(0);

    expect(ids(tray)).toEqual([]);
    expect(popups).toEqual([]);
  });
});

describe('notificationFromEvent', () => {
  it('accepts the user-scoped payload', () => {
    expect(notificationFromEvent(userEventPayload('n1'), 'u1')).toMatchObject({
      id: 'n1',
      agentId: 'agent-2',
      projectId: 'project-1',
      status: 'SCHEDULE_BLOCKED',
      message: 'n1 blocked',
      createdAt: '2026-10-19T00:01:00.000Z',
      acknowledged: false,
      subscriberId: 'u1',
    });
  });

  it('accepts acknowledged false and a missing subscriberId', () => {
    const { subscriberId: _s, ...noSubscriber } = userEventPayload('n1');
    expect(notificationFromEvent({ ...noSubscriber, acknowledged: false }, 'u1')).not.toBeNull();
  });

  it('accepts a payload with kind and resource fields and no agent', () => {
    const payload = {
      ...userEventPayload('n2'),
      agentId: '00000000-0000-0000-0000-000000000000',
      status: 'ARTIFACT_REVIEW_REQUESTED',
      kind: 'artifact_grant',
      resourceType: 'artifact',
      resourceId: '5f1c2d3e-0000-4000-8000-000000000001',
    };
    expect(notificationFromEvent(payload, 'u1')).toMatchObject({
      id: 'n2',
      agentId: '00000000-0000-0000-0000-000000000000',
      status: 'ARTIFACT_REVIEW_REQUESTED',
      subscriberId: 'u1',
    });
  });

  it('rejects anything it cannot render or that is not for this user', () => {
    const ok = userEventPayload('n1');
    expect(notificationFromEvent(undefined, 'u1')).toBeNull();
    expect(notificationFromEvent('n1', 'u1')).toBeNull();
    expect(notificationFromEvent(ok, null)).toBeNull();
    expect(notificationFromEvent({ ...ok, acknowledged: true }, 'u1')).toBeNull();
    expect(notificationFromEvent({ ...ok, subscriberId: 'u2' }, 'u1')).toBeNull();
    expect(notificationFromEvent({ ...ok, id: '' }, 'u1')).toBeNull();
    expect(notificationFromEvent({ ...ok, status: undefined }, 'u1')).toBeNull();
    expect(notificationFromEvent({ ...ok, message: 7 }, 'u1')).toBeNull();
    expect(notificationFromEvent({ ...ok, createdAt: 'yesterday' }, 'u1')).toBeNull();
    expect(notificationFromEvent({ ...ok, agentId: undefined }, 'u1')).toBeNull();
  });
});
