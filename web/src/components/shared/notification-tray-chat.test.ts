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
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { render } from 'lit';

import { PUSH_STORAGE_KEY } from '../../client/push-preference.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('[]', { status: 200 }))),
}));

/** The all-zero UUID the hub writes into chat notification rows. */
const NIL_UUID = '00000000-0000-0000-0000-000000000000';

let popups: Array<{ title: string; options: NotificationOptions }> = [];

class FakeNotification {
  static permission: NotificationPermission = 'granted';
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
