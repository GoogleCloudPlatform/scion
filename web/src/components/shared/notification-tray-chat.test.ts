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

import { PUSH_PREFERENCE_EVENT, PUSH_STORAGE_KEY } from '../../client/push-preference.js';

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

    expect(popups.map((p) => p.title)).toEqual(['Agent Completed', 'Agent Needs Input']);
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
