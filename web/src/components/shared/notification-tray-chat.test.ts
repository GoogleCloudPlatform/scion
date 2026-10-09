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
 * The tray's half of the exactly-once boundary for chat notifications.
 *
 * Two components can fire a browser notification for the same mention: this
 * tray (which re-fetches when a notification-created event arrives, then pops
 * for every ID it has not seen) and chat-notifications.ts (which pops straight
 * off the SSE payload). They are driven by the same event, so without an
 * explicit split every mention would appear twice.
 *
 * The split is by status, and it is asserted here rather than assumed.
 *
 * The second half of the file covers the master desktop-notification toggle
 * the tray carries. It lives here because the tray is the only notification
 * surface present on every page including chat, and because the rule that
 * matters about it — permission is requested on click and never on load — is
 * invisible unless something asserts it.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { render } from 'lit';

import { apiFetch } from '../../client/api.js';
import { PUSH_PREFERENCE_EVENT, PUSH_STORAGE_KEY } from '../../client/push-preference.js';
import { stateManager } from '../../client/state.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('[]', { status: 200 }))),
}));

/** The all-zero UUID the hub writes into chat notification rows. */
const NIL_UUID = '00000000-0000-0000-0000-000000000000';

let popups: Array<{ title: string; options: NotificationOptions }> = [];

class FakeNotification {
  static permission: NotificationPermission = 'granted';
  static requestPermission = vi.fn(async (): Promise<NotificationPermission> => {
    FakeNotification.permission = 'granted';
    return FakeNotification.permission;
  });

  constructor(title: string, options: NotificationOptions = {}) {
    popups.push({ title, options });
  }
}

function notification(status: string, agentId = NIL_UUID): any {
  return {
    id: `notif-${status}`,
    status,
    message: `${status} happened`,
    agentId,
    createdAt: new Date().toISOString(),
  };
}

/** An unattached tray — connectedCallback (and its polling) never runs. */
function createTray(): any {
  return document.createElement('scion-notification-tray');
}

describe('notification tray: chat notifications', () => {
  beforeAll(async () => {
    await import('./notification-tray.js');
  });

  beforeEach(() => {
    popups = [];
    FakeNotification.permission = 'granted';
    (window as unknown as { Notification: unknown }).Notification = FakeNotification;
    localStorage.setItem(PUSH_STORAGE_KEY, 'true');
  });

  afterEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
  });

  it('does not fire browser notifications for mentions or DMs', () => {
    const tray = createTray();

    tray.dispatchBrowserNotification(notification('MENTION'));
    tray.dispatchBrowserNotification(notification('DM_RECEIVED'));

    expect(popups).toHaveLength(0);
  });

  it('still fires browser notifications for agent statuses', () => {
    const tray = createTray();

    tray.dispatchBrowserNotification(notification('COMPLETED', 'agent-1'));
    tray.dispatchBrowserNotification(notification('WAITING_FOR_INPUT', 'agent-1'));

    expect(popups.map((p) => p.title)).toEqual(['Agent Completed', 'Agent Waiting on Parent']);
  });

  it('honours the shared push preference for agent statuses', () => {
    localStorage.setItem(PUSH_STORAGE_KEY, 'false');
    createTray().dispatchBrowserNotification(notification('COMPLETED', 'agent-1'));
    expect(popups).toHaveLength(0);
  });

  it('omits the agent link on chat rows, which have no agent', () => {
    const host = document.createElement('div');
    const tray = createTray();

    render(tray.renderItem(notification('MENTION')), host);
    const chatLinks = host.querySelectorAll('a[href^="/agents/"]');
    expect(chatLinks).toHaveLength(0);
    // The row itself still renders — only the broken link is gone.
    expect(host.textContent).toContain('MENTION happened');

    render(tray.renderItem(notification('COMPLETED', 'agent-1')), host);
    const agentLinks = host.querySelectorAll('a[href^="/agents/"]');
    expect(agentLinks).toHaveLength(1);
    expect(agentLinks[0].getAttribute('href')).toBe('/agents/agent-1');
  });
});

