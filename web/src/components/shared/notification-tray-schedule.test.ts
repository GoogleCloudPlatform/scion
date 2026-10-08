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
 * SCHEDULE_BLOCKED rows: a scheduled dispatch blocked by an agent row in
 * phase error (ptone/scion#3701). The row carries the errored agent's id, so
 * the tray links to it, and it pops a browser notification like any other
 * non-chat status.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { render } from 'lit';

import { PUSH_STORAGE_KEY } from '../../client/push-preference.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('[]', { status: 200 }))),
}));

let popups: Array<{ title: string; options: NotificationOptions }> = [];

class FakeNotification {
  static permission: NotificationPermission = 'granted';
  constructor(title: string, options: NotificationOptions = {}) {
    popups.push({ title, options });
  }
}

const blocked = {
  id: 'notif-blocked',
  status: 'SCHEDULE_BLOCKED',
  message:
    'Schedule "nightly" is blocked: agent "worker-1" is in phase error. Delete the agent to resume this schedule.',
  agentId: 'agent-errored',
  createdAt: new Date().toISOString(),
};

describe('notification tray: SCHEDULE_BLOCKED', () => {
  let SCHEDULE_BLOCKED_STATUS = '';

  beforeAll(async () => {
    ({ SCHEDULE_BLOCKED_STATUS } = await import('./notification-tray.js'));
  });

  beforeEach(() => {
    popups = [];
    (window as unknown as { Notification: unknown }).Notification = FakeNotification;
    localStorage.setItem(PUSH_STORAGE_KEY, 'true');
  });

  afterEach(() => {
    localStorage.clear();
  });

  it('matches the hub status', () => {
    expect(SCHEDULE_BLOCKED_STATUS).toBe('SCHEDULE_BLOCKED');
  });

  it('renders the message, a danger calendar icon and a link to the errored agent', () => {
    const host = document.createElement('div');
    const tray: any = document.createElement('scion-notification-tray');

    render(tray.renderItem(blocked), host);

    expect(host.textContent).toContain('Delete the agent to resume this schedule.');
    const icon = host.querySelector('.notif-icon');
    expect(icon?.classList.contains('status-danger')).toBe(true);
    expect(icon?.querySelector('sl-icon')?.getAttribute('name')).toBe('calendar-x');
    const links = host.querySelectorAll('a[href^="/agents/"]');
    expect(links).toHaveLength(1);
    expect(links[0].getAttribute('href')).toBe('/agents/agent-errored');
  });

  it('fires a browser notification titled for the schedule', () => {
    const tray: any = document.createElement('scion-notification-tray');
    tray.dispatchBrowserNotification(blocked);
    expect(popups.map((p) => p.title)).toEqual(['Schedule Blocked']);
    expect(popups[0].options.body).toBe(blocked.message);
  });

  it('ships its icon in production builds', async () => {
    const fs = await import('node:fs');
    const path = await import('node:path');
    const script = fs.readFileSync(
      path.resolve(__dirname, '../../../scripts/copy-shoelace-icons.mjs'),
      'utf8'
    );
    expect(script).toContain("'calendar-x'");
  });
});