describe('notification tray: desktop notification toggle', () => {
  beforeAll(async () => {
    await import('./notification-tray.js');
  });

  beforeEach(() => {
    FakeNotification.permission = 'default';
    FakeNotification.requestPermission.mockClear();
    (window as unknown as { Notification: unknown }).Notification = FakeNotification;
    localStorage.clear();
  });

  afterEach(() => {
    localStorage.clear();
  });

  it('never asks for permission on load', async () => {
    const tray = createTray();
    document.body.appendChild(tray);
    await tray.updateComplete;

    expect(FakeNotification.requestPermission).not.toHaveBeenCalled();

    tray.remove();
  });

  it('asks for permission when the user turns it on, and only then', async () => {
    const tray = createTray();

    await tray.handlePushToggle();

    expect(FakeNotification.requestPermission).toHaveBeenCalledTimes(1);
    expect(localStorage.getItem(PUSH_STORAGE_KEY)).toBe('true');
    expect(tray.pushEnabled).toBe(true);
  });

  it('turns off without touching the browser permission', async () => {
    FakeNotification.permission = 'granted';
    localStorage.setItem(PUSH_STORAGE_KEY, 'true');
    const tray = createTray();
    tray.syncPushState();
    expect(tray.pushEnabled).toBe(true);

    await tray.handlePushToggle();

    expect(FakeNotification.requestPermission).not.toHaveBeenCalled();
    expect(localStorage.getItem(PUSH_STORAGE_KEY)).toBe('false');
    expect(tray.pushEnabled).toBe(false);
  });

  it('reports the preference change so other surfaces follow', async () => {
    const heard: boolean[] = [];
    const listener = (e: Event): void => {
      heard.push((e as CustomEvent<{ enabled: boolean }>).detail.enabled);
    };
    window.addEventListener(PUSH_PREFERENCE_EVENT, listener);
    try {
      await createTray().handlePushToggle();
    } finally {
      window.removeEventListener(PUSH_PREFERENCE_EVENT, listener);
    }

    expect(heard).toEqual([true]);
  });

  it('shows the toggle as blocked, not as off, when the browser refused', () => {
    FakeNotification.permission = 'denied';
    const host = document.createElement('div');
    const tray = createTray();
    tray.syncPushState();

    render(tray.renderPushToggle(), host);
    const button = host.querySelector('button');

    expect(button?.textContent).toContain('blocked');
    expect(button?.hasAttribute('disabled')).toBe(true);
  });

  it('shows blocked immediately when permission was revoked after opting in', () => {
    // The stored opt-in survives a revoke in site settings. If it were allowed
    // to decide, the button would render "off" and stay clickable, and the
    // click could not re-prompt — it would just clear the flag, so the user
    // would spend a click to be told what the browser already knew.
    FakeNotification.permission = 'denied';
    localStorage.setItem(PUSH_STORAGE_KEY, 'true');
    const host = document.createElement('div');
    const tray = createTray();
    tray.syncPushState();

    render(tray.renderPushToggle(), host);
    const button = host.querySelector('button');

    expect(button?.textContent).toContain('blocked');
    expect(button?.hasAttribute('disabled')).toBe(true);
  });

  it('hides the toggle entirely where the API does not exist', () => {
    delete (window as unknown as { Notification?: unknown }).Notification;
    const host = document.createElement('div');
    const tray = createTray();
    tray.syncPushState();

    render(tray.renderPushToggle(), host);

    expect(host.querySelector('button')).toBeNull();
  });
});

describe('notification tray: loading for the signed-in user', () => {
  const POLL_MS = 5 * 60_000;
  const LIST_URL = '/api/v1/notifications?acknowledged=false';
  const fetchMock = apiFetch as unknown as ReturnType<typeof vi.fn>;
  let server: any[] = [];
  let trays: any[] = [];

  function user(id: string): any {
    return { id, email: `${id}@example.com`, name: id };
  }

  function listRequests(): number {
    return fetchMock.mock.calls.filter((c) => c[0] === LIST_URL).length;
  }

  async function mount(initialUser: any): Promise<any> {
    const tray = createTray();
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

  beforeAll(async () => {
    await import('./notification-tray.js');
  });

  beforeEach(() => {
    vi.useFakeTimers();
    server = [];
    trays = [];
    popups = [];
    FakeNotification.permission = 'granted';
    (window as unknown as { Notification: unknown }).Notification = FakeNotification;
    localStorage.setItem(PUSH_STORAGE_KEY, 'true');
    fetchMock.mockReset();
    fetchMock.mockImplementation(() =>
      Promise.resolve(new Response(JSON.stringify(server), { status: 200 }))
    );
  });

  afterEach(() => {
    for (const t of trays) t.remove();
    localStorage.clear();
    vi.useRealTimers();
  });

  it('sends one request when mounted with a user', async () => {
    const tray = await mount(user('u1'));

    expect(listRequests()).toBe(1);
    // One polling timer.
    expect(vi.getTimerCount()).toBe(1);
    // One SSE listener: one event, one refetch.
    stateManager.dispatchEvent(new CustomEvent('notification-created', { detail: {} }));
    await vi.advanceTimersByTimeAsync(0);
    expect(listRequests()).toBe(2);
    expect(tray.pollTimer).not.toBeNull();
  });

  it('sends one request when the user arrives after mount', async () => {
    const tray = await mount(null);
    expect(listRequests()).toBe(0);
    expect(vi.getTimerCount()).toBe(0);

    await setUser(tray, user('u1'));

    expect(listRequests()).toBe(1);
    expect(vi.getTimerCount()).toBe(1);
    stateManager.dispatchEvent(new CustomEvent('notification-created', { detail: {} }));
    await vi.advanceTimersByTimeAsync(0);
    expect(listRequests()).toBe(2);
  });

  it('does not refetch for a new object of the same user', async () => {
    const tray = await mount(user('u1'));
    const timer = tray.pollTimer;

    await setUser(tray, user('u1'));

    expect(listRequests()).toBe(1);
    // Polling was not restarted either.
    expect(tray.pollTimer).toBe(timer);
  });

  it('refetches once when a different user signs in', async () => {
    const tray = await mount(user('u1'));

    await setUser(tray, user('u2'));

    expect(listRequests()).toBe(2);
    expect(vi.getTimerCount()).toBe(1);
  });

  it('clears the list and stops loading when the user signs out', async () => {
    server = [notification('COMPLETED', 'agent-1')];
    const tray = await mount(user('u1'));
    expect(tray.notifications).toHaveLength(1);

    await setUser(tray, null);

    expect(tray.notifications).toEqual([]);
    expect(tray.pollTimer).toBeNull();
    expect(vi.getTimerCount()).toBe(0);
    stateManager.dispatchEvent(new CustomEvent('notification-created', { detail: {} }));
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

  it('loads again when the same user signs back in after signing out', async () => {
    const tray = await mount(user('u1'));
    await setUser(tray, null);

    await setUser(tray, user('u1'));

    expect(listRequests()).toBe(2);
    expect(vi.getTimerCount()).toBe(1);
    // One SSE listener: one event, one refetch.
    stateManager.dispatchEvent(new CustomEvent('notification-created', { detail: {} }));
    await vi.advanceTimersByTimeAsync(0);
    expect(listRequests()).toBe(3);
  });

  it('does not load for a user change while removed, and loads on re-add', async () => {
    const tray = await mount(user('u1'));
    tray.remove();

    await setUser(tray, user('u2'));

    expect(vi.getTimerCount()).toBe(0);
    expect(listRequests()).toBe(1);

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

  it('refetches when the panel opens', async () => {
    const tray = await mount(user('u1'));

    tray.toggle();
    await vi.advanceTimersByTimeAsync(0);

    expect(listRequests()).toBe(2);
  });

  it('pops only for notifications that arrive after the first load', async () => {
    server = [{ ...notification('COMPLETED', 'agent-1'), id: 'old' }];
    await mount(user('u1'));
    expect(popups).toHaveLength(0);

    server = [
      { ...notification('COMPLETED', 'agent-1'), id: 'old' },
      { ...notification('WAITING_FOR_INPUT', 'agent-2'), id: 'new' },
    ];
    stateManager.dispatchEvent(new CustomEvent('notification-created', { detail: {} }));
    await vi.advanceTimersByTimeAsync(0);

    expect(popups.map((p) => p.title)).toEqual(['Agent Waiting on Parent']);
  });
});
